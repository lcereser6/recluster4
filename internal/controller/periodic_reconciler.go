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
	"encoding/json"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	reclusteriov1 "github.com/lorenzocereser/recluster4/api/v1"
	"github.com/lorenzocereser/recluster4/internal/solver"
)

// PeriodicReconciler reads RcNodes, Nodes, and Pods on a configurable interval.
// It uses the manager's cached client for performance.
type PeriodicReconciler struct {
	client        client.Client
	interval      time.Duration
	solver        *solver.DefaultSolver
	applier       *AssignmentApplier
	budgetTracker *BudgetTracker
}

// NewPeriodicReconciler creates a new PeriodicReconciler with the given interval.
func NewPeriodicReconciler(client client.Client, interval time.Duration) *PeriodicReconciler {
	return &PeriodicReconciler{
		client:        client,
		interval:      interval,
		solver:        solver.NewSolver(),
		applier:       NewAssignmentApplier(client),
		budgetTracker: NewBudgetTracker(),
	}
}

// Start implements manager.Runnable - runs the periodic reconciliation loop.
func (r *PeriodicReconciler) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("periodic-reconciler")
	logger.Info("Starting periodic reconciler", "interval", r.interval)

	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	// Run immediately on start, then on each tick
	if err := r.reconcile(ctx); err != nil {
		logger.Error(err, "initial reconciliation failed")
	}

	for {
		select {
		case <-ctx.Done():
			logger.Info("Stopping periodic reconciler")
			return nil
		case <-ticker.C:
			if err := r.reconcile(ctx); err != nil {
				logger.Error(err, "periodic reconciliation failed")
				// Continue running despite errors
			}
		}
	}
}

// reconcile performs the actual work - reads all resources and processes them.
func (r *PeriodicReconciler) reconcile(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("periodic-reconciler")
	start := time.Now()

	// Fetch all resources in parallel using goroutines for better performance
	type result struct {
		rcNodes    *reclusteriov1.RcNodeList
		rcPolicies *reclusteriov1.RcPolicyList
		nodes      *corev1.NodeList
		pods       *corev1.PodList
		err        error
	}

	rcNodesCh := make(chan result, 1)
	rcPoliciesCh := make(chan result, 1)
	nodesCh := make(chan result, 1)
	podsCh := make(chan result, 1)

	// Fetch RcNodes
	go func() {
		rcNodes := &reclusteriov1.RcNodeList{}
		err := r.client.List(ctx, rcNodes)
		rcNodesCh <- result{rcNodes: rcNodes, err: err}
	}()

	// Fetch RcPolicies
	go func() {
		rcPolicies := &reclusteriov1.RcPolicyList{}
		err := r.client.List(ctx, rcPolicies)
		rcPoliciesCh <- result{rcPolicies: rcPolicies, err: err}
	}()

	// Fetch Nodes
	go func() {
		nodes := &corev1.NodeList{}
		err := r.client.List(ctx, nodes)
		nodesCh <- result{nodes: nodes, err: err}
	}()

	// Fetch Pods (only non-system namespaces for performance)
	go func() {
		pods := &corev1.PodList{}
		// List all pods - the cache makes this fast
		err := r.client.List(ctx, pods)
		podsCh <- result{pods: pods, err: err}
	}()

	// Collect results
	rcNodesResult := <-rcNodesCh
	rcPoliciesResult := <-rcPoliciesCh
	nodesResult := <-nodesCh
	podsResult := <-podsCh

	// Check for errors
	if rcNodesResult.err != nil {
		return rcNodesResult.err
	}
	if rcPoliciesResult.err != nil {
		return rcPoliciesResult.err
	}
	if nodesResult.err != nil {
		return nodesResult.err
	}
	if podsResult.err != nil {
		return podsResult.err
	}

	// Build summary data structures for processing
	summary := r.buildSummary(rcNodesResult.rcNodes, rcPoliciesResult.rcPolicies, nodesResult.nodes, podsResult.pods)

	// Log the summary (placeholder for future action)
	logger.Info("Periodic reconciliation complete",
		"rcNodes", summary.RcNodeCount,
		"rcPolicies", summary.RcPolicyCount,
		"nodes", summary.NodeCount,
		"pods", summary.PodCount,
		"pendingPods", summary.PendingPodCount,
		"gatedPods", summary.GatedPodCount,
		"duration", time.Since(start),
	)

	// Run solver only if there are gated pods
	if summary.GatedPodCount > 0 && summary.RcNodeCount > 0 {
		logger.Info("Running solver for gated pods",
			"gatedPods", summary.GatedPodCount,
			"rcNodes", summary.RcNodeCount,
			"rcPolicies", summary.RcPolicyCount,
		)

		// Log budget state before solving
		powerOnsInFlight, migrationsThisHour := r.budgetTracker.GetCurrentState()
		logger.V(1).Info("Budget state before solve",
			"powerOnsInFlight", powerOnsInFlight,
			"migrationsThisHour", migrationsThisHour,
		)

		// Build solver input
		inputBuildStart := time.Now()
		solverInput := r.buildSolverInput(
			rcNodesResult.rcNodes,
			rcPoliciesResult.rcPolicies,
			summary.GatedPods,
			podsResult.pods,
		)
		inputBuildDuration := time.Since(inputBuildStart)

		// Log input summary
		onlineNodes := 0
		offlineNodes := 0
		for _, node := range solverInput.RcNodes {
			if node.IsReady {
				onlineNodes++
			} else if node.IsOffline {
				offlineNodes++
			}
		}
		logger.V(1).Info("Solver input prepared",
			"inputBuildTime", inputBuildDuration,
			"pods", len(solverInput.GatedPods),
			"nodes", len(solverInput.RcNodes),
			"onlineNodes", onlineNodes,
			"offlineNodes", offlineNodes,
			"activePolicy", func() string {
				if solverInput.ActivePolicy != nil {
					return solverInput.ActivePolicy.Name
				}
				return "<none>"
			}(),
			"deadline", solverInput.Constraints.Deadline.Format(time.RFC3339),
		)

		// Run solver
		solveStart := time.Now()
		solverOutput := r.solver.Solve(ctx, solverInput)
		totalSolveTime := time.Since(solveStart)

		// Log solver results
		logger.Info("Solver completed",
			"algorithm", solverOutput.Algorithm,
			"assigned", len(solverOutput.Assignments),
			"unassigned", len(solverOutput.UnassignedPods),
			"nodesUsed", len(solverOutput.NodesUsed),
			"offlineNodesUsed", solverOutput.OfflineNodesUsed,
			"totalScore", fmt.Sprintf("%.2f", solverOutput.TotalScore),
			"solveTime", solverOutput.SolveTime,
			"totalTime", totalSolveTime,
		)

		// Log each assignment at debug level
		for podUID, assignment := range solverOutput.Assignments {
			logger.V(1).Info("Assignment",
				"pod", assignment.PodNamespace+"/"+assignment.PodName,
				"podUID", podUID,
				"targetNode", assignment.TargetNode,
				"score", fmt.Sprintf("%.2f", assignment.Score),
				"requiresWake", assignment.RequiresWake,
			)
		}

		// Log unassigned pods with reasons
		for _, unassigned := range solverOutput.UnassignedPods {
			logger.Info("Unassigned pod",
				"pod", unassigned.PodNamespace+"/"+unassigned.PodName,
				"reason", unassigned.Reason,
				"details", unassigned.Details,
			)
		}

		// Apply assignments
		if len(solverOutput.Assignments) > 0 {
			applyStart := time.Now()
			if err := r.applier.ApplyAssignments(ctx, solverOutput); err != nil {
				logger.Error(err, "Failed to apply some assignments")
			}
			applyDuration := time.Since(applyStart)

			// Update budget tracker for power-ons
			wakeCount := 0
			for _, assignment := range solverOutput.Assignments {
				if assignment.RequiresWake {
					r.budgetTracker.RecordPowerOn(assignment.TargetNode)
					wakeCount++
				}
			}

			logger.Info("Assignments applied",
				"count", len(solverOutput.Assignments),
				"wakeTriggered", wakeCount,
				"applyTime", applyDuration,
			)
		}
	} else if summary.GatedPodCount > 0 {
		logger.Info("Skipping solver - no RcNodes available", "gatedPods", summary.GatedPodCount)
	}

	// ── Gate removal: bind assigned pods whose target K8s Node is Ready ──
	if err := r.processAssignedPods(ctx, podsResult.pods, nodesResult.nodes); err != nil {
		logger.Error(err, "Failed to process assigned pods")
	}

	// Print details for debugging
	logger.Info("--- RcNodes ---")
	for _, rcNode := range summary.RcNodes {
		logger.Info("RcNode",
			"name", rcNode.Name,
			"nodeGroup", rcNode.NodeGroup,
			"desiredPhase", rcNode.DesiredPhase,
			"wakeMethod", rcNode.WakeMethod,
			"cpu", rcNode.CPU,
			"memory", rcNode.Memory,
		)
	}

	logger.Info("--- RcPolicies ---")
	for _, rcPolicy := range summary.RcPolicies {
		logger.Info("RcPolicy",
			"name", rcPolicy.Name,
			"namespace", rcPolicy.Namespace,
			"multiplierCount", rcPolicy.MultiplierCount,
			"avgFeatureMatch", fmt.Sprintf("%.2f", rcPolicy.AvgFeatureMatch),
			"targetLabels", rcPolicy.TargetLabels,
		)
	}

	logger.Info("--- Nodes ---")
	for _, node := range summary.Nodes {
		logger.Info("Node",
			"name", node.Name,
			"nodeGroup", node.NodeGroup,
			"ready", node.Ready,
			"schedulable", node.Schedulable,
			"isFake", node.IsFake,
		)
	}

	logger.Info("--- Gated Pods ---")
	for _, pod := range summary.GatedPods {
		logger.Info("GatedPod",
			"namespace", pod.Namespace,
			"name", pod.Name,
			"gate", pod.GateName,
		)
	}

	logger.Info("--- Pending Pods ---")
	for _, pod := range summary.PendingPods {
		logger.Info("PendingPod",
			"namespace", pod.Namespace,
			"name", pod.Name,
			"phase", pod.Phase,
		)
	}

	return nil
}

// ClusterSummary holds aggregated cluster state for processing.
type ClusterSummary struct {
	RcNodeCount     int
	RcPolicyCount   int
	NodeCount       int
	PodCount        int
	PendingPodCount int
	GatedPodCount   int

	// Detailed data for processing
	RcNodes     []RcNodeInfo
	RcPolicies  []RcPolicyInfo
	Nodes       []NodeInfo
	PendingPods []PodInfo
	GatedPods   []PodInfo
}

// RcPolicyInfo contains relevant RcPolicy data for processing.
type RcPolicyInfo struct {
	Name            string
	Namespace       string
	MultiplierCount int
	TargetLabels    map[string]string
	AvgFeatureMatch float64 // Average number of matching features across all RcNodes
}

// RcNodeInfo contains relevant RcNode data for processing.
type RcNodeInfo struct {
	Name         string
	NodeGroup    string
	DesiredPhase reclusteriov1.NodePhase
	WakeMethod   reclusteriov1.WakeMethod
	CPU          string
	Memory       string
}

// NodeInfo contains relevant Node data for processing.
type NodeInfo struct {
	Name        string
	NodeGroup   string
	Ready       bool
	Schedulable bool
	IsFake      bool // KWOK fake node
}

// PodInfo contains relevant Pod data for processing.
type PodInfo struct {
	Namespace string
	Name      string
	Phase     corev1.PodPhase
	NodeName  string
	IsGated   bool
	GateName  string
}

// buildSummary creates a ClusterSummary from the raw resource lists.
func (r *PeriodicReconciler) buildSummary(
	rcNodes *reclusteriov1.RcNodeList,
	rcPolicies *reclusteriov1.RcPolicyList,
	nodes *corev1.NodeList,
	pods *corev1.PodList,
) ClusterSummary {
	summary := ClusterSummary{
		RcNodeCount:   len(rcNodes.Items),
		RcPolicyCount: len(rcPolicies.Items),
		NodeCount:     len(nodes.Items),
		PodCount:      len(pods.Items),
	}

	// Process RcNodes
	summary.RcNodes = make([]RcNodeInfo, 0, len(rcNodes.Items))
	for _, rcNode := range rcNodes.Items {
		info := RcNodeInfo{
			Name:         rcNode.Name,
			NodeGroup:    rcNode.Spec.NodeGroup,
			DesiredPhase: rcNode.Spec.DesiredPhase,
			WakeMethod:   rcNode.Spec.Activation.WakeMethod,
		}
		if cpu, ok := rcNode.Spec.Resources.Allocatable["cpu"]; ok && !cpu.IsZero() {
			info.CPU = cpu.String()
		}
		if memory, ok := rcNode.Spec.Resources.Allocatable["memory"]; ok && !memory.IsZero() {
			info.Memory = memory.String()
		}
		summary.RcNodes = append(summary.RcNodes, info)
	}

	// Process RcPolicies
	summary.RcPolicies = make([]RcPolicyInfo, 0, len(rcPolicies.Items))
	for _, rcPolicy := range rcPolicies.Items {
		info := RcPolicyInfo{
			Name:            rcPolicy.Name,
			Namespace:       rcPolicy.Namespace,
			MultiplierCount: len(rcPolicy.Spec.Multipliers),
		}
		if rcPolicy.Spec.DeploymentSelector.MatchLabels != nil {
			info.TargetLabels = rcPolicy.Spec.DeploymentSelector.MatchLabels
		}

		// Calculate average feature match across all RcNodes
		if len(rcNodes.Items) > 0 && len(rcPolicy.Spec.Multipliers) > 0 {
			totalMatches := 0
			for _, rcNode := range rcNodes.Items {
				// Build a set of feature names for this RcNode
				featureNames := make(map[string]struct{})
				for _, feature := range rcNode.Spec.Features {
					featureNames[feature.Name] = struct{}{}
				}
				// Count how many policy multipliers match RcNode features
				for _, mult := range rcPolicy.Spec.Multipliers {
					if _, exists := featureNames[mult.Name]; exists {
						totalMatches++
					}
				}
			}
			info.AvgFeatureMatch = float64(totalMatches) / float64(len(rcNodes.Items))
		}

		summary.RcPolicies = append(summary.RcPolicies, info)
	}

	// Process Nodes
	summary.Nodes = make([]NodeInfo, 0, len(nodes.Items))
	for _, node := range nodes.Items {
		info := NodeInfo{
			Name:        node.Name,
			NodeGroup:   node.Labels["recluster.io/node-group"],
			Schedulable: !node.Spec.Unschedulable,
			IsFake:      node.Labels["kwok.x-k8s.io/node"] == "fake",
		}
		// Check if node is ready
		for _, cond := range node.Status.Conditions {
			if cond.Type == corev1.NodeReady {
				info.Ready = cond.Status == corev1.ConditionTrue
				break
			}
		}
		summary.Nodes = append(summary.Nodes, info)
	}

	// Process Pods
	summary.PendingPods = make([]PodInfo, 0)
	summary.GatedPods = make([]PodInfo, 0)
	for _, pod := range pods.Items {
		// Skip system namespace pods
		if isSystemNamespace(pod.Namespace) {
			continue
		}

		info := PodInfo{
			Namespace: pod.Namespace,
			Name:      pod.Name,
			Phase:     pod.Status.Phase,
			NodeName:  pod.Spec.NodeName,
		}

		// Check for scheduling gates
		if len(pod.Spec.SchedulingGates) > 0 {
			info.IsGated = true
			info.GateName = pod.Spec.SchedulingGates[0].Name

			// Skip pods that have already been assigned (have the assignment label)
			if _, hasAssignment := pod.Labels["recluster.io/assigned-node"]; !hasAssignment {
				summary.GatedPods = append(summary.GatedPods, info)
				summary.GatedPodCount++
			}
		}

		// Track pending pods (not yet scheduled)
		if pod.Status.Phase == corev1.PodPending && pod.Spec.NodeName == "" {
			summary.PendingPods = append(summary.PendingPods, info)
			summary.PendingPodCount++
		}
	}

	return summary
}

// processAssignedPods scans for pods that have been assigned to a node
// (recluster.io/assigned-node label) but still carry the scheduling gate.
// When the target K8s Node exists and is Ready, it adds the matching
// toleration + nodeSelector and removes the gate, letting the standard
// K8s scheduler perform the actual binding.
func (r *PeriodicReconciler) processAssignedPods(ctx context.Context, allPods *corev1.PodList, allNodes *corev1.NodeList) error {
	logger := log.FromContext(ctx).WithName("gate-remover")

	// Build a set of Ready K8s Node names for fast lookup
	readyNodes := make(map[string]bool)
	for i := range allNodes.Items {
		node := &allNodes.Items[i]
		for _, cond := range node.Status.Conditions {
			if cond.Type == corev1.NodeReady && cond.Status == corev1.ConditionTrue {
				readyNodes[node.Name] = true
				break
			}
		}
	}

	ungated := 0
	var firstErr error

	for i := range allPods.Items {
		pod := &allPods.Items[i]

		// Skip system namespaces
		if isSystemNamespace(pod.Namespace) {
			continue
		}

		// Must have the assignment label
		targetNode, hasAssignment := pod.Labels["recluster.io/assigned-node"]
		if !hasAssignment {
			continue
		}

		// Must still have the scheduling gate
		if !HasSchedulingGate(pod) {
			continue
		}

		// Target K8s Node must be Ready
		if !readyNodes[targetNode] {
			logger.V(1).Info("Target node not ready yet, keeping gate",
				"pod", pod.Namespace+"/"+pod.Name,
				"targetNode", targetNode,
			)
			continue
		}

		// Add toleration + nodeSelector, remove gate → scheduler binds
		if err := r.releaseToScheduler(ctx, pod, targetNode); err != nil {
			logger.Error(err, "Failed to release pod to scheduler",
				"pod", pod.Namespace+"/"+pod.Name,
				"targetNode", targetNode,
			)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}

		ungated++
		logger.Info("Pod released to scheduler",
			"pod", pod.Namespace+"/"+pod.Name,
			"targetNode", targetNode,
		)
	}

	if ungated > 0 {
		logger.Info("Gate removal pass complete", "unblocked", ungated)
	}

	return firstErr
}

// releaseToScheduler prepares a gated pod for scheduling to a specific node
// using the taint-and-toleration pattern from the thesis:
//  1. Add a toleration for the target node's unique taint
//     (recluster.io/node=<name>:NoSchedule)
//  2. Set nodeSelector to pin the pod to that exact node
//  3. Remove the scheduling gate
//
// The Kubernetes scheduler then binds the pod normally — we never touch
// spec.nodeName or the Binding subresource directly.
// nodeSelector is mutable while the pod is gated (KEP-3838, K8s 1.27+).
func (r *PeriodicReconciler) releaseToScheduler(ctx context.Context, pod *corev1.Pod, nodeName string) error {
	// Build the full tolerations list: keep existing + add assignment toleration.
	// tolerations is +listType=atomic, so strategic-merge replaces the whole list.
	newTolerations := make([]corev1.Toleration, len(pod.Spec.Tolerations), len(pod.Spec.Tolerations)+1)
	copy(newTolerations, pod.Spec.Tolerations)
	newTolerations = append(newTolerations, corev1.Toleration{
		Key:      "recluster.io/node",
		Operator: corev1.TolerationOpEqual,
		Value:    nodeName,
		Effect:   corev1.TaintEffectNoSchedule,
	})

	patch := map[string]interface{}{
		"spec": map[string]interface{}{
			"tolerations": newTolerations,
			"nodeSelector": map[string]string{
				"kubernetes.io/hostname": nodeName,
			},
			"schedulingGates": nil, // clears all gates
		},
	}

	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("marshal scheduling patch: %w", err)
	}

	if err := r.client.Patch(ctx, pod, client.RawPatch(types.StrategicMergePatchType, patchBytes)); err != nil {
		return fmt.Errorf("release pod to scheduler: %w", err)
	}

	return nil
}

// isSystemNamespace returns true for Kubernetes system namespaces.
func isSystemNamespace(namespace string) bool {
	switch namespace {
	case "kube-system", "kube-public", "kube-node-lease", "recluster4-system", "cert-manager":
		return true
	default:
		return false
	}
}

// buildSolverInput constructs the input for the solver
func (r *PeriodicReconciler) buildSolverInput(
	rcNodes *reclusteriov1.RcNodeList,
	rcPolicies *reclusteriov1.RcPolicyList,
	gatedPods []PodInfo,
	allPods *corev1.PodList,
) solver.SolverInput {
	input := solver.SolverInput{
		GatedPods: make([]solver.PodCandidate, 0, len(gatedPods)),
		RcNodes:   make([]solver.RcNodeCandidate, 0, len(rcNodes.Items)),
	}

	// Build pod candidates from gated pods
	podMap := make(map[string]*corev1.Pod)
	for i := range allPods.Items {
		pod := &allPods.Items[i]
		podMap[pod.Namespace+"/"+pod.Name] = pod
	}

	for _, gatedPod := range gatedPods {
		pod, exists := podMap[gatedPod.Namespace+"/"+gatedPod.Name]
		if !exists {
			continue
		}

		candidate := solver.PodCandidate{
			Pod:       pod,
			Namespace: pod.Namespace,
			PolicyTag: GetPodPolicyTag(pod),
		}

		// Sum resource requests from all containers
		for _, container := range pod.Spec.Containers {
			if cpu, ok := container.Resources.Requests[corev1.ResourceCPU]; ok {
				candidate.CPURequest.Add(cpu)
			}
			if mem, ok := container.Resources.Requests[corev1.ResourceMemory]; ok {
				candidate.MemoryRequest.Add(mem)
			}
		}

		// Extract owner reference
		for _, owner := range pod.OwnerReferences {
			candidate.OwnerKind = owner.Kind
			candidate.OwnerName = owner.Name
			break
		}

		input.GatedPods = append(input.GatedPods, candidate)
	}

	// Build RcNode candidates
	for i := range rcNodes.Items {
		rcNode := &rcNodes.Items[i]

		candidate := solver.RcNodeCandidate{
			RcNode:   rcNode,
			Name:     rcNode.Name,
			Features: make(map[string]string),
		}

		// Get allocatable resources
		if cpu, ok := rcNode.Spec.Resources.Allocatable[string(corev1.ResourceCPU)]; ok {
			candidate.AllocatableCPU = cpu
			candidate.RemainingCPU = cpu.DeepCopy()
		}
		if mem, ok := rcNode.Spec.Resources.Allocatable[string(corev1.ResourceMemory)]; ok {
			candidate.AllocatableMemory = mem
			candidate.RemainingMemory = mem.DeepCopy()
		}

		// Set phase flags
		candidate.IsReady = rcNode.Status.CurrentPhase == reclusteriov1.NodePhaseOnline
		candidate.IsOffline = rcNode.Status.CurrentPhase == reclusteriov1.NodePhaseOffline ||
			rcNode.Spec.DesiredPhase == reclusteriov1.NodePhaseOffline

		// Extract features into map for quick lookup
		for _, feature := range rcNode.Spec.Features {
			candidate.Features[feature.Name] = feature.Value
		}

		// Extract boot time and power from features if available
		if bootTime, ok := candidate.Features["boot.timeseconds"]; ok {
			if val, err := parseIntFeature(bootTime); err == nil {
				candidate.BootTimeSeconds = val
			}
		}
		if idleWatts, ok := candidate.Features["power.idle"]; ok {
			if val, err := parseIntFeature(idleWatts); err == nil {
				candidate.IdleWatts = val
			}
		}

		input.RcNodes = append(input.RcNodes, candidate)
	}

	// Select the highest priority policy
	if len(rcPolicies.Items) > 0 {
		input.ActivePolicy = solver.SelectPolicy(rcPolicies.Items, "default")
	}

	// Set constraints
	powerOnsInFlight, migrationsThisHour := r.budgetTracker.GetCurrentState()
	maxConcurrent, maxMigrations, _ := r.budgetTracker.GetLimits(input.ActivePolicy)

	input.Constraints = solver.SolverConstraints{
		Deadline:                time.Now().Add(r.interval - 100*time.Millisecond),
		MaxConcurrentPowerOns:   maxConcurrent,
		CurrentPowerOnsInFlight: powerOnsInFlight,
		MaxMigrationsPerHour:    maxMigrations,
		MigrationsThisHour:      migrationsThisHour,
		PlannerCadence:          r.interval,
	}

	return input
}

// parseIntFeature parses an integer from a feature value string
func parseIntFeature(value string) (int, error) {
	var result int
	_, err := fmt.Sscanf(value, "%d", &result)
	return result, err
}

// Ensure PeriodicReconciler implements manager.Runnable
var _ manager.Runnable = &PeriodicReconciler{}
