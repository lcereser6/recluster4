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

package webhook

import (
	"context"
	"encoding/json"
	"net/http"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

const (
	// SchedulingGateName is the name of the scheduling gate added to pods
	SchedulingGateName = "recluster.io/scheduling-gate"
)

// PodSchedulingGateWebhook is a mutating webhook that adds a scheduling gate to pods
type PodSchedulingGateWebhook struct {
	decoder admission.Decoder
}

// NewPodSchedulingGateWebhook creates a new PodSchedulingGateWebhook
func NewPodSchedulingGateWebhook(decoder admission.Decoder) *PodSchedulingGateWebhook {
	return &PodSchedulingGateWebhook{
		decoder: decoder,
	}
}

// Handle implements the admission.Handler interface
func (w *PodSchedulingGateWebhook) Handle(ctx context.Context, req admission.Request) admission.Response {
	logger := log.FromContext(ctx).WithValues(
		"webhook", "pod-scheduling-gate",
		"namespace", req.Namespace,
		"name", req.Name,
	)

	pod := &corev1.Pod{}
	if err := w.decoder.Decode(req, pod); err != nil {
		logger.Error(err, "failed to decode pod")
		return admission.Errored(http.StatusBadRequest, err)
	}

	// Skip if pod already has the scheduling gate
	if hasSchedulingGate(pod) {
		logger.V(1).Info("pod already has scheduling gate, skipping")
		return admission.Allowed("pod already has scheduling gate")
	}

	// Skip system namespaces
	if isSystemNamespace(pod.Namespace) {
		logger.V(1).Info("skipping system namespace pod")
		return admission.Allowed("system namespace pod")
	}

	// Add the scheduling gate
	logger.Info("adding scheduling gate to pod")
	addSchedulingGate(pod)

	// Marshal the mutated pod
	marshaledPod, err := json.Marshal(pod)
	if err != nil {
		logger.Error(err, "failed to marshal pod")
		return admission.Errored(http.StatusInternalServerError, err)
	}

	return admission.PatchResponseFromRaw(req.Object.Raw, marshaledPod)
}

// hasSchedulingGate checks if the pod already has the recluster scheduling gate
func hasSchedulingGate(pod *corev1.Pod) bool {
	if pod.Spec.SchedulingGates == nil {
		return false
	}
	for _, gate := range pod.Spec.SchedulingGates {
		if gate.Name == SchedulingGateName {
			return true
		}
	}
	return false
}

// addSchedulingGate adds the recluster scheduling gate to the pod
func addSchedulingGate(pod *corev1.Pod) {
	if pod.Spec.SchedulingGates == nil {
		pod.Spec.SchedulingGates = []corev1.PodSchedulingGate{}
	}
	pod.Spec.SchedulingGates = append(pod.Spec.SchedulingGates, corev1.PodSchedulingGate{
		Name: SchedulingGateName,
	})
}

// isSystemNamespace returns true if the namespace is a system namespace
// that should not have scheduling gates injected.
// This must match the namespaceSelector in the MutatingWebhookConfiguration.
func isSystemNamespace(namespace string) bool {
	systemNamespaces := map[string]bool{
		"kube-system":       true,
		"kube-public":       true,
		"kube-node-lease":   true,
		"recluster4-system": true,
		"monitoring":        true,
		"cert-manager":      true,
	}
	return systemNamespaces[namespace]
}
