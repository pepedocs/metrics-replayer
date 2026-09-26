# metrics-replayer

A studio for studying metrics and alerts.

Describe how a system misbehaves, whether it's a bad deploy, a launch surge, a flaky dependency or a slow leak. Play it through a real Prometheus and watch what your alerts do. It takes seconds, because history is backfilled and rules are evaluated over it, including `for`. If you'd rather watch it unfold, play it in real time. The Prometheus console is the lens: the metric, the recording rules and the alert states, side by side.

## What it's for

Understanding alerts, before and after they matter:

- **Find blind spots.** Which failures does this alert miss? A slow drift below the threshold, a flake that clears before `for` runs out, a surge the burn-rate windows smooth away.
  *Try it:* in the [flaky dependency demo](docs/demo.md), a threshold alert never fires, while a fast-burn alert pages on every spike, 24 times in 6h.
- **Plan and decide.** Try thresholds, windows and `for` durations against realistic profiles before an alert reaches production. Where an alert can't cover a failure, decide what the SRE team does instead.
  *Try it:* the [flapping profile](docs/profiles.md#the-five-profiles) turns one dependency's flaps into one incident with `keep_firing_for`; delete that line from the rules and re-run to see the difference.
- **Explain.** Show a reviewer, a teammate or a postmortem why an alert paged 24 times, or never.
  *Try it:* the [kube-apiserver example](examples/kube-apiserver/README.md) shows a p99 alert firing on a long tail while p50 stays flat.

Under the hood it's a small, fast tool. It simulates metric profiles (error rates, traffic, anything you can template), evaluates alerts instantly over backfilled history or live in real time, and leaves the evidence in Prometheus to look at.

```
profile (shape + template) ──▶ replayer ──▶ Prometheus ──▶ graphs, recording rules, ALERTS
```

## Quick start

```bash
make up    # replayer on :8081, Prometheus on :9091

go run ./cmd/emit --shape step \
  --template examples/profiles/error-ratio.tmpl \
  --rules examples/profiles/rules.yaml
```

This plays a bad deploy from 2h ago into 6h of history and evaluates the rules over it. Open [localhost:9091](http://localhost:9091) and graph `service:http_error_ratio:rate5m` and `ALERTS` over 6h.

Add `--realtime --window 30m` to watch it live instead, or `noise=0.25` to `--params` to make the curve realistically jagged.

## Documentation

- Demos: [a flaky dependency](docs/demo.md), and [blind spots of burn-rate alerts](docs/demo-fast-burn.md) on realistic, jagged traffic
- [Profiles](docs/profiles.md): the built-in failure shapes and how to describe your own metrics
- [API](docs/api.md): pushing, backfilling and loading rules directly with `curl`
- [Configuration](docs/configuration.md): flags, Prometheus setup, troubleshooting
- [Known limitations and enhancements](docs/roadmap.md)
- Examples: [profiles](examples/profiles), [fast-burn](examples/fast-burn), [api-latency](examples/api-latency), [kube-apiserver](examples/kube-apiserver/README.md)

## Development

```bash
make up           # start the stack (make down to stop, make clean to wipe data)
make logs         # follow logs
make test-unit    # unit tests
make test-e2e     # e2e tests (starts and stops the stack)
```

## Acknowledgment

This project was written by a human with the assistance of an AI code assistant.
