# Cluster Setup Guide

How to set up a Kubernetes cluster for Tenstorrent T3K hardware and deploy the DRA plugin.

---

## Cluster architecture

```
┌─────────────────────────────────┐
│  Control plane node (no NPU)    │
│                                 │
│  kube-apiserver                 │  ← kubectl talks to this
│  kube-scheduler                 │  ← decides which T3K gets a pod
│  kube-controller-manager        │
│  etcd                           │  ← stores all cluster state
└─────────────────────────────────┘
            │ network
  ┌─────────┴─────────┐
  │                   │
┌─────────────┐  ┌─────────────┐
│ T3K node A  │  │ T3K node B  │   ...more T3K nodes
│             │  │             │
│ kubelet     │  │ kubelet     │  ← managed by control plane
│ containerd  │  │ containerd  │
│             │  │             │
│ plugin pod  │  │ plugin pod  │  ← DaemonSet auto-deploys here
│ (DaemonSet) │  │ (DaemonSet) │
│             │  │             │
│ workload    │  │ workload    │  ← scheduled by kube-scheduler
│ pod         │  │ pod         │
└─────────────┘  └─────────────┘
```

The control plane does **not** need a T3K card — it only runs Kubernetes system processes.
The T3K nodes are pure workers: they run kubelet, containerd, the plugin DaemonSet, and workload pods.

---

## Where each YAML is applied

`kubectl apply` is run **once** from anywhere (your laptop, the control plane, CI).
Objects live in the API server. The DaemonSet then automatically deploys the plugin to every labeled T3K node.

| File | Applied | Lives in | Auto-deployed to T3K nodes? |
|---|---|---|---|
| `rbac.yaml` | once to cluster | API server | — |
| `deviceclass.yaml` | once to cluster | API server | — |
| `daemonset.yaml` | once to cluster | API server | **yes** — one plugin pod per T3K node |
| `odin/*.yaml` | once to cluster | API server | — |
| `test-ttnn.yaml` | on demand | API server | **yes** — scheduler places it on a T3K node |

---

## Step-by-step setup

### Step 1 — Set up the control plane

Install k3s on a plain Linux server (no NPU required):

```bash
curl -sfL https://get.k3s.io | sh -
```

Verify:

```bash
kubectl get nodes
# NAME           STATUS   ROLES                  AGE
# control-plane  Ready    control-plane,master   1m
```

Get the join token for worker nodes:

```bash
cat /var/lib/rancher/k3s/server/node-token
```

---

### Step 2 — Join each T3K node as a worker

Run on each T3K node:

```bash
curl -sfL https://get.k3s.io | \
  K3S_URL=https://<control-plane-ip>:6443 \
  K3S_TOKEN=<token-from-step-1> \
  sh -
```

Verify from the control plane:

```bash
kubectl get nodes
# NAME           STATUS   ROLES                  AGE
# control-plane  Ready    control-plane,master   5m
# t3k-node-a     Ready    <none>                 1m
# t3k-node-b     Ready    <none>                 30s
```

---

### Step 3 — Enable CDI in containerd (each T3K node)

CDI must be enabled so containerd can read the plugin's device spec files:

```bash
# On each T3K node:
sudo mkdir -p /etc/containerd
sudo tee -a /etc/containerd/config.toml <<'EOF'
[plugins."io.containerd.grpc.v1.cri"]
  enable_cdi = true
  cdi_spec_dirs = ["/var/run/cdi", "/etc/cdi"]
EOF

sudo systemctl restart containerd
```

---

### Step 4 — Verify hugepages on each T3K node

The Wormhole firmware requires 1 GiB hugepages for remote chip access:

```bash
# Check allocation (must be > 0):
cat /sys/kernel/mm/hugepages/hugepages-1048576kB/nr_hugepages

# Check mount point exists:
ls /dev/hugepages-1G

# If not allocated, add to /etc/default/grub:
# GRUB_CMDLINE_LINUX="hugepagesz=1G hugepages=4"
# Then: sudo update-grub && sudo reboot
```

---

### Step 5 — Label each T3K node

Run from the control plane:

```bash
kubectl label node <t3k-node-name> \
  tenstorrent.com/arch=wormhole \
  tenstorrent.com/board-type=n300 \
  tenstorrent.com/chip-count=4 \
  tenstorrent.com/physical-pod=t3k-a \
  tenstorrent.com/host-rank=0 \
  tenstorrent.com/pod-size=1

# For Odin / MoAI:
kubectl label node <t3k-node-name> \
  moai.moreh.io/accelerator.vendor=tenstorrent \
  moai.moreh.io/accelerator.model=wormhole
```

Repeat for each T3K node, incrementing `physical-pod` and `host-rank` as appropriate.

---

### Step 6 — Build the plugin binary (on each T3K node or CI)

```bash
cd /home/ubuntu/wh-dra-plugin

export PATH=$PATH:/home/ubuntu/go/bin
export GOPATH=/home/ubuntu/gopath

go build -o bin/wh-dra-kubelet-plugin ./cmd/wh-dra-kubelet-plugin
```

The `daemonset.yaml` mounts `/home/ubuntu/wh-dra-plugin/bin` from the host, so the binary must exist at that path on each T3K node before the DaemonSet pod starts.

---

### Step 7 — Apply cluster-level resources

Run once from the control plane (or any machine with `kubectl` access):

```bash
kubectl apply -f deploy/rbac.yaml
kubectl apply -f deploy/deviceclass.yaml
kubectl apply -f deploy/daemonset.yaml
```

The plugin DaemonSet automatically starts on every node labeled `tenstorrent.com/arch=wormhole` — existing nodes and any future ones added to the cluster.

Verify the plugin is running:

```bash
kubectl -n kube-system get pods -l app=wh-dra-kubelet-plugin
kubectl -n kube-system logs -l app=wh-dra-kubelet-plugin

# Verify device is visible to the scheduler:
kubectl get resourceslices
```

---

### Step 8 — Run a hardware test

```bash
# Import the workload image into k3s containerd on each T3K node:
docker save npu-metal-llk:latest | sudo k3s ctr images import -

# Apply from the control plane:
kubectl apply -f deploy/test-ttnn.yaml
kubectl logs -f wh-ttnn-test
kubectl delete -f deploy/test-ttnn.yaml
```

---

## Current dev setup vs production

| | Dev (what we tested) | Production |
|---|---|---|
| Control plane | k3s running on the T3K node itself | Separate VM or server, no NPU |
| Workers | Same machine | Dedicated T3K nodes |
| Plugin binary | Mounted from host filesystem | Built into container image |
| `tt-smi` | Mounted from host conda environment | Bundled static binary in image |
| Cluster size | 1 node | Many nodes |

The plugin code and all YAML files are identical in both setups — only the cluster topology changes.

---

## Adding more T3K nodes later

No changes to any YAML files needed. For each new node:

1. Join it as a k3s worker (Step 2)
2. Enable CDI in containerd (Step 3)
3. Verify hugepages (Step 4)
4. Label the node (Step 5)
5. Copy the plugin binary to `/home/ubuntu/wh-dra-plugin/bin/` on the new node

The DaemonSet sees the new label and automatically starts a plugin pod on the node within seconds.
