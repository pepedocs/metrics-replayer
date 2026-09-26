// Package shapes defines error and load profiles as pure functions of time.
//
// A Shape returns a value y for a time t relative to now: negative t is in
// the past (backfill), positive t is in the future (live). Shapes know nothing
// about metrics; a template decides how y becomes metric lines.
package shapes

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Shape returns the value at time t relative to now.
type Shape func(t time.Duration) float64

// constructor builds a Shape from its parameters.
type constructor func(p *Params) Shape

var registry = map[string]constructor{
	"constant": constant,
	"launch":   launch,
	"step":     step,
	"spikes":   spikes,
	"drift":    drift,
	"flap":     flap,
}

// Names lists the available shapes.
func Names() []string {
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// New builds the named shape from "key=value,key=value" parameters. Unknown
// keys and malformed values are errors.
//
// Every shape also accepts noise parameters (see withNoise): noise, seed and
// noise_period.
func New(name, params string) (Shape, error) {
	build, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("unknown shape %q (available: %s)", name, strings.Join(Names(), ", "))
	}
	p, err := parseParams(params)
	if err != nil {
		return nil, err
	}
	s := withNoise(build(p), p)
	if err := p.check(); err != nil {
		return nil, fmt.Errorf("shape %s: %w", name, err)
	}
	return s, nil
}

// constant: y = baseline.
func constant(p *Params) Shape {
	baseline := p.Float("baseline", 0.01)
	return func(time.Duration) float64 { return baseline }
}

// launch: a GA / big-bang launch. A log-normal surge that starts at start,
// peaks at peak_at after it, then decays slowly back towards baseline.
func launch(p *Params) Shape {
	baseline := p.Float("baseline", 0.01)
	peak := p.Float("peak", 0.5)
	start := p.Duration("start", -3*time.Hour)
	peakAt := p.Duration("peak_at", 20*time.Minute)
	sigma := p.Float("sigma", 1)
	mu := math.Log(peakAt.Hours())
	return func(t time.Duration) float64 {
		x := (t - start).Hours()
		if x <= 0 {
			return baseline
		}
		d := math.Log(x) - mu
		return baseline + (peak-baseline)*math.Exp(-d*d/(2*sigma*sigma))
	}
}

// step: a bad deployment. Jumps from baseline to peak at start and stays
// there for duration (0 means no rollback). rise and fall turn the vertical
// edges into linear ramps: the deploy rolls out over rise, and the rollback
// drains over fall, starting at start+duration.
func step(p *Params) Shape {
	baseline := p.Float("baseline", 0.01)
	peak := p.Float("peak", 0.3)
	start := p.Duration("start", -2*time.Hour)
	duration := p.Duration("duration", 30*time.Minute)
	rise := p.Duration("rise", 0)
	fall := p.Duration("fall", 0)
	ramp := func(from, to float64, elapsed, length time.Duration) float64 {
		return from + (to-from)*float64(elapsed)/float64(length)
	}
	return func(t time.Duration) float64 {
		end := start + duration
		switch {
		case t < start:
			return baseline
		case t < start+rise:
			return ramp(baseline, peak, t-start, rise)
		case duration == 0 || t < end:
			return peak
		case t < end+fall:
			return ramp(peak, baseline, t-end, fall)
		default:
			return baseline
		}
	}
}

// spikes: intermittent flakes. peak for width, every period, otherwise baseline.
func spikes(p *Params) Shape {
	baseline := p.Float("baseline", 0)
	peak := p.Float("peak", 1)
	every := p.Duration("every", time.Hour)
	width := p.Duration("width", 30*time.Second)
	offset := p.Duration("offset", 0)
	if every <= 0 {
		p.fail("every must be positive")
		every = time.Hour
	}
	return func(t time.Duration) float64 {
		phase := ((t-offset)%every + every) % every
		if phase < width {
			return peak
		}
		return baseline
	}
}

// drift: slow poison. From start, y = baseline + a * (elapsed/unit)^b.
func drift(p *Params) Shape {
	baseline := p.Float("baseline", 0.01)
	a := p.Float("a", 0.01)
	b := p.Float("b", 1)
	start := p.Duration("start", -24*time.Hour)
	unit := p.Duration("unit", time.Hour)
	if unit <= 0 {
		p.fail("unit must be positive")
		unit = time.Hour
	}
	return func(t time.Duration) float64 {
		if t < start {
			return baseline
		}
		return baseline + a*math.Pow(float64(t-start)/float64(unit), b)
	}
}

// flap: a flapping dependency. y = offset + amplitude * sin(2πt / period),
// floored at 0.
func flap(p *Params) Shape {
	offset := p.Float("offset", 0.1)
	amplitude := p.Float("amplitude", 0.1)
	period := p.Duration("period", 10*time.Minute)
	if period <= 0 {
		p.fail("period must be positive")
		period = 10 * time.Minute
	}
	return func(t time.Duration) float64 {
		return math.Max(0, offset+amplitude*math.Sin(2*math.Pi*float64(t)/float64(period)))
	}
}

// withNoise makes any shape realistic by scaling it with smooth random
// jitter: y * (1 + noise * n(t)), where n(t) in [-1, 1] is value noise (random
// points every noise_period, smoothly blended, plus a finer layer for jagged
// detail). The result depends only on t and seed, so the same seed always
// reproduces the same curve, and backfill and live join seamlessly.
func withNoise(s Shape, p *Params) Shape {
	noise := p.Float("noise", 0)
	seed := p.Int("seed", 1)
	period := p.Duration("noise_period", 30*time.Second)
	if noise == 0 {
		return s
	}
	if period <= 0 {
		p.fail("noise_period must be positive")
		return s
	}
	return func(t time.Duration) float64 {
		n := 0.7*valueNoise(seed, t, period) + 0.3*valueNoise(seed+1, t, period/4)
		return math.Max(0, s(t)*(1+noise*n))
	}
}

// valueNoise returns a smooth pseudo-random value in [-1, 1]: random points at
// multiples of period, blended with smoothstep.
func valueNoise(seed int64, t, period time.Duration) float64 {
	k := int64(math.Floor(float64(t) / float64(period)))
	frac := float64(t-time.Duration(k)*period) / float64(period)
	frac = frac * frac * (3 - 2*frac)
	a, b := lattice(seed, k), lattice(seed, k+1)
	return a + (b-a)*frac
}

// lattice hashes (seed, k) to a value in [-1, 1] (splitmix64).
func lattice(seed, k int64) float64 {
	x := uint64(seed)*0x9e3779b97f4a7c15 ^ uint64(k)
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	x ^= x >> 31
	return float64(x>>11)/float64(1<<53)*2 - 1
}

// Params holds shape parameters and collects errors while they are read.
type Params struct {
	raw  map[string]string
	used map[string]bool
	errs []string
}

func parseParams(s string) (*Params, error) {
	p := &Params{raw: map[string]string{}, used: map[string]bool{}}
	for _, kv := range strings.Split(s, ",") {
		kv = strings.TrimSpace(kv)
		if kv == "" {
			continue
		}
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return nil, fmt.Errorf("invalid parameter %q: want key=value", kv)
		}
		p.raw[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return p, nil
}

// Float returns the named parameter, or def if it is not set.
func (p *Params) Float(key string, def float64) float64 {
	p.used[key] = true
	v, ok := p.raw[key]
	if !ok {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		p.fail(fmt.Sprintf("%s: %q is not a number", key, v))
	}
	return f
}

// Int returns the named integer parameter, or def if it is not set.
func (p *Params) Int(key string, def int64) int64 {
	p.used[key] = true
	v, ok := p.raw[key]
	if !ok {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		p.fail(fmt.Sprintf("%s: %q is not an integer", key, v))
	}
	return n
}

// Duration returns the named parameter (e.g. "-2h", "30m"), or def if unset.
func (p *Params) Duration(key string, def time.Duration) time.Duration {
	p.used[key] = true
	v, ok := p.raw[key]
	if !ok {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		p.fail(fmt.Sprintf("%s: %q is not a duration", key, v))
	}
	return d
}

func (p *Params) fail(msg string) { p.errs = append(p.errs, msg) }

func (p *Params) check() error {
	for k := range p.raw {
		if !p.used[k] {
			p.fail(fmt.Sprintf("unknown parameter %q", k))
		}
	}
	if len(p.errs) > 0 {
		sort.Strings(p.errs)
		return fmt.Errorf("%s", strings.Join(p.errs, "; "))
	}
	return nil
}
