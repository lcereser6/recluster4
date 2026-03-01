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

	reclusteriov1 "github.com/lorenzocereser/recluster4/api/v1"
)

// ExactSolver implements branch-and-bound optimal assignment for small problems
// (< 20 feasible pod×node pairs). It enumerates possible placements, using an
// upper-bound estimate to prune branches that cannot beat the best known solution.
type ExactSolver struct {
	greedy *GreedySolver
}

// NewExactSolver creates a new exact solver
func NewExactSolver() *ExactSolver {
	return &ExactSolver{
		greedy: NewGreedySolver(),
	}
}

// Solve finds the optimal pod-to-node assignment via depth-first branch-and-bound.
// If the search times out it returns the best solution found so far (which is at
// least as good as greedy since greedy seeds the incumbent).
func (e *ExactSolver) Solve(input SolverInput, policy *reclusteriov1.RcPolicy) SolverOutput {
	startTime := time.Now()

	// ── Seed the incumbent with greedy ────────────────────────────────
	greedyOutput := e.greedy.Solve(input, policy)

	// Safety margin: stop searching 50ms before deadline
	safetyMargin := 50 * time.Millisecond
	deadline := input.Constraints.Deadline.Add(-safetyMargin)

	// If greedy already used nearly all the time, just return it
	if time.Now().After(deadline) {
		greedyOutput.Algorithm = AlgorithmExact
		greedyOutput.SolveTime = time.Since(startTime)
		if greedyOutput.Timings != nil {
			greedyOutput.Timings.Total = greedyOutput.SolveTime
		}
		return greedyOutput
	}

	// ── Prepare node copies with full capacity for B&B ────────────────
	nodes := make([]RcNodeCandidate, len(input.RcNodes))
	copy(nodes, input.RcNodes)
	for i := range nodes {
		nodes[i].RemainingCPU = nodes[i].AllocatableCPU.DeepCopy()
		nodes[i].RemainingMemory = nodes[i].AllocatableMemory.DeepCopy()
		nodes[i].BaseScore = ComputeNodeBaseScore(&nodes[i], policy)
	}
	NormalizeScores(nodes)

	// Sort pods by demand descending (largest first → prune early)
	pods := make([]PodCandidate, len(input.GatedPods))
	copy(pods, input.GatedPods)
	sortPodsByDemand(pods)

	// Pre-compute per-(pod,node) assignment scores for fast lookup
	nPods := len(pods)
	nNodes := len(nodes)
	scoreMatrix := make([][]float64, nPods)
	for p := range pods {
		scoreMatrix[p] = make([]float64, nNodes)
		for n := range nodes {
			scoreMatrix[p][n] = ComputeAssignmentScore(&pods[p], &nodes[n], policy)
		}
	}

	// Pre-compute optimistic upper bound for each pod (best possible score)
	podBestScore := make([]float64, nPods)
	for p := range pods {
		best := 0.0
		for n := range nodes {
			if scoreMatrix[p][n] > best {
				best = scoreMatrix[p][n]
			}
		}
		podBestScore[p] = best
	}

	// Suffix sums of podBestScore for bounding: suffixBound[i] = sum of podBestScore[i:]
	suffixBound := make([]float64, nPods+1)
	for i := nPods - 1; i >= 0; i-- {
		suffixBound[i] = suffixBound[i+1] + podBestScore[i]
	}

	// ── B&B state ─────────────────────────────────────────────────────
	state := &bbState{
		// Current path
		assignments:  make([]int, nPods), // node index per pod (-1 = unassigned)
		currentScore: 0,
		nodesUsed:    make(map[int]bool),
		powerOnsUsed: input.Constraints.CurrentPowerOnsInFlight,
		maxPowerOns:  input.Constraints.MaxConcurrentPowerOns,

		// Best solution (seeded from greedy)
		bestScore:  greedyOutput.TotalScore,
		bestAssign: make([]int, nPods),

		// Data
		pods:        pods,
		nodes:       nodes,
		scoreMatrix: scoreMatrix,
		suffixBound: suffixBound,
		deadline:    deadline,
		timedOut:    false,
		explored:    0,
		policy:      policy,
	}

	// Initialise assignments to -1 (unassigned)
	for i := range state.assignments {
		state.assignments[i] = -1
	}
	// Seed bestAssign from greedy output
	for i := range state.bestAssign {
		state.bestAssign[i] = -1
	}
	// Map greedy assignments to pod/node indices
	podIndex := make(map[string]int, nPods)
	for i, p := range pods {
		podIndex[string(p.Pod.UID)] = i
	}
	nodeIndex := make(map[string]int, nNodes)
	for i, n := range nodes {
		nodeIndex[n.Name] = i
	}
	for _, a := range greedyOutput.Assignments {
		if pi, ok := podIndex[a.PodUID]; ok {
			if ni, ok := nodeIndex[a.TargetNode]; ok {
				state.bestAssign[pi] = ni
			}
		}
	}

	// ── Run B&B ───────────────────────────────────────────────────────
	e.branch(state, 0)

	// ── Build output from best solution ───────────────────────────────
	output := NewSolverOutput()
	output.Algorithm = AlgorithmExact
	output.TotalScore = state.bestScore

	for p := 0; p < nPods; p++ {
		ni := state.bestAssign[p]
		if ni < 0 {
			output.UnassignedPods = append(output.UnassignedPods, UnassignedPod{
				PodNamespace: pods[p].Namespace,
				PodName:      pods[p].Pod.Name,
				Reason:       ReasonNoCapacity,
				Details:      "exact solver could not place this pod in optimal solution",
			})
			continue
		}
		node := &nodes[ni]
		assignment := Assignment{
			PodNamespace: pods[p].Namespace,
			PodName:      pods[p].Pod.Name,
			PodUID:       string(pods[p].Pod.UID),
			TargetNode:   node.Name,
			Score:        state.scoreMatrix[p][ni],
			RequiresWake: node.IsOffline,
		}
		output.Assignments[assignment.PodUID] = assignment
		output.NodesUsed[node.Name] = true
	}

	// Count offline nodes used (per unique node, not per assignment)
	for nodeName := range output.NodesUsed {
		if ni, ok := nodeIndex[nodeName]; ok && nodes[ni].IsOffline {
			output.OfflineNodesUsed++
		}
	}

	output.SolveTime = time.Since(startTime)
	output.Timings = greedyOutput.Timings
	if output.Timings != nil {
		output.Timings.Total = output.SolveTime
	}
	return output
}

// ── Branch-and-bound internals ───────────────────────────────────────────────

// bbState holds mutable state for the B&B search
type bbState struct {
	// Current partial solution
	assignments  []int // pod index → node index (-1 = skip)
	currentScore float64
	nodesUsed    map[int]bool // node indices currently in use
	powerOnsUsed int

	// Best complete solution
	bestScore  float64
	bestAssign []int

	// Problem data (immutable during search)
	pods        []PodCandidate
	nodes       []RcNodeCandidate
	scoreMatrix [][]float64
	suffixBound []float64
	maxPowerOns int
	deadline    time.Time
	policy      *reclusteriov1.RcPolicy

	// Stats
	timedOut bool
	explored int64
}

// branch explores the search tree starting from podIdx.
func (e *ExactSolver) branch(s *bbState, podIdx int) {
	// Time check every 256 nodes to avoid syscall overhead
	s.explored++
	if s.explored&0xFF == 0 {
		if time.Now().After(s.deadline) {
			s.timedOut = true
			return
		}
	}

	// Base case: all pods considered
	if podIdx >= len(s.pods) {
		if s.currentScore > s.bestScore {
			s.bestScore = s.currentScore
			copy(s.bestAssign, s.assignments)
		}
		return
	}

	// Pruning: even if all remaining pods get their best possible score,
	// can we beat the incumbent?
	upperBound := s.currentScore + s.suffixBound[podIdx]
	if upperBound <= s.bestScore {
		return
	}

	pod := &s.pods[podIdx]

	// Try assigning to each feasible node
	for ni := range s.nodes {
		if s.timedOut {
			return
		}

		node := &s.nodes[ni]

		// Capacity check
		if pod.CPURequest.Cmp(node.RemainingCPU) > 0 {
			continue
		}
		if pod.MemoryRequest.Cmp(node.RemainingMemory) > 0 {
			continue
		}

		// Power budget check for offline nodes
		if node.IsOffline && !s.nodesUsed[ni] {
			if s.powerOnsUsed >= s.maxPowerOns {
				continue
			}
		}

		score := s.scoreMatrix[podIdx][ni]

		// Make assignment
		s.assignments[podIdx] = ni
		s.currentScore += score
		node.RemainingCPU.Sub(pod.CPURequest)
		node.RemainingMemory.Sub(pod.MemoryRequest)
		wasUsed := s.nodesUsed[ni]
		powerOnDelta := 0
		if node.IsOffline && !wasUsed {
			s.nodesUsed[ni] = true
			s.powerOnsUsed++
			powerOnDelta = 1
		} else {
			s.nodesUsed[ni] = true
		}

		// Recurse
		e.branch(s, podIdx+1)

		// Undo
		s.assignments[podIdx] = -1
		s.currentScore -= score
		node.RemainingCPU.Add(pod.CPURequest)
		node.RemainingMemory.Add(pod.MemoryRequest)
		if !wasUsed {
			delete(s.nodesUsed, ni)
		}
		s.powerOnsUsed -= powerOnDelta
	}

	// Also try leaving this pod unassigned (it may yield a better overall
	// score if assigning it forces worse choices for bigger pods later).
	s.assignments[podIdx] = -1
	e.branch(s, podIdx+1)
}
