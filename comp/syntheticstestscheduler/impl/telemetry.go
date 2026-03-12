// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2025-present Datadog, Inc.

package syntheticstestschedulerimpl

import telemetrydef "github.com/DataDog/datadog-agent/comp/core/telemetry/def"

const subsystem = "synthetics_agent"

type syntheticsTelemetry struct {
	// ChecksReceived tracks the number of synthetics checks received from remote config
	ChecksReceived telemetrydef.Counter
	// ChecksProcessed tracks the number of synthetics checks processed
	ChecksProcessed telemetrydef.Counter
	// ErrorTestConfig tracks errors when interpreting test configuration
	ErrorTestConfig telemetrydef.Counter
	// TracerouteError tracks errors when running traceroute
	TracerouteError telemetrydef.Counter
	// SendResultFailure tracks errors when sending results to the event platform
	SendResultFailure telemetrydef.Counter
}

func newSyntheticsTelemetry(comp telemetrydef.Component) *syntheticsTelemetry {
	return &syntheticsTelemetry{
		ChecksReceived: comp.NewCounter(
			subsystem,
			"checks_received",
			nil,
			"Number of synthetics checks received from remote config",
		),
		ChecksProcessed: comp.NewCounter(
			subsystem,
			"checks_processed",
			[]string{"status", "subtype"},
			"Number of synthetics checks processed",
		),
		ErrorTestConfig: comp.NewCounter(
			subsystem,
			"error_test_config",
			[]string{"subtype"},
			"Errors when interpreting test configuration",
		),
		TracerouteError: comp.NewCounter(
			subsystem,
			"traceroute_error",
			[]string{"subtype"},
			"Errors when running datadog traceroute",
		),
		SendResultFailure: comp.NewCounter(
			subsystem,
			"evp_send_result_failure",
			[]string{"subtype"},
			"Errors when sending results to the event platform",
		),
	}
}
