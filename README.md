# metrics-replayer

## Overview

HTTP server that sits between your scripts and Prometheus. Push metrics, backfill historical data, and manage recording/alerting rules via REST.

```
your script ──POST /push/{name}──> replayer <──scrape── Prometheus
                                      │                     ^
                            POST /backfill ──remote write────┘
                            POST /rules/{name} ──file + reload─┘
```

## Getting Started

```bash
make up
./examples/api-latency/run.sh
```

`make up` starts the replayer (`:8081`) and Prometheus (`:9091`). The example script then:
1. Submits error-rate and p99-latency recording rules and alerts
2. Backfills 24 hours of API traffic: a daily traffic curve, a bad deploy 18h ago (35% 500s, slow responses), a dependency outage 6h ago (60% POST 503s), and a slow burn over the last 30 minutes
3. Pushes live updates every 2 seconds: the slow burn continues for ~3 minutes, so `HighErrorRate` and `HighLatencyP99` fire, then resolve

Open [localhost:9091](http://localhost:9091) and graph `sum by (status) (rate(http_requests_total{status=~"5.."}[5m]))` over 1d.

## Problem Statement

Say you have metric profiles from your service — maybe from a real incident, maybe from a load test, maybe you just know what a slow-burn failure looks like. You want to know: will my alerts actually catch this? And you want to verify it visually — open Prometheus, run the queries, and analyze how your alerts and recording rules behave against those shapes.

Writing promtool tests for it is painful. The data is non-linear, the DSL doesn't fit, and you can't see what's going on. What you really want is to push those profiles through a real Prometheus, open the graph, and see the shapes.

## Solution

metrics-replayer abstracts Prometheus behind a simple HTTP API. You don't need to deal with protobuf encoding for remote write, file paths for rules, or reload signals — just curl with plain text.

- **Push**: POST Prometheus-format text to a named stream. Pushes are queued FIFO — each Prometheus scrape dequeues the oldest entry.
- **Backfill**: POST timestamped samples in Prometheus text format. The replayer writes them to Prometheus via remote write.
- **Rules**: POST a YAML rule file. The replayer writes it to disk and reloads Prometheus.

This lets you script entire scenarios — backfill days of data, submit your rules, push live updates, and observe how alerts fire and resolve in Prometheus graphs.

## Promtool

`promtool test rules` validates rule syntax and asserts on expected values using a linear DSL (`0+5x20`). It runs without a Prometheus instance and is good for CI and simple threshold checks.

It doesn't handle non-linear data well. Real metrics are shapes — spikes, exponential decay, slow ramps, correlated signals. A 3-day burn-rate window at 1-minute resolution is 4,320 values per metric. The DSL has no way to express these curves, and there's no graphing — you assert on a number at a point in time but never see the actual behavior.

metrics-replayer is not a replacement for promtool. Use promtool for syntax validation and deterministic assertions in CI. Use metrics-replayer when you need to push non-linear data through a real Prometheus and see how rules and alerts respond visually.

## Use Cases

**Alert development** — Push a metric profile that simulates an incident (e.g., 85% error rate spike over 2 hours). Verify that your burn-rate alert fires at the right threshold and clears on recovery. See the shape in Prometheus graphs.

**Burn-rate validation** — Backfill days of data to test slow-burn SLO alerts (72h+6h windows). These are impractical to test in real-time or with promtool's linear DSL.

**Incident replay** — Capture real metrics from a production incident, replay them locally, and verify whether new or modified rules would have caught it.

**Edge case testing** — Low-volume scenarios (3 failures out of 50 provisions) where burn-rate alerts stay silent. Verify the gap exists, then test cause-based alerts that catch it.

---

## API

### Streams

| Method | Path | Body | Description |
|--------|------|------|-------------|
| POST | `/register/{name}` | — | Create a named stream (and its scrape config, with `--scrapes-dir`) |
| POST | `/push/{name}` | Prometheus text | Push metrics (FIFO queue) |
| GET | `/metrics/{name}` | — | Scrape endpoint (dequeues oldest push) |

### Backfill

| Method | Path | Body | Description |
|--------|------|------|-------------|
| POST | `/backfill` | Prometheus text | Write historical samples to Prometheus |
| POST | `/backfill?align=now` | Prometheus text | Same, shifted so the latest sample lands at the current time |

The body uses the same text format as `/push`, with a timestamp (ms) on every line:

```
http_requests_total{status="500"} 1200 1727200000000
http_requests_total{status="500"} 1260 1727200030000
http_requests_total{status="500"} 1410 1727200060000
```

Lines can be in any order. Counter, gauge and untyped metrics are supported; send histograms and summaries as untyped `_bucket`/`_sum`/`_count` lines. Use `?align=now` to replay captured data, or data generated with relative timestamps starting at 0.

Recording rules and alerts only evaluate from the moment they are loaded. To see them over backfilled history, graph the raw expression.

### Rules

| Method | Path | Body | Description |
|--------|------|------|-------------|
| POST | `/rules/{name}` | YAML | Create/update rule file, reload Prometheus |
| DELETE | `/rules/{name}` | — | Delete rule file |
| GET | `/rules` | — | List rule names |

## Configuration

```
metrics-replayer [flags]
  --port              Listen port (default: 8081)
  --remote-write-url  Prometheus remote write URL (enables /backfill)
  --prometheus-url    Prometheus base URL (enables /rules reload)
  --rules-dir         Directory for rule files
  --bearer-token      Bearer token for remote write auth
  --strip-for         Remove 'for' durations from alert rules (alerts fire immediately)
  --scrapes-dir       Directory for per-stream scrape configs (enables scrape registration)
  --scrape-target     host:port Prometheus uses to reach the replayer (default: localhost:<port>)
```

With `--scrapes-dir`, `POST /register/{name}` writes a scrape config for the stream (`job=<name>`, `instance=<scrape-target>`) to `{scrapes-dir}/{name}.yaml`, reloads Prometheus, and waits until the target is up. If Prometheus can't reach `--scrape-target`, register fails with `502` and names the address. Prometheus must load the directory via `scrape_config_files` (see `prometheus.yaml`).

`--strip-for` is useful for testing. Alert rules use `for:` to suppress noise in production (e.g., `for: 5m` means the condition must hold for 5 minutes before firing). In a test environment you only care whether the expression matches — not whether it stays true long enough. With `--strip-for`, alerts transition directly from inactive to firing on the first matching evaluation.

## Troubleshooting

**Clean restart** — wipe all metrics and rules:

```bash
make clean && make up
```

**Prometheus "no space left on device"**: The docker-compose stack uses a tmpfs (RAM disk) for Prometheus data. Large backfills can fill it up. Set `PROM_STORAGE_SIZE` to increase it (default: 2G):

```bash
PROM_STORAGE_SIZE=4G docker compose up -d
```

## Testing

```bash
make test-unit        # unit tests
make up               # start stack for e2e
make test-e2e         # e2e tests
```

## Acknowledgment

This project was written by a human with the assistance of an AI code assistant.
