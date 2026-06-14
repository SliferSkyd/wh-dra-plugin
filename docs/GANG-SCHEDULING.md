# Gang Scheduling with Coscheduling Plugin

Gang scheduling ensures that all N pods in a group are scheduled **atomically** — either all
place successfully, or all remain Pending. Without this, the scheduler can place pod 0 on
node-a while pod 1 waits, letting pod 0 hold its device claim while pod 1 cannot start.

---

## How It Works

The `kubernetes-sigs/scheduler-plugins` project ships a scheduler called
`scheduler-plugins-scheduler`. It includes all default Kubernetes scheduler plugins **plus**
a `Coscheduling` plugin that understands `PodGroup` objects.

```
Default scheduler               scheduler-plugins-scheduler
──────────────────              ────────────────────────────
NodeFit                         NodeFit
VolumeBinding                   VolumeBinding
DynamicResources  ◄── DRA       DynamicResources  ◄── DRA
...                             ...
                                Coscheduling      ◄── gang
```

Pods labeled `scheduling.sigs.k8s.io/pod-group: <name>` and routed via
`schedulerName: scheduler-plugins-scheduler` are held in the scheduling queue until
`PodGroup.spec.minMember` pods can ALL be placed. At that point they are placed together
in one scheduling round.

---

## Install

### Prerequisite

Match the scheduler-plugins version to your Kubernetes server version:

```bash
kubectl version --short   # note the server version, e.g. v1.32.x
```

Releases: https://github.com/kubernetes-sigs/scheduler-plugins/releases
(tag `v0.32.x` corresponds to K8s `v1.32.x`)

### Option 1 — Helm (recommended)

```bash
helm repo add scheduler-plugins https://scheduler-plugins.sigs.k8s.io
helm repo update

# Replace 0.32.7 with the version matching your K8s server.
helm install scheduler-plugins scheduler-plugins/scheduler-plugins \
  --version 0.32.7 \
  --namespace scheduler-plugins \
  --create-namespace
```

Verify:

```bash
kubectl get pods -n scheduler-plugins
# NAME                                           READY   STATUS    RESTARTS
# scheduler-plugins-controller-manager-xxx       1/1     Running   0
# scheduler-plugins-scheduler-xxx                1/1     Running   0
```

### Option 2 — Release manifests

```bash
VERSION=v0.32.7   # match your K8s version

# PodGroup CRD
kubectl apply -f https://github.com/kubernetes-sigs/scheduler-plugins/releases/download/${VERSION}/manifests.yaml

# Scheduler deployment
kubectl apply -f https://github.com/kubernetes-sigs/scheduler-plugins/releases/download/${VERSION}/scheduler-plugins.yaml
```

---

## Usage in deploy/test-scheduler.yaml

Three additions to the standard Job YAML:

### 1. PodGroup (new object)

```yaml
apiVersion: scheduling.sigs.k8s.io/v1alpha1
kind: PodGroup
metadata:
  name: t3k-scheduler-test-gang
  namespace: default
spec:
  minMember: 2            # must equal Job.spec.completions
  scheduleTimeoutSeconds: 60
```

`scheduleTimeoutSeconds`: if the full group hasn't placed within this window, the scheduler
marks all pods `Unschedulable` and they stay Pending until the next retry cycle.

### 2. Pod label (in Job pod template)

```yaml
metadata:
  labels:
    scheduling.sigs.k8s.io/pod-group: t3k-scheduler-test-gang
```

### 3. schedulerName (in pod spec)

```yaml
spec:
  schedulerName: scheduler-plugins-scheduler
```

---

## How This Combines With resourceSliceCount

Two independent guards are now active:

| Guard | Condition blocked | Mechanism |
|---|---|---|
| `resourceSliceCount: 2` | One node plugin down → pool has 1/2 slices | DRA scheduler rejects allocation |
| PodGroup `minMember: 2` | Only one pod can place today | Coscheduling holds both until both fit |

Both must pass before any pod runs. Together they prevent every partial-allocation scenario.

---

## Test Run

```bash
# Apply (PodGroup + ResourceClaimTemplate + Job)
kubectl apply -f deploy/test-scheduler.yaml

# Watch: should see both pods go Pending → Running at the same time
kubectl get pods -l job-name=t3k-scheduler-test -o wide -w

# Expected: one pod on t3k-node-a, one on t3k-node-b, timestamps within seconds
# NAME                        READY  STATUS    NODE
# t3k-scheduler-test-0-xxxxx  1/1    Running   t3k-node-a
# t3k-scheduler-test-1-xxxxx  1/1    Running   t3k-node-b

# Check gang enforcement: kill plugin on node-b, then apply
kubectl rollout restart daemonset/wh-dra-kubelet-plugin -n kube-system \
  --field-selector spec.nodeName=t3k-node-b
kubectl apply -f deploy/test-scheduler.yaml
# Expected: BOTH pods stay Pending (not just pod 1)

# Cleanup
kubectl delete -f deploy/test-scheduler.yaml
```

---

## Troubleshooting

**PodGroup not found / CRD missing**

```
error: resource mapping not found for name "t3k-scheduler-test-gang"
kind "PodGroup"
```

→ The scheduler-plugins CRD is not installed. Run the Helm install above.

**Both pods stuck Pending with event "Gang is not schedulable"**

→ The full group cannot place. Check:
1. Are both nodes healthy? (`kubectl get resourceslice -o yaml | grep -A2 resourceSliceCount`)
2. Does the pool have 2 slices? (`kubectl get resourceslice`)
3. Is the CEL selector matching the right `physical_pod`?

**Pods scheduled by default-scheduler instead**

→ `schedulerName: scheduler-plugins-scheduler` is missing from the pod spec.
Check: `kubectl get pod <name> -o jsonpath='{.spec.schedulerName}'`
