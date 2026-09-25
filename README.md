# metrics-replayer

Replay metric profiles through a real Prometheus and watch how your recording rules and alerts respond.

```
your script ──POST /push/{name}──> replayer <──scrape── Prometheus
                                      │                     ^
                            POST /backfill ──remote write────┤
                            POST /rules/{name} ──file + reload─┘
```

metrics-replayer is an HTTP server in front of Prometheus. You push live metrics, backfill history and load rules with plain `curl`: no protobuf, rule file paths or reload signals.

## Quick start

```bash
make up    # replayer on :8081, Prometheus on :9091

go run ./cmd/emit --shape step \
  --template examples/profiles/error-ratio.tmpl \
  --rules examples/profiles/rules.yaml
```

This backfills 6h of an error ratio with a bad deploy 2h ago, loads the rules and evaluates them over that history, then keeps pushing live. Open [localhost:9091](http://localhost:9091) and graph `service:http_error_ratio:rate5m` or `ALERTS` over 6h.

To watch it unfold live instead, add `--realtime --window 30m` and set the shape's times inside that window (see [profiles](docs/profiles.md)).

## Why

You have a failure shape in mind, whether from a real incident, a load test, or something you know can happen, and you want to know whether your alerts catch it. `promtool test rules` is good for deterministic CI checks, but its linear series DSL (`0+5x20`) can't express spikes, decay or slow drift, and it shows you no graph. metrics-replayer complements it: send the shape through a real Prometheus and look.

## Use cases

These are common uses, but the replayer isn't limited to them.

- **Alert development**: simulate an incident and check that the alert fires at the right threshold and clears on recovery.
- **Burn-rate validation**: backfill days of data to test slow-burn SLO alerts (e.g. 72h + 6h windows).
- **Incident replay**: replay captured production metrics and check whether new or changed rules would have caught the incident.
- **Visual analysis**: explore curves, recording rules and alerts together in Prometheus graphs, and tune thresholds and windows by eye.

## Documentation

- [API](docs/api.md): streams, backfill, rules and rule backfill
- [Profiles](docs/profiles.md): reusable error shapes (launch, step, spikes, drift, flap) and the `emit` generator
- [Configuration](docs/configuration.md): flags, Prometheus setup, troubleshooting
- Examples: [profiles](examples/profiles), [api-latency](examples/api-latency), [kube-apiserver](examples/kube-apiserver/README.md)

## Development

```bash
make up           # start the stack (make down to stop, make clean to wipe data)
make logs         # follow logs
make test-unit    # unit tests
make test-e2e     # e2e tests (starts and stops the stack)
```

## Acknowledgment

This project was written by a human with the assistance of an AI code assistant.
