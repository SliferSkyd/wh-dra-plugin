# wh-dra-kubelet-plugin — Team Presentation

> Kubernetes DRA plugin for Tenstorrent Wormhole T3K hardware
> Status: deployed and verified on live cluster (June 2026)

---

## 1. Kubernetes Basics

### What is Kubernetes?

Kubernetes (K8s) is a system for running containerized workloads across a cluster of machines. Instead of manually SSHing into servers to start processes, you declare *what* you want to run, and Kubernetes figures out *where* and *how* to run it.

```
You write:  "I want 3 copies of my model server running, each needing 16 GB RAM"
K8s does:   picks machines with enough RAM, starts containers, restarts on crash
```

### Key concepts

**Node** — a physical or virtual machine in the cluster. Every node runs two system processes:
- `kubelet` — the local agent; talks to the API server, starts/stops containers
- `containerd` — the container runtime; actually creates and runs containers

**Pod** — the smallest unit in Kubernetes. A pod wraps one or more containers that share a network namespace and run on the same node.

**Namespace** — a logical partition inside the cluster. `kube-system` holds system components; your workloads go in their own namespace.

**DaemonSet** — a controller that ensures exactly one pod runs on every (matching) node. Used for node-level agents like our hardware plugin.

**API Server** — the central brain. All components (kubelet, scheduler, kubectl) talk to it. State is stored in `etcd`.

**Scheduler** — watches for Pending pods and decides which node each pod runs on, based on resource requests, node labels, and taints/tolerations.

```
┌──────────────────────────────────────────────────────┐
│  Control Plane                                        │
│                                                       │
│  API Server ←── kubectl (your commands)              │
│      │                                                │
│  Scheduler      Controller Manager      etcd         │
└──────┼───────────────────────────────────────────────┘
       │ (manages)
  ┌────┴────┐
  │  Node   │
  │ kubelet │ ← talks to API server, starts pods
  │containerd│ ← runs containers
  └─────────┘
```

### How a pod starts (simplified)

```
kubectl apply -f my-pod.yaml
  → API server stores the pod spec (status: Pending)
    → Scheduler picks a node (status: Assigned)
      → kubelet on that node pulls the image and starts the container (status: Running)
```

### Node labels

Labels are key-value tags on any Kubernetes object. We use them to mark which nodes have T3K hardware:

```
tenstorrent.com/arch=wormhole
tenstorrent.com/chip-count=4
```

A DaemonSet with `nodeSelector: tenstorrent.com/arch=wormhole` only runs on nodes with that label — so the plugin automatically deploys to T3K nodes and nowhere else.

---

## 2. The Problem

Running AI workloads on Tenstorrent T3K hardware inside Kubernetes has historically required:

- **Manual device management** — operators had to manually configure which node a pod runs on and ensure the hardware wasn't double-allocated
- **Privileged pods everywhere** — pods needed `privileged: true` and explicit `hostPath` volume mounts to access `/dev/tenstorrent/*` and hugepages
- **No health feedback to scheduler** — if a chip stalled, the scheduler had no way to know; new workloads kept getting sent to a broken node
- **No multi-node coordination** — nothing told workers their rank, pod size, or peer addresses automatically

The goal: let Kubernetes schedule T3K workloads **the same way it schedules GPUs** — with automatic device allocation, clean injection, and health-aware scheduling.

---

## 2. The Solution: Kubernetes DRA

**Dynamic Resource Allocation (DRA)** is a Kubernetes feature (GA in v1.35) that allows hardware vendors to write plugins that:

1. **Advertise** devices to the Kubernetes scheduler via `ResourceSlice` objects
2. **Inject** device access into pods at startup via the CDI (Container Device Interface) standard
3. **Reclaim** device access cleanly when pods finish

Think of it as the GPU device plugin model, but more expressive — devices can have rich attributes, multiple pods can share or exclusively hold a device, and the plugin fully controls what gets injected.

**This plugin implements DRA for Tenstorrent Wormhole T3K hardware.**

---

## 3. Cluster Architecture

```
Your Machine (macOS / control host)
  └── kubectl → talks to API server

┌─────────────────────────────────────────┐
│  Control Plane Node  (192.168.1.60)     │
│                                          │
│  kube-apiserver   :6443                 │  ← receives ResourceSlice from plugin
│  kube-scheduler                         │  ← decides which T3K gets a pod
│  kube-controller-manager                │
│  etcd                                   │  ← stores all cluster state
│                                          │
│  No T3K card needed here                │
└─────────────────────────────────────────┘
            │
  ┌─────────┴─────────┐
  │                   │
┌───────────────┐  ┌───────────────┐
│  T3K Node A   │  │  T3K Node B   │   ... more nodes
│               │  │               │
│  kubelet      │  │  kubelet      │
│  containerd   │  │  containerd   │
│               │  │               │
│  plugin pod   │  │  plugin pod   │  ← DaemonSet auto-deploys
│  (DaemonSet)  │  │  (DaemonSet)  │
│               │  │               │
│  workload     │  │  workload     │  ← placed by scheduler
│  pod          │  │  pod          │
└───────────────┘  └───────────────┘
```

**Version requirements:**
| Component | Version | Why |
|---|---|---|
| Kubernetes | 1.35+ | `resource.k8s.io/v1` (DRA GA) first available here |
| Kubespray | v2.31.0 | First to support k8s 1.35 |

---

## 4. What the Plugin Does

The plugin runs as a **DaemonSet pod** on every labeled T3K node and has three responsibilities:

### 4.1 Device Advertising (Scheduling)

At startup, the plugin reads its node's labels and scans `/dev/tenstorrent/` to discover the actual hardware. It then publishes a `ResourceSlice` to the Kubernetes API server:

```
ResourceSlice: t3k-node-a-wormhole.tenstorrent.com
  pool: t3k-node-a
  device: wormhole-t3k
    attributes:
      arch = "wormhole"
      board_type = "n300"
      chip_count = 4
      physical_pod = "t3k-a"
      host_rank = 0
      pod_size = 1
```

The scheduler reads these slices when deciding where to place a pod that requests a T3K device.

### 4.2 Device Injection (CDI)

When the scheduler places a pod on a T3K node:

1. **kubelet calls `PrepareResourceClaims`** on the plugin
2. The plugin writes a **CDI spec file** to `/var/run/cdi/` — a YAML file that tells containerd exactly what to inject
3. **containerd reads the CDI spec** when starting the container and injects:
   - `/dev/tenstorrent/0`, `/dev/tenstorrent/1`, ... (hardware device nodes)
   - `/dev/hugepages-1G` (bind mount — no `hostPath` in pod spec needed)
   - `/tmp/tt_logs` (bind mount for firmware logs)
   - Environment variables: `TT_CHIP_COUNT`, `TT_MESH_HOST_RANK`, `TT_PHYSICAL_POD`, `TT_POD_SIZE`, `WH_RESOURCE_CLAIM_UID`
4. When the pod finishes, kubelet calls `UnprepareResourceClaims` — the plugin deletes the CDI spec

**Two CDI spec types:**

| Spec | Written | Contains | Lifetime |
|---|---|---|---|
| `common` | Plugin startup | Env vars, hugepages mount, logs mount | Node lifetime |
| Per-claim | Per `PrepareResourceClaims` | Device nodes (`/dev/tenstorrent/*`), claim UID env | Pod lifetime |

### 4.3 Health Monitoring

A goroutine runs in the background for the plugin's lifetime:

```
every 30 seconds:
  run tt-smi -s  →  parse JSON output
    for each chip:
      check board_info present  (chip is visible)
      check heartbeat counter is strictly increasing  (chip is not stalled)
  
  if status changed (healthy ↔ unhealthy):
    republish ResourceSlice
      healthy   → full ResourceSlice  (scheduler places new pods here)
      unhealthy → empty ResourceSlice (scheduler stops placing pods here)
```

The health state feeds directly back into scheduling — a stalled chip is invisible to the scheduler within one 30-second tick.

---

## 5. Code Structure

```
cmd/wh-dra-kubelet-plugin/
  main.go        — flags, signal handling, metrics server, wires everything together
  driver.go      — ResourceSlice publication + health → republication callback
  health.go      — tt-smi polling goroutine, heartbeat comparison
  cdi.go         — writes/deletes CDI YAML spec files in /var/run/cdi/
  state.go       — PrepareResourceClaims / UnprepareResourceClaims (kubelet DRA interface)
  manager.go     — reads node labels, walks /dev/tenstorrent/ to discover device nodes
  checkpoint.go  — persists PreparedClaims state to disk for crash recovery

cmd/wh-node-labeler/
  main.go        — runs tt-smi, reads tt-node-topology ConfigMap, patches node labels every 5m

deploy/
  rbac.yaml                  — ServiceAccount + ClusterRole (API permissions for plugin)
  deviceclass.yaml           — DeviceClass: pods select this to request T3K
  node-labeler.yaml          — node labeler DaemonSet + tt-node-topology ConfigMap
  daemonset.yaml             — plugin DaemonSet on tenstorrent.com/arch=wormhole nodes
  test-claim.yaml            — smoke test: device injection (no special image)
  test-two-pods.yaml         — exclusivity test: only one pod holds device at a time
  test-ttnn.yaml             — hardware test: real ttnn.add on silicon
  multinode/                 — 2-node StatefulSet + MPI tests
  odin/                      — InferenceServiceTemplate presets for 1/2/4/8 T3K nodes
```

---

## 6. Full Request Flow

```
User:  kubectl apply -f my-workload.yaml
       (pod spec contains resourceClaims: [{name: my-t3k, resourceClaimName: my-claim}])

1. API server stores ResourceClaim and Pod objects

2. kube-scheduler sees the pod is Pending
   → looks for ResourceSlice matching DeviceClass "t3k.wormhole.tenstorrent.com"
   → finds the slice published by the plugin on t3k-node-a
   → allocates the device, binds pod to t3k-node-a

3. kubelet on t3k-node-a sees the bound pod
   → calls wh-dra-kubelet-plugin.PrepareResourceClaims([my-claim])

4. Plugin PrepareResourceClaims:
   → writes /var/run/cdi/k8s.wormhole.tenstorrent.com-t3k-<claim-uid>.yaml
      (contains /dev/tenstorrent/* device nodes + WH_RESOURCE_CLAIM_UID env)
   → saves state to checkpoint (crash safety)
   → returns CDI device IDs to kubelet

5. kubelet passes CDI IDs to containerd

6. containerd starts container:
   → reads CDI common spec  → injects hugepages, logs mount, TT_* env vars
   → reads CDI claim spec   → injects /dev/tenstorrent/* device nodes
   → container starts with full hardware access, no privileged: true needed

7. Pod runs workload

8. Pod finishes → kubelet calls UnprepareResourceClaims
   → plugin deletes CDI claim spec file
   → removes from checkpoint
   → ResourceClaim status updated: device released
   → next pod can now claim the device
```

---

## 7. What Has Been Done

### Infrastructure
- [x] Kubernetes cluster deployed: Kubespray v2.31.0 + k8s v1.35.0
  - control-plane-01 (192.168.1.60) — Ready
  - t3k-node-a (192.168.1.247) — Ready, labeled
- [x] CDI enabled in containerd on t3k-node-a
- [x] Self-contained container image (`wh-dra-kubelet-plugin:v0.1.0`) — plugin binary + `tt-smi` baked in, no host mounts
- [x] Plugin deployed as DaemonSet
- [x] ResourceSlice published and visible to scheduler

### Plugin Features
- [x] ResourceSlice publication (device advertising)
- [x] CDI-based device injection (`/dev/tenstorrent/*`, env vars, hugepages, logs)
- [x] Health monitoring via `tt-smi` heartbeat check (30s interval)
- [x] Prometheus metrics at `:9090/metrics`
- [x] Crash-recovery checkpoint
- [x] Automatic node labeling (`wh-node-labeler` DaemonSet) — discovers arch/board/chip-count from tt-smi, reads topology from ConfigMap

### Tests Run
- [x] **test-claim** — device injection verified (env vars + `/dev/tenstorrent/` in container)
- [x] **test-two-pods** — DRA exclusivity verified (pod-b blocks until pod-a releases device)
- [ ] **test-ttnn** — hardware test pending (needs `npu-metal-llk:latest` image imported)
- [ ] **multinode** — needs second T3K node

---

## 8. What Is TODO

### Short-term
| Item | Notes |
|---|---|
| Run `test-ttnn.yaml` | Import `npu-metal-llk:latest` into containerd on t3k-node-a, then `kubectl apply` |
| Test auto node labeling with t3k-node-b | Add ConfigMap entry, import image, verify labels applied automatically |
| Fix VM network isolation | Control plane can't reach kubelet port 10250 on worker — `kubectl logs/exec` times out; workaround: `crictl logs` on node directly |
| Add second T3K node | Required for multinode StatefulSet and MPI tests |

### Production hardening
| Item | Notes |
|---|---|
| Set up container registry | Push `wh-dra-kubelet-plugin:v0.1.0` to Harbor/ACR so nodes pull automatically instead of manual `ctr import` |
| CI/CD pipeline | Auto-build and push image on git push; auto-rollout to cluster |
| Deploy Odin InferenceServiceTemplates | `deploy/odin/` presets ready; need MIF operator running |
| MPI Operator setup | Required for `multinode/test-mpi-two-t3k.yaml` |
| Fix `kubectl logs` networking | Investigate hypervisor port group / VLAN isolation between control plane VM and T3K node |

### Nice to have
| Item | Notes |
|---|---|
| Deep hardware telemetry | Export temperature, power, utilization from `tt-smi -s` as Prometheus metrics |
| Fault / error monitoring | Parse tt-kmd dmesg errors; detect silent hardware failures beyond heartbeat stall |
| Graceful drain | On SIGTERM: publish empty ResourceSlice, wait for running workloads to finish |
| Automatic Galaxy topology discovery | Auto-assign `physical-pod`/`host-rank`/`pod-size` via Tenstorrent Ethernet neighbor detection |
| Helm chart | Package all deploy YAMLs for easier versioning |
| Alerting | Wire Prometheus metrics to Alertmanager for health state changes |

---

## 9. Key Design Decisions

**Why DRA instead of the device plugin API?**
The older device plugin API (`nvidia.com/gpu: 1` style) is limited — it can only allocate integer counts and has no way to express rich device attributes or multi-device topologies. DRA lets the plugin publish structured attributes (arch, board type, host rank, pod size) that the scheduler can use with CEL selector expressions.

**Why CDI instead of direct device injection?**
CDI is a standard (adopted by containerd, cri-o, and podman) that decouples device setup from the runtime. The plugin writes a YAML file; containerd reads it. This means the plugin doesn't need to hook into containerd internals — just write files to `/var/run/cdi/`.

**Why two CDI specs (common + per-claim)?**
The common spec (env vars, hugepages) is node-level — same for every pod on this node. The per-claim spec (device nodes) is pod-level — different UID per pod, cleaned up when the pod finishes. Separating them avoids rewriting the same env vars on every PrepareResourceClaims call.

**Why heartbeat instead of a simpler `tt-smi` alive check?**
A chip can be visible and responding to `tt-smi` but have its firmware frozen. The heartbeat counter in telemetry increments every few seconds in firmware. If it stops incrementing across two consecutive checks, the chip is truly stalled — even if it still "responds."

---

## 10. Live Demo Commands

```bash
# Cluster health
kubectl get nodes
kubectl get resourceslices

# Plugin status
kubectl -n kube-system get pods -l app=wh-dra-kubelet-plugin

# Run smoke test
kubectl apply -f deploy/test-claim.yaml
kubectl get pods -w
# (on t3k-node-a) sudo crictl logs $(sudo crictl ps -a | grep wh-demo | awk '{print $1}')
kubectl delete -f deploy/test-claim.yaml

# Run exclusivity test
kubectl apply -f deploy/test-two-pods.yaml
kubectl get pods -w
# pod-a runs, pod-b is Pending until pod-a finishes
kubectl delete -f deploy/test-two-pods.yaml
```
