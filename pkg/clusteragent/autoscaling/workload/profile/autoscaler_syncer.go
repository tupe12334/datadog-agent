// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

//go:build kubeapiserver

package profile

import (
	"context"
	"sync"
	"time"

	autoscalingv2 "k8s.io/api/autoscaling/v2"

	datadoghq "github.com/DataDog/datadog-operator/api/datadoghq/v1alpha2"

	"github.com/DataDog/datadog-agent/pkg/clusteragent/autoscaling"
	"github.com/DataDog/datadog-agent/pkg/clusteragent/autoscaling/workload/model"
	"github.com/DataDog/datadog-agent/pkg/util/log"
)

const (
	syncerStoreID autoscaling.SenderID = "prof-s"

	syncerReconcilePeriod = 1 * time.Minute
)

// AutoscalerSyncer maintains consistency between the profile store (workload
// references set by the WorkloadWatcher) and the DPA store. It registers as an
// observer on both stores and reconciles whenever either changes.
//
// Internal state:
//   - dpaOwnership maps each DPA store key to the profile name that owns it.
//     This is the single source of truth for what the syncer has created.
type AutoscalerSyncer struct {
	profileStore *autoscaling.Store[model.PodAutoscalerProfileInternal]
	dpaStore     *autoscaling.Store[model.PodAutoscalerInternal]
	isLeader     func() bool

	mu           sync.Mutex
	dpaOwnership map[string]string // dpa store key → profile name

	reconcileCh chan struct{}
}

// NewAutoscalerSyncer creates a new AutoscalerSyncer and registers observers.
func NewAutoscalerSyncer(
	profileStore *autoscaling.Store[model.PodAutoscalerProfileInternal],
	dpaStore *autoscaling.Store[model.PodAutoscalerInternal],
	isLeader func() bool,
) *AutoscalerSyncer {
	s := &AutoscalerSyncer{
		profileStore: profileStore,
		dpaStore:     dpaStore,
		isLeader:     isLeader,
		dpaOwnership: make(map[string]string),
		reconcileCh:  make(chan struct{}, 1),
	}

	// Only observe the profile store. DPA store changes (values updates, scaling
	// events) are irrelevant and would trigger expensive reconciles every ~30s.
	// Conflict with user-created DPAs is detected by the periodic safety-net reconcile.
	profileStore.RegisterObserver(autoscaling.Observer{
		SetFunc:    func(_ string, sender autoscaling.SenderID) { s.enqueue(sender) },
		DeleteFunc: func(_ string, sender autoscaling.SenderID) { s.enqueue(sender) },
	})

	return s
}

func (s *AutoscalerSyncer) enqueue(sender autoscaling.SenderID) {
	if sender == syncerStoreID {
		return
	}
	select {
	case s.reconcileCh <- struct{}{}:
	default:
	}
}

// Run starts the syncer loop. It blocks until ctx is cancelled.
func (s *AutoscalerSyncer) Run(ctx context.Context) {
	ticker := time.NewTicker(syncerReconcilePeriod)
	defer ticker.Stop()

	log.Infof("AutoscalerSyncer: starting")

	for {
		select {
		case <-s.reconcileCh:
			if s.isLeader() {
				s.reconcile()
			}
		case <-ticker.C:
			if s.isLeader() {
				s.reconcile()
			}
		case <-ctx.Done():
			return
		}
	}
}

// desiredDPA holds the information needed to create or update a single DPA entry.
type desiredDPA struct {
	profileName  string
	ref          model.NamespacedObjectReference
	template     *datadoghq.DatadogPodAutoscalerTemplate
	templateHash string
}

// reconcile performs a full sync between the profile store and the DPA store.
func (s *AutoscalerSyncer) reconcile() {
	s.mu.Lock()
	defer s.mu.Unlock()

	desired := s.buildDesiredState()
	s.removeConflicts(desired)
	s.applyChanges(desired)
}

// buildDesiredState builds the map of DPA store keys to desired DPA entries
// from all valid profiles with workload references.
func (s *AutoscalerSyncer) buildDesiredState() map[string]desiredDPA {
	desired := make(map[string]desiredDPA)

	profiles := s.profileStore.GetAll()
	for _, profileInternal := range profiles {
		if !profileInternal.Valid() || profileInternal.Template() == nil {
			continue
		}
		for dpaKey, ref := range profileInternal.Workloads() {
			desired[dpaKey] = desiredDPA{
				profileName:  profileInternal.Name(),
				ref:          ref,
				template:     profileInternal.Template(),
				templateHash: profileInternal.TemplateHash(),
			}
		}
	}

	return desired
}

// removeConflicts removes entries from desired that conflict with user-created DPAs.
// If we previously owned a DPA that now conflicts, it is marked deleted.
func (s *AutoscalerSyncer) removeConflicts(desired map[string]desiredDPA) {
	// Build a lookup: "namespace/kind/name" → dpa key for all desired entries.
	workloadToDesired := make(map[string]string, len(desired))
	for dpaKey, d := range desired {
		wKey := d.ref.Namespace + "/" + d.ref.Kind + "/" + d.ref.Name
		workloadToDesired[wKey] = dpaKey
	}

	if len(workloadToDesired) == 0 {
		return
	}

	// Scan user-created DPAs for conflicts.
	userDPAs := s.dpaStore.GetFiltered(func(pai model.PodAutoscalerInternal) bool {
		return !pai.IsProfileManaged() && pai.Spec() != nil && !pai.Deleted()
	})

	for _, userDPA := range userDPAs {
		wKey := userDPA.Namespace() + "/" + userDPA.Spec().TargetRef.Kind + "/" + userDPA.Spec().TargetRef.Name
		conflictingDPAKey, ok := workloadToDesired[wKey]
		if !ok {
			continue
		}

		log.Infof("AutoscalerSyncer: user-created DPA %s/%s conflicts with profile-managed DPA %s, removing generated DPA", userDPA.Namespace(), userDPA.Name(), conflictingDPAKey)
		delete(desired, conflictingDPAKey)

		if _, owned := s.dpaOwnership[conflictingDPAKey]; owned {
			s.markDPADeleted(conflictingDPAKey)
			delete(s.dpaOwnership, conflictingDPAKey)
		}
	}
}

// applyChanges diffs the desired state against the current dpaOwnership and
// applies creates, updates, and deletes to the DPA store.
func (s *AutoscalerSyncer) applyChanges(desired map[string]desiredDPA) {
	// 1. Remove DPAs that are no longer desired.
	for dpaKey, profileName := range s.dpaOwnership {
		if _, ok := desired[dpaKey]; !ok {
			log.Infof("AutoscalerSyncer: marking DPA %s (profile %s) deleted — no longer desired", dpaKey, profileName)
			s.markDPADeleted(dpaKey)
			delete(s.dpaOwnership, dpaKey)
		}
	}

	// 2. Create or update desired DPAs.
	for dpaKey, d := range desired {
		existingProfile, owned := s.dpaOwnership[dpaKey]
		if !owned {
			s.createOrUpdateDPA(dpaKey, d)
			s.dpaOwnership[dpaKey] = d.profileName
			continue
		}

		if existingProfile != d.profileName {
			// Workload label changed from one profile to another.
			log.Infof("AutoscalerSyncer: DPA %s switching from profile %s to %s", dpaKey, existingProfile, d.profileName)
			s.updateDPA(dpaKey, d)
			s.dpaOwnership[dpaKey] = d.profileName
			continue
		}

		// Same profile — check if template changed.
		s.maybeUpdateDPA(dpaKey, d)
	}
}

// createOrUpdateDPA creates a new DPA entry or updates an existing one.
func (s *AutoscalerSyncer) createOrUpdateDPA(dpaKey string, d desiredDPA) {
	targetRef := buildTargetRef(d.ref)

	pai, found, unlock := s.dpaStore.LockRead(dpaKey, true)
	if found {
		if pai.IsProfileManaged() {
			pai.UpdateFromProfile(d.profileName, d.template, targetRef, d.templateHash)
			s.dpaStore.UnlockSet(dpaKey, pai, syncerStoreID)
		} else {
			unlock()
		}
		return
	}

	log.Infof("AutoscalerSyncer: creating DPA %s for profile %s", dpaKey, d.profileName)
	pai = model.NewPodAutoscalerFromProfile(d.ref.Namespace, dpaNameFromKey(dpaKey), d.profileName, d.template, targetRef, d.templateHash)
	s.dpaStore.UnlockSet(dpaKey, pai, syncerStoreID)
}

// updateDPA updates an existing DPA entry with a new profile and template.
func (s *AutoscalerSyncer) updateDPA(dpaKey string, d desiredDPA) {
	targetRef := buildTargetRef(d.ref)

	pai, found, unlock := s.dpaStore.LockRead(dpaKey, false)
	if !found {
		unlock()
		s.createOrUpdateDPA(dpaKey, d)
		return
	}

	pai.UpdateFromProfile(d.profileName, d.template, targetRef, d.templateHash)
	s.dpaStore.UnlockSet(dpaKey, pai, syncerStoreID)
}

// maybeUpdateDPA updates the DPA if the template hash has changed.
// maybeUpdateDPA is a fast-path check: compare the cached profile template hash
// on the DPA with the desired hash. Only acquire the write lock when there's an
// actual change, turning steady-state reconciles into a cheap string comparison.
func (s *AutoscalerSyncer) maybeUpdateDPA(dpaKey string, d desiredDPA) {
	pai, found := s.dpaStore.Get(dpaKey)
	if !found {
		s.createOrUpdateDPA(dpaKey, d)
		return
	}

	if pai.DesiredProfileTemplateHash() == d.templateHash {
		return
	}

	s.updateDPA(dpaKey, d)
}

// markDPADeleted marks a DPA entry as deleted in the store.
func (s *AutoscalerSyncer) markDPADeleted(dpaKey string) {
	pai, found, unlock := s.dpaStore.LockRead(dpaKey, false)
	if !found {
		unlock()
		return
	}

	pai.SetDeleted()
	s.dpaStore.UnlockSet(dpaKey, pai, syncerStoreID)
}

func buildTargetRef(ref model.NamespacedObjectReference) autoscalingv2.CrossVersionObjectReference {
	return autoscalingv2.CrossVersionObjectReference{
		Kind:       ref.Kind,
		Name:       ref.Name,
		APIVersion: ref.APIVersion(),
	}
}

// dpaNameFromKey extracts the DPA name from a store key ("namespace/name" → "name").
func dpaNameFromKey(key string) string {
	for i := len(key) - 1; i >= 0; i-- {
		if key[i] == '/' {
			return key[i+1:]
		}
	}
	return key
}
