## TT Fabric Manager Helm Chart

Helm Chart for for TT-Fabric-Manager controller deployment.

### How to install
Pre-requisites:
* Flux installed on the cluster
* Namespace `ttfm` created
* `tt-fabric-manager` imagePullSecret created

Install (on ExaBox):
```
helm -nttfm upgrade --install ttfm -f values.exabox.yaml .
```

This will use the docker image tagged `latest`. To fix image tag use  `image.tag` field in `values.yaml`.

**IMPORTANT**: On ExaBox and ClosetBox this HelmChart is managed by [Flux](../../deployment).