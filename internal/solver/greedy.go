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
	"sort"
	"time"

	reclusteriov1 "github.com/lorenzocereser/recluster4/api/v1"
)

// GreedySolver implements fast single-pass greedy assignment
type GreedySolver struct{}

// NewGreedySolver creates a new greedy solver instance
func NewGreedySolver() *GreedySolver {
	return &GreedySolver{}
}

// Solve performs greedy pod-to-node assignment
// Algorithm:
// 1. Sort pods by resource demand (CPU desc, Memory desc)
// 2. For each pod, find the highest-scoring feasible node
// 3. Assign and update remaining capacity
func (g *GreedySolver) Solve(input SolverInput, policy *reclusteriov1.RcPolicy) SolverOutput {
	startTime := time.Now()
	timings := &Timings{}
	output := NewSolverOutput()
	output.Algorithm = AlgorithmGreedy

	// Make a copy of nodes for capacity tracking
	nodes := make([]RcNodeCandidate, len(input.RcNodes))
	copy(nodes, input.RcNodes)

	// Time probe: Scoring
	scoringStart := time.Now()
	// Pre-compute base scores for all nodes
	for i := range nodes {
		nodes[i].BaseScore = ComputeNodeBaseScore(&nodes[i], policy)
	}
	NormalizeScores(nodes)
	timings.Scoring = time.Since(scoringStart)

	// Time probe: Sorting
	sortingStart := time.Now()
	// Sort pods by resource demand (largest first - FFD approach)
	pods := make([]PodCandidate, len(input.GatedPods))
	copy(pods, input.GatedPods)
	sortPodsByDemand(pods)

	// Sort nodes: Ready first, then by score descending
	sortNodesByPreference(nodes)
	timings.Sorting = time.Since(sortingStart)

	// Track power-ons for budget
	powerOnsUsed := input.Constraints.CurrentPowerOnsInFlight

	// Time probe: Assignment
	assignmentStart := time.Now()

	// Greedy assignment
	for _, pod := range pods {
		assigned := false

		// Try to find a feasible node
		for i := range nodes {
			node := &nodes[i]

			// Check capacity
			if !CanFit(pod, *node) {
				continue
			}

			// Check power budget for offline nodes
			if node.IsOffline && !node.IsReady {
				if powerOnsUsed >= input.Constraints.MaxConcurrentPowerOns {
					continue // Can't use this node, budget exhausted
				}
			}

			// Check migration budget for rebalancing moves
			if pod.IsRebalancingMove && !input.Constraints.CanMigrate() {
				output.UnassignedPods = append(output.UnassignedPods, UnassignedPod{
					PodNamespace: pod.Namespace,
					PodName:      pod.Pod.Name,
					Reason:       ReasonMigrationBudget,
					Details:      "migration budget exhausted",
				})
				assigned = true // Mark as handled (not assigned, but processed)
				break
			}

			// Compute assignment score
			score := ComputeAssignmentScore(&pod, node, policy)

			// Create assignment
			assignment := Assignment{
				PodNamespace: pod.Namespace,
				PodName:      pod.Pod.Name,
				PodUID:       string(pod.Pod.UID),
				TargetNode:   node.Name,
				Score:        score,
				RequiresWake: node.IsOffline && !node.IsReady,
			}

			// Update tracking
			output.Assignments[assignment.PodUID] = assignment
			output.TotalScore += score
			output.NodesUsed[node.Name] = true

			// Update node capacity
			node.RemainingCPU.Sub(pod.CPURequest)
			node.RemainingMemory.Sub(pod.MemoryRequest)

			// Track power-on if needed
			if assignment.RequiresWake {
				powerOnsUsed++
				output.OfflineNodesUsed++
				// Mark node as "will be ready" for subsequent assignments
				node.IsReady = true
				node.IsOffline = false
			}

			assigned = true
			break
		}

		// Record unassigned pods
		if !assigned {
			reason := ReasonNoCapacity
			details := "no node with sufficient capacity"

			// Check if there were feasible nodes but budget blocked them
			hasFeasibleOffline := false
			for _, node := range nodes {
				if CanFit(pod, node) && node.IsOffline {
					hasFeasibleOffline = true
					break
				}
			}
			if hasFeasibleOffline && powerOnsUsed >= input.Constraints.MaxConcurrentPowerOns {
				reason = ReasonPowerBudget
				details = "power budget exhausted, offline nodes available but can't be woken"
			}

			output.UnassignedPods = append(output.UnassignedPods, UnassignedPod{
				PodNamespace: pod.Namespace,
				PodName:      pod.Pod.Name,
				Reason:       reason,
				Details:      details,
			})
		}
	}

	timings.Assignment = time.Since(assignmentStart)
	timings.Total = time.Since(startTime)
	output.SolveTime = timings.Total
	output.Timings = timings

	return output
}

// sortPodsByDemand sorts pods by resource demand descending (CPU primary, memory secondary)
func sortPodsByDemand(pods []PodCandidate) {
	sort.Slice(pods, func(i, j int) bool {
		// Compare CPU first
		cpuCmp := pods[i].CPURequest.Cmp(pods[j].CPURequest)
		if cpuCmp != 0 {
			return cpuCmp > 0 // Descending
		}
		// Then memory
		memCmp := pods[i].MemoryRequest.Cmp(pods[j].MemoryRequest)
		if memCmp != 0 {
			return memCmp > 0 // Descending
		}
		// Stable sort by name
		return pods[i].Pod.Name < pods[j].Pod.Name
	})
}

// sortNodesByPreference sorts nodes: Ready first, then by score descending
func sortNodesByPreference(nodes []RcNodeCandidate) {
	sort.Slice(nodes, func(i, j int) bool {
		// Ready nodes first
		if nodes[i].IsReady != nodes[j].IsReady {
			return nodes[i].IsReady
		}
		// Then by normalized score descending
		if nodes[i].NormalizedScore != nodes[j].NormalizedScore {
			return nodes[i].NormalizedScore > nodes[j].NormalizedScore
		}
		// Stable sort by name
		return nodes[i].Name < nodes[j].Name
	})
}
