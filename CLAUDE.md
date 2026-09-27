# CLAUDE.md

A studio for studying metrics and alerts: replay metric profiles through a real Prometheus, evaluate rules over the history instantly (rule backfill) or live, and look at the result in the Prometheus console. See README.md and docs/.

## Layout

- `cmd/metrics-replayer`: the HTTP server (streams, backfill, rules, scrape registration)
- `cmd/emit`: profile generator. It renders a user template with a shape's value `y(t)` and sends it to the replayer. It must stay metric-agnostic: metric names and labels live in templates, never in code.
- `cmd/metric-producer`: demo producer run by docker-compose
- `internal/replayer`: server code, split by area: `streams.go`, `backfill.go`, `rules.go`, `scrapes.go`, `rulebackfill.go`
- `internal/shapes`: profiles as pure functions of time. A new shape is one function plus a registry entry. Keep them pure: same `t` and seed, same value.
- `tests/`: all unit tests (external `package tests`, exported API only); `tests/e2e/` needs the docker stack
- `examples/`: runnable scenarios (templates, rules, `run.sh`); `docs/`: API, configuration, profiles, demos, roadmap

## Commands

```bash
make test-unit     # unit tests (excludes e2e)
make test-e2e      # builds and starts the stack, runs e2e, tears it down
make up / make down / make clean   # stack: replayer :8081, Prometheus :9091; clean wipes data
make logs
```

Run `gofmt` and `go vet ./...` before committing.

## Conventions

- Keep `go 1.25.8` and the existing dependency versions in `go.mod`. Run `go mod tidy -go=1.25.8`; a plain tidy bumps the toolchain and breaks the `golang:1.25` Containerfiles.
- Tests go in `tests/`, not next to the code.
- Docs: keep README.md short; details belong in `docs/`. Claims in docs and demos must be measured against a real run, not predicted.
- Update `docs/roadmap.md` when a known limitation or idea is addressed.

## Gotchas

- Backfill and rule backfill only ever add samples. Re-running a scenario without `make clean && make up` mixes old and new data in the same series.
- `prometheus.yaml` is bind-mounted. After editing it, recreate the stack (`make clean && make up`); a running container keeps the old file.
- Load rules *after* backfilling the metrics they read, since rule backfill evaluates existing data.
- Prometheus graphs for docs: use a fixed resolution (`g0.res_type=fixed&g0.res_step=30`), and zero-fill `ALERTS` (`or label_replace(vector(0), ...)`). Otherwise short pulses and absent series render misleadingly.
