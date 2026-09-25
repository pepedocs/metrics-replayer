# API

All endpoints are served on the replayer's `--port` (default `8081`). Bodies are plain text or YAML.

## Streams

A stream is a FIFO queue of pushed payloads. Each Prometheus scrape of a stream takes the oldest payload. When the queue is empty, the scrape returns an empty body.

| Method | Path | Body | Description |
|---|---|---|---|
| POST | `/register/{name}` | — | Create a stream. With `--scrapes-dir`, also adds a scrape job for it (see [configuration](configuration.md#scrape-registration)) |
| POST | `/push/{name}` | Prometheus text | Queue a payload |
| GET | `/metrics/{name}` | — | Scrape endpoint: returns and removes the oldest payload |

```bash
curl -X POST localhost:8081/register/api
curl -X POST localhost:8081/push/api --data-binary 'http_requests_total{code="200"} 1024'
```

Each push should be a full snapshot of the stream's series. A series missing from a scrape is marked stale by Prometheus.

## Backfill

Writes historical samples to Prometheus via remote write. Requires `--remote-write-url`.

| Method | Path | Body | Description |
|---|---|---|---|
| POST | `/backfill` | Prometheus text | Write samples at their timestamps |
| POST | `/backfill?align=now` | Prometheus text | Shift all samples so the latest one lands at the current time |

The body is the same text format as `/push`, with a timestamp in milliseconds on every line:

```
http_requests_total{code="500"} 1200 1727200000000
http_requests_total{code="500"} 1260 1727200030000
http_requests_total{code="500"} 1410 1727200060000
```

- Lines can be in any order and are grouped per series.
- Counter, gauge and untyped metrics are supported. Send histograms and summaries as untyped `_bucket`/`_sum`/`_count` lines.
- `?align=now` is for replaying captured data, or data generated with relative timestamps starting at 0.
- To continue a counter live after a backfill, start pushing from its last backfilled value, or `rate()` sees a reset.

## Rules

Rule files are written to `--rules-dir`, and Prometheus is reloaded. Requires `--prometheus-url` and `--rules-dir`.

| Method | Path | Body | Description |
|---|---|---|---|
| POST | `/rules/{name}` | YAML | Create or replace a rule file and reload Prometheus |
| POST | `/rules/{name}?backfill=6h[&step=30s]` | YAML | Same, then evaluate the rules over the last 6h and write the results |
| DELETE | `/rules/{name}` | — | Delete a rule file and reload |
| GET | `/rules` | — | List rule file names |

If Prometheus rejects the file on reload, the file is removed and the call returns `502`.

### Rule backfill

Prometheus only evaluates rules from the moment they're loaded. `?backfill=` fills in the history by evaluating each rule with `query_range` over the window, at `step` resolution (default 30s), and writing the results via remote write:

- **Recording rules** write their recorded series.
- **Alerting rules** write `ALERTS{alertstate="pending"|"firing"}`, applying `for` and `keep_firing_for`.

Rules are evaluated in file order, so an alert can use a recording rule defined above it. Load rules *after* backfilling the metrics they read.

Limitations:
- `ALERTS` history is for graphing only. Backfilled alerts don't appear on the Alerts page or reach Alertmanager.
- Label and annotation templates aren't applied to backfilled results.
- `keep_firing_for` can run past the end of the window, so it may overlap Prometheus' own evaluation.
