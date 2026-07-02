#!/usr/bin/env bash
# spegel-bench.sh — measure cold-pull time for a Shpiel-produced weights
# image (tar-layers format), the number that decides time-to-first-token
# on autoscaled GPU nodes.
#
# Usage:
#   ./spegel-bench.sh <image-reference> [runs]
#
#   image-reference   e.g. zot.registry.svc.cluster.local:5000/models/org/name:main
#   runs              repetitions (default 3)
#
# Run on a node (or privileged debug pod) with crictl access. For the
# Spegel number, run on a node whose peers already hold the image; for the
# Zot-direct number, run with Spegel disabled or on the first node.
set -euo pipefail

IMAGE="${1:?usage: spegel-bench.sh <image-reference> [runs]}"
RUNS="${2:-3}"
CRICTL="${CRICTL:-crictl}"

command -v "$CRICTL" >/dev/null || { echo "crictl not found (set CRICTL=...)"; exit 1; }

echo "image: $IMAGE"
echo "runs:  $RUNS"
echo

total=0
for i in $(seq 1 "$RUNS"); do
  # Cold start: drop the image if present.
  "$CRICTL" rmi "$IMAGE" >/dev/null 2>&1 || true

  start=$(date +%s.%N)
  "$CRICTL" pull "$IMAGE" >/dev/null
  end=$(date +%s.%N)

  elapsed=$(echo "$end $start" | awk '{printf "%.2f", $1 - $2}')
  total=$(echo "$total $elapsed" | awk '{printf "%.2f", $1 + $2}')
  echo "run $i: ${elapsed}s"
done

echo
avg=$(echo "$total $RUNS" | awk '{printf "%.2f", $1 / $2}')
echo "average: ${avg}s over $RUNS cold pulls"

# Layer inventory for the report.
if "$CRICTL" inspecti "$IMAGE" >/dev/null 2>&1; then
  layers=$("$CRICTL" inspecti "$IMAGE" | grep -c '"sha256:' || true)
  echo "layers:  ~$layers (one per model file + shpiel manifest)"
fi
