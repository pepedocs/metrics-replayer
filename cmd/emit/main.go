// Command emit drives a metric template with a shape and sends the result to
// the replayer: first as a timestamped backfill, then as live pushes.
//
// The shape decides the value y at each point in time; the template decides
// which metric lines that value becomes. emit itself knows nothing about
// metrics.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/pepedocs/metrics-replayer/internal/shapes"
)

func main() {
	replayerURL := flag.String("replayer", "http://localhost:8081", "replayer base URL")
	stream := flag.String("stream", "profile", "stream to push live data to")
	shapeName := flag.String("shape", "", "shape: "+strings.Join(shapes.Names(), ", "))
	params := flag.String("params", "", "shape parameters, e.g. peak=0.3,start=-2h")
	tmplPath := flag.String("template", "", "metric template file")
	window := flag.Duration("window", 6*time.Hour, "scenario length; shape times run from -window to 0")
	realtime := flag.Bool("realtime", false, "play the window live in real time instead of backfilling it")
	step := flag.Duration("step", 30*time.Second, "backfill resolution")
	live := flag.Bool("live", true, "keep pushing live after the window (backfill mode)")
	interval := flag.Duration("interval", 2*time.Second, "live push interval")
	rulesPath := flag.String("rules", "", "rule file to load; in backfill mode it is also evaluated over the window")
	dryRun := flag.Bool("dry-run", false, "print the window as backfill data instead of sending anything, then exit")
	flag.Parse()

	if *shapeName == "" || *tmplPath == "" || *window <= 0 {
		flag.Usage()
		os.Exit(2)
	}
	// Noise is random per run unless a seed is given; log the seed so a run
	// can be reproduced.
	if hasParam(*params, "noise") && !hasParam(*params, "seed") {
		*params = strings.TrimPrefix(*params+",seed="+strconv.FormatInt(rand.Int64N(1<<31), 10), ",")
	}
	shape, err := shapes.New(*shapeName, *params)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("shape=%s params=%q", *shapeName, *params)
	e, err := newEmitter(*tmplPath, shape)
	if err != nil {
		log.Fatal(err)
	}

	start := time.Now()
	// offset maps wall-clock time since start to shape time. Backfill mode
	// writes the window up to now and continues after it; realtime mode
	// starts at the beginning of the window.
	offset := time.Duration(0)
	if *realtime && !*dryRun {
		offset = -*window
	} else {
		var body bytes.Buffer
		for t := -*window; t <= 0; t += *step {
			if err := e.render(&body, t, *step, start.Add(t)); err != nil {
				log.Fatal(err)
			}
		}
		if *dryRun {
			io.Copy(os.Stdout, &body)
			return
		}
		out, err := post(*replayerURL+"/backfill", &body, http.StatusOK)
		if err != nil {
			log.Fatalf("backfill: %v", err)
		}
		log.Printf("backfill: %s", out)
	}

	if *rulesPath != "" {
		query := ""
		if !*realtime {
			query = "?backfill=" + window.String() + "&step=" + step.String()
		}
		if err := loadRules(*replayerURL, *rulesPath, query); err != nil {
			log.Fatalf("rules: %v", err)
		}
	}
	if !*live && !*realtime {
		return
	}

	if _, err := post(*replayerURL+"/register/"+*stream, nil, http.StatusCreated, http.StatusConflict); err != nil {
		log.Fatalf("register: %v", err)
	}
	if *realtime {
		// Registering waits for Prometheus to scrape; start the clock after
		// it so the beginning of the window isn't skipped.
		start = time.Now()
		log.Printf("playing %s window in real time to stream %q (ctrl-c to stop)", *window, *stream)
	} else {
		log.Printf("pushing live to stream %q every %s (ctrl-c to stop)", *stream, *interval)
	}
	last := start
	for {
		time.Sleep(*interval)
		now := time.Now()
		var body bytes.Buffer
		if err := e.render(&body, offset+now.Sub(start), now.Sub(last), time.Time{}); err != nil {
			log.Fatal(err)
		}
		last = now
		if _, err := post(*replayerURL+"/push/"+*stream, &body, http.StatusOK); err != nil {
			log.Printf("push: %v", err)
		}
	}
}

// loadRules posts a rule file, named after the file, with an optional query.
func loadRules(replayerURL, path, query string) error {
	rules, err := os.Open(path)
	if err != nil {
		return err
	}
	defer rules.Close()
	name := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	out, err := post(replayerURL+"/rules/"+name+query, rules, http.StatusCreated)
	if err != nil {
		return err
	}
	log.Printf("rules %q loaded %s", name, out)
	return nil
}

// emitter renders the template once per tick, keeping counter state between
// ticks so counters never reset across backfill and live.
type emitter struct {
	tmpl     *template.Template
	shape    shapes.Shape
	counters map[string]float64
	dt       time.Duration // length of the current tick, used by counter
}

// data is what the template sees.
type data struct {
	Y value // shape value at this tick
	T value // seconds relative to now (negative during backfill)
}

// value prints without float noise (0.21, not 0.21000000000000002).
type value float64

func (v value) String() string {
	return strconv.FormatFloat(math.Round(float64(v)*1e9)/1e9, 'f', -1, 64)
}

func newEmitter(path string, shape shapes.Shape) (*emitter, error) {
	e := &emitter{shape: shape, counters: map[string]float64{}}
	tmpl, err := template.New(filepath.Base(path)).Funcs(template.FuncMap{
		"counter": e.counter,
		"add":     func(v ...any) float64 { return fold(v, func(a, b float64) float64 { return a + b }) },
		"sub":     func(v ...any) float64 { return fold(v, func(a, b float64) float64 { return a - b }) },
		"mul":     func(v ...any) float64 { return fold(v, func(a, b float64) float64 { return a * b }) },
		"div":     func(v ...any) float64 { return fold(v, func(a, b float64) float64 { return a / b }) },
	}).ParseFiles(path)
	if err != nil {
		return nil, err
	}
	e.tmpl = tmpl
	return e, nil
}

// counter adds perSecond * tick length to the named counter and returns its
// new total. Use each name once per template.
func (e *emitter) counter(name string, perSecond any) string {
	e.counters[name] += math.Max(0, toFloat(perSecond)) * e.dt.Seconds()
	return strconv.FormatFloat(math.Round(e.counters[name]*1000)/1000, 'f', -1, 64)
}

// render writes one tick. A non-zero ts appends that timestamp to every
// sample line, as /backfill requires, and drops comment lines, since
// repeating # TYPE lines for every tick is invalid.
func (e *emitter) render(w io.Writer, t, dt time.Duration, ts time.Time) error {
	e.dt = dt
	var buf bytes.Buffer
	if err := e.tmpl.Execute(&buf, data{Y: value(e.shape(t)), T: value(t.Seconds())}); err != nil {
		return err
	}
	for _, line := range strings.Split(buf.String(), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !ts.IsZero() {
			if strings.HasPrefix(line, "#") {
				continue
			}
			line += " " + strconv.FormatInt(ts.UnixMilli(), 10)
		}
		fmt.Fprintln(w, line)
	}
	return nil
}

func post(url string, body io.Reader, ok ...int) (string, error) {
	resp, err := http.Post(url, "text/plain", body)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	for _, code := range ok {
		if resp.StatusCode == code {
			return strings.TrimSpace(string(out)), nil
		}
	}
	return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(out)))
}

func fold(v []any, f func(a, b float64) float64) float64 {
	if len(v) == 0 {
		return 0
	}
	acc := toFloat(v[0])
	for _, x := range v[1:] {
		acc = f(acc, toFloat(x))
	}
	return acc
}

func toFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case value:
		return float64(n)
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case string:
		f, _ := strconv.ParseFloat(n, 64)
		return f
	}
	return 0
}

// hasParam reports whether "key=value,..." params set key.
func hasParam(params, key string) bool {
	for _, kv := range strings.Split(params, ",") {
		if k, _, _ := strings.Cut(strings.TrimSpace(kv), "="); k == key {
			return true
		}
	}
	return false
}
