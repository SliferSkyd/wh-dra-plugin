# wh-dra-kubelet-plugin — Team Presentation

> Kubernetes DRA plugin for Tenstorrent Wormhole T3K hardware
> Status: deployed and verified on live cluster (June 2026)

---

## Part 1 — Kubernetes from scratch

This section is for anyone who has not worked with Kubernetes before. Read it end-to-end before the demo.

---

### 1.1 What problem does Kubernetes solve?

Imagine you have 10 bare-metal servers and 50 services to run. Without Kubernetes you have to:

- SSH into each machine and manually start processes
- Remember which service is on which machine
- Restart crashed processes by hand
- Make sure two services don't fight over the same GPU

Kubernetes (K8s) is a *cluster operating system* that automates all of this. You write a YAML file that says **what** you want running, and K8s figures out **where** and **how**.

```
You write:    "I want 2 copies of my inference server, each needing 16 GB RAM"
Kubernetes:   picks machines with free RAM, starts containers, restarts on crash,
              moves workloads away from unhealthy machines automatically
```

---

### 1.2 Nodes and the control plane

A Kubernetes cluster has two kinds of machines:

**Control plane node** — the brain. Runs no workloads. Has three key processes:
- `kube-apiserver` — every other component talks to this. Think of it as the cluster's REST API + database front end.
- `kube-scheduler` — watches for Pods waiting to run and picks a machine for each one.
- `etcd` — a distributed key-value store. All cluster state (pod definitions, node status, …) lives here. If etcd is lost, the cluster is lost.

**Worker nodes** — the muscle. Run actual containers. Each worker has:
- `kubelet` — the local agent. Watches the API server and starts/stops containers on this machine.
- `containerd` — the container runtime. Actually creates and destroys containers (pulls images, manages namespaces, mounts filesystems).
- `kube-proxy` — handles network routing so pods can reach each other across machines.

```
┌────────────────────────────────────────────────────────┐
│  CONTROL PLANE NODE  (192.168.1.60 in our cluster)     │
│                                                        │
│  kube-apiserver   ← all kubectl commands land here    │
│  kube-scheduler   ← decides which node runs each pod  │
│  kube-controller-manager ← reconciliation loops       │
│  etcd             ← all state stored here             │
└──────────────┬─────────────────────────────────────────┘
               │ kubelet on each worker watches API server
     ┌─────────┴──────────┐
     │                    │
┌────┴──────────┐  ┌──────┴─────────┐
│  t3k-node-a   │  │  t3k-node-b    │
│  192.168.1.247│  │  192.168.1.243 │
│               │  │                │
│  kubelet      │  │  kubelet       │
│  containerd   │  │  containerd    │
│  kube-proxy   │  │  kube-proxy    │
└───────────────┘  └────────────────┘
```

---

### 1.3 Pods — the smallest unit

A **Pod** is the smallest deployable unit in Kubernetes. It wraps one or more containers that:
- Share a network interface (same IP address)
- Share mounted volumes
- Always run on the same machine

99% of the time a Pod has exactly one container. Think of a Pod as "one instance of one process."

**Example pod spec:**
```yaml
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

You never start a container directly — you always create a Pod.

---

### 1.4 How a Pod goes from YAML to Running

```
Step 1:  kubectl apply -f my-pod.yaml
         → sends the YAML to the API server over HTTPS
         → API server validates it and writes it to etcd
         → Pod status: Pending

Step 2:  kube-scheduler sees a Pending pod
         → looks at all nodes, checks: available CPU, RAM, labels, taints
         → picks a node and writes "this pod goes to t3k-node-a" into etcd
         → Pod status: Scheduled (still Pending from container perspective)

Step 3:  kubelet on t3k-node-a is watching the API server
         → sees a pod assigned to its node
         → tells containerd: "pull this image and start this container"
         → containerd pulls image, creates container, starts it
         → kubelet reports back: Running

Step 4:  Pod runs your process

Step 5:  Process exits (or you run kubectl delete pod)
         → kubelet tells containerd to stop the container
         → Pod status: Succeeded / Failed
         → etcd updated
```

---

### 1.5 Labels and selectors

**Labels** are key-value tags you put on any Kubernetes object. They have no built-in meaning — you define what they mean.

```bash
# Label a node manually
kubectl label node t3k-node-a tenstorrent.com/arch=wormhole

# See all labels on a node
kubectl get node t3k-node-a --show-labels
```

**Selectors** filter objects by labels. The scheduler uses them. DaemonSets use them. Services use them.

```yaml
# This DaemonSet only runs on nodes that have the wormhole label
spec:
  template:
    spec:
      nodeSelector:
        tenstorrent.com/arch: wormhole
```

In our cluster, `wh-node-labeler` automatically sets these labels on T3K nodes so the DRA plugin DaemonSet deploys there — and nowhere else.

---

### 1.6 Namespaces

A **namespace** is a logical partition inside a cluster. It lets you separate workloads:

| Namespace | What lives there |
|---|---|
| `kube-system` | System components (kubelet plugin, CoreDNS, Calico, …) |
| `default` | Where your workloads go if you don't specify |
| `ttfm` | tt-fabric-manager controller + agents |

```bash
kubectl get pods               # default namespace
kubectl get pods -n kube-system   # system namespace
kubectl get pods -A            # ALL namespaces
```

---

### 1.7 DaemonSet — run one pod on every node

A **DaemonSet** ensures exactly one Pod runs on every node matching a selector. When a new node is added to the cluster and it has the right labels, the DaemonSet automatically schedules a Pod there.

We use DaemonSets for:
- `wh-dra-kubelet-plugin` — the DRA plugin (one per T3K node)
- `wh-node-labeler` — hardware discovery agent (one per T3K node)
- `ttfm-agent` — tt-fabric-manager agent (one per T3K node)

---

### 1.8 ConfigMap — configuration as cluster state

A **ConfigMap** stores arbitrary key-value data inside Kubernetes. Pods can read it as environment variables or mounted files. We use one to store the T3K mesh topology:

```yaml
# tt-node-topology ConfigMap (in kube-system)
data:
  t3k-node-a: "physical-pod=t3k-a host-rank=0 pod-size=2"
  t3k-node-b: "physical-pod=t3k-a host-rank=1 pod-size=2"
```

The `wh-node-labeler` reads this ConfigMap on each T3K node and sets the corresponding node labels automatically.

---

### 1.9 Services and DNS

A **Service** gives a stable DNS name and IP address to a set of pods. Pod IPs change when pods restart; a Service IP never changes.

```
ttfm-agent.ttfm.svc.cluster.local:50053
  ↑ service name
       ↑ namespace
              ↑ cluster-internal DNS suffix
                                  ↑ port
```

`internalTrafficPolicy: Local` — a special mode that routes to a pod on the **same node** as the caller. We use this for the FM agent service so each DRA plugin pod talks to its own node's FM agent, never a remote one.

---

### 1.10 Useful kubectl commands

```bash
# Cluster state
kubectl get nodes -o wide                         # all nodes + IPs
kubectl get pods -A -o wide                       # all pods on all nodes
kubectl describe node t3k-node-a                  # full node info + labels

# Pods in a namespace
kubectl -n kube-system get pods                   # system pods
kubectl -n kube-system logs <pod-name>            # pod log output
kubectl -n kube-system logs <pod-name> -f         # follow live

# Delete and recreate
kubectl delete pod <pod-name> -n kube-system      # pod restarts from DaemonSet
kubectl apply -f deploy/daemonset.yaml            # apply changes

# DRA-specific
kubectl get resourceslices                        # what devices are advertised
kubectl get resourceclaims                        # what devices are allocated
kubectl get deviceclass                           # device type definitions
```

---

### 1.11 K8s workflow: from YAML file to running workload

This section walks through everything that happens when you deploy something — from writing a YAML file to watching the pod run. This is the practical workflow you will use for every workload on the cluster.

---

#### YAML anatomy — every K8s object has the same four fields

```yaml
apiVersion: apps/v1      # which API group handles this type
kind: Deployment         # what kind of object this is
metadata:                # identity
  name: my-app
  namespace: default
  labels:
    app: my-app
spec:                    # desired state — what YOU write
  replicas: 2
  ...
status:                  # actual state — Kubernetes fills this in automatically
  readyReplicas: 2
  ...
```

The critical distinction: **`spec` is what you want; `status` is what exists right now.** Kubernetes continuously reconciles them. If a pod crashes, the `status` drops below `spec`, and a controller creates a replacement.

You never write `status` yourself. You only write `spec`.

Multiple objects can live in one file separated by `---`:
```yaml
kind: ResourceClaim
metadata:
  name: my-claim
spec: ...
---
kind: Pod
metadata:
  name: my-pod
spec: ...
```

---

#### Step 1 — Write the YAML

Describe what you want. Example: a pod that requests a T3K device:
```yaml
# my-workload.yaml
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
---
apiVersion: v1
kind: Pod
metadata:
  name: my-workload
spec:
  resourceClaims:
  - name: t3k
    resourceClaimName: my-t3k-claim
  restartPolicy: Never
  containers:
  - name: app
    image: ubuntu:22.04
    command: ["bash", "-c", "ls /dev/tenstorrent/ && env | grep TT_"]
    resources:
      claims:
      - name: t3k
```

---

#### Step 2 — Apply

```bash
kubectl apply -f my-workload.yaml
```

What happens internally:
```
kubectl apply -f my-workload.yaml
  ↓
sends YAML to kube-apiserver over HTTPS
  ↓
API server validates the schema (rejects bad YAML early)
  ↓
stores objects in etcd
  ↓
ResourceClaim status: Pending
Pod status: Pending
```

`kubectl apply` is **idempotent**: run it again and it only updates what changed. It is safe to run on an already-running workload. Compare with `kubectl create` which errors if the object already exists.

---

#### Step 3 — Watch

```bash
kubectl get pods -w
```

`-w` (watch) streams live updates as pod status changes:
```
NAME          READY   STATUS    RESTARTS   AGE
my-workload   0/1     Pending   0          1s    ← scheduler looking for a node
my-workload   0/1     Pending   0          2s    ← ResourceClaim being allocated
my-workload   0/1     Init:0/1  0          3s    ← kubelet calling PrepareResourceClaims
my-workload   0/1     Running   0          4s    ← container starting
my-workload   1/1     Running   0          5s    ← container healthy
my-workload   0/1     Completed 0          10s   ← process exited 0
```

---

#### Step 4 — Inspect

```bash
# One-line status of all pods
kubectl get pods -o wide

# Full details: spec, status, and most importantly — Events
kubectl describe pod my-workload

# Container stdout/stderr
kubectl logs my-workload

# Follow logs live while the container runs
kubectl logs my-workload -f

# Check the ResourceClaim was allocated and which device was assigned
kubectl describe resourceclaim my-t3k-claim
```

The `Events:` section at the bottom of `kubectl describe pod` is the most useful field for debugging. It shows exactly what each component (scheduler, kubelet, plugin) did and what errors occurred.

---

#### Step 5 — Clean up

```bash
kubectl delete -f my-workload.yaml
```

Deletes all objects defined in the file in reverse order. For DRA workloads: the pod is terminated → kubelet calls `UnprepareResourceClaims` → plugin deletes the CDI spec → ResourceClaim is released → device is free for the next workload.

---

#### Debugging: pod stuck in Pending

`Pending` means the scheduler cannot place the pod. Check the events:

```bash
kubectl describe pod my-workload
# Look for "Events:" at the bottom

# Common messages and their meaning:
"0/2 nodes are available: 2 Insufficient memory"
  → not enough RAM on any node; reduce memory request

"0/2 nodes are available: no device class matched"
  → DeviceClass not found, or no ResourceSlice matches the class CEL
  → check: kubectl get deviceclass
  →        kubectl get resourceslices

"0/2 nodes are available: ResourceClaim not yet allocated"
  → scheduler found a node but claim allocation is still in progress
  → wait a few seconds; if it persists check plugin logs

"unbound immediate PersistentVolumeClaims"
  → a volume claim can't be satisfied; unrelated to DRA
```

---

#### Debugging: pod stuck in ImagePullBackOff

The container image could not be pulled:

```bash
kubectl describe pod my-workload
# Events: Failed to pull image "ghcr.io/...: 403 Forbidden"
# Fix: create an imagePullSecret and reference it in the pod spec

kubectl -n kube-system create secret docker-registry ghcr-credentials \
  --docker-server=ghcr.io \
  --docker-username=<user> \
  --docker-password=<token>
```

---

#### Debugging: pod in CrashLoopBackOff

The container started but exited with a non-zero code:

```bash
kubectl logs my-workload           # logs from last run
kubectl logs my-workload --previous  # logs from the run before that
kubectl describe pod my-workload   # exit code in "Last State"
```

---

#### Our T3K workload end-to-end: the full sequence of kubectl commands

```bash
# 1. Verify the cluster is ready
kubectl get nodes
kubectl get resourceslices          # should show both T3K nodes with chip_count=8

# 2. Apply the DeviceClass (only needed once per cluster)
kubectl apply -f deploy/deviceclass.yaml

# 3. Apply the ResourceClaimTemplate (needed once per namespace)
kubectl apply -f deploy/odin/resourceclaimtemplate.yaml

# 4. Deploy a test workload
kubectl apply -f deploy/test-claim.yaml

# 5. Watch the pod come up
kubectl get pods -w

# 6. Verify device injection
kubectl logs wh-demo-pod
# Expected output:
#   /dev/tenstorrent/0  /dev/tenstorrent/1  /dev/tenstorrent/2  /dev/tenstorrent/3
#   TT_CHIP_COUNT=8
#   TT_MESH_HOST_RANK=0
#   WH_RESOURCE_CLAIM_UID=...

# 7. Check the ResourceClaim was allocated
kubectl describe resourceclaim wh-demo-claim
# Look for: status.allocation.devices.results[0].device = "wormhole-t3k"

# 8. Clean up
kubectl delete -f deploy/test-claim.yaml
kubectl get resourceclaims          # should be empty now
```

---

### 1.12 Helm — the Kubernetes package manager

#### The problem with raw YAML

Deploying an application to Kubernetes means writing many YAML files: a Deployment, a Service,
a ConfigMap, a ServiceAccount, RBAC roles, a DaemonSet, possibly a CRD. For the DRA plugin alone
there are 8+ YAML files. Challenges immediately appear:

- How do you deploy the same application to a dev cluster vs production, with different image tags
  or replica counts?
- How do you upgrade cleanly — apply only the changed files without touching the rest?
- How do you roll back if the new version breaks something?
- How do you share the plugin with another team so they can deploy it in their own cluster?

Helm solves all of these by treating a set of Kubernetes YAML files as one **package** (a *chart*),
with variables that can be customised per deployment.

#### Key concepts

**Chart** — a directory of YAML templates + a `values.yaml` file. Think of it as an npm package
or a Docker image, but for Kubernetes objects.

```
wh-dra-plugin/          ← chart root
  Chart.yaml            ← name, version, description
  values.yaml           ← default variables (image tag, replica count, …)
  templates/
    daemonset.yaml      ← YAML with {{ .Values.image.tag }} placeholders
    serviceaccount.yaml
    clusterrole.yaml
    configmap.yaml
    …
```

**Values** — variables that change between deployments. In `values.yaml`:
```yaml
image:
  repository: harbor.moreh.io/wh-dra-kubelet-plugin
  tag: v0.1.0

plugin:
  fmAddr: ""            # empty = FM disabled
  healthInterval: 30s
```

A user can override any value without editing the templates:
```bash
helm install wh-dra ./chart --set image.tag=v0.2.0 --set plugin.fmAddr=ttfm-agent:50051
```

**Release** — one installed instance of a chart in a cluster. The same chart can be installed
multiple times under different release names (e.g. dev vs staging).

**Registry** — Helm charts can be pushed to an OCI registry (Harbor, GHCR) just like Docker
images. A team deploys by pulling the chart rather than cloning the repo.

#### Key Helm commands

```bash
# Install the chart for the first time
helm install wh-dra ./deploy/chart -n kube-system

# Upgrade to a new version (applies only what changed)
helm upgrade wh-dra ./deploy/chart -n kube-system --set image.tag=v0.2.0

# Roll back to the previous release
helm rollback wh-dra 1 -n kube-system

# See all deployed releases
helm list -A

# See what Kubernetes objects a release owns
helm get manifest wh-dra -n kube-system

# Uninstall (deletes all objects the chart created)
helm uninstall wh-dra -n kube-system

# Render templates locally without applying (useful for debugging)
helm template wh-dra ./deploy/chart --set image.tag=v0.2.0
```

#### Why Helm matters for our plugin

Without Helm, deploying the DRA plugin to a new cluster means:
1. Clone the repo
2. Manually edit image tags in 3 YAML files
3. Apply 8 files in the right order
4. Hope you didn't miss one

With Helm:
```bash
helm install wh-dra oci://harbor.moreh.io/charts/wh-dra-plugin --version 0.2.0 \
  --set plugin.fmAddr=ttfm-agent:50051
```
One command. Versioned. Reproducible. Rollback in 10 seconds if something breaks.

---

### 1.13 GitHub CI/CD — automating build and deploy

#### What is CI/CD?

**CI (Continuous Integration)** — every time code is pushed, automatically:
- Build the binary / Docker image
- Run tests
- Report pass or fail on the pull request

**CD (Continuous Deployment)** — when code is merged, automatically:
- Build and push the production image
- Deploy to the cluster (via Helm upgrade)
- Verify the rollout succeeded

The goal: a developer pushes code and the change is running on the cluster within minutes, with
no manual steps.

#### GitHub Actions basics

GitHub Actions is GitHub's built-in CI/CD system. A **workflow** is a YAML file in
`.github/workflows/` that describes jobs and steps triggered by events (push, pull_request,
release).

```yaml
# .github/workflows/build.yaml
name: Build and Deploy

on:
  push:
    branches: [main]       # trigger: every push to main

jobs:
  build:
    runs-on: ubuntu-latest
    steps:
    - uses: actions/checkout@v4

    - name: Build Docker image
      run: docker build -t wh-dra-kubelet-plugin:${{ github.sha }} .

    - name: Push to registry
      run: |
        docker tag wh-dra-kubelet-plugin:${{ github.sha }} harbor.moreh.io/wh-dra-kubelet-plugin:${{ github.sha }}
        docker push harbor.moreh.io/wh-dra-kubelet-plugin:${{ github.sha }}

    - name: Deploy to cluster
      run: |
        helm upgrade wh-dra ./deploy/chart \
          --set image.tag=${{ github.sha }} \
          --namespace kube-system \
          --wait    # block until all DaemonSet pods are Running
```

#### Our specific CI/CD pipeline

```
Developer pushes to PR
        │
        ▼
GitHub Actions: CI job
  ├── go build ./...         # compile plugin binary
  ├── go test ./...          # run unit tests
  └── docker build           # build image (not pushed — just validates)
        │
        │  (PR approved + merged to main)
        ▼
GitHub Actions: CD job
  ├── docker build
  ├── docker push → harbor.moreh.io/wh-dra-kubelet-plugin:<git-sha>
  ├── helm upgrade wh-dra    # updates DaemonSet image tag
  └── kubectl rollout status daemonset/wh-dra-kubelet-plugin
              │
              ▼
        DaemonSet rolling update:
          t3k-node-a: old pod terminated → new pod started → Ready
          t3k-node-b: old pod terminated → new pod started → Ready
              │
              ▼
        New ResourceSlice published by new plugin version
        Scheduler immediately sees updated devices
```

#### Rolling update — zero downtime for running workloads

When Helm upgrades the DaemonSet, Kubernetes does a **rolling update** — it replaces pods one
node at a time:

1. New pod starts on `t3k-node-a` alongside the old one
2. New pod becomes Ready (plugin registered, ResourceSlice published)
3. Old pod is terminated
4. Repeat for `t3k-node-b`

Running workloads are not affected because the plugin pod is separate from workload pods. The
only brief gap is between old pod termination and new pod Ready — during which the scheduler
uses the last-known ResourceSlice (stale by seconds, safe in practice).

#### Environment-specific deployment

The same chart ships to multiple environments, each with different values:

```
main branch push → dev cluster
  helm upgrade wh-dra ./chart --set image.tag=<sha> --set plugin.healthInterval=10s

release tag v0.2.0 → production cluster
  helm upgrade wh-dra oci://harbor.moreh.io/charts/wh-dra-plugin --version 0.2.0 \
    --set image.tag=v0.2.0 --set plugin.healthInterval=60s
```

One chart, two clusters, two environments — no code duplication, no manual file editing.

---

## Part 2 — The Problem

Before this plugin, running AI workloads on T3K inside Kubernetes required:

**Manual placement**
```yaml
# Every workload had to hard-code the node name
spec:
  nodeName: t3k-node-a   # manual, breaks if node is renamed
```

**Privileged containers everywhere**
```yaml
# Every workload pod needed this — a security hole
securityContext:
  privileged: true
# AND explicit device mounts
volumes:
- name: dev-tenstorrent
  hostPath:
    path: /dev/tenstorrent
```

**No double-allocation prevention** — two pods could race and both try to use the same chip.

**No health feedback** — if a chip stalled, the scheduler kept sending new workloads to the broken node.

**No automatic environment setup** — rank, pod size, peer addresses had to be configured manually per workload YAML.

---

## Part 3 — The Solution: Kubernetes DRA

**Dynamic Resource Allocation (DRA)** is a Kubernetes feature (graduated to GA in v1.35) that lets hardware vendors write plugins that integrate with the scheduler and runtime.

### Three new Kubernetes object types

| Object | Who creates it | What it does |
|---|---|---|
| `ResourceSlice` | **Plugin** (us, automatically) | Advertises hardware to the scheduler |
| `DeviceClass` | Admin, once | Defines a named category of device |
| `ResourceClaim` | Workload user | "I need one T3K device" |

### What changes for the workload author

**Before (manual, privileged):**
```yaml
spec:
  nodeName: t3k-node-a
  securityContext:
    privileged: true
  volumes:
  - name: dev
    hostPath:
      path: /dev/tenstorrent
  containers:
  - name: vllm
    env:
    - name: TT_MESH_HOST_RANK
      value: "0"
    - name: TT_CHIP_COUNT
      value: "4"
    volumeMounts:
    - name: dev
      mountPath: /dev/tenstorrent
```

**After (DRA, no privilege needed):**
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
# That's it. Device, hugepages, env vars arrive automatically.
```

---

## Part 3.5 — DRA Core Concepts

This section covers the DRA API in depth. Skim if you already know K8s DRA; read carefully if you are implementing or debugging claims.

---

### 3.5.1 ResourceClaim vs ResourceClaimTemplate

Two patterns for requesting a device:

**Pattern A — ResourceClaim (direct, manually managed)**

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaim
metadata:
  name: my-t3k-claim
  namespace: default
spec:
  devices:
    requests:
    - name: t3k
      exactly:
        deviceClassName: t3k.wormhole.tenstorrent.com
```

Pod references it by name:
```yaml
spec:
  resourceClaims:
  - name: my-device
    resourceClaimName: my-t3k-claim    # must already exist in same namespace
  containers:
  - name: app
    resources:
      claims:
      - name: my-device
```

- Multiple Pods can reference the same ResourceClaim (soft node affinity — scheduler tries to land them on the same node)
- Lifecycle managed by the operator; claim persists until manually deleted
- Use when: shared device across containers, or you need explicit lifecycle control

**Pattern B — ResourceClaimTemplate (per-Pod, auto-generated)**

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata:
  name: wh-t3k-template
  namespace: default
spec:
  spec:
    devices:
      requests:
      - name: t3k
        exactly:
          deviceClassName: t3k.wormhole.tenstorrent.com
```

Pod references the template:
```yaml
spec:
  resourceClaims:
  - name: my-device
    resourceClaimTemplateName: wh-t3k-template    # Kubernetes auto-creates one claim per Pod
  containers:
  - name: app
    resources:
      claims:
      - name: my-device
```

- Kubernetes creates one ResourceClaim per Pod when the Pod is created
- That claim is deleted automatically when the Pod exits
- Use when: each Pod needs its own exclusive device (Jobs, StatefulSets, vLLM instances)

| | ResourceClaim | ResourceClaimTemplate |
|---|---|---|
| Device exclusivity | Shared across pods | Per-pod exclusive |
| Lifecycle | Manual | Tied to Pod, auto-deleted |
| Good for | Shared device (monitor sidecar + trainer) | Standard workloads, batch Jobs |

---

### 3.5.2 DeviceClass — defining device categories

A **DeviceClass** is a cluster-scoped object that defines a category of devices. It uses CEL (Common Expression Language) to select which devices from published ResourceSlices belong to this class.

```yaml
apiVersion: resource.k8s.io/v1
kind: DeviceClass
metadata:
  name: t3k.wormhole.tenstorrent.com   # workloads reference this name
spec:
  selectors:
  - cel:
      expression: device.driver == "wormhole.tenstorrent.com"
```

This selects all devices in any ResourceSlice whose driver field is `wormhole.tenstorrent.com`.

A narrower DeviceClass could also filter on chip architecture:
```yaml
spec:
  selectors:
  - cel:
      expression: >
        device.driver == "wormhole.tenstorrent.com" &&
        device.attributes["tenstorrent.com/arch"].string == "wormhole"
```

DeviceClasses are created by the admin once. Workload authors reference the class by name and do not need to know which nodes have the hardware — that is expressed in the class and in their per-claim CEL selectors.

---

### 3.5.3 CEL selectors — fine-grained device filtering

Both DeviceClass and ResourceClaim support CEL expressions that access device properties published in the ResourceSlice:

| CEL accessor | Return type | Example |
|---|---|---|
| `device.driver` | string | `device.driver == "wormhole.tenstorrent.com"` |
| `device.attributes["domain/key"].string` | string | `device.attributes["tenstorrent.com/chip_arch"].string == "wormhole_b0"` |
| `device.attributes["domain/key"].int` | int64 | `device.attributes["tenstorrent.com/host_rank"].int == 0` |
| `device.attributes["domain/key"].bool` | bool | — |
| `device.capacity["domain/key"]` | quantity | `device.capacity["tenstorrent.com/memory"] >= quantity("96Gi")` |

**Example: claim a device from a 2-node physical pod with at least 96 Gi memory:**
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

**Example: rank-0 leader node only:**
```yaml
selectors:
- cel:
    expression: device.attributes["tenstorrent.com/host_rank"].int == 0
```

CEL is evaluated by the scheduler against ResourceSlice data during allocation. No driver code runs at this point — the scheduler resolves it entirely from the published attributes.

---

### 3.5.4 Device sharing

Multiple containers in the same Pod can share one ResourceClaim by listing the same claim name in `resources.claims`:

```yaml
spec:
  resourceClaims:
  - name: shared-t3k
    resourceClaimTemplateName: wh-t3k-template
  containers:
  - name: trainer
    resources:
      claims:
      - name: shared-t3k     # both containers receive the same injected device nodes
  - name: monitor-sidecar
    resources:
      claims:
      - name: shared-t3k
```

When multiple Pods reference the same manually-created ResourceClaim, the scheduler tries to place them all on the same node (soft affinity — best-effort, not guaranteed).

---

### 3.5.5 How the scheduler allocates (Structured Parameters)

Since K8s 1.35, the scheduler resolves device allocation entirely from ResourceSlice data — no driver webhook or roundtrip:

```
1. Scheduler receives a Pod with a ResourceClaim in spec

2. Reads all ResourceSlices from etcd
   (each driver published its slices when it started)

3. For each device in each slice:
   evaluates DeviceClass CEL → is this the right device category?
   evaluates ResourceClaim CEL selectors → does it satisfy workload requirements?

4. Selects a node + device combination satisfying all constraints

5. Writes allocation result into ResourceClaim.status:
   allocation:
     devices:
       results:
       - driver:  "wormhole.tenstorrent.com"
         pool:    "t3k-node-a"
         device:  "wormhole-t3k"
         request: "t3k"

6. Binds Pod to that node

7. kubelet calls PrepareResourceClaims([claim])
   → plugin reads status.allocation.devices.results
   → calls profile.DeviceNodePaths("wormhole-t3k")
   → writes CDI spec file
   → returns CDI device IDs to kubelet
```

The driver's only responsibility is:
1. Publish accurate ResourceSlices with correct attributes and capacity
2. Handle `PrepareResourceClaims` / `UnprepareResourceClaims` callbacks

---

### 3.5.6 ResourceClaim lifecycle

```
kubectl apply -f claim.yaml   (or Pod with template is created)
        │
  ┌─────▼──────┐
  │  Pending   │  no pod has referenced it yet, OR
  │            │  scheduler hasn't found a matching device
  └─────┬──────┘
        │ scheduler allocates a matching device
  ┌─────▼──────────────┐
  │ Allocated          │  status.allocation set
  │                    │  device reserved; pod can now be bound
  └─────┬──────────────┘
        │ pod bound to node → kubelet calls PrepareResourceClaims
  ┌─────▼──────────────┐
  │ Reserved (in use)  │  CDI spec file written
  │                    │  container starts with device injected
  └─────┬──────────────┘
        │ pod exits → kubelet calls UnprepareResourceClaims
  ┌─────▼──────────────┐
  │ Deallocated        │  allocation cleared from status
  │                    │  CDI spec file deleted
  └─────┬──────────────┘
        │
  if from ResourceClaimTemplate → Kubernetes deletes the claim
  if direct ResourceClaim       → claim stays, can be reallocated
```

The plugin's checkpoint file persists which claims were prepared across plugin restarts. If the plugin crashes between `PrepareResourceClaims` and pod start, the checkpoint ensures correct recovery when kubelet re-queries.

---

### 3.5.7 Prioritized list — fallback device alternatives

Since K8s v1.36, a request can contain multiple subrequests in priority order. The scheduler tries each in sequence:

```yaml
spec:
  devices:
    requests:
    - name: t3k-preferred
      subrequests:
      - devices:
          requests:
          - exactly:
              deviceClassName: t3k.wormhole.tenstorrent.com
              selectors:
              - cel:
                  expression: device.attributes["tenstorrent.com/pod_size"].int == 2
      - devices:
          requests:
          - exactly:
              deviceClassName: t3k.wormhole.tenstorrent.com
              # fallback: any T3K (any pod_size)
```

Scheduler tries subrequest 0 first (pod_size=2). If no matching device exists, tries subrequest 1 (any T3K). Useful for expressing hardware preference without hard-coding node names.

---

### 3.5.8 Device health monitoring in DRA

Drivers report health by modifying their ResourceSlice. The scheduler will not allocate unhealthy devices.

**Our approach — publish empty pool when any chip is inaccessible:**
```go
// driver.go, publishResourceSlice when !d.healthy:
resources = resourceslice.DriverResources{
    Pools: map[string]resourceslice.Pool{
        d.manager.nodeName: {Slices: nil},   // zero devices → scheduler places nothing new here
    },
}
```

**K8s v1.36+ per-device health conditions:**
```yaml
devices:
- name: wormhole-t3k
  conditions:
  - type: Unhealthy
    status: "True"
    reason: DeviceNotAccessible
    message: "os.Open /dev/tenstorrent/2: no such file or directory"
```

With per-device conditions, healthy devices on the same node can still be allocated. Our current empty-pool approach is simpler but blocks the whole node.

**Kubelet DRA metrics (K8s v1.36+):**
```
kubelet_dra_devices_allocated_total   — currently in use
kubelet_dra_devices_available_total   — free to allocate
kubelet_dra_devices_unhealthy_total   — marked unhealthy
```

**P99 latency queries for production monitoring:**
```promql
# End-to-end scheduling latency including resource allocation:
histogram_quantile(0.99,
  sum(increase(scheduler_pod_scheduling_sli_duration_seconds_bucket[5m])) by (le))

# PrepareResources call duration (kubelet → plugin gRPC):
histogram_quantile(0.99,
  sum(rate(dra_operations_duration_seconds_bucket
          {operation_name="PrepareResources"}[5m])) by (le))
```

---

### 3.5.9 How the four DRA objects map to this repo

#### DeviceClass

**What it is:** A cluster-wide catalog entry that names a category of device. It uses a CEL expression to declare which devices belong to this class. Applied once by an admin. Workload authors reference it by name — they do not need to know which nodes have the hardware.

**In this repo:** `deploy/deviceclass.yaml`
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

The string `"wormhole.tenstorrent.com"` must match the `driverName` constant in `cmd/wh-dra-kubelet-plugin/driver.go:21`. The scheduler uses this to decide: "any device published by that driver qualifies as a T3K device."

Apply once:
```bash
kubectl apply -f deploy/deviceclass.yaml
```

---

#### ResourceSlice

**What it is:** The plugin's advertisement to the scheduler. Each plugin instance publishes one ResourceSlice for its node, listing the devices it manages along with their attributes and capacity. The scheduler reads all ResourceSlices cluster-wide when looking for a node that can satisfy a ResourceClaim.

**In this repo:** Not a static file — the plugin builds and publishes it at runtime. The code path:

```
startup
  └── driver.go NewDriver()
        └── driver.go publishResourceSlice()
              ├── FM path:  tenstorrent.go EnumerateDevices()
              │     calls FM agent GetTopology()
              │     bundleHostASICs() → groups MMIO + remote chips
              │     buildDevice()     → constructs resourceapi.Device
              │                          with all attributes + Capacity 96Gi
              └── fallback: driver.go labelBasedResources()
                    builds device from node labels only (no FM data)

both paths call:
  helper.PublishResources(ctx, resources)
    → writes ResourceSlice to K8s API server
       (scheduler can now see it)
```

Key files and lines:

| File | Role |
|---|---|
| `cmd/wh-dra-kubelet-plugin/driver.go:104` | `publishResourceSlice()` — decides FM vs fallback path |
| `internal/profiles/tenstorrent/tenstorrent.go:81` | `EnumerateDevices()` — calls FM, builds Device with FM attributes + 96Gi capacity |
| `internal/profiles/tenstorrent/tenstorrent.go:130` | `buildDevice()` — constructs `resourceapi.Device{Attributes, Capacity}` |
| `cmd/wh-dra-kubelet-plugin/driver.go:169` | `labelBasedResources()` — fallback; uses node labels, no FM data |

Inspect the live object on the cluster:
```bash
kubectl get resourceslice -o yaml
# spec.devices[0].attributes  → the map built by buildDevice()
# spec.devices[0].capacity    → tenstorrent.com/memory: 96Gi
```

---

#### ResourceClaim

**What it is:** A workload's request for a device. It names a DeviceClass and optionally adds CEL filters to narrow the selection. The scheduler finds a matching ResourceSlice, writes the allocation result into `status.allocation`, and binds the pod to that node. The device is held exclusively until the pod exits and `UnprepareResourceClaims` is called.

**In this repo — as YAML test fixtures:**

| File | What it tests |
|---|---|
| `deploy/test-claim.yaml` | Single claim + single pod — basic smoke test |
| `deploy/test-two-pods.yaml` | Two claims for the same DeviceClass — proves only one pod holds the device at a time |

```yaml
# deploy/test-claim.yaml
kind: ResourceClaim
metadata:
  name: wh-demo-claim
spec:
  devices:
    requests:
    - name: t3k
      exactly:
        deviceClassName: t3k.wormhole.tenstorrent.com  # ← references our DeviceClass
```

**In this repo — consumed by the plugin:** `cmd/wh-dra-kubelet-plugin/state.go:54`

When kubelet calls `PrepareResourceClaims`, the claim object is already allocated — it carries `Status.Allocation.Devices.Results`. The plugin reads that to find out which device the scheduler assigned, then resolves the `/dev` paths for CDI injection:

```go
// state.go:150
for _, result := range claim.Status.Allocation.Devices.Results {
    // result.Device == "wormhole-t3k"
    p, _ := s.profile.DeviceNodePaths(result.Device)
    // p == ["/dev/tenstorrent/0", "1", "2", "3"]
    paths = append(paths, p...)
}
// → cdi.CreateClaimSpecFile(uid, paths)
```

The `UnprepareResourceClaims` path (`state.go:163`) deletes the CDI spec file and removes the entry from the checkpoint, releasing the device.

---

#### ResourceClaimTemplate

**What it is:** A template that Kubernetes uses to auto-generate one ResourceClaim per pod. The template itself is permanent; the generated claims are pod-scoped — created when the pod is created, deleted when the pod exits. This is the standard pattern for batch workloads and deployments where each pod needs its own exclusive device.

**In this repo:**

| File | Used by |
|---|---|
| `deploy/odin/resourceclaimtemplate.yaml` | Odin InferenceService pods — must be applied to every workload namespace before deploying |
| `deploy/multinode/test-statefulset-two-t3k.yaml` | 2-node StatefulSet test — template defined inline, one per replica |
| `deploy/vllm-qwen3-2node-dra.yaml` | vLLM Deployment with `replicas: 2` — K8s creates one claim per replica |

```yaml
# deploy/odin/resourceclaimtemplate.yaml
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

Pod references it:
```yaml
spec:
  resourceClaims:
  - name: wh-t3k
    resourceClaimTemplateName: wh-t3k-template   # ← K8s creates "wh-t3k-template-<pod-uid>"
  containers:
  - name: vllm
    resources:
      claims:
      - name: wh-t3k
```

The plugin (`state.go`) does not distinguish between a manually created ResourceClaim and one auto-generated from a template — both arrive at `PrepareResourceClaims` as `*resourceapi.ResourceClaim` objects with the same structure.

---

#### Runtime connection between all four

```
Admin (once):
  kubectl apply -f deploy/deviceclass.yaml
    → DeviceClass "t3k.wormhole.tenstorrent.com"
       CEL: device.driver == "wormhole.tenstorrent.com"

Plugin (per node, at startup):
  tenstorrent.go EnumerateDevices()
  driver.go publishResourceSlice()
    → ResourceSlice on t3k-node-a
         driver: "wormhole.tenstorrent.com"   ← matches DeviceClass CEL
         device: "wormhole-t3k"
         chip_count=8, memory=96Gi, ...

User (per workload):
  kubectl apply -f deploy/odin/resourceclaimtemplate.yaml
    → ResourceClaimTemplate "wh-t3k-template"
  kubectl apply -f my-pod.yaml  (references wh-t3k-template)
    → K8s creates ResourceClaim "wh-t3k-template-<uid>"
         deviceClassName: t3k.wormhole.tenstorrent.com

Scheduler:
  DeviceClass CEL matches "wormhole-t3k" device in ResourceSlice on t3k-node-a
  Writes ResourceClaim.status.allocation:
    device: "wormhole-t3k", pool: "t3k-node-a"
  Binds pod to t3k-node-a

kubelet → plugin PrepareResourceClaims(claim):
  state.go reads claim.Status.Allocation → "wormhole-t3k"
  profile.DeviceNodePaths()              → [/dev/tenstorrent/0..3]
  cdi.CreateClaimSpecFile()              → pod container gets devices injected
```

**Owner summary:**

| Object | Who writes it | File in repo | Who reads it |
|---|---|---|---|
| `DeviceClass` | Admin, once | `deploy/deviceclass.yaml` | Scheduler (CEL filter), workload YAML (`deviceClassName`) |
| `ResourceSlice` | Plugin at runtime | `driver.go` + `tenstorrent.go` | Scheduler (allocation decisions) |
| `ResourceClaim` | User YAML or K8s (from template) | `deploy/test-claim.yaml`, etc. | Scheduler (sets `status.allocation`), plugin `state.go` (reads allocation on Prepare) |
| `ResourceClaimTemplate` | User, once per namespace | `deploy/odin/resourceclaimtemplate.yaml` | K8s controller (generates per-pod ResourceClaims) |

---

## Part 4 — System Architecture

### 4.1 Full component map

```
┌──────────────────────────────────────────────────────────────────┐
│  CONTROL PLANE (192.168.1.60)                                    │
│                                                                  │
│  kube-apiserver  ←─────────────────── kubectl (your laptop)     │
│        │                                                         │
│  kube-scheduler  ←── reads ResourceSlice, places pods           │
│  etcd            ←── stores all state                           │
│                                                                  │
│  tt-fabric-manager controller  (ttfm namespace)                 │
│    port :50052 gRPC ← agents register here                      │
│    port :8080  web UI                                            │
└──────────────────────┬───────────────────────────────────────────┘
                       │ Kubernetes API
          ┌────────────┴────────────┐
          │                         │
┌─────────┴──────────┐   ┌──────────┴─────────┐
│   t3k-node-a        │   │   t3k-node-b        │
│   192.168.1.247     │   │   192.168.1.243      │
│                     │   │                     │
│ ┌─────────────────┐ │   │ ┌─────────────────┐ │
│ │ wh-node-labeler │ │   │ │ wh-node-labeler │ │
│ │ (reads tt-smi,  │ │   │ │                 │ │
│ │  sets labels)   │ │   │ │                 │ │
│ └─────────────────┘ │   │ └─────────────────┘ │
│                     │   │                     │
│ ┌─────────────────┐ │   │ ┌─────────────────┐ │
│ │  ttfm-agent     │ │   │ │  ttfm-agent     │ │
│ │  :50053 gRPC    │ │   │ │  :50053 gRPC    │ │
│ │  (discovers     │ │   │ │                 │ │
│ │   chip topology)│ │   │ │                 │ │
│ └────────┬────────┘ │   │ └────────┬────────┘ │
│          │          │   │          │          │
│ ┌────────┴────────┐ │   │ ┌────────┴────────┐ │
│ │ wh-dra-plugin   │ │   │ │ wh-dra-plugin   │ │
│ │ ① reads FM topo │ │   │ │                 │ │
│ │ ② publishes     │ │   │ │                 │ │
│ │    ResourceSlice│ │   │ │                 │ │
│ │ ③ writes CDI    │ │   │ │                 │ │
│ │    spec files   │ │   │ │                 │ │
│ └─────────────────┘ │   │ └─────────────────┘ │
│                     │   │                     │
│  /dev/tenstorrent/  │   │  /dev/tenstorrent/  │
│  0 1 2 3 (MMIO)     │   │  0 1 2 3 (MMIO)     │
│  4 5 6 7 (remote)   │   │  4 5 6 7 (remote)   │
│                     │   │                     │
│  workload pod       │   │  workload pod       │
│  (rank=0)           │   │  (rank=1)           │
└─────────────────────┘   └─────────────────────┘
        └──── Tenstorrent Ethernet mesh ─────────┘
```

### 4.2 What each component does

| Component | Namespace | Role |
|---|---|---|
| `wh-node-labeler` | kube-system | Runs `tt-smi`, reads topology ConfigMap, patches node labels every 5 min |
| `ttfm-agent` | ttfm | Per-node FM agent; discovers all 8 chips (4 MMIO + 4 remote) via UMD |
| `ttfm-controller` | ttfm | Cluster aggregator; agents register here; serves combined topology |
| `wh-dra-plugin` | kube-system | Calls FM agent at startup, publishes ResourceSlice, handles CDI injection |

---

## Part 5 — Inside the DRA Plugin

### 5.1 What happens at plugin startup

```
Plugin pod starts on t3k-node-a
  │
  ├── 1. Read node labels from Kubernetes API
  │       arch=wormhole, chip-count=4, host-rank=0, pod-size=2, physical-pod=t3k-a
  │
  ├── 2. Scan /dev/tenstorrent/ to count device files
  │       /dev/tenstorrent/0  /dev/tenstorrent/1  /dev/tenstorrent/2  /dev/tenstorrent/3
  │       (these are the 4 MMIO-capable chips — have PCIe connection to host CPU)
  │
  ├── 3. Dial FM agent at ttfm-agent.ttfm.svc.cluster.local:50053
  │       call GetTopology()
  │       → returns 8 ASICs: 4 MMIO + 4 remote (non-MMIO, only reachable via MMIO fd)
  │       → returns MemoryBytes per chip, PciAddress, ChipArch
  │
  ├── 4. Build and publish ResourceSlice to API server
  │       device: wormhole-t3k
  │         chip_count=8, mmio_chip_count=4, remote_chip_count=4
  │         chip_arch=wormhole_b0
  │         pci_addresses=0000:00:10.0,...
  │         remote_chip_ids=4,5,6,7
  │         memory=96Gi
  │
  └── 5. Write CDI common spec file
          /var/run/cdi/k8s.wormhole.tenstorrent.com-t3k-common.yaml
          (contains env vars + hugepages mount — same for every pod on this node)
```

If the FM agent is not ready yet (still starting), the plugin publishes a label-based fallback ResourceSlice immediately and retries FM every 10 seconds in the background until it succeeds.

### 5.2 MMIO vs non-MMIO chips

On a T3K board, 8 Wormhole chips are connected to each other via high-speed chip-to-chip links. But only 4 of them have a PCIe connection to the host CPU:

```
Host CPU
  │ PCIe
  ├── chip 0 (MMIO) ← /dev/tenstorrent/0   ← software opens this fd
  ├── chip 1 (MMIO) ← /dev/tenstorrent/1
  ├── chip 2 (MMIO) ← /dev/tenstorrent/2
  └── chip 3 (MMIO) ← /dev/tenstorrent/3
       │  chip-to-chip links (not PCIe)
       ├── chip 4 (remote) ← NO /dev entry — reached through chip 0's fd
       ├── chip 5 (remote) ← NO /dev entry — reached through chip 1's fd
       ├── chip 6 (remote) ← NO /dev entry
       └── chip 7 (remote) ← NO /dev entry
```

The plugin groups remote chips with their MMIO parent into a **bundle**. The CDI spec injects only `/dev/tenstorrent/0-3` — the runtime library then reaches chips 4-7 automatically through those fds.

### 5.3 What gets published in the ResourceSlice

```
ResourceSlice: t3k-node-a-wormhole.tenstorrent.com
  pool: t3k-node-a
  device: wormhole-t3k
    attributes:
      tenstorrent.com/arch           = "wormhole"
      tenstorrent.com/chip_arch      = "wormhole_b0"        ← from FM
      tenstorrent.com/chip_count     = 8                    ← from FM (4 MMIO + 4 remote)
      tenstorrent.com/mmio_chip_count = 4                   ← from FM
      tenstorrent.com/remote_chip_count = 4                 ← from FM
      tenstorrent.com/remote_chip_ids = "4,5,6,7"           ← from FM
      tenstorrent.com/pci_addresses  = "0000:00:10.0,..."   ← from FM
      tenstorrent.com/board_type     = "n300"               ← from node label
      tenstorrent.com/physical_pod   = "t3k-a"              ← from node label
      tenstorrent.com/host_rank      = 0                    ← from node label
      tenstorrent.com/pod_size       = 2                    ← from node label
    capacity:
      tenstorrent.com/memory = 96Gi                         ← from FM (sum of all 8 chips)
```

The scheduler uses these attributes with CEL expressions. Example DeviceClass selector:
```yaml
selectors:
- cel:
    expression: device.driver == "wormhole.tenstorrent.com"
```

### 5.4 What happens when a pod is scheduled

```
User: kubectl apply -f my-workload.yaml

① API server stores ResourceClaim (status: Pending) + Pod (status: Pending)

② kube-scheduler:
   - finds ResourceSlice on t3k-node-a matching DeviceClass "t3k.wormhole.tenstorrent.com"
   - allocates the device (marks it in ResourceClaim.status.allocation)
   - binds pod to t3k-node-a

③ kubelet on t3k-node-a:
   - calls wh-dra-plugin PrepareResourceClaims([my-claim])

④ Plugin PrepareResourceClaims:
   - reads ResourceClaim.status.allocation.devices.results → device name = "wormhole-t3k"
   - calls profile.DeviceNodePaths("wormhole-t3k") → ["/dev/tenstorrent/0", "1", "2", "3"]
   - stats each path to get major/minor numbers
   - writes /var/run/cdi/k8s.wormhole.tenstorrent.com-t3k-<uid>.yaml:
       deviceNodes:
         - path: /dev/tenstorrent/0  major: 236  minor: 0
         - path: /dev/tenstorrent/1  major: 236  minor: 1
         - ...
       env:
         - WH_RESOURCE_CLAIM_UID=<uid>
   - saves state to checkpoint (crash safety)
   - returns CDI device IDs to kubelet

⑤ kubelet passes CDI IDs to containerd

⑥ containerd starts container:
   - reads CDI common spec  → injects TT_CHIP_COUNT=8, TT_MESH_HOST_RANK=0,
                                       hugepages mount, tt_logs mount
   - reads CDI claim spec   → injects /dev/tenstorrent/0-3 device nodes,
                                       WH_RESOURCE_CLAIM_UID=<uid>
   Container starts. Zero privileged: true. Zero hostPath. Everything injected.

⑦ Pod runs workload

⑧ Pod finishes → kubelet calls UnprepareResourceClaims
   - plugin deletes CDI claim spec file
   - removes from checkpoint
   - ResourceClaim status: released
   - Next pod can now claim the device
```

### 5.5 Health monitoring

A background goroutine runs for the plugin's lifetime:

```
every 30 seconds:
  for each chip 0..N-1:
    try os.Open("/dev/tenstorrent/<i>")
      success → chip present, kernel driver loaded
      ENOENT  → chip missing or driver crashed

  if all chips accessible:  publish full ResourceSlice
  if any chip missing:      publish EMPTY ResourceSlice
                            → scheduler stops placing new pods here
                            → already running pods are not affected
```

If FM is enabled, health flips also re-run `EnumerateDevices` so the ResourceSlice is refreshed from new FM topology data.

---

## Part 6 — tt-fabric-manager Integration

### 6.1 What is tt-fabric-manager?

The **Tenstorrent Fabric Manager (TTFM)** is a system daemon that discovers the physical topology of Wormhole ASICs on a node using the UMD (User Mode Driver) API — the same API that workloads use to communicate with the chips.

TTFM has two components:

| Component | Where it runs | What it does |
|---|---|---|
| **Controller** | Control plane node | Aggregates topology from all agents, serves cluster-wide view |
| **Agent** | Each T3K node | Discovers local chips via UMD, registers with controller |

Discovery methods:
- **UMD** (preferred) — opens each chip, reads chip-to-chip connection table, finds all 8 chips including remotes. Only works when chips are not busy.
- **FALLBACK** — PCIe scan only. Finds 4 MMIO chips, misses remotes. Used when chips are held by a running workload.

### 6.2 Why integrate with FM?

Before FM integration, the plugin only knew what node labels said:

```
chip_count = 4    ← wrong! node has 8 chips (4 MMIO + 4 remote)
memory     = ?    ← unknown
chip_arch  = ?    ← unknown
```

After FM integration, the plugin calls `GetTopology()` at startup and learns the full picture:

```
chip_count       = 8      ← correct: 4 MMIO + 4 remote
mmio_chip_count  = 4      ← how many /dev entries exist
remote_chip_count = 4     ← how many are reachable through MMIO fds
chip_arch        = wormhole_b0
memory           = 96Gi   ← 12 GB × 8 chips
pci_addresses    = 0000:00:10.0, ...
```

### 6.3 FM timeline on node boot

```
T+0s   kernel loads tt-kmd → /dev/tenstorrent/0-3 appear
T+5s   ttfm-agent starts → tries UMD discovery
       if chips free:   UMD succeeds → 8 ASICs, 40 local connections
       if chips busy:   FALLBACK → 4 ASICs, 0 connections

T+10s  wh-dra-plugin starts → calls FM agent GetTopology()
       if FM ready:    publishes FM-enriched ResourceSlice
       if FM not ready: publishes label-based fallback, retries every 10s
                        → upgrades ResourceSlice when FM becomes available
```

---

## Part 6.5 — How TPU Schedules Multiple Devices: Lessons for T3K

Understanding how Google's TPU DRA plugin solves the same multi-node scheduling problems we face explains both our current gaps and the correct path forward.

---

### 6.5.1 The core difference: infrastructure atomicity vs scheduler atomicity

The first thing to understand about TPU multi-host scheduling is that **most of the hard guarantee happens before Kubernetes even sees the nodes.**

On GKE, a multi-host TPU slice node pool (for example, a v5e-2x4 = 4 hosts × 4 chips each, 16 chips total) is provisioned by GCP as an **indivisible atomic unit**. GCP either creates all 4 VMs with their ICI (Inter-Chip Interconnect) fabric, or creates none. The VMs are physically wired together at provision time — the Kubernetes scheduler does not need to reason about physical connectivity at all.

For **bare-metal T3K nodes like ours**, this guarantee does not exist. t3k-node-a and t3k-node-b are always in the same Tenstorrent Ethernet mesh, but Kubernetes has no way to know which pairs of nodes are physically connected unless we tell it via attributes in the ResourceSlice.

---

### 6.5.2 How ResourceSlices are published for multi-host TPU

Each node in a multi-host TPU slice publishes its **own** ResourceSlice containing only that node's local chips:

```
t3k-node-a (host 0):  ResourceSlice → driver: google.com/tpu, devices: [tpu-0, tpu-1, tpu-2, tpu-3]
t3k-node-b (host 1):  ResourceSlice → driver: google.com/tpu, devices: [tpu-0, tpu-1, tpu-2, tpu-3]
```

There is **no single ResourceSlice that spans both hosts**. The `pool.resourceSliceCount` field on each slice tells the scheduler how many slices make up the complete pool, so it can detect when it has seen all of them.

**Our approach is the same**: each node publishes its own `wormhole-t3k` device with 8 chips. What we are missing is the pool-level metadata that lets the scheduler reason about which nodes form a complete T3K "pod" (physical-pod=t3k-a).

---

### 6.5.3 allocationMode: ExactCount (per pod) not a single cross-node claim

Each pod in a multi-host workload creates **its own ResourceClaim** (via a ResourceClaimTemplate) requesting only the chips on its own node:

```yaml
# ResourceClaimTemplate for one TPU host
spec:
  spec:
    devices:
      requests:
      - name: tpu
        exactly:
          deviceClassName: google.com/tpu
          allocationMode: ExactCount
          count: 4          # chips per host, not total chips
```

For a 4-host TPU slice: 4 pods × 4 chips each = 16 chips total. Each pod allocates independently from its node's ResourceSlice. There is no single ResourceClaim that atomically reserves chips on all 4 nodes at once.

**Our current approach is already correct**: we use ResourceClaimTemplate so each pod gets its own claim for its node's `wormhole-t3k` device.

---

### 6.5.4 Gang scheduling: how all pods land on connected nodes

Since each pod claims its own node independently, the question is: **how does the scheduler guarantee all pods land on nodes that are physically connected?**

TPU uses three mechanisms, from simplest to most complex:

**Mechanism 1 — Node labels + nodeSelector (always applied)**

All nodes in the same TPU slice have the same `cloud.google.com/gke-tpu-topology` label. A pod's nodeSelector forces it to only schedule onto nodes with that label. This alone isn't enough — two separate slices could have the same topology — but it narrows the candidate set.

**Mechanism 2 — JobSet + exclusive-topology annotation (production standard)**

```yaml
annotations:
  alpha.jobset.sigs.k8s.io/exclusive-topology: cloud.google.com/gke-nodepool
```

This annotation tells JobSet to add pod affinity rules that force all pods in a single replica to land on nodes with the **same value** of the given label (`gke-nodepool`). Since each physical TPU slice has its own unique node pool name, this guarantees all 4 pods land on 4 nodes from the same physical slice.

**For T3K equivalence:** We would use `tenstorrent.com/physical_pod` as the exclusive topology label. All pods in a T3K workload would need to land on nodes where `physical_pod == "t3k-a"`. We already publish this attribute in the ResourceSlice and set it as a node label.

**Mechanism 3 — Native K8s PodGroup gang scheduling (alpha, K8s 1.35)**

```yaml
apiVersion: scheduling.k8s.io/v1alpha2
kind: PodGroup
spec:
  schedulingPolicy:
    gang:
      minCount: 4   # all 4 pods must be schedulable simultaneously
```

The scheduler holds all 4 pods in a waiting queue, then binds all of them atomically if it can place all 4. If it cannot place all 4, none are bound. This is true atomic gang scheduling, but it requires the `GenericWorkload` feature gate (alpha in K8s 1.35, not yet production-ready).

---

### 6.5.5 TPU_WORKER_HOSTNAMES: injected by a webhook, not by PrepareResourceClaims

This is the most important lesson for our peer hostname injection gap.

`TPU_WORKER_HOSTNAMES` is **not** injected by `PrepareResourceClaims`. It is injected by a **mutating admission webhook** that fires when a Job is created.

**Why:** `PrepareResourceClaims` runs on a single node's kubelet. It only knows about the local device. Computing `TPU_WORKER_HOSTNAMES` requires knowing the DNS names of **all** pods in the slice — information that only exists at the cluster level (API server has full view of all pods), not at the per-node plugin level.

**The GKE webhook fires when all of:**
1. The pod spec has `completionMode: Indexed`
2. `subdomain` is set (enables stable pod DNS names)
3. `parallelism > 1`
4. Pod requests `google.com/tpu` resources

**What the webhook injects:**
```yaml
env:
- name: TPU_WORKER_ID
  value: "0"         # this pod's index (0..N-1)
- name: TPU_WORKER_HOSTNAMES
  value: "job-0.subdomain.ns.svc,job-1.subdomain.ns.svc,job-2.subdomain.ns.svc,job-3.subdomain.ns.svc"
```

The hostnames are computed from the Indexed Job naming convention: `<job-name>-<index>.<subdomain>.<namespace>.svc.cluster.local`. The webhook knows all the names at admission time without needing to wait for pods to be scheduled.

**Equivalent for T3K:** We need a webhook that fires on StatefulSet/Job pod creation and injects:
```yaml
- name: TT_WORKER_HOSTNAMES
  value: "wh-t3k-worker-0.wh-t3k-headless,wh-t3k-worker-1.wh-t3k-headless"
- name: TT_WORKER_ID
  value: "0"
```

The StatefulSet naming convention (`<statefulset-name>-<index>.<headless-svc>`) is predictable at admission time, making this straightforward to implement.

---

### 6.5.6 Summary: what TPU does that we should copy

| TPU mechanism | Our equivalent | Status |
|---|---|---|
| Per-node ResourceSlice with pool metadata | Per-node ResourceSlice (missing `resourceSliceCount`) | Partial |
| `allocationMode: ExactCount` per pod | ResourceClaimTemplate per pod | ✅ Done |
| `physical_pod` as exclusive topology label | `tenstorrent.com/physical_pod` in ResourceSlice + node label | ✅ Published, not yet used for gang scheduling |
| JobSet + `exclusive-topology` annotation | StatefulSet + `physical_pod` affinity (not yet wired) | ❌ Missing |
| Mutating webhook for `TPU_WORKER_HOSTNAMES` | Mutating webhook for `TT_WORKER_HOSTNAMES` | ❌ Missing |
| Native PodGroup gang scheduling | Available alpha in K8s 1.35 | ❌ Not yet used |

The two missing pieces that block 2-node vLLM today are: **(1) a webhook for `TT_WORKER_HOSTNAMES`** and **(2) wiring `physical_pod` into JobSet/StatefulSet affinity rules** so the scheduler guarantees both pods land on nodes that are actually connected.

---

## Part 7 — What Has Been Done

### Infrastructure
- [x] Kubernetes cluster: v1.35.0, Kubespray v2.31.0
  - control-plane-01 (192.168.1.60)
  - t3k-node-a (192.168.1.247) — Ready, labeled
  - t3k-node-b (192.168.1.243) — Ready, labeled
- [x] CDI enabled in containerd on both T3K nodes
- [x] CI/CD pipeline: GitHub Actions → build image → Helm deploy via self-hosted runner
- [x] Self-contained container image (plugin + tt-smi baked in)
- [x] Proxmox VM firewall issue resolved (UDP 4789 VXLAN was blocked on t3k-node-a)

### Plugin features
- [x] ResourceSlice publication — label-based (arch, board_type, host_rank, pod_size)
- [x] CDI device injection — `/dev/tenstorrent/*`, hugepages, env vars, logs dir
- [x] Health monitoring — `os.Open /dev/tenstorrent/N`, republishes slice on state change
- [x] Prometheus metrics at `:9090/metrics`
- [x] Crash-recovery checkpoint
- [x] Automatic node labeling — `wh-node-labeler` DaemonSet
- [x] **FM integration** — plugin calls FM agent `GetTopology()` at startup
- [x] **Device bundling** — groups 4 MMIO + 4 remote chips into one device, publishes `mmio_chip_count`, `remote_chip_count`, `remote_chip_ids`
- [x] **Memory capacity** — `Device.Capacity["tenstorrent.com/memory"] = 96Gi` (from FM, sum of all 8 chips)
- [x] **Profiles abstraction** — `internal/profiles/Profile` interface; Tenstorrent implementation in `internal/profiles/tenstorrent/`
- [x] **FM startup retry** — if FM agent not ready at boot, retries every 10s in background and upgrades ResourceSlice when it succeeds

### Tests passed
| Test | Result | What it proves |
|---|---|---|
| `test-claim` | ✅ | Device injection works end-to-end |
| `test-two-pods` | ✅ | DRA exclusivity — only one pod holds device at a time |
| multinode StatefulSet | ✅ | Two pods, two nodes, correct rank and devices |
| scheduler stress test | ✅ | 10 jobs, 5 per node, 2 concurrent max |
| FM topology export | ✅ | wh-topology-export returns 8 ASICs on both nodes |
| ResourceSlice with FM data | ✅ | Both nodes show `chip_count=8`, `memory=96Gi`, `chip_arch=wormhole_b0` |

### Current ResourceSlice (live cluster)
```json
{
  "node": "t3k-node-a",
  "device": "wormhole-t3k",
  "attrs": {
    "chip_arch":          "wormhole_b0",
    "chip_count":         8,
    "mmio_chip_count":    4,
    "remote_chip_count":  4,
    "remote_chip_ids":    "7,6,5,4",
    "pci_addresses":      "0000:00:10.0,0000:00:11.0,0000:00:1b.0,0000:00:1c.0",
    "physical_pod":       "t3k-a",
    "host_rank":          0,
    "pod_size":           2
  },
  "capacity": {
    "tenstorrent.com/memory": "96Gi"
  }
}
```

---

## Part 8 — What Is Next

### Goal 1: Make Moreh vLLM run on Kubernetes (in progress)

| Step | Status | Notes |
|---|---|---|
| 1-node vLLM via DRA | 🔲 todo | Test `deploy/vllm-qwen3-2node-dra.yaml` on single node |
| Fix `board_type=unknown` | 🔲 todo | tt-smi outputs ANSI color codes breaking JSON parse; strip escape codes |
| Peer hostname injection | 🔲 todo | Containers need `TT_WORKER_HOSTNAMES` for multi-node — same gap TPU had |
| 2-node vLLM via DRA | 🔲 todo | Requires peer hostname injection + gang scheduling |
| Validate with Huginn | 🔲 todo | Endpoint quality test tool |

### Comparing to TPU and NVIDIA

| Feature | NVIDIA | Google TPU | Ours (current) |
|---|:---:|:---:|:---:|
| Device discovery + advertising | ✅ | ✅ | ✅ |
| CDI injection | ✅ | ✅ | ✅ |
| Health monitoring | ✅ | ✅ | ✅ |
| FM topology integration | ✅ (NVLink) | ✅ (ICI) | ✅ (TTFM) |
| Memory capacity in ResourceSlice | ✅ | ✅ | ✅ |
| Device bundling (MMIO + remote) | ✅ | ✅ | ✅ |
| Peer hostname injection (multi-node) | ✅ | ✅ | ❌ |
| Deep hardware telemetry | ✅ (DCGM) | ✅ | ❌ |
| Graceful drain on SIGTERM | ✅ | ✅ | ❌ |
| Fault / error code monitoring | ✅ (XID) | ✅ | ❌ |
| Automatic topology discovery | ✅ | ✅ | ❌ (manual ConfigMap) |

### Next implementation priorities

| Priority | Item | Why |
|---|---|---|
| P1 | Fix `board_type=unknown` (tt-smi ANSI) | Attribute accuracy |
| P1 | Peer hostname injection | Multi-node vLLM cannot coordinate without it |
| P1 | 1-node vLLM end-to-end test | Goal 1 validation |
| P2 | Deep hardware telemetry | Temperature, power, utilization from tt-smi as Prometheus metrics |
| P2 | Graceful drain on SIGTERM | Empty ResourceSlice before exit, wait for workloads |
| P3 | Automatic topology discovery | Detect host-rank/pod-size from FM ExitNodeInfo instead of ConfigMap |

---

## Part 9 — Key Design Decisions

**Why DRA and not the old device plugin API?**
The device plugin API (`nvidia.com/gpu: 1` style) can only express integer counts. No attributes, no topology, no memory capacity. DRA lets us publish structured metadata that the scheduler and workloads can query.

**Why CDI and not direct injection?**
CDI decouples device setup from the container runtime. The plugin writes YAML files; containerd reads them. No runtime hooks, no patching containerd — just files. CDI is an open standard supported by containerd, cri-o, and podman.

**Why two CDI specs (common + per-claim)?**
Common spec (env vars, hugepages) is node-level — identical for every pod on this node, written once at startup. Per-claim spec (device nodes) is pod-level — different UID per pod, deleted when pod exits. Separating them avoids rewriting env vars on every PrepareResourceClaims call and makes cleanup precise.

**Why FM at startup instead of per-claim?**
FM UMD discovery works when chips are idle. Once a workload is running, the FM agent falls back to PCIe-only discovery (4 chips instead of 8). So we call GetTopology at plugin startup — before any workload runs — to capture the full 8-chip topology. The ResourceSlice is valid for the lifetime of the node.

**Why retry FM in the background?**
On a node reboot, the plugin pod and the FM agent pod both start simultaneously. The plugin cannot control which starts first. If FM wins the race, great — full topology immediately. If the plugin wins, it publishes a label-based fallback and keeps retrying FM every 10 seconds until the agent is ready, then upgrades the ResourceSlice transparently.

**Why `internalTrafficPolicy: Local` for the FM agent service?**
Each plugin pod must talk to the FM agent on its own node — not any other node's agent. `internalTrafficPolicy: Local` makes kube-proxy route the service ClusterIP only to endpoints on the same node. Requests from a node with no agent fail loudly (connection refused) instead of silently routing to the wrong host.

---

## Part 10 — Live Demo Commands

```bash
# 1. Show cluster is healthy
kubectl get nodes
# Expected: control-plane-01 + t3k-node-a + t3k-node-b — all Ready

# 2. Show what devices are advertised
kubectl get resourceslices
kubectl get resourceslice -o json | jq '.items[] | {node: .spec.nodeName, attrs: .spec.devices[0].attributes, cap: .spec.devices[0].capacity}'
# Expected: both nodes, chip_count=8, memory=96Gi

# 3. Show plugin + FM agent running on both nodes
kubectl -n kube-system get pods -l app=wh-dra-kubelet-plugin -o wide
kubectl -n ttfm get pods -o wide
kubectl -n ttfm logs -l ttfm.tenstorrent.com/component=agent | grep "physical topology"
# Expected: 8 ASICs, 40 local connections, UMD on both nodes

# 4. Run single-node smoke test
kubectl apply -f deploy/test-claim.yaml
kubectl get pods -w
kubectl logs wh-demo-pod
# Expected: /dev/tenstorrent/0-3 visible, TT_CHIP_COUNT=8
kubectl delete -f deploy/test-claim.yaml

# 5. Run exclusivity test
kubectl apply -f deploy/test-two-pods.yaml
kubectl get pods -w
# Expected: wh-excl-pod-a runs, wh-excl-pod-b stays Pending
# After pod-a finishes: pod-b starts
kubectl delete -f deploy/test-two-pods.yaml

# 6. Run multinode StatefulSet
kubectl apply -f deploy/multinode/test-statefulset-two-t3k.yaml
kubectl get pods -l app=wh-t3k-worker -o wide
# Expected: wh-t3k-worker-0 → t3k-node-a (rank=0), wh-t3k-worker-1 → t3k-node-b (rank=1)
kubectl delete -f deploy/multinode/test-statefulset-two-t3k.yaml

# 7. Scheduler stress test (10 jobs, 2 parallel, ~50s)
kubectl apply -f deploy/multinode/test-job-scheduler.yaml
kubectl get pods -l job-name=wh-t3k-scheduler-test -o wide -w
# Expected: 5 completions per node, never more than 2 running simultaneously
kubectl delete -f deploy/multinode/test-job-scheduler.yaml
```
