# Slide Outline — wh-dra-kubelet-plugin

> Paste into Google Slides / PowerPoint.
> Each `---` is a slide boundary. Speaker notes follow each slide.
> Slides 2–8 are K8s basics. Skip them with experienced K8s audiences.

---

## Slide 1 — Title

**wh-dra-kubelet-plugin**
Kubernetes Dynamic Resource Allocation for Tenstorrent T3K Hardware

*[Your name] · June 2026*

> **Speaker notes:** This talk is split into two parts. Part 1 explains Kubernetes from scratch — skip if everyone knows K8s. Part 2 covers the actual plugin work. By the end you will understand what the plugin does, how it was built, and what the live cluster looks like today.

---

## Slide 2 — What is Kubernetes? (The problem)

**You have 10 servers and 50 services.**

Without Kubernetes:
- SSH into each server and start processes manually
- Remember which service runs on which machine
- Restart crashed processes by hand
- Prevent two services from using the same GPU yourself

**Kubernetes (K8s) is a cluster operating system.**

```
You write:   "I want 2 copies of my inference server,
              each needing 16 GB RAM and one T3K device"

Kubernetes:  picks machines with free RAM + free T3K,
             starts containers, restarts on crash,
             moves workloads away from broken machines
```

> **Speaker notes:** The key insight is declarative configuration. You describe the desired state; K8s reconciles the actual state toward it continuously. You don't write scripts that say "start process on machine X" — you write YAML that says "I need N copies of this running."

---

## Slide 3 — K8s: Control Plane and Nodes

**Two kinds of machines in a cluster:**

**Control plane node** — the brain. Runs no workloads.
- `kube-apiserver` — every command goes here (REST API + database front-end)
- `kube-scheduler` — picks which node runs each pod
- `etcd` — key-value store that holds all cluster state
- `kube-controller-manager` — reconciliation loops

**Worker nodes** — the muscle. Run actual containers.
- `kubelet` — local agent, watches API server, starts/stops containers
- `containerd` — actually creates containers (pulls images, manages namespaces)
- `kube-proxy` — handles pod-to-pod networking

```
Our cluster:
  control-plane-01  192.168.1.60   ← brain
  t3k-node-a        192.168.1.247  ← T3K worker
  t3k-node-b        192.168.1.243  ← T3K worker
```

> **Speaker notes:** etcd is the source of truth. If etcd is lost, the cluster is lost. kubelet is the component that our DRA plugin talks to via a Unix socket — it calls PrepareResourceClaims on the plugin before starting each container that needs a T3K device.

---

## Slide 4 — K8s: Pods — the smallest unit

**A Pod wraps one or more containers that:**
- share a single IP address
- share mounted volumes
- always run on the same machine

99% of the time: **one Pod = one process**.

```yaml
# Minimal pod example
apiVersion: v1
kind: Pod
metadata:
  name: my-inference-server
spec:
  containers:
  - name: server
    image: my-vllm:latest
    resources:
      requests:
        memory: "16Gi"
```

> You never start a container directly in K8s.
> You always create a Pod.

> **Speaker notes:** A pod is the scheduling unit. The scheduler places entire pods on nodes, not individual containers. This is why if you have two containers in the same pod they always land on the same machine — which is intentional for tightly coupled sidecars (e.g., log collector + main app).

---

## Slide 5 — K8s: How a Pod Goes from YAML to Running

```
1. kubectl apply -f my-pod.yaml
   → sends YAML to API server over HTTPS
   → API server validates and writes to etcd
   → Pod status: Pending

2. kube-scheduler sees a Pending pod
   → checks all nodes: available CPU, RAM, labels, taints
   → picks t3k-node-a
   → writes "this pod goes to t3k-node-a" to etcd
   → Pod status: Scheduled (still Pending from container view)

3. kubelet on t3k-node-a is watching the API server
   → sees pod assigned to its node
   → tells containerd: "pull this image, start this container"
   → containerd pulls image, creates container, starts process
   → kubelet reports: Running

4. Process exits (or kubectl delete pod)
   → kubelet tells containerd to stop container
   → Pod status: Succeeded or Failed
   → etcd updated, device released
```

> **Speaker notes:** The scheduler doesn't talk to kubelet directly — it just writes the node assignment to etcd. kubelet polls the API server constantly and acts when it sees a pod assigned to its node. This decoupling is what makes the system resilient: control plane and workers can temporarily lose network, and everything reconnects cleanly.

---

## Slide 6 — K8s: Labels, DaemonSets, and Namespaces

**Labels** — key-value tags you put on any K8s object
```bash
kubectl label node t3k-node-a tenstorrent.com/arch=wormhole
kubectl get node t3k-node-a --show-labels
```

**DaemonSet** — ensures exactly one pod runs on every matching node
- New node added + right labels → pod auto-deployed
- We use DaemonSets for: plugin, node-labeler, FM agent

**NodeSelector** — only schedule on nodes with these labels
```yaml
spec:
  nodeSelector:
    tenstorrent.com/arch: wormhole
# → only deploys on T3K nodes, never on control plane
```

**Namespaces** — logical partitions inside a cluster
```
kube-system   → system components (our plugin, labeler)
ttfm          → tt-fabric-manager controller + agents
default       → workloads if you don't specify
```

> **Speaker notes:** Labels are how components discover each other in K8s. Our node-labeler DaemonSet automatically sets hardware labels on T3K nodes, so the plugin DaemonSet and the FM agent DaemonSet know where to deploy without any manual configuration per machine.

---

## Slide 7 — K8s: Services and DNS

**A Service** gives a stable DNS name and IP to a set of pods.
Pod IPs change on restart; Service IP never changes.

```
ttfm-agent.ttfm.svc.cluster.local:50053
  ↑            ↑          ↑         ↑
  service    namespace  cluster   port
  name                  suffix
```

**internalTrafficPolicy: Local** — route to pod on the SAME node
- Our DRA plugin on t3k-node-a → FM agent on t3k-node-a
- Our DRA plugin on t3k-node-b → FM agent on t3k-node-b
- Never crosses nodes

**ConfigMap** — stores config as cluster state
```yaml
# tt-node-topology ConfigMap
data:
  t3k-node-a: "physical-pod=t3k-a host-rank=0 pod-size=2"
  t3k-node-b: "physical-pod=t3k-a host-rank=1 pod-size=2"
```
Node-labeler reads this and sets the corresponding node labels.

> **Speaker notes:** `internalTrafficPolicy: Local` is critical for our FM agent service. Each plugin pod must talk to the FM agent on its own node — not a remote one — because FM only knows about the chips on its own machine.

---

## Slide 8 — K8s: Useful kubectl Commands

```bash
# Cluster status
kubectl get nodes                          # list all nodes
kubectl get nodes -o wide                  # + IPs
kubectl describe node t3k-node-a           # full node details + labels

# Pods
kubectl get pods -A -o wide               # ALL namespaces
kubectl get pods -n kube-system           # system namespace
kubectl logs <pod-name> -n kube-system    # pod output
kubectl logs <pod-name> -n kube-system -f # follow live

# DRA-specific objects
kubectl get resourceslices                 # advertised devices
kubectl get resourceclaims                 # allocated devices
kubectl get deviceclass                    # device type definitions

# Make changes
kubectl apply -f my-file.yaml             # create or update
kubectl delete pod <name> -n kube-system  # delete; DaemonSet pod restarts
```

> **Speaker notes:** `kubectl get pods -A` is the first thing to run when something is wrong. It shows every pod in the cluster, their status, and which node they're on. `kubectl describe` gives you events which are usually the first clue about why a pod is Pending or crashing.

---

## Slide 9 — K8s Workflow: From YAML to Running Workload

**Every K8s object has four fields — only `spec` is yours to write:**
```yaml
apiVersion: apps/v1   # API group
kind: Deployment      # object type
metadata:             # identity (name, namespace, labels)
  name: my-app
spec:                 # desired state — what YOU write
  replicas: 2
status:               # actual state — Kubernetes fills this automatically
  readyReplicas: 2
```

**The five-step workflow:**

```
① Write    create my-workload.yaml (ResourceClaim + Pod)

② Apply    kubectl apply -f my-workload.yaml
             → API server validates + stores in etcd
             → Pod: Pending, ResourceClaim: Pending

③ Watch    kubectl get pods -w
             Pending → Init → Running → Completed
             (scheduler → PrepareResourceClaims → container start)

④ Inspect  kubectl describe pod <name>     → spec + status + Events
           kubectl logs <name>              → container stdout
           kubectl describe resourceclaim   → allocation result

⑤ Delete   kubectl delete -f my-workload.yaml
             → pod stops → UnprepareResourceClaims → device released
```

**When pod is stuck in Pending — always check Events:**
```bash
kubectl describe pod <name>
# "no device class matched"      → DeviceClass missing or wrong driver name
# "ResourceClaim not allocated"  → plugin not ready or no matching ResourceSlice
# "Insufficient memory"          → node has no free RAM
```

**When pod is stuck in ImagePullBackOff:**
```bash
# Events: "403 Forbidden" → missing imagePullSecret
kubectl create secret docker-registry ghcr-credentials \
  --docker-server=ghcr.io --docker-username=X --docker-password=Y
```

> **Speaker notes:** `kubectl apply` is idempotent — safe to run multiple times, only applies what changed. `kubectl describe` is the single most useful debugging command: the `Events:` section at the bottom shows exactly what each component (scheduler, kubelet, plugin) did and what failed. For DRA workloads, a missing DeviceClass is the most common first-time mistake.

---

## Slide 11 — The Problem We Are Solving

**Before this plugin — running T3K workloads in Kubernetes was painful:**

```yaml
# Every workload had to hard-code this:
spec:
  nodeName: t3k-node-a          # ← manual, breaks if node renamed
  securityContext:
    privileged: true             # ← security hole
  volumes:
  - name: dev-tenstorrent
    hostPath:
      path: /dev/tenstorrent     # ← host device manually mounted
  containers:
  - name: vllm
    env:
    - name: TT_MESH_HOST_RANK
      value: "0"                 # ← manually configured
    volumeMounts:
    - name: dev-tenstorrent
      mountPath: /dev/tenstorrent
```

**Problems:**
- No double-allocation protection — two pods could race on the same chip
- No health feedback — scheduler sends jobs to broken nodes
- Every workload author must know hardware topology

> **Speaker notes:** `privileged: true` means the container has root on the host — it can see all host processes, all host devices, modify network config. This is unacceptable in production. DRA gives precise device access without any host privilege.

---

## Slide 10 — The Solution: DRA

**Dynamic Resource Allocation (DRA)** — GA in Kubernetes v1.35

**What the workload author writes now:**
```yaml
spec:
  resourceClaims:
  - name: my-t3k
    resourceClaimTemplateName: wh-t3k-template
  containers:
  - name: vllm
    resources:
      claims:
      - name: my-t3k
# That's it. No nodeName, no privileged, no volumeMounts.
```

**What arrives in the container automatically:**
```
/dev/tenstorrent/0-3  (device nodes — no hostPath needed)
TT_CHIP_COUNT=8
TT_MESH_HOST_RANK=0
TT_POD_SIZE=2
/dev/hugepages-1G (hugepages mounted)
/tmp/tt_logs (log directory)
```

> **Speaker notes:** The workload author declares intent — "I need a T3K device." The plugin and the scheduler handle placement, injection, and exclusivity. This is identical to how GPU workloads work in cloud Kubernetes: `resources.limits: nvidia.com/gpu: 1`.

---

## Slide 12 — Three New K8s Objects (DRA)

| Object | Who creates it | What it does |
|---|---|---|
| `ResourceSlice` | **Plugin** (automatic) | Advertises hardware to the scheduler with rich attributes |
| `DeviceClass` | Admin (once) | Defines a named category of device (`t3k.wormhole.tenstorrent.com`) |
| `ResourceClaim` | Workload user | "I need one device matching this DeviceClass" |

**Flow:**
```
① Plugin publishes ResourceSlice
     → scheduler sees "t3k-node-a has a wormhole-t3k with 96Gi memory"

② User creates Pod + ResourceClaim
     → scheduler finds matching ResourceSlice
     → allocates device (marks in ResourceClaim.status.allocation)
     → binds Pod to t3k-node-a

③ kubelet calls plugin PrepareResourceClaims
     → plugin writes CDI spec file (device nodes + env vars)
     → kubelet passes CDI IDs to containerd

④ containerd starts container with injected devices
```

> **Speaker notes:** The key difference from the old device plugin API: ResourceSlice can carry structured metadata — chip count, memory, physical pod, host rank, architecture. A DeviceClass CEL selector can filter on any of these. The old API could only count integers (e.g., "2 GPUs").

---

## Slide 13 — ResourceClaim vs ResourceClaimTemplate

**Two patterns for requesting a device:**

**Pattern A — ResourceClaim (direct, shared)**
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
```
Pod: `resourceClaimName: my-t3k-claim` → claim must already exist, multiple pods can share it

**Pattern B — ResourceClaimTemplate (per-pod, auto-generated)**
```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata:
  name: wh-t3k-template
spec:
  spec:
    devices:
      requests:
      - name: t3k
        exactly:
          deviceClassName: t3k.wormhole.tenstorrent.com
```
Pod: `resourceClaimTemplateName: wh-t3k-template` → K8s auto-creates + auto-deletes one claim per Pod

| | ResourceClaim | ResourceClaimTemplate |
|---|---|---|
| Sharing | Multiple pods can share one device | Each pod gets its own exclusive device |
| Lifecycle | Manual — persists until deleted | Automatic — tied to Pod lifetime |
| Best for | Sidecar + trainer sharing a device | Jobs, StatefulSets, vLLM instances |

> **Speaker notes:** We use ResourceClaimTemplate for all our test workloads (test-claim.yaml, multinode StatefulSet) because each pod needs exclusive ownership of its T3K device. A monitoring sidecar that only reads telemetry from the same chip could share a ResourceClaim with the training pod.

---

## Slide 14 — DeviceClass and CEL Selectors

**DeviceClass** — cluster-level, admin-defined category of devices (CEL selects from ResourceSlices):
```yaml
apiVersion: resource.k8s.io/v1
kind: DeviceClass
metadata:
  name: t3k.wormhole.tenstorrent.com
spec:
  selectors:
  - cel:
      expression: device.driver == "wormhole.tenstorrent.com"
```

**CEL selectors in ResourceClaim** — workload-level fine-grained filtering:

| CEL expression | What it filters |
|---|---|
| `device.driver == "wormhole.tenstorrent.com"` | Our plugin's devices only |
| `device.attributes["tenstorrent.com/pod_size"].int == 2` | 2-node physical pods only |
| `device.attributes["tenstorrent.com/host_rank"].int == 0` | Rank-0 node only |
| `device.capacity["tenstorrent.com/memory"] >= quantity("96Gi")` | At least 96 GB DRAM |

**Example claim: 2-node T3K with 96 Gi memory minimum:**
```yaml
spec:
  devices:
    requests:
    - name: t3k
      exactly:
        deviceClassName: t3k.wormhole.tenstorrent.com
        selectors:
        - cel:
            expression: >
              device.attributes["tenstorrent.com/pod_size"].int == 2 &&
              device.capacity["tenstorrent.com/memory"] >= quantity("96Gi")
```

> **Speaker notes:** CEL is a Go-like expression language evaluated entirely by the scheduler — no driver code runs. `quantity("96Gi")` is a Kubernetes resource quantity constructor. The expressions run against the attributes and capacity fields we publish in our ResourceSlice, which is why accurate FM-sourced values matter.

---

## Slide 15 — How the Scheduler Allocates (Structured Parameters)

In DRA v1 (K8s 1.35+), **the scheduler resolves device allocation itself** — no driver webhook:

```
Pod with ResourceClaim arrives
  ↓
Scheduler reads all ResourceSlices from etcd
  ↓
Evaluates DeviceClass CEL + claim CEL selectors against every device
  ↓
Finds a node + device satisfying all constraints
  ↓
Writes result into ResourceClaim.status.allocation:

  allocation:
    devices:
      results:
      - driver:  "wormhole.tenstorrent.com"
        pool:    "t3k-node-a"
        device:  "wormhole-t3k"
        request: "t3k"
  ↓
Binds Pod to t3k-node-a
  ↓
kubelet calls PrepareResourceClaims(claim)
Plugin reads status.allocation → calls DeviceNodePaths → writes CDI spec
```

**Driver's only jobs:**
1. Publish accurate ResourceSlices (what devices exist + their attributes/capacity)
2. Handle PrepareResourceClaims / UnprepareResourceClaims (inject / cleanup)

> **Speaker notes:** Before Structured Parameters (stable in K8s 1.35), DRA required the driver to serve a webhook that the scheduler called back during allocation. That webhook was a latency bottleneck and a single point of failure. Now the scheduler resolves allocation directly from published ResourceSlice data — faster and simpler.

---

## Slide 16 — ResourceClaim Lifecycle

```
kubectl apply -f claim.yaml  (or Pod references a template)
        │
  ┌─────▼──────┐
  │  Pending   │  no pod referenced it yet, or no matching device found
  └─────┬──────┘
        │ scheduler finds matching device + node
  ┌─────▼──────────────┐
  │    Allocated       │  status.allocation set
  │                    │  device reserved, pod can now be bound
  └─────┬──────────────┘
        │ pod bound → kubelet calls PrepareResourceClaims
  ┌─────▼──────────────┐
  │ Reserved (in use)  │  CDI spec file written
  │                    │  container running with injected device
  └─────┬──────────────┘
        │ pod exits → kubelet calls UnprepareResourceClaims
  ┌─────▼──────────────┐
  │   Deallocated      │  allocation cleared, CDI spec deleted
  └─────┬──────────────┘
        │
   from template → claim auto-deleted
   direct claim  → claim stays, available for reallocation
```

Plugin checkpoint at `/var/lib/wh-dra/checkpoint/` persists prepared state across restarts — if the plugin crashes, it knows what was prepared on recovery.

> **Speaker notes:** The lifecycle is why DRA gives you exclusive access guarantees. A device is "Allocated" the moment the scheduler writes to status — no two claims can allocate the same device on the same node simultaneously. The scheduler uses optimistic locking on the ResourceSlice to prevent races.

---

## Slide 17 — Device Sharing, Fallback Alternatives, and Observability

**Device sharing** — multiple containers in one Pod share the same claim:
```yaml
spec:
  resourceClaims:
  - name: shared-t3k
    resourceClaimTemplateName: wh-t3k-template
  containers:
  - name: trainer
    resources:
      claims: [{name: shared-t3k}]   # same /dev/tenstorrent/* injected here
  - name: monitor
    resources:
      claims: [{name: shared-t3k}]   # and here
```

**Prioritized list (K8s v1.36)** — try preferred device first, fallback if unavailable:
```yaml
spec:
  devices:
    requests:
    - name: t3k-preferred
      subrequests:
      - devices:  # try: pod_size=2
          requests: [{exactly: {deviceClassName: t3k.wormhole.tenstorrent.com,
            selectors: [{cel: {expression: "device.attributes['tenstorrent.com/pod_size'].int == 2"}}]}}]
      - devices:  # fallback: any T3K
          requests: [{exactly: {deviceClassName: t3k.wormhole.tenstorrent.com}}]
```

**DRA observability (K8s v1.36+ kubelet metrics):**
```
kubelet_dra_devices_allocated_total   — devices in use right now
kubelet_dra_devices_available_total   — devices free for allocation
kubelet_dra_devices_unhealthy_total   — devices marked unhealthy by driver
```

> **Speaker notes:** Prioritized lists solve the "I prefer an 8-chip cluster but will accept a 4-chip one" scheduling problem without hard-coding node names. The kubelet DRA metrics are exposed at `:10255/metrics` (read-only kubelet port) and can be scraped by Prometheus — useful for capacity planning dashboards.

---

## Slide 18 — The Four DRA Objects: What They Are and Where They Live

**Each object has one clear owner and one clear consumer:**

| Object | Who creates it | Who reads it |
|---|---|---|
| `DeviceClass` | Admin, once | Scheduler (CEL filter) + workload YAML |
| `ResourceSlice` | Plugin, at runtime | Scheduler (allocation decisions) |
| `ResourceClaim` | User YAML or K8s (from template) | Scheduler writes `status.allocation`; plugin reads it on Prepare |
| `ResourceClaimTemplate` | User, once per namespace | K8s controller (generates per-pod ResourceClaims) |

**Where each object lives in our repo:**

```
deploy/deviceclass.yaml                  ← DeviceClass (apply once)
deploy/odin/resourceclaimtemplate.yaml   ← ResourceClaimTemplate (apply per namespace)
deploy/test-claim.yaml                   ← ResourceClaim + Pod (smoke test)
deploy/test-two-pods.yaml                ← 2× ResourceClaim (exclusivity test)
deploy/multinode/test-statefulset-…yaml  ← ResourceClaimTemplate inline (2-node test)
deploy/vllm-qwen3-2node-dra.yaml         ← ResourceClaimTemplate inline (vLLM deployment)

cmd/wh-dra-kubelet-plugin/driver.go      ← publishes ResourceSlice (runtime, not a file)
internal/profiles/tenstorrent/…go        ← builds ResourceSlice Device struct (FM path)
cmd/wh-dra-kubelet-plugin/state.go       ← reads ResourceClaim.status.allocation on Prepare
```

**The DeviceClass CEL expression ties plugin and workload together:**
```
DeviceClass CEL:  device.driver == "wormhole.tenstorrent.com"
                                         ↑
                        must match driver.go:21  const driverName = "wormhole.tenstorrent.com"
```

> **Speaker notes:** DeviceClass and ResourceClaimTemplate are YAML files applied by humans. ResourceSlice is never a file — the plugin creates and owns it in the K8s API at runtime; it disappears when the plugin stops. ResourceClaim is created either by a human (`test-claim.yaml`) or automatically by Kubernetes when a pod references a ResourceClaimTemplate. The plugin's state.go only ever sees ResourceClaim — it never sees the template.

---

## Slide 19 — System Architecture

```
Control Plane (192.168.1.60)
  kube-apiserver ←── kubectl commands
  kube-scheduler ←── reads ResourceSlice, places pods
  etcd           ←── all state
  ttfm-controller (ttfm namespace) ←── agents register here

           │ Kubernetes API
     ┌─────┴──────┐
     │            │
 t3k-node-a    t3k-node-b
 .247           .243

 ┌─────────────────────────────────┐
 │ wh-node-labeler                 │
 │   reads tt-smi + ConfigMap      │
 │   sets node labels every 5 min  │
 │                                 │
 │ ttfm-agent  :50053 gRPC         │
 │   discovers all 8 chips via UMD │
 │   registers with controller     │
 │                                 │
 │ wh-dra-plugin                   │
 │   ① calls FM agent GetTopology  │
 │   ② publishes ResourceSlice     │
 │   ③ writes CDI spec files       │
 │   ④ PrepareResourceClaims()     │
 │   ⑤ health monitoring           │
 └─────────────────────────────────┘

 /dev/tenstorrent/0-3  (MMIO chips, PCIe to CPU)
 chips 4-7             (remote, via chip-to-chip links)

 workload pod (rank=0)          workload pod (rank=1)
```

> **Speaker notes:** Every component except the controller runs as a DaemonSet on each T3K node. The FM agent uses `internalTrafficPolicy: Local` so the plugin always reaches its own node's agent, never a remote one.

---

## Slide 20 — MMIO vs. Remote Chips

**A T3K board has 8 Wormhole chips. Only 4 have PCIe connections.**

```
Host CPU
  │ PCIe bus
  ├── chip 0  → /dev/tenstorrent/0   (MMIO capable)
  ├── chip 1  → /dev/tenstorrent/1
  ├── chip 2  → /dev/tenstorrent/2
  └── chip 3  → /dev/tenstorrent/3
       │ chip-to-chip links (not PCIe)
       ├── chip 4   (remote: no /dev entry)
       ├── chip 5   (remote: no /dev entry)
       ├── chip 6   (remote: no /dev entry)
       └── chip 7   (remote: no /dev entry)
```

**The plugin groups them:**
- 4 MMIO chips → `/dev/tenstorrent/0-3` injected via CDI
- 4 remote chips → reached through MMIO chip fds by the runtime library

**Before FM integration:** plugin saw 4 chips (only what `ls /dev/tenstorrent/` showed)
**After FM integration:** plugin sees all 8, publishes correct `chip_count=8` and `memory=96Gi`

> **Speaker notes:** The FM agent uses the UMD (User Mode Driver) API to open each MMIO chip and read its chip-to-chip connection table. This is how it discovers the 4 remote chips. When a workload is already holding a chip, FM can't open it via UMD and falls back to PCIe-only scan — which is why we call FM at plugin startup before any workload runs.

---

## Slide 21 — FM Integration: What the Plugin Does at Startup

```
Plugin pod starts on t3k-node-a
  │
  ├── 1. Read node labels from Kubernetes API
  │       arch, board_type, host_rank, pod_size
  │
  ├── 2. Dial FM agent at ttfm-agent.ttfm.svc.cluster.local:50053
  │       call GetTopology()
  │       ┌─ if FM ready: returns 8 ASICs
  │       │                4 MMIO + 4 remote
  │       │                MemoryBytes per chip
  │       │                PciAddress, ChipArch per chip
  │       │   → publish FM-enriched ResourceSlice ──────┐
  │       │                                              ↓
  │       └─ if FM NOT ready: publish label-based fallback
  │           start background goroutine, retry every 10s
  │           → upgrade ResourceSlice when FM answers
  │
  ├── 3. Write CDI common spec file
  │       /var/run/cdi/k8s.wormhole.tenstorrent.com-t3k-common.yaml
  │       (env vars + hugepages mount — same for every pod on this node)
  │
  └── 4. Start health monitoring goroutine (os.Open every 30s)
```

> **Speaker notes:** The 10-second retry loop solves the startup race: both the plugin pod and the FM agent pod start when the node boots. Whichever wins the race, the ResourceSlice ends up correct within 10 seconds. No pod restart needed.

---

## Slide 22 — What Gets Published in the ResourceSlice

```yaml
# kubectl get resourceslice t3k-node-a-wormhole.tenstorrent.com -o yaml
spec:
  nodeName: t3k-node-a
  driver: wormhole.tenstorrent.com
  pool:
    name: t3k-node-a
  devices:
  - name: wormhole-t3k
    attributes:
      tenstorrent.com/arch:              wormhole
      tenstorrent.com/chip_arch:         wormhole_b0    ← from FM
      tenstorrent.com/chip_count:        8              ← from FM
      tenstorrent.com/mmio_chip_count:   4              ← from FM
      tenstorrent.com/remote_chip_count: 4              ← from FM
      tenstorrent.com/remote_chip_ids:   "7,6,5,4"     ← from FM
      tenstorrent.com/pci_addresses:     "0000:00:10.0,..." ← from FM
      tenstorrent.com/physical_pod:      t3k-a          ← node label
      tenstorrent.com/host_rank:         0              ← node label
      tenstorrent.com/pod_size:          2              ← node label
    capacity:
      tenstorrent.com/memory:            96Gi           ← FM: 12GB × 8 chips
```

> **Speaker notes:** The scheduler can use any of these attributes in a CEL expression inside a DeviceClass. For example: `device.attributes["tenstorrent.com/pod_size"].int == 2` to only allocate devices from 2-node physical pods. The capacity field lets workloads request a minimum memory guarantee.

---

## Slide 23 — CDI: What Gets Injected into the Container

**Two spec files per node:**

**common spec** — written once at plugin startup, lasts forever:
```yaml
env:
  - TT_CHIP_COUNT=8
  - TT_MESH_HOST_RANK=0
  - TT_POD_SIZE=2
  - TT_PHYSICAL_POD=t3k-a
mounts:
  - /dev/hugepages-1G → /dev/hugepages-1G
  - /tmp/tt_logs      → /tmp/tt_logs
```

**per-claim spec** — written per-pod, deleted when pod exits:
```yaml
deviceNodes:
  - /dev/tenstorrent/0  (major=236, minor=0)
  - /dev/tenstorrent/1  (major=236, minor=1)
  - /dev/tenstorrent/2  (major=236, minor=2)
  - /dev/tenstorrent/3  (major=236, minor=3)
env:
  - WH_RESOURCE_CLAIM_UID=abc123
```

**In the container — zero pod spec needed:**
```bash
ls /dev/tenstorrent/     # 0 1 2 3
echo $TT_CHIP_COUNT      # 8
ls /dev/hugepages-1G     # present
```

> **Speaker notes:** CDI (Container Device Interface) is an open standard supported by containerd, cri-o, and podman. The plugin writes YAML files; containerd reads them at container start time. No runtime patching needed. The two-file split means common config is only written once; per-pod config is deleted precisely when the pod exits.

---

## Slide 24 — Full Request Flow: Step by Step

```
① kubectl apply -f my-workload.yaml
   API server stores: ResourceClaim (Pending) + Pod (Pending)

② kube-scheduler:
   reads ResourceSlice → finds wormhole-t3k on t3k-node-a
   allocates device → writes allocation to ResourceClaim.status
   binds Pod to t3k-node-a

③ kubelet on t3k-node-a:
   sees pod scheduled here
   calls plugin PrepareResourceClaims([my-claim])

④ Plugin PrepareResourceClaims:
   reads ResourceClaim.status.allocation.devices.results
   → device name = "wormhole-t3k"
   calls profile.DeviceNodePaths("wormhole-t3k")
   → ["/dev/tenstorrent/0", "1", "2", "3"]
   stats each path → major/minor numbers
   writes /var/run/cdi/k8s.wormhole.tenstorrent.com-t3k-<uid>.yaml
   saves state to checkpoint (crash safety)
   returns CDI device IDs to kubelet

⑤ kubelet → containerd: "start container, inject CDI IDs"

⑥ containerd reads both CDI specs:
   injects device nodes, env vars, hugepages, log dir
   starts container

⑦ Pod: Running. Zero privileged, zero hostPath.

⑧ Pod exits → kubelet calls UnprepareResourceClaims
   plugin deletes CDI claim spec
   removes from checkpoint
   ResourceClaim released → next pod can use device
```

> **Speaker notes:** Step ④ is where FM integration pays off: `DeviceNodePaths("wormhole-t3k")` returns exactly the `/dev/tenstorrent` paths for the MMIO chips discovered by FM, not just whatever happens to be in `/dev`. These are injected precisely — the container gets exactly what it needs and nothing else.

---

## Slide 25 — Health Monitoring

**Problem:** A crashed kernel driver or missing chip is invisible to the scheduler.

**Solution:** `os.Open` each `/dev/tenstorrent/N` every 30 seconds.

```
every 30s:
  for chip 0..N-1:
    open("/dev/tenstorrent/N")
      success → driver loaded, chip present
      ENOENT  → chip missing or driver crashed

  if status CHANGED:
    healthy   → publish full ResourceSlice
                → scheduler places new pods here again
    unhealthy → publish EMPTY ResourceSlice
                → scheduler stops placing new pods here
                → already running pods are not affected
```

**Log output:**
```
# On failure:
T3K health changed: healthy=false — chip 2 not accessible
published empty ResourceSlice on pool t3k-node-a (T3K unhealthy)

# On recovery:
T3K health changed: healthy=true — all 8 chips accessible
```

> **Speaker notes:** We tried `tt-smi -s` (a Python subprocess) first. It hangs indefinitely when the chip ethernet mesh is inconsistent — e.g. after one node reboots while the other is still up. `os.Open` is the same approach used by Google's TPU plugin: if the kernel driver file is accessible, the device is present. Zero subprocess overhead, cannot hang.

---

## Slide 26 — What Has Been Done

**Infrastructure**
- ✅ Kubernetes v1.35.0 cluster (Kubespray v2.31.0): control-plane + 2× T3K
- ✅ CDI enabled in containerd on both T3K nodes
- ✅ CI/CD: GitHub Actions → build image → Helm deploy via self-hosted runner
- ✅ Self-contained image (plugin + tt-smi baked in, no host dependencies)

**Plugin features**
- ✅ ResourceSlice publication — device advertising to scheduler
- ✅ CDI injection — devices, hugepages, env vars, log dir
- ✅ Health monitoring — `os.Open /dev/tenstorrent/N` every 30s
- ✅ Prometheus metrics `:9090/metrics`
- ✅ Crash-recovery checkpoint
- ✅ Automatic node labeling (`wh-node-labeler` DaemonSet)
- ✅ **FM integration** — calls `GetTopology()` at startup
- ✅ **Device bundling** — 4 MMIO + 4 remote grouped into one device
- ✅ **Memory capacity** — `96Gi` in ResourceSlice.Device.Capacity
- ✅ **Profiles abstraction** — clean `profiles.Profile` interface
- ✅ **FM startup retry** — if FM not ready at boot, retries every 10s

---

## Slide 27 — Tests Passed

| Test | Result | What it proves |
|---|---|---|
| `test-claim` | ✅ | Device injection works end-to-end |
| `test-two-pods` | ✅ | DRA exclusivity: only one pod at a time |
| Multinode StatefulSet | ✅ | 2 pods, 2 nodes, correct rank + devices |
| Scheduler stress test | ✅ | 10 jobs, 5/node, max 2 concurrent |
| FM topology export | ✅ | 8 ASICs on both nodes via UMD |
| ResourceSlice with FM data | ✅ | `chip_count=8`, `memory=96Gi`, `chip_arch=wormhole_b0` |

**Live ResourceSlice today (both nodes):**
```json
{
  "chip_count": 8,  "mmio_chip_count": 4,  "remote_chip_count": 4,
  "chip_arch": "wormhole_b0",
  "remote_chip_ids": "7,6,5,4",
  "pci_addresses": "0000:00:10.0,...",
  "physical_pod": "t3k-a",
  "host_rank": 0,   "pod_size": 2,
  "memory": "96Gi"
}
```

> **Speaker notes:** The ResourceSlice with FM data is the main milestone of this phase. Before FM integration the scheduler could see `chip_count=4` (only MMIO) and had no memory info. Now it sees the full picture.

---

## Slide 28 — Comparing to NVIDIA and Google TPU

| Feature | NVIDIA | Google TPU | Ours (current) |
|---|:---:|:---:|:---:|
| Device discovery + advertising | ✅ | ✅ | ✅ |
| CDI injection | ✅ | ✅ | ✅ |
| Health monitoring | ✅ | ✅ | ✅ |
| FM / interconnect topology | ✅ | ✅ | ✅ |
| Memory capacity in ResourceSlice | ✅ | ✅ | ✅ |
| Device bundling (host + remote chips) | ✅ | ✅ | ✅ |
| Peer hostname injection (multi-node) | ✅ | ✅ | ❌ |
| Deep hardware telemetry | ✅ (DCGM) | ✅ | ❌ |
| Graceful drain on SIGTERM | ✅ | ✅ | ❌ |
| Fault / error code monitoring | ✅ (XID) | ✅ | ❌ |
| Auto topology discovery (no ConfigMap) | ✅ | ✅ | ❌ |

> **Speaker notes:** The peer hostname injection gap is the blocker for multi-node vLLM. NVIDIA injects `NVIDIA_VISIBLE_DEVICES` + NVLink topology. TPU injects `TPU_WORKER_HOSTNAMES` (via CDI for single-host; GKE closed-source for multi-host). We need an equivalent `TT_WORKER_HOSTNAMES` injected at the cluster level — a webhook or controller, not PrepareResourceClaims (which only has per-node visibility).

---

## Slide 29 — How TPU Schedules Multiple Devices: The Architecture

**Key insight: two separate atomicity guarantees, not one.**

**Layer 1 — Infrastructure (before Kubernetes sees the nodes):**
```
GCP provisions a TPU slice node pool (e.g. v5e-2x4 = 4 hosts × 4 chips)
  → either ALL 4 VMs are created (wired together via ICI fabric), or NONE
  → physical connectivity is guaranteed at provisioning time
  → Kubernetes sees 4 already-connected nodes; scheduler doesn't need to reason about wiring
```

**Layer 2 — Kubernetes scheduling (per-pod ResourceClaims + affinity):**
```
ResourceClaimTemplate → one claim per pod (4 pods × 4 chips each = 16 total)
JobSet exclusive-topology annotation → all pods in one replica land on same node pool
  (node pool = physical slice = ICI boundary)
```

**What each node's ResourceSlice actually looks like** (verified from `driver.go:144`):
```yaml
# Each host publishes its OWN ResourceSlice keyed by NODE NAME (not a shared pool)
spec:
  driver: google.com/tpu
  pool:
    name: <node-name>          # ← node name, same as our current model
    resourceSliceCount: 1      # ← auto-computed by framework as len(pool.Slices)
  devices:
  - name: tpu-0
  ...
```

`resourceSliceCount` is set automatically by the DRA framework — not by the driver. The driver just puts devices into slices; the framework sets the count. For single-node pools it is always 1.

> **Speaker notes:** Key correction from code review: TPU does NOT use a shared pool name across multiple hosts. Each node uses its own node name as the pool key — identical to our current model. The GCP infrastructure atomicity (all-or-nothing node pool provisioning) is what guarantees connected nodes are available together, not a shared pool name. Our bare-metal equivalent is the `physical_pod` label in affinity rules.

---

## Slide 30 — How TPU Injects Peer Hostnames (and What We Need)

**What the open-source code actually shows** (verified from `util.go:412` and `cdi.go`):

```go
// Single-host path — injected via CDI during PrepareResourceClaims
func addSingleHostEnvs(envs map[string]string) {
    envs["TPU_WORKER_ID"] = "0"
    envs["TPU_WORKER_HOSTNAMES"] = "localhost"
}
```

For **single-host** deployments, `TPU_WORKER_HOSTNAMES=localhost` is injected through CDI —
the same mechanism we use. No mutating webhook exists in the open-source plugin.

**For multi-host (GKE closed-source):** Google's production GKE implementation handles this
server-side, but the code is not public. The architectural reason why CDI cannot do it for
multi-host is sound:

```
PrepareResourceClaims runs on ONE node's kubelet
  → only knows about the local device
  → cannot see other pods' IP addresses
  → cannot compute the full peer list

A cluster-level mechanism (webhook or controller) runs at Job admission
  → has full cluster view
  → knows all pod names before they are scheduled
  → can compute DNS names from Indexed Job naming convention
```

**What we need for T3K** (our design, same architectural reasoning):
```yaml
env:
- name: TT_WORKER_ID
  value: "0"
- name: TT_WORKER_HOSTNAMES
  value: "wh-t3k-worker-0.wh-t3k-headless,wh-t3k-worker-1.wh-t3k-headless"
```

**What we already have that helps:**
- `TT_ETHERNET_IFACE` injected via CDI today ✅
- `host_rank` and `pod_size` in CDI env vars ✅
- Headless Service in StatefulSet YAML → predictable DNS names ✅

**The gap:** no cluster-level mechanism to inject `TT_WORKER_HOSTNAMES` across nodes.

> **Speaker notes:** The open-source TPU DRA code does NOT contain a mutating webhook — only a ValidatingAdmissionPolicy. The multi-host hostname injection is handled by GKE infrastructure (closed-source). Our design (admission webhook for TT_WORKER_HOSTNAMES) is the right approach for the same architectural reasons, but it's our own design, not a copy of TPU.

---

## Slide 32 — What Is Next

**P1 — Goal 1: Moreh vLLM on Kubernetes**

| Step | Status |
|---|---|
| Fix `board_type=unknown` (tt-smi ANSI escape codes) | 🔲 code fix needed |
| 1-node vLLM end-to-end test | 🔲 todo |
| Peer hostname injection (`TT_WORKER_HOSTNAMES`) | 🔲 design needed |
| 2-node vLLM end-to-end test | 🔲 blocks on peer injection |
| Validate with Huginn (endpoint quality) | 🔲 todo |

**P2 — Hardening**

| Step | Status |
|---|---|
| Deep telemetry (temp, power, utilization → Prometheus) | 🔲 |
| Graceful drain on SIGTERM (empty slice, wait for pods) | 🔲 |
| Automatic topology discovery (from FM ExitNodeInfo) | 🔲 |

> **Speaker notes:** `board_type=unknown` is a quick fix: tt-smi outputs ANSI color codes (escape sequences like `\x1b[0m`) when run non-interactively, breaking JSON parsing. The fix is to strip ANSI sequences before parsing. Peer hostname injection is harder: it requires knowing the IP of the peer pod before that pod has started, which may require a new K8s object or a sidecar that updates a ConfigMap/env after scheduling.

---

## Slide 31 — Demo (live)

```bash
# 1. Cluster health
kubectl get nodes
# control-plane-01   Ready   2d
# t3k-node-a         Ready   2d
# t3k-node-b         Ready   2d

# 2. FM-enriched ResourceSlices
kubectl get resourceslices
kubectl get resourceslice -o json | \
  jq '.items[] | {node: .spec.nodeName, attrs: .spec.devices[0].attributes, cap: .spec.devices[0].capacity}'
# chip_count=8, memory=96Gi on both nodes

# 3. Plugin and FM agent running
kubectl -n kube-system get pods -l app=wh-dra-kubelet-plugin -o wide
kubectl -n ttfm get pods -o wide
kubectl -n ttfm logs <ttfm-agent-node-a> | grep "physical topology"
# 8 ASICs, 40 local connections, UMD mode

# 4. Single-node smoke test
kubectl apply -f deploy/test-claim.yaml
kubectl get pods -w
kubectl logs wh-demo-pod
# /dev/tenstorrent/0-3, TT_CHIP_COUNT=8
kubectl delete -f deploy/test-claim.yaml

# 5. Exclusivity test
kubectl apply -f deploy/test-two-pods.yaml
kubectl get pods -w
# pod-a Running, pod-b Pending → after pod-a exits, pod-b starts
kubectl delete -f deploy/test-two-pods.yaml

# 6. Multinode StatefulSet
kubectl apply -f deploy/multinode/test-statefulset-two-t3k.yaml
kubectl get pods -l app=wh-t3k-worker -o wide
# wh-t3k-worker-0 → t3k-node-a (rank=0)
# wh-t3k-worker-1 → t3k-node-b (rank=1)
kubectl delete -f deploy/multinode/test-statefulset-two-t3k.yaml
```

---

## Slide 33 — Q&A

**Key takeaways:**

1. Kubernetes manages where workloads run — we made T3K hardware schedulable
2. DRA = structured device metadata + automatic injection, no privileged containers
3. FM integration gives the scheduler the full picture: 8 chips, 96 GB memory, chip topology
4. The cluster is live — both nodes publishing FM-sourced ResourceSlices today
5. Next blocker: peer hostname injection for multi-node vLLM

*Thank you*

---

*Source: [github.com/tenstorrent/wh-dra-plugin](https://github.com/tenstorrent/wh-dra-plugin)*
