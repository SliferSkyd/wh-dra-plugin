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

| Component | Host | Why it lives there |
|---|---|---|
| kube-apiserver + etcd | control plane | source of truth for ResourceSlice / ResourceClaim / DeviceClass / node labels |
| kube-scheduler (DRA structured parameters) | control plane | matches claims to nodes; pure metadata, never touches a chip |
| fabric-manager **controller** *(optional)* | control plane / mgmt node | cluster-wide topology view + cross-host link retraining; **not** required by the DRA path |
| **wh-dra-kubelet-plugin** (DaemonSet) | every T3K node | enumerates devices, publishes ResourceSlice, injects devices via CDI |
| **wh-node-labeler** (DaemonSet) | every T3K node | runs `tt-smi`, writes node labels consumed by the scheduler / DeviceClass |
| fabric-manager **agent** (`:50053`) *(optional)* | every T3K node | UMD ethernet-mesh discovery; only source of `local_connections` / `exit_nodes` / `cluster_descriptor_yaml` |
| tt-kmd + `/dev/tenstorrent` + hugepages | every T3K node | kernel/driver layer the chips are reached through |
| workload pod (vLLM / tt-metal) | every T3K node | gets `/dev/tenstorrent/N` + hugepages injected via CDI |
| `wh-device-probe`, `wh-topology-export` (tools) | every T3K node | local debugging: scan kernel state / dump full FM topology |

Nothing hardware-facing runs on the control plane. The kubelet plugin is pinned
to hardware nodes by `nodeSelector: tenstorrent.com/arch=wormhole` and
hostPath-mounts `/dev/tenstorrent`, so it can only schedule where the chips are.
The one component that *may* live on the control plane is the fabric-manager
controller — and the DRA path does not need it.

---

## 2. Topology diagram

```
┌────────────────────────────────────────────────────────────────────────────┐
│  CONTROL-PLANE HOST(s)                          (no Tenstorrent hardware)    │
│                                                                              │
│   ┌───────────────┐   ┌──────────────────┐   ┌──────────────────────────┐   │
│   │ kube-apiserver│   │  kube-scheduler  │   │ fabric-manager CONTROLLER │   │
│   │  + etcd       │   │  (DRA structured │   │  :50051/52  (OPTIONAL —   │   │
│   │               │   │   parameters)    │   │  not in wh-dra manifests, │   │
│   │ stores:       │   │                  │   │  not needed by DRA path)  │   │
│   │  ResourceSlice│◄──┤ matches Claim ↔  │   │  central topology view,   │   │
│   │  ResourceClaim│   │ Slice, allocates,│   │  link-retrain orchestration│  │
│   │  DeviceClass  │   │ binds pod→node   │   └─────────────▲────────────┘   │
│   │  Node labels  │   └──────────────────┘                 │ heartbeats /   │
│   └───▲────▲──────┘                                         │ registration   │
│       │    │                                                │ (if deployed)  │
└───────┼────┼────────────────────────────────────────────────┼───────────────┘
        │    │ publish ResourceSlice          set node labels  │
        │    │ + NodePrepare CDI                                │
════════╪════╪══════════════════ same K8s cluster ═════════════╪═══════════════
        │    │                                                  │
┌───────┼────┼──────────────────────────────────────────────────┼──────────────┐
│  T3K WORKER HOST  (label: tenstorrent.com/arch=wormhole)        │   ×N nodes   │
│       │    │                                                    │   for multi- │
│   ┌───┴────┴───────────────────────────────────────────────┐   │   node mesh  │
│   │                      kubelet                            │   │              │
│   └───▲──────────────▲─────────────────────────────────────┘   │              │
│       │ register     │ NodePrepareResources → CDI edits         │              │
│       │ DRA plugin   │                                          │              │
│   ┌───┴──────────────┴────────┐   ┌──────────────────────┐      │              │
│   │ wh-dra-kubelet-plugin     │   │ wh-node-labeler      │      │              │
│   │  (DaemonSet)              │   │  (DaemonSet)         │      │              │
│   │  • enumerate devices      │   │  • tt-smi → labels   │──────┼──► apiserver │
│   │  • publish ResourceSlice  │   │    (arch, board…)    │      │   (labels)   │
│   │  • Prepare/Unprepare:     │   └──────────┬───────────┘      │              │
│   │    inject /dev/tenstorrent│              │ tt-smi (fails    │              │
│   │    + hugepages via CDI    │              ▼  when busy)      │              │
│   │                           │   ┌──────────────────────┐      │              │
│   │  reads ▼                  │   │ tt-smi (Python CLI)  │      │              │
│   └────────┬──────────────────┘   └──────────────────────┘      │              │
│            │ scan (busy-safe)                                    │              │
│   ┌────────▼───────────────────────────────────────────────┐   │              │
│   │ KERNEL / DRIVER LAYER                                   │   │              │
│   │  tt-kmd → /dev/tenstorrent/{0..N}, by-id/, /sys/class   │   │              │
│   │  hugepages (2M + 1G)                                    │   │              │
│   └────────▲───────────────────────────▲───────────────────┘   │              │
│            │ UMD (maps BAR0,            │ char-dev injection     │              │
│            │  fails when busy)          │ at container start     │              │
│   ┌────────┴───────────────┐   ┌────────┴───────────────────┐   │              │
│   │ fabric-manager AGENT    │   │  WORKLOAD POD              │   │              │
│   │  :50053  (OPTIONAL)     │   │  vLLM / tt-metal           │   │              │
│   │  • UMD ethernet-mesh    │   │  • gets /dev/tenstorrent/N │   │              │
│   │    discovery            │   │    + hugepages via CDI     │   │              │
│   │  • RetrainLinks         │   │  • TT_MESH_HOST_RANK, etc. │   │              │
│   │  AgentService gRPC      │   │    from label-derived envs │   │              │
│   └────────▲────────────────┘   └────────────────────────────┘  │              │
│            │ GetTopology (localhost:50053)                       │              │
│   ┌────────┴───────────────────────────────────────────────┐   │              │
│   │ TOOLS (run on this host, on demand)                     │   │              │
│   │  • wh-device-probe     → reads /dev + /sys (busy-safe)  │   │              │
│   │  • wh-topology-export  → dials AGENT, dumps all 4 fields│   │              │
│   └────────────────────────────────────────────────────────┘   │              │
└──────────────────────────────────────────────────────────────────────────────┘
```

For a multi-host mesh (2-/4-/8-node deployments) the `×N` block repeats: each
T3K host runs its own plugin + labeler (+ optional agent) and publishes a
node-local ResourceSlice. Cross-host wiring (which host is rank 0, which pod)
comes from admin-set labels, and the scheduler co-schedules the pod group.

---

## 3. How components communicate

| Edge | Mechanism | Transport / endpoint | Auth |
|---|---|---|---|
| plugin ↔ kubelet (register) | kubelet plugin-registration gRPC | **UDS** `/var/lib/kubelet/plugins_registry/` | filesystem perms |
| kubelet → plugin (allocate) | DRA node API gRPC: `NodePrepareResources` / `NodeUnprepareResources` | **UDS** `/var/lib/kubelet/plugins/wormhole.tenstorrent.com/` | filesystem perms |
| plugin → apiserver | kube REST + watch (publish `ResourceSlice`) | **HTTPS** (apiserver :6443) | ServiceAccount + RBAC |
| node-labeler → apiserver | kube REST PATCH (node labels) | **HTTPS** :6443 | ServiceAccount + RBAC |
| scheduler ↔ apiserver | watch / list / bind | **HTTPS** :6443 | TLS + RBAC |
| plugin → CDI | writes CDI spec JSON files | filesystem `/var/run/cdi` | — |
| kubelet → containerd | CRI gRPC; runtime reads CDI, injects device nodes | **UDS** (CRI socket) | filesystem perms |
| FM agent ↔ FM controller | gRPC: register + heartbeat stream | **TCP** :50051/50052 | **plaintext, unauthenticated** |
| wh-topology-export → FM agent | gRPC `AgentService.GetTopology` | **TCP** `localhost:50053` | **plaintext (insecure)** |
| plugin / wh-device-probe → kernel | `stat()` + sysfs reads | syscalls (no network) | root in container |
| UMD / tt-smi / FM agent → chips | `mmap` BAR0 + ioctl | char dev `/dev/tenstorrent/N` | device perms |
| workload pod → chips | UMD mmap of injected char devs + hugepages | `/dev/tenstorrent/N`, `/dev/hugepages*` | CDI-injected |
| chip ↔ chip (multi-node) | TT fabric over ethernet; host-host MPI over NIC `TT_ETHERNET_IFACE` | Ethernet | — |

Two things to note about *how*:

- **Inside a node, almost nothing is networked.** plugin↔kubelet and
  kubelet↔runtime are **Unix domain sockets**; the device hand-off itself is
  **files** (CDI JSON in `/var/run/cdi`) plus **char-device injection** — not
  RPC. The only TCP on the node is the fabric-manager agent's `:50053`.
- **Trust boundaries differ.** Everything touching the apiserver is
  TLS + ServiceAccount + RBAC. The kubelet UDS channels are gated by filesystem
  permissions (hostPath, root). The **fabric-manager gRPC is plaintext and
  unauthenticated** — fine on `localhost:50053`, but put it behind mTLS or a
  NetworkPolicy if you ever expose the agent/controller off-host.

---

## 4. End-to-end flow

### Phase 1 — A T3K node joins and advertises its chips

1. **node-labeler** runs `tt-smi` once and writes labels
   (`tenstorrent.com/arch=wormhole`, board type, …) onto the node via the
   apiserver. These are *inputs* the scheduler reads later.
2. **wh-dra-kubelet-plugin** starts and opens a UDS in
   `/var/lib/kubelet/plugins_registry/`. The kubelet's plugin-watcher sees it
   and does the registration handshake, learning the driver name
   `wormhole.tenstorrent.com`.
3. The plugin **enumerates devices** by scanning `/dev/tenstorrent` + `/sys`
   (the busy-safe path) and **publishes a ResourceSlice** to the apiserver:
   "this node offers these devices, with these attributes."

At the end of Phase 1 the control plane knows — purely from metadata — what
every T3K node holds, without ever touching a chip.

### Phase 2 — A user asks for hardware

4. A user applies a Pod + **ResourceClaim** referencing the DeviceClass
   `t3k.wormhole.tenstorrent.com` (selector
   `device.driver == "wormhole.tenstorrent.com"`).
5. The **scheduler** matches the claim against published ResourceSlices, finds a
   node whose devices satisfy it, records the allocation in the claim status,
   and **binds the pod to that node**. Still pure metadata — no chip touched.

### Phase 3 — The kubelet wires the chips into the container

6. The node's **kubelet** calls the plugin over the DRA socket:
   **`NodePrepareResources(claim)`**.
7. The plugin resolves which physical devices the claim maps to and writes a
   **CDI spec** (JSON) into `/var/run/cdi` — a recipe: "inject
   `/dev/tenstorrent/N` and bind-mount the hugepage filesystems." It returns the
   CDI device name to the kubelet.
8. The kubelet tells **containerd** (over CRI) to create the container
   referencing that CDI device; the **runtime reads the CDI spec and performs
   the injection**. The device nodes and hugepage mounts appear inside the
   container. The hand-off is files + device nodes, not a network call.

### Phase 4 — The workload runs

9. The container starts with `/dev/tenstorrent/N` + `/dev/hugepages*` visible.
   UMD opens the char devices and `mmap`s BAR0 to drive the chips. Multi-node
   jobs talk chip↔chip over the ethernet mesh and coordinate host-to-host via
   MPI over `TT_ETHERNET_IFACE` (derived from node labels).

### Phase 5 — Teardown

10. On pod exit the kubelet calls **`NodeUnprepareResources`**; the plugin
    removes its CDI spec, the allocation is released in the apiserver, and the
    devices are free for the next claim.

Steps 1–3 happen once at node bring-up; 4–10 repeat per pod.

```
 1. node-labeler ──HTTPS──► apiserver        : set arch/board labels
 2. plugin ──UDS──► kubelet                   : registration handshake
 3. plugin ──HTTPS──► apiserver               : publish ResourceSlice
 4. user applies ResourceClaim + Pod
 5. scheduler ◄─watch─► apiserver             : match, allocate, bind pod→node
 6. kubelet ──UDS──► plugin                   : NodePrepareResources(claim)
 7. plugin ──► /var/run/cdi (CDI spec) ──UDS──► kubelet : returns CDI device
 8. kubelet ──CRI/UDS──► containerd           : create container, inject devices
 9. workload ──mmap BAR0──► chips             : runs; multi-node over ethernet
10. kubelet ──UDS──► plugin                   : NodeUnprepareResources (on exit)
```

---

## 5. The fabric-manager side-channel (off the critical path)

Everything in §4 gets a workload onto chips **without** the fabric manager. The
FM flow is a separate, optional loop for topology visibility and link health:

```
FM agent (:50053) ──gRPC register+heartbeat──► FM controller (:50051/52)
wh-topology-export ──gRPC GetTopology──► FM agent (localhost:50053)
FM agent ──UMD mmap BAR0──► chips   (ethernet-mesh discovery; fails when busy)
```

- The **agent** discovers the chip-to-chip ethernet mesh and registers +
  heartbeats to the **controller**, which holds a cluster-wide view.
- `wh-topology-export` dials the local agent and dumps the full topology — the
  `local_connections` / `exit_nodes` / `cluster_descriptor_yaml` the DRA path
  never reads.

This loop is **not on the allocation critical path**. If the agent is down — or
its UMD discovery fails because the chips are busy — DRA allocation still works,
because allocation reads `/dev/tenstorrent`, not the fabric manager. The fabric
manager earns its place only when you need the mesh connectivity itself: link
validation, retraining, or a future finer-grained allocation model that must
know which chips form a contiguous mesh.

---

## 6. The two read paths (and the busy-hardware split)

This is the crux of the design:

| Path | Used by | Source | Works while chips busy? | Gives you |
|---|---|---|---|---|
| **Busy-safe** | wh-dra-kubelet-plugin, `wh-device-probe` | `/dev/tenstorrent` + `/sys` | ✅ yes | chip count, device nodes, arch, unique-id, PCI |
| **UMD** | fabric-manager agent, `wh-topology-export`, `tt-smi` | UMD (maps BAR0) | ❌ falls back to empty | the above **plus** ethernet mesh + cluster descriptor |

The busy-safe path never maps BAR0, so device **enumeration** is reliable even
when a workload owns the chips. The UMD path is the only way to get the
**mesh/connectivity** data, but it needs idle hardware (or it returns empty mesh
fields and no cluster descriptor — exactly the fallback you see when a workload
is running).

---

## 7. Tools

| Tool | Source | Talks to | Purpose |
|---|---|---|---|
| `wh-device-probe` | `cmd/wh-device-probe` | kernel (`/dev`, `/sys`) | dependency-free, busy-safe enumeration; `-json`, `-expect N` |
| `wh-topology-export` | `cmd/wh-topology-export` | FM agent `:50053` | dump all four topology fields to JSON + cluster-descriptor YAML; `-rediscover`, `-stdout` |

Both run **on the T3K node** — `wh-device-probe` because it reads local kernel
state, `wh-topology-export` because it dials the local agent.

---

## 8. Deployment, namespaces, and Helm

The DRA plugin and the (optional) fabric manager are **two independent
deployments in two different namespaces**. They share nodes, not lifecycles —
you can install, upgrade, or remove either without touching the other.

### Namespaces

| Namespace | What lives there | Deployed by |
|---|---|---|
| `kube-system` | `wh-dra-kubelet-plugin` (DaemonSet), `wh-node-labeler` (DaemonSet), their RBAC + `tt-node-topology` ConfigMap | wh-dra-plugin Helm chart / CI |
| `ttfm` *(optional)* | fabric-manager `controller` (Deployment + Services) and `agent` (DaemonSet) | `tt-fabric-manager` `charts/ttfm` |
| cluster-scoped | `DeviceClass t3k.wormhole.tenstorrent.com`, ClusterRoles/Bindings | both charts |
| workload namespaces (e.g. `mif`) | `ResourceClaimTemplate`s + the pods that claim devices | users / Odin |

The DRA plugin lives in `kube-system` because it is node-critical
infrastructure that must come up before workloads. The fabric manager is a
separate operational tool, isolated in `ttfm` so its (privileged) agents and
unauthenticated web UI are scoped away from system components and easy to
remove. A `ResourceClaim` is namespaced with the workload; the `ResourceSlice`
the plugin publishes and the `DeviceClass` are **not** namespaced (cluster
scope) — which is why a pod in any namespace can claim a device.

### Helm — wh-dra-plugin

Chart: `helm/wh-dra-plugin`. Installed into `kube-system`. In this repo it is
driven by CI: every push to `main` builds the image and runs
`helm upgrade` on the self-hosted runner, which rolls the two DaemonSets. Manual
equivalent:

```bash
helm upgrade --install wh-dra-plugin ./helm/wh-dra-plugin \
  --namespace kube-system \
  --set image.repository=ghcr.io/slifersky/wh-dra-plugin \
  --set image.tag=sha-$(git rev-parse --short HEAD)
```

Because it is a DaemonSet gated by `nodeSelector: tenstorrent.com/arch=wormhole`,
it auto-lands on every current and future T3K node — there is no per-node setup.

### Helm — fabric manager (optional)

Chart: `tt-fabric-manager/charts/ttfm`. Installed into `ttfm`. The agent is a
DaemonSet, so it likewise auto-lands on every Tenstorrent node — installing the
chart is the entire "per-node setup". A cluster-tailored values file lives at
`charts/ttfm/values.t3k.yaml` (agent affinity matched to
`tenstorrent.com/arch=wormhole`, web UI on, ingress off, no FSD):

```bash
kubectl create namespace ttfm
# reuse the registry secret the DRA plugin already uses:
kubectl get secret ghcr-credentials -n kube-system -o yaml \
  | sed 's/namespace: kube-system/namespace: ttfm/' | kubectl apply -f -
helm -n ttfm upgrade --install ttfm -f charts/ttfm/values.t3k.yaml charts/ttfm
```

Monitor it via the controller's web UI / JSON API:

```bash
kubectl -n ttfm port-forward svc/ttfm-controller-ui 8080:8080
#   http://localhost:8080  → topology diagram + /api/topology-summary
```

### Ports and the manual-vs-Helm overlap

The Helm agent and a hand-started Docker agent both use host ports
`:50053` (agent) / `:50051-52` (controller). Run **one or the other**, not
both on the same node — stop any manual `docker run` fabric-manager containers
(`docker rm -f fabric-agent fabric-controller`) before installing the Helm
chart, or they collide. The in-cluster pods pull
`ghcr.io/tenstorrent/tt-fabric-manager` from the registry (needing the
`ghcr-credentials` secret), **not** a locally built image — so a node's manual
`tt-fabric-manager:latest` build is unrelated to what the DaemonSet runs.

---

## 9. CI/CD pipeline and image registries

Two image registries serve different parts of the stack. They are independent
and neither replaces the other.

### Registries at a glance

| Registry | Address | What it stores | Auth |
|---|---|---|---|
| GitHub Container Registry | `ghcr.io/<repo>` | `wh-dra-plugin` (the DRA daemon) | `GITHUB_TOKEN` / `ghcr-credentials` secret |
| Local plain-HTTP registry | `192.168.1.60:5000` | workload images (`moreh-vllm`, etc.) | none (LAN-only, insecure) |

**ghcr.io** is the source of truth for the DRA plugin. Every merge to `main`
produces a fresh image there, and the self-hosted runner applies it
automatically (see below). **The local registry** (`192.168.1.60:5000`, hosted
on the control-plane node) holds workload images that are too large or
proprietary to push to a public registry. It speaks plain HTTP, so every
worker node's Docker / containerd daemon must list it under
`insecure-registries`:

```jsonc
// /var/snap/docker/current/config/daemon.json  (snap-Docker nodes)
// /etc/docker/daemon.json                       (package-Docker nodes)
{
  "insecure-registries": ["192.168.1.60:5000"]
}
```

Restart Docker after editing (`sudo snap restart docker` on snap nodes;
`sudo systemctl restart docker` on package nodes).

### GitHub Actions pipeline — wh-dra-plugin

File: `.github/workflows/build.yml`. Triggers on push **or PR** to `main`
when any of `cmd/**`, `pkg/**`, `Dockerfile`, `go.mod`, `go.sum`, or
`helm/**` changes.

```
push to main
    │
    ▼
Job 1: build  (runs on: ubuntu-latest)
    ├── docker/login-action  →  ghcr.io  (uses GITHUB_TOKEN)
    ├── docker/metadata-action
    │     tags: sha-<7-char SHA>
    │            latest  (main only)
    ├── docker/setup-buildx-action  (layer cache via GHA cache)
    └── docker/build-push-action
          push: only on push events (not PRs)
          image: ghcr.io/<repo-lowercase>:<tag>
    │
    ▼
Job 2: deploy  (runs on: self-hosted, main push only, needs: build)
    ├── azure/setup-helm
    └── helm upgrade --install wh-dra-plugin ./helm/wh-dra-plugin \
              --namespace kube-system \
              --set image.repository=ghcr.io/<repo> \
              --set image.tag=sha-<SHA>
              → rolls wh-dra-kubelet-plugin DaemonSet
              → rolls wh-node-labeler DaemonSet
```

Key properties:

- **Self-hosted runner lives on the control-plane node.** It already has
  `kubectl` + `helm` + a kubeconfig that reaches the API server — no extra
  credential plumbing.
- **PRs build but do not push or deploy.** The `push:` flag on
  `build-push-action` is `${{ github.event_name == 'push' }}`, and the
  `deploy` job has `if: github.ref == 'refs/heads/main' && github.event_name == 'push'`.
- **Image tag is the short commit SHA** (`sha-<7 chars>`), so every build is
  immutable and rollback is `helm upgrade --set image.tag=sha-<old-SHA>`.
- **`latest` tag is updated on every main merge**, convenient for one-off
  `docker pull` debugging on a worker node.
- **DaemonSet rolling update** is handled by Helm + Kubernetes: the scheduler
  replaces one pod at a time on each T3K node; no manual node drain needed for
  plugin updates.

### Workload images — local registry workflow

Workload images (e.g. `moreh-vllm:dev`) are built and pushed manually to the
local registry. There is no CI pipeline for these today:

```bash
# on the build machine (or control plane):
docker build -t 192.168.1.60:5000/moreh-vllm:dev .
docker push 192.168.1.60:5000/moreh-vllm:dev
```

Because the registry is plain HTTP, the push also requires the build machine
to have `192.168.1.60:5000` in its own `insecure-registries`. Worker nodes
pull with `imagePullPolicy: Always` in the pod spec, so re-pushing the same
tag is enough to roll the workload — no image-tag change or pod delete needed
(Kubernetes re-pulls on the next restart).

### Summary — which image comes from where

| Component | Registry | Updated by |
|---|---|---|
| `wh-dra-kubelet-plugin` DaemonSet | `ghcr.io/<repo>:sha-…` | CI on every `main` push |
| `wh-node-labeler` DaemonSet | `ghcr.io/<repo>:sha-…` | CI on every `main` push |
| `fabric-manager` agent + controller | `ghcr.io/tenstorrent/tt-fabric-manager` | separate repo / manual |
| `moreh-vllm` (workload) | `192.168.1.60:5000/moreh-vllm:dev` | manual `docker build && push` |
