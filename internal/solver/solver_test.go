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
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	reclusteriov1 "github.com/lorenzocereser/recluster4/api/v1"
)

func makeTestNode(name, cpu, memory string, ready, offline bool) RcNodeCandidate {
	cpuQty := resource.MustParse(cpu)
	memQty := resource.MustParse(memory)
	return RcNodeCandidate{
		RcNode: &reclusteriov1.RcNode{
			ObjectMeta: metav1.ObjectMeta{Name: name},
		},
		Name:              name,
		AllocatableCPU:    cpuQty,
		AllocatableMemory: memQty,
		RemainingCPU:      cpuQty.DeepCopy(),
		RemainingMemory:   memQty.DeepCopy(),
		IsReady:           ready,
		IsOffline:         offline,
		Features:          make(map[string]string),
	}
}

func makeTestPod(name, namespace, cpu, memory string) PodCandidate {
	return PodCandidate{
		Pod: &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
				UID:       types.UID(name + "-uid"),
			},
		},
		Namespace:     namespace,
		CPURequest:    resource.MustParse(cpu),
		MemoryRequest: resource.MustParse(memory),
		PolicyTag:     "default",
	}
}

func makeTestNodes(count int) []RcNodeCandidate {
	nodes := make([]RcNodeCandidate, count)
	for i := 0; i < count; i++ {
		ready := i%3 != 0
		offline := !ready
		nodes[i] = makeTestNode(
			fmt.Sprintf("node-%d", i),
			fmt.Sprintf("%d", 2+i%6),
			fmt.Sprintf("%dGi", 4+i%8),
			ready,
			offline,
		)
	}
	return nodes
}

func makeTestPods(count int) []PodCandidate {
	pods := make([]PodCandidate, count)
	for i := 0; i < count; i++ {
		pods[i] = makeTestPod(
			fmt.Sprintf("pod-%d", i),
			"default",
			fmt.Sprintf("%dm", 100+i%500),
			fmt.Sprintf("%dMi", 128+i%384),
		)
	}
	return pods
}

func TestGreedySolver_Basic(t *testing.T) {
	solver := NewGreedySolver()

	nodes := []RcNodeCandidate{
		makeTestNode("node-1", "4", "8Gi", true, false),
		makeTestNode("node-2", "2", "4Gi", true, false),
		makeTestNode("node-3", "8", "16Gi", false, true),
	}

	pods := []PodCandidate{
		makeTestPod("pod-1", "default", "500m", "512Mi"),
		makeTestPod("pod-2", "default", "1", "1Gi"),
		makeTestPod("pod-3", "default", "2", "2Gi"),
	}

	input := SolverInput{
		GatedPods: pods,
		RcNodes:   nodes,
		Constraints: SolverConstraints{
			Deadline:              time.Now().Add(10 * time.Second),
			MaxConcurrentPowerOns: 3,
			MaxMigrationsPerHour:  10,
			PlannerCadence:        30 * time.Second,
		},
	}

	output := solver.Solve(input, nil)

	if len(output.Assignments) != 3 {
		t.Errorf("Expected 3 assignments, got %d", len(output.Assignments))
	}
	if len(output.UnassignedPods) != 0 {
		t.Errorf("Expected 0 unassigned pods, got %d", len(output.UnassignedPods))
	}
	if output.Algorithm != AlgorithmGreedy {
		t.Errorf("Expected algorithm greedy, got %s", output.Algorithm)
	}

	t.Logf("Solver completed in %v", output.SolveTime)
	t.Logf("Total score: %.2f", output.TotalScore)
}

func TestGreedySolver_CapacityExhausted(t *testing.T) {
	solver := NewGreedySolver()

	nodes := []RcNodeCandidate{
		makeTestNode("small-node", "1", "1Gi", true, false),
	}

	pods := []PodCandidate{
		makeTestPod("pod-1", "default", "500m", "512Mi"),
		makeTestPod("pod-2", "default", "800m", "800Mi"),
	}

	input := SolverInput{
		GatedPods: pods,
		RcNodes:   nodes,
		Constraints: SolverConstraints{
			Deadline:              time.Now().Add(10 * time.Second),
			MaxConcurrentPowerOns: 3,
			MaxMigrationsPerHour:  10,
			PlannerCadence:        30 * time.Second,
		},
	}

	output := solver.Solve(input, nil)

	if len(output.Assignments) != 1 {
		t.Errorf("Expected 1 assignment, got %d", len(output.Assignments))
	}
	if len(output.UnassignedPods) != 1 {
		t.Errorf("Expected 1 unassigned pod, got %d", len(output.UnassignedPods))
	}
	if output.UnassignedPods[0].Reason != ReasonNoCapacity {
		t.Errorf("Expected reason %s, got %s", ReasonNoCapacity, output.UnassignedPods[0].Reason)
	}

	t.Logf("Unassigned pod: %s - %s", output.UnassignedPods[0].PodName, output.UnassignedPods[0].Details)
}

func TestGreedySolver_PowerBudget(t *testing.T) {
	solver := NewGreedySolver()

	// Each node can only hold ONE pod (2 cores, pod needs 2 cores)
	nodes := []RcNodeCandidate{
		makeTestNode("offline-1", "2", "4Gi", false, true),
		makeTestNode("offline-2", "2", "4Gi", false, true),
		makeTestNode("offline-3", "2", "4Gi", false, true),
	}

	// Each pod requires 2 cores, so needs its own node
	pods := []PodCandidate{
		makeTestPod("pod-1", "default", "2", "2Gi"),
		makeTestPod("pod-2", "default", "2", "2Gi"),
		makeTestPod("pod-3", "default", "2", "2Gi"),
	}

	input := SolverInput{
		GatedPods: pods,
		RcNodes:   nodes,
		Constraints: SolverConstraints{
			Deadline:              time.Now().Add(10 * time.Second),
			MaxConcurrentPowerOns: 2, // Can only power on 2
			MaxMigrationsPerHour:  10,
			PlannerCadence:        30 * time.Second,
		},
	}

	output := solver.Solve(input, nil)

	// Should assign 2 pods and leave 1 unassigned due to power budget
	if len(output.Assignments) != 2 {
		t.Errorf("Expected 2 assignments, got %d", len(output.Assignments))
	}
	if len(output.UnassignedPods) != 1 {
		t.Errorf("Expected 1 unassigned pod, got %d", len(output.UnassignedPods))
	}
	if output.OfflineNodesUsed != 2 {
		t.Errorf("Expected 2 offline nodes used, got %d", output.OfflineNodesUsed)
	}
	if len(output.UnassignedPods) > 0 && output.UnassignedPods[0].Reason != ReasonPowerBudget {
		t.Errorf("Expected reason %s, got %s", ReasonPowerBudget, output.UnassignedPods[0].Reason)
	}
}

func TestGreedySolver_PreferReadyNodes(t *testing.T) {
	solver := NewGreedySolver()

	nodes := []RcNodeCandidate{
		makeTestNode("offline-big", "16", "32Gi", false, true),
		makeTestNode("ready-small", "4", "8Gi", true, false),
		makeTestNode("offline-medium", "8", "16Gi", false, true),
	}

	pods := []PodCandidate{
		makeTestPod("pod-1", "default", "1", "1Gi"),
	}

	input := SolverInput{
		GatedPods: pods,
		RcNodes:   nodes,
		Constraints: SolverConstraints{
			Deadline:              time.Now().Add(10 * time.Second),
			MaxConcurrentPowerOns: 3,
			MaxMigrationsPerHour:  10,
			PlannerCadence:        30 * time.Second,
		},
	}

	output := solver.Solve(input, nil)

	if len(output.Assignments) != 1 {
		t.Fatalf("Expected 1 assignment, got %d", len(output.Assignments))
	}

	for _, assignment := range output.Assignments {
		if assignment.TargetNode != "ready-small" {
			t.Errorf("Expected assignment to ready-small, got %s", assignment.TargetNode)
		}
		if assignment.RequiresWake {
			t.Error("Should not require wake for ready node")
		}
	}

	if output.OfflineNodesUsed != 0 {
		t.Errorf("Expected 0 offline nodes used, got %d", output.OfflineNodesUsed)
	}
}

func TestDefaultSolver_RegimeSelection(t *testing.T) {
	solver := NewSolver()
	ctx := context.Background()

	testCases := []struct {
		name     string
		numPods  int
		numNodes int
		timePct  float64
	}{
		{"Small problem", 3, 3, 0.8},
		{"Medium problem", 10, 5, 0.5},
		{"Large problem", 50, 20, 0.5},
		{"Time pressure", 5, 5, 0.1},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			nodes := makeTestNodes(tc.numNodes)
			pods := makeTestPods(tc.numPods)

			cadence := 30 * time.Second
			deadline := time.Now().Add(time.Duration(float64(cadence) * tc.timePct))

			input := SolverInput{
				GatedPods: pods,
				RcNodes:   nodes,
				Constraints: SolverConstraints{
					Deadline:              deadline,
					MaxConcurrentPowerOns: 10,
					MaxMigrationsPerHour:  50,
					PlannerCadence:        cadence,
				},
			}

			output := solver.Solve(ctx, input)

			t.Logf("Algorithm: %s, Assigned: %d, Time: %v",
				output.Algorithm, len(output.Assignments), output.SolveTime)
		})
	}
}

func TestGreedySolver_Performance(t *testing.T) {
	solver := NewGreedySolver()

	sizes := []struct {
		pods  int
		nodes int
	}{
		{10, 5},
		{50, 20},
		{100, 50},
		{500, 100},
	}

	for _, size := range sizes {
		t.Run(fmt.Sprintf("pods=%d_nodes=%d", size.pods, size.nodes), func(t *testing.T) {
			nodes := makeTestNodes(size.nodes)
			pods := makeTestPods(size.pods)

			input := SolverInput{
				GatedPods: pods,
				RcNodes:   nodes,
				Constraints: SolverConstraints{
					Deadline:              time.Now().Add(10 * time.Second),
					MaxConcurrentPowerOns: 20,
					MaxMigrationsPerHour:  50,
					PlannerCadence:        30 * time.Second,
				},
			}

			start := time.Now()
			output := solver.Solve(input, nil)
			elapsed := time.Since(start)

			t.Logf("Pods: %d, Nodes: %d", size.pods, size.nodes)
			t.Logf("  Assigned: %d, Unassigned: %d", len(output.Assignments), len(output.UnassignedPods))
			t.Logf("  Solve Time: %v", output.SolveTime)
			t.Logf("  Wall Time: %v", elapsed)

			if elapsed > 100*time.Millisecond {
				t.Errorf("Solver too slow: %v (expected < 100ms)", elapsed)
			}
		})
	}
}

func TestScoring_Curves(t *testing.T) {
	testCases := []struct {
		curve     reclusteriov1.CurveType
		value     float64
		minExpect float64
		maxExpect float64
	}{
		{reclusteriov1.CurveTypeLinear, 100.0, 95, 105},
		{reclusteriov1.CurveTypeLogarithmic, 100.0, 0, 10},
		{reclusteriov1.CurveTypeExponential, 2.0, 3, 5}, // 2^2 = 4
		{reclusteriov1.CurveTypeSigmoid, 0.0, 0.4, 0.6},
		{reclusteriov1.CurveTypeStep, 5.0, 0.9, 1.1},
		{reclusteriov1.CurveTypeInverse, 10.0, 0.05, 0.15},
	}

	for _, tc := range testCases {
		t.Run(string(tc.curve), func(t *testing.T) {
			result := applyCurve(tc.value, tc.curve, reclusteriov1.CurveParameters{})
			if result < tc.minExpect || result > tc.maxExpect {
				t.Errorf("applyCurve(%v, %s) = %v, expected in [%v, %v]",
					tc.value, tc.curve, result, tc.minExpect, tc.maxExpect)
			}
			t.Logf("%s(%v) = %v", tc.curve, tc.value, result)
		})
	}
}

func TestSelectPolicy(t *testing.T) {
	policies := []reclusteriov1.RcPolicy{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "low-priority"},
			Spec:       reclusteriov1.RcPolicySpec{Priority: 1},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "high-priority"},
			Spec:       reclusteriov1.RcPolicySpec{Priority: 100},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "medium-priority"},
			Spec:       reclusteriov1.RcPolicySpec{Priority: 50},
		},
	}

	selected := SelectPolicy(policies, "default")
	if selected == nil {
		t.Fatal("Expected a policy to be selected")
	}

	if selected.Name != "high-priority" {
		t.Errorf("Expected high-priority policy, got %s", selected.Name)
	}

	t.Logf("Selected policy: %s (priority=%d)", selected.Name, selected.Spec.Priority)
}

// ── Heuristic solver tests ──────────────────────────────────────────────────

func TestHeuristicSolver_ImprovesOverGreedy(t *testing.T) {
	// Set up a scenario where the greedy FFD order is suboptimal.
	// Two big pods and one small pod, two nodes.
	// Greedy packs big-1 on node-A (best score), big-2 on node-B,
	// then small can go on either. Heuristic should find a better fit
	// or at least match greedy.
	solver := NewHeuristicSolver()

	nodes := []RcNodeCandidate{
		makeTestNode("node-A", "4", "8Gi", true, false),
		makeTestNode("node-B", "4", "8Gi", true, false),
	}

	pods := []PodCandidate{
		makeTestPod("big-1", "default", "3", "4Gi"),
		makeTestPod("big-2", "default", "3", "4Gi"),
		makeTestPod("small", "default", "500m", "512Mi"),
	}

	input := SolverInput{
		GatedPods: pods,
		RcNodes:   nodes,
		Constraints: SolverConstraints{
			Deadline:              time.Now().Add(500 * time.Millisecond),
			MaxConcurrentPowerOns: 5,
			MaxMigrationsPerHour:  10,
			PlannerCadence:        1 * time.Second,
		},
	}

	output := solver.Solve(input, nil)

	if output.Algorithm != AlgorithmHeuristic {
		t.Errorf("Expected algorithm heuristic, got %s", output.Algorithm)
	}
	if len(output.Assignments) != 3 {
		t.Errorf("Expected 3 assignments, got %d", len(output.Assignments))
	}
	if output.SolveTime > 600*time.Millisecond {
		t.Errorf("Heuristic took too long: %v", output.SolveTime)
	}
	t.Logf("Heuristic: assigned=%d, score=%.2f, time=%v",
		len(output.Assignments), output.TotalScore, output.SolveTime)
}

func TestHeuristicSolver_Consolidation(t *testing.T) {
	// Three nodes, each with one small pod — heuristic should consolidate
	// onto fewer nodes.
	solver := NewHeuristicSolver()

	nodes := []RcNodeCandidate{
		makeTestNode("node-A", "4", "8Gi", true, false),
		makeTestNode("node-B", "4", "8Gi", true, false),
		makeTestNode("node-C", "4", "8Gi", true, false),
	}

	pods := []PodCandidate{
		makeTestPod("pod-1", "default", "500m", "512Mi"),
		makeTestPod("pod-2", "default", "500m", "512Mi"),
		makeTestPod("pod-3", "default", "500m", "512Mi"),
	}

	input := SolverInput{
		GatedPods: pods,
		RcNodes:   nodes,
		Constraints: SolverConstraints{
			Deadline:              time.Now().Add(500 * time.Millisecond),
			MaxConcurrentPowerOns: 5,
			MaxMigrationsPerHour:  10,
			PlannerCadence:        1 * time.Second,
		},
	}

	output := solver.Solve(input, nil)

	if len(output.Assignments) != 3 {
		t.Fatalf("Expected 3 assignments, got %d", len(output.Assignments))
	}

	// All 3 pods should be on ≤ 2 nodes after consolidation (ideally 1)
	if len(output.NodesUsed) > 2 {
		t.Errorf("Expected consolidation to ≤ 2 nodes, got %d", len(output.NodesUsed))
	}
	t.Logf("Heuristic consolidation: %d nodes used (from 3 possible)", len(output.NodesUsed))
}

func TestHeuristicSolver_RespectsDeadline(t *testing.T) {
	solver := NewHeuristicSolver()

	nodes := makeTestNodes(10)
	pods := makeTestPods(20)

	deadline := 200 * time.Millisecond
	input := SolverInput{
		GatedPods: pods,
		RcNodes:   nodes,
		Constraints: SolverConstraints{
			Deadline:              time.Now().Add(deadline),
			MaxConcurrentPowerOns: 20,
			MaxMigrationsPerHour:  50,
			PlannerCadence:        deadline,
		},
	}

	output := solver.Solve(input, nil)

	if output.SolveTime > deadline+100*time.Millisecond {
		t.Errorf("Heuristic exceeded deadline: %v (limit %v)", output.SolveTime, deadline)
	}
	t.Logf("Heuristic: assigned=%d, time=%v (deadline=%v)",
		len(output.Assignments), output.SolveTime, deadline)
}

// ── Exact solver tests ──────────────────────────────────────────────────────

func TestExactSolver_FindsOptimal(t *testing.T) {
	exact := NewExactSolver()
	greedy := NewGreedySolver()

	// Scenario where greedy may not be optimal:
	// node-A: small, high score (ready)
	// node-B: large, low score (offline)
	// pod-big needs to go on node-B (only fit), pod-small can go on either.
	// Greedy assigns pod-small to node-A first (highest score), then pod-big to node-B.
	// Exact should find this or better.

	nodeA := makeTestNode("node-A", "2", "2Gi", true, false)
	nodeA.BaseScore = 200
	nodeA.NormalizedScore = 1.0

	nodeB := makeTestNode("node-B", "8", "16Gi", false, true)
	nodeB.BaseScore = 50
	nodeB.NormalizedScore = 0.25

	nodes := []RcNodeCandidate{nodeA, nodeB}

	pods := []PodCandidate{
		makeTestPod("pod-big", "default", "4", "8Gi"),
		makeTestPod("pod-small", "default", "1", "1Gi"),
	}

	input := SolverInput{
		GatedPods: pods,
		RcNodes:   nodes,
		Constraints: SolverConstraints{
			Deadline:              time.Now().Add(1 * time.Second),
			MaxConcurrentPowerOns: 5,
			MaxMigrationsPerHour:  10,
			PlannerCadence:        2 * time.Second,
		},
	}

	exactOutput := exact.Solve(input, nil)
	greedyOutput := greedy.Solve(input, nil)

	if exactOutput.Algorithm != AlgorithmExact {
		t.Errorf("Expected algorithm exact, got %s", exactOutput.Algorithm)
	}

	// Exact should assign at least as many pods as greedy
	if len(exactOutput.Assignments) < len(greedyOutput.Assignments) {
		t.Errorf("Exact assigned fewer pods (%d) than greedy (%d)",
			len(exactOutput.Assignments), len(greedyOutput.Assignments))
	}

	// Exact score should be >= greedy score
	if exactOutput.TotalScore < greedyOutput.TotalScore {
		t.Errorf("Exact score (%.2f) worse than greedy (%.2f)",
			exactOutput.TotalScore, greedyOutput.TotalScore)
	}

	t.Logf("Exact:  assigned=%d, score=%.2f, time=%v",
		len(exactOutput.Assignments), exactOutput.TotalScore, exactOutput.SolveTime)
	t.Logf("Greedy: assigned=%d, score=%.2f, time=%v",
		len(greedyOutput.Assignments), greedyOutput.TotalScore, greedyOutput.SolveTime)
}

func TestExactSolver_SmallProblem(t *testing.T) {
	solver := NewExactSolver()

	nodes := []RcNodeCandidate{
		makeTestNode("node-1", "4", "8Gi", true, false),
		makeTestNode("node-2", "2", "4Gi", true, false),
	}

	pods := []PodCandidate{
		makeTestPod("pod-1", "default", "1", "1Gi"),
		makeTestPod("pod-2", "default", "1", "1Gi"),
		makeTestPod("pod-3", "default", "1", "1Gi"),
	}

	input := SolverInput{
		GatedPods: pods,
		RcNodes:   nodes,
		Constraints: SolverConstraints{
			Deadline:              time.Now().Add(1 * time.Second),
			MaxConcurrentPowerOns: 5,
			MaxMigrationsPerHour:  10,
			PlannerCadence:        2 * time.Second,
		},
	}

	output := solver.Solve(input, nil)

	if len(output.Assignments) != 3 {
		t.Errorf("Expected 3 assignments, got %d", len(output.Assignments))
	}
	if output.SolveTime > 500*time.Millisecond {
		t.Errorf("Exact solver too slow for tiny problem: %v", output.SolveTime)
	}

	t.Logf("Exact: assigned=%d, score=%.2f, time=%v",
		len(output.Assignments), output.TotalScore, output.SolveTime)
}

func TestExactSolver_RespectsDeadline(t *testing.T) {
	solver := NewExactSolver()

	// Slightly larger problem that would take a while exhaustively
	nodes := makeTestNodes(6)
	pods := makeTestPods(8)

	deadline := 200 * time.Millisecond
	input := SolverInput{
		GatedPods: pods,
		RcNodes:   nodes,
		Constraints: SolverConstraints{
			Deadline:              time.Now().Add(deadline),
			MaxConcurrentPowerOns: 20,
			MaxMigrationsPerHour:  50,
			PlannerCadence:        deadline,
		},
	}

	output := solver.Solve(input, nil)

	// Should finish within deadline + generous margin (greedy seed + B&B + overhead)
	if output.SolveTime > deadline+150*time.Millisecond {
		t.Errorf("Exact solver exceeded deadline: %v (limit %v)", output.SolveTime, deadline)
	}

	// Should still produce valid assignments (at least greedy's result)
	if len(output.Assignments) == 0 && len(pods) > 0 {
		t.Error("Expected at least some assignments from exact solver")
	}

	t.Logf("Exact: assigned=%d, score=%.2f, time=%v (deadline=%v)",
		len(output.Assignments), output.TotalScore, output.SolveTime, deadline)
}

func TestExactSolver_PowerBudget(t *testing.T) {
	solver := NewExactSolver()

	nodes := []RcNodeCandidate{
		makeTestNode("offline-1", "4", "8Gi", false, true),
		makeTestNode("offline-2", "4", "8Gi", false, true),
		makeTestNode("offline-3", "4", "8Gi", false, true),
	}

	pods := []PodCandidate{
		makeTestPod("pod-1", "default", "2", "2Gi"),
		makeTestPod("pod-2", "default", "2", "2Gi"),
		makeTestPod("pod-3", "default", "2", "2Gi"),
	}

	input := SolverInput{
		GatedPods: pods,
		RcNodes:   nodes,
		Constraints: SolverConstraints{
			Deadline:              time.Now().Add(1 * time.Second),
			MaxConcurrentPowerOns: 2, // Only 2 allowed
			MaxMigrationsPerHour:  10,
			PlannerCadence:        2 * time.Second,
		},
	}

	output := solver.Solve(input, nil)

	// Should respect power budget: max 2 nodes powered on
	if output.OfflineNodesUsed > 2 {
		t.Errorf("Expected ≤ 2 offline nodes used, got %d", output.OfflineNodesUsed)
	}

	t.Logf("Exact with power budget: assigned=%d, offline=%d, score=%.2f",
		len(output.Assignments), output.OfflineNodesUsed, output.TotalScore)
}
