// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

//go:build linux_bpf

package testprogs

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v3"

	"github.com/DataDog/datadog-agent/pkg/dyninst/ir"
	"github.com/DataDog/datadog-agent/pkg/dyninst/rcjson"
)

type probeYaml struct {
	Binary string           `yaml:"binary"`
	Probes []map[string]any `yaml:"probes"`
}

// MustGetProbeDefinitions calls GetProbeDefinitions and checks for an error.
func MustGetProbeDefinitions(t testing.TB, name string) []ir.ProbeDefinition {
	probes, err := GetProbeDefinitions(name)
	require.NoError(t, err)
	return probes
}

// GetProbeDefinitions returns the probe definitions for binary of a given name.
func GetProbeDefinitions(name string) ([]ir.ProbeDefinition, error) {
	probes, err := getProbeDefinitions(name)
	if err != nil {
		return nil, fmt.Errorf("get probe definitions for %s: %w", name, err)
	}
	return probes, nil
}

func getProbeDefinitions(name string) ([]ir.ProbeDefinition, error) {
	state, err := getState()
	if err != nil {
		return nil, err
	}
	yamlData, err := os.ReadFile(path.Join(state.probesCfgsDir, name+".yaml"))
	if err != nil {
		return nil, err
	}
	var probeYaml probeYaml
	err = yaml.Unmarshal(yamlData, &probeYaml)
	if err != nil {
		return nil, err
	}
	var probes []ir.ProbeDefinition
	for _, probe := range probeYaml.Probes {
		probeBytes, err := json.Marshal(probe)
		if err != nil {
			return nil, err
		}
		probe, err := rcjson.UnmarshalProbe(probeBytes)
		if err != nil {
			return nil, err
		}
		if err := rcjson.Validate(probe); err != nil {
			return nil, fmt.Errorf("validate probe %s: %w", probe.GetID(), err)
		}
		probes = append(probes, probe)
	}
	return probes, nil
}

// IssueTagPrefix is the prefix of the issue tag.
const IssueTagPrefix = "issue:"

// GetIssueTag returns the issue tag for a probe definition.
func GetIssueTag(p ir.ProbeDefinition) (string, bool) {
	tags := p.GetTags()
	index := slices.IndexFunc(tags, func(tag string) bool {
		return strings.HasPrefix(tag, IssueTagPrefix)
	})
	if index == -1 {
		return "", false
	}
	return tags[index][len(IssueTagPrefix):], true
}

// HasIssueTag returns true if the probe definition has an issue tag.
func HasIssueTag(p ir.ProbeDefinition) bool {
	_, ok := GetIssueTag(p)
	return ok
}

// SkipIntegrationConfigTagPrefix is the prefix for tags that mark a probe as
// skipped for a specific testprogs Config in integration tests. The tag value
// is the Config.String() representation (e.g. "arch=amd64,toolchain=go1.23.11").
// A probe may carry multiple such tags to skip multiple configs.
const SkipIntegrationConfigTagPrefix = "skip_integration_config:"

// GetSkippedIntegrationConfigs returns the set of Config values for which the
// given probe should be skipped in integration tests. Each
// "skip_integration_config:<config>" tag contributes one entry.
func GetSkippedIntegrationConfigs(p ir.ProbeDefinition) ([]Config, error) {
	var cfgs []Config
	for _, tag := range p.GetTags() {
		if !strings.HasPrefix(tag, SkipIntegrationConfigTagPrefix) {
			continue
		}
		cfg, err := parseConfig(tag[len(SkipIntegrationConfigTagPrefix):])
		if err != nil {
			return nil, fmt.Errorf("parse skip_integration_config tag %q on probe %s: %w", tag, p.GetID(), err)
		}
		cfgs = append(cfgs, cfg)
	}
	return cfgs, nil
}

// MustGetSkippedIntegrationConfigs calls GetSkippedIntegrationConfigs and
// fails the test on error.
func MustGetSkippedIntegrationConfigs(t testing.TB, p ir.ProbeDefinition) []Config {
	cfgs, err := GetSkippedIntegrationConfigs(p)
	require.NoError(t, err)
	return cfgs
}

// IsIntegrationConfigSkipped returns true if the probe should be skipped for
// the given Config.
func IsIntegrationConfigSkipped(t testing.TB, p ir.ProbeDefinition, cfg Config) bool {
	cfgs := MustGetSkippedIntegrationConfigs(t, p)
	return slices.Contains(cfgs, cfg)
}
