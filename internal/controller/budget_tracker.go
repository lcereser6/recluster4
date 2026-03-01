/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	reclusteriov1 "github.com/lorenzocereser/recluster4/api/v1"
)

// BudgetTracker tracks power and migration budgets
type BudgetTracker struct {
	mu sync.RWMutex

	// Power-on tracking
	powerOnsInFlight map[string]time.Time // nodeName -> startTime
	powerOnsThisHour int
	powerOnHourStart time.Time

	// Migration tracking
	migrationsThisHour int
	migrationHourStart time.Time

	// Limits (can be overridden by policy)
	defaultMaxConcurrentPowerOns int
	defaultMaxMigrationsPerHour  int
	defaultMaxPowerOnsPerHour    int
}

// NewBudgetTracker creates a new budget tracker with default limits
func NewBudgetTracker() *BudgetTracker {
	now := time.Now()
	return &BudgetTracker{
		powerOnsInFlight:             make(map[string]time.Time),
		powerOnHourStart:             now,
		migrationHourStart:           now,
		defaultMaxConcurrentPowerOns: 3,
		defaultMaxMigrationsPerHour:  10,
		defaultMaxPowerOnsPerHour:    20,
	}
}

// GetLimits returns the effective limits, considering policy overrides
func (b *BudgetTracker) GetLimits(policy *reclusteriov1.RcPolicy) (maxConcurrent, maxMigrations, maxPowerOns int) {
	maxConcurrent = b.defaultMaxConcurrentPowerOns
	maxMigrations = b.defaultMaxMigrationsPerHour
	maxPowerOns = b.defaultMaxPowerOnsPerHour

	if policy != nil {
		if policy.Spec.Budgets.MaxConcurrentPowerOns > 0 {
			maxConcurrent = policy.Spec.Budgets.MaxConcurrentPowerOns
		}
		if policy.Spec.Budgets.MaxMigrationsPerHour > 0 {
			maxMigrations = policy.Spec.Budgets.MaxMigrationsPerHour
		}
		if policy.Spec.Budgets.MaxPowerOnsPerHour > 0 {
			maxPowerOns = policy.Spec.Budgets.MaxPowerOnsPerHour
		}
	}

	return
}

// GetCurrentState returns the current budget consumption
func (b *BudgetTracker) GetCurrentState() (powerOnsInFlight, migrationsThisHour int) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.resetHourlyIfNeededLocked()
	return len(b.powerOnsInFlight), b.migrationsThisHour
}

// CanPowerOn checks if we can power on another node
func (b *BudgetTracker) CanPowerOn(policy *reclusteriov1.RcPolicy) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.resetHourlyIfNeededLocked()
	maxConcurrent, _, maxPerHour := b.GetLimits(policy)

	return len(b.powerOnsInFlight) < maxConcurrent && b.powerOnsThisHour < maxPerHour
}

// RecordPowerOn records that we're starting to power on a node
func (b *BudgetTracker) RecordPowerOn(nodeName string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.resetHourlyIfNeededLocked()
	b.powerOnsInFlight[nodeName] = time.Now()
	b.powerOnsThisHour++
}

// CompletePowerOn records that a node has finished booting
func (b *BudgetTracker) CompletePowerOn(nodeName string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	delete(b.powerOnsInFlight, nodeName)
}

// CanMigrate checks if we can perform another migration
func (b *BudgetTracker) CanMigrate(policy *reclusteriov1.RcPolicy) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.resetHourlyIfNeededLocked()
	_, maxMigrations, _ := b.GetLimits(policy)

	return b.migrationsThisHour < maxMigrations
}

// RecordMigration records that we're performing a migration
func (b *BudgetTracker) RecordMigration() {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.resetHourlyIfNeededLocked()
	b.migrationsThisHour++
}

// resetHourlyIfNeededLocked resets hourly counters if an hour has passed
// Must be called with lock held
func (b *BudgetTracker) resetHourlyIfNeededLocked() {
	now := time.Now()

	if now.Sub(b.powerOnHourStart) >= time.Hour {
		b.powerOnsThisHour = 0
		b.powerOnHourStart = now
	}

	if now.Sub(b.migrationHourStart) >= time.Hour {
		b.migrationsThisHour = 0
		b.migrationHourStart = now
	}
}

// SyncWithCluster updates the tracker based on actual cluster state
// This helps recover from controller restarts
func (b *BudgetTracker) SyncWithCluster(ctx context.Context, c client.Client) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	// Get all RcNodes
	rcNodes := &reclusteriov1.RcNodeList{}
	if err := c.List(ctx, rcNodes); err != nil {
		return err
	}

	// Track nodes currently booting
	newInFlight := make(map[string]time.Time)
	for _, rcNode := range rcNodes.Items {
		if rcNode.Status.CurrentPhase == reclusteriov1.NodePhaseBooting {
			// Preserve existing start time if we know it
			if startTime, exists := b.powerOnsInFlight[rcNode.Name]; exists {
				newInFlight[rcNode.Name] = startTime
			} else {
				newInFlight[rcNode.Name] = time.Now()
			}
		}
	}
	b.powerOnsInFlight = newInFlight

	return nil
}

// CountBootingNodes counts nodes currently in Booting phase
func CountBootingNodes(rcNodes []reclusteriov1.RcNode) int {
	count := 0
	for _, rcNode := range rcNodes {
		if rcNode.Status.CurrentPhase == reclusteriov1.NodePhaseBooting {
			count++
		}
	}
	return count
}

// CountReadyRcNodes counts RcNodes that have a corresponding ready K8s node
func CountReadyRcNodes(rcNodes []reclusteriov1.RcNode, k8sNodes []corev1.Node) int {
	// Build map of ready K8s node names
	readyNodes := make(map[string]bool)
	for _, node := range k8sNodes {
		for _, cond := range node.Status.Conditions {
			if cond.Type == corev1.NodeReady && cond.Status == corev1.ConditionTrue {
				readyNodes[node.Name] = true
				break
			}
		}
	}

	// Count RcNodes with matching ready K8s node
	count := 0
	for _, rcNode := range rcNodes {
		if readyNodes[rcNode.Name] {
			count++
		}
	}
	return count
}
