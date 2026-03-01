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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// CurveType defines the type of scoring curve to apply
// +kubebuilder:validation:Enum=linear;logarithmic;exponential;inverse;step;sigmoid
type CurveType string

const (
	// CurveTypeLinear: score = multiplier * value
	// Simple linear relationship
	CurveTypeLinear CurveType = "linear"

	// CurveTypeLogarithmic: score = multiplier * log(value + 1)
	// Diminishing returns - good for resources where more is better but with limits
	CurveTypeLogarithmic CurveType = "logarithmic"

	// CurveTypeExponential: score = multiplier * (value ^ exponent)
	// Accelerating returns - good for penalizing bad values heavily
	CurveTypeExponential CurveType = "exponential"

	// CurveTypeInverse: score = multiplier * (1 / (value + offset))
	// Lower values are better (e.g., latency, boot time, power consumption)
	CurveTypeInverse CurveType = "inverse"

	// CurveTypeStep: score = multiplier if value >= threshold, else 0
	// Binary scoring based on meeting a threshold
	CurveTypeStep CurveType = "step"

	// CurveTypeSigmoid: score = multiplier * sigmoid((value - midpoint) / scale)
	// S-curve with smooth transition around a midpoint
	CurveTypeSigmoid CurveType = "sigmoid"
)

// CurveParameters defines the parameters for different curve types
type CurveParameters struct {
	// Exponent for exponential curve (default: 2.0)
	// +kubebuilder:default=2.0
	// +optional
	Exponent float64 `json:"exponent,omitempty"`

	// Offset for inverse curve to avoid division by zero (default: 1.0)
	// +kubebuilder:default=1.0
	// +optional
	Offset float64 `json:"offset,omitempty"`

	// Threshold for step curve - value must be >= this to score
	// +optional
	Threshold float64 `json:"threshold,omitempty"`

	// Midpoint for sigmoid curve - the center of the S-curve
	// +optional
	Midpoint float64 `json:"midpoint,omitempty"`

	// Scale for sigmoid curve - controls steepness (default: 1.0)
	// +kubebuilder:default=1.0
	// +optional
	Scale float64 `json:"scale,omitempty"`

	// Min clamps the output score to this minimum value
	// +optional
	Min *float64 `json:"min,omitempty"`

	// Max clamps the output score to this maximum value
	// +optional
	Max *float64 `json:"max,omitempty"`

	// Normalize divides the input value by this before applying the curve
	// Useful for normalizing different units (e.g., divide bytes by 1Gi)
	// +optional
	Normalize float64 `json:"normalize,omitempty"`
}

// Multiplier represents a scoring multiplier for an RcNode feature.
// Multipliers mirror the Feature structure in RcNode, allowing direct
// matching between policy multipliers and node features.
//
// The Name field must match an RcNode feature name exactly.
// When calculating scores: score = nodeFeatureValue * multiplier.value
//
// Common multiplier names (matching RcNode features):
//   - compute.cores: Multiplier for CPU cores
//   - compute.threads: Multiplier for threads
//   - compute.clockMhz: Multiplier for clock speed
//   - compute.benchmark.single: Multiplier for single-thread benchmark
//   - compute.benchmark.multi: Multiplier for multi-thread benchmark
//   - memory.total: Multiplier for total memory
//   - memory.bandwidthMbps: Multiplier for memory bandwidth
//   - storage.total: Multiplier for total storage
//   - storage.readMbps: Multiplier for read speed
//   - storage.writeMbps: Multiplier for write speed
//   - power.idle: Multiplier for idle power (usually negative)
//   - power.max: Multiplier for max power (usually negative)
//   - network.bandwidth: Multiplier for network bandwidth
//   - boot.timeSeconds: Multiplier for boot time (usually negative)
//   - shutdown.timeSeconds: Multiplier for shutdown time (usually negative)
type Multiplier struct {
	// Name is the feature name this multiplier applies to.
	// Must match an RcNode feature name exactly (e.g., "compute.cores", "memory.total")
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[a-z][a-z0-9]*(\.[a-z][a-z0-9]*)*$`
	Name string `json:"name"`

	// Value is the multiplier value applied to the feature.
	// Positive values reward higher feature values.
	// Negative values penalize higher feature values (good for power, latency, boot time).
	// +kubebuilder:default=1.0
	Value float64 `json:"value,omitempty"`

	// Curve defines how the feature value is transformed before multiplying.
	// +kubebuilder:default=linear
	Curve CurveType `json:"curve,omitempty"`

	// CurveParams contains parameters for the selected curve
	// +optional
	CurveParams CurveParameters `json:"curveParams,omitempty"`

	// Weight for this multiplier in the overall score (0-100)
	// Higher weight means this multiplier has more influence on the final score
	// +kubebuilder:default=100
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	Weight int `json:"weight,omitempty"`

	// Required if true, nodes without this feature get score 0
	// +optional
	Required bool `json:"required,omitempty"`

	// Description provides human-readable context for this multiplier
	// +optional
	Description string `json:"description,omitempty"`
}

// TimeConstraints defines time-based scheduling constraints
type TimeConstraints struct {
	// MaxBootTimeSeconds is the maximum acceptable boot time for nodes
	// Nodes with boot.timeSeconds > this value will be penalized or excluded
	// +optional
	MaxBootTimeSeconds int `json:"maxBootTimeSeconds,omitempty"`

	// MaxShutdownTimeSeconds is the maximum acceptable shutdown time for nodes
	// +optional
	MaxShutdownTimeSeconds int `json:"maxShutdownTimeSeconds,omitempty"`

	// SchedulingDeadlineSeconds is the deadline for the workload to be scheduled
	// The planner will prioritize nodes that can boot within this deadline
	// +optional
	SchedulingDeadlineSeconds int `json:"schedulingDeadlineSeconds,omitempty"`

	// PreferOnlineNodes if true, gives strong preference to already-online nodes
	// to avoid boot time delays
	// +kubebuilder:default=true
	PreferOnlineNodes bool `json:"preferOnlineNodes,omitempty"`

	// OnlineBonus is the score bonus for nodes that are already online
	// +kubebuilder:default=100
	OnlineBonus float64 `json:"onlineBonus,omitempty"`
}

// DeploymentSelector defines how to select which deployments/workloads this policy applies to
type DeploymentSelector struct {
	// MatchLabels selects deployments with these labels
	// +optional
	MatchLabels map[string]string `json:"matchLabels,omitempty"`

	// MatchAnnotations selects deployments with these annotations
	// +optional
	MatchAnnotations map[string]string `json:"matchAnnotations,omitempty"`

	// Namespaces limits the policy to these namespaces
	// If empty, applies to all namespaces
	// +optional
	Namespaces []string `json:"namespaces,omitempty"`

	// Names is a list of specific deployment names to match
	// +optional
	Names []string `json:"names,omitempty"`
}

// RcPolicySpec defines the desired state of RcPolicy.
// RcPolicy contains scoring multipliers for RcNode features.
// Each multiplier in the Multipliers array corresponds to a feature in RcNode.
// This allows direct multiplication: score = sum(nodeFeature * multiplier) for each matching pair.
type RcPolicySpec struct {
	// Multipliers is the array of feature multipliers.
	// Each multiplier matches an RcNode feature by name and defines
	// how that feature contributes to the node's score.
	// This is the modular, flexible way to define scoring.
	// +optional
	Multipliers []Multiplier `json:"multipliers,omitempty"`

	// TimeConstraints defines time-based scheduling preferences
	// +optional
	TimeConstraints TimeConstraints `json:"timeConstraints,omitempty"`

	// DeploymentSelector defines which deployments this policy applies to
	// If not specified, the policy can be referenced by name in deployment annotations
	// +optional
	DeploymentSelector DeploymentSelector `json:"deploymentSelector,omitempty"`

	// NodeSelector limits this policy to nodes matching these labels
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// NodeGroupSelector limits this policy to nodes in specific node groups
	// If empty, applies to all nodes
	// +optional
	NodeGroupSelector []string `json:"nodeGroupSelector,omitempty"`

	// BaseScore is added to every node's calculated score
	// +kubebuilder:default=0
	// +optional
	BaseScore float64 `json:"baseScore,omitempty"`

	// NormalizeScores if true, normalizes final scores to 0-100 range
	// +kubebuilder:default=false
	// +optional
	NormalizeScores bool `json:"normalizeScores,omitempty"`

	// Priority determines which policy takes precedence when multiple policies match
	// Higher values = higher priority
	// +kubebuilder:default=0
	// +optional
	Priority int `json:"priority,omitempty"`

	// Budgets defines operational limits for this policy
	// +optional
	Budgets PolicyBudgets `json:"budgets,omitempty"`
}

// PolicyBudgets defines operational limits for scheduling decisions
type PolicyBudgets struct {
	// MaxConcurrentPowerOns limits how many nodes can be booted simultaneously
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=1
	// +optional
	MaxConcurrentPowerOns int `json:"maxConcurrentPowerOns,omitempty"`

	// MaxMigrationsPerHour limits pod migrations for rebalancing
	// +kubebuilder:default=10
	// +kubebuilder:validation:Minimum=0
	// +optional
	MaxMigrationsPerHour int `json:"maxMigrationsPerHour,omitempty"`

	// MaxPowerOnsPerHour limits total node power-ons per hour
	// +kubebuilder:default=20
	// +kubebuilder:validation:Minimum=0
	// +optional
	MaxPowerOnsPerHour int `json:"maxPowerOnsPerHour,omitempty"`
}

// RcPolicyStatus defines the observed state of RcPolicy.
type RcPolicyStatus struct {
	// LastUpdated is the timestamp when this policy was last processed
	// +optional
	LastUpdated *metav1.Time `json:"lastUpdated,omitempty"`

	// MatchingNodes is the count of nodes that match this policy's selectors
	// +optional
	MatchingNodes int `json:"matchingNodes,omitempty"`

	// MatchingDeployments is the count of deployments that match this policy
	// +optional
	MatchingDeployments int `json:"matchingDeployments,omitempty"`

	// Message provides additional status information
	// +optional
	Message string `json:"message,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Priority",type=integer,JSONPath=`.spec.priority`
// +kubebuilder:printcolumn:name="Multipliers",type=integer,JSONPath=`.spec.multipliers`
// +kubebuilder:printcolumn:name="Matching Nodes",type=integer,JSONPath=`.status.matchingNodes`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// RcPolicy is the Schema for the rcpolicies API.
// RcPolicy defines scoring multipliers for RcNode characteristics,
// allowing administrators to configure how nodes are scored and prioritized
// for workload placement.
type RcPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RcPolicySpec   `json:"spec,omitempty"`
	Status RcPolicyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// RcPolicyList contains a list of RcPolicy.
type RcPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RcPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(&RcPolicy{}, &RcPolicyList{})
}
