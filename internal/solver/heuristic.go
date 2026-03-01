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
	// resource.Quantity methods used via types imported from types.go
)

// HeuristicSolver implements FFD constructive heuristic + local search improvement.
// Phase 1: Build an initial solution with the greedy FFD solver.
// Phase 2: Iteratively improve via three move types (swap, move, consolidate)
//
//	until no improving move is found or the time budget runs out.
type HeuristicSolver struct {
	greedy *GreedySolver
}

// NewHeuristicSolver creates a new heuristic solver
func NewHeuristicSolver() *HeuristicSolver {
	return &HeuristicSolver{
		greedy: NewGreedySolver(),
	}
}

// Solve performs heuristic pod-to-node assignment
func (h *HeuristicSolver) Solve(input SolverInput, policy *reclusteriov1.RcPolicy) SolverOutput {
	startTime := time.Now()

	// Phase 1: Constructive solution via greedy FFD
	output := h.greedy.Solve(input, policy)
	output.Algorithm = AlgorithmHeuristic

	if len(output.Assignments) < 2 {
		// Nothing to improve with < 2 assignments
		output.SolveTime = time.Since(startTime)
		if output.Timings != nil {
			output.Timings.Total = output.SolveTime
		}
		return output
	}

	// Phase 2: Local search improvement
	safetyMargin := 50 * time.Millisecond
	deadline := input.Constraints.Deadline.Add(-safetyMargin)

	// Build working state from greedy output
	state := h.buildSearchState(input, output, policy)

	// Iterate improvement passes until no improvement or out of time
	improved := true
	for improved && time.Now().Before(deadline) {
		improved = false

		// Try moves in priority order: consolidate > swap > relocate
		if time.Now().Before(deadline) {
			if h.tryConsolidate(&state, deadline) {
				improved = true
			}
		}
		if time.Now().Before(deadline) {
			if h.trySwaps(&state, deadline) {
				improved = true
			}
		}
		if time.Now().Before(deadline) {
			if h.tryRelocations(&state, deadline) {
				improved = true
			}
		}
	}

	// Rebuild output from final state
	output = h.stateToOutput(state, output)
	output.Algorithm = AlgorithmHeuristic
	output.SolveTime = time.Since(startTime)
	if output.Timings != nil {
		output.Timings.Total = output.SolveTime
	}
	return output
}

// ── Working state for local search ───────────────────────────────────────────

// searchState holds mutable state for local search iterations.
type searchState struct {
	// podAssignment maps pod UID → node name
	podAssignment map[string]string
	// podByUID maps pod UID → PodCandidate (immutable reference)
	podByUID map[string]*PodCandidate
	// nodeByName maps node name → index into nodes slice
	nodeByName map[string]int
	// nodes is a mutable copy of candidates with remaining capacity
	nodes []RcNodeCandidate
	// scores maps pod UID → current assignment score
	scores map[string]float64
	// totalScore is the sum of all scores
	totalScore float64
	// nodesUsed tracks which nodes have at least one pod
	nodesUsed map[string]bool
	// offlineNodesUsed counts offline nodes turned on
	offlineNodesUsed int
	// policy for re-scoring
	policy *reclusteriov1.RcPolicy
	// unassigned pods carried from greedy
	unassigned []UnassignedPod
}

func (h *HeuristicSolver) buildSearchState(input SolverInput, output SolverOutput, policy *reclusteriov1.RcPolicy) searchState {
	s := searchState{
		podAssignment: make(map[string]string, len(output.Assignments)),
		podByUID:      make(map[string]*PodCandidate, len(input.GatedPods)),
		nodeByName:    make(map[string]int, len(input.RcNodes)),
		nodes:         make([]RcNodeCandidate, len(input.RcNodes)),
		scores:        make(map[string]float64, len(output.Assignments)),
		nodesUsed:     make(map[string]bool),
		policy:        policy,
		unassigned:    output.UnassignedPods,
		totalScore:    output.TotalScore,
	}

	// Copy nodes (with full original capacity)
	copy(s.nodes, input.RcNodes)
	for i := range s.nodes {
		s.nodes[i].RemainingCPU = s.nodes[i].AllocatableCPU.DeepCopy()
		s.nodes[i].RemainingMemory = s.nodes[i].AllocatableMemory.DeepCopy()
		// Keep base/normalized scores from greedy's pre-compute
		s.nodeByName[s.nodes[i].Name] = i
	}

	// Index pods
	for i := range input.GatedPods {
		pod := &input.GatedPods[i]
		s.podByUID[string(pod.Pod.UID)] = pod
	}

	// Replay assignments to consume capacity and build maps
	for uid, a := range output.Assignments {
		s.podAssignment[uid] = a.TargetNode
		s.scores[uid] = a.Score
		s.nodesUsed[a.TargetNode] = true

		ni := s.nodeByName[a.TargetNode]
		pod := s.podByUID[uid]
		s.nodes[ni].RemainingCPU.Sub(pod.CPURequest)
		s.nodes[ni].RemainingMemory.Sub(pod.MemoryRequest)

		if a.RequiresWake {
			// Mark the node as "will be ready" for search purposes
			s.nodes[ni].IsReady = true
			s.nodes[ni].IsOffline = false
			s.offlineNodesUsed++
		}
	}

	return s
}

// ── Move operators ───────────────────────────────────────────────────────────

// tryRelocations attempts to move each pod to a higher-scoring node.
// Returns true if any move improved the solution.
func (h *HeuristicSolver) tryRelocations(s *searchState, deadline time.Time) bool {
	improved := false

	for podUID, fromNode := range s.podAssignment {
		if time.Now().After(deadline) {
			break
		}

		pod := s.podByUID[podUID]
		currentScore := s.scores[podUID]

		bestDelta := 0.0
		bestNode := ""

		for j := range s.nodes {
			toNode := &s.nodes[j]
			if toNode.Name == fromNode {
				continue
			}
			// Check capacity on target
			if pod.CPURequest.Cmp(toNode.RemainingCPU) > 0 ||
				pod.MemoryRequest.Cmp(toNode.RemainingMemory) > 0 {
				continue
			}

			newScore := ComputeAssignmentScore(pod, toNode, s.policy)
			delta := newScore - currentScore

			// Also give a consolidation bonus: if the source node becomes empty
			// we can "free" it (one fewer node used).
			if h.wouldFreeNode(s, podUID, fromNode) {
				delta += DefaultConsolidationBonus * 0.5
			}

			if delta > bestDelta {
				bestDelta = delta
				bestNode = toNode.Name
			}
		}

		if bestNode != "" {
			h.applyRelocate(s, podUID, fromNode, bestNode)
			improved = true
		}
	}
	return improved
}

// trySwaps attempts to swap two pods between different nodes for better score.
func (h *HeuristicSolver) trySwaps(s *searchState, deadline time.Time) bool {
	improved := false

	// Collect UIDs for stable iteration
	uids := make([]string, 0, len(s.podAssignment))
	for uid := range s.podAssignment {
		uids = append(uids, uid)
	}

	for i := 0; i < len(uids); i++ {
		if time.Now().After(deadline) {
			break
		}
		uidA := uids[i]
		podA := s.podByUID[uidA]
		nodeAName := s.podAssignment[uidA]
		niA := s.nodeByName[nodeAName]

		for j := i + 1; j < len(uids); j++ {
			if time.Now().After(deadline) {
				break
			}
			uidB := uids[j]
			nodeBName := s.podAssignment[uidB]
			if nodeAName == nodeBName {
				continue // same node, no point swapping
			}

			podB := s.podByUID[uidB]
			niB := s.nodeByName[nodeBName]

			// Check if swap is capacity-feasible:
			// Node A loses podA, gains podB → remaining + podA.req - podB.req ≥ 0
			// Node B loses podB, gains podA → remaining + podB.req - podA.req ≥ 0
			if !h.swapFeasible(s, podA, podB, niA, niB) {
				continue
			}

			// Compute score delta
			oldScoreA := s.scores[uidA]
			oldScoreB := s.scores[uidB]

			newScoreA := ComputeAssignmentScore(podA, &s.nodes[niB], s.policy)
			newScoreB := ComputeAssignmentScore(podB, &s.nodes[niA], s.policy)

			delta := (newScoreA + newScoreB) - (oldScoreA + oldScoreB)
			if delta > 0 {
				h.applySwap(s, uidA, uidB, nodeAName, nodeBName)
				improved = true
			}
		}
	}
	return improved
}

// tryConsolidate tries to empty lightly-loaded nodes by moving all their pods elsewhere.
// Returns true if any node was freed.
func (h *HeuristicSolver) tryConsolidate(s *searchState, deadline time.Time) bool {
	improved := false

	// Build per-node pod lists and sort nodes by load (ascending = lightest first)
	type nodeLoad struct {
		name    string
		podUIDs []string
		load    int64 // CPU millis used
	}

	loads := make([]nodeLoad, 0)
	for nodeName := range s.nodesUsed {
		nl := nodeLoad{name: nodeName}
		ni := s.nodeByName[nodeName]
		usedCPU := s.nodes[ni].AllocatableCPU.MilliValue() - s.nodes[ni].RemainingCPU.MilliValue()
		nl.load = usedCPU
		for uid, n := range s.podAssignment {
			if n == nodeName {
				nl.podUIDs = append(nl.podUIDs, uid)
			}
		}
		if len(nl.podUIDs) > 0 {
			loads = append(loads, nl)
		}
	}

	sort.Slice(loads, func(i, j int) bool {
		return loads[i].load < loads[j].load
	})

	for _, nl := range loads {
		if time.Now().After(deadline) {
			break
		}
		if len(nl.podUIDs) == 0 {
			continue
		}

		// Try moving all pods from this node to other nodes
		moves := make([]struct {
			uid    string
			target string
		}, 0, len(nl.podUIDs))

		canEvacuate := true
		for _, uid := range nl.podUIDs {
			pod := s.podByUID[uid]
			bestTarget := ""
			bestScore := -1.0

			for j := range s.nodes {
				tn := &s.nodes[j]
				if tn.Name == nl.name {
					continue
				}
				if pod.CPURequest.Cmp(tn.RemainingCPU) > 0 ||
					pod.MemoryRequest.Cmp(tn.RemainingMemory) > 0 {
					continue
				}
				sc := ComputeAssignmentScore(pod, tn, s.policy)
				if sc > bestScore {
					bestScore = sc
					bestTarget = tn.Name
				}
			}
			if bestTarget == "" {
				canEvacuate = false
				break
			}
			moves = append(moves, struct {
				uid    string
				target string
			}{uid, bestTarget})
		}

		if canEvacuate {
			// Apply all moves for this node
			for _, m := range moves {
				h.applyRelocate(s, m.uid, nl.name, m.target)
			}
			improved = true
		}
	}

	return improved
}

// ── Apply helpers ────────────────────────────────────────────────────────────

func (h *HeuristicSolver) applyRelocate(s *searchState, podUID, fromNode, toNode string) {
	pod := s.podByUID[podUID]
	fi := s.nodeByName[fromNode]
	ti := s.nodeByName[toNode]

	// Return capacity to source
	s.nodes[fi].RemainingCPU.Add(pod.CPURequest)
	s.nodes[fi].RemainingMemory.Add(pod.MemoryRequest)

	// Consume capacity on target
	s.nodes[ti].RemainingCPU.Sub(pod.CPURequest)
	s.nodes[ti].RemainingMemory.Sub(pod.MemoryRequest)

	// Update score
	oldScore := s.scores[podUID]
	newScore := ComputeAssignmentScore(pod, &s.nodes[ti], s.policy)
	s.totalScore += (newScore - oldScore)
	s.scores[podUID] = newScore

	// Update assignment
	s.podAssignment[podUID] = toNode
	s.nodesUsed[toNode] = true

	// Check if source node is now empty
	if h.nodeEmpty(s, fromNode) {
		delete(s.nodesUsed, fromNode)
	}
}

func (h *HeuristicSolver) applySwap(s *searchState, uidA, uidB, nodeAName, nodeBName string) {
	podA := s.podByUID[uidA]
	podB := s.podByUID[uidB]
	niA := s.nodeByName[nodeAName]
	niB := s.nodeByName[nodeBName]

	// Node A: remove podA, add podB
	s.nodes[niA].RemainingCPU.Add(podA.CPURequest)
	s.nodes[niA].RemainingMemory.Add(podA.MemoryRequest)
	s.nodes[niA].RemainingCPU.Sub(podB.CPURequest)
	s.nodes[niA].RemainingMemory.Sub(podB.MemoryRequest)

	// Node B: remove podB, add podA
	s.nodes[niB].RemainingCPU.Add(podB.CPURequest)
	s.nodes[niB].RemainingMemory.Add(podB.MemoryRequest)
	s.nodes[niB].RemainingCPU.Sub(podA.CPURequest)
	s.nodes[niB].RemainingMemory.Sub(podA.MemoryRequest)

	// Update scores
	oldA := s.scores[uidA]
	oldB := s.scores[uidB]
	newA := ComputeAssignmentScore(podA, &s.nodes[niB], s.policy)
	newB := ComputeAssignmentScore(podB, &s.nodes[niA], s.policy)
	s.totalScore += (newA + newB) - (oldA + oldB)
	s.scores[uidA] = newA
	s.scores[uidB] = newB

	// Update assignments
	s.podAssignment[uidA] = nodeBName
	s.podAssignment[uidB] = nodeAName
}

func (h *HeuristicSolver) swapFeasible(s *searchState, podA, podB *PodCandidate, niA, niB int) bool {
	nA := &s.nodes[niA]
	nB := &s.nodes[niB]

	// After swap: nodeA remaining += podA.req - podB.req (must be ≥ 0)
	afterACPU := nA.RemainingCPU.DeepCopy()
	afterACPU.Add(podA.CPURequest)
	afterACPU.Sub(podB.CPURequest)
	if afterACPU.Cmp(zeroQuantity) < 0 {
		return false
	}
	afterAMem := nA.RemainingMemory.DeepCopy()
	afterAMem.Add(podA.MemoryRequest)
	afterAMem.Sub(podB.MemoryRequest)
	if afterAMem.Cmp(zeroQuantity) < 0 {
		return false
	}

	// After swap: nodeB remaining += podB.req - podA.req (must be ≥ 0)
	afterBCPU := nB.RemainingCPU.DeepCopy()
	afterBCPU.Add(podB.CPURequest)
	afterBCPU.Sub(podA.CPURequest)
	if afterBCPU.Cmp(zeroQuantity) < 0 {
		return false
	}
	afterBMem := nB.RemainingMemory.DeepCopy()
	afterBMem.Add(podB.MemoryRequest)
	afterBMem.Sub(podA.MemoryRequest)
	if afterBMem.Cmp(zeroQuantity) < 0 {
		return false
	}

	return true
}

// ── Query helpers ────────────────────────────────────────────────────────────

func (h *HeuristicSolver) wouldFreeNode(s *searchState, excludeUID, nodeName string) bool {
	for uid, n := range s.podAssignment {
		if n == nodeName && uid != excludeUID {
			return false
		}
	}
	return true
}

func (h *HeuristicSolver) nodeEmpty(s *searchState, nodeName string) bool {
	for _, n := range s.podAssignment {
		if n == nodeName {
			return false
		}
	}
	return true
}

// ── Output conversion ────────────────────────────────────────────────────────

func (h *HeuristicSolver) stateToOutput(s searchState, original SolverOutput) SolverOutput {
	out := NewSolverOutput()
	out.Timings = original.Timings
	out.TotalScore = s.totalScore
	out.UnassignedPods = s.unassigned

	for uid, nodeName := range s.podAssignment {
		pod := s.podByUID[uid]
		ni := s.nodeByName[nodeName]
		node := &s.nodes[ni]

		out.Assignments[uid] = Assignment{
			PodNamespace: pod.Namespace,
			PodName:      pod.Pod.Name,
			PodUID:       uid,
			TargetNode:   nodeName,
			Score:        s.scores[uid],
			RequiresWake: node.IsOffline && !node.IsReady,
		}
		out.NodesUsed[nodeName] = true
	}

	// Count offline nodes used (per unique node)
	for nodeName := range out.NodesUsed {
		ni := s.nodeByName[nodeName]
		node := &s.nodes[ni]
		if node.IsOffline {
			out.OfflineNodesUsed++
		}
	}

	return out
}
