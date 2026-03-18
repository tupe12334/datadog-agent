// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

package secretsimpl

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	json "github.com/json-iterator/go"

	secrets "github.com/DataDog/datadog-agent/comp/core/secrets/def"
	"github.com/DataDog/datadog-agent/pkg/util/filesystem"
	"github.com/DataDog/datadog-agent/pkg/util/log"
)

type limitBuffer struct {
	max int
	buf *bytes.Buffer
}

func (b *limitBuffer) Write(p []byte) (n int, err error) {
	if len(p)+b.buf.Len() > b.max {
		return 0, fmt.Errorf("command output was too long: exceeded %d bytes", b.max)
	}
	return b.buf.Write(p)
}

func (r *secretResolver) execCommand(inputPayload string, timeout int) ([]byte, error) {
	// hook used only for tests
	if r.commandHookFunc != nil {
		return r.commandHookFunc(inputPayload)
	}

	ctx, cancel := context.WithTimeout(context.Background(),
		time.Duration(timeout)*time.Second)
	defer cancel()

	cmd, done, err := commandContext(ctx, r.backendCommand, r.backendArguments...)
	if err != nil {
		return nil, err
	}
	defer done()

	if !r.embeddedBackendPermissiveRights {
		if err := checkRightsFunc(cmd.Path, r.commandAllowGroupExec); err != nil {
			return nil, err
		}
	}

	cmd.Stdin = strings.NewReader(inputPayload)

	stdout := limitBuffer{
		buf: &bytes.Buffer{},
		max: r.responseMaxSize,
	}
	stderr := limitBuffer{
		buf: &bytes.Buffer{},
		max: r.responseMaxSize,
	}
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	// We add the actual time to the log message. This is needed in the case we have a secret in the datadog.yaml.
	// When it's the case the log package is not yet initialized (since it needs the configuration) and it will
	// buffer logs until it's initialized. This means the time of the log line will be the one after the package is
	// initialized and not the creation time. This is an issue when troubleshooting a secret_backend_command in
	// datadog.yaml.
	log.Debugf("%s | calling secret_backend_command with payload: '%s'", time.Now().String(), inputPayload)
	start := time.Now()
	err = cmd.Run()
	elapsed := time.Since(start)
	log.Debugf("%s | secret_backend_command '%s' completed in %s", time.Now().String(), r.backendCommand, elapsed)

	// We always log stderr to allow a secret_backend_command to logs info in the agent log file. This is useful to
	// troubleshoot secret_backend_command in a containerized environment.
	if err != nil {
		log.Errorf("secret_backend_command stderr: %s", stderr.buf.String())

		exitCode := "unknown"
		var e *exec.ExitError
		if errors.As(err, &e) {
			exitCode = strconv.Itoa(e.ExitCode())
		} else if ctx.Err() == context.DeadlineExceeded {
			exitCode = "timeout"
		}
		r.tlmSecretBackendElapsed.Add(float64(elapsed.Milliseconds()), r.backendCommand, exitCode)

		if ctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("error while running '%s': command timeout", r.backendCommand)
		}
		return nil, fmt.Errorf("error while running '%s': %s", r.backendCommand, err)
	}

	log.Debugf("secret_backend_command stderr: %s", stderr.buf.String())

	r.tlmSecretBackendElapsed.Add(float64(elapsed.Milliseconds()), r.backendCommand, "0")
	return stdout.buf.Bytes(), nil
}

func (r *secretResolver) fetchSecretBackendVersion() (string, error) {
	// hook used only for tests
	if r.versionHookFunc != nil {
		return r.versionHookFunc()
	}

	// Only get version when secret_backend_type or extra_secret_backends is used
	if r.backendType == "" && len(r.backendConfigs) == 0 {
		return "", errors.New("version only supported when secret_backend_type or extra_secret_backends is configured")
	}

	ctx, cancel := context.WithTimeout(context.Background(),
		min(time.Duration(r.backendTimeout)*time.Second, 1*time.Second))
	defer cancel()

	// Execute with --version argument
	cmd, done, err := commandContext(ctx, r.backendCommand, "--version")
	if err != nil {
		return "", err
	}
	defer done()

	if !r.embeddedBackendPermissiveRights {
		if err := filesystem.CheckRights(cmd.Path, r.commandAllowGroupExec); err != nil {
			return "", err
		}
	}

	stdout := limitBuffer{
		buf: &bytes.Buffer{},
		max: r.responseMaxSize,
	}
	stderr := limitBuffer{
		buf: &bytes.Buffer{},
		max: r.responseMaxSize,
	}
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	log.Debugf("calling secret_backend_command --version")
	err = cmd.Run()

	if err != nil {
		log.Debugf("secret_backend_command --version stderr: %s", stderr.buf.String())
		if ctx.Err() == context.DeadlineExceeded {
			return "", errors.New("version command timeout")
		}
		return "", fmt.Errorf("version command failed: %w", err)
	}

	return strings.TrimSpace(stdout.buf.String()), nil
}

// splitSecretHandle splits a handle on "::" returning (backendID, secretKey).
// If no "::" is present, backendID is "" (default backend) and the full string is the key.
// The double-colon delimiter avoids ambiguity with handle formats that already contain
// a single colon, such as vault://path#/json/pointer or Windows absolute paths (C:\...).
func splitSecretHandle(handle string) (backendID, secretKey string) {
	const delim = "::"
	idx := strings.Index(handle, delim)
	if idx == -1 {
		return "", handle
	}
	return handle[:idx], handle[idx+len(delim):]
}

// resolveBackendConfig returns the type, config, and timeout for a named backend.
// An empty backendID or the literal "default" refers to the default backend
// (secret_backend_type / secret_backend_config), so ENC[default::key] and ENC[key]
// are equivalent. The timeout falls back to the global r.backendTimeout if not set.
func (r *secretResolver) resolveBackendConfig(backendID string) (string, map[string]interface{}, int, error) {
	if backendID == "" || backendID == "default" {
		return r.backendType, r.backendConfig, r.backendTimeout, nil
	}
	raw, ok := r.backendConfigs[backendID]
	if !ok {
		return "", nil, 0, fmt.Errorf("unknown backend %q", backendID)
	}
	entry, ok := raw.(map[string]interface{})
	if !ok {
		return "", nil, 0, fmt.Errorf("invalid config for backend %q", backendID)
	}
	bType, _ := entry["type"].(string)
	bConfig, _ := entry["config"].(map[string]interface{})
	if bConfig == nil {
		bConfig = make(map[string]interface{})
	}
	bTimeout := r.backendTimeout
	switch v := entry["secret_backend_timeout"].(type) {
	case int:
		bTimeout = v
	case float64:
		bTimeout = int(v)
	}
	return bType, bConfig, bTimeout, nil
}

// fetchSecret groups the provided handles by backend (using the "::" delimiter),
// calls fetchSingleBackend once per backend, and returns a merged result keyed by
// the original handles. Each backend is attempted independently so a failure in one
// does not affect the others. Per-handle errors are returned in the second map.
func (r *secretResolver) fetchSecret(handles []string) (map[string]string, map[string]error) {
	type group struct {
		backendType    string
		backendConfig  map[string]interface{}
		backendTimeout int
		keys           []string // stripped secret keys sent to the binary
		origHandles    []string // original handles for result remapping
		cfgErr         error    // set when the backend ID could not be resolved
	}

	groups := map[string]*group{}
	for _, handle := range handles {
		backendID, secretKey := splitSecretHandle(handle)
		// Normalize "default" to "" so ENC[default::key] and ENC[key] share the same group
		// and result in a single backend call.
		if backendID == "default" {
			backendID = ""
		}
		if _, exists := groups[backendID]; !exists {
			bType, bConfig, bTimeout, err := r.resolveBackendConfig(backendID)
			groups[backendID] = &group{backendType: bType, backendConfig: bConfig, backendTimeout: bTimeout, cfgErr: err}
		}
		groups[backendID].keys = append(groups[backendID].keys, secretKey)
		groups[backendID].origHandles = append(groups[backendID].origHandles, handle)
	}

	result := make(map[string]string, len(handles))
	var handleErrors map[string]error
	for _, g := range groups {
		if g.cfgErr != nil {
			if handleErrors == nil {
				handleErrors = make(map[string]error)
			}
			for _, h := range g.origHandles {
				handleErrors[h] = fmt.Errorf("handle %q: %s", h, g.cfgErr)
			}
			continue
		}
		res, err := r.fetchSingleBackend(g.backendType, g.backendConfig, g.backendTimeout, g.keys)
		if err != nil {
			if handleErrors == nil {
				handleErrors = make(map[string]error)
			}
			for _, h := range g.origHandles {
				handleErrors[h] = err
			}
			continue
		}
		for i, key := range g.keys {
			if val, ok := res[key]; ok {
				result[g.origHandles[i]] = val
			}
		}
	}
	return result, handleErrors
}

// fetchSingleBackend calls the secret backend command for a single backend type/config
// and returns a map of secret key → value.
func (r *secretResolver) fetchSingleBackend(backendType string, backendConfig map[string]interface{}, backendTimeout int, secretsHandle []string) (map[string]string, error) {
	payload := map[string]interface{}{
		"version":                secrets.PayloadVersion,
		"secrets":                secretsHandle,
		"secret_backend_timeout": backendTimeout,
	}
	if backendType != "" {
		payload["type"] = backendType
	}
	if len(backendConfig) > 0 {
		payload["config"] = backendConfig
	}
	jsonPayload, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("could not serialize secrets IDs to fetch password: %s", err)
	}
	output, err := r.execCommand(string(jsonPayload), backendTimeout)
	if err != nil {
		return nil, err
	}

	secrets := map[string]secrets.SecretVal{}
	err = json.Unmarshal(output, &secrets)
	if err != nil {
		r.tlmSecretUnmarshalError.Inc()
		return nil, fmt.Errorf("could not unmarshal 'secret_backend_command' output: %s", err)
	}

	res := map[string]string{}
	for _, sec := range secretsHandle {
		v, ok := secrets[sec]
		if !ok {
			r.tlmSecretResolveError.Inc("missing", sec)
			return nil, fmt.Errorf("secret handle '%s' was not resolved by the secret_backend_command", sec)
		}

		if v.ErrorMsg != "" {
			r.tlmSecretResolveError.Inc("error", sec)
			return nil, fmt.Errorf("an error occurred while resolving '%s': %s", sec, v.ErrorMsg)
		}

		if r.removeTrailingLinebreak {
			v.Value = strings.TrimRight(v.Value, "\r\n")
		}

		if v.Value == "" {
			r.tlmSecretResolveError.Inc("empty", sec)
			return nil, fmt.Errorf("resolved secret for '%s' is empty", sec)
		}
		res[sec] = v.Value
	}
	return res, nil
}
