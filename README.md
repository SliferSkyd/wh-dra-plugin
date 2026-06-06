# wh-dra-kubelet-plugin

Kubernetes DRA (Dynamic Resource Allocation) plugin for Tenstorrent Wormhole hardware (n300, T3K).

Publishes each Wormhole node as a single allocatable device in Kubernetes, injects `/dev/tenstorrent/*` device nodes and environment variables into pods via CDI.

## Prerequisites

- Kubernetes ≥ v1.35 (DRA stable)
- `tt-kmd` kernel driver loaded (`/dev/tenstorrent/` must exist)
- Go ≥ 1.26 (for building)
- Node labels set (see below)

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
| `arch` | `wormhole` | Hardware architecture (plugin node selector) |
| `board-type` | `n300` | Board form factor |
| `chip-count` | `4` | Number of PCIe-local chips (validated against `/dev/tenstorrent/`) |
| `physical-pod` | `t3k-a` | Identifies which T3K unit this node belongs to |
| `host-rank` | `0` | Host rank within the physical pod (cable order) |
| `pod-size` | `1` | Number of T3K units in this logical allocation group |

## Step 2 — Build

```bash
export PATH=$PATH:/home/ubuntu/go/bin
export GOPATH=/home/ubuntu/gopath

go build -o bin/wh-dra-kubelet-plugin ./cmd/wh-dra-kubelet-plugin
```

## Step 3 — Run locally (for testing, without a container)

Requires read access to `/etc/rancher/k3s/k3s.yaml`:

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

Verify the device is visible:

```bash
kubectl get resourceslice
kubectl get resourceslice -o yaml
```

Expected output: one ResourceSlice named `<node>-wormhole.tenstorrent.com-<suffix>` with device `wormhole-t3k` and all attributes.

## Step 4 — Deploy as DaemonSet (production)

```bash
# Apply RBAC and DeviceClass
kubectl apply -f deploy/rbac.yaml
kubectl apply -f deploy/deviceclass.yaml

# Build and import image into k3s containerd
docker build -t wh-dra-kubelet-plugin:latest .
docker save wh-dra-kubelet-plugin:latest | sudo k3s ctr images import -

# Deploy
kubectl apply -f deploy/daemonset.yaml

# Check
kubectl -n kube-system get pods -l app=wh-dra-kubelet-plugin
kubectl -n kube-system logs -l app=wh-dra-kubelet-plugin
```

## Step 5 — Test DRA allocation

```bash
kubectl apply -f deploy/test-claim.yaml

# Wait for pod to run
kubectl get pod wh-dra-test

# Check devices and env vars were injected
kubectl logs wh-dra-test
```

Expected output:
```
=== devices ===
0  1  2  3
=== env ===
TT_CHIP_COUNT=4
TT_MESH_HOST_RANK=0
TT_PHYSICAL_POD=t3k-a
TT_POD_SIZE=1
WH_RESOURCE_CLAIM_UID=<uid>
OK
```

Cleanup:
```bash
kubectl delete -f deploy/test-claim.yaml
```

## Project layout

```
cmd/wh-dra-kubelet-plugin/
  main.go        # Flags, kubeconfig/in-cluster config, signal handling
  driver.go      # Plugin startup, ResourceSlice publication
  manager.go     # Reads node labels, validates /dev/tenstorrent/ chip count
  state.go       # PrepareResourceClaims / UnprepareResourceClaims
  cdi.go         # Writes CDI YAML spec files to /var/run/cdi/
  checkpoint.go  # Crash recovery: checkpoint.json with boot ID validation
pkg/
  flock/         # Cross-process file lock (safe during rolling DaemonSet updates)
  bootid/        # Reads /proc/sys/kernel/random/boot_id
  metrics/       # Prometheus metrics (6 counters/gauges/histograms)
deploy/
  rbac.yaml          # ServiceAccount, ClusterRole, ClusterRoleBinding
  deviceclass.yaml   # DeviceClass selecting wormhole.tenstorrent.com devices
  daemonset.yaml     # DaemonSet (nodeSelector: tenstorrent.com/arch=wormhole)
  test-claim.yaml    # ResourceClaim + test Pod
```

## CDI spec files

The plugin writes two types of CDI files to `/var/run/cdi/`:

- `k8s.wormhole.tenstorrent.com-t3k-common.yaml` — node-level env vars (`TT_MESH_HOST_RANK`, `TT_CHIP_COUNT`, etc.), written once at startup
- `k8s.wormhole.tenstorrent.com-t3k-<claimUID>.yaml` — per-pod device nodes (`/dev/tenstorrent/0..N`), written in `PrepareResourceClaims`, deleted in `UnprepareResourceClaims`

## Environment variables injected into pods

| Variable | Example | Source |
|---|---|---|
| `TT_MESH_HOST_RANK` | `0` | `host-rank` node label |
| `TT_CHIP_COUNT` | `4` | `chip-count` node label |
| `TT_POD_SIZE` | `1` | `pod-size` node label |
| `TT_PHYSICAL_POD` | `t3k-a` | `physical-pod` node label |
| `TT_ETHERNET_IFACE` | `cnx1` | `ethernet-iface` node label (optional) |
| `WH_RESOURCE_CLAIM_UID` | `<uid>` | Kubernetes ResourceClaim UID |

## Multi-T3K topology (future)

With external Ethernet links between two T3K units, update labels to `pod-size=2` and `host-rank=0/1`. Use `matchAttribute` constraint in the ResourceClaim to co-schedule both nodes:

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

No plugin code changes needed — only label updates and ResourceClaim template changes.
