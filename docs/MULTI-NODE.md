# Multi-Node T3K: Chip-to-Chip Experiment and Galaxy Preparation

> Design document for running a 2-node T3K workload over chip-to-chip Tenstorrent Ethernet,
> using Kubernetes DRA with no MPI dependency. Findings apply directly to the Galaxy target
> topology described in REQUIREMENTS.md.

---

## 1. Background

### Current setup (host Ethernet)

```
node-a [T3K] ── host NIC ── switch ── host NIC ── [T3K] node-b
                (TCP/IP, host-managed)
```

Each T3K is an independent device. The plugin publishes one ResourceSlice per node with a
unique pool name. Two pods, each claiming one T3K, run independently and communicate via
the host network.

### Experiment target (chip-to-chip Ethernet)

```
node-a [T3K] ── TT Ethernet port ──── TT Ethernet port ── [T3K] node-b
                (chip-to-chip, hardware-managed)
```

The 8 chips on node-a and the 8 chips on node-b are directly wired together via Tenstorrent
Ethernet ports. This forms one 16-chip logical device — the same topology as an 8-node Galaxy
pod at smaller scale. All findings from this experiment transfer directly to Galaxy.

---

## 2. Why Still Two Pods?

Even with chip-to-chip Ethernet, `/dev/tenstorrent` files are **local PCI devices** on each
host. A pod running on node-a cannot open `/dev/tenstorrent` on node-b — those files do not
exist on node-a's filesystem.

```
Pod A (node-a)                    Pod B (node-b)
  /dev/tenstorrent/0-3              /dev/tenstorrent/0-3
  (local PCI chips)                 (local PCI chips)
         │                                 │
         └──────── TT Ethernet ────────────┘
                   (chip-to-chip)
                   tt-metal handles this at the library level
```

Each pod sees only its local chips via `/dev`. The chip-to-chip link is a hardware
communication channel — the host OS does not participate. This is identical to how Google TPU
multi-host works: one pod per host, ICI handles cross-host chip communication.

---

## 3. How tt-run Works (and Why We Don't Need It)

`tt-run` is a thin wrapper around `mpirun`. Its only job is to set three environment variables
per rank before launching the program:

| Variable | Purpose |
|---|---|
| `TT_MESH_ID` | Which mesh this process belongs to |
| `TT_HOST_RANK` | This host's position within the mesh |
| `TT_MESH_GRAPH_DESC_PATH` | Path to the topology descriptor YAML |

tt-metal reads these via plain `getenv()` in `control_plane.cpp` — MPI does not own them.
If these variables are already set in the container environment, the program does not need
`tt-run` or `mpirun` at all.

### The one thing MPI actually does

At startup, each host reads its chips' Ethernet port state (local board ID, remote board ID,
channel ID) from hardware. It then calls `exchange_intermesh_link_tables()` to broadcast this
information to all other hosts so everyone builds a complete routing table.

```cpp
// control_plane.cpp:1931 — skipped when size() == 1
if (*distributed_context.size() == 1) {
    return;
}
```

For our single-mesh 2-host configuration (`Graph: []` — no inter-mesh links), this table is
empty. The broadcast is a no-op and is skipped entirely when `SingleHostContext` (no MPI) is
used. **MPI is not required for our use case.**

---

## 4. Mesh Graph Descriptor

No existing descriptor covers 2 T3K hosts as one mesh. We need a custom file.

**Constraint enforced by tt-metal:**
```
device_topology == host_topology × board_topology
```

For T3K (`board_topology = [2, 4]`) with 2 hosts:

```
device_topology = [1, 2] × [2, 4] = [2, 8]   (16 chips total)
```

### `deploy/mesh-graph/t3k_2host_mesh_graph.yaml`

```yaml
ChipSpec: {
  arch: wormhole_b0,
  ethernet_ports: { N: 2, E: 2, S: 2, W: 2 }
}

Board: [
  { name: T3K, type: Mesh, topology: [2, 4] }
]

Mesh: [
  {
    id: 0,
    board: T3K,
    device_topology: [2, 8],   # 2 rows × 8 cols = 16 chips
    host_topology:   [1, 2],   # 1 row × 2 hosts
    host_ranks:      [[0, 1]]  # rank 0 = node-a (W), rank 1 = node-b (E)
  }
]

Graph: []   # no inter-mesh links — both T3Ks are one mesh
```

`Graph` is empty because intra-mesh chip connections are discovered automatically at runtime
by reading Ethernet port state from the chips. Only cross-mesh links (like TG gateway→Galaxy)
need explicit `Graph` entries.

---

## 5. Plugin Changes Required

### 5.1 `manager.go` — rename and add env vars

`CommonEnvs()` currently injects `TT_MESH_HOST_RANK` but tt-metal reads `TT_HOST_RANK`.
Two additions needed:

```go
func (m *WHManager) CommonEnvs() []string {
    envs := []string{
        fmt.Sprintf("TT_HOST_RANK=%d", m.hostRank),                      // renamed from TT_MESH_HOST_RANK
        fmt.Sprintf("TT_MESH_ID=0"),                                      // add: mesh 0 for single-pod configs
        fmt.Sprintf("TT_METAL_CACHE=/tmp/tt-metal-cache-%d", m.hostRank), // add: per-rank, avoids collision
        fmt.Sprintf("TT_POD_SIZE=%d", m.podSize),
        fmt.Sprintf("TT_PHYSICAL_POD=%s", m.physicalPod),
    }
    if m.ethernetIface != "" {
        envs = append(envs, fmt.Sprintf("TT_ETHERNET_IFACE=%s", m.ethernetIface))
    }
    return envs
}
```

### 5.2 `driver.go` — shared pool + `resourceSliceCount`

For chip-to-chip connected nodes, publish under a shared pool name so the scheduler treats
both nodes as one logical device:

```go
// Multi-node mode: pool name = physicalPod, Count = podSize
// Single-node mode: pool name = nodeName, Count = 0 (no constraint)
poolName := m.nodeName
var poolCount int64 = 0
if m.podSize > 1 {
    poolName = m.physicalPod   // "t3k-pod-0" — same on both nodes
    poolCount = int64(m.podSize)
}

Pools: map[string]resourceslice.Pool{
    poolName: {
        Count:  poolCount,   // resourceSliceCount — scheduler waits for all slices
        Slices: []resourceslice.Slice{{Devices: []resourceapi.Device{device}}},
    },
}
```

`resourceSliceCount` is a **plugin-side field** — it does not appear in user YAML. Its effect:
if node-b's plugin is down, the pool is incomplete and the scheduler blocks all allocations
until both nodes are healthy. Without it, a half-visible pool could cause one pod to schedule
while the other hangs.

---

## 6. Kubernetes YAML

### 6.1 ConfigMap — mesh graph descriptor

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: t3k-2host-mesh-graph
  namespace: default
data:
  mesh_graph.yaml: |
    ChipSpec: {
      arch: wormhole_b0,
      ethernet_ports: { N: 2, E: 2, S: 2, W: 2 }
    }
    Board: [
      { name: T3K, type: Mesh, topology: [2, 4] }
    ]
    Mesh: [
      {
        id: 0,
        board: T3K,
        device_topology: [2, 8],
        host_topology:   [1, 2],
        host_ranks:      [[0, 1]]
      }
    ]
    Graph: []
```

### 6.2 ResourceClaimTemplate — one device per pod, pinned to the chip-to-chip pair

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata:
  name: t3k-2host-claim-template
  namespace: default
spec:
  spec:
    devices:
      requests:
      - name: t3k
        exactly:
          deviceClassName: t3k.wormhole.tenstorrent.com
          selectors:
          - cel:
              expression: >
                device.attributes["tenstorrent.com/physical_pod"].string == "t3k-pod-0"
```

The CEL selector ensures both pods can only land on nodes in the `t3k-pod-0` physical pod —
the pair that is chip-to-chip connected.

### 6.3 Headless Service — peer DNS

```yaml
apiVersion: v1
kind: Service
metadata:
  name: t3k-2host-svc
  namespace: default
spec:
  clusterIP: None   # headless
  selector:
    job-name: t3k-2host-job
  ports:
  - name: tt-fabric
    port: 22300
```

### 6.4 Job

```yaml
apiVersion: batch/v1
kind: Job
metadata:
  name: t3k-2host-job
  namespace: default
spec:
  completionMode: Indexed   # pods get JOB_COMPLETION_INDEX=0 and 1
  completions: 2
  parallelism: 2

  # Any pod failure → terminate all pods immediately (replaces MPI_Abort)
  podFailurePolicy:
    rules:
    - action: FailJob
      onExitCodes:
        operator: NotIn
        values: [0]

  template:
    metadata:
      labels:
        job-name: t3k-2host-job
    spec:
      restartPolicy: Never
      subdomain: t3k-2host-svc

      # Each pod gets its own ResourceClaim from the template
      resourceClaims:
      - name: t3k-device
        resourceClaimTemplateName: t3k-2host-claim-template

      affinity:
        # Only nodes in the chip-to-chip connected pair
        nodeAffinity:
          requiredDuringSchedulingIgnoredDuringExecution:
            nodeSelectorTerms:
            - matchExpressions:
              - key: tenstorrent.com/physical-pod
                operator: In
                values: ["t3k-pod-0"]
        # Prevent both pods landing on the same node
        podAntiAffinity:
          requiredDuringSchedulingIgnoredDuringExecution:
          - topologyKey: kubernetes.io/hostname
            labelSelector:
              matchLabels:
                job-name: t3k-2host-job

      volumes:
      - name: mesh-graph
        configMap:
          name: t3k-2host-mesh-graph

      # Wait for all peers to be reachable before starting (replaces MPI barrier)
      initContainers:
      - name: wait-for-peers
        image: busybox:1.36
        command:
        - sh
        - -c
        - |
          for host in $(echo $TT_WORKER_HOSTNAMES | tr ',' ' '); do
            until nc -z "$host" 22300; do
              echo "waiting for $host on port 22300..."; sleep 2
            done
            echo "$host is ready"
          done
        env:
        - name: TT_WORKER_HOSTNAMES
          value: "injected-by-webhook"   # mutating webhook replaces at admission time

      containers:
      - name: tt-workload
        image: my-tt-workload:latest
        resources:
          claims:
          - name: t3k-device   # triggers CDI injection of /dev paths + env vars

        env:
        # Static — same on both pods
        - name: TT_MESH_GRAPH_DESC_PATH
          value: /etc/tenstorrent/mesh_graph.yaml
        #
        # Injected by CDI (PrepareResourceClaims, per-node):
        #   TT_HOST_RANK=0 or 1       (from manager.hostRank)
        #   TT_MESH_ID=0              (mesh 0 for single-pod configs)
        #   TT_METAL_CACHE=/tmp/tt-metal-cache-<rank>
        #   TT_ETHERNET_IFACE=<iface>
        #   /dev/tenstorrent/0-3      (device nodes)
        #
        # Injected by mutating webhook (at Job admission time):
        #   TT_WORKER_HOSTNAMES=<pod-a-ip>,<pod-b-ip>

        volumeMounts:
        - name: mesh-graph
          mountPath: /etc/tenstorrent
          readOnly: true
```

---

## 7. How Each Option B Con is Solved

| Con | Solution in this design |
|---|---|
| **tt-run compatibility** | Not needed — env vars set by CDI. App calls tt-metal API directly. |
| **Startup synchronization** | `initContainers: wait-for-peers` polls peer IPs until reachable. |
| **Multi-process abort** | `podFailurePolicy: FailJob` — K8s kills all pods on any failure. |
| **TT_METAL_CACHE collisions** | CDI injects `/tmp/tt-metal-cache-<rank>` per node (rank differs per node). |

No MPI, no MPI Operator, no launcher pod, no SSH inside containers.

---

## 8. Environment Variable Flow

```
                  node-a                              node-b
          ┌───────────────────┐              ┌───────────────────┐
          │  DRA plugin       │              │  DRA plugin       │
          │  manager.hostRank=0              │  manager.hostRank=1
          │  manager.physPod ="t3k-pod-0"   │  manager.physPod ="t3k-pod-0"
          └────────┬──────────┘              └────────┬──────────┘
                   │ CDI injection                    │ CDI injection
                   ▼                                  ▼
          Pod A container env:              Pod B container env:
            TT_HOST_RANK=0                   TT_HOST_RANK=1
            TT_MESH_ID=0                     TT_MESH_ID=0
            TT_METAL_CACHE=.../0             TT_METAL_CACHE=.../1
            TT_ETHERNET_IFACE=eth1           TT_ETHERNET_IFACE=eth1
            /dev/tenstorrent/0-3             /dev/tenstorrent/0-3
                   │                                  │
                   └──── mutating webhook ────────────┘
                          injects TT_WORKER_HOSTNAMES=<ip-a>,<ip-b>
                          into both pods at Job admission time
                   │                                  │
                   └──── static (pod spec) ───────────┘
                          TT_MESH_GRAPH_DESC_PATH=/etc/tenstorrent/mesh_graph.yaml
```

---

## 9. What This Experiment Teaches for Galaxy

| Learning | Galaxy application |
|---|---|
| TT_HOST_RANK + TT_MESH_ID injected by CDI works | Same injection, podSize=8 |
| Mesh graph descriptor describes multi-host topology | Galaxy descriptor: `host_topology: [1,8]`, `device_topology: [8,32]` |
| `resourceSliceCount=podSize` prevents half-pool allocation | `Count=8` for Galaxy 8-node pod |
| CEL selector on `physical_pod` pins pods to right nodes | Same selector, different pod name |
| `podFailurePolicy: FailJob` propagates crashes cleanly | Same policy, 8 pods |
| `TT_WORKER_HOSTNAMES` webhook scales to N hosts | Webhook injects 8 hostnames |
| MPI not needed for single-mesh multi-host | Same conclusion for Galaxy single-pod |

The 2-node T3K experiment is a Galaxy pod at 1/4 scale. Every design decision made here
applies directly when `podSize` grows from 2 to 8.

---

## 10. Prerequisites Before Running

1. **Physical cables**: Tenstorrent Ethernet cables connecting T3K board ports between node-a and node-b.
2. **Node labels**: Both nodes must have `tenstorrent.com/physical-pod=t3k-pod-0` set in the `tt-node-topology` ConfigMap.
3. **Plugin rebuilt** with the `CommonEnvs()` and pool name changes from section 5.
4. **Mutating webhook deployed** for `TT_WORKER_HOSTNAMES` injection (same webhook as current Odin integration).
5. **Workload image** compiled with tt-metal without `OPEN_MPI` (uses `SingleHostContext`), or with `OPEN_MPI` but the workload only uses the chip-to-chip fabric for data movement (not host-level MPI collectives).
