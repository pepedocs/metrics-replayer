# Profiles

`emit` generates metric data from a **shape** and a **template**, and sends it to the replayer.

```
 shape              template                replayer
 y(t) = 0.3   ──▶   metric lines      ──▶   /backfill, /push
 "what curve"       "what metric"
```

- A **shape** is a function of time that returns a number `y`. It knows nothing about metrics.
- A **template** is your file, and it turns `y` into metric lines: names, labels, counter or gauge, traffic rate. Supporting another metric means writing another template, not changing code.
- **`emit`** walks through time, asks the shape for `y`, renders the template and sends the result.

## Shapes

Time `t` is relative to the end of the window: `start=-2h` means 2 hours before the window ends. Every parameter is optional. They're passed as `--params key=value,key=value`.

| Shape | Profile | Parameters (defaults) |
|---|---|---|
| `step` | **Bad deployment**: jumps to a plateau, drops back on rollback | `baseline=0.01`, `peak=0.3`, `start=-2h`, `duration=30m` (`0` means no rollback) |
| `launch` | **GA / big-bang launch**: log-normal surge that decays slowly | `baseline=0.01`, `peak=0.5`, `start=-3h`, `peak_at=20m`, `sigma=1` |
| `spikes` | **Intermittent flake**: short, recurring spikes | `baseline=0`, `peak=1`, `every=1h`, `width=30s`, `offset=0` |
| `drift` | **Slow poison**: `baseline + a * (elapsed/unit)^b` | `baseline=0.01`, `a=0.01`, `b=1`, `start=-24h`, `unit=1h` |
| `flap` | **Flapping dependency**: `offset + amplitude * sin(2πt/period)`, floored at 0 | `offset=0.1`, `amplitude=0.1`, `period=10m` |
| `constant` | Flat baseline | `baseline=0.01` |

To add a shape, write one function in [`internal/shapes/shapes.go`](../internal/shapes/shapes.go) and add it to the registry.

## Templates

A template is a Go [text/template](https://pkg.go.dev/text/template) that renders one snapshot of metric lines. Don't add timestamps; `emit` does that.

| In the template | Meaning |
|---|---|
| `.Y` | Shape value at this point |
| `.T` | Seconds relative to the end of the window |
| `counter "name" <per-second>` | Adds `per-second × tick length` to a running total and prints it. Counters keep increasing across backfill and live. Use each name once. |
| `add`, `sub`, `mul`, `div` | Arithmetic, e.g. `mul 50 .Y` |

An error ratio (`errors / total = .Y`) at 50 requests per second, from [`error-ratio.tmpl`](../examples/profiles/error-ratio.tmpl):

```
http_requests_total{service="checkout",code="500"} {{ counter "err" (mul 50 .Y) }}
http_requests_total{service="checkout",code="200"} {{ counter "ok" (mul 50 (sub 1 .Y)) }}
```

A gauge that follows the shape directly, from [`gauge.tmpl`](../examples/profiles/gauge.tmpl):

```
node_filesystem_used_ratio{mountpoint="/data"} {{ .Y }}
```

## Running `emit`

```bash
go run ./cmd/emit --shape <shape> --params <k=v,...> --template <file> [flags]
```

| Flag | Default | |
|---|---|---|
| `--window` | `6h` | Scenario length. Shape times run from `-window` to `0`. |
| `--realtime` | `false` | Play the window live in real time instead of backfilling it |
| `--rules` | — | Rule file to load. In backfill mode it's also evaluated over the window. |
| `--step` | `30s` | Backfill resolution |
| `--interval` | `2s` | Live push interval |
| `--live` | `true` | Keep pushing after the backfilled window |
| `--stream` | `profile` | Stream for live pushes |
| `--dry-run` | `false` | Print the window as backfill data and exit, without sending anything |
| `--replayer` | `http://localhost:8081` | Replayer URL |

There are two modes:

- **Backfill (default)**: the window is written instantly and ends at now. Rules are evaluated over it, so you see `ALERTS` history right away. Live pushes then continue after the window.
- **Realtime (`--realtime`)**: nothing is backfilled. The window plays from its beginning at wall-clock speed, so you can watch series and alerts change as they happen. Choose a short window, and set shape times inside it.

## The five profiles

Each command uses the templates and [`rules.yaml`](../examples/profiles/rules.yaml) in [`examples/profiles`](../examples/profiles). Run one at a time, with `make clean && make up` in between.

| Profile | Command | What to check |
|---|---|---|
| Bad deploy | `--shape step --template error-ratio.tmpl` | `ErrorRatioHigh` fires ~3m after the deploy and resolves after the rollback |
| Launch | `--shape launch --params peak=0.6 --template error-ratio.tmpl` | `ErrorBudgetFastBurn` fires within minutes of the surge |
| Flake | `--shape spikes --params every=15m,width=30s --template error-ratio.tmpl` | `ErrorRatioHigh` never fires, because `for: 3m` outlasts each spike. `ErrorBudgetFastBurn` (no `for`) pages on every spike. |
| Flapping | `--shape flap --params offset=0.07,amplitude=0.05,period=10m --template error-ratio.tmpl` | `ErrorRatioHigh` stays one incident (`keep_firing_for`) instead of opening and resolving every few minutes |
| Slow poison | `--shape drift --params baseline=0.3,a=0.02,start=-12h --template gauge.tmpl` | `DiskWillFillIn48h` fires long before usage reaches 100% |

The paths are relative to `examples/profiles`. For example:

```bash
go run ./cmd/emit --shape step \
  --template examples/profiles/error-ratio.tmpl \
  --rules examples/profiles/rules.yaml
```
