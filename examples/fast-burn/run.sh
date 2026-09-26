#!/usr/bin/env bash
# Three services with jagged, realistic error profiles over 24h, evaluated
# against multi-window burn-rate alerts. See docs/demo-fast-burn.md.
# Usage: make clean && make up && ./examples/fast-burn/run.sh
set -euo pipefail
cd "$(dirname "$0")/../.."
REPLAYER=${REPLAYER_URL:-http://localhost:8081}
D=examples/fast-burn

emit() { go run ./cmd/emit --replayer "$REPLAYER" --shape step --window 24h --live=false "$@"; }

# A real incident: 20x burn (2% errors) for 45m, 6h ago.
emit --template $D/checkout.tmpl \
  --params baseline=0.0002,peak=0.02,start=-6h,duration=45m,rise=3m,fall=10m,noise=0.3,seed=11
# A slow burn: 5x (0.5% errors) for 8h.
emit --template $D/payments.tmpl \
  --params baseline=0.0002,peak=0.005,start=-10h,duration=8h,rise=30m,fall=30m,noise=0.3,seed=22
# A burn just above the page threshold (16.5x), with heavy jitter, ongoing.
emit --template $D/search.tmpl \
  --params baseline=0.0002,peak=0.0165,start=-3h,duration=0,rise=5m,noise=0.5,noise_period=3m,seed=33

# Load the rules and evaluate them over the same 24h.
curl -sf -X POST "$REPLAYER/rules/slo?backfill=24h" --data-binary @$D/rules.yaml
