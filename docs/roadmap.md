# Known limitations and enhancements

None of these block the documented workflow: a clean stack per scenario, read with aggregate queries. They matter when you iterate on a scenario, look at raw series, or run live for a long time.

## Known limitations

### Re-running mixes old and new data

Backfill and rule backfill always add samples; they never replace them. Re-running a profile, or re-loading an edited rule with `?backfill=`, interleaves new samples with the old ones in the same series. Counters look reset, and `ALERTS` shows old and new rule versions together.

**For now:** run `make clean && make up` before each re-run.
**Enhancement:** delete the affected series before backfilling (Prometheus admin API, `--web.enable-admin-api`), or add a replace option to `/backfill` and `/rules?backfill=`.

### Backfilled and live data are different series

A Prometheus scrape adds `job` and `instance` labels, and backfilled samples don't have them. After `emit` backfills and then goes live, each metric has two series with a seam at "now". Aggregations like `sum by (service)` hide this; raw queries and per-series `rate()` show it.

**Enhancement:** have backfill add the same `job` and `instance` labels the stream's scrape config uses.

### Live pushes are tied to scrapes

Each scrape takes exactly one queued push, and samples get the scrape's timestamp. If pushes fall behind the scrape interval, a scrape finds the queue empty and the series briefly goes stale. If they get ahead, the queue grows without limit and live data lags.

**Enhancement:** a queue limit (`--max-queue`, `429` when full), and pushing with explicit timestamps or pacing `emit` to the scrape interval.

## Smaller items

- `/backfill` sends one remote-write request; split large ones into chunks.
- Backfilled `ALERTS` history doesn't carry over into Prometheus' live alert state, so live evaluation restarts `for` when rules are loaded.
- Rule backfill doesn't apply label and annotation templates.
- The replayer API has no authentication. It's meant for local use.
- `--strip-for` removes lines by text; use a YAML parser.
- Validate rule names, and stream names even without `--scrapes-dir`.
- The e2e test shares the `default` stream with the demo producer; give it its own stream.
- `examples/api-latency` has no README.

## Ideas

- **kube-apiserver burn-rate flapping demo.** Replay the kubernetes-mixin `KubeAPIErrorBudgetBurn` rules on a jittery, apiserver-like profile, the case OpenShift clusters see as noisy and teams end up silencing. Show which fix stops the flapping without delaying real incidents: `keep_firing_for`, a minimum-traffic floor, or excluding long-running requests from the SLI.
- **Flap probability sweep.** Vary how far an incident's burn sits above the threshold (`peak`) and how jittery it is (`noise`), and count pages per incident. An empirical curve for "the closer to the threshold, the more likely it flaps", to set next to the uniform or Gaussian model.
- **Detectors beyond threshold + `for`.** Compare hysteresis, fraction of time above the threshold (`avg_over_time((expr > bool T)[10m:])`), budget accumulation (CUSUM-style), and a simple ML classifier (e.g. logistic regression on window features) against the PromQL rules on the same replayed profiles. The replayer provides labeled data for free: the shape is the label.
