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
	"math"
	"strconv"

	reclusteriov1 "github.com/lorenzocereser/recluster4/api/v1"
)

// DefaultConsolidationBonus is the bonus score for using an already-ready node
const DefaultConsolidationBonus = 100.0

// DefaultRebalancingBonus is the bonus score for placing on preferred node
const DefaultRebalancingBonus = 50.0

// ComputeNodeBaseScore calculates the base score for an RcNode based on the policy multipliers.
// This score is computed once per node and reused for all pod assignments.
func ComputeNodeBaseScore(node *RcNodeCandidate, policy *reclusteriov1.RcPolicy) float64 {
	if policy == nil || len(policy.Spec.Multipliers) == 0 {
		return 0.0
	}

	var totalScore float64
	var totalWeight float64

	for _, mult := range policy.Spec.Multipliers {
		// Find matching feature in node
		featureValue, exists := node.Features[mult.Name]
		if !exists {
			continue
		}

		// Parse feature value to float
		value, err := parseFeatureValue(featureValue)
		if err != nil {
			continue
		}

		// Apply normalization if specified
		if mult.CurveParams.Normalize > 0 {
			value = value / mult.CurveParams.Normalize
		}

		// Apply curve transformation
		transformedValue := applyCurve(value, mult.Curve, mult.CurveParams)

		// Apply multiplier value
		score := transformedValue * mult.Value

		// Apply min/max clamping
		if mult.CurveParams.Min != nil && score < *mult.CurveParams.Min {
			score = *mult.CurveParams.Min
		}
		if mult.CurveParams.Max != nil && score > *mult.CurveParams.Max {
			score = *mult.CurveParams.Max
		}

		// Weight the score
		weight := float64(mult.Weight)
		if weight == 0 {
			weight = 100 // default weight
		}

		totalScore += score * weight
		totalWeight += weight
	}

	// Return weighted average
	if totalWeight == 0 {
		return 0.0
	}
	return totalScore / totalWeight
}

// applyCurve transforms a value according to the specified curve type
func applyCurve(value float64, curveType reclusteriov1.CurveType, params reclusteriov1.CurveParameters) float64 {
	switch curveType {
	case reclusteriov1.CurveTypeLinear, "":
		return value

	case reclusteriov1.CurveTypeLogarithmic:
		if value <= 0 {
			return 0
		}
		return math.Log(value + 1)

	case reclusteriov1.CurveTypeExponential:
		exponent := params.Exponent
		if exponent == 0 {
			exponent = 2.0
		}
		return math.Pow(value, exponent)

	case reclusteriov1.CurveTypeInverse:
		offset := params.Offset
		if offset == 0 {
			offset = 1.0
		}
		return 1.0 / (value + offset)

	case reclusteriov1.CurveTypeStep:
		if value >= params.Threshold {
			return 1.0
		}
		return 0.0

	case reclusteriov1.CurveTypeSigmoid:
		midpoint := params.Midpoint
		scale := params.Scale
		if scale == 0 {
			scale = 1.0
		}
		x := (value - midpoint) / scale
		return 1.0 / (1.0 + math.Exp(-x))

	default:
		return value
	}
}

// parseFeatureValue attempts to parse a feature value string to float64
func parseFeatureValue(value string) (float64, error) {
	// Try direct float parse
	if f, err := strconv.ParseFloat(value, 64); err == nil {
		return f, nil
	}

	// Try integer parse
	if i, err := strconv.ParseInt(value, 10, 64); err == nil {
		return float64(i), nil
	}

	// Try parsing Kubernetes quantity (e.g., "4Gi", "500m")
	// For now, just handle simple numeric strings
	// TODO: Add proper resource.Quantity parsing

	return 0, nil
}

// ComputeAssignmentScore calculates the score for assigning a specific pod to a node
func ComputeAssignmentScore(pod *PodCandidate, node *RcNodeCandidate, policy *reclusteriov1.RcPolicy) float64 {
	// Start with node's base score
	score := node.BaseScore

	// Consolidation bonus: prefer already-ready nodes
	if node.IsReady {
		score += DefaultConsolidationBonus
	}

	// Rebalancing bonus: prefer the hinted node
	if pod.PreferredNode != "" && pod.PreferredNode == node.Name {
		score += DefaultRebalancingBonus
	}

	return score
}

// NormalizeScores normalizes node scores to [0, 1] range across the fleet
func NormalizeScores(nodes []RcNodeCandidate) {
	if len(nodes) == 0 {
		return
	}

	// Find min and max scores
	minScore := nodes[0].BaseScore
	maxScore := nodes[0].BaseScore

	for _, node := range nodes {
		if node.BaseScore < minScore {
			minScore = node.BaseScore
		}
		if node.BaseScore > maxScore {
			maxScore = node.BaseScore
		}
	}

	// Normalize
	scoreRange := maxScore - minScore
	for i := range nodes {
		if scoreRange == 0 {
			nodes[i].NormalizedScore = 1.0 // All equal, give max score
		} else {
			nodes[i].NormalizedScore = (nodes[i].BaseScore - minScore) / scoreRange
		}
	}
}

// CountFeasiblePairs counts the number of feasible pod-to-node pairs
// Used for algorithm selection
func CountFeasiblePairs(pods []PodCandidate, nodes []RcNodeCandidate) int {
	count := 0
	for _, pod := range pods {
		for _, node := range nodes {
			if CanFit(pod, node) {
				count++
			}
		}
	}
	return count
}

// CanFit checks if a pod can fit on a node based on resources
func CanFit(pod PodCandidate, node RcNodeCandidate) bool {
	// Check CPU
	if pod.CPURequest.Cmp(node.RemainingCPU) > 0 {
		return false
	}

	// Check Memory
	if pod.MemoryRequest.Cmp(node.RemainingMemory) > 0 {
		return false
	}

	return true
}

// GetEfficiencyScore calculates efficiency score (compute per watt)
func GetEfficiencyScore(node *RcNodeCandidate) float64 {
	if node.IdleWatts == 0 {
		return 0
	}

	cpuMillis := node.AllocatableCPU.MilliValue()
	return float64(cpuMillis) / float64(node.IdleWatts)
}

// GetBootSpeedScore calculates boot speed score (faster = higher)
func GetBootSpeedScore(node *RcNodeCandidate) float64 {
	if node.BootTimeSeconds == 0 {
		return 1.0 // Assume instant if not specified
	}
	return 1.0 / float64(node.BootTimeSeconds)
}
