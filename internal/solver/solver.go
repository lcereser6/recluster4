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
	"context"
	"sort"

	"sigs.k8s.io/controller-runtime/pkg/log"

	reclusteriov1 "github.com/lorenzocereser/recluster4/api/v1"
)

// Thresholds for algorithm selection
const (
	// ExactSolverMaxCandidates is the max feasible pairs for exact solver
	ExactSolverMaxCandidates = 20

	// HeuristicSolverMaxCandidates is the max feasible pairs for heuristic
	HeuristicSolverMaxCandidates = 100

	// ExactSolverMinTimePercent is minimum remaining time percent to use exact
	ExactSolverMinTimePercent = 0.50

	// HeuristicSolverMinTimePercent is minimum remaining time percent to use heuristic
	HeuristicSolverMinTimePercent = 0.25
)

// Solver is the main interface for the scheduling solver
type Solver interface {
	// Solve takes input and returns optimal pod-to-node assignments
	Solve(ctx context.Context, input SolverInput) SolverOutput
}

// DefaultSolver implements the Solver interface with regime switching
type DefaultSolver struct {
	greedy    *GreedySolver
	heuristic *HeuristicSolver
	exact     *ExactSolver
}

// NewSolver creates a new solver with all algorithm implementations
func NewSolver() *DefaultSolver {
	return &DefaultSolver{
		greedy:    NewGreedySolver(),
		heuristic: NewHeuristicSolver(),
		exact:     NewExactSolver(),
	}
}

// Solve determines which algorithm to use based on problem size and time budget,
// then executes the selected algorithm
func (s *DefaultSolver) Solve(ctx context.Context, input SolverInput) SolverOutput {
	logger := log.FromContext(ctx)

	// If no gated pods, nothing to do
	if len(input.GatedPods) == 0 {
		logger.V(1).Info("No gated pods to solve")
		return NewSolverOutput()
	}

	// If no candidate nodes, nothing can be assigned
	if len(input.RcNodes) == 0 {
		logger.Info("No RcNodes available for scheduling")
		output := NewSolverOutput()
		for _, pod := range input.GatedPods {
			output.UnassignedPods = append(output.UnassignedPods, UnassignedPod{
				PodNamespace: pod.Namespace,
				PodName:      pod.Pod.Name,
				Reason:       ReasonNoFeasibleNodes,
				Details:      "no RcNodes available",
			})
		}
		return output
	}

	// Count feasible pairs for algorithm selection
	candidateCount := CountFeasiblePairs(input.GatedPods, input.RcNodes)
	remainingTimePercent := input.Constraints.RemainingTimePercent()

	logger.Info("Solver analyzing problem",
		"pods", len(input.GatedPods),
		"nodes", len(input.RcNodes),
		"feasiblePairs", candidateCount,
		"remainingTimePercent", remainingTimePercent,
	)

	// Select and execute algorithm based on regime
	var output SolverOutput

	if candidateCount < ExactSolverMaxCandidates && remainingTimePercent > ExactSolverMinTimePercent {
		// Try exact solver for small problems with enough time
		logger.Info("Using exact solver", "candidateCount", candidateCount)
		output = s.exact.Solve(input, input.ActivePolicy)

		// Fall back to heuristic if exact failed
		if len(output.Assignments) == 0 && len(input.GatedPods) > 0 {
			logger.Info("Exact solver failed, falling back to heuristic")
			output = s.heuristic.Solve(input, input.ActivePolicy)
		}
	} else if candidateCount < HeuristicSolverMaxCandidates && remainingTimePercent > HeuristicSolverMinTimePercent {
		// Use heuristic for medium problems
		logger.Info("Using heuristic solver", "candidateCount", candidateCount)
		output = s.heuristic.Solve(input, input.ActivePolicy)
	} else {
		// Use greedy for large problems or time pressure
		logger.Info("Using greedy solver", "candidateCount", candidateCount)
		output = s.greedy.Solve(input, input.ActivePolicy)
	}

	logger.Info("Solver complete",
		"algorithm", output.Algorithm,
		"assigned", len(output.Assignments),
		"unassigned", len(output.UnassignedPods),
		"totalScore", output.TotalScore,
		"solveTime", output.SolveTime,
		"offlineNodesUsed", output.OfflineNodesUsed,
	)

	// Record timing metrics with the correct algorithm label
	if output.Timings != nil {
		output.Timings.RecordMetrics(output.Algorithm)
	}

	// Record solver output metrics
	RecordSolverMetrics(input, output)

	return output
}

// SelectPolicy chooses the highest priority policy that matches the pod's tag
func SelectPolicy(policies []reclusteriov1.RcPolicy, podTag string) *reclusteriov1.RcPolicy {
	if len(policies) == 0 {
		return nil
	}

	// Sort policies by priority descending
	sorted := make([]reclusteriov1.RcPolicy, len(policies))
	copy(sorted, policies)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Spec.Priority > sorted[j].Spec.Priority
	})

	// Find first matching policy
	for i := range sorted {
		policy := &sorted[i]

		// Check if policy matches the pod tag
		if matchesPolicyTag(policy, podTag) {
			return policy
		}
	}

	// Return highest priority policy as default
	return &sorted[0]
}

// matchesPolicyTag checks if a policy applies to a given pod tag
func matchesPolicyTag(policy *reclusteriov1.RcPolicy, podTag string) bool {
	// If policy has no tag selector, it matches everything
	if policy.Spec.DeploymentSelector.MatchLabels == nil {
		return true
	}

	// Check for recluster.io/policy-tag label
	if policyTag, exists := policy.Spec.DeploymentSelector.MatchLabels["recluster.io/policy-tag"]; exists {
		return policyTag == podTag
	}

	// If no specific tag matcher, policy matches all
	return true
}

// GroupPodsByPolicy groups pods by their applicable policy
func GroupPodsByPolicy(pods []PodCandidate, policies []reclusteriov1.RcPolicy) map[*reclusteriov1.RcPolicy][]PodCandidate {
	groups := make(map[*reclusteriov1.RcPolicy][]PodCandidate)

	for _, pod := range pods {
		policy := SelectPolicy(policies, pod.PolicyTag)
		if policy == nil {
			continue // Skip pods with no matching policy
		}

		// Use policy name as key for grouping
		groups[policy] = append(groups[policy], pod)
	}

	return groups
}
