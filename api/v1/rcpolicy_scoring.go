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
	"math"
	"strconv"
)

// ApplyCurve applies the scoring curve to a value and returns the transformed result
func ApplyCurve(value float64, curve CurveType, params CurveParameters) float64 {
	// Apply normalization if specified
	if params.Normalize > 0 {
		value = value / params.Normalize
	}

	var result float64

	switch curve {
	case CurveTypeLinear:
		result = value

	case CurveTypeLogarithmic:
		// log(value + 1) to handle value = 0
		result = math.Log(value + 1)

	case CurveTypeExponential:
		exponent := params.Exponent
		if exponent == 0 {
			exponent = 2.0
		}
		result = math.Pow(value, exponent)

	case CurveTypeInverse:
		offset := params.Offset
		if offset == 0 {
			offset = 1.0
		}
		// Higher values result in lower scores (good for latency, power, etc.)
		result = 1.0 / (value + offset)

	case CurveTypeStep:
		if value >= params.Threshold {
			result = 1.0
		} else {
			result = 0.0
		}

	case CurveTypeSigmoid:
		scale := params.Scale
		if scale == 0 {
			scale = 1.0
		}
		// Sigmoid: 1 / (1 + e^(-(value - midpoint) / scale))
		x := (value - params.Midpoint) / scale
		result = 1.0 / (1.0 + math.Exp(-x))

	default:
		// Default to linear
		result = value
	}

	// Apply clamping if specified
	if params.Min != nil && result < *params.Min {
		result = *params.Min
	}
	if params.Max != nil && result > *params.Max {
		result = *params.Max
	}

	return result
}

// ApplyMultiplier applies a multiplier to a feature value
// Returns: curve(featureValue) * multiplier.Value * (weight/100)
func ApplyMultiplier(featureValue float64, m Multiplier) float64 {
	// Apply curve transformation
	transformed := ApplyCurve(featureValue, m.Curve, m.CurveParams)

	// Apply multiplier value
	score := transformed * m.Value

	// Apply weight as a percentage
	if m.Weight > 0 && m.Weight <= 100 {
		score = score * (float64(m.Weight) / 100.0)
	}

	return score
}

// CalculateNodeScore calculates the total score for an RcNode based on the policy
func (p *RcPolicy) CalculateNodeScore(node *RcNode) float64 {
	if p == nil || node == nil {
		return 0
	}

	// Check if node matches the policy's selectors
	if !p.MatchesNode(node) {
		return 0
	}

	score := p.Spec.BaseScore

	// Build a feature map for quick lookup
	featureMap := make(map[string]Feature)
	for _, f := range node.Spec.Features {
		featureMap[f.Name] = f
	}

	// Apply each multiplier
	for _, mult := range p.Spec.Multipliers {
		feature, exists := featureMap[mult.Name]
		if !exists {
			if mult.Required {
				return 0 // Required feature missing, score is 0
			}
			continue
		}

		// Get numeric value from feature
		featureValue := getFeatureNumericValue(feature)
		multiplierScore := ApplyMultiplier(featureValue, mult)
		score += multiplierScore
	}

	// Apply time constraints
	score += p.applyTimeConstraints(node, featureMap)

	// Normalize if requested
	if p.Spec.NormalizeScores {
		// Normalize to 0-100 range using sigmoid
		score = 100.0 / (1.0 + math.Exp(-score/10.0))
	}

	return score
}

// applyTimeConstraints applies time-based scoring adjustments
func (p *RcPolicy) applyTimeConstraints(node *RcNode, featureMap map[string]Feature) float64 {
	tc := p.Spec.TimeConstraints
	var bonus float64

	// Bonus for online nodes
	if tc.PreferOnlineNodes && node.Status.CurrentPhase == NodePhaseOnline {
		bonus += tc.OnlineBonus
	}

	// Penalty for slow boot times
	if tc.MaxBootTimeSeconds > 0 {
		if bootFeature, exists := featureMap["boot.timeSeconds"]; exists {
			bootTime := getFeatureNumericValue(bootFeature)
			if bootTime > float64(tc.MaxBootTimeSeconds) {
				// Penalize nodes that exceed max boot time
				bonus -= (bootTime - float64(tc.MaxBootTimeSeconds)) * 10
			}
		}
	}

	// Penalty for slow shutdown times
	if tc.MaxShutdownTimeSeconds > 0 {
		if shutdownFeature, exists := featureMap["shutdown.timeSeconds"]; exists {
			shutdownTime := getFeatureNumericValue(shutdownFeature)
			if shutdownTime > float64(tc.MaxShutdownTimeSeconds) {
				bonus -= (shutdownTime - float64(tc.MaxShutdownTimeSeconds)) * 5
			}
		}
	}

	return bonus
}

// MatchesNode checks if a node matches the policy's selectors
func (p *RcPolicy) MatchesNode(node *RcNode) bool {
	if p == nil || node == nil {
		return false
	}

	// Check node selector
	if len(p.Spec.NodeSelector) > 0 {
		for k, v := range p.Spec.NodeSelector {
			if nodeVal, exists := node.Spec.Labels[k]; !exists || nodeVal != v {
				return false
			}
		}
	}

	// Check node group selector
	if len(p.Spec.NodeGroupSelector) > 0 {
		nodeGroup := node.Spec.NodeGroup
		matched := false
		for _, ng := range p.Spec.NodeGroupSelector {
			if ng == nodeGroup {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}

	return true
}

// MatchesDeployment checks if a deployment matches this policy's deployment selector
func (p *RcPolicy) MatchesDeployment(labels, annotations map[string]string, namespace, name string) bool {
	if p == nil {
		return false
	}

	sel := p.Spec.DeploymentSelector

	// If no selector specified, policy must be explicitly referenced
	if len(sel.MatchLabels) == 0 && len(sel.MatchAnnotations) == 0 &&
		len(sel.Namespaces) == 0 && len(sel.Names) == 0 {
		return false
	}

	// Check label selector
	if len(sel.MatchLabels) > 0 {
		for k, v := range sel.MatchLabels {
			if labelVal, exists := labels[k]; !exists || labelVal != v {
				return false
			}
		}
	}

	// Check annotation selector
	if len(sel.MatchAnnotations) > 0 {
		for k, v := range sel.MatchAnnotations {
			if annoVal, exists := annotations[k]; !exists || annoVal != v {
				return false
			}
		}
	}

	// Check namespace selector
	if len(sel.Namespaces) > 0 {
		matched := false
		for _, ns := range sel.Namespaces {
			if ns == namespace {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}

	// Check name selector
	if len(sel.Names) > 0 {
		matched := false
		for _, n := range sel.Names {
			if n == name {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}

	return true
}

// getFeatureNumericValue extracts a numeric value from a feature
func getFeatureNumericValue(f Feature) float64 {
	switch f.Type {
	case FeatureTypeInteger:
		val, err := strconv.ParseInt(f.Value, 10, 64)
		if err != nil {
			return 0
		}
		return float64(val)
	case FeatureTypeFloat:
		val, err := strconv.ParseFloat(f.Value, 64)
		if err != nil {
			return 0
		}
		return val
	case FeatureTypeBoolean:
		val, err := strconv.ParseBool(f.Value)
		if err != nil {
			return 0
		}
		if val {
			return 1.0
		}
		return 0.0
	case FeatureTypeQuantity:
		// Parse capacity string (e.g., "16Gi", "500Mi")
		return parseCapacity(f.Value)
	default:
		// Try to parse as float
		val, err := strconv.ParseFloat(f.Value, 64)
		if err != nil {
			return 0
		}
		return val
	}
}

// parseCapacity parses a Kubernetes-style capacity string to bytes
func parseCapacity(s string) float64 {
	if len(s) == 0 {
		return 0
	}

	multiplier := 1.0
	numStr := s

	// Check for suffixes
	suffixes := []struct {
		suffix string
		mult   float64
	}{
		{"Ti", 1024 * 1024 * 1024 * 1024},
		{"Gi", 1024 * 1024 * 1024},
		{"Mi", 1024 * 1024},
		{"Ki", 1024},
		{"T", 1000 * 1000 * 1000 * 1000},
		{"G", 1000 * 1000 * 1000},
		{"M", 1000 * 1000},
		{"K", 1000},
	}

	for _, suf := range suffixes {
		if len(s) > len(suf.suffix) && s[len(s)-len(suf.suffix):] == suf.suffix {
			numStr = s[:len(s)-len(suf.suffix)]
			multiplier = suf.mult
			break
		}
	}

	val, err := strconv.ParseFloat(numStr, 64)
	if err != nil {
		return 0
	}

	return val * multiplier
}
