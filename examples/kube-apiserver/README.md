# kube-apiserver long-tail latency

Simulates kube-apiserver request latency and tests a p99 alert against a **long tail**: most requests stay fast, so p50 looks healthy, but a small share of slow requests pushes p99 above 1s.

## Run

```bash
make clean && make up
./examples/kube-apiserver/run.sh
```

Open [localhost:9091](http://localhost:9091) and graph over the last 6h:

```promql
histogram_quantile(0.99, sum by (le, verb) (rate(apiserver_request_duration_seconds_bucket{verb!="WATCH"}[5m])))
histogram_quantile(0.5,  sum by (le, verb) (rate(apiserver_request_duration_seconds_bucket{verb!="WATCH"}[5m])))
```

<!-- Add a Prometheus graph screenshot here, e.g. ![p99 vs p50](snapshot.png) -->

## Traffic profile

The script backfills 6h of history, then pushes live every 2s.

| When | What happens | p50 | p99 |
|---|---|---|---|
| 6h → 3h ago | Normal traffic | ~0.04s | ~0.43s |
| 3h ago, 10 min | etcd compaction: 2% of all requests take 1–4s | ~0.04s | ~1.5s (all verbs) |
| Last 15 min | A controller runs unpaginated `LIST pods`: 4% of LISTs take 2–8s | ~0.04s | ~5s (LIST only) |
| Live, first ~4 min | The LIST tail continues, then the controller is fixed | ~0.04s | LIST drops back below 1s |

Series (verb / resource / requests per second): `GET pods` 40, `LIST pods` 8, `POST pods` 5, `PUT configmaps` 10, and `WATCH pods` 2. Every WATCH lasts 30–60s.

## Rule and alert

```yaml
- record: cluster_quantile:apiserver_request_duration_seconds:histogram_quantile
  expr: |
    histogram_quantile(0.99, sum by (le, verb, resource) (
      rate(apiserver_request_duration_seconds_bucket{verb!~"WATCH|CONNECT",subresource!~"exec|attach|log|portforward|proxy"}[5m])
    ))
  labels:
    quantile: "0.99"
- alert: KubeAPIServerLatencyP99High
  expr: cluster_quantile:apiserver_request_duration_seconds:histogram_quantile{quantile="0.99"} > 1
  for: 2m
```

Long-running requests are excluded: watches, `CONNECT`, and the `exec`/`attach`/`log`/`portforward`/`proxy` subresources. They are slow by design, and without the filter p99 would sit at the top bucket (60s) permanently.

## What to expect

- `KubeAPIServerLatencyP99High{verb="LIST", resource="pods"}` is **pending** right away and **firing** after ~2 minutes. No other verb alerts.
- After the live tail ends (~4 min), the 5m rate window drains, and the alert resolves a few minutes later.
- The etcd compaction 3h ago shows up in the graph but never alerts: rules only evaluate from the moment they are loaded, not over backfilled history.

## Files

- `run.sh`: registers the `kube-apiserver` stream, submits the rule, and runs the profile.
- `profile.awk`: generates the backfill (relative timestamps, sent with `?align=now`) and the live pushes. Edit `spec` for the series and `tick()` for the incidents.
