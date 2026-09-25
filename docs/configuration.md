# Configuration

```
metrics-replayer [flags]
  --port              Listen port (default: 8081)
  --remote-write-url  Prometheus remote write URL (enables /backfill)
  --prometheus-url    Prometheus base URL (enables /rules and reloads)
  --rules-dir         Directory for rule files, shared with Prometheus
  --scrapes-dir       Directory for per-stream scrape configs (enables scrape registration)
  --scrape-target     host:port Prometheus uses to reach the replayer (default: localhost:<port>)
  --bearer-token      Bearer token for remote write
  --strip-for         Remove `for` from alert rules, so alerts fire immediately
```

## Prometheus setup

The bundled `docker-compose.yaml` and `prometheus.yaml` configure all of this. If you run your own Prometheus, it needs:

- `--web.enable-remote-write-receiver`, for `/backfill` and rule backfill
- `--web.enable-lifecycle`, so the replayer can reload it
- `rule_files: [<rules-dir>/*.yaml]`
- `scrape_config_files: [<scrapes-dir>/*.yaml]`, for scrape registration
- `storage.tsdb.out_of_order_time_window` of at least your longest backfill, so backfills work while live data is being scraped

The replayer and Prometheus must share `--rules-dir` and `--scrapes-dir`, either on the same host or through a shared volume.

## Scrape registration

With `--scrapes-dir`, `POST /register/{name}` writes `{scrapes-dir}/{name}.yaml`:

```yaml
scrape_configs:
  - job_name: "<name>"
    metrics_path: "/metrics/<name>"
    static_configs:
      - targets: ["<scrape-target>"]
```

It then reloads Prometheus and waits until the target is up. Series get `job="<name>"` and `instance="<scrape-target>"`.

`--scrape-target` must be the address Prometheus uses to reach the replayer:

| Setup | `--scrape-target` |
|---|---|
| Both on one host | `localhost:8081` (default) |
| Docker Compose | `metrics-replayer:8081` |
| Different machines | `<replayer-host>:8081` |

If Prometheus can't reach it, register fails with `502` and names the address. Leftover scrape files are removed on startup, because streams live in memory.

## `--strip-for`

In production, `for: 5m` suppresses noise. In a test you often only care whether the expression matches. With `--strip-for`, alerts go straight from inactive to firing.

## Troubleshooting

**Clean restart**: wipe all metrics, rules and streams.

```bash
make clean && make up
```

**Prometheus uses an old config**: `prometheus.yaml` is bind-mounted, and editors replace the file, so a running container keeps the old version. Run `make clean && make up`.

**"out of bounds" on backfill**: the backfill is older than Prometheus' `out_of_order_time_window` allows.

**"no space left on device"**: Prometheus data is on a tmpfs (RAM disk, 2G by default). Increase it:

```bash
PROM_STORAGE_SIZE=4G make up
```

**Counters look reset or garbled**: two generators are pushing to the same stream. Stop all but one.
