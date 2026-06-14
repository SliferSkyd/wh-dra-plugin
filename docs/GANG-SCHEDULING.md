# Gang Scheduling — Investigation, Failures, and Final Solution

Gang scheduling ensures that all N pods in a group are scheduled **atomically** — either
all place on separate nodes, or all stay Pending.  Without this guarantee the scheduler can
place pod-0 on node-a while pod-1 waits indefinitely, leaving pod-0 holding a device claim
it cannot use until its partner arrives.

---

## Table of Contents

1. [Background — why we need gang scheduling](#background)
2. [What we investigated](#investigation)
3. [What we tried — coscheduling plugin](#tried)
4. [Problems encountered](#problems)
5. [Root cause proof (source code)](#root-cause)
6. [Final solution — Scheduling Gates](#solution)
7. [How the controller works](#controller)
8. [RBAC and deployment](#deployment)
9. [Test manifest](#test)
10. [Interaction with TotalSliceCount](#totalslicecount)

---

<a name="background"></a>
## 1. Background

Our hardware is a T3K (Wormhole) chip-to-chip pod.  A 2-node pod means two physical
servers are connected via high-speed ethernet; any workload must run on **both nodes
simultaneously** or not at all.

DRA (Dynamic Resource Allocation) gives each node a ResourceSlice in a shared pool.  The
scheduler allocates one device per node from that pool, one pod per node.  But if pod-0
gets scheduled before pod-1, pod-0 occupies its node's device and waits for pod-1, while
pod-1 may be stuck Pending for other reasons (resource pressure, anti-affinity, etc.).
The result is a deadlock or partial startup.

We needed gang scheduling: schedule both pods in the same scheduling pass, or neither.

---

<a name="investigation"></a>
## 2. What We Investigated

### 2.1 The scheduling framework phases

Kubernetes schedules one pod at a time through these phases:

```
Filter → Score → Reserve → Permit → PreBind → Bind → PostBind
```

- **Reserve** — allocates resources in memory (DRA calls `inFlightAllocations`)
- **Permit** — last gate before binding; plugins can WAIT here to synchronize a group
- **PreBind** — writes the allocation to the API server (DRA's critical write)
- **Bind** — assigns the pod to a node

Gang scheduling requires holding all pods in **Permit** until the whole group can be placed,
then releasing them together.

### 2.2 The coscheduling plugin

`kubernetes-sigs/scheduler-plugins` ships a scheduler called
`scheduler-plugins-scheduler`.  It adds a `Coscheduling` plugin that introduces the
`PodGroup` CRD.  Pods labeled with a PodGroup are held in Permit(WAIT) until all
`minMember` pods can be placed simultaneously.

We checked:
- The plugin uses the native K8s 1.34 `DynamicResources` plugin — no DRA-specific code of
  its own.
- The latest available release was **0.34.7** (K8s 1.34), running on our K8s 1.35 cluster.
  We confirmed there was no 0.35.x available at the time.
- The API version of PodGroup changed across releases: early versions used
  `scheduling.sigs.k8s.io/v1alpha1`, current uses `scheduling.x-k8s.io/v1alpha1`.

### 2.3 ResourceSlice pool semantics and TotalSliceCount

Early in the investigation we questioned whether `TotalSliceCount` was still needed once
gang scheduling was in place.  We traced through the vendor patch:

```go
// vendor/k8s.io/dynamic-resource-allocation/resourceslice/resourceslicecontroller.go
resourceSliceCount := len(pool.Slices)
if pool.TotalSliceCount > 0 {
    resourceSliceCount = int(pool.TotalSliceCount)
}
```

Without `TotalSliceCount`, each node publishes `resourceSliceCount: 1`.  The scheduler
sees the pool as having only 1 allocatable slot and blocks pod-1 after pod-0 claims it —
wrong for a 2-device pool.  `TotalSliceCount: 2` tells the scheduler the pool has 2
devices and both can be allocated independently.

We temporarily removed `TotalSliceCount` thinking gang scheduling made it redundant, then
had to restore it when we confirmed it serves a second purpose: blocking allocation from
an incomplete pool (e.g. one node's plugin is down).

### 2.4 DRA's inFlightAllocations

We read the DRA source to understand how the scheduler tracks in-progress allocations:

```go
// k8s.io/dynamic-resource-allocation — Reserve phase
inFlightAllocations.Store(claim.UID, allocatedClaim)
```

`inFlightAllocations` is keyed by **claim UID**, not by device.  It prevents the same
claim from being double-allocated, but it does NOT prevent two different claims (one per
pod) from both seeing the same device as available before either PreBind runs.

---

<a name="tried"></a>
## 3. What We Tried — Coscheduling Plugin

### 3.1 Installation

```bash
helm repo add scheduler-plugins https://scheduler-plugins.sigs.k8s.io
helm repo update
helm install scheduler-plugins scheduler-plugins/scheduler-plugins \
  --namespace scheduler-plugins --create-namespace --version 0.34.7
```

Both pods were Running after install — the scheduler itself worked.

### 3.2 PodGroup CRD

The CRD must be installed separately before applying any PodGroup manifest:

```bash
helm show crds scheduler-plugins/scheduler-plugins --version 0.34.7 | kubectl apply -f -
```

We initially used the wrong API version (`scheduling.sigs.k8s.io/v1alpha1`) and got:

```
no matches for kind "PodGroup" in version "scheduling.sigs.k8s.io/v1alpha1"
```

Fixed by using `scheduling.x-k8s.io/v1alpha1` and updating the pod label to match.

### 3.3 Test manifest (coscheduling era)

```yaml
apiVersion: scheduling.x-k8s.io/v1alpha1
kind: PodGroup
metadata:
  name: t3k-scheduler-test-gang
spec:
  minMember: 2
  scheduleTimeoutSeconds: 60
---
# Job pod template:
metadata:
  labels:
    scheduling.x-k8s.io/pod-group: t3k-scheduler-test-gang
spec:
  schedulerName: scheduler-plugins-scheduler
```

Both pods went Pending — the scheduler was holding them.  But they never progressed.

---

<a name="problems"></a>
## 4. Problems Encountered

### 4.1 resourceSliceCount: 1 on both nodes

After first deploying with coscheduling, we saw:

```
$ kubectl get resourceslices -o yaml | grep resourceSliceCount
  resourceSliceCount: 1   # t3k-node-a
  resourceSliceCount: 1   # t3k-node-b
```

Both nodes showed `resourceSliceCount: 1` instead of `2`.  This meant the scheduler saw
only 1 device in the pool and blocked pod-1 after pod-0 claimed it.

**Root cause:** the `TotalSliceCount` vendor patch existed only in the local working tree.
The `vendor/` folder was never committed to git, so the Docker build (which ran
`go mod vendor` from scratch inside the container) never saw the patch.  The built image
had the unpatched code.

**Fix:** committed `vendor/` to git (it is not gitignored), pushed, rebuilt image.
After the fix ResourceSlices showed `resourceSliceCount: 2`.

### 4.2 Both pods stuck in Permit on the same node

Even after fixing `resourceSliceCount`, both pods never left Pending.  Scheduler logs
revealed:

```
Permit: pod-0 → t3k-node-a (WAIT)
Permit: pod-1 → t3k-node-a (WAIT)   ← both picked the same node
```

Both pods were waiting in Permit on `t3k-node-a`.  Pod-1 should have been filtered to
`t3k-node-b` by DRA exclusivity, but it wasn't.

### 4.3 Client-side throttling

Because coscheduling's retry loop fires at ~40 ms cadence, the scheduler accumulated over
3 minutes of client-side throttle delay posting events.  `kubectl describe pod` showed
`Events: <none>` even though scheduling was failing continuously.

---

<a name="root-cause"></a>
## 5. Root Cause Proof (Source Code)

We traced through the scheduler-plugins and Kubernetes source code to prove the failure
is fundamental, not a configuration mistake.

### 5.1 Permit phase and PreBind ordering

```
Scheduling Framework:

  RunFilterPlugins
  RunReservePluginsReserve    ← DRA stores inFlightAllocation (memory only)
  RunPermitPlugins            ← Coscheduling returns WAIT here
    (pod sits here until all gang members arrive)
  RunPreBindPlugins           ← DRA writes allocation to API ← NEVER REACHED while WAITing
  RunBindPlugins
```

While pod-0 is in Permit(WAIT), its DRA allocation has NOT been written to the API server.
When pod-1 runs through Filter and Reserve, it queries the API for existing allocations
and sees **device-A as free** — because pod-0's PreBind hasn't run yet.  Pod-1 therefore
also selects device-A on `t3k-node-a`.

### 5.2 PostFilter rejection loop

When pod-1 cannot fit (anti-affinity blocks it from joining pod-0 on `t3k-node-a`),
coscheduling's `PostFilter` fires:

```go
// scheduler-plugins/pkg/coscheduling/coscheduling.go
func (cs *Coscheduling) PostFilter(...) {
    // Reject all waiting gang members
    for _, waitingPod := range waitingPods {
        waitingPod.Reject(cs.Name(), "optimistic rejection in PostFilter")
    }
}
```

`Reject()` triggers the framework to call `Unreserve` on pod-0, which clears the DRA
in-flight allocation.  Both pods are now back to square one.  The scheduler retries
immediately, the same race happens, and the loop repeats indefinitely.

### 5.3 Why inFlightAllocations cannot fix this

`inFlightAllocations` in the DRA plugin is:

```go
var inFlightAllocations sync.Map  // key: claim.UID → value: allocated claim
```

It prevents the **same** claim from being double-allocated.  It does not prevent **two
different claims** (one per pod) from both seeing device-A as free, because neither claim
has reached PreBind yet.  This is a cross-claim race that inFlightAllocations was not
designed to prevent.

**Conclusion:** DRA + coscheduling is fundamentally broken in any K8s version where
Permit precedes PreBind — which is all of them.  The only fix would be a DRA-aware
gang plugin that defers DRA reservation until all gang members are ready, which
scheduler-plugins does not implement.

---

<a name="solution"></a>
## 6. Final Solution — Scheduling Gates + nodeSelector

Instead of holding pods inside the scheduler (Permit), we hold them **before** they enter
the scheduler at all, using Kubernetes Scheduling Gates (stable since K8s 1.30).

A gated pod is invisible to the scheduler.  Once the gate is removed, the pod enters the
scheduling queue normally.

We also add a `nodeSelector` pinning each pod to its specific node (by `tenstorrent.com/host-rank`).
This eliminates DRA device competition: pod-0 can only reach node-a (one device), pod-1 can
only reach node-b (a different device).  With no competition, the sequential scheduler
correctly allocates via PreBind with no races.

```
t=0  pod-0 created (gated, nodeSelector=host-rank=0)  → not visible to scheduler
     pod-1 created (gated, nodeSelector=host-rank=1)  → not visible to scheduler
     (Kubernetes claim controller immediately creates ResourceClaims for both pods)

t=2  controller: 2/2 pods present → remove both gates simultaneously

t=3  scheduler processes pod-0:
       nodeSelector → only node-a considered
       Filter → Reserve → PreBind → device-A written to API → Bind → node-a ✓

t=4  scheduler processes pod-1:
       nodeSelector → only node-b considered
       device-B on node-b is free (pod-0 took device-A on a different node)
       Filter → Reserve → PreBind → device-B written to API → Bind → node-b ✓
```

Why nodeSelector is critical: without it, the scheduler is free to place pod-0 on
node-b (allocating device-B), then pod-1 finds node-b's device already taken and
node-a's device is on a node the scheduler assigned to no one — resulting in
"cannot allocate all claims" on every retry.

Why coscheduling fails even with nodeSelector: the coscheduling PostFilter fires when
any gang member fails a filter pass.  PostFilter calls Unreserve on waiting pods,
clearing their in-flight DRA allocations.  This can leave ResourceClaims in a
transitional state that causes "cannot allocate all claims" on the next retry.
Removing coscheduling from the picture (using the default scheduler) eliminates this.

---

<a name="controller"></a>
## 7. How the Controller Works

The gang gate controller is a goroutine inside every DaemonSet plugin instance (`gang_gate_controller.go`).  It polls every 2 seconds:

1. List all pods cluster-wide with label `tenstorrent.com/gang-group` (existence selector).
2. Skip pods where `DeletionTimestamp != nil` or phase is `Succeeded`/`Failed`.
3. Group remaining pods by `(namespace, gang-group)`.
4. For each group: if `len(total) >= gang-size` and any pod still has the gate:
   - Remove `tenstorrent.com/gang-ready` from every gated pod via strategic merge patch
     (preserving any other scheduling gates).

**Why no leader election is needed:** the remove-gate patch is idempotent.  If two
DaemonSet instances both decide to release the same group, both succeed — the second patch
is a no-op.  If the controller crashes mid-release (after removing pod-0's gate but before
pod-1's), the next poll cycle sees 2 total / 1 gated → still `>= gang-size` → removes
pod-1's gate.

---

<a name="deployment"></a>
## 8. RBAC and Deployment

The Helm chart's ClusterRole (`helm/wh-dra-plugin/templates/rbac.yaml`) includes:

```yaml
- apiGroups: [""]
  resources: [pods]
  verbs: [get, list, watch, patch]
```

After changing RBAC, run `helm upgrade` to apply it to the cluster.  The controller starts
automatically with the plugin binary — no separate deployment needed.

---

<a name="test"></a>
## 9. Test Manifest

`deploy/test-scheduler.yaml` creates two bare Pods (one per node) with scheduling gates
and nodeSelector.  No coscheduling scheduler or PodGroup is required.

```bash
# Clean up any previous run first
kubectl delete pod t3k-scheduler-test-0 t3k-scheduler-test-1 --ignore-not-found
kubectl delete resourceclaimtemplate t3k-scheduler-test-claim --ignore-not-found

kubectl apply -f deploy/test-scheduler.yaml

# Both pods start SchedulingGated.  After ~2 s the controller releases both gates.
kubectl get pods -l app=t3k-scheduler-test -o wide -w

# Expected: t3k-scheduler-test-0 on host-rank=0 node, t3k-scheduler-test-1 on host-rank=1 node
kubectl logs -l app=t3k-scheduler-test --prefix

kubectl delete -f deploy/test-scheduler.yaml
```

Pod excerpt:

```yaml
metadata:
  labels:
    app: t3k-scheduler-test
    tenstorrent.com/gang-group: t3k-scheduler-test   # identifies the gang
    tenstorrent.com/gang-size: "2"
spec:
  schedulingGates:
  - name: tenstorrent.com/gang-ready
  nodeSelector:
    tenstorrent.com/host-rank: "0"   # or "1" for the second pod
```

Use a unique `gang-group` value per job run so completed pods from previous runs
don't count toward the quorum of a new run.

---

<a name="totalslicecount"></a>
## 10. Interaction with TotalSliceCount

Gang scheduling via gates works alongside — not instead of — the `TotalSliceCount`
mechanism.  They solve different problems:

| Mechanism | Problem solved |
|---|---|
| `TotalSliceCount: N` | Prevents scheduler from allocating from an incomplete pool (e.g. one node's plugin is down).  Without it, the pool appears to have only 1 device even though 2 are expected. |
| Scheduling Gates | Prevents one pod from scheduling before its partner exists, so DRA's sequential PreBind can correctly assign different devices to each. |

Both must be active for correct multi-node scheduling.
