# Architecture — wh-dra-plugin on T3K Kubernetes

How the pieces fit together: which components run on which hosts, how they
communicate, and the end-to-end flow that gets a workload onto Tenstorrent
chips.

**Mental model:** a node advertises what hardware it has → the scheduler
matches a pod's request to a node that has it → that node's kubelet asks the
plugin to wire those chips into the container. Everything below is that one
idea, expanded.

---

## 1. Components and where they run

| Component | Namespace | Host | Role |
|---|---|---|---|
| kube-apiserver + etcd | `kube-system` | control-plane-01 | source of truth for ResourceSlice / ResourceClaim / DeviceClass / node labels |
| kube-scheduler (DRA structured parameters) | `kube-system` | control-plane-01 | matches claims to nodes; pure metadata, never touches a chip |
| **fm controller** (Deployment) | `ttfm` | control-plane-01 | aggregates topology from all agents; serves web UI + JSON API on `:8080`; receives agent heartbeats on `:50051/52` |
| **wh-dra-kubelet-plugin** (DaemonSet) | `kube-system` | every T3K node | enumerates devices, publishes ResourceSlice, injects devices via CDI |
| **wh-node-labeler** (DaemonSet) | `kube-system` | every T3K node | runs `tt-smi`, writes node labels consumed by the scheduler / DeviceClass |
| **fm agent** (DaemonSet) | `ttfm` | every T3K node | UMD ethernet-mesh discovery; reports topology to controller; exposes `AgentService` gRPC on `:50053` |
| tt-kmd + `/dev/tenstorrent` + hugepages | — | every T3K node | kernel/driver layer the chips are reached through |
| workload pod (vLLM / tt-metal) | workload ns | T3K node (scheduled) | gets `/dev/tenstorrent/N` + hugepages injected via CDI |
| `wh-device-probe` (binary in plugin image) | — | exec into plugin pod | busy-safe chip enumeration from `/dev` + `/sys` |
| `wh-topology-export` (binary in plugin image) | — | exec into plugin pod | calls `AgentService.GetTopology` via K8s service, dumps full topology |

Nothing hardware-facing runs on the control plane. The kubelet plugin and FM
agent are both pinned to T3K nodes by
`nodeAffinity: tenstorrent.com/arch=wormhole`. The FM controller is the only
component that runs on the control plane, and it never touches a chip directly.

---

## 2. Topology diagram

```
┌─────────────────────────────────────────────────────────────────────────────┐
│  control-plane-01  (192.168.1.60)            no Tenstorrent hardware         │
│                                                                               │
│  ┌──────────────────┐  ┌─────────────────┐  ┌──────────────────────────────┐ │
│  │  kube-apiserver  │  │  kube-scheduler │  │  fm-controller  [ttfm ns]   │ │
│  │  + etcd          │  │  DRA structured │  │                              │ │
│  │                  │  │  parameters     │  │  • aggregates agent reports  │ │
│  │  ResourceSlice   │◄─┤                 │  │  • cluster-wide topology     │ │
│  │  ResourceClaim   │  │  claim ↔ slice  │  │  • web UI :8080              │ │
│  │  DeviceClass     │  │  allocate+bind  │  │  • gRPC :50051/52            │ │
│  │  Node labels     │  └─────────────────┘  └──────────────▲───────────────┘ │
│  └──────▲───────────┘                                       │ register +      │
│         │                                                    │ heartbeat       │
│         │  GHA self-hosted runner                           │ (K8s svc)       │
│         │  helm upgrade → rolls DaemonSets                  │                 │
└─────────┼────────────────────────────────────────────────────┼────────────────┘
          │  HTTPS :6443                                        │ TCP :50051/52
══════════╪════════════════════ same K8s cluster ══════════════╪════════════════
          │                                                     │
┌─────────┼─────────────────────────────────────────────────────┼──────────────┐
│  t3k-node-a  (label: tenstorrent.com/arch=wormhole)            │  ×N nodes    │
│         │                                                     │              │
│  ┌──────┴──────────────────────────────────────────────────┐  │              │
│  │  kubelet                                                 │  │              │
│  └──────▲──────────────▲────────────────────────────────────┘  │              │
│         │ register     │ NodePrepareResources / CDI             │              │
│         │ DRA plugin   │                                        │              │
│  ┌──────┴──────────────┴───────┐  ┌──────────────────────┐     │              │
│  │  wh-dra-kubelet-plugin      │  │  wh-node-labeler     │     │              │
│  │  [kube-system]              │  │  [kube-system]       │     │              │
│  │  • enumerate /dev + /sys    │  │  • tt-smi → labels   │─────┼──► apiserver │
│  │  • publish ResourceSlice    │  │    arch, board, rank  │     │   HTTPS      │
│  │  • NodePrepare: CDI inject  │  └──────────────────────┘     │              │
│  │    /dev/tenstorrent/N                                        │              │
│  │    + hugepages              │  ┌──────────────────────┐     │              │
│  │                             │  │  fm-agent  [ttfm]    │─────┼──► controller│
│  │  wh-device-probe (tool)     │  │  • UMD mesh discovery│     │   via K8s    │
│  │  • reads /dev + /sys        │  │  • AgentService gRPC │     │   service    │
│  │  • busy-safe, no FM needed  │  │    :50053 (pod IP)   │     │   TCP        │
│  │                             │  └──────────▲───────────┘     │              │
│  │  wh-topology-export (tool)  │             │ UMD mmap BAR0   │              │
│  │  • dials ttfm-agent svc     │─────────────┘ (fails busy)    │              │
│  │    :50053 (local node only) │                                │              │
│  └─────────────────────────────┘                                │              │
│                                                                  │              │
│  ┌──────────────────────────────────────────────────────────┐   │              │
│  │  KERNEL / DRIVER LAYER                                    │   │              │
│  │  tt-kmd → /dev/tenstorrent/{0..N}, by-id/, /sys/class    │   │              │
│  │  hugepages (1G)                                           │   │              │
│  └──────────────────────▲───────────────────────────────────┘   │              │
│                          │ char-dev injection at container start  │              │
│  ┌───────────────────────┴───────────────────────────────────┐   │              │
│  │  WORKLOAD POD  (vLLM / tt-metal)                          │   │              │
│  │  • /dev/tenstorrent/N + /dev/hugepages-1G injected by CDI │   │              │
│  │  • TT_MESH_HOST_RANK, TT_CHIP_COUNT, etc. from labels     │   │              │
│  │  • chip↔chip over TT ethernet mesh                        │   │              │
│  └───────────────────────────────────────────────────────────┘   │              │
└──────────────────────────────────────────────────────────────────────────────┘
```

For a 2-node deployment both `t3k-node-a` and `t3k-node-b` run identical
stacks. The scheduler places one workload pod per node based on ResourceClaim
availability. Cross-node chip communication happens over the TT ethernet mesh;
host-to-host MPI uses `TT_ETHERNET_IFACE`.

---

## 3. How components communicate

| Edge | Mechanism | Transport / endpoint | Auth |
|---|---|---|---|
| plugin ↔ kubelet (register) | kubelet plugin-registration gRPC | **UDS** `/var/lib/kubelet/plugins_registry/` | filesystem perms |
| kubelet → plugin (allocate) | DRA node API: `NodePrepareResources` / `NodeUnprepareResources` | **UDS** `/var/lib/kubelet/plugins/wormhole.tenstorrent.com/` | filesystem perms |
| plugin → apiserver | kube REST + watch (publish `ResourceSlice`) | **HTTPS** :6443 | ServiceAccount + RBAC |
| node-labeler → apiserver | kube REST PATCH (node labels) | **HTTPS** :6443 | ServiceAccount + RBAC |
| scheduler ↔ apiserver | watch / list / bind | **HTTPS** :6443 | TLS + RBAC |
| plugin → CDI | writes CDI spec JSON files | filesystem `/var/run/cdi` | — |
| kubelet → containerd | CRI gRPC; runtime reads CDI, injects device nodes | **UDS** (CRI socket) | filesystem perms |
| FM agent → FM controller | gRPC register + heartbeat stream | **TCP** via K8s service `ttfm-controller.ttfm:50052` | plaintext |
| wh-topology-export → FM agent | gRPC `AgentService.GetTopology` | **TCP** `ttfm-agent.ttfm.svc.cluster.local:50053` (`internalTrafficPolicy: Local` → routes to same-node agent only) | plaintext |
| plugin / wh-device-probe → kernel | `stat()` + sysfs reads | syscalls (no network) | root in container |
| UMD / tt-smi / FM agent → chips | `mmap` BAR0 + ioctl | char dev `/dev/tenstorrent/N` | device perms |
| workload pod → chips | UMD mmap of injected char devs + hugepages | `/dev/tenstorrent/N`, `/dev/hugepages-1G` | CDI-injected |
| chip ↔ chip (multi-node) | TT fabric over ethernet | Ethernet (`TT_ETHERNET_IFACE`) | — |

**Key design points:**

- **Inside a node, almost nothing is networked.** plugin↔kubelet and
  kubelet↔runtime are Unix domain sockets; the device hand-off is files (CDI
  JSON) + char-device injection — not RPC.
- **FM agent does not use hostNetwork.** It binds `:50053` on its pod IP, not
  the host IP. `localhost:50053` from inside another pod or from the host
  itself will not reach it — always use the K8s service name.
- **`internalTrafficPolicy: Local`** on the `ttfm-agent` service ensures each
  caller is routed to the agent on its own node. A pod on `t3k-node-a` always
  hits the agent on `t3k-node-a`, never `t3k-node-b`.
- **FM gRPC is plaintext and unauthenticated.** Fine within the cluster
  network, but do not expose it outside without mTLS or a NetworkPolicy.

---

## 4. End-to-end flow

### Phase 1 — A T3K node joins and advertises its chips

1. **wh-node-labeler** runs `tt-smi` and writes labels
   (`tenstorrent.com/arch=wormhole`, board type, chip count, host rank, …)
   onto the node via the apiserver. These are inputs the scheduler reads later.
2. **wh-dra-kubelet-plugin** starts and opens a UDS in
   `/var/lib/kubelet/plugins_registry/`. The kubelet's plugin-watcher sees it
   and does the registration handshake, learning the driver name
   `wormhole.tenstorrent.com`.
3. The plugin **enumerates devices** by scanning `/dev/tenstorrent` + `/sys`
   (the busy-safe path) and **publishes a ResourceSlice** to the apiserver:
   "this node offers these devices, with these attributes."
4. **fm-agent** starts on the same node, opens `/dev/tenstorrent` devices via
   UMD to discover the ethernet mesh, and registers + streams heartbeats to the
   **fm-controller** on the control plane. The controller now has a cluster-wide
   topology view.

At the end of Phase 1 the control plane knows — purely from metadata — what
every T3K node holds, without running any workload.

### Phase 2 — A user asks for hardware

5. A user applies a Pod + **ResourceClaimTemplate** referencing the DeviceClass
   `t3k.wormhole.tenstorrent.com`.
6. The **scheduler** matches the claim against published ResourceSlices, finds a
   node whose devices satisfy it, records the allocation in the claim status,
   and **binds the pod to that node**. Still pure metadata — no chip touched.

### Phase 3 — The kubelet wires the chips into the container

7. The node's **kubelet** calls the plugin over the DRA socket:
   **`NodePrepareResources(claim)`**.
8. The plugin resolves which physical devices the claim maps to and writes a
   **CDI spec** (JSON) into `/var/run/cdi` — a recipe: inject
   `/dev/tenstorrent/N` and bind-mount the hugepage filesystem. It returns the
   CDI device name to the kubelet.
9. The kubelet tells **containerd** (over CRI) to create the container
   referencing that CDI device. The runtime reads the CDI spec and performs
   the injection. Device nodes and hugepage mounts appear inside the container.

### Phase 4 — The workload runs

10. The container starts with `/dev/tenstorrent/N` + `/dev/hugepages-1G`
    visible. UMD opens the char devices and `mmap`s BAR0 to drive the chips.
    Multi-node jobs talk chip↔chip over the TT ethernet mesh and coordinate
    host-to-host via MPI over `TT_ETHERNET_IFACE` (derived from node labels).

### Phase 5 — Teardown

11. On pod exit the kubelet calls **`NodeUnprepareResources`**; the plugin
    removes its CDI spec, the allocation is released in the apiserver, and the
    devices are free for the next claim.

Phases 1 and 4 (node join + FM discovery) happen once at node bring-up.
Phases 2–3 and 5 repeat per pod.

```
 1. wh-node-labeler ──HTTPS──► apiserver     : set arch/board/rank labels
 2. plugin ──UDS──► kubelet                  : registration handshake
 3. plugin ──HTTPS──► apiserver              : publish ResourceSlice
 4. fm-agent ──UMD──► chips                 : ethernet-mesh discovery
    fm-agent ──TCP──► fm-controller          : register + heartbeat
 5. user applies ResourceClaimTemplate + Pod
 6. scheduler ◄─watch─► apiserver            : match, allocate, bind pod→node
 7. kubelet ──UDS──► plugin                  : NodePrepareResources(claim)
 8. plugin ──► /var/run/cdi                  : write CDI spec
    kubelet ◄── plugin                       : return CDI device name
 9. kubelet ──CRI/UDS──► containerd          : create container, inject devices
10. workload ──mmap BAR0──► chips            : runs; multi-node over TT ethernet
11. kubelet ──UDS──► plugin                  : NodeUnprepareResources (on exit)
```

---

## 5. The fabric-manager topology loop

The FM loop runs independently of the DRA allocation path. It provides
cluster-wide topology visibility and link health — but DRA allocation works
even if FM is down.

```
fm-agent (pod IP :50053)
    │
    ├──UMD mmap BAR0──► chips          ethernet-mesh discovery
    │                                  (fails if chips busy — returns empty mesh)
    │
    └──gRPC TCP──► ttfm-controller K8s svc ──► fm-controller :50052
                   register + heartbeat stream
                   controller aggregates → cluster-wide view + web UI

wh-topology-export (exec in plugin pod)
    │
    └──gRPC TCP──► ttfm-agent.ttfm.svc.cluster.local:50053
                   internalTrafficPolicy:Local → hits same-node agent only
                   GetTopology → asics + local_connections + exit_nodes
                                + cluster_descriptor_yaml
```

- **`local_connections`** — intra-node chip-to-chip ethernet links
- **`exit_nodes`** — inter-node links (which chip connects to which remote chip)
- **`cluster_descriptor_yaml`** — UMD cluster descriptor consumed by tt-metal

These three fields are populated only when the agent ran UMD discovery on idle
hardware. While chips are busy the agent falls back to PCIe-only enumeration
and those fields come back empty. Run `wh-topology-export` before launching a
workload, or use `-rediscover` to force a fresh scan.

---

## 6. The two read paths (busy-hardware split)

| Path | Used by | Source | Works while chips busy? | Gives you |
|---|---|---|---|---|
| **Busy-safe** | wh-dra-kubelet-plugin, `wh-device-probe` | `/dev/tenstorrent` + `/sys` | ✅ yes | chip count, device nodes, arch, unique-id, PCI address |
| **UMD** | fm-agent, `wh-topology-export`, `tt-smi` | UMD (maps BAR0) | ❌ falls back to empty | the above **plus** ethernet mesh + cluster descriptor |

The busy-safe path never maps BAR0, so device enumeration is reliable even
when a workload owns the chips. The UMD path is the only way to get mesh
connectivity data, but it requires idle hardware.

---

## 7. Tools

Both tools are compiled into the `wh-dra-plugin` image and run by exec-ing
into the plugin pod on the target node.

| Tool | Talks to | Purpose | Key flags |
|---|---|---|---|
| `wh-device-probe` | kernel (`/dev`, `/sys`) only | busy-safe chip enumeration; no FM required | `-json`, `-expect N` |
| `wh-topology-export` | FM agent via K8s service | dump all 4 topology fields to JSON + cluster-descriptor YAML | `-addr`, `-rediscover`, `-stdout` |

```bash
PLUGIN_POD=$(kubectl -n kube-system get pods -l app=wh-dra-kubelet-plugin \
  -o jsonpath='{.items[0].metadata.name}')

# busy-safe enumeration (works anytime)
kubectl -n kube-system exec -it $PLUGIN_POD -- wh-device-probe

# full topology (FM agent must be running, chips should be idle)
kubectl -n kube-system exec -it $PLUGIN_POD -- \
  wh-topology-export \
  -addr ttfm-agent.ttfm.svc.cluster.local:50053 \
  -stdout
```

The `-addr` flag must point to the K8s service — `localhost:50053` will not
work because the FM agent runs in pod networking, not on the host network.

---

## 8. Deployment, namespaces, and Helm

Two Helm charts, two namespaces, independent lifecycles.

### Namespaces

| Namespace | What lives there | Deployed by |
|---|---|---|
| `kube-system` | `wh-dra-kubelet-plugin` DaemonSet, `wh-node-labeler` DaemonSet, RBAC, `tt-node-topology` ConfigMap | CI (GitHub Actions) on every `main` push |
| `ttfm` | `fm-controller` Deployment + Services, `fm-agent` DaemonSet, `ghcr-credentials` Secret | manual `helm upgrade` |
| cluster-scoped | `DeviceClass t3k.wormhole.tenstorrent.com`, ClusterRoles/Bindings | CI |
| workload namespaces (e.g. `default`, `mif`) | `ResourceClaimTemplate` + workload pods | users / Odin |

### Helm — wh-dra-plugin (CI-managed)

Chart: `helm/wh-dra-plugin`. Every push to `main` triggers GitHub Actions:

```
git push → GHA build job  → docker build + push to ghcr.io/slifersky/wh-dra-plugin:sha-XXXX
                ↓
           GHA deploy job → helm upgrade --install wh-dra-plugin
           (self-hosted runner on control-plane-01)
                ↓
           Kubernetes rolling update on t3k-node-a + t3k-node-b
```

Manual equivalent:
```bash
helm upgrade --install wh-dra-plugin ./helm/wh-dra-plugin \
  --namespace kube-system \
  --set image.repository=ghcr.io/slifersky/wh-dra-plugin \
  --set image.tag=sha-$(git rev-parse --short HEAD)
```

### Helm — tt-fabric-manager (manual)

Chart: `deploy/ttfm` (bundled in this repo). Requires a `ghcr-credentials`
secret with a GitHub PAT that has `read:packages` access to
`ghcr.io/tenstorrent/tt-fabric-manager`.

```bash
# First time only:
kubectl create namespace ttfm
kubectl create secret docker-registry ghcr-credentials \
  --namespace ttfm \
  --docker-server=ghcr.io \
  --docker-username=<github-username> \
  --docker-password=<github-pat-read-packages>

# Install or upgrade:
helm -n ttfm upgrade --install ttfm \
  -f deploy/ttfm/values.t3k.yaml \
  deploy/ttfm
```

Verify:
```bash
kubectl -n ttfm get pods -o wide
# Expected: ttfm-controller-* on control-plane-01
#           ttfm-agent-* on t3k-node-a and t3k-node-b

kubectl -n ttfm port-forward svc/ttfm-controller-ui 8080:8080
# http://localhost:8080 → topology diagram + /api/topology-summary
```

### Ports

| Component | Port | Exposed as | Reachable from |
|---|---|---|---|
| FM agent gRPC | `50053` | ClusterIP service `ttfm-agent.ttfm` (`internalTrafficPolicy: Local`) | inside cluster only (same node) |
| FM controller gRPC | `50052` | ClusterIP service `ttfm-controller.ttfm` | inside cluster only |
| FM controller web UI | `8080` | ClusterIP service `ttfm-controller-ui.ttfm` | `kubectl port-forward` |
| DRA plugin metrics | `9090` | not exposed | node-local only |

---

## 9. CI/CD pipeline and image registries

### Registries

| Registry | Address | What it stores | Auth |
|---|---|---|---|
| GitHub Container Registry | `ghcr.io/slifersky/wh-dra-plugin` | DRA plugin image | `GITHUB_TOKEN` (CI) |
| GitHub Container Registry | `ghcr.io/tenstorrent/tt-fabric-manager` | FM agent + controller image | PAT with `read:packages` in `ghcr-credentials` secret |
| Local plain-HTTP registry | `192.168.1.60:5000` | workload images (`moreh-vllm`, etc.) | none (LAN-only) |

Worker nodes must list the local registry under `insecure-registries`:
```jsonc
// /var/snap/docker/current/config/daemon.json  (snap-Docker nodes)
// /etc/docker/daemon.json                       (package-Docker nodes)
{ "insecure-registries": ["192.168.1.60:5000"] }
```

### GitHub Actions pipeline

Triggers on push or PR to `main` when `cmd/**`, `pkg/**`, `Dockerfile`,
`go.mod`, `go.sum`, or `helm/**` changes.

```
push to main
    │
    ▼
Job 1: build  (ubuntu-latest)
    ├── login to ghcr.io  (GITHUB_TOKEN)
    ├── tag: sha-<7-char SHA>  +  latest (main only)
    ├── docker buildx  (layer cache via GHA)
    └── push ghcr.io/slifersky/wh-dra-plugin:<tag>
    │
    ▼
Job 2: deploy  (self-hosted runner on control-plane-01)
    └── helm upgrade --install wh-dra-plugin
        --set image.tag=sha-<SHA>
        → rolls wh-dra-kubelet-plugin DaemonSet
        → rolls wh-node-labeler DaemonSet
```

- PRs build but do not push or deploy.
- Image tag is the short commit SHA — rollback is `--set image.tag=sha-<old>`.
- FM chart (`deploy/ttfm`) is **not** managed by CI — upgrade it manually.

### Image summary

| Component | Registry | Updated by |
|---|---|---|
| `wh-dra-kubelet-plugin` + `wh-node-labeler` | `ghcr.io/slifersky/wh-dra-plugin:sha-…` | CI on every `main` push |
| `fm-agent` + `fm-controller` | `ghcr.io/tenstorrent/tt-fabric-manager:latest` | manual `helm upgrade` (separate repo) |
| `moreh-vllm` (workload) | `192.168.1.60:5000/moreh-vllm:dev` | manual `docker build && push` |
