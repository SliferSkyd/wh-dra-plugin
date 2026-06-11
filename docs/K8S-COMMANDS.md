# Kubernetes Command Reference

Practical reference for day-to-day cluster operations. Commands are grouped by what you're
trying to do, not by resource type.

---

## Cluster health

```bash
# Are all nodes Ready?
kubectl get nodes

# Node details: labels, taints, resource capacity
kubectl describe node <node-name>

# All system pods — are control-plane components Running?
kubectl get pods -n kube-system

# Component health (scheduler, controller-manager, etcd)
kubectl get componentstatuses
```

---

## Pods

### Listing

```bash
# All pods in a namespace
kubectl get pods -n kube-system

# All pods across all namespaces
kubectl get pods -A

# Wide output: adds IP and node columns
kubectl get pods -n kube-system -o wide

# Filter by label
kubectl get pods -n kube-system -l app=wh-dra-kubelet-plugin

# Watch live (refreshes in place)
kubectl get pods -n kube-system -w
```

### Inspecting

```bash
# Full status, events, restart history
kubectl describe pod <pod-name> -n kube-system

# Logs from a running pod
kubectl logs <pod-name> -n kube-system

# Follow logs live
kubectl logs -f <pod-name> -n kube-system

# Last 100 lines
kubectl logs --tail=100 <pod-name> -n kube-system

# Logs from the previous crashed container (after a CrashLoopBackOff)
kubectl logs --previous <pod-name> -n kube-system

# If a pod has multiple containers, name the one you want
kubectl logs <pod-name> -c <container-name> -n kube-system
```

### Executing commands inside a pod

```bash
# Open an interactive shell
kubectl exec -it <pod-name> -n kube-system -- /bin/sh

# Run a one-off command
kubectl exec <pod-name> -n kube-system -- ls /dev/tenstorrent
```

### Deleting pods

```bash
# Delete a pod (it will be recreated by its DaemonSet/Deployment)
kubectl delete pod <pod-name> -n kube-system

# Force-delete a stuck Terminating pod
kubectl delete pod <pod-name> -n kube-system --force --grace-period=0
```

---

## DaemonSets

DaemonSets run one pod per matching node. The wh-dra-plugin uses two of them.

```bash
# List DaemonSets
kubectl get daemonset -n kube-system

# Check rollout status (blocks until all nodes are updated)
kubectl rollout status daemonset/wh-dra-kubelet-plugin -n kube-system

# Restart all pods in a DaemonSet (pulls new image if pullPolicy: Always)
kubectl rollout restart daemonset/wh-dra-kubelet-plugin -n kube-system

# View rollout history
kubectl rollout history daemonset/wh-dra-kubelet-plugin -n kube-system

# Undo the last rollout
kubectl rollout undo daemonset/wh-dra-kubelet-plugin -n kube-system

# Describe: shows selector, node count, image, events
kubectl describe daemonset wh-dra-kubelet-plugin -n kube-system
```

---

## Deployments

```bash
# List Deployments
kubectl get deployments -n <namespace>

# Scale a Deployment up or down
kubectl scale deployment <name> --replicas=3 -n <namespace>

# Rollout commands (same as DaemonSet)
kubectl rollout status deployment/<name> -n <namespace>
kubectl rollout restart deployment/<name> -n <namespace>
kubectl rollout undo deployment/<name> -n <namespace>
```

---

## ConfigMaps and Secrets

```bash
# List ConfigMaps
kubectl get configmap -n kube-system

# Show the data inside a ConfigMap
kubectl describe configmap tt-node-topology -n kube-system

# Edit a ConfigMap in-place (opens $EDITOR)
kubectl edit configmap tt-node-topology -n kube-system

# Decode a Secret value (secrets are base64-encoded)
kubectl get secret <name> -n <namespace> -o jsonpath='{.data.<key>}' | base64 -d
```

---

## Nodes: labels and taints

```bash
# Show all labels on every node
kubectl get nodes --show-labels

# Add a label
kubectl label node <node-name> tenstorrent.com/arch=wormhole

# Remove a label (trailing -)
kubectl label node <node-name> tenstorrent.com/arch-

# Add a taint (prevents pods without a matching toleration from scheduling)
kubectl taint node <node-name> key=value:NoSchedule

# Remove a taint
kubectl taint node <node-name> key=value:NoSchedule-
```

---

## RBAC

```bash
# List ClusterRoles and ClusterRoleBindings
kubectl get clusterrole | grep wh
kubectl get clusterrolebinding | grep wh

# Show what a ServiceAccount is allowed to do
kubectl auth can-i --list --as=system:serviceaccount:kube-system:wh-dra-plugin
```

---

## Resource requests and capacity

```bash
# How much CPU/memory is each node allocating?
kubectl describe nodes | grep -A 5 "Allocated resources"

# Resource quotas in a namespace
kubectl describe resourcequota -n <namespace>

# Top nodes (requires metrics-server)
kubectl top nodes

# Top pods
kubectl top pods -n kube-system
```

---

## Events

Events are the fastest way to diagnose why something isn't starting.

```bash
# All events in a namespace, newest last
kubectl get events -n kube-system --sort-by='.lastTimestamp'

# Events for a specific pod (grep by name)
kubectl get events -n kube-system --field-selector involvedObject.name=<pod-name>
```

---

## Apply, delete, and dry-run

```bash
# Apply a manifest
kubectl apply -f deploy/daemonset.yaml

# Apply all manifests in a directory
kubectl apply -f deploy/

# Preview what apply would change (dry run, server-side)
kubectl apply -f deploy/daemonset.yaml --dry-run=server

# Delete resources defined in a manifest
kubectl delete -f deploy/daemonset.yaml

# Delete with a label selector
kubectl delete pods -n kube-system -l app=wh-dra-kubelet-plugin
```

---

## jsonpath and custom output

`-o jsonpath` extracts specific fields without piping through `jq`.

```bash
# Image tag currently running on each DaemonSet pod
kubectl get pods -n kube-system -l app=wh-dra-kubelet-plugin \
  -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.spec.containers[0].image}{"\n"}{end}'

# Node each pod is running on
kubectl get pods -n kube-system -l app=wh-dra-kubelet-plugin \
  -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.spec.nodeName}{"\n"}{end}'

# All node names
kubectl get nodes -o jsonpath='{.items[*].metadata.name}'
```

---

## Contexts and namespaces

```bash
# Which cluster am I talking to?
kubectl config current-context

# List all contexts (clusters/users)
kubectl config get-contexts

# Switch context
kubectl config use-context <context-name>

# Set a default namespace so you don't have to type -n every time
kubectl config set-context --current --namespace=kube-system

# Reset to default namespace
kubectl config set-context --current --namespace=default
```

---

## Shortcuts

| Long form | Short alias |
|---|---|
| `kubectl get pods` | `kubectl get po` |
| `kubectl get services` | `kubectl get svc` |
| `kubectl get deployments` | `kubectl get deploy` |
| `kubectl get daemonsets` | `kubectl get ds` |
| `kubectl get configmaps` | `kubectl get cm` |
| `kubectl get namespaces` | `kubectl get ns` |
| `kubectl get nodes` | `kubectl get no` |
| `-n kube-system` | `-n kube-system` (no alias, but set default above) |

---

## Common failure patterns

| Symptom | First command to run |
|---|---|
| Pod stuck `Pending` | `kubectl describe pod <name> -n <ns>` → look at Events |
| Pod in `CrashLoopBackOff` | `kubectl logs --previous <name> -n <ns>` |
| Pod stuck `Terminating` | `kubectl delete pod <name> -n <ns> --force --grace-period=0` |
| `InvalidImageName` | Check `image:` field for uppercase letters |
| `ImagePullBackOff` | `kubectl describe pod` → Events shows exact registry error |
| `OOMKilled` | Pod exceeded memory limit → raise `resources.limits.memory` |
| Node `NotReady` | `kubectl describe node <name>` → Conditions section |
| Wrong node scheduling | Check `nodeSelector` / taints on the node |
