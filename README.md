# wh-dra-kubelet-plugin

Kubernetes DRA (Dynamic Resource Allocation) plugin for Tenstorrent Wormhole hardware (n300, T3K).

Each Wormhole node is published as a single allocatable device. When a pod claims it, the plugin injects `/dev/tenstorrent/*` device nodes and mesh environment variables into the container via CDI — no `privileged: true` required in workload pods.

## Hardware topology

This plugin targets **T3K** units — each T3K is one host with an n300 board:

- 4 PCIe-local chips (`/dev/tenstorrent/0..3`, char major 236)
- 4 remote chips reachable via on-board Ethernet fabric
- Total: 8 Wormhole chips per T3K

Multiple T3K units can be linked externally (see [Multi-node T3K](#multi-node-t3k)).

## Prerequisites

- Kubernetes ≥ v1.35 (DRA stable — enabled by default in k3s v1.35+)
- `tt-kmd` 2.7.0+ kernel driver loaded (`/dev/tenstorrent/` must exist)
- Go ≥ 1.26 (for building)
- 1 GiB hugepages allocated on the host (required by Wormhole firmware for remote chip access)

Verify hugepages:
```bash
cat /sys/kernel/mm/hugepages/hugepages-1048576kB/nr_hugepages  # must be > 0
ls /dev/hugepages-1G                                            # must exist
```

## Step 1 — Label each Wormhole node

```bash
kubectl label node <node-name> \
  tenstorrent.com/arch=wormhole \
  tenstorrent.com/board-type=n300 \
  tenstorrent.com/chip-count=4 \
  tenstorrent.com/physical-pod=t3k-a \
  tenstorrent.com/host-rank=0 \
  tenstorrent.com/pod-size=1
```

| Label | Example | Meaning |
|---|---|---|
| `arch` | `wormhole` | Hardware architecture — used as DaemonSet nodeSelector |
| `board-type` | `n300` | Board form factor |
| `chip-count` | `4` | Number of PCIe-local chips; validated against `/dev/tenstorrent/` at startup |
| `physical-pod` | `t3k-a` | Identifies which T3K unit this host belongs to |
| `host-rank` | `0` | Host rank within the T3K unit (cable order) |
| `pod-size` | `1` | Number of T3K units in this logical allocation group |

## Step 2 — Build

The plugin is automatically built and pushed to `ghcr.io/slifersky/wh-dra-plugin:latest` by GitHub Actions on every push to `main`. No manual build or image import is needed — nodes pull the image directly.

For local development only:

```bash
export PATH=$PATH:/home/ubuntu/go/bin
export GOPATH=/home/ubuntu/gopath

cd /home/ubuntu/wh-dra-plugin
go build -o bin/wh-dra-kubelet-plugin ./cmd/wh-dra-kubelet-plugin
```

## Step 3 — Run locally (development/testing)

Runs the plugin binary directly on the host, without Kubernetes. Useful for iterating.

```bash
sudo chmod 644 /etc/rancher/k3s/k3s.yaml

./bin/wh-dra-kubelet-plugin \
  --node-name=$(hostname) \
  --kubeconfig=/etc/rancher/k3s/k3s.yaml \
  --cdi-dir=/var/run/cdi \
  --checkpoint-dir=/var/lib/wh-dra/checkpoint \
  --plugin-dir=/var/lib/kubelet/plugins/wormhole.tenstorrent.com \
  --registrar-dir=/var/lib/kubelet/plugins_registry
```

Verify the device is published:

```bash
kubectl get resourceslice
kubectl get resourceslice -o yaml
```

Expected: one ResourceSlice named `<node>-wormhole.tenstorrent.com-<suffix>` containing device `wormhole-t3k` with attributes.

## Step 4 — Deploy as DaemonSet

The DaemonSet pulls `ghcr.io/slifersky/wh-dra-plugin:latest` automatically from ghcr.io — no manual image import required on each node.

```bash
# Apply RBAC, DeviceClass, node labeler, and DaemonSet
kubectl apply -f deploy/rbac.yaml
kubectl apply -f deploy/deviceclass.yaml
kubectl apply -f deploy/node-labeler.yaml
kubectl apply -f deploy/daemonset.yaml

# Check
kubectl -n kube-system get pods -l app=wh-dra-kubelet-plugin
kubectl -n kube-system logs -l app=wh-dra-kubelet-plugin
```

The DaemonSet runs only on nodes labeled `tenstorrent.com/arch=wormhole`.

## Step 5 — Smoke test: DRA device injection

Verifies the plugin publishes a device, kubelet calls PrepareResourceClaims, and the pod receives the injected device nodes and env vars.

```bash
kubectl apply -f deploy/test-claim.yaml
kubectl wait --for=condition=Ready pod/wh-demo-pod --timeout=60s
kubectl logs wh-demo-pod
kubectl delete -f deploy/test-claim.yaml
```

Expected output:
```
=== /dev/tenstorrent/ devices ===
crw-rw-rw- 1 root root 236, 0 ... 0
crw-rw-rw- 1 root root 236, 1 ... 1
crw-rw-rw- 1 root root 236, 2 ... 2
crw-rw-rw- 1 root root 236, 3 ... 3

=== injected env vars ===
TT_CHIP_COUNT=4
TT_MESH_HOST_RANK=0
TT_PHYSICAL_POD=t3k-a
TT_POD_SIZE=1
WH_RESOURCE_CLAIM_UID=<uid>

=== DRA demo OK ===
```

## Step 6 — Hardware test: ttnn on DRA-allocated device

Runs a real ttnn computation on the Wormhole silicon inside a DRA-allocated pod.

**One-time setup** — import the image into k3s containerd:
```bash
docker save npu-metal-llk:latest | sudo k3s ctr images import -
sudo k3s ctr images list | grep npu-metal-llk
```

**Run the test:**
```bash
kubectl apply -f deploy/test-ttnn.yaml
kubectl wait --for=condition=Ready pod/wh-ttnn-test --timeout=120s
kubectl logs wh-ttnn-test
kubectl delete -f deploy/test-ttnn.yaml
```

Expected output:
```
=== DRA-injected devices ===
crw-rw-rw- 1 root root 236, 0 ... 0
crw-rw-rw- 1 root root 236, 1 ... 1
crw-rw-rw- 1 root root 236, 2 ... 2
crw-rw-rw- 1 root root 236, 3 ... 3

=== DRA-injected env vars ===
TT_CHIP_COUNT=4
TT_MESH_HOST_RANK=0
TT_PHYSICAL_POD=t3k-a
TT_POD_SIZE=1
WH_RESOURCE_CLAIM_UID=<uid>

=== Running ttnn hardware test ===
Opening Wormhole device 0 via ttnn...
Device opened: MeshDevice(1x1 grid, 1 devices)
ttnn.add([1,2,3], [4,5,6]) = [5.0, 7.0, 9.0]
Device closed.
SUCCESS: Wormhole hardware verified via DRA-allocated pod
```

DRA injects the `/dev/tenstorrent/*` device nodes, `/dev/hugepages-1G`, and env vars into the container via CDI — no `privileged: true` and no `hostPath` volume needed in the pod spec. The CDI spec includes the device `type`, `major`, `minor`, and `permissions` fields so containerd updates the cgroup device allowlist directly, equivalent to `docker run --device /dev/tenstorrent`.

## Project layout

```
cmd/wh-dra-kubelet-plugin/
  main.go        # Flags, kubeconfig/in-cluster config, Prometheus, signal handling
  driver.go      # Plugin startup, ResourceSlice publication, health-triggered republication
  manager.go     # Reads node labels, walks /dev/tenstorrent/, stats each device node (type/major/minor)
  state.go       # PrepareResourceClaims / UnprepareResourceClaims
  cdi.go         # Writes CDI YAML spec files to /var/run/cdi/
  health.go      # Periodic os.Open health checker (checks /dev/tenstorrent/N)
  checkpoint.go  # Crash recovery: checkpoint.json with boot ID validation
pkg/
  flock/         # Cross-process file lock (safe during rolling DaemonSet updates)
  bootid/        # Reads /proc/sys/kernel/random/boot_id
  metrics/       # Prometheus metrics (6 counters/gauges/histograms)
deploy/
  rbac.yaml          # ServiceAccount, ClusterRole, ClusterRoleBinding
  deviceclass.yaml   # DeviceClass selecting wormhole.tenstorrent.com devices
  daemonset.yaml     # DaemonSet (nodeSelector: tenstorrent.com/arch=wormhole)
  test-claim.yaml    # Smoke test: ResourceClaim + Pod (device injection only)
  test-ttnn.yaml     # Hardware test: ResourceClaim + Pod running real ttnn ops
  test-two-jobs.yaml # Exclusivity test: two Jobs + ResourceClaimTemplate (recommended pattern)
  multinode/
    node-labels.sh              # Label 2 T3K nodes (isolated or connected)
    test-statefulset-two-t3k.yaml  # 2 independent workers via StatefulSet
    test-mpi-two-t3k.yaml          # 2 coordinated workers via MPIJob (needs MPI Operator)
  odin/
    node-labels.sh                       # Add moai.moreh.io labels for Odin node selection
    resourceclaimtemplate.yaml           # ResourceClaimTemplate (apply per workload namespace)
    inferenceservicetemplate-1node.yaml  # Odin preset: 1 T3K, 8 chips (Deployment)
    inferenceservicetemplate-2node.yaml  # Odin preset: 2 T3K, 16 chips (LWS, data=2)
    inferenceservicetemplate-4node.yaml  # Odin preset: 4 T3K, 32 chips (LWS, data=4)
    inferenceservicetemplate-8node.yaml  # Odin preset: 8 T3K, 64 chips (LWS, data=8)
    example-inferenceservice.yaml        # Example InferenceService for all 4 sizes
```

## Step 7 — Exclusivity test: two Jobs, one device

Demonstrates that DRA enforces resource exclusivity — only one pod can hold the T3K device at a time. Uses `ResourceClaimTemplate` inside `Job` objects so claim lifecycle is fully automatic.

```bash
kubectl apply -f deploy/test-two-jobs.yaml
```

Expected progression:

```
# Immediately after apply — one job gets the device, the other is blocked:
NAME             STATUS      CLAIM                      STATE
wh-job-a-r9qnh   Running     wh-job-a-r9qnh-t3k-*      allocated,reserved  ← holds device (30s)
wh-job-b-l4hjc   Pending     wh-job-b-l4hjc-t3k-*      pending             ← blocked, no device

# job-a finishes → ttlSecondsAfterFinished:10 → pod deleted → claim GC'd automatically
# job-b gets the device and runs:
NAME             STATUS      CLAIM
wh-job-b-l4hjc   Completed   wh-job-b-l4hjc-t3k-*      allocated,reserved

# job-b finishes → TTL → pod deleted → claim GC'd automatically
# No manual cleanup needed.
```

Each job pod gets a unique auto-named claim (e.g. `wh-job-a-<id>-t3k-<suffix>`), so claim names are never reused — no ghost UIDs, no manual intervention.

Cleanup:
```bash
kubectl delete -f deploy/test-two-jobs.yaml
```

### How ResourceClaimTemplate works

```
ResourceClaimTemplate  ←  shared template, defines what device to request
       │
       ├── Job wh-job-a  →  pod wh-job-a-<id>  →  auto-creates claim wh-job-a-<id>-t3k-<suffix>
       └── Job wh-job-b  →  pod wh-job-b-<id>  →  auto-creates claim wh-job-b-<id>-t3k-<suffix>
```

Each auto-created claim is owned by its pod (ownerReference). When the pod is deleted (by TTL or manually), Kubernetes garbage-collects the claim. No force-deletes needed, no stale state left behind.

## How it works

1. **Startup**: plugin reads node labels, walks `/dev/tenstorrent/`, validates chip count, then calls `kubeletplugin.Start()` and publishes a `ResourceSlice` with one device (`wormhole-t3k`) and its attributes.
2. **Pod scheduling**: scheduler finds a node whose ResourceSlice satisfies the pod's ResourceClaim and binds them.
3. **PrepareResourceClaims**: kubelet calls the plugin before starting the container. Plugin writes a per-claim CDI spec file (`/var/run/cdi/k8s.wormhole.tenstorrent.com-t3k-<claimUID>.yaml`) listing the `/dev/tenstorrent/*` device nodes. State is checkpointed to survive plugin restarts.
4. **Container start**: containerd reads CDI spec files, injects the device nodes, `/dev/hugepages-1G`, and env vars into the container. Device allowlist is updated via `type/major/minor/permissions` — equivalent to `docker run --device`. No `privileged: true` needed in the pod spec.
5. **UnprepareResourceClaims**: kubelet calls after the pod exits. Plugin deletes the per-claim CDI file and removes the claim from the checkpoint.
6. **Health monitoring**: a background goroutine calls `os.Open("/dev/tenstorrent/N")` for every chip every `--health-check-interval` (default 30 s). If any chip is inaccessible, the plugin republishes the ResourceSlice with an empty device list so the scheduler stops placing new workloads on this node. The slice is restored to normal once all chips are accessible again. This approach avoids spawning `tt-smi` (a Python process that can hang when the Ethernet mesh is in an inconsistent state).

## CDI spec files

Two files are written to `/var/run/cdi/`:

| File | Written | Contents |
|---|---|---|
| `k8s.wormhole.tenstorrent.com-t3k-common.yaml` | Once at startup | Node-level env vars (`TT_MESH_HOST_RANK`, `TT_CHIP_COUNT`, etc.) + `/dev/hugepages-1G` bind mount + `/tmp/tt_logs` bind mount |
| `k8s.wormhole.tenstorrent.com-t3k-<claimUID>.yaml` | Per PrepareResourceClaims | `/dev/tenstorrent/0..N` device nodes (with `type`, `major`, `minor`, `permissions: rw`) + `WH_RESOURCE_CLAIM_UID` |

## Environment variables injected into pods

| Variable | Example | Source |
|---|---|---|
| `TT_MESH_HOST_RANK` | `0` | `host-rank` node label |
| `TT_CHIP_COUNT` | `4` | `chip-count` node label |
| `TT_POD_SIZE` | `1` | `pod-size` node label |
| `TT_PHYSICAL_POD` | `t3k-a` | `physical-pod` node label |
| `TT_ETHERNET_IFACE` | `cnx1` | `ethernet-iface` node label (optional) |
| `WH_RESOURCE_CLAIM_UID` | `<uid>` | Kubernetes ResourceClaim UID |

## Troubleshooting

**Pod stuck in `ContainerCreating` with `FailedPrepareDynamicResources`**

Caused by force-deleting a pod that held a ResourceClaim. The kubelet retains the old claim UID in its on-disk state (`claim_info_state`). When a new claim with the same name but a different UID is created, the kubelet rejects it.

Prevention: use `ResourceClaimTemplate` inside a `Job` (as in `test-two-jobs.yaml`). Each pod gets a unique auto-named claim — the same name is never reused, so UID conflicts cannot occur.

Recovery if it happens with bare claims:
```bash
# Clear the kubelet's persisted DRA state and restart (requires sudo):
sudo rm -f /var/lib/kubelet/plugins/wormhole.tenstorrent.com/claim_info_state
sudo systemctl restart k3s
sudo chmod 644 /etc/rancher/k3s/k3s.yaml
```

**Node tainted `disk-pressure:NoSchedule`**

k3s containerd images take significant disk space. Free Docker volumes and build cache:
```bash
docker volume prune -f
docker builder prune -f
docker image prune -f
```
The taint clears automatically once the kubelet sees disk above its eviction threshold (~15% free).

**`failed to initialize FW! Try resetting the board`**

Another process left the chip in a bad state. Reload the kernel driver on the affected T3K node:
```bash
sudo modprobe -r tenstorrent && sudo modprobe tenstorrent
```

## Multi-node T3K

### Two isolated T3K units (no external Ethernet link)

Each T3K is an independent device on its own Kubernetes node. Workers communicate via regular pod networking.

**Step 1 — label both nodes:**
```bash
bash deploy/multinode/node-labels.sh <node-a> <node-b>
```

This sets `physical-pod=t3k-a` and `physical-pod=t3k-b` on the two nodes. The DaemonSet picks up new nodes automatically (nodeSelector: `tenstorrent.com/arch=wormhole`).

**Step 2 — choose a workload pattern:**

| File | When to use |
|---|---|
| `deploy/multinode/test-statefulset-two-t3k.yaml` | Independent workers, no MPI needed |
| `deploy/multinode/test-mpi-two-t3k.yaml` | Coordinated MPI job (requires MPI Operator) |

The scheduler places each worker pod on a separate T3K node automatically — no node affinity rules needed. Each pod gets its own claim from `ResourceClaimTemplate`.

**StatefulSet (simple):**
```bash
kubectl apply -f deploy/multinode/test-statefulset-two-t3k.yaml
kubectl logs wh-t3k-worker-0   # running on t3k-a
kubectl logs wh-t3k-worker-1   # running on t3k-b
kubectl delete -f deploy/multinode/test-statefulset-two-t3k.yaml
```

**MPIJob (coordinated):**
```bash
# Install MPI Operator once:
kubectl apply -f https://raw.githubusercontent.com/kubeflow/mpi-operator/v0.5.0/deploy/v2beta1/mpi-operator.yaml

kubectl apply -f deploy/multinode/test-mpi-two-t3k.yaml
kubectl logs -l training.kubeflow.org/job-role=launcher -f
kubectl delete -f deploy/multinode/test-mpi-two-t3k.yaml
```

### Two T3K units with external Ethernet link (connected mesh)

When the two T3K units are physically connected via external Ethernet cables, they form a single 16-chip mesh. Update labels to reflect this:

```bash
kubectl label node <node-a> tenstorrent.com/pod-size=2 tenstorrent.com/host-rank=0 --overwrite
kubectl label node <node-b> tenstorrent.com/pod-size=2 tenstorrent.com/host-rank=1 --overwrite
# same physical-pod on both nodes:
kubectl label node <node-a> <node-b> tenstorrent.com/physical-pod=t3k-ab --overwrite
```

Then use `matchAttribute` in the ResourceClaim to ensure both nodes are co-scheduled:

```yaml
devices:
  requests:
  - name: t3k
    exactly:
      deviceClassName: t3k.wormhole.tenstorrent.com
      count: 2
  constraints:
  - requests: [t3k]
    matchAttribute: tenstorrent.com/physical_pod
```

No plugin code changes required — only label updates and ResourceClaim template changes.
