# Feature Comparison: wh-dra-plugin vs NVIDIA GPU Operator vs Google TPU Plugin

> Reference for gap analysis and roadmap planning.

---

## Side-by-side feature matrix

| Feature | NVIDIA GPU Operator | Google TPU Plugin | wh-dra-plugin | Notes |
|---|:---:|:---:|:---:|---|
| **Device discovery & advertising** | ✅ | ✅ | ✅ | All three publish devices to the scheduler |
| **Device injection into containers** | ✅ | ✅ | ✅ | NVIDIA/us use CDI; TPU uses device plugin API |
| **Basic health monitoring** | ✅ | ✅ | ✅ | We use tt-smi heartbeat; NVIDIA uses DCGM |
| **Scheduler feedback on unhealthy node** | ✅ | ✅ | ✅ | Empty ResourceSlice / taint |
| **Prometheus metrics** | ✅ | ✅ | ✅ | We have basic metrics; NVIDIA has ~100 |
| **Automatic node labeling** | ✅ (GFD) | ✅ | ❌ | We require manual `kubectl label` |
| **Automatic driver installation** | ✅ | ✅ (GKE managed) | ❌ | tt-kmd must be pre-installed manually |
| **Deep hardware telemetry** | ✅ (DCGM) | ✅ | ❌ | Temperature, power, utilization, ECC, bandwidth |
| **Per-pod hardware metrics** | ✅ | ✅ | ❌ | Which pod is using how much chip capacity |
| **Multi-device sharing (time-slice)** | ✅ | ❌ | ❌ | Multiple pods sharing one GPU |
| **Fractional allocation (MIG)** | ✅ | ❌ | ❌ | Split one GPU into smaller slices |
| **Topology-aware scheduling** | ✅ (NVLink) | ✅ (strict) | ❌ | Route pods to chips that are directly connected |
| **Multi-node network integration** | ✅ (RDMA/IB) | ✅ (TPU ICI) | ❌ | Automated high-speed interconnect setup |
| **Graceful drain / maintenance mode** | ✅ | ✅ | ❌ | Evict workloads cleanly before node maintenance |
| **Fault / error code monitoring** | ✅ (XID errors) | ✅ | ❌ | We only check heartbeat stall |
| **Firmware/driver version management** | ✅ | ✅ | ❌ | Upgrade tt-kmd across nodes |
| **Pod preemption (priority eviction)** | ✅ | ✅ | ❌ | Evict low-priority pods to make room |
| **Self-contained container image** | ✅ | ✅ | ✅ | Plugin binary + tt-smi baked into `wh-dra-kubelet-plugin:v0.1.0` |
| **Health diagnostic tests** | ✅ (DCGM diag) | ✅ | ❌ | Active chip-to-chip connectivity tests |

---

## What each missing feature means in practice

### ❌ Automatic node labeling

**NVIDIA:** GPU Feature Discovery (GFD) runs as a DaemonSet, reads GPU properties via NVML, and automatically sets labels like `nvidia.com/gpu.product=A100-SXM4-80GB`, `nvidia.com/gpu.memory=80000`, `nvidia.com/cuda.driver.major=535`.

**TPU:** GKE automatically labels TPU node pools with chip type, topology, and accelerator count.

**Ours:** An operator must run `kubectl label node t3k-node-a tenstorrent.com/arch=wormhole ...` manually after every cluster reset.

**Impact:** Labels are silently missing after any cluster reset. The DaemonSet won't deploy and workloads won't schedule — and there's no warning, the cluster just looks like it has no T3K nodes.

**Fix:** A small DaemonSet (similar to GFD) that reads `/dev/tenstorrent/`, queries `tt-smi`, and self-labels the node. Or a one-time node join script called by Kubespray.

---

### ❌ Automatic driver installation

**NVIDIA:** The GPU Operator includes a driver DaemonSet that compiles and loads the NVIDIA kernel module on each node. You can upgrade drivers cluster-wide with a single manifest update.

**TPU:** GKE manages TPU firmware as part of the managed node pool lifecycle.

**Ours:** `tt-kmd` (the Tenstorrent kernel module) must be manually installed and loaded on each T3K node before the plugin starts. If the driver isn't loaded, the plugin crashes immediately (`read /dev/tenstorrent: no such file or directory`).

**Impact:** Scaling to a new node requires SSH + manual driver install. No way to upgrade `tt-kmd` version cluster-wide.

**Fix:** A `tt-kmd-installer` DaemonSet that loads the kernel module (or compiles + installs it if missing). This is standard practice for hardware operators.

---

### ❌ Deep hardware telemetry

**NVIDIA:** DCGM Exporter publishes ~100 Prometheus metrics per GPU:
- `DCGM_FI_DEV_GPU_TEMP` — temperature
- `DCGM_FI_DEV_POWER_USAGE` — power draw (watts)
- `DCGM_FI_DEV_GPU_UTIL` — SM utilization (%)
- `DCGM_FI_DEV_FB_USED` — VRAM used
- `DCGM_FI_DEV_ECC_DBE_VOL_TOTAL` — double-bit ECC errors (data corruption risk)
- `DCGM_FI_DEV_NVLINK_BANDWIDTH_TOTAL` — NVLink throughput

**TPU:** GKE exports chip utilization, memory bandwidth, idle time, and error rates.

**Ours:** We export `wh_dra_prepared_devices_total`, `wh_dra_prepare_duration_seconds`, and a few request counters. No chip utilization, no temperature, no power, no error rates.

**Impact:** No visibility into whether chips are actually being used, running hot, or experiencing errors. Operators are flying blind.

**Fix:** Parse the full `tt-smi -s` JSON output (which includes temperature, voltage, board power, AICLK frequency) and export them as Prometheus metrics. A dedicated telemetry goroutine separate from the health checker.

---

### ❌ Per-pod hardware metrics

**NVIDIA:** DCGM can attribute GPU utilization and memory to specific pods/namespaces via cgroup mapping. You can answer "which team's workload is consuming how much GPU?"

**Ours:** We know which claims are prepared (via checkpoint) but we don't track per-claim utilization.

**Impact:** No chargeback, no capacity planning, no "who is hogging the T3K" visibility.

**Fix:** Map prepared claim UIDs to pod names via the Kubernetes API, then export per-pod chip utilization from `tt-smi`.

---

### ❌ Topology-aware scheduling

**NVIDIA:** NVIDIA publishes NVLink topology via node labels (`nvidia.com/gpu.links.X=Y`) so the scheduler can place multi-GPU workloads on GPUs that are directly connected via NVLink, maximizing inter-GPU bandwidth.

**TPU:** Google TPU pods have a rigid topology requirement. A 4x4x4 TPU pod needs exactly 64 chips in a specific physical arrangement. The TPU plugin validates and enforces this.

**Ours:** If a workload needs two T3K nodes that can communicate via Tenstorrent Ethernet (Galaxy topology), we don't verify or enforce that the two allocated nodes are actually connected. The scheduler might pick two nodes on different switches.

**Impact:** Multi-node workloads that require direct chip-to-chip connectivity may be placed on nodes that can't communicate efficiently, silently degrading performance.

**Fix:** Publish Tenstorrent Ethernet peer information as device attributes in the ResourceSlice. Add a `TopologySpreadConstraint` or a scheduler extender that validates connectivity before binding.

---

### ❌ Multi-node network integration

**NVIDIA:** The NVIDIA Network Operator installs RDMA drivers, configures InfiniBand / RoCE interfaces, and publishes RDMA device availability. Multi-node GPU workloads get high-speed RDMA automatically.

**TPU:** Inter-chip interconnect (ICI) is managed by Google infrastructure. The TPU plugin ensures pods are co-located on the same TPU pod network.

**Ours:** We have the `tenstorrent.com/ethernet-iface` label and inject `TT_ETHERNET_IFACE` into containers. But there is no automation that:
- Verifies the interface is up and connected
- Configures IP addressing for T3K-to-T3K links
- Validates the mesh topology before scheduling

**Impact:** Multi-node Galaxy workloads require manual network validation. The plugin can't detect or report a broken Tenstorrent Ethernet link.

**Fix:** A network validation step in the health checker (ping peers via `TT_ETHERNET_IFACE`), and a network operator component that configures IP addresses for the T3K mesh links.

---

### ❌ Graceful drain / maintenance mode

**NVIDIA:** `kubectl drain <node>` evicts GPU pods gracefully. The GPU Operator integrates with the node lifecycle to annotate nodes before maintenance and wait for workloads to finish.

**TPU:** GKE TPU maintenance windows trigger graceful pod eviction before firmware updates.

**Ours:** `kubectl drain t3k-node-a` will evict pods, but:
- Running workloads are killed immediately (no checkpoint/resume)
- The plugin pod itself is evicted, stopping health monitoring
- There is no "quiesce" signal from the plugin to workloads

**Impact:** Any rolling firmware update or maintenance requires manually watching for running workloads and waiting for them to finish.

**Fix:** Implement a SIGTERM handler in the plugin that first sets the ResourceSlice to empty (so no new pods schedule), waits for running workloads to finish, then exits cleanly.

---

### ❌ Fault / error code monitoring

**NVIDIA:** DCGM captures XID error codes from the GPU driver (XID 74 = NVLink error, XID 79 = GPU reset, etc.) and can trigger automatic node taint/eviction based on specific faults.

**Ours:** The health checker only catches a stalled heartbeat. It would miss:
- A chip that responds to `tt-smi` but has corrupted compute results
- A firmware panic that doesn't stall the heartbeat counter
- PCIe link errors
- Tenstorrent Ethernet link failures

**Impact:** Silent hardware failures that don't manifest as heartbeat stalls go undetected. Workloads may get wrong results with no error reported.

**Fix:** Parse `tt-smi` error/fault fields (if exposed), integrate with `dmesg` kernel log monitoring for tt-kmd errors, and add a compute-level health check (run a small kernel and verify the result).

---

### ✅ Self-contained container image — DONE (June 2026)

`wh-dra-kubelet-plugin:v0.1.0` bakes both the plugin binary and `tt-smi` into the image. No host mounts for Python or the binary. Deployable to any T3K node without pre-installed conda.

**Dockerfile:** multi-stage build — Go builder compiles the binary, Ubuntu 22.04 runtime installs tt-smi via pip from local source.

**Remaining step:** push to a registry (Harbor / ACR) so nodes pull automatically instead of requiring manual `ctr import` per node.

---

## Summary: Priority order for gaps

| Priority | Gap | Effort | Impact |
|---|---|---|---|
| ~~**P0**~~ | ~~Self-contained container image~~ | ~~Medium~~ | ✅ Done — `wh-dra-kubelet-plugin:v0.1.0` |
| **P0** | Automatic node labeling | Low | Cluster resets break deployments silently |
| **P1** | Deep hardware telemetry | Medium | Ops visibility for production |
| **P1** | Fault / error code monitoring | Medium | Silent failures in production |
| **P1** | Graceful drain | Medium | Required for maintenance without workload disruption |
| **P2** | Per-pod hardware metrics | Medium | Chargeback / capacity planning |
| **P2** | Topology-aware scheduling | High | Multi-node performance correctness |
| **P2** | Multi-node network integration | High | Galaxy / multi-T3K workloads |
| **P3** | Automatic driver installation | High | Simplifies node onboarding |
| **P3** | Multi-device sharing | High | Not a current use case |
