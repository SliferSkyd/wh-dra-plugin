#!/usr/bin/env bash
# Label both T3K nodes for the DRA plugin.
# Run this once when adding nodes to the cluster.
#
# Each node gets:
#   physical-pod = unique T3K unit identifier (t3k-a, t3k-b, ...)
#   host-rank    = 0 for single-host T3K, 0/1 for dual-host T3K
#   pod-size     = number of hosts in this T3K unit (1 for isolated T3K)

set -euo pipefail

NODE_A="${1:-tt-mv-n2-vm4}"   # first  T3K host
NODE_B="${2:-tt-mv-n2-vm5}"   # second T3K host

kubectl label node "$NODE_A" --overwrite \
  tenstorrent.com/arch=wormhole \
  tenstorrent.com/board-type=n300 \
  tenstorrent.com/chip-count=4 \
  tenstorrent.com/physical-pod=t3k-a \
  tenstorrent.com/host-rank=0 \
  tenstorrent.com/pod-size=1

echo "labeled $NODE_A as t3k-a"

kubectl label node "$NODE_B" --overwrite \
  tenstorrent.com/arch=wormhole \
  tenstorrent.com/board-type=n300 \
  tenstorrent.com/chip-count=4 \
  tenstorrent.com/physical-pod=t3k-b \
  tenstorrent.com/host-rank=0 \
  tenstorrent.com/pod-size=1

echo "labeled $NODE_B as t3k-b"

kubectl get nodes -l tenstorrent.com/arch=wormhole \
  -o custom-columns='NODE:.metadata.name,PHYSICAL-POD:.metadata.labels.tenstorrent\.com/physical-pod,CHIP-COUNT:.metadata.labels.tenstorrent\.com/chip-count'
