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

package solver

import (
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	reclusteriov1 "github.com/lorenzocereser/recluster4/api/v1"
)

// Algorithm represents which solving algorithm was used
type Algorithm string

const (
	AlgorithmExact     Algorithm = "exact"
	AlgorithmHeuristic Algorithm = "heuristic"
	AlgorithmGreedy    Algorithm = "greedy"
)

// zeroQuantity is a zero resource.Quantity used for comparisons.
var zeroQuantity = resource.MustParse("0")

// UnassignedReason explains why a pod couldn't be assigned
type UnassignedReason string

const (
	ReasonNoCapacity       UnassignedReason = "no-capacity"
	ReasonPowerBudget      UnassignedReason = "power-budget-exhausted"
	ReasonMigrationBudget  UnassignedReason = "migration-budget-exhausted"
	ReasonNoFeasibleNodes  UnassignedReason = "no-feasible-nodes"
	ReasonNodeSelectorFail UnassignedReason = "node-selector-mismatch"
)

// SolverInput contains all data needed for the solver to make decisions
type SolverInput struct {
	// GatedPods are pods waiting for scheduling decisions
	GatedPods []PodCandidate

	// RcNodes are candidate nodes for placement
	RcNodes []RcNodeCandidate

	// ActivePolicy is the effective policy for this solve
	// If multiple policies apply, the highest priority one is used
	ActivePolicy *reclusteriov1.RcPolicy

	// Constraints define budgets and limits
	Constraints SolverConstraints
}

// PodCandidate represents a pod awaiting placement
type PodCandidate struct {
	// Pod is the original Kubernetes pod
	Pod *corev1.Pod

	// CPURequest is the total CPU requested by all containers
	CPURequest resource.Quantity

	// MemoryRequest is the total memory requested by all containers
	MemoryRequest resource.Quantity

	// OwnerKind is the type of owner (Deployment, StatefulSet, etc.)
	OwnerKind string

	// OwnerName is the name of the owning workload
	OwnerName string

	// Namespace of the pod
	Namespace string

	// IsRebalancingMove indicates if this pod is being migrated
	IsRebalancingMove bool

	// PreferredNode is a hint from rebalancing (bonus score if assigned here)
	PreferredNode string

	// PolicyTag is the workload category label (default, batch, critical, etc.)
	PolicyTag string
}

// RcNodeCandidate represents an RcNode with runtime capacity tracking
type RcNodeCandidate struct {
	// RcNode is the original RcNode resource
	RcNode *reclusteriov1.RcNode

	// Name for quick access
	Name string

	// AllocatableCPU from RcNode spec
	AllocatableCPU resource.Quantity

	// AllocatableMemory from RcNode spec
	AllocatableMemory resource.Quantity

	// RemainingCPU after accounting for assigned pods
	RemainingCPU resource.Quantity

	// RemainingMemory after accounting for assigned pods
	RemainingMemory resource.Quantity

	// BaseScore pre-computed from policy multipliers
	BaseScore float64

	// NormalizedScore in [0, 1] range across fleet
	NormalizedScore float64

	// IsReady indicates if the node is currently online
	IsReady bool

	// IsOffline indicates if the node needs to be woken
	IsOffline bool

	// BootTimeSeconds estimated time to boot
	BootTimeSeconds int

	// IdleWatts power consumption when idle
	IdleWatts int

	// Features map for quick lookup
	Features map[string]string
}

// SolverConstraints define budgets and limits for the solver
type SolverConstraints struct {
	// Deadline is the absolute time by which solver must return
	Deadline time.Time

	// MaxConcurrentPowerOns limits simultaneous node boots
	MaxConcurrentPowerOns int

	// CurrentPowerOnsInFlight tracks nodes currently booting
	CurrentPowerOnsInFlight int

	// MaxMigrationsPerHour limits pod migrations
	MaxMigrationsPerHour int

	// MigrationsThisHour tracks migrations done
	MigrationsThisHour int

	// PlannerCadence is the total time budget (for calculating percentages)
	PlannerCadence time.Duration
}

// SolverOutput contains the solver's decisions
type SolverOutput struct {
	// Assignments maps pod UID to target RcNode name
	Assignments map[string]Assignment

	// UnassignedPods lists pods that couldn't be placed
	UnassignedPods []UnassignedPod

	// Algorithm indicates which algorithm was used
	Algorithm Algorithm

	// SolveTime is how long the solve took
	SolveTime time.Duration

	// Timings captures detailed timing breakdown for metrics
	Timings *Timings `json:"-"`

	// TotalScore is the sum of assignment scores
	TotalScore float64

	// NodesUsed tracks which nodes have assignments
	NodesUsed map[string]bool

	// OfflineNodesUsed counts how many offline nodes were selected
	OfflineNodesUsed int
}

// Assignment represents a pod-to-node assignment
type Assignment struct {
	// PodNamespace is the pod's namespace
	PodNamespace string

	// PodName is the pod's name
	PodName string

	// PodUID is the pod's UID for patching
	PodUID string

	// TargetNode is the RcNode to assign to
	TargetNode string

	// Score is the assignment score
	Score float64

	// RequiresWake indicates if the target node is offline
	RequiresWake bool
}

// UnassignedPod represents a pod that couldn't be placed
type UnassignedPod struct {
	// PodNamespace is the pod's namespace
	PodNamespace string

	// PodName is the pod's name
	PodName string

	// Reason explains why placement failed
	Reason UnassignedReason

	// Details provides additional context
	Details string
}

// NewSolverOutput creates an initialized SolverOutput
func NewSolverOutput() SolverOutput {
	return SolverOutput{
		Assignments:    make(map[string]Assignment),
		UnassignedPods: make([]UnassignedPod, 0),
		NodesUsed:      make(map[string]bool),
	}
}

// CanPowerOn checks if the power budget allows turning on another node
func (c *SolverConstraints) CanPowerOn() bool {
	return c.CurrentPowerOnsInFlight < c.MaxConcurrentPowerOns
}

// CanMigrate checks if the migration budget allows another migration
func (c *SolverConstraints) CanMigrate() bool {
	return c.MigrationsThisHour < c.MaxMigrationsPerHour
}

// RemainingTime returns time left until deadline
func (c *SolverConstraints) RemainingTime() time.Duration {
	return time.Until(c.Deadline)
}

// RemainingTimePercent returns remaining time as percentage of total budget
func (c *SolverConstraints) RemainingTimePercent() float64 {
	if c.PlannerCadence == 0 {
		return 0
	}
	remaining := c.RemainingTime()
	if remaining < 0 {
		return 0
	}
	return float64(remaining) / float64(c.PlannerCadence)
}
