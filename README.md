# Recluster

Energy-aware Kubernetes operator that manages heterogeneous physical machines. Nodes are powered on/off based on workload demand using a policy-driven solver that scores machines by hardware features, power consumption, and boot time.

## How it works

A mutating webhook gates every new pod. A periodic reconciler collects pending pods, runs a solver to find the best node assignments, then releases pods to the scheduler with the right tolerations and node selectors. Nodes are simulated via KWOK until physically woken.

**CRDs:**
- `RcNode` — represents a physical machine (resources, features, wake method)
- `RcPolicy` — scoring policy with weighted multipliers and budget limits

## Quick start

```sh
# Full stack: Kind cluster + CRDs + controller + webhook + monitoring
make full-deploy

# Nuke and redo from scratch
make full-reset
```

After deploy:
- Grafana: http://localhost:3000 (admin/admin)
- Prometheus: http://localhost:9090

## Make targets

```sh
make full-deploy          # One-command deploy (idempotent)
make full-reset           # Delete everything, redeploy from zero
make status               # Show all component status
make kind-logs            # Tail controller logs
make install-samples      # Apply sample RcNodes + RcPolicy
make monitoring-install   # Deploy Prometheus + Grafana
make kind-delete          # Tear down the Kind cluster
make test                 # Run unit tests
```

## Prerequisites

- Go 1.23+
- Docker
- kubectl
- [Kind](https://kind.sigs.k8s.io/)

## License

Apache 2.0

