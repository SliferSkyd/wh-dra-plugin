#!/usr/bin/env bash
# Add moai.moreh.io accelerator labels to Tenstorrent Wormhole nodes.
# Run once per node when adding it to the cluster.
#
# Usage: bash deploy/odin/node-labels.sh <node-name> [<node-name> ...]
#
# These labels are required by Odin InferenceServiceTemplates for node selection.
# They complement the tenstorrent.com/* labels set by deploy/multinode/node-labels.sh.

set -euo pipefail

if [[ $# -eq 0 ]]; then
  echo "Usage: $0 <node-name> [<node-name> ...]"
  exit 1
fi

for NODE in "$@"; do
  kubectl label node "$NODE" --overwrite \
    moai.moreh.io/accelerator.vendor=tenstorrent \
    moai.moreh.io/accelerator.model=wormhole
  echo "labeled $NODE: moai.moreh.io/accelerator.vendor=tenstorrent moai.moreh.io/accelerator.model=wormhole"
done

echo ""
kubectl get nodes \
  -l moai.moreh.io/accelerator.vendor=tenstorrent \
  -L moai.moreh.io/accelerator.vendor,moai.moreh.io/accelerator.model,tenstorrent.com/pod-size
