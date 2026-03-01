# KWOK Provider for Cluster Autoscaler

This directory contains configuration for the [KWOK cloud provider](https://github.com/kubernetes/autoscaler/tree/master/cluster-autoscaler/cloudprovider/kwok) 
used with the Kubernetes Cluster Autoscaler.

## Overview

KWOK (Kubernetes WithOut Kubelet) allows simulating thousands of fake nodes in a cluster. 
Combined with the cluster-autoscaler, this enables testing autoscaling behavior without 
provisioning real infrastructure.

## Files

- `kwok-provider-config.yaml` - KWOK provider configuration (how to group nodes)
- `kwok-provider-templates.yaml` - Node templates for each node group

## Node Groups

The following node groups are configured to match RcNode specs:

| Node Group | Architecture | CPU | Memory | Max Nodes |
|------------|--------------|-----|--------|-----------|
| `x86-old` | amd64 | 15 cores | 28Gi | 10 |
| `arm-dev` | arm64 | 4 cores | 8Gi | 20 |
| `x86-remote` | amd64 | 32 cores | 64Gi | 5 |

## Scale Down Settings

The cluster-autoscaler is configured with 20-second scale down delays for fast testing:

```yaml
--scale-down-delay-after-add=20s
--scale-down-delay-after-delete=20s
--scale-down-delay-after-failure=20s
--scale-down-unneeded-time=20s
--scale-down-unready-time=20s
```

## Usage

```bash
# Install KWOK controller
make kwok-install

# Install cluster-autoscaler with KWOK provider
make autoscaler-install

# Check status
make kwok-status

# View autoscaler logs
make autoscaler-logs

# Full setup (Kind + KWOK + Autoscaler + CRDs)
make kind-full-setup
```

## Testing Autoscaling

1. Create a deployment that requests resources:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: test-workload
spec:
  replicas: 5
  selector:
    matchLabels:
      app: test
  template:
    metadata:
      labels:
        app: test
    spec:
      tolerations:
        - key: "kwok.x-k8s.io/node"
          operator: "Exists"
          effect: "NoSchedule"
      nodeSelector:
        recluster.io/node-group: arm-dev
      containers:
        - name: test
          image: nginx
          resources:
            requests:
              cpu: "1"
              memory: "1Gi"
```

2. Watch the autoscaler create fake nodes:

```bash
kubectl get nodes -w
make autoscaler-logs
```

3. Scale down the deployment and watch nodes get removed after 20s.
