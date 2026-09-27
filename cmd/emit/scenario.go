package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/pepedocs/metrics-replayer/internal/shapes"
	"go.yaml.in/yaml/v3"
)

// scenario describes a set of instances to replay and the rules to evaluate
// over them. Paths are relative to the scenario file.
type scenario struct {
	Window        string     `yaml:"window"`         // history to backfill per instance, e.g. 144h
	Step          string     `yaml:"step"`           // backfill and evaluation resolution
	Template      string     `yaml:"template"`       // default template for all instances
	Rules         string     `yaml:"rules"`          // rule file to load after the backfills
	RulesBackfill string     `yaml:"rules_backfill"` // how far back to evaluate the rules
	Instances     []instance `yaml:"instances"`
}

type instance struct {
	Name     string   `yaml:"name"`
	Shape    string   `yaml:"shape"`
	Params   string   `yaml:"params"`
	Template string   `yaml:"template"`
	Alert    string   `yaml:"alert"`    // alert that must fire, e.g. MyAlert{severity="page"}; may use {{ .Name }}
	Incident incident `yaml:"incident"` // when the incident happens, relative to the end of the window
}

type incident struct {
	Start string `yaml:"start"` // e.g. -70h
	End   string `yaml:"end"`   // e.g. -67h; empty means still ongoing
}

// runRecord remembers when each instance's window ended, so a report can be
// produced later with --report-only.
type runRecord struct {
	Ends map[string]time.Time `json:"ends"`
}

func runScenario(path, replayerURL, prometheusURL string, replay, report, markdown bool) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var sc scenario
	if err := yaml.Unmarshal(raw, &sc); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	dir := filepath.Dir(path)
	window, err := parseDur("window", sc.Window, 6*time.Hour)
	if err != nil {
		return err
	}
	step, err := parseDur("step", sc.Step, 30*time.Second)
	if err != nil {
		return err
	}
	rulesBackfill, err := parseDur("rules_backfill", sc.RulesBackfill, window)
	if err != nil {
		return err
	}
	recordPath := filepath.Join(dir, ".last-run.json")

	rec := runRecord{Ends: map[string]time.Time{}}
	if replay {
		for _, in := range sc.Instances {
			end, err := replayInstance(in, dir, sc.Template, replayerURL, window, step)
			if err != nil {
				return fmt.Errorf("instance %s: %w", in.Name, err)
			}
			rec.Ends[in.Name] = end
		}
		if sc.Rules != "" {
			query := "?backfill=" + rulesBackfill.String() + "&step=" + step.String()
			if err := loadRules(replayerURL, filepath.Join(dir, sc.Rules), query); err != nil {
				return fmt.Errorf("rules: %w", err)
			}
		}
		b, _ := json.MarshalIndent(rec, "", "  ")
		if err := os.WriteFile(recordPath, b, 0o644); err != nil {
			return err
		}
	} else {
		b, err := os.ReadFile(recordPath)
		if err != nil {
			return fmt.Errorf("no earlier run to report on (%s): %w", recordPath, err)
		}
		if err := json.Unmarshal(b, &rec); err != nil {
			return err
		}
	}

	if !report {
		return nil
	}
	var rows []reportRow
	for _, in := range sc.Instances {
		end, ok := rec.Ends[in.Name]
		if !ok {
			return fmt.Errorf("instance %s missing from %s; replay first", in.Name, recordPath)
		}
		row, err := evaluate(in, prometheusURL, end, rulesBackfill, step)
		if err != nil {
			return fmt.Errorf("report %s: %w", in.Name, err)
		}
		rows = append(rows, row)
	}
	printReport(os.Stdout, rows, markdown)
	return nil
}

// replayInstance backfills one instance and returns the end of its window.
func replayInstance(in instance, dir, defaultTemplate, replayerURL string, window, step time.Duration) (time.Time, error) {
	tmpl := in.Template
	if tmpl == "" {
		tmpl = defaultTemplate
	}
	if tmpl == "" || in.Shape == "" || in.Name == "" {
		return time.Time{}, fmt.Errorf("name, shape and template are required")
	}
	shape, err := shapes.New(in.Shape, in.Params)
	if err != nil {
		return time.Time{}, err
	}
	e, err := newEmitter(filepath.Join(dir, tmpl), shape, in.Name)
	if err != nil {
		return time.Time{}, err
	}
	end := time.Now()
	var body bytes.Buffer
	for t := -window; t <= 0; t += step {
		if err := e.render(&body, t, step, end.Add(t)); err != nil {
			return time.Time{}, err
		}
	}
	out, err := post(replayerURL+"/backfill", &body, http.StatusOK)
	if err != nil {
		return time.Time{}, fmt.Errorf("backfill: %w", err)
	}
	log.Printf("%s: %s", in.Name, out)
	return end, nil
}

type reportRow struct {
	name     string
	fired    bool
	toFire   string
	firings  int
	late     string
	incident string
}

// evaluate checks the instance's alert over the rules' evaluation range.
func evaluate(in instance, prometheusURL string, end time.Time, rulesBackfill, step time.Duration) (reportRow, error) {
	row := reportRow{name: in.Name, toFire: "-", late: "-"}
	start, err := parseDur("incident.start", in.Incident.Start, -rulesBackfill)
	if err != nil {
		return row, err
	}
	incStart := end.Add(start)
	incEnd := end
	row.incident = "ongoing"
	if in.Incident.End != "" {
		d, err := time.ParseDuration(in.Incident.End)
		if err != nil {
			return row, fmt.Errorf("incident.end: %w", err)
		}
		incEnd = end.Add(d)
		row.incident = human(incEnd.Sub(incStart))
	}

	selector, err := alertSelector(in)
	if err != nil {
		return row, err
	}
	times, err := firingTimes(prometheusURL, selector, end.Add(-rulesBackfill), end, step)
	if err != nil {
		return row, err
	}
	runs := toRuns(times, step)
	row.firings = len(runs)
	row.fired = len(runs) > 0
	if row.fired {
		row.toFire = human(runs[0][0].Sub(incStart))
		if in.Incident.End != "" {
			for _, r := range runs {
				if r[0].After(incEnd) {
					row.late = human(r[0].Sub(incEnd)) + " after it ended"
					break
				}
			}
		}
	}
	return row, nil
}

// alertSelector turns `Name{labels}` into a query for its firing ALERTS series.
func alertSelector(in instance) (string, error) {
	t, err := template.New("alert").Parse(in.Alert)
	if err != nil {
		return "", fmt.Errorf("alert: %w", err)
	}
	var b strings.Builder
	if err := t.Execute(&b, struct{ Name string }{in.Name}); err != nil {
		return "", err
	}
	a := strings.TrimSpace(b.String())
	if a == "" {
		return "", fmt.Errorf("alert is required")
	}
	name, labels := a, ""
	if i := strings.Index(a, "{"); i >= 0 {
		name, labels = a[:i], strings.TrimSuffix(strings.TrimSpace(a[i+1:]), "}")
	}
	sel := fmt.Sprintf(`ALERTS{alertname=%q,alertstate="firing"`, strings.TrimSpace(name))
	if labels != "" {
		sel += "," + labels
	}
	return sel + "}", nil
}

// firingTimes returns the evaluation timestamps at which any matching series
// was firing.
func firingTimes(prometheusURL, selector string, from, to time.Time, step time.Duration) ([]time.Time, error) {
	q := url.Values{
		"query": {selector},
		"start": {strconv.FormatInt(from.Unix(), 10)},
		"end":   {strconv.FormatInt(to.Unix(), 10)},
		"step":  {strconv.FormatFloat(step.Seconds(), 'f', -1, 64)},
	}
	resp, err := http.Get(prometheusURL + "/api/v1/query_range?" + q.Encode())
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var res struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			Result []struct {
				Values [][2]any `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, fmt.Errorf("decode query_range: %w", err)
	}
	if res.Status != "success" {
		return nil, fmt.Errorf("query_range: %s", res.Error)
	}
	seen := map[int64]bool{}
	var out []time.Time
	for _, r := range res.Data.Result {
		for _, v := range r.Values {
			ts, _ := v[0].(float64)
			ms := int64(ts * 1000)
			if !seen[ms] {
				seen[ms] = true
				out = append(out, time.UnixMilli(ms))
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Before(out[j]) })
	return out, nil
}

// toRuns groups consecutive timestamps (at most one step apart) into runs.
func toRuns(times []time.Time, step time.Duration) [][2]time.Time {
	var runs [][2]time.Time
	for i, t := range times {
		if i == 0 || t.Sub(times[i-1]) > step {
			runs = append(runs, [2]time.Time{t, t})
		} else {
			runs[len(runs)-1][1] = t
		}
	}
	return runs
}

func printReport(w io.Writer, rows []reportRow, markdown bool) {
	head := []string{"instance", "incident", "fired", "time to fire", "firings", "late"}
	var lines [][]string
	for _, r := range rows {
		fired := "no"
		if r.fired {
			fired = "yes"
		}
		lines = append(lines, []string{r.name, r.incident, fired, r.toFire, strconv.Itoa(r.firings), r.late})
	}
	if markdown {
		fmt.Fprintln(w, "| "+strings.Join(head, " | ")+" |")
		fmt.Fprintln(w, "|"+strings.Repeat("---|", len(head)))
		for _, l := range lines {
			fmt.Fprintln(w, "| "+strings.Join(l, " | ")+" |")
		}
		return
	}
	width := make([]int, len(head))
	for _, l := range append([][]string{head}, lines...) {
		for i, c := range l {
			width[i] = max(width[i], len(c))
		}
	}
	for _, l := range append([][]string{head}, lines...) {
		for i, c := range l {
			fmt.Fprintf(w, "%-*s  ", width[i], c)
		}
		fmt.Fprintln(w)
	}
}

func parseDur(field, s string, def time.Duration) (time.Duration, error) {
	if s == "" {
		return def, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", field, err)
	}
	return d, nil
}

// human formats a duration compactly: 32m, 3.2h.
func human(d time.Duration) string {
	if d < 0 {
		return "-" + human(-d)
	}
	if d < 90*time.Minute {
		return fmt.Sprintf("%dm", int(d.Round(time.Minute).Minutes()))
	}
	return strconv.FormatFloat(float64(d.Round(6*time.Minute))/float64(time.Hour), 'f', -1, 64) + "h"
}
