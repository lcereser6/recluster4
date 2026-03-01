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

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	// SolverDuration tracks total solver execution time
	SolverDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "recluster_solver_duration_seconds",
			Help:    "Time spent in solver by algorithm",
			Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0, 2.5, 5.0},
		},
		[]string{"algorithm"},
	)

	// SolverScoringDuration tracks time spent on scoring
	SolverScoringDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "recluster_solver_scoring_duration_seconds",
			Help:    "Time spent computing node scores",
			Buckets: []float64{0.0001, 0.0005, 0.001, 0.005, 0.01, 0.025, 0.05, 0.1},
		},
	)

	// SolverAssignmentDuration tracks time spent on assignment logic
	SolverAssignmentDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "recluster_solver_assignment_duration_seconds",
			Help:    "Time spent on assignment logic by algorithm",
			Buckets: []float64{0.0001, 0.0005, 0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25},
		},
		[]string{"algorithm"},
	)

	// SolverPodsTotal tracks pod counts
	SolverPodsTotal = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "recluster_solver_pods_total",
			Help: "Number of pods processed by solver",
		},
		[]string{"status"}, // "assigned", "unassigned"
	)

	// SolverNodesTotal tracks node counts
	SolverNodesTotal = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "recluster_solver_nodes_total",
			Help: "Number of nodes considered by solver",
		},
		[]string{"status"}, // "available", "used", "offline_used"
	)

	// SolverFeasiblePairs tracks problem size
	SolverFeasiblePairs = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "recluster_solver_feasible_pairs",
			Help: "Number of feasible pod-node pairs",
		},
	)

	// SolverTotalScore tracks solution quality
	SolverTotalScore = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "recluster_solver_total_score",
			Help: "Total score of assignments",
		},
	)

	// SolverAlgorithmUsed tracks which algorithm was selected
	SolverAlgorithmUsed = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "recluster_solver_algorithm_used_total",
			Help: "Count of solver runs by algorithm",
		},
		[]string{"algorithm"},
	)
)

func init() {
	// Register metrics with controller-runtime
	metrics.Registry.MustRegister(
		SolverDuration,
		SolverScoringDuration,
		SolverAssignmentDuration,
		SolverPodsTotal,
		SolverNodesTotal,
		SolverFeasiblePairs,
		SolverTotalScore,
		SolverAlgorithmUsed,
	)
}

// TimeProbe helps measure execution time of code sections
type TimeProbe struct {
	Name      string
	StartTime time.Time
	EndTime   time.Time
	Duration  time.Duration
}

// NewTimeProbe creates and starts a new time probe
func NewTimeProbe(name string) *TimeProbe {
	return &TimeProbe{
		Name:      name,
		StartTime: time.Now(),
	}
}

// Stop stops the time probe and records the duration
func (p *TimeProbe) Stop() time.Duration {
	p.EndTime = time.Now()
	p.Duration = p.EndTime.Sub(p.StartTime)
	return p.Duration
}

// Timings collects multiple time probes for a solver run
type Timings struct {
	Total      time.Duration
	Scoring    time.Duration
	Sorting    time.Duration
	Assignment time.Duration
	Cleanup    time.Duration
}

// RecordMetrics records the collected timings to Prometheus
func (t *Timings) RecordMetrics(algorithm Algorithm) {
	SolverDuration.WithLabelValues(string(algorithm)).Observe(t.Total.Seconds())
	SolverScoringDuration.Observe(t.Scoring.Seconds())
	SolverAssignmentDuration.WithLabelValues(string(algorithm)).Observe(t.Assignment.Seconds())
}

// RecordSolverMetrics records solver output metrics
func RecordSolverMetrics(input SolverInput, output SolverOutput) {
	// Record pod counts
	SolverPodsTotal.WithLabelValues("assigned").Set(float64(len(output.Assignments)))
	SolverPodsTotal.WithLabelValues("unassigned").Set(float64(len(output.UnassignedPods)))

	// Record node counts
	SolverNodesTotal.WithLabelValues("available").Set(float64(len(input.RcNodes)))
	SolverNodesTotal.WithLabelValues("used").Set(float64(len(output.NodesUsed)))
	SolverNodesTotal.WithLabelValues("offline_used").Set(float64(output.OfflineNodesUsed))

	// Record problem size
	feasiblePairs := CountFeasiblePairs(input.GatedPods, input.RcNodes)
	SolverFeasiblePairs.Set(float64(feasiblePairs))

	// Record solution quality
	SolverTotalScore.Set(output.TotalScore)

	// Record algorithm usage
	SolverAlgorithmUsed.WithLabelValues(string(output.Algorithm)).Inc()
}
