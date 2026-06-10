# Slide Outline — wh-dra-kubelet-plugin

> Paste into Google Slides / PowerPoint.
> Each `---` is a slide boundary. Speaker notes follow each slide.

---

## Slide 1 — Title

**wh-dra-kubelet-plugin**
Kubernetes Dynamic Resource Allocation for Tenstorrent T3K Hardware

*[Your name] · [Date]*

> **Speaker notes:** Short intro — this is about making T3K hardware a first-class citizen in Kubernetes, the same way GPUs are in large cloud clusters.

---

## Slide 2 — What is Kubernetes? (1/2)

**The problem Kubernetes solves:**

> "I have 10 machines and 50 containers to run. Who starts what, where?"

- **Node** — a machine (VM or bare metal) in the cluster
- **Pod** — one or more containers that run together on a node
- **kubelet** — the agent on each node; starts/stops pods on behalf of the cluster
- **containerd** — the runtime that actually creates containers

*Diagram:*
```
Control Plane
  ├── API Server   ← all commands go here
  ├── Scheduler    ← decides which node runs each pod
  └── etcd         ← stores all state

Node A          Node B
  kubelet         kubelet
  containerd      containerd
  [pod] [pod]     [pod]
```

> **Speaker notes:** K8s is essentially an operating system for a cluster. You tell it what you want; it figures out the how. kubelet is the local agent — it watches the API server and starts containers. containerd is the actual container runtime underneath kubelet.

---

## Slide 3 — What is Kubernetes? (2/2)

**Key abstractions for this project:**

| Object | What it is |
|---|---|
| **DaemonSet** | Runs exactly one pod on every matching node — auto-deploys our plugin |
| **Node Labels** | Key-value tags marking which nodes have T3K hardware |
| **Namespace** | Logical partition; `kube-system` = system components |
| **Scheduler** | Places pods based on resource requests + node labels |

**How a pod starts:**
```
kubectl apply →  API server stores spec (Pending)
               → Scheduler picks a node (Assigned)
               → kubelet starts container (Running)
```

> **Speaker notes:** The DaemonSet is the key object for our plugin. Because T3K nodes are labeled `tenstorrent.com/arch=wormhole`, the DaemonSet automatically runs one plugin pod on every T3K node — no manual deployment per machine.

---

## Slide 4 — The Problem We Are Solving

**Before this plugin, running workloads on T3K in Kubernetes was painful:**

- **Manual placement** — had to pin pods to specific nodes by hand
- **Double allocation risk** — no mechanism to prevent two pods accessing the same hardware simultaneously
- **Privileged containers** — every workload needed `privileged: true` and explicit device mounts in its pod spec
- **No health awareness** — if a chip stalled, the scheduler kept sending workloads to the broken node
- **No automatic environment setup** — rank, pod size, peer addresses had to be set manually per workload

**Goal:** T3K should work like a GPU — declare "I need a T3K", get one, hardware injected automatically.

> **Speaker notes:** The "privileged container" problem is a security concern too — any process in a privileged container effectively has root on the host. DRA lets us give fine-grained device access without full privilege.

---

## Slide 5 — What is DRA?

**Dynamic Resource Allocation (DRA)** — a Kubernetes feature (GA in v1.35) for custom hardware

Three standard objects:

| Object | Who creates it | What it does |
|---|---|---|
| `ResourceSlice` | **Plugin** (us) | Advertises available devices to the scheduler |
| `DeviceClass` | Admin (once) | Defines a "type" of device pods can request |
| `ResourceClaim` | Workload user | "I need one T3K device" |

**Flow:**
```
Plugin publishes ResourceSlice  →  Scheduler sees available T3K devices
User creates pod + ResourceClaim →  Scheduler matches claim to slice
Pod starts  →  Plugin injects device into container via CDI
Pod ends    →  Plugin cleans up, device available again
```

> **Speaker notes:** DRA replaced the older "device plugin" API. The older API could only count devices (e.g., "2 GPUs"), with no attributes or topology info. DRA lets us publish rich metadata — arch, chip count, host rank, pod size — that the scheduler and the workload can use.

---

## Slide 6 — Plugin Architecture

```
T3K Node
┌──────────────────────────────────────────────────────┐
│  wh-dra-kubelet-plugin (DaemonSet pod, kube-system)  │
│                                                       │
│  ① WHManager        reads node labels + /dev/        │
│  ② driver.go        publishes ResourceSlice           │
│  ③ CDIHandler       writes device spec files         │
│  ④ state.go         PrepareResourceClaims callback   │
│  ⑤ healthChecker    polls tt-smi every 30s           │
│  ⑥ metrics server   Prometheus :9090/metrics         │
│                                                       │
│  /var/run/cdi/                                       │
│    common.yaml    → hugepages, env vars (node-level) │
│    <claim>.yaml   → /dev/tenstorrent/* (pod-level)   │
└──────────────────────────────────────────────────────┘
         ↑ socket                    ↑ reads spec
      kubelet              containerd
```

> **Speaker notes:** The plugin runs as a privileged DaemonSet pod — privileged because it needs to read /dev/tenstorrent and write CDI files. Workload pods themselves do NOT need to be privileged — that's the whole point.

---

## Slide 7 — Full Request Flow

```
① User: kubectl apply -f workload.yaml
         (pod requests ResourceClaim for T3K)

② API server stores Pod (Pending) + ResourceClaim

③ Scheduler:
   - finds ResourceSlice from plugin on t3k-node-a
   - allocates device, binds pod to t3k-node-a

④ kubelet on t3k-node-a:
   - calls plugin PrepareResourceClaims(claim)

⑤ Plugin writes CDI spec to /var/run/cdi/<claim-uid>.yaml
   (contains /dev/tenstorrent/0..3 device nodes)

⑥ containerd starts container:
   - reads CDI common spec  → injects hugepages, TT_* env vars
   - reads CDI claim spec   → injects /dev/tenstorrent/* devices

⑦ Container runs — sees hardware, correct env, hugepages

⑧ Pod finishes → kubelet calls UnprepareResourceClaims
   → plugin deletes CDI spec
   → device released for next workload
```

> **Speaker notes:** Step ⑤ and ⑥ are the key insight. The plugin doesn't inject into the container directly — it writes a YAML file (the CDI spec) and containerd reads it. CDI is a standard supported by all major runtimes.

---

## Slide 8 — CDI: What Gets Injected

**Two spec files per node:**

**common spec** (written once at startup, lasts as long as the plugin runs):
```yaml
env:
  - TT_CHIP_COUNT=4
  - TT_MESH_HOST_RANK=0
  - TT_POD_SIZE=1
  - TT_PHYSICAL_POD=t3k-a
mounts:
  - /dev/hugepages-1G  →  /dev/hugepages-1G
  - /tmp/tt_logs       →  /tmp/tt_logs
```

**per-claim spec** (written per pod, deleted when pod finishes):
```yaml
deviceNodes:
  - /dev/tenstorrent/0  (major=236, minor=0)
  - /dev/tenstorrent/1  (major=236, minor=1)
  - /dev/tenstorrent/2  (major=236, minor=2)
  - /dev/tenstorrent/3  (major=236, minor=3)
env:
  - WH_RESOURCE_CLAIM_UID=<uid>
```

**Result in the container — zero pod spec config needed:**
```bash
$ ls /dev/tenstorrent/    # hardware present
$ echo $TT_CHIP_COUNT     # 4
$ ls /dev/hugepages-1G    # hugepages present
```

> **Speaker notes:** This is why workload pod specs are clean — no hostPath volumes, no device mounts, no env vars. All of that lives in the CDI spec files managed by the plugin. The workload author just requests "one T3K" and everything arrives.

---

## Slide 9 — Health Monitoring

**Problem:** a chip can be missing or have a crashed driver. The scheduler has no way to know.

**Solution:** lightweight device-file check every 30 seconds.

```
every 30s:
  for each chip 0..N-1:
    os.Open("/dev/tenstorrent/<i>")
      success → chip present, driver loaded
      error   → chip missing or driver crashed

  if status CHANGED:
    healthy   → publish full ResourceSlice  (scheduler places pods here again)
    unhealthy → publish EMPTY ResourceSlice (scheduler stops placing pods here)
```

**Log output:**
```
# On failure (always visible):
T3K health changed: healthy=false — chip 2 not accessible: no such file or directory
published empty ResourceSlice on pool t3k-node-a (T3K unhealthy)

# On recovery:
T3K health changed: healthy=true — all 4 chips accessible
published ResourceSlice: device wormhole-t3k with 4 chips on pool t3k-node-a
```

> **Speaker notes:** We originally used `tt-smi -s` (Python) for health checks, but it hangs indefinitely when the chip ethernet mesh is in an inconsistent state — e.g. after one node reboots while the other is still up. `os.Open` is the same approach Google's TPU plugin uses: if the kernel driver file is accessible, the device is present. Zero subprocess overhead, cannot hang.

---

## Slide 10 — Cluster Setup

**What we deployed:**

| Component | Version |
|---|---|
| Kubernetes | v1.35.0 |
| Kubespray (provisioner) | v2.31.0 |
| Container runtime | containerd |

**Nodes:**
| Node | IP | Role |
|---|---|---|
| control-plane-01 | 192.168.1.60 | API server, scheduler, etcd |
| t3k-node-a | 192.168.1.247 | T3K worker (4 chips, n300, host-rank=0) |
| t3k-node-b | 192.168.1.243 | T3K worker (4 chips, n300, host-rank=1) |

**Deployment tool:** Kubespray runs as a Docker container on the macOS laptop — no need to install Ansible locally. Adding t3k-node-b used `scale.yml --limit t3k-node-b`.

> **Speaker notes:** K8s 1.35 is required because DRA v1 (`resource.k8s.io/v1`) was only GA'd in 1.35. Earlier versions (1.32, 1.33) only have `v1beta1` which is incompatible with our plugin code.

---

## Slide 11 — What Has Been Done

**Infrastructure**
- Kubernetes cluster: v1.35.0, 3 nodes (control-plane + 2× T3K)
- CDI enabled in containerd on both T3K nodes
- Self-contained image (`wh-dra-kubelet-plugin:v0.1.0`) — plugin + tt-smi, no host dependencies
- Plugin + labeler DaemonSets running on both nodes

**Plugin features implemented**
- ResourceSlice publication (device advertising to scheduler)
- CDI-based device injection (devices, hugepages, env vars, logs dir)
- Lightweight health monitoring via `os.Open /dev/tenstorrent/N`
- Prometheus metrics endpoint
- Crash-recovery checkpoint
- **Automatic node labeling** (`wh-node-labeler` DaemonSet) — no more manual `kubectl label`

**Tests passed**
| Test | Result | What it proves |
|---|---|---|
| `test-claim` | ✅ PASS | Device injection works end-to-end |
| `test-two-pods` | ✅ PASS | DRA exclusivity — only one pod holds device at a time |
| **multinode StatefulSet** | ✅ PASS | Two pods, two nodes, correct rank/devices injected |
| `test-ttnn` | ⏳ pending | Real silicon via ttnn (needs image import) |

---

## Slide 12 — What Is Next (TODO)

**Short-term**

| Task | Notes |
|---|---|
| Run `test-ttnn` hardware test | Import `npu-metal-llk:latest` into containerd on both nodes |
| Re-enable health monitoring | Hardware stabilised; set `--health-check-interval=30s` in DaemonSet |
| Fix `board-type=unknown` label | Code fix done; needs image rebuild + redeploy |
| MPI Operator + multinode training | Run `multinode/test-mpi-two-t3k.yaml` across t3k-node-a and t3k-node-b |

**Production hardening**

| Task | Why |
|---|---|
| Set up container registry (Harbor/ACR) | Nodes currently need manual `ctr import` after each build |
| CI/CD pipeline | Auto-build + push + rollout on git push |
| Fix `kubectl logs` networking | Port 10250 not routable from control plane; use `crictl logs` as workaround |
| Deploy Odin InferenceServiceTemplates | YAML presets ready for 1/2/4/8 T3K configs |

**Future: integrate tt-fabric-manager**

| What | Why |
|---|---|
| Replace `tt-smi` with TTFM gRPC `GetTopology()` | UMD-based, never hangs, richer attributes |
| Auto-detect T3K topology from `ExitNodeInfo` | Eliminate manual `tt-node-topology` ConfigMap |
| Contribute multi-node scheduling to `tt-dra-driver` | Upstream our physical-pod/host-rank model |

> **Speaker notes:** The `kubectl logs` timeout is a known infrastructure issue — the control plane VM and the T3K nodes are on different network segments and port 10250 (kubelet) is not routable between them. It doesn't affect workloads — only log streaming from the laptop. Workaround: `crictl logs` directly on the node.

---

## Slide 13 — Demo (live)

```bash
# Show cluster is up — both T3K nodes + ResourceSlices
kubectl get nodes
kubectl get resourceslices

# Show plugin + labeler running on both nodes
kubectl -n kube-system get pods -l app=wh-dra-kubelet-plugin -o wide
kubectl -n kube-system get pods -l app=wh-node-labeler -o wide

# Run device injection test (single node)
kubectl apply -f deploy/test-claim.yaml
kubectl get pods -w
# [on t3k-node-a] sudo crictl logs <container-id>
kubectl delete -f deploy/test-claim.yaml

# Run multinode StatefulSet — one pod per T3K node
kubectl apply -f deploy/multinode/test-statefulset-two-t3k.yaml
kubectl get pods -l app=wh-t3k-worker -o wide
# → wh-t3k-worker-0  Running  t3k-node-a
# → wh-t3k-worker-1  Running  t3k-node-b

# Show what each worker sees (devices + env vars injected by CDI)
# [on t3k-node-a] sudo crictl logs $(sudo crictl ps | grep wh-t3k-worker-0 | awk '{print $1}')
# Output: TT_MESH_HOST_RANK=0, /dev/tenstorrent/0-3
# [on t3k-node-b] sudo crictl logs $(sudo crictl ps | grep wh-t3k-worker-1 | awk '{print $1}')
# Output: TT_MESH_HOST_RANK=1, /dev/tenstorrent/0-3
```

---

## Slide 14 — Q&A

**Key takeaways:**
1. T3K devices are now schedulable Kubernetes resources — no manual placement
2. Workload pods need zero hardware-specific config — devices arrive via CDI
3. Health monitoring closes the feedback loop between hardware state and the scheduler
4. The cluster is running and tests pass — ready for real workloads

*Thank you*
