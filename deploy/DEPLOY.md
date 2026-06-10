# Deploy Directory — File Reference

What each YAML file does and when to apply it.

---

## CI/CD — automatic build and deploy

Every `git push` to `main` triggers the full pipeline automatically:

```
git push origin main
     ↓  GitHub Actions build job (~2-3 min)
     docker build → push to ghcr.io/slifersky/wh-dra-plugin:sha-XXXXXXX + :latest
     ↓  GitHub Actions deploy job (runs on self-hosted runner on control-plane-01)
     helm upgrade → updates DaemonSet image tag to sha-XXXXXXX
     ↓  Kubernetes rolling update (~1-2 min)
     kubelet pulls new image on t3k-node-a and t3k-node-b
     plugin pods restart with new code
```

**What gets updated on every push:**
- `wh-dra-kubelet-plugin` DaemonSet on all T3K nodes
- `wh-node-labeler` DaemonSet on all T3K nodes
- Any Helm chart changes (RBAC, DeviceClass, topology ConfigMap)

**What is NOT touched:**
- Workload pods already running — they continue until they exit naturally
- Cluster infrastructure (Kubernetes, containerd, kernel driver)

**Verify a deploy landed:**
```bash
kubectl get pods -n kube-system -l app=wh-dra-kubelet-plugin \
  -o jsonpath='{range .items[*]}{.spec.containers[0].image}{"\n"}{end}'
# should show: ghcr.io/slifersky/wh-dra-plugin:sha-XXXXXXX
# where XXXXXXX = first 7 chars of your latest git commit
```

**Self-hosted runner** (required for deploy job to reach the private cluster):
- Installed on `control-plane-01` at `~/actions-runner/`
- Runs as a systemd service: `sudo systemctl status actions.runner.*`
- If the runner goes offline, the build job still succeeds (image is pushed to ghcr.io)
  but the deploy step will be queued until the runner comes back online

**Full Helm reference** (values, rollback, manual install): `helm/wh-dra-plugin/README.md`

**Re-run a failed deploy without a code change:**
```bash
# Option 1: empty commit to re-trigger the pipeline
git commit --allow-empty -m "ci: retrigger deploy"
git push origin main

# Option 2: manual helm upgrade on the control plane
SHA=$(git rev-parse --short HEAD)
helm upgrade wh-dra-plugin ./helm/wh-dra-plugin \
  --namespace kube-system \
  --set image.repository=ghcr.io/slifersky/wh-dra-plugin \
  --set image.tag=sha-${SHA}
```

---

## Dependency order (apply from scratch)

```
rbac.yaml
deviceclass.yaml
node-labeler.yaml                       ← labels T3K nodes; plugin DaemonSet depends on these labels
daemonset.yaml                          ← plugin must be running before any workload pod
odin/resourceclaimtemplate.yaml         (repeat for every workload namespace)
odin/inferenceservicetemplate-*.yaml
```

Test files (`test-*.yaml`, `multinode/`) are standalone — apply and delete as needed.

---

## Core plugin infrastructure

Apply these once per cluster. They are cluster-scoped and have no namespace dependencies.

### `rbac.yaml`

Gives the plugin permission to talk to the Kubernetes API.

Creates three objects:

| Object | Name | Namespace |
|---|---|---|
| ServiceAccount | `wh-dra-plugin` | `kube-system` |
| ClusterRole | `wh-dra-plugin` | — |
| ClusterRoleBinding | `wh-dra-plugin` | — |

The ClusterRole grants exactly what the plugin needs:

| API group | Resource | Verbs |
|---|---|---|
| `resource.k8s.io` | `resourceslices` | get, list, watch, create, update, patch, delete |
| `resource.k8s.io` | `resourceclaims` | get, list, watch |
| `resource.k8s.io` | `resourceclaims/status` | update, patch |
| `""` (core) | `nodes` | get |

Without this the plugin crashes immediately on startup when it tries to publish a ResourceSlice.

```bash
kubectl apply -f deploy/rbac.yaml
kubectl get clusterrole wh-dra-plugin
```

---

### `deviceclass.yaml`

Defines the device type that pods can request.

Creates a `DeviceClass` named `t3k.wormhole.tenstorrent.com` with one CEL selector rule:

```
device.driver == "wormhole.tenstorrent.com"
```

The scheduler uses this to match pod `ResourceClaims` against the `ResourceSlices` the plugin publishes. Pods reference this class by name in their claim spec.

```bash
kubectl apply -f deploy/deviceclass.yaml
kubectl get deviceclass t3k.wormhole.tenstorrent.com
```

---

### `node-labeler.yaml`

Automatically labels T3K nodes with hardware metadata discovered at runtime.
Apply this **before** `daemonset.yaml` — the plugin DaemonSet's `nodeSelector` depends on
the labels this sets.

Creates five objects:

| Object | Name | Namespace |
|---|---|---|
| ServiceAccount | `wh-node-labeler` | `kube-system` |
| ClusterRole | `wh-node-labeler` | — |
| ClusterRoleBinding | `wh-node-labeler` | — |
| ConfigMap | `tt-node-topology` | `kube-system` |
| DaemonSet | `wh-node-labeler` | `kube-system` |

The DaemonSet runs on every worker node. On non-T3K nodes (no `/dev/tenstorrent/` devices)
it sleeps permanently and uses zero resources. On T3K nodes it runs `tt-smi -s`, reads the
`tt-node-topology` ConfigMap, and patches the node with labels every 5 minutes.

Labels applied automatically:

| Label | Source |
|---|---|
| `tenstorrent.com/arch` | `tt-smi` board_type (wormhole / blackhole) |
| `tenstorrent.com/board-type` | `tt-smi` board_type (n300, n150, …) |
| `tenstorrent.com/chip-count` | Count of `/dev/tenstorrent/` entries |
| `tenstorrent.com/physical-pod` | `tt-node-topology` ConfigMap |
| `tenstorrent.com/host-rank` | `tt-node-topology` ConfigMap |
| `tenstorrent.com/pod-size` | `tt-node-topology` ConfigMap |
| `moai.moreh.io/accelerator.vendor` | Hardcoded `tenstorrent` |
| `moai.moreh.io/accelerator.model` | Same as arch |

**Configuring topology for a new node** — edit the ConfigMap (no pod restart needed):

```bash
kubectl edit configmap tt-node-topology -n kube-system
# Add: t3k-node-b: "physical-pod=t3k-a host-rank=1 pod-size=1"
```

```bash
kubectl apply -f deploy/node-labeler.yaml
kubectl -n kube-system get pods -l app=wh-node-labeler
kubectl get node t3k-node-a --show-labels | tr ',' '\n' | grep tenstorrent
```

---

### `daemonset.yaml`

Runs the plugin binary on every T3K node. This is the object that does all the work.

Key details:

| Field | Value | Why |
|---|---|---|
| Namespace | `kube-system` | System-level workload |
| `nodeSelector` | `tenstorrent.com/arch=wormhole` | Only runs on labeled T3K nodes (set by wh-node-labeler) |
| `priorityClassName` | `system-node-critical` | Starts before workload pods |
| `securityContext.privileged` | `true` | Needed to write CDI specs and access `/dev/tenstorrent` |
| `image` | `ghcr.io/slifersky/wh-dra-plugin:latest` | Built by GitHub Actions on every push to main |
| `imagePullPolicy` | `Always` | Ensures latest image is pulled on each pod restart |

Volume mounts:

| Name | Host path | Container path | Purpose |
|---|---|---|---|
| `plugin-dir` | `/var/lib/kubelet/plugins/wormhole.tenstorrent.com` | same | kubelet plugin socket |
| `registrar-dir` | `/var/lib/kubelet/plugins_registry` | same | kubelet plugin registration |
| `cdi-dir` | `/var/run/cdi` | same | CDI spec files |
| `dev-tenstorrent` | `/dev/tenstorrent` | same | Hardware device nodes |
| `checkpoint-dir` | `/var/lib/wh-dra/checkpoint` | same | Crash-recovery state |
| `tt-logs-dir` | `/tmp/tt_logs` | `/tmp/tt_logs` | Firmware log directory (DirectoryOrCreate — kubelet creates on host) |

```bash
kubectl apply -f deploy/daemonset.yaml
kubectl -n kube-system get pods -l app=wh-dra-kubelet-plugin
kubectl -n kube-system logs -l app=wh-dra-kubelet-plugin
```

---

## Test files

Standalone — apply and delete independently. No ordering dependency on each other.

### `test-claim.yaml`

**Purpose**: Minimal smoke test — verifies device injection works without any special image.

Creates:
- `ResourceClaim` named `wh-demo-claim`
- `Pod` named `wh-demo-pod` using `ubuntu:22.04`

The pod runs `ls /dev/tenstorrent/` and `env | grep TT_` then exits. Use this to confirm the plugin is wired up correctly before testing real hardware workloads.

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
...
=== injected env vars ===
TT_CHIP_COUNT=4
TT_MESH_HOST_RANK=0
TT_PHYSICAL_POD=t3k-a
WH_RESOURCE_CLAIM_UID=<uid>
```

---

### `test-ttnn.yaml`

**Purpose**: Full hardware test — runs real silicon via `ttnn`.

Requires the `npu-metal-llk:latest` image imported into k3s containerd:
```bash
docker save npu-metal-llk:latest | sudo k3s ctr images import -
```

Creates:
- `ResourceClaim` named `wh-ttnn-claim`
- `Pod` named `wh-ttnn-test` using `npu-metal-llk:latest`

The pod opens Wormhole device 0, runs `ttnn.add([1,2,3], [4,5,6])`, asserts the result is `[5,7,9]`, then closes the device. Also confirms `/dev/hugepages-1G` is present (injected via CDI — no `hostPath` in the pod spec).

```bash
kubectl apply -f deploy/test-ttnn.yaml
kubectl logs -f wh-ttnn-test
kubectl delete -f deploy/test-ttnn.yaml
```

Expected last lines:
```
ttnn.add([1,2,3], [4,5,6]) = [5.0, 7.0, 9.0]
SUCCESS: Wormhole hardware verified via DRA-allocated pod
```

---

### `test-two-pods.yaml`

**Purpose**: Proves DRA exclusivity — only one pod can hold the device at a time.

Creates two independent `ResourceClaim` + `Pod` pairs:
- `wh-excl-pod-a` — acquires the device, holds it for 30 s, then exits
- `wh-excl-pod-b` — stays `Pending` until pod-a's claim is released

Claim cleanup after pod-a finishes is **manual** (you must `kubectl delete pod wh-excl-pod-a && kubectl delete resourceclaim wh-excl-claim-a`). For automated exclusivity testing with clean lifecycle, use `ResourceClaimTemplate` inside a `Job` instead.

```bash
kubectl apply -f deploy/test-two-pods.yaml
kubectl get pods -w   # watch pod-a run, pod-b pending
# after pod-a completes:
kubectl delete pod wh-excl-pod-a && kubectl delete resourceclaim wh-excl-claim-a
kubectl logs wh-excl-pod-b
kubectl delete -f deploy/test-two-pods.yaml
```

---

### `multinode/test-job-scheduler.yaml`

**Purpose**: Scheduler stress test — verifies the scheduler distributes work across all available T3K nodes.

Creates a `ResourceClaimTemplate` + `Job` with `completions=10, parallelism=2`. Ten 10-second tasks run two at a time; the scheduler fills both nodes in parallel.

**Verified result (2 nodes):** 5 completions on t3k-node-a, 5 completions on t3k-node-b, ~50 s total.

```bash
kubectl apply -f deploy/multinode/test-job-scheduler.yaml
kubectl get pods -o wide -w       # watch pods distribute across nodes
kubectl get pods -o wide          # after completion: 5 per node
kubectl delete -f deploy/multinode/test-job-scheduler.yaml
```

---

## Multinode tests

Both files require at least two nodes labeled `tenstorrent.com/arch=wormhole` and the DaemonSet running on both.

### `multinode/test-statefulset-two-t3k.yaml`

**Purpose**: Two independent workers across two T3K nodes — no MPI Operator required.

Creates:
- `ResourceClaimTemplate` named `wh-t3k-ss-template` — each pod gets its own auto-named claim
- Headless `Service` named `wh-t3k-headless` — gives pods stable DNS (`wh-t3k-worker-0.wh-t3k-headless`, etc.)
- `StatefulSet` with 2 replicas — scheduler places each on a different T3K node automatically

Use when workers communicate over regular TCP/IP and do not need `mpirun` coordination.

```bash
kubectl apply -f deploy/multinode/test-statefulset-two-t3k.yaml
kubectl logs wh-t3k-worker-0
kubectl logs wh-t3k-worker-1
kubectl delete -f deploy/multinode/test-statefulset-two-t3k.yaml
```

---

### `multinode/test-mpi-two-t3k.yaml`

**Purpose**: Two coordinated MPI workers across two T3K nodes.

Requires MPI Operator installed:
```bash
kubectl apply -f https://raw.githubusercontent.com/kubeflow/mpi-operator/v0.5.0/deploy/v2beta1/mpi-operator.yaml
```

Creates:
- `ResourceClaimTemplate` named `wh-t3k-mpi-template`
- `MPIJob` named `wh-mpi-two-t3k` with 1 launcher + 2 workers

Workers synchronize at an `MPI.Barrier()`. Demonstrates DRA claims work inside MPI operator-managed pods. Use when the workload needs `mpirun` / `mpi4py` coordination.

```bash
kubectl apply -f deploy/multinode/test-mpi-two-t3k.yaml
kubectl get mpijob wh-mpi-two-t3k
kubectl logs -l training.kubeflow.org/job-role=launcher -f
kubectl delete -f deploy/multinode/test-mpi-two-t3k.yaml
```

---

## Odin / InferenceService templates

These are for production inference deployments via the Odin operator (MoAI Inference Framework).

### `odin/resourceclaimtemplate.yaml`

**Purpose**: The T3K device claim template for Odin-managed pods.

Creates a `ResourceClaimTemplate` named `wh-t3k-template`. Must be applied to **every namespace** where an `InferenceService` runs. Odin automatically instantiates one `ResourceClaim` per worker pod from this template and garbage-collects it when the pod is deleted.

```bash
# Apply to each workload namespace:
kubectl apply -n <namespace> -f deploy/odin/resourceclaimtemplate.yaml
```

---

### `odin/inferenceservicetemplate-1node.yaml`
### `odin/inferenceservicetemplate-2node.yaml`
### `odin/inferenceservicetemplate-4node.yaml`
### `odin/inferenceservicetemplate-8node.yaml`

**Purpose**: Hardware configuration presets for Odin InferenceServices.

Each file defines the **hardware side** only — node selector, DRA claim, `hostNetwork: true`, tolerations. They do **not** include the container image or startup command. Those come from a separate runtime-base template (`vllm-wormhole-runtime` or `vllm-wormhole-runtime-dp`) provided by the NPU team in Seoul.

| File | Template name | Nodes | Chips | Odin workload type |
|---|---|---|---|---|
| `inferenceservicetemplate-1node.yaml` | `vllm-wormhole-1node` | 1 | 8 | Deployment (single pod) |
| `inferenceservicetemplate-2node.yaml` | `vllm-wormhole-2node` | 2 | 16 | LeaderWorkerSet, data=2 |
| `inferenceservicetemplate-4node.yaml` | `vllm-wormhole-4node` | 4 | 32 | LeaderWorkerSet, data=4 |
| `inferenceservicetemplate-8node.yaml` | `vllm-wormhole-8node` | 8 | 64 | LeaderWorkerSet, data=8 |

Hugepages are injected automatically via CDI — no `hostPath` volumes needed in the InferenceService spec.

```bash
kubectl apply -n mif -f deploy/odin/inferenceservicetemplate-1node.yaml
kubectl apply -n mif -f deploy/odin/inferenceservicetemplate-2node.yaml
kubectl apply -n mif -f deploy/odin/inferenceservicetemplate-4node.yaml
kubectl apply -n mif -f deploy/odin/inferenceservicetemplate-8node.yaml
```

Reference in an InferenceService:

```yaml
spec:
  templateRefs:
    - name: vllm-wormhole-runtime    # image + command (NPU team provides)
    - name: vllm-wormhole-1node      # hardware config (this file)
```

---

### `odin/example-inferenceservice.yaml`

**Purpose**: Usage examples — not applied to production directly.

Contains four complete `InferenceService` examples (1/2/4/8 node) showing how to combine `templateRefs`, set `ISVC_MODEL_NAME`, `ISVC_EXTRA_ARGS`, and `HF_TOKEN`. Fill in the blanks and apply to your inference namespace.

Runtime-base template selection:

| Pod size | `templateRefs[0]` |
|---|---|
| 1-node | `vllm-wormhole-runtime` |
| 2/4/8-node | `vllm-wormhole-runtime-dp` |
