# wh-dra-plugin Helm Chart

Packages the Wormhole DRA plugin and node labeler as a single deployable unit.

---

## What the chart manages

| Resource | Kind | Namespace |
|---|---|---|
| `wh-dra-plugin` | ServiceAccount | kube-system |
| `wh-dra-plugin` | ClusterRole | — |
| `wh-dra-plugin` | ClusterRoleBinding | — |
| `t3k.wormhole.tenstorrent.com` | DeviceClass | — |
| `tt-node-topology` | ConfigMap | kube-system |
| `wh-node-labeler` | DaemonSet | kube-system |
| `wh-dra-kubelet-plugin` | DaemonSet | kube-system |

The node-labeler RBAC (`ServiceAccount/ClusterRole/ClusterRoleBinding wh-node-labeler`) is
managed separately in `deploy/node-labeler.yaml` — not part of this chart.

---

## Normal usage — CI/CD handles everything

Under normal operation you do not run Helm manually. Every `git push origin main` triggers
GitHub Actions, which builds a new image and runs `helm upgrade` automatically.

```
git push origin main  →  build  →  helm upgrade  →  rolling restart on T3K nodes
```

See `deploy/DEPLOY.md` for the full CI/CD pipeline description.

---

## Manual Helm commands (troubleshooting / first install)

### Install from scratch

```bash
cd /home/ubuntu/wh-dra-plugin

helm upgrade --install wh-dra-plugin ./helm/wh-dra-plugin \
  --namespace kube-system
```

### Upgrade to a specific image SHA

```bash
helm upgrade wh-dra-plugin ./helm/wh-dra-plugin \
  --namespace kube-system \
  --set image.tag=sha-abc1234
```

### Check release status

```bash
helm list -n kube-system
helm status wh-dra-plugin -n kube-system
```

### View deploy history

```bash
helm history wh-dra-plugin -n kube-system
```

### Rollback to the previous release

```bash
helm rollback wh-dra-plugin -n kube-system
# or to a specific revision:
helm rollback wh-dra-plugin 2 -n kube-system
```

### Uninstall

```bash
helm uninstall wh-dra-plugin -n kube-system
```

---

## Values reference

| Key | Default | Description |
|---|---|---|
| `image.repository` | `ghcr.io/slifersky/wh-dra-plugin` | Image registry + repo. Override if hosting in a private registry. |
| `image.tag` | `latest` | Image tag. CI/CD sets this to `sha-XXXXXXX` on every deploy. |
| `image.pullPolicy` | `Always` | `Always` ensures the latest tag is pulled on pod restart. |
| `imagePullSecrets` | `[]` | Set to `[{name: ghcr-credentials}]` if the ghcr.io package is private. |
| `plugin.healthCheckInterval` | `30s` | How often to check chip accessibility via `os.Open(/dev/tenstorrent/N)`. Set to `0` to disable. |
| `plugin.ttSmiPath` | `/usr/local/bin/tt-smi` | Path to `tt-smi` binary (used by node-labeler only). |
| `plugin.metricsPort` | `9090` | Prometheus metrics port inside the plugin container. |
| `plugin.cdiDir` | `/var/run/cdi` | Directory where CDI spec files are written. |
| `plugin.checkpointDir` | `/var/lib/wh-dra/checkpoint` | Crash-recovery checkpoint location. |
| `plugin.pluginDir` | `/var/lib/kubelet/plugins/wormhole.tenstorrent.com` | kubelet plugin socket directory. |
| `plugin.registrarDir` | `/var/lib/kubelet/plugins_registry` | kubelet plugin registration directory. |
| `nodeLabeler.interval` | `5m` | How often the labeler re-reads topology and re-labels the node. |
| `nodeLabeler.ttSmiPath` | `/usr/local/bin/tt-smi` | Path to `tt-smi` (used to detect arch and board type). |
| `nodeSelector` | `tenstorrent.com/arch: wormhole` | Both DaemonSets run only on nodes with this label. |
| `topology` | see below | Maps each node name to its T3K topology string. |

### `topology` values

The topology ConfigMap tells the node-labeler what physical-pod, host-rank, and pod-size
to assign to each node. Edit this when adding new T3K nodes to the cluster.

```yaml
topology:
  t3k-node-a: "physical-pod=t3k-a host-rank=0 pod-size=2"
  t3k-node-b: "physical-pod=t3k-a host-rank=1 pod-size=2"
```

| Field | Meaning |
|---|---|
| `physical-pod` | Name of the Galaxy pod this server belongs to |
| `host-rank` | 0-based position of this server within the pod |
| `pod-size` | Total number of T3K nodes in this connected mesh |

Changes to `topology` take effect within `nodeLabeler.interval` (default 5 min) — no pod restart needed.

To override at deploy time:

```bash
helm upgrade wh-dra-plugin ./helm/wh-dra-plugin \
  --namespace kube-system \
  --set topology.t3k-node-a="physical-pod=t3k-a host-rank=0 pod-size=2" \
  --set topology.t3k-node-b="physical-pod=t3k-a host-rank=1 pod-size=2"
```

---

## Customising values without editing values.yaml

Pass `--set` flags on the command line or create a `values-override.yaml` file:

```yaml
# values-override.yaml — not committed, used for local overrides
plugin:
  healthCheckInterval: 60s
topology:
  t3k-node-a: "physical-pod=t3k-a host-rank=0 pod-size=1"
```

```bash
helm upgrade wh-dra-plugin ./helm/wh-dra-plugin \
  --namespace kube-system \
  -f values-override.yaml
```

---

## Disabling the health check

The health check calls `os.Open(/dev/tenstorrent/N)` every 30 s. If the T3K hardware is
being serviced or the driver needs reloading, you can disable it temporarily without
redeploying the image:

```bash
helm upgrade wh-dra-plugin ./helm/wh-dra-plugin \
  --namespace kube-system \
  --set plugin.healthCheckInterval=0
```

Re-enable after the hardware is stable:

```bash
helm upgrade wh-dra-plugin ./helm/wh-dra-plugin \
  --namespace kube-system \
  --set plugin.healthCheckInterval=30s
```

---

## Chart structure

```
helm/wh-dra-plugin/
  Chart.yaml               chart metadata (name, version, appVersion)
  values.yaml              default values — source of truth for configuration
  templates/
    _helpers.tpl           shared template helpers (image ref, common labels)
    rbac.yaml              ServiceAccount + ClusterRole + ClusterRoleBinding
    deviceclass.yaml       DeviceClass (t3k.wormhole.tenstorrent.com)
    node-labeler.yaml      tt-node-topology ConfigMap + wh-node-labeler DaemonSet
    daemonset.yaml         wh-dra-kubelet-plugin DaemonSet
```
