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

	reclusteriov1 "github.com/lorenzocereser/recluster4/api/v1"
	"github.com/lorenzocereser/recluster4/internal/solver"
)

// SchedulingGateName is the name of the scheduling gate added by the webhook.
// Must match the constant in internal/webhook/pod_scheduling_gate.go
const SchedulingGateName = "recluster.io/scheduling-gate"

// AssignmentApplier applies solver assignments to pods and triggers node wakes
type AssignmentApplier struct {
	client client.Client
}

// NewAssignmentApplier creates a new AssignmentApplier
func NewAssignmentApplier(client client.Client) *AssignmentApplier {
	return &AssignmentApplier{
		client: client,
	}
}

// ApplyAssignments applies all solver assignments
// For each assignment:
// 1. Patch pod with node affinity to target node
// 2. Remove the scheduling gate
// 3. If target is offline RcNode, trigger wake
func (a *AssignmentApplier) ApplyAssignments(ctx context.Context, output solver.SolverOutput) error {
	logger := log.FromContext(ctx).WithName("assignment-applier")

	if len(output.Assignments) == 0 {
		logger.V(1).Info("No assignments to apply")
		return nil
	}

	logger.Info("Applying assignments", "count", len(output.Assignments))

	var errors []error
	nodesToWake := make(map[string]bool)

	for _, assignment := range output.Assignments {
		// Get the pod
		pod := &corev1.Pod{}
		err := a.client.Get(ctx, types.NamespacedName{
			Namespace: assignment.PodNamespace,
			Name:      assignment.PodName,
		}, pod)
		if err != nil {
			logger.Error(err, "Failed to get pod", "pod", assignment.PodName)
			errors = append(errors, err)
			continue
		}

		// Apply node affinity and remove gate
		if err := a.patchPodAssignment(ctx, pod, assignment.TargetNode); err != nil {
			logger.Error(err, "Failed to patch pod", "pod", assignment.PodName)
			errors = append(errors, err)
			continue
		}

		logger.Info("Applied assignment",
			"pod", fmt.Sprintf("%s/%s", assignment.PodNamespace, assignment.PodName),
			"targetNode", assignment.TargetNode,
			"score", assignment.Score,
		)

		// Track nodes that need to be woken
		if assignment.RequiresWake {
			nodesToWake[assignment.TargetNode] = true
		}
	}

	// Trigger node wakes
	for nodeName := range nodesToWake {
		if err := a.triggerNodeWake(ctx, nodeName); err != nil {
			logger.Error(err, "Failed to trigger node wake", "node", nodeName)
			errors = append(errors, err)
		}
	}

	if len(errors) > 0 {
		return fmt.Errorf("failed to apply %d assignments", len(errors))
	}

	return nil
}

// patchPodAssignment patches a pod with assignment metadata but keeps the scheduling gate.
// This allows testing the solver without actually scheduling pods.
// When node simulation is implemented, this can be updated to remove the gate.
func (a *AssignmentApplier) patchPodAssignment(ctx context.Context, pod *corev1.Pod, targetNode string) error {
	// Build patch to:
	// 1. Add labels/annotations to track the assignment
	// 2. Keep the scheduling gate (don't remove it yet - no node simulation)

	patch := map[string]interface{}{
		"metadata": map[string]interface{}{
			"labels": map[string]string{
				"recluster.io/assigned-node": targetNode,
			},
			"annotations": map[string]string{
				"recluster.io/assignment-time": time.Now().UTC().Format(time.RFC3339),
			},
		},
	}

	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return err
	}

	return a.client.Patch(ctx, pod, client.RawPatch(types.StrategicMergePatchType, patchBytes))
}

// triggerNodeWake sets an RcNode's desiredPhase to Booting to trigger wake
func (a *AssignmentApplier) triggerNodeWake(ctx context.Context, nodeName string) error {
	logger := log.FromContext(ctx).WithName("assignment-applier")

	// Get the RcNode
	rcNode := &reclusteriov1.RcNode{}
	err := a.client.Get(ctx, types.NamespacedName{Name: nodeName}, rcNode)
	if err != nil {
		return err
	}

	// Only wake if currently offline
	if rcNode.Spec.DesiredPhase == reclusteriov1.NodePhaseOnline ||
		rcNode.Spec.DesiredPhase == reclusteriov1.NodePhaseBooting {
		logger.V(1).Info("Node already online or booting", "node", nodeName)
		return nil
	}

	// Patch to set desiredPhase = Booting
	patch := map[string]interface{}{
		"spec": map[string]interface{}{
			"desiredPhase": reclusteriov1.NodePhaseBooting,
		},
	}

	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return err
	}

	logger.Info("Triggering node wake", "node", nodeName)
	return a.client.Patch(ctx, rcNode, client.RawPatch(types.MergePatchType, patchBytes))
}

// RemoveSchedulingGate removes the scheduling gate from a pod
func (a *AssignmentApplier) RemoveSchedulingGate(ctx context.Context, pod *corev1.Pod) error {
	// Build patch to remove scheduling gates
	patch := map[string]interface{}{
		"spec": map[string]interface{}{
			"schedulingGates": nil,
		},
	}

	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return err
	}

	return a.client.Patch(ctx, pod, client.RawPatch(types.StrategicMergePatchType, patchBytes))
}

// HasSchedulingGate checks if a pod has the recluster scheduling gate
func HasSchedulingGate(pod *corev1.Pod) bool {
	for _, gate := range pod.Spec.SchedulingGates {
		if gate.Name == SchedulingGateName {
			return true
		}
	}
	return false
}

// GetPodPolicyTag extracts the policy tag label from a pod
func GetPodPolicyTag(pod *corev1.Pod) string {
	if pod.Labels == nil {
		return "default"
	}
	if tag, exists := pod.Labels["recluster.io/policy-tag"]; exists {
		return tag
	}
	return "default"
}
