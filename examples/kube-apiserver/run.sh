#!/usr/bin/env bash
# Simulate kube-apiserver latency with a long tail: p50 stays fast while p99
# for LIST pods crosses 1s and fires an alert.
# Usage: make up && ./examples/kube-apiserver/run.sh
set -euo pipefail

REPLAYER=${REPLAYER_URL:-http://localhost:8081}
DIR=$(cd "$(dirname "$0")" && pwd)

printf "waiting for replayer..."
until curl -sf "$REPLAYER/rules" >/dev/null 2>&1; do sleep 1; printf "."; done
echo " ready"

echo "==> registering stream 'kube-apiserver'"
status=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$REPLAYER/register/kube-apiserver")
case $status in
  201|409) ;;
  *) echo "register failed: HTTP $status" >&2; exit 1 ;;
esac

# Long-running requests (watches, exec/attach/log streams, port-forwards,
# proxies) are slow by design and would pin p99 at the top bucket.
echo "    graph over 6h to compare p50 and p99:"
echo '      histogram_quantile(0.5,  sum by (le, verb) (rate(apiserver_request_duration_seconds_bucket{verb!="WATCH"}[5m])))'
echo '      histogram_quantile(0.99, sum by (le, verb) (rate(apiserver_request_duration_seconds_bucket{verb!="WATCH"}[5m])))'

body=$(mktemp)
trap 'rm -f "$body"; pkill -P $$ 2>/dev/null' EXIT
# The profile submits rules.yaml with ?backfill=6h right after the metric
# backfill, so rule results and ALERTS history cover the whole 6h.
awk -v url="$REPLAYER" -v stream=kube-apiserver -v body="$body" -v rules="$DIR/rules.yaml" \
  -f "$DIR/profile.awk"
