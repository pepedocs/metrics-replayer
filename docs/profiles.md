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

Each parameter is explained in the [parameter reference](#parameter-reference) below.

| Shape | Profile | Parameters (defaults) |
|---|---|---|
| `step` | **Bad deployment**: jumps to a plateau, drops back on rollback | `baseline=0.01`, `peak=0.3`, `start=-2h`, `duration=30m` (`0` means no rollback), `rise=0`, `fall=0` |
| `launch` | **GA / big-bang launch**: log-normal surge that decays slowly | `baseline=0.01`, `peak=0.5`, `start=-3h`, `peak_at=20m`, `sigma=1` |
| `spikes` | **Intermittent flake**: short, recurring spikes | `baseline=0`, `peak=1`, `every=1h`, `width=30s`, `offset=0` |
| `drift` | **Slow poison**: `baseline + a * (elapsed/unit)^b` | `baseline=0.01`, `a=0.01`, `b=1`, `start=-24h`, `unit=1h` |
| `flap` | **Flapping dependency**: `offset + amplitude * sin(2πt/period)`, floored at 0 | `offset=0.1`, `amplitude=0.1`, `period=10m` |
| `constant` | Flat baseline | `baseline=0.01` |

Every shape also takes `noise`, `seed` and `noise_period` to make it [realistic](#realistic-noise).

### Parameter reference

Durations use Go syntax (`30s`, `15m`, `2h`) and can be negative. Times like `start` are relative to the end of the window, so `-2h` means 2 hours before it ends. The value `y` is whatever your template makes of it; with `error-ratio.tmpl` it's the fraction of failing requests (`0.3` = 30%).

#### `step`: bad deployment

`y = peak` from `start` for `duration`, otherwise `baseline`, with optional linear ramps on both edges.

| Param | Default | Meaning |
|---|---|---|
| `baseline` | `0.01` | Value before the deploy and after the rollback |
| `peak` | `0.3` | Value while the bad build is live |
| `start` | `-2h` | When the deploy happens |
| `duration` | `30m` | How long until the rollback starts. `0` means it never rolls back. |
| `rise` | `0` | How long the deploy takes to roll out: the value ramps from `baseline` to `peak` over this time. `0` is a vertical edge. |
| `fall` | `0` | How long the rollback takes to drain, ramping from `peak` back to `baseline`, starting at `start + duration` |

#### `launch`: GA / big-bang launch

A log-normal surge: it rises almost vertically, peaks, then decays slowly with a long tail. `y = baseline + (peak − baseline) · exp(−(ln x − ln peak_at)² / 2σ²)`, where `x` is the time since `start`.

| Param | Default | Meaning |
|---|---|---|
| `baseline` | `0.01` | Value before the launch, and what the tail decays towards |
| `peak` | `0.5` | Highest value reached |
| `start` | `-3h` | When the launch begins |
| `peak_at` | `20m` | Time from `start` to the peak |
| `sigma` | `1` | Width of the surge. Larger means a wider peak and a longer tail; smaller means a sharper spike that recovers quickly. |

#### `spikes`: intermittent flake

`y = peak` for `width` at the start of every `every` period, otherwise `baseline`.

| Param | Default | Meaning |
|---|---|---|
| `baseline` | `0` | Value between spikes |
| `peak` | `1` | Value during a spike (`1` = every request fails) |
| `every` | `1h` | Time between spike starts, e.g. an hourly cron job |
| `width` | `30s` | How long each spike lasts. Keep it at or below the scrape/eval interval to mimic a single-sample blip. |
| `offset` | `0` | Shifts all spikes in time, to line them up with something else |

#### `drift`: slow poison

`y = baseline + a · (elapsed / unit)^b` from `start`, otherwise `baseline`.

| Param | Default | Meaning |
|---|---|---|
| `baseline` | `0.01` | Value before the drift starts |
| `a` | `0.01` | Growth per `unit` (with `b=1`, `y` grows by `a` every `unit`) |
| `b` | `1` | Curvature. `1` is linear (disk filling at a steady rate); above `1` accelerates (a leak that gets worse). |
| `start` | `-24h` | When the drift begins. It can be before the window, so the window opens mid-drift. |
| `unit` | `1h` | Time unit for `a` |

Example: `baseline=0.3,a=0.02,start=-12h` starts at 30% disk usage and adds 2 percentage points per hour.

#### `flap`: flapping dependency

`y = offset + amplitude · sin(2π t / period)`, floored at 0.

| Param | Default | Meaning |
|---|---|---|
| `offset` | `0.1` | Center line of the wave |
| `amplitude` | `0.1` | How far it swings above and below `offset` |
| `period` | `10m` | Length of one fail-recover cycle |

To test alert flapping, put your threshold between `offset − amplitude` and `offset + amplitude`. Where it sits decides how long each cycle spends above and below it. For example, with `offset=0.07,amplitude=0.05` and a 5% threshold, each 10m cycle spends about 6.3m above and 3.7m below.

#### `constant`

| Param | Default | Meaning |
|---|---|---|
| `baseline` | `0.01` | The value, at all times |

#### Realistic noise

Real signals are never clean rectangles. These parameters work on every shape and add smooth, random jitter on top of the curve:

`y_noisy = y · (1 + noise · n(t))`, where `n(t)` is smooth random noise between −1 and 1.

| Param | Default | Meaning |
|---|---|---|
| `noise` | `0` (off) | Relative jitter. `0.25` lets the value wander ±25% around the curve: a 30% plateau moves between about 22% and 38%, and a 1% baseline between 0.75% and 1.25%. |
| `seed` | random | Picks the random pattern. `emit` chooses a new one each run and logs it (`shape=step params="…,seed=626392463"`). Pass the same seed to reproduce a run exactly. |
| `noise_period` | `30s` | How fast the noise changes. The curve drifts between new random points every `noise_period`, with finer jagged detail on top. Larger values are slower and smoother. |

The noise depends only on time and the seed, so backfilled and live data join without a seam.

```bash
# A bad deploy that rolls out over 2m, a jagged plateau, and a rollback that drains over 5m
--shape step --params peak=0.3,rise=2m,fall=5m,noise=0.25
```

To add a shape, write one function in [`internal/shapes/shapes.go`](../internal/shapes/shapes.go) and add it to the registry.

## Templates

A template is a Go [text/template](https://pkg.go.dev/text/template) that renders one snapshot of metric lines. Don't add timestamps; `emit` does that.

### Values

| Field | Type | Meaning |
|---|---|---|
| `.Y` | number | Shape value at this tick |
| `.T` | number | Seconds relative to the end of the window: negative inside a backfilled window, positive after it |

### Functions

Functions use template call syntax: `func arg1 arg2`. Nest calls in parentheses: `mul 50 (sub 1 .Y)`.

#### `counter NAME PER_SECOND`

A monotonically increasing counter.

| Param | Type | Meaning |
|---|---|---|
| `NAME` | string | Identifies the running total. Each distinct name is a separate counter. |
| `PER_SECOND` | number | Rate for this tick. It's multiplied by the tick length (`--step` in backfill, the time since the last push in live), so rates stay consistent whatever the resolution. Negative values count as `0`. |

It returns the new total, rounded to 3 decimals. Totals carry over from the backfill into live pushes, so the counter never resets. Call each `NAME` only once per template, because every call adds to the total.

For example, `{{ counter "err" (mul 50 .Y) }}` counts errors at 50 requests per second times the error ratio `.Y`.

#### `add`, `sub`, `mul`, `div` `A B [C ...]`

Arithmetic on two or more numbers, applied left to right: `sub 1 .Y` is `1 − Y`, and `div 10 2 5` is `(10 / 2) / 5 = 1`.

| Param | Type | Meaning |
|---|---|---|
| `A B ...` | numbers | Literals (`50`, `0.5`), `.Y`, `.T`, or the result of another function |

The built-in text/template functions such as `printf` also work, e.g. `{{ printf "%.2f" .Y }}`.

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
