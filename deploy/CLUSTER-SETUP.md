# Cluster Setup Guide

How to set up a Kubernetes cluster for Tenstorrent T3K hardware and deploy the DRA plugin.

> **Verified working:** Kubespray v2.31.0 + Kubernetes v1.35.0 (macOS deployment host, June 2026)

---

## Cluster architecture

```
Deployment Host (macOS laptop, VPN access)
  └── Kubespray in Docker  →  provisions cluster
  └── kubectl              →  applies K8s manifests

┌─────────────────────────────────┐
│  Control plane node (no NPU)    │
│                                 │
│  kube-apiserver  :6443          │  ← kubectl talks to this
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
The T3K nodes are pure workers: kubelet, containerd, the plugin DaemonSet, and workload pods.

---

## Version requirements

| Component | Version | Why |
|---|---|---|
| Kubernetes | **1.35+** | `resource.k8s.io/v1` (DRA GA) first available in 1.35 — earlier versions only have `v1beta1` which is incompatible with this plugin |
| Kubespray | **v2.31.0** | First version with k8s 1.35 support |
| Docker image | `quay.io/kubespray/kubespray:v2.31.0` | Must match git checkout version exactly |

---

## Where each YAML is applied

`kubectl apply` is run **once** from the deployment host.
Objects live in the API server. The DaemonSet then automatically deploys the plugin to every labeled T3K node.

| File | Applied | Lives in | Auto-deployed to T3K nodes? |
|---|---|---|---|
| `rbac.yaml` | once to cluster | API server | — |
| `deviceclass.yaml` | once to cluster | API server | — |
| `daemonset.yaml` | once to cluster | API server | **yes** — one plugin pod per T3K node |
| `odin/*.yaml` | once to cluster | API server | — |
| `test-ttnn.yaml` | on demand | API server | **yes** — scheduler places it on a T3K node |

---

## Step 1 — Provision the cluster with Kubespray

Run from your macOS laptop. SSH access to all nodes required (via VPN if remote).

**Install dependencies:**

```bash
# Install Homebrew if not already installed
/bin/bash -c "$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)"

# Install Git and kubectl
brew install git kubectl

# Install Docker Desktop (provides the docker command used to run Kubespray)
brew install --cask docker
# Open Docker.app from Applications and wait for the whale icon to stop animating
```

**Set up SSH key** (needed so Kubespray can SSH into the nodes):

```bash
# Generate key if you don't have one:
ssh-keygen -t rsa -b 4096   # press Enter for all prompts

# Copy to each node (will ask for password once):
ssh-copy-id ubuntu@192.168.1.60    # control plane
ssh-copy-id ubuntu@192.168.1.247   # t3k node

# Test (should not ask for password):
ssh ubuntu@192.168.1.60 hostname
```

**Clone Kubespray:**

```bash
cd ~
git clone https://github.com/kubernetes-sigs/kubespray.git
cd kubespray
git checkout v2.31.0   # must match the Docker image version below
```

**Configure containerd registry mirrors** in `inventory/sample/group_vars/all/containerd.yml`:

```yaml
containerd_registries_mirrors:
  - prefix: docker.io
    mirrors:
      - host: https://registry-1.docker.io
        capabilities: ["pull", "resolve"]
        skip_verify: false
```

**Create inventory:**

```bash
cp -rfp inventory/sample inventory/mycluster
```

Edit `inventory/mycluster/hosts.yaml` to list your nodes.
Use the real hostnames (what `hostname` returns on each machine) to avoid confusion with `kubectl get nodes`:

```yaml
all:
  hosts:
    # Control plane node
    control-plane-01:
      ansible_host: 192.168.1.60
      ansible_user: ubuntu
      ansible_ssh_private_key_file: ~/.ssh/id_rsa
      ansible_become_pass: <sudo-password>

    # T3K worker nodes
    t3k-node-a:
      ansible_host: 192.168.1.247
      ansible_user: ubuntu
      ansible_ssh_private_key_file: ~/.ssh/id_rsa
      ansible_become_pass: <sudo-password>

  children:
    kube_control_plane:
      hosts:
        control-plane-01:

    kube_node:
      hosts:
        t3k-node-a:

    etcd:
      hosts:
        control-plane-01:

    k8s_cluster:
      children:
        kube_control_plane:
        kube_node:
```

Key sections:
- `kube_control_plane` — runs the API server, scheduler, controller-manager
- `kube_node` — runs kubelet and hosts workload pods (T3K machines go here)
- `etcd` — same as `kube_control_plane` for a single-master setup

**If any T3K node previously ran k3s** — uninstall it first or port 10250 will conflict:

```bash
ssh ubuntu@192.168.1.247
sudo systemctl stop k3s && sudo systemctl disable k3s
sudo /usr/local/bin/k3s-uninstall.sh
exit
```

**Run the installer** (Docker mounts the kubespray directory and your SSH key into the container):

```bash
docker run --rm -it \
  --mount type=bind,source="$(pwd)",dst=/kubespray \
  --mount type=bind,source="$HOME/.ssh",dst=/root/.ssh,readonly \
  quay.io/kubespray/kubespray:v2.31.0 bash

# Inside the container:
ansible-playbook -i inventory/mycluster/hosts.yaml \
  --become --become-user=root \
  -e kube_version=1.35.0 \
  cluster.yml
```

> **Note:** `kube_version` must have **no `v` prefix** — use `1.35.0` not `v1.35.0`.
> The git checkout and Docker image tag must match exactly.

**Export kubeconfig** (SSH into the master node after cluster.yml finishes):

```bash
ssh ubuntu@192.168.1.60
sudo cp -f /etc/kubernetes/admin.conf ~/.kube/config
sudo chown $(id -u):$(id -g) ~/.kube/config
exit
```

Copy to your Mac and fix the server address (Kubespray sets `127.0.0.1` by default):

```bash
scp ubuntu@192.168.1.60:~/.kube/config ~/.kube/config
sed -i '' 's|server: https://127.0.0.1:6443|server: https://192.168.1.60:6443|' ~/.kube/config

kubectl get nodes
# NAME               STATUS   ROLES           AGE   VERSION
# control-plane-01   Ready    control-plane   5m    v1.35.0
# t3k-node-a         Ready    <none>          4m    v1.35.0
```

---

## Step 2 — Enable CDI in containerd (each T3K node)

CDI must be enabled so containerd can read the plugin's device spec files:

```bash
# On each T3K node:
sudo tee -a /etc/containerd/config.toml <<'EOF'
[plugins."io.containerd.grpc.v1.cri"]
  enable_cdi = true
  cdi_spec_dirs = ["/var/run/cdi", "/etc/cdi"]
EOF

sudo systemctl restart containerd
```

---

## Step 3 — Verify hugepages on each T3K node

The Wormhole firmware requires 1 GiB hugepages for remote chip access:

```bash
# Check allocation (must be > 0):
cat /sys/kernel/mm/hugepages/hugepages-1048576kB/nr_hugepages

# Check mount point:
ls /dev/hugepages-1G
```

If not allocated, add to the kernel command line and reboot:

```bash
# In /etc/default/grub:
GRUB_CMDLINE_LINUX="hugepagesz=1G hugepages=4"

sudo update-grub && sudo reboot
```

---

## Step 4 — Configure node topology (one-time per node)

The `wh-node-labeler` DaemonSet automatically labels T3K nodes with hardware info
(`arch`, `board-type`, `chip-count`) discovered from `tt-smi`. The three topology labels
(`physical-pod`, `host-rank`, `pod-size`) cannot be auto-detected — they describe your
physical rack layout and must be set once in a ConfigMap.

Edit `deploy/node-labeler.yaml` and add an entry for each T3K node under the
`tt-node-topology` ConfigMap data section:

```yaml
data:
  t3k-node-a: "physical-pod=t3k-a host-rank=0 pod-size=1"
  t3k-node-b: "physical-pod=t3k-a host-rank=1 pod-size=2"  # add new nodes here
```

| Field | Meaning |
|---|---|
| `physical-pod` | Name of the logical Galaxy pod this server belongs to |
| `host-rank` | 0-based position of this server within the pod |
| `pod-size` | How many servers form this logical pod |

If you leave a node out of the ConfigMap, the labeler uses safe defaults (`physical-pod=<nodename>`, `host-rank=0`, `pod-size=1`).

---

## Step 5 — Container image (pulled automatically from ghcr.io)

The plugin image is built and pushed by GitHub Actions on every push to `main`:

```
ghcr.io/slifersky/wh-dra-plugin:latest
ghcr.io/slifersky/wh-dra-plugin:sha-<commit>
```

Both DaemonSets use `imagePullPolicy: Always`, so nodes pull the latest image automatically when the pod restarts. No manual `docker build` or `ctr images import` is needed.

**If ghcr.io is private** — create an image pull secret on each node once:

```bash
kubectl create secret docker-registry ghcr-credentials \
  --docker-server=ghcr.io \
  --docker-username=<github-username> \
  --docker-password=<github-PAT-with-read:packages> \
  -n kube-system
```

Then set `imagePullSecrets: [{name: ghcr-credentials}]` in `helm/wh-dra-plugin/values.yaml`.

---

## Step 6 — Apply cluster-level resources

Run once from the deployment host:

```bash
kubectl apply -f deploy/rbac.yaml
kubectl apply -f deploy/deviceclass.yaml
kubectl apply -f deploy/node-labeler.yaml   # node labeler + tt-node-topology ConfigMap
kubectl apply -f deploy/daemonset.yaml
```

The node labeler DaemonSet runs on every worker node. On non-T3K nodes it detects no
`/dev/tenstorrent` devices and sleeps permanently. On T3K nodes it labels the node
within seconds — then the plugin DaemonSet starts automatically.

Verify:

```bash
# Node labeler running on each node
kubectl -n kube-system get pods -l app=wh-node-labeler

# Labels applied to T3K nodes
kubectl get node t3k-node-a --show-labels | tr ',' '\n' | grep tenstorrent

# Plugin pod running on each T3K node (starts after labels are applied)
kubectl -n kube-system get pods -l app=wh-dra-kubelet-plugin

# Plugin logs — look for "published ResourceSlice" and "health monitoring started"
kubectl -n kube-system logs -l app=wh-dra-kubelet-plugin

# Device visible to the scheduler
kubectl get resourceslices
```

---

## Step 7 — Run the hardware test

```bash
# Import the workload image into containerd on each T3K node:
docker save npu-metal-llk:latest | sudo ctr -n k8s.io images import -

# Apply from the deployment host:
kubectl apply -f deploy/test-ttnn.yaml
kubectl logs -f wh-ttnn-test
kubectl delete -f deploy/test-ttnn.yaml
```

Expected last lines:

```
ttnn.add([1,2,3], [4,5,6]) = [5.0, 7.0, 9.0]
SUCCESS: Wormhole hardware verified via DRA-allocated pod
```

---

## Adding more T3K nodes later

For each new node:

1. Add to `inventory/mycluster/hosts.yaml` under `kube_node`
2. Copy SSH key: `ssh-copy-id ubuntu@<new-node-ip>`
3. Run `scale.yml` (not `cluster.yml`) inside the Kubespray container:
   ```bash
   ansible-playbook -i inventory/mycluster/hosts.yaml \
     --become --become-user=root \
     -e kube_version=1.35.0 \
     scale.yml
   ```
4. Enable CDI in containerd (Step 2)
5. Verify hugepages (Step 3)
6. Add the node's topology entry to the `tt-node-topology` ConfigMap:
   ```bash
   kubectl edit configmap tt-node-topology -n kube-system
   # Add: t3k-node-b: "physical-pod=t3k-a host-rank=1 pod-size=1"
   ```
The node labeler DaemonSet detects the new node's hardware and applies labels automatically.
The plugin DaemonSet then starts on the newly labeled node within seconds — no manifest changes needed.

---

## Quick reference

```bash
# Plugin status
kubectl -n kube-system get pods -l app=wh-dra-kubelet-plugin
kubectl -n kube-system logs -l app=wh-dra-kubelet-plugin -f

# Node labeler status
kubectl -n kube-system get pods -l app=wh-node-labeler
kubectl -n kube-system logs -l app=wh-node-labeler

# Check node labels
kubectl get node t3k-node-a --show-labels | tr ',' '\n' | grep -E 'tenstorrent|moai'

# Device availability
kubectl get resourceslices

# Redeploy after a code change:
# 1. git push origin main  →  GitHub Actions builds + pushes to ghcr.io
# 2. Then trigger a rolling restart to pull the new image:
kubectl rollout restart daemonset/wh-dra-kubelet-plugin -n kube-system
kubectl rollout restart daemonset/wh-node-labeler -n kube-system

# Plugin metrics
kubectl -n kube-system port-forward \
  $(kubectl -n kube-system get pod -l app=wh-dra-kubelet-plugin -o name | head -1) \
  9090:9090
curl -s localhost:9090/metrics

# Driver reload (if chips become inaccessible — run on the T3K node)
sudo modprobe -r tenstorrent && sudo modprobe tenstorrent
```

---

## Troubleshooting

### `Port 10250 is in use` during cluster.yml

Kubelet is already running from a previous k3s or Kubernetes install. Uninstall it first:

```bash
ssh ubuntu@<node-ip>
# For k3s:
sudo systemctl stop k3s && sudo /usr/local/bin/k3s-uninstall.sh
# For a previous kubeadm install:
sudo kubeadm reset -f
sudo systemctl stop kubelet
sudo rm -rf /etc/kubernetes /var/lib/kubelet
```

### `Missing sudo password`

Add `ansible_become_pass: <password>` to each host in `hosts.yaml`, or run the playbook with `--ask-become-pass`.

### `Version comparison failed` / `'dict object' has no attribute '1.33'`

The git checkout version doesn't match the Docker image version. Both must be the same:

```bash
git checkout v2.31.0
# and use: quay.io/kubespray/kubespray:v2.31.0
```

### `All version strings have been normalized to not use a leading 'v'`

Remove the `v` prefix from `kube_version`: use `1.35.0` not `v1.35.0`.

### `no matches for kind "DeviceClass" in version "resource.k8s.io/v1"`

The cluster is running a Kubernetes version older than 1.35. This plugin requires `resource.k8s.io/v1` which is only available in k8s 1.35+. Reset and reinstall:

```bash
# Inside Kubespray container:
ansible-playbook -i inventory/mycluster/hosts.yaml --become --become-user=root reset.yml
ansible-playbook -i inventory/mycluster/hosts.yaml --become --become-user=root -e kube_version=1.35.0 cluster.yml
```

### `x509: certificate signed by unknown authority` after cluster reinstall

The kubeconfig on the Mac has stale certificates from the previous cluster. Re-fetch:

```bash
ssh ubuntu@192.168.1.60 "sudo cp -f /etc/kubernetes/admin.conf ~/.kube/config && sudo chown ubuntu:ubuntu ~/.kube/config"
scp ubuntu@192.168.1.60:~/.kube/config ~/.kube/config
sed -i '' 's|server: https://127.0.0.1:6443|server: https://192.168.1.60:6443|' ~/.kube/config
```

### DaemonSet pod not appearing after `kubectl apply`

The node is missing the required label. Check that the node labeler is running and has
applied labels:

```bash
kubectl -n kube-system get pods -l app=wh-node-labeler
kubectl -n kube-system logs -l app=wh-node-labeler
kubectl get node t3k-node-a --show-labels | grep tenstorrent
```

If the labeler pod is not running (e.g. image not imported), import the image first (Step 5),
then apply `deploy/node-labeler.yaml`. Labels are applied within seconds of the labeler pod
starting.

### Pods stuck `Terminating` during `reset.yml`

Calico can't clean up networking because the API server was already stopped. These errors are expected and Kubespray ignores them (`...ignoring`). Let the reset continue.

### `kubectl get nodes` — `connection refused` to `127.0.0.1:6443`

Kubespray sets `127.0.0.1` in the kubeconfig. Fix the server address:

```bash
sed -i '' 's|server: https://127.0.0.1:6443|server: https://192.168.1.60:6443|' ~/.kube/config
```
