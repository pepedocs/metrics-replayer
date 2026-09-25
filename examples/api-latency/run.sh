#!/usr/bin/env bash
# Simulate an API service: 24h of traffic with incidents, then live pushes.
# Usage: make up && ./examples/api-latency/run.sh
set -euo pipefail

REPLAYER=${REPLAYER_URL:-http://localhost:8081}
DIR=$(cd "$(dirname "$0")" && pwd)

wait_ready() {
  printf "waiting for replayer..."
  until curl -sf "$REPLAYER/rules" >/dev/null 2>&1; do sleep 1; printf "."; done
  echo " ready"
}

register() {
  echo "==> registering stream 'api'"
  local status
  status=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$REPLAYER/register/api")
  case $status in
    201|409) ;;
    *) echo "register failed: HTTP $status" >&2; exit 1 ;;
  esac
}

submit_rules() {
  echo "==> submitting recording rules and alerts (error rate > 10%, p99 latency > 500ms)"
  curl -sf -X POST "$REPLAYER/rules/api-rules" \
    -H 'Content-Type: application/yaml' \
    -d '
groups:
  - name: api_rules
    rules:
      - record: api:http_error_rate:5m
        expr: |
          sum(rate(http_requests_total{status=~"5.."}[5m]))
          / sum(rate(http_requests_total[5m]))
      - record: api:http_latency_p99:5m
        expr: histogram_quantile(0.99, sum by (le) (rate(http_request_duration_seconds_bucket[5m])))
      - alert: HighErrorRate
        expr: api:http_error_rate:5m > 0.10
        for: 2m
        labels:
          severity: warning
        annotations:
          summary: "API error rate above 10%"
      - alert: HighLatencyP99
        expr: api:http_latency_p99:5m > 0.5
        for: 2m
        labels:
          severity: warning
        annotations:
          summary: "API p99 latency above 500ms"
' >/dev/null
}

wait_ready
register
submit_rules

echo "    graph over 24h to see the incidents (rules only evaluate from now on):"
echo '      sum by (status) (rate(http_requests_total{status=~"5.."}[5m]))'
echo '      histogram_quantile(0.99, sum by (le) (rate(http_request_duration_seconds_bucket[5m])))'

body=$(mktemp)
trap 'rm -f "$body"; pkill -P $$ 2>/dev/null' EXIT
awk -v url="$REPLAYER" -v stream=api -v body="$body" -f "$DIR/profile.awk"
