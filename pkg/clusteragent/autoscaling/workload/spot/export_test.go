// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2026-present Datadog, Inc.

//go:build kubeapiserver && test

package spot

import (
	"context"
	"time"

	"k8s.io/utils/clock"

	workloadmeta "github.com/DataDog/datadog-agent/comp/core/workloadmeta/def"
)

// NewTestScheduler create a Scheduler for testing.
func NewTestScheduler(config Config, clk clock.WithTicker, wlm workloadmeta.Component) *Scheduler {
	rollout := rolloutFunc(func(context.Context, ownerKey, time.Time) (bool, error) {
		return true, nil
	})
	isLeader := func() bool {
		return true
	}
	return newScheduler(config, clk, wlm, rollout, isLeader)
}

// rolloutFunc is a function type implementing rollout for testing.
type rolloutFunc func(context.Context, ownerKey, time.Time) (bool, error)

func (f rolloutFunc) restart(ctx context.Context, k ownerKey, ts time.Time) (bool, error) {
	return f(ctx, k, ts)
}
