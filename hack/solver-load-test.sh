#!/usr/bin/env bash
# ──────────────────────────────────────────────────────────────────────
# solver-load-test.sh — drive all three solver regimes through the
# periodic reconciler so metrics appear in Prometheus / Grafana.
#
# Phases:
#   1. Setup     – create RcNodes + RcPolicy (if missing)
#   2. Exact     – inject 3 gated pods  (< 20 pairs → exact solver)
#   3. Heuristic – inject 8 gated pods  (20–100 pairs → heuristic)
#   4. Greedy    – inject 20 gated pods (> 100 pairs → greedy)
#   5. Cleanup   – delete test pods
#
# Usage:  ./hack/solver-load-test.sh [--no-cleanup]
# ──────────────────────────────────────────────────────────────────────
set -euo pipefail

GATE_NAME="recluster.io/scheduling-gate"
TEST_NS="solver-test"
WAIT_SECONDS=15  # wait for periodic reconciler (10s interval + margin)

# Colours
RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; CYAN='\033[0;36m'; NC='\033[0m'
info()  { echo -e "${CYAN}▶ $*${NC}"; }
ok()    { echo -e "${GREEN}✔ $*${NC}"; }
warn()  { echo -e "${YELLOW}⚠ $*${NC}"; }

# ── 0. Pre-flight ────────────────────────────────────────────────────
info "Pre-flight: checking cluster connectivity"
kubectl cluster-info --context kind-recluster-dev >/dev/null 2>&1 || {
  echo -e "${RED}✘ Cannot reach kind-recluster-dev cluster. Run 'make full-deploy' first.${NC}"
  exit 1
}
ok "Cluster reachable"

# Create test namespace
kubectl create namespace "$TEST_NS" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
ok "Namespace $TEST_NS ready"

# ── 1. Setup: RcNodes ────────────────────────────────────────────────
info "Phase 1: Ensuring RcNodes exist (need ≥ 6 for greedy regime)"

# We'll create 8 RcNodes with varied capacity to trigger all regimes
for i in $(seq 1 8); do
  CPU=$((4 + (i % 3) * 4))       # 4, 8, 12, 4, 8, 12, 4, 8
  MEM=$((8 * (1 + (i % 3))))     # 8, 16, 24 (Gi)
  CORES=$((2 + i))
  CLOCK=$((2400 + i * 200))
  IDLE=$((20 + i * 5))
  BOOT=$((15 + i * 3))

  cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: recluster.io/v1
kind: RcNode
metadata:
  name: test-node-$(printf "%02d" $i)
  labels:
    recluster.io/node-group: load-test
spec:
  nodeGroup: load-test
  desiredPhase: Offline
  activation:
    wakeMethod: wol
    wolConfig:
      macAddress: "AA:BB:CC:DD:EE:$(printf "%02X" $i)"
      broadcastAddress: "192.168.1.255"
  resources:
    allocatable:
      cpu: "${CPU}"
      memory: "${MEM}Gi"
      pods: "110"
  features:
    - name: compute.cores
      type: integer
      value: "${CORES}"
    - name: compute.threads
      type: integer
      value: "$((CORES * 2))"
    - name: compute.clockmhz
      type: integer
      unit: MHz
      value: "${CLOCK}"
    - name: memory.total
      type: quantity
      value: "${MEM}Gi"
    - name: storage.total
      type: quantity
      value: "500Gi"
    - name: power.idle
      type: integer
      unit: watts
      value: "${IDLE}"
    - name: power.max
      type: integer
      unit: watts
      value: "$((IDLE * 3))"
    - name: boot.timeseconds
      type: integer
      unit: seconds
      value: "${BOOT}"
EOF
done
ok "8 test RcNodes applied"

# Show current state
echo ""
kubectl get rcnodes -l recluster.io/node-group=load-test --no-headers
echo ""

# ── Helper: inject a batch of gated pods ──────────────────────────────
inject_pods() {
  local count=$1
  local batch_label=$2
  local cpu_req=$3
  local mem_req=$4

  info "Injecting $count gated pods (batch=$batch_label, cpu=$cpu_req, mem=$mem_req)"

  for j in $(seq 1 "$count"); do
    cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: v1
kind: Pod
metadata:
  name: ${batch_label}-pod-$(printf "%03d" $j)
  namespace: ${TEST_NS}
  labels:
    app: solver-load-test
    batch: ${batch_label}
spec:
  schedulingGates:
    - name: ${GATE_NAME}
  containers:
    - name: busybox
      image: busybox:1.36
      command: ["sleep", "3600"]
      resources:
        requests:
          cpu: "${cpu_req}"
          memory: "${mem_req}"
        limits:
          cpu: "${cpu_req}"
          memory: "${mem_req}"
  restartPolicy: Never
  terminationGracePeriodSeconds: 0
EOF
  done
  ok "$count pods created"
}

# ── Helper: wait for solver to process and show results ───────────────
wait_and_show() {
  local batch_label=$1
  info "Waiting ${WAIT_SECONDS}s for periodic reconciler to pick up ${batch_label} pods..."
  sleep "$WAIT_SECONDS"

  # Show gated pods remaining
  local gated
  gated=$(kubectl get pods -n "$TEST_NS" -l "batch=$batch_label" -o jsonpath='{range .items[*]}{.metadata.name}{" phase="}{.status.phase}{" gates="}{.spec.schedulingGates[*].name}{"\n"}{end}' 2>/dev/null || true)

  if [ -n "$gated" ]; then
    echo "$gated" | head -5
    local total
    total=$(echo "$gated" | wc -l | tr -d ' ')
    echo "  ... ($total pods total)"
  fi

  # Query Prometheus for latest metrics
  local prom_pod
  prom_pod=$(kubectl get pod -n monitoring -l app=prometheus -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
  if [ -n "$prom_pod" ]; then
    echo ""
    info "Latest solver metrics from Prometheus:"
    for metric in \
      'recluster_solver_algorithm_used_total' \
      'recluster_solver_pods_total' \
      'recluster_solver_nodes_total' \
      'recluster_solver_total_score' \
      'recluster_solver_feasible_pairs'; do
      local val
      val=$(kubectl exec -n monitoring "$prom_pod" -- \
        wget -qO- "http://localhost:9090/api/v1/query?query=${metric}" 2>/dev/null | \
        grep -o '"value":\[[^]]*\]' | head -3 || echo "(no data)")
      echo "  ${metric}: ${val}"
    done
  fi
  echo ""
}

# ── Helper: delete batch ──────────────────────────────────────────────
delete_batch() {
  local batch_label=$1
  info "Cleaning up batch: $batch_label"
  kubectl delete pods -n "$TEST_NS" -l "batch=$batch_label" --grace-period=0 --force 2>/dev/null || true
  ok "Batch $batch_label cleaned"
}

# ── 2. Phase: EXACT solver (3 pods × 8 nodes = 24 pairs → ~20 → exact) ──
echo ""
echo -e "${YELLOW}═══════════════════════════════════════════════════════${NC}"
echo -e "${YELLOW}  Phase 2: EXACT solver  (3 small pods, 8 nodes)      ${NC}"
echo -e "${YELLOW}═══════════════════════════════════════════════════════${NC}"
inject_pods 3 "exact" "500m" "256Mi"
wait_and_show "exact"
delete_batch "exact"
sleep 5

# ── 3. Phase: HEURISTIC solver (8 pods × 8 nodes = 64 pairs → heuristic) ──
echo ""
echo -e "${YELLOW}═══════════════════════════════════════════════════════${NC}"
echo -e "${YELLOW}  Phase 3: HEURISTIC solver  (8 pods, 8 nodes)        ${NC}"
echo -e "${YELLOW}═══════════════════════════════════════════════════════${NC}"
inject_pods 8 "heuristic" "1" "512Mi"
wait_and_show "heuristic"
delete_batch "heuristic"
sleep 5

# ── 4. Phase: GREEDY solver (20 pods × 8 nodes = 160 pairs → greedy) ──
echo ""
echo -e "${YELLOW}═══════════════════════════════════════════════════════${NC}"
echo -e "${YELLOW}  Phase 4: GREEDY solver  (20 pods, 8 nodes)          ${NC}"
echo -e "${YELLOW}═══════════════════════════════════════════════════════${NC}"
inject_pods 20 "greedy" "500m" "256Mi"
wait_and_show "greedy"
delete_batch "greedy"
sleep 5

# ── 5. Phase: Mixed burst ────────────────────────────────────────────
echo ""
echo -e "${YELLOW}═══════════════════════════════════════════════════════${NC}"
echo -e "${YELLOW}  Phase 5: MIXED burst  (3 waves in quick succession) ${NC}"
echo -e "${YELLOW}═══════════════════════════════════════════════════════${NC}"
inject_pods 2 "burst-small" "250m" "128Mi"
sleep 2
inject_pods 6 "burst-mid" "750m" "384Mi"
sleep 2
inject_pods 15 "burst-large" "500m" "256Mi"
info "Waiting ${WAIT_SECONDS}s for all 3 burst waves..."
sleep "$WAIT_SECONDS"
wait_and_show "burst-small"

# ── 6. Cleanup ────────────────────────────────────────────────────────
if [[ "${1:-}" != "--no-cleanup" ]]; then
  echo ""
  info "Final cleanup: removing all test resources"
  delete_batch "burst-small"
  delete_batch "burst-mid"
  delete_batch "burst-large"
  kubectl delete rcnodes -l recluster.io/node-group=load-test 2>/dev/null || true
  kubectl delete namespace "$TEST_NS" --grace-period=0 --force 2>/dev/null || true
  ok "Cleanup complete"
else
  warn "Skipping cleanup (--no-cleanup). Remove manually with:"
  echo "  kubectl delete pods -n $TEST_NS --all --grace-period=0 --force"
  echo "  kubectl delete rcnodes -l recluster.io/node-group=load-test"
  echo "  kubectl delete namespace $TEST_NS"
fi

echo ""
echo -e "${GREEN}═══════════════════════════════════════════════════════${NC}"
echo -e "${GREEN}  Load test complete! Open Grafana to see the results: ${NC}"
echo -e "${GREEN}  http://localhost:30030                               ${NC}"
echo -e "${GREEN}  Dashboard: Recluster → Recluster Solver              ${NC}"
echo -e "${GREEN}═══════════════════════════════════════════════════════${NC}"
