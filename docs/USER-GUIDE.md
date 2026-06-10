# User Guide — Running Workloads on T3K Kubernetes

> For teammates who want to launch pods / jobs on the cluster.

---

## 1. Prerequisites

### Network access
You must be able to reach the control plane at `192.168.1.60:6443`.
If you are not on the same network, connect to the VPN first.

### Install kubectl
```bash
# macOS
brew install kubectl

# Linux
curl -LO "https://dl.k8s.io/release/$(curl -Ls https://dl.k8s.io/release/stable.txt)/bin/linux/amd64/kubectl"
chmod +x kubectl && sudo mv kubectl /usr/local/bin/
```

---

## 2. Get cluster access (kubeconfig)

Ask the cluster admin to generate a scoped kubeconfig for you:

```bash
# Admin runs this once (sets up namespace + RBAC):
kubectl apply -f deploy/user-rbac.yaml

# Then generates your personal kubeconfig:
./deploy/scripts/gen-kubeconfig.sh <your-name>
# → produces kubeconfig-<your-name>.yaml and sends it to you
```

Place it on your machine:

```bash
mkdir -p ~/.kube
cp kubeconfig-<your-name>.yaml ~/.kube/config
```

This gives you access scoped to the `t3k-workloads` namespace — you can create pods, jobs, and resource claims, but cannot modify cluster-level config.

> **Admin shortcut (trusted team only):** share the raw admin kubeconfig instead:
> ```bash
> scp ubuntu@control-plane-01:/etc/kubernetes/admin.conf ~/t3k-kubeconfig.yaml
> cp ~/t3k-kubeconfig.yaml ~/.kube/config
> ```
> This grants full cluster-admin access — use only for a small trusted team.

Verify access:
```bash
kubectl get nodes
# NAME               STATUS   ROLES
# control-plane-01   Ready    control-plane
# t3k-node-a         Ready    worker
# t3k-node-b         Ready    worker
```

---

## 3. Check available T3K devices

```bash
# See what T3K hardware is available to the scheduler
kubectl get resourceslices

# See node labels (arch, chip count, rank)
kubectl get nodes -L tenstorrent.com/arch,tenstorrent.com/chip-count,tenstorrent.com/physical-pod,tenstorrent.com/host-rank
```

---

## 4. Launch a workload on a single T3K node

Every pod that needs a T3K device must:
1. Create a `ResourceClaim` (or use a `ResourceClaimTemplate`)
2. Reference the claim in the pod spec under `resourceClaims`
3. Reference it again inside the container under `resources.claims`

### Minimal example — single pod

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaim
metadata:
  name: my-t3k-claim
spec:
  devices:
    requests:
    - name: t3k
      exactly:
        deviceClassName: t3k.wormhole.tenstorrent.com
---
apiVersion: v1
kind: Pod
metadata:
  name: my-t3k-pod
spec:
  resourceClaims:
  - name: t3k
    resourceClaimName: my-t3k-claim
  containers:
  - name: worker
    image: ubuntu:22.04
    command: ["/bin/bash", "-c"]
    args:
    - |
      echo "Devices: $(ls /dev/tenstorrent/)"
      env | grep TT_
      # your workload here
      sleep infinity
    resources:
      claims:
      - name: t3k
```

Apply and check:
```bash
kubectl apply -f my-pod.yaml
kubectl get pods -o wide          # shows which node it landed on
kubectl get resourceclaims        # shows allocation status
```

What gets injected automatically (no config needed in the pod spec):
```
/dev/tenstorrent/0-3    ← T3K chip devices
TT_CHIP_COUNT=4
TT_MESH_HOST_RANK=0     ← 0 for node-a, 1 for node-b
TT_PHYSICAL_POD=t3k-a
TT_POD_SIZE=1
/dev/hugepages-1G       ← required by Tenstorrent runtime
/tmp/tt_logs            ← firmware log directory
```

---

## 5. Launch a batch job

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata:
  name: my-job-template
spec:
  spec:
    devices:
      requests:
      - name: t3k
        exactly:
          deviceClassName: t3k.wormhole.tenstorrent.com
---
apiVersion: batch/v1
kind: Job
metadata:
  name: my-t3k-job
spec:
  template:
    spec:
      restartPolicy: Never
      resourceClaims:
      - name: t3k
        resourceClaimTemplateName: my-job-template
      containers:
      - name: worker
        image: ubuntu:22.04
        command: ["/bin/bash", "-c"]
        args: ["echo done && sleep 5"]
        resources:
          claims:
          - name: t3k
```

---

## 6. Launch across two T3K nodes (multi-node)

Use a StatefulSet so each pod gets a stable hostname and its own device claim:

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata:
  name: my-multinode-template
spec:
  spec:
    devices:
      requests:
      - name: t3k
        exactly:
          deviceClassName: t3k.wormhole.tenstorrent.com
---
apiVersion: v1
kind: Service
metadata:
  name: my-workers-headless
spec:
  clusterIP: None
  selector:
    app: my-worker
  ports:
  - port: 22
    name: ssh
---
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: my-worker
spec:
  serviceName: my-workers-headless
  replicas: 2
  selector:
    matchLabels:
      app: my-worker
  template:
    metadata:
      labels:
        app: my-worker
    spec:
      hostNetwork: true
      resourceClaims:
      - name: t3k
        resourceClaimTemplateName: my-multinode-template
      containers:
      - name: worker
        image: ubuntu:22.04
        command: ["/bin/bash", "-c"]
        args:
        - |
          echo "I am $(hostname), rank=$TT_MESH_HOST_RANK"
          echo "My peer: $([ $TT_MESH_HOST_RANK -eq 0 ] && echo my-worker-1.my-workers-headless || echo my-worker-0.my-workers-headless)"
          sleep infinity
        resources:
          claims:
          - name: t3k
```

Each pod lands on a different T3K node and gets:
- `TT_MESH_HOST_RANK=0` (node-a) or `TT_MESH_HOST_RANK=1` (node-b)
- Stable DNS: `my-worker-0.my-workers-headless`, `my-worker-1.my-workers-headless`

---

## 7. Useful commands

```bash
# See your pods and which node they're on
kubectl get pods -o wide

# See device allocation status
kubectl get resourceclaims

# Check available T3K resources
kubectl get resourceslices

# See pod logs (if network allows)
kubectl logs <pod-name>

# Exec into a running pod
kubectl exec -it <pod-name> -- bash

# Delete everything from a manifest
kubectl delete -f my-workload.yaml

# Watch pods in real time
kubectl get pods -w
```

---

## 8. Constraints to know

| Constraint | Detail |
|---|---|
| **One workload per node at a time** | DRA allocates devices exclusively — if a pod is running on t3k-node-a, no other pod can get t3k-node-a's device until it finishes |
| **2 T3K nodes available** | t3k-node-a (192.168.1.247) and t3k-node-b (192.168.1.243) |
| **4 chips per node** | `/dev/tenstorrent/0-3` on each node |
| **`kubectl logs` may time out** | Known network issue — use `crictl logs` on the node directly as workaround |
| **No registry yet** | Custom images must be imported manually via `ctr import` — ask the admin |
