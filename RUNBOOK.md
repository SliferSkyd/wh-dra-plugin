# wh-dra-kubelet-plugin — Operations Runbook

Practical guide for building, deploying, testing, and monitoring the Wormhole DRA plugin.
For architecture details see [README.md](README.md).

---

## What We Built

A **Kubernetes DRA (Dynamic Resource Allocation) plugin** for Tenstorrent Wormhole T3K hardware.
It runs as a DaemonSet on every T3K node and does three things:

| Feature | How |
|---|---|
| **Device scheduling** | Publishes a `ResourceSlice` so the scheduler can allocate T3K devices to pods |
| **Device injection** | On pod start, writes a CDI spec that injects `/dev/tenstorrent/*`, env vars, and `/dev/hugepages-1G` into the container — no `privileged: true` needed |
| **Health monitoring** | Polls `tt-smi -s` every 30 s; if any chip's heartbeat stalls the `ResourceSlice` is emptied so the scheduler stops placing new workloads on this node |

```
Node (Wormhole T3K host)
┌──────────────────────────────────────────────────────────┐
│  wh-dra-kubelet-plugin (DaemonSet in kube-system)        │
│                                                           │
│  1. PublishResourceSlice()  →  scheduler sees device     │
│  2. PrepareResourceClaims() →  writes CDI spec per pod   │
│  3. Health goroutine        →  empties slice if unhealthy │
│  4. Prometheus :9090/metrics                             │
│                                                           │
│  /var/run/cdi/                                           │
│    common spec  → hugepages-1G bind, env vars, logs dir  │
│    claim spec   → /dev/tenstorrent/* device nodes        │
└──────────────────────────────────────────────────────────┘
```

### Key files

```
cmd/wh-dra-kubelet-plugin/
  main.go       flags, signal handling, metrics server
  driver.go     ResourceSlice publication, health-triggered republication
  health.go     periodic tt-smi heartbeat checker
  cdi.go        writes CDI YAML spec files to /var/run/cdi/
  state.go      PrepareResourceClaims / UnprepareResourceClaims
  manager.go    reads node labels, walks /dev/tenstorrent/
  checkpoint.go crash-recovery state

deploy/
  rbac.yaml           ServiceAccount, ClusterRole, ClusterRoleBinding
  deviceclass.yaml    DeviceClass for wormhole.tenstorrent.com
  daemonset.yaml      DaemonSet (nodeSelector: tenstorrent.com/arch=wormhole)
  test-ttnn.yaml      hardware test pod (ttnn.add on real silicon)
  odin/               Odin InferenceServiceTemplate presets (1/2/4/8 node)
```

---

## Prerequisites

| Requirement | Check |
|---|---|
| Kubernetes ≥ 1.33 (DRA stable) | `kubectl version` |
| `tt-kmd` kernel driver loaded | `ls /dev/tenstorrent/` |
| 1 GiB hugepages allocated | `cat /sys/kernel/mm/hugepages/hugepages-1048576kB/nr_hugepages` (> 0) |
| `/dev/hugepages-1G` exists | `ls /dev/hugepages-1G` |
| containerd CDI enabled | `grep enable_cdi /etc/containerd/config.toml` → `true` |
| `/tmp/tt_logs` exists on host | `mkdir -p /tmp/tt_logs` |
| Go ≥ 1.21 (build only) | `go version` |

---

## Step 1 — Label each Wormhole node

Run once per node. The DaemonSet `nodeSelector` requires `tenstorrent.com/arch=wormhole`.

```bash
kubectl label node <node-name> \
  tenstorrent.com/arch=wormhole \
  tenstorrent.com/board-type=n300 \
  tenstorrent.com/chip-count=4 \
  tenstorrent.com/physical-pod=t3k-a \
  tenstorrent.com/host-rank=0 \
  tenstorrent.com/pod-size=1
```

For Odin / MoAI also add:

```bash
kubectl label node <node-name> \
  moai.moreh.io/accelerator.vendor=tenstorrent \
  moai.moreh.io/accelerator.model=wormhole
```

Or use the helper script:

```bash
bash deploy/odin/node-labels.sh <node-name>
```

---

## Step 2 — Build the plugin binary

```bash
cd /home/ubuntu/wh-dra-plugin

export PATH=$PATH:/home/ubuntu/go/bin
export GOPATH=/home/ubuntu/gopath

go build -o bin/wh-dra-kubelet-plugin ./cmd/wh-dra-kubelet-plugin
# → bin/wh-dra-kubelet-plugin
```

---

## Step 3 — Apply cluster-level resources (once per cluster)

```bash
# RBAC: ServiceAccount + ClusterRole for the plugin
kubectl apply -f deploy/rbac.yaml

# DeviceClass: tells scheduler which driver owns T3K devices
kubectl apply -f deploy/deviceclass.yaml
```

Verify:

```bash
kubectl get clusterrole wh-dra-plugin
kubectl get deviceclass t3k.wormhole.tenstorrent.com
```

---

## Step 4 — Deploy the DaemonSet

```bash
kubectl apply -f deploy/daemonset.yaml
```

Watch it come up:

```bash
kubectl -n kube-system get pods -l app=wh-dra-kubelet-plugin -w
```

Follow logs:

```bash
kubectl -n kube-system logs -l app=wh-dra-kubelet-plugin --follow
```

Expected startup lines:

```
node tt-mv-n2-vm4: arch=wormhole board=n300 chips=4 pod=t3k-a rank=0
published ResourceSlice: device wormhole-t3k with 4 chips on pool tt-mv-n2-vm4
health monitoring started (interval=30s tt-smi=... chips=4)
wh-dra-kubelet-plugin running on node tt-mv-n2-vm4
metrics on :9090/metrics
```

Verify the device is visible to the scheduler:

```bash
kubectl get resourceslices
kubectl describe resourceslice <name>
```

---

## Step 5 — Run the hardware test pod

Runs a real `ttnn.add` operation on the Wormhole silicon inside a DRA-allocated pod.

**One-time setup** — import the workload image into k3s containerd:

```bash
docker save npu-metal-llk:latest | sudo k3s ctr images import -
sudo k3s ctr images list | grep npu-metal-llk
```

**Run the test:**

```bash
kubectl apply -f deploy/test-ttnn.yaml

# Watch it schedule
kubectl get pods -w

# Follow output
kubectl logs -f wh-ttnn-test
```

Expected output:

```
=== DRA-injected devices ===
crw-rw-rw- 1 root root 236, 0 /dev/tenstorrent/0
crw-rw-rw- 1 root root 236, 1 /dev/tenstorrent/1
crw-rw-rw- 1 root root 236, 2 /dev/tenstorrent/2
crw-rw-rw- 1 root root 236, 3 /dev/tenstorrent/3

=== DRA-injected env vars ===
TT_CHIP_COUNT=4
TT_MESH_HOST_RANK=0
TT_PHYSICAL_POD=t3k-a
TT_POD_SIZE=1
WH_RESOURCE_CLAIM_UID=<uid>

=== Running ttnn hardware test ===
Opening Wormhole device 0 via ttnn...
ttnn.add([1,2,3], [4,5,6]) = [5.0, 7.0, 9.0]
Device closed.
SUCCESS: Wormhole hardware verified via DRA-allocated pod
```

Clean up:

```bash
kubectl delete -f deploy/test-ttnn.yaml
```

---

## Step 6 — Verify hugepages injection

Hugepages are injected by the CDI common spec — no `hostPath` volume in the pod spec needed.

```bash
# While the test pod is running:
kubectl exec -it wh-ttnn-test -- ls /dev/hugepages-1G
```

Should list hugepage files. The pod spec has zero `volumes` or `volumeMounts` entries for hugepages.

---

## Step 7 — Monitor health checks

### View health log lines

```bash
kubectl -n kube-system logs -l app=wh-dra-kubelet-plugin | grep -i health
```

Normal (no flip — at default verbosity only status changes are logged):

```
health monitoring started (interval=30s tt-smi=... chips=4)
```

On failure:

```
T3K health changed: healthy=false — chip 2: heartbeat stalled (prev=41234 cur=41234)
published empty ResourceSlice on pool tt-mv-n2-vm4 (T3K unhealthy)
```

On recovery:

```
T3K health changed: healthy=true — all 4 chips healthy
published ResourceSlice: device wormhole-t3k with 4 chips on pool tt-mv-n2-vm4
```

### Enable verbose per-tick logging (debug)

```bash
kubectl -n kube-system patch daemonset wh-dra-kubelet-plugin \
  --type=json \
  -p='[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"-v=4"}]'

# Follow — you will see a line every 30 s:
# health check: healthy=true all 4 chips healthy
kubectl -n kube-system logs -l app=wh-dra-kubelet-plugin -f | grep 'health check'

# Remove -v=4 when done (rollout restart resets args):
kubectl -n kube-system rollout restart daemonset/wh-dra-kubelet-plugin
```

### View Prometheus metrics

```bash
# Forward the in-pod port to localhost
kubectl -n kube-system port-forward \
  $(kubectl -n kube-system get pod -l app=wh-dra-kubelet-plugin -o name | head -1) \
  9090:9090

# In another terminal:
curl -s localhost:9090/metrics | grep -v '^#'
```

---

## Daily command reference

```bash
# --- Build & deploy ---
go build -o bin/wh-dra-kubelet-plugin ./cmd/wh-dra-kubelet-plugin
kubectl rollout restart daemonset/wh-dra-kubelet-plugin -n kube-system

# --- Status ---
kubectl -n kube-system get pod -l app=wh-dra-kubelet-plugin
kubectl -n kube-system logs -l app=wh-dra-kubelet-plugin -f

# --- Device visibility ---
kubectl get resourceslices
kubectl describe resourceslice <name>

# --- Hardware test ---
kubectl apply -f deploy/test-ttnn.yaml
kubectl logs -f wh-ttnn-test
kubectl delete -f deploy/test-ttnn.yaml

# --- Metrics ---
kubectl -n kube-system port-forward \
  $(kubectl -n kube-system get pod -l app=wh-dra-kubelet-plugin -o name | head -1) \
  9090:9090
curl -s localhost:9090/metrics

# --- Board reset (if firmware is stuck) ---
/home/ubuntu/miniconda3/envs/moreh/bin/tt-smi -r all
```

---

## Odin / InferenceService deployment

Apply once per cluster (or per namespace as needed):

```bash
# ResourceClaimTemplate — apply to every workload namespace
kubectl apply -n mif -f deploy/odin/resourceclaimtemplate.yaml

# Preset InferenceServiceTemplates (hardware config only — image comes from runtime-base)
kubectl apply -n mif -f deploy/odin/inferenceservicetemplate-1node.yaml
kubectl apply -n mif -f deploy/odin/inferenceservicetemplate-2node.yaml
kubectl apply -n mif -f deploy/odin/inferenceservicetemplate-4node.yaml
kubectl apply -n mif -f deploy/odin/inferenceservicetemplate-8node.yaml
```

Reference in an InferenceService:

```yaml
spec:
  templateRefs:
    - name: vllm-wormhole-runtime     # image + entrypoint (from NPU team Seoul)
    - name: vllm-wormhole-1node       # hardware config (this plugin)
```

Template sizes:

| Template | Nodes | Chips | Parallelism |
|---|---|---|---|
| `vllm-wormhole-1node` | 1 | 8 | Deployment (data=1) |
| `vllm-wormhole-2node` | 2 | 16 | LWS (data=2) |
| `vllm-wormhole-4node` | 4 | 32 | LWS (data=4) |
| `vllm-wormhole-8node` | 8 | 64 | LWS (data=8) |

Hugepages are injected automatically via CDI. No `hostPath` volumes or `privileged: true` needed in the InferenceService spec.

---

## Health monitor lifecycle

The health goroutine runs for the lifetime of the plugin process — it does **not** need to be manually stopped.

```
Node boots
  → kubelet starts DaemonSet pod
    → plugin starts, health goroutine starts
      → polls tt-smi every 30 s ... indefinitely
        → node drains / kubectl delete pod / rollout restart
          → SIGTERM sent to plugin process
            → context cancelled (signal.NotifyContext in main.go)
              → goroutine exits cleanly via <-ctx.Done()
```

If the plugin pod crashes, Kubernetes restarts it and the goroutine starts fresh with heartbeat state reset.

---

## Troubleshooting

### Pod stuck in `ContainerCreating` with `FailedPrepareDynamicResources`

Caused by UID mismatch in kubelet's persisted DRA state (usually after force-deleting a pod).

```bash
sudo rm -f /var/lib/kubelet/plugins/wormhole.tenstorrent.com/claim_info_state
sudo systemctl restart k3s
sudo chmod 644 /etc/rancher/k3s/k3s.yaml
```

Prevention: use `ResourceClaimTemplate` inside a `Job` — each pod gets a unique auto-named claim so the same name is never reused.

### Plugin logs `tt-smi: no such file or directory`

`tt-smi` is a Python script (not a native binary). The DaemonSet must mount the conda environment and source directories from the host. Verify the three mounts exist in `deploy/daemonset.yaml`:

```
miniconda   → /home/ubuntu/miniconda3
tt-smi-src  → /home/ubuntu/tt-smi
local-pylib → /home/ubuntu/.local
```

### ResourceSlice is empty / scheduler not placing pods

```bash
kubectl describe resourceslice <name>
```

If `Slices` is empty, the health check failed. Check logs:

```bash
kubectl -n kube-system logs -l app=wh-dra-kubelet-plugin | grep -i health
```

If the hardware is actually healthy, reset the board and wait for the next health tick:

```bash
/home/ubuntu/miniconda3/envs/moreh/bin/tt-smi -r all
# health goroutine recovers automatically within --health-check-interval (default 30s)
```

### Node tainted `disk-pressure:NoSchedule`

Free space in Docker / containerd:

```bash
docker volume prune -f
docker builder prune -f
docker image prune -f
```

The taint clears automatically once kubelet sees disk above its eviction threshold.
