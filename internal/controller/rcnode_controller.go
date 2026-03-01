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
	"fmt"
	"math/rand"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	reclusteriov1 "github.com/lorenzocereser/recluster4/api/v1"
)

const (
	// rcNodeFinalizer ensures we clean up the K8s Node when an RcNode is deleted
	rcNodeFinalizer = "recluster.io/node-cleanup"

	// defaultBootTimeSeconds is the fallback if no boot.timeseconds feature is set
	defaultBootTimeSeconds = 30

	// bootTimeJitterFraction adds ±this fraction of randomness to boot delay
	bootTimeJitterFraction = 0.2
)

// RcNodeReconciler reconciles a RcNode object.
// It implements a state machine that creates/deletes simulated (KWOK) K8s Nodes
// based on the RcNode desiredPhase field.
type RcNodeReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=recluster.io,resources=rcnodes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=recluster.io,resources=rcnodes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=recluster.io,resources=rcnodes/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=nodes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;patch;update
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=recluster.io,resources=rcpolicies,verbs=get;list;watch
// +kubebuilder:rbac:groups=recluster.io,resources=rcpolicies/status,verbs=get

func (r *RcNodeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithValues("rcnode", req.Name)

	// 1. Fetch the RcNode
	var rcNode reclusteriov1.RcNode
	if err := r.Get(ctx, req.NamespacedName, &rcNode); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// 2. Handle deletion (finalizer)
	if !rcNode.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, &rcNode)
	}

	// 3. Ensure finalizer is present
	if !controllerutil.ContainsFinalizer(&rcNode, rcNodeFinalizer) {
		controllerutil.AddFinalizer(&rcNode, rcNodeFinalizer)
		if err := r.Update(ctx, &rcNode); err != nil {
			return ctrl.Result{}, err
		}
		// Re-fetch after update to avoid conflicts
		if err := r.Get(ctx, req.NamespacedName, &rcNode); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
	}

	desired := rcNode.Spec.DesiredPhase
	current := rcNode.Status.CurrentPhase

	log.Info("Reconciling RcNode",
		"desired", desired,
		"current", current,
	)

	// 4. State machine
	switch {

	// ── Offline and should boot ──────────────────────────────────────
	case desired == reclusteriov1.NodePhaseBooting && (current == "" || current == reclusteriov1.NodePhaseOffline):
		return r.startBooting(ctx, &rcNode)

	// ── Booting timer expired → come Online ──────────────────────────
	case desired == reclusteriov1.NodePhaseBooting && current == reclusteriov1.NodePhaseBooting:
		return r.checkBootComplete(ctx, &rcNode)

	// ── Desired Online but still Booting (assignment_applier sets Booting, CA sets Online) ──
	case desired == reclusteriov1.NodePhaseOnline && current == reclusteriov1.NodePhaseBooting:
		return r.checkBootComplete(ctx, &rcNode)

	// ── Desired Online, already Online → ensure K8s Node exists ──────
	case desired == reclusteriov1.NodePhaseOnline && (current == "" || current == reclusteriov1.NodePhaseOffline):
		// Treat "desired Online from Offline" the same as Booting
		return r.startBooting(ctx, &rcNode)

	case desired == reclusteriov1.NodePhaseOnline && current == reclusteriov1.NodePhaseOnline:
		// Steady state – ensure K8s Node still exists
		return r.ensureNodeExists(ctx, &rcNode)

	// ── Shutdown ─────────────────────────────────────────────────────
	case desired == reclusteriov1.NodePhaseOffline && (current == reclusteriov1.NodePhaseOnline || current == reclusteriov1.NodePhaseBooting):
		return r.shutdownNode(ctx, &rcNode)

	case desired == reclusteriov1.NodePhaseOffline && (current == "" || current == reclusteriov1.NodePhaseOffline):
		// Already offline — no-op
		if current == "" {
			return r.setStatus(ctx, &rcNode, reclusteriov1.NodePhaseOffline, "Initialised as Offline")
		}
		return ctrl.Result{}, nil

	default:
		log.Info("No state transition needed",
			"desired", desired,
			"current", current,
		)
		return ctrl.Result{}, nil
	}
}

// ─── State transition handlers ───────────────────────────────────────────────

// startBooting sets currentPhase=Booting, records lastBootTime, and requeues
// after a simulated boot delay.
func (r *RcNodeReconciler) startBooting(ctx context.Context, rcNode *reclusteriov1.RcNode) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithValues("rcnode", rcNode.Name)

	bootDelay := r.getBootDelay(rcNode)
	now := metav1.Now()

	rcNode.Status.CurrentPhase = reclusteriov1.NodePhaseBooting
	rcNode.Status.LastBootTime = &now
	rcNode.Status.Message = fmt.Sprintf("Simulated boot started, will be ready in %s", bootDelay)
	rcNode.Status.LastTransitionTime = &now

	if err := r.Status().Update(ctx, rcNode); err != nil {
		return ctrl.Result{}, err
	}

	log.Info("Node booting (simulated)",
		"bootDelay", bootDelay,
		"bootTime", now.Time,
	)

	return ctrl.Result{RequeueAfter: bootDelay}, nil
}

// checkBootComplete checks if the boot delay has passed. If so, creates the
// K8s Node and transitions to Online. If not, requeues for the remaining time.
func (r *RcNodeReconciler) checkBootComplete(ctx context.Context, rcNode *reclusteriov1.RcNode) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithValues("rcnode", rcNode.Name)

	if rcNode.Status.LastBootTime == nil {
		// No boot time recorded — start fresh
		return r.startBooting(ctx, rcNode)
	}

	bootDelay := r.getBootDelay(rcNode)
	elapsed := time.Since(rcNode.Status.LastBootTime.Time)
	remaining := bootDelay - elapsed

	if remaining > 0 {
		log.V(1).Info("Boot still in progress", "remaining", remaining)
		return ctrl.Result{RequeueAfter: remaining}, nil
	}

	// Boot complete — create the K8s Node
	if err := r.createOrUpdateNode(ctx, rcNode); err != nil {
		return ctrl.Result{}, err
	}

	// Promote desiredPhase from Booting → Online so the state machine
	// reaches the steady-state case (desired=Online, current=Online).
	if rcNode.Spec.DesiredPhase == reclusteriov1.NodePhaseBooting {
		rcNode.Spec.DesiredPhase = reclusteriov1.NodePhaseOnline
		if err := r.Update(ctx, rcNode); err != nil {
			return ctrl.Result{}, fmt.Errorf("promote desiredPhase to Online: %w", err)
		}
		// Re-fetch after spec update to avoid stale resourceVersion on status update
		if err := r.Get(ctx, types.NamespacedName{Name: rcNode.Name}, rcNode); err != nil {
			return ctrl.Result{}, err
		}
	}

	return r.setStatus(ctx, rcNode, reclusteriov1.NodePhaseOnline, "Node online (simulated)")
}

// shutdownNode deletes the K8s Node and transitions to Offline.
func (r *RcNodeReconciler) shutdownNode(ctx context.Context, rcNode *reclusteriov1.RcNode) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithValues("rcnode", rcNode.Name)

	if err := r.deleteNode(ctx, rcNode.Name); err != nil {
		return ctrl.Result{}, err
	}

	now := metav1.Now()
	rcNode.Status.LastShutdownTime = &now
	log.Info("Node shut down (simulated)")

	return r.setStatus(ctx, rcNode, reclusteriov1.NodePhaseOffline, "Node offline (simulated)")
}

// ensureNodeExists checks that the K8s Node still exists for an Online RcNode.
// If it was deleted externally, recreate it.
// It also refreshes the heartbeat so the node-controller doesn't mark the
// simulated node as Unknown/NotReady (default grace period is 40s).
func (r *RcNodeReconciler) ensureNodeExists(ctx context.Context, rcNode *reclusteriov1.RcNode) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithValues("rcnode", rcNode.Name)

	var node corev1.Node
	err := r.Get(ctx, types.NamespacedName{Name: rcNode.Name}, &node)
	if errors.IsNotFound(err) {
		log.Info("K8s Node missing for Online RcNode, recreating", "rcnode", rcNode.Name)
		if err := r.createOrUpdateNode(ctx, rcNode); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	if err != nil {
		return ctrl.Result{}, err
	}

	// Refresh heartbeat timestamps to prevent node-controller from marking
	// this simulated node as Unknown.
	now := metav1.Now()
	needsUpdate := false
	for i := range node.Status.Conditions {
		cond := &node.Status.Conditions[i]
		if time.Since(cond.LastHeartbeatTime.Time) > 20*time.Second {
			cond.LastHeartbeatTime = now
			needsUpdate = true
		}
	}

	if needsUpdate {
		if err := r.Status().Update(ctx, &node); err != nil {
			log.Error(err, "Failed to refresh node heartbeat")
			// Non-fatal: requeue and try again
		}
	}

	// Requeue to keep refreshing the heartbeat (every 30s, well within 40s grace)
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// handleDeletion is called when the RcNode is being deleted. It cleans up the
// K8s Node and removes the finalizer.
func (r *RcNodeReconciler) handleDeletion(ctx context.Context, rcNode *reclusteriov1.RcNode) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithValues("rcnode", rcNode.Name)

	if controllerutil.ContainsFinalizer(rcNode, rcNodeFinalizer) {
		log.Info("Cleaning up K8s Node for deleted RcNode")
		if err := r.deleteNode(ctx, rcNode.Name); err != nil {
			return ctrl.Result{}, err
		}

		controllerutil.RemoveFinalizer(rcNode, rcNodeFinalizer)
		if err := r.Update(ctx, rcNode); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

// ─── K8s Node CRUD ───────────────────────────────────────────────────────────

// createOrUpdateNode creates or updates the fake K8s Node for an RcNode.
func (r *RcNodeReconciler) createOrUpdateNode(ctx context.Context, rcNode *reclusteriov1.RcNode) error {
	log := logf.FromContext(ctx).WithValues("rcnode", rcNode.Name)

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: rcNode.Name,
		},
	}

	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, node, func() error {
		// Labels
		if node.Labels == nil {
			node.Labels = make(map[string]string)
		}
		// KWOK marker so other controllers/schedulers know this is simulated
		node.Labels["kwok.x-k8s.io/node"] = "fake"
		node.Labels["type"] = "kwok"
		// RcNode metadata
		node.Labels["recluster.io/node-group"] = rcNode.Spec.NodeGroup
		node.Labels["recluster.io/managed-by"] = "recluster"
		node.Labels["kubernetes.io/hostname"] = rcNode.Name
		// Copy user-defined labels from RcNode spec
		for k, v := range rcNode.Spec.Labels {
			node.Labels[k] = v
		}

		// Annotations
		if node.Annotations == nil {
			node.Annotations = make(map[string]string)
		}
		node.Annotations["recluster.io/rcnode"] = rcNode.Name

		// ProviderID
		node.Spec.ProviderID = fmt.Sprintf("recluster://%s", rcNode.Name)

		// Build allocatable and capacity from RcNode resources
		allocatable := make(corev1.ResourceList)
		capacity := make(corev1.ResourceList)

		for resName, qty := range rcNode.Spec.Resources.Allocatable {
			allocatable[corev1.ResourceName(resName)] = qty
			// Capacity slightly larger than allocatable (system reserved)
			capacity[corev1.ResourceName(resName)] = qty
		}
		// Add reserved to capacity
		for resName, qty := range rcNode.Spec.Resources.Reserved {
			if existing, ok := capacity[corev1.ResourceName(resName)]; ok {
				existing.Add(qty)
				capacity[corev1.ResourceName(resName)] = existing
			} else {
				capacity[corev1.ResourceName(resName)] = qty
			}
		}

		// Ensure pods limit exists
		if _, ok := allocatable[corev1.ResourcePods]; !ok {
			allocatable[corev1.ResourcePods] = resource.MustParse("110")
			capacity[corev1.ResourcePods] = resource.MustParse("110")
		}

		node.Status.Allocatable = allocatable
		node.Status.Capacity = capacity

		// Set Ready condition
		node.Status.Conditions = []corev1.NodeCondition{
			{
				Type:               corev1.NodeReady,
				Status:             corev1.ConditionTrue,
				LastHeartbeatTime:  metav1.Now(),
				LastTransitionTime: metav1.Now(),
				Reason:             "KWOKSimulation",
				Message:            "recluster simulated node is ready",
			},
			{
				Type:               corev1.NodeMemoryPressure,
				Status:             corev1.ConditionFalse,
				LastHeartbeatTime:  metav1.Now(),
				LastTransitionTime: metav1.Now(),
				Reason:             "KWOKSimulation",
				Message:            "simulated - no memory pressure",
			},
			{
				Type:               corev1.NodeDiskPressure,
				Status:             corev1.ConditionFalse,
				LastHeartbeatTime:  metav1.Now(),
				LastTransitionTime: metav1.Now(),
				Reason:             "KWOKSimulation",
				Message:            "simulated - no disk pressure",
			},
			{
				Type:               corev1.NodePIDPressure,
				Status:             corev1.ConditionFalse,
				LastHeartbeatTime:  metav1.Now(),
				LastTransitionTime: metav1.Now(),
				Reason:             "KWOKSimulation",
				Message:            "simulated - no PID pressure",
			},
		}

		// Node info from features
		arch := "amd64"
		if archFeat := rcNode.GetFeatureValue("compute.architecture"); archFeat != "" {
			arch = archFeat
		}
		node.Status.NodeInfo = corev1.NodeSystemInfo{
			MachineID:               rcNode.Name,
			SystemUUID:              rcNode.Name,
			KubeletVersion:          "v1.31.0-kwok-sim",
			KubeProxyVersion:        "v1.31.0-kwok-sim",
			OperatingSystem:         "linux",
			Architecture:            arch,
			ContainerRuntimeVersion: "containerd://simulated",
		}

		// Per-node taint: only pods with the matching toleration (added
		// by the periodic reconciler when the solver assigns them here)
		// can be scheduled onto this node. This lets the standard K8s
		// scheduler perform the actual binding safely.
		node.Spec.Taints = []corev1.Taint{
			{
				Key:    "recluster.io/node",
				Value:  rcNode.Name,
				Effect: corev1.TaintEffectNoSchedule,
			},
		}

		return nil
	})

	if err != nil {
		return fmt.Errorf("create/update K8s Node %q: %w", rcNode.Name, err)
	}

	log.Info("K8s Node synced", "operation", op)
	return nil
}

// deleteNode deletes the K8s Node if it exists.
func (r *RcNodeReconciler) deleteNode(ctx context.Context, name string) error {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
	}
	err := r.Delete(ctx, node)
	if errors.IsNotFound(err) {
		return nil
	}
	return err
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

// setStatus updates the RcNode status fields and persists them.
func (r *RcNodeReconciler) setStatus(ctx context.Context, rcNode *reclusteriov1.RcNode, phase reclusteriov1.NodePhase, message string) (ctrl.Result, error) {
	now := metav1.Now()
	rcNode.Status.CurrentPhase = phase
	rcNode.Status.Message = message
	rcNode.Status.LastTransitionTime = &now

	if err := r.Status().Update(ctx, rcNode); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// getBootDelay returns the simulated boot delay from the RcNode's
// boot.timeseconds feature, with ±20% jitter.
func (r *RcNodeReconciler) getBootDelay(rcNode *reclusteriov1.RcNode) time.Duration {
	seconds := defaultBootTimeSeconds

	if val := rcNode.GetFeatureValue("boot.timeseconds"); val != "" {
		if parsed, err := strconv.Atoi(val); err == nil && parsed > 0 {
			seconds = parsed
		}
	}

	// Add jitter: ±20%
	jitter := float64(seconds) * bootTimeJitterFraction
	offset := (rand.Float64()*2 - 1) * jitter // range [-jitter, +jitter]
	finalSeconds := float64(seconds) + offset

	if finalSeconds < 1 {
		finalSeconds = 1
	}

	return time.Duration(finalSeconds) * time.Second
}

// SetupWithManager sets up the controller with the Manager.
func (r *RcNodeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&reclusteriov1.RcNode{}).
		Named("rcnode").
		Complete(r)
}
