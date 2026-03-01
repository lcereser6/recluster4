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

package v1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// WakeMethod defines how the node can be powered on
// +kubebuilder:validation:Enum=wol;smartplug;ssh;manual
type WakeMethod string

const (
	// WakeMethodWOL uses Wake-on-LAN to power on the node
	WakeMethodWOL WakeMethod = "wol"
	// WakeMethodSmartPlug uses a smart plug to power on the node
	WakeMethodSmartPlug WakeMethod = "smartplug"
	// WakeMethodSSH uses SSH to wake/resume the node
	WakeMethodSSH WakeMethod = "ssh"
	// WakeMethodManual requires manual intervention to power on
	WakeMethodManual WakeMethod = "manual"
)

// NodePhase represents the desired operational state of the node
// +kubebuilder:validation:Enum=Online;Offline;Booting;ShuttingDown;Error
type NodePhase string

const (
	NodePhaseOnline       NodePhase = "Online"
	NodePhaseOffline      NodePhase = "Offline"
	NodePhaseBooting      NodePhase = "Booting"
	NodePhaseShuttingDown NodePhase = "ShuttingDown"
	NodePhaseError        NodePhase = "Error"
)

// FeatureType defines the type of a feature value
// +kubebuilder:validation:Enum=quantity;integer;float;string;boolean
type FeatureType string

const (
	// FeatureTypeQuantity for Kubernetes resource quantities (e.g., "4Gi", "2000m")
	FeatureTypeQuantity FeatureType = "quantity"
	// FeatureTypeInteger for whole numbers
	FeatureTypeInteger FeatureType = "integer"
	// FeatureTypeFloat for decimal numbers
	FeatureTypeFloat FeatureType = "float"
	// FeatureTypeString for text values
	FeatureTypeString FeatureType = "string"
	// FeatureTypeBoolean for true/false values
	FeatureTypeBoolean FeatureType = "boolean"
)

// Feature represents a single characteristic of an RcNode.
// Features are flexible key-value pairs that allow users to define
// any node characteristic without schema limitations.
//
// Common feature names (recommended conventions):
//   - compute.cores: Number of CPU cores (integer)
//   - compute.threads: Total threads (integer)
//   - compute.architecture: CPU architecture like arm64, amd64 (string)
//   - compute.model: CPU model name (string)
//   - compute.clockMhz: Base clock speed (integer, unit: MHz)
//   - compute.benchmark.single: Single-thread benchmark score (integer)
//   - compute.benchmark.multi: Multi-thread benchmark score (integer)
//   - memory.total: Total memory (quantity, e.g., "8Gi")
//   - memory.type: Memory type like DDR4, DDR5 (string)
//   - memory.bandwidthMbps: Memory bandwidth (integer, unit: MB/s)
//   - storage.total: Total storage (quantity, e.g., "256Gi")
//   - storage.type: Storage type like SSD, NVMe, HDD (string)
//   - storage.readMbps: Sequential read speed (integer, unit: MB/s)
//   - storage.writeMbps: Sequential write speed (integer, unit: MB/s)
//   - power.idle: Idle power consumption (integer, unit: watts)
//   - power.max: Maximum power consumption (integer, unit: watts)
//   - network.bandwidth: Network bandwidth (quantity, e.g., "1Gi")
//   - boot.timeSeconds: Expected boot time (integer, unit: seconds)
//   - shutdown.timeSeconds: Expected shutdown time (integer, unit: seconds)
type Feature struct {
	// Name is the unique identifier for this feature.
	// Use dot notation for categories (e.g., "cpu.cores", "memory.total", "power.idle")
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[a-z][a-z0-9]*(\.[a-z][a-z0-9]*)*$`
	Name string `json:"name"`

	// Type defines how to interpret the value
	// +kubebuilder:default=string
	Type FeatureType `json:"type,omitempty"`

	// Value is the string representation of the feature value.
	// Interpreted according to Type:
	// - quantity: Kubernetes quantity (e.g., "4Gi", "2000m", "500Mi")
	// - integer: Whole number (e.g., "4", "8", "16")
	// - float: Decimal number (e.g., "3.14", "2.5")
	// - string: Text value (e.g., "DDR4", "arm64")
	// - boolean: "true" or "false"
	// +kubebuilder:validation:Required
	Value string `json:"value"`

	// Unit is an optional display unit for the value (e.g., "MHz", "MB/s", "watts").
	// This is for documentation purposes and UI display.
	// +optional
	Unit string `json:"unit,omitempty"`

	// Description provides human-readable context for this feature
	// +optional
	Description string `json:"description,omitempty"`
}

// KubernetesResources defines the Kubernetes-native resources for scheduling.
// These are the resources that will be reported to the Kubernetes scheduler.
type KubernetesResources struct {
	// Allocatable resources available for pods
	// +optional
	Allocatable map[string]resource.Quantity `json:"allocatable,omitempty"`
	// Reserved resources for system daemons
	// +optional
	Reserved map[string]resource.Quantity `json:"reserved,omitempty"`
}

// WOLConfig contains Wake-on-LAN configuration
type WOLConfig struct {
	// MAC address for Wake-on-LAN
	MACAddress string `json:"macAddress"`
	// Broadcast address for Wake-on-LAN packets
	// +optional
	BroadcastAddress string `json:"broadcastAddress,omitempty"`
}

// SmartPlugConfig contains smart plug configuration
type SmartPlugConfig struct {
	// IP address or hostname of the smart plug
	Host string `json:"host"`
	// Port number for the smart plug API
	// +optional
	Port int `json:"port,omitempty"`
	// Plug ID for multi-outlet smart plugs
	// +optional
	PlugID string `json:"plugId,omitempty"`
	// API token or password for authentication (reference to a secret)
	// +optional
	AuthSecretRef string `json:"authSecretRef,omitempty"`
}

// SSHConfig contains SSH wake configuration
type SSHConfig struct {
	// SSH host to connect to (jump host or IPMI)
	Host string `json:"host"`
	// SSH port
	// +optional
	Port int `json:"port,omitempty"`
	// SSH user
	User string `json:"user"`
	// Reference to secret containing SSH key or password
	// +optional
	AuthSecretRef string `json:"authSecretRef,omitempty"`
	// Command to execute to wake the node
	// +optional
	WakeCommand string `json:"wakeCommand,omitempty"`
}

// ActivationSpec defines how to boot/shutdown the node
type ActivationSpec struct {
	// Method used to wake/power on the node
	WakeMethod WakeMethod `json:"wakeMethod"`
	// Wake-on-LAN configuration (required if wakeMethod is "wol")
	// +optional
	WOLConfig *WOLConfig `json:"wolConfig,omitempty"`
	// Smart plug configuration (required if wakeMethod is "smartplug")
	// +optional
	SmartPlugConfig *SmartPlugConfig `json:"smartPlugConfig,omitempty"`
	// SSH configuration (required if wakeMethod is "ssh")
	// +optional
	SSHConfig *SSHConfig `json:"sshConfig,omitempty"`
}

// RcNodeSpec defines the desired state of RcNode.
type RcNodeSpec struct {
	// NodeGroup this node belongs to (optional grouping for similar nodes)
	// +optional
	NodeGroup string `json:"nodeGroup,omitempty"`

	// DesiredPhase is the desired operational state of the node
	// +kubebuilder:default=Offline
	DesiredPhase NodePhase `json:"desiredPhase,omitempty"`

	// Activation defines how to boot/shutdown the node (required)
	Activation ActivationSpec `json:"activation"`

	// Resources defines the Kubernetes-native resources for scheduling.
	// These will be reported to the scheduler (cpu, memory, ephemeral-storage, etc.)
	// +optional
	Resources KubernetesResources `json:"resources,omitempty"`

	// Features is a flexible array of node characteristics.
	// Users can define any feature they want using the Feature schema.
	// Features are used for scoring, filtering, and display purposes.
	// +optional
	Features []Feature `json:"features,omitempty"`

	// Labels are additional labels to apply to the node when it joins the cluster
	// +optional
	Labels map[string]string `json:"labels,omitempty"`

	// Taints to apply to the node when it joins the cluster
	// +optional
	Taints []string `json:"taints,omitempty"`
}

// RcNodeStatus defines the observed state of RcNode.
type RcNodeStatus struct {
	// CurrentPhase is the current operational state of the node
	// +optional
	CurrentPhase NodePhase `json:"currentPhase,omitempty"`

	// LastTransitionTime is the last time the phase changed
	// +optional
	LastTransitionTime *metav1.Time `json:"lastTransitionTime,omitempty"`

	// Message provides additional information about the current state
	// +optional
	Message string `json:"message,omitempty"`

	// Conditions represent the latest available observations of the node's state
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// LastBootTime records when the node last started booting
	// +optional
	LastBootTime *metav1.Time `json:"lastBootTime,omitempty"`

	// LastShutdownTime records when the node last started shutting down
	// +optional
	LastShutdownTime *metav1.Time `json:"lastShutdownTime,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Node Group",type=string,JSONPath=`.spec.nodeGroup`
// +kubebuilder:printcolumn:name="Desired",type=string,JSONPath=`.spec.desiredPhase`
// +kubebuilder:printcolumn:name="Current",type=string,JSONPath=`.status.currentPhase`
// +kubebuilder:printcolumn:name="Wake Method",type=string,JSONPath=`.spec.activation.wakeMethod`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// RcNode is the Schema for the rcnodes API.
// RcNode represents a physical or virtual node that can be brought online/offline
// dynamically by the recluster controller.
type RcNode struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RcNodeSpec   `json:"spec,omitempty"`
	Status RcNodeStatus `json:"status,omitempty"`
}

// GetFeature returns the feature with the given name, or nil if not found
func (r *RcNode) GetFeature(name string) *Feature {
	for i := range r.Spec.Features {
		if r.Spec.Features[i].Name == name {
			return &r.Spec.Features[i]
		}
	}
	return nil
}

// GetFeatureValue returns the value of the feature with the given name, or empty string if not found
func (r *RcNode) GetFeatureValue(name string) string {
	if f := r.GetFeature(name); f != nil {
		return f.Value
	}
	return ""
}

// GetFeaturesByPrefix returns all features whose names start with the given prefix
func (r *RcNode) GetFeaturesByPrefix(prefix string) []Feature {
	var result []Feature
	for _, f := range r.Spec.Features {
		if len(f.Name) >= len(prefix) && f.Name[:len(prefix)] == prefix {
			result = append(result, f)
		}
	}
	return result
}

// +kubebuilder:object:root=true

// RcNodeList contains a list of RcNode.
type RcNodeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RcNode `json:"items"`
}

func init() {
	SchemeBuilder.Register(&RcNode{}, &RcNodeList{})
}
