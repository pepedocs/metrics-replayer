package replayer

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"

	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/prompb"
	"go.yaml.in/yaml/v3"
)

// maxQueryPoints stays under Prometheus' limit of 11000 points per series in
// one range query.
const maxQueryPoints = 10000

// staleNaN is Prometheus' staleness marker. Writing it where a series ends
// stops instant queries from carrying the last value forward for 5 minutes.
var staleNaN = math.Float64frombits(0x7ff0000000000002)

type ruleFile struct {
	Groups []struct {
		Name  string `yaml:"name"`
		Rules []struct {
			Record string            `yaml:"record"`
			Alert  string            `yaml:"alert"`
			Expr   string            `yaml:"expr"`
			For    string            `yaml:"for"`
			Labels map[string]string `yaml:"labels"`
		} `yaml:"rules"`
	} `yaml:"groups"`
}

// point is one evaluation result: a timestamp (ms) and value.
type point struct {
	t int64
	v float64
}

// backfillRules evaluates every rule over [end-duration, end] with range
// queries and remote-writes the results: recording rules as their recorded
// series, alerting rules as ALERTS{alertstate="pending|firing"}. Rules are
// evaluated in file order, so later rules can use earlier recorded series.
func (r *Replayer) backfillRules(data []byte, end time.Time, duration, step time.Duration) (samples int, err error) {
	var rf ruleFile
	if err := yaml.Unmarshal(data, &rf); err != nil {
		return 0, fmt.Errorf("parse rules: %w", err)
	}
	start := end.Add(-duration)

	for _, g := range rf.Groups {
		for _, rule := range g.Rules {
			results, err := r.queryRange(rule.Expr, start, end, step)
			if err != nil {
				return samples, fmt.Errorf("evaluate %s%s: %w", rule.Record, rule.Alert, err)
			}

			var series []prompb.TimeSeries
			if rule.Record != "" {
				for _, res := range results {
					lbls := withLabels(res.labels, rule.Labels)
					lbls[model.MetricNameLabel] = rule.Record
					series = append(series, toTimeSeries(lbls, markStale(res.points, step)))
				}
			} else {
				holdFor, err := model.ParseDuration(defaultString(rule.For, "0s"))
				if err != nil {
					return samples, fmt.Errorf("alert %s: invalid for: %w", rule.Alert, err)
				}
				for _, res := range results {
					lbls := withLabels(res.labels, rule.Labels)
					delete(lbls, model.MetricNameLabel)
					lbls[model.AlertNameLabel] = rule.Alert
					pending, firing := alertStates(res.points, step, time.Duration(holdFor))
					for state, pts := range map[string][]point{"pending": pending, "firing": firing} {
						if len(pts) == 0 {
							continue
						}
						stateLbls := withLabels(lbls, map[string]string{model.MetricNameLabel: "ALERTS", "alertstate": state})
						series = append(series, toTimeSeries(stateLbls, markStale(pts, step)))
					}
				}
			}
			if len(series) == 0 {
				continue
			}
			if err := r.remoteWrite(series); err != nil {
				return samples, fmt.Errorf("write %s%s: %w", rule.Record, rule.Alert, err)
			}
			for _, ts := range series {
				samples += len(ts.Samples)
			}
		}
	}
	return samples, nil
}

// alertStates splits the steps where the alert expression returned a value
// into pending and firing, applying the rule's `for` duration.
func alertStates(active []point, step, holdFor time.Duration) (pending, firing []point) {
	var since int64
	for i, p := range active {
		if i == 0 || p.t-active[i-1].t > step.Milliseconds() {
			since = p.t // a new active run starts
		}
		if time.Duration(p.t-since)*time.Millisecond >= holdFor {
			firing = append(firing, point{p.t, 1})
		} else {
			pending = append(pending, point{p.t, 1})
		}
	}
	return pending, firing
}

// markStale appends a staleness marker one step after every run of
// consecutive points, where the series stops.
func markStale(pts []point, step time.Duration) []point {
	var out []point
	for i, p := range pts {
		out = append(out, p)
		if i == len(pts)-1 || pts[i+1].t-p.t > step.Milliseconds() {
			out = append(out, point{p.t + step.Milliseconds(), staleNaN})
		}
	}
	return out
}

type rangeResult struct {
	labels map[string]string
	points []point
}

// queryRange runs a range query in chunks of at most maxQueryPoints steps and
// merges the results per series.
func (r *Replayer) queryRange(expr string, start, end time.Time, step time.Duration) ([]rangeResult, error) {
	merged := map[string]*rangeResult{}
	var keys []string
	chunk := time.Duration(maxQueryPoints-1) * step
	for from := start; !from.After(end); from = from.Add(chunk + step) {
		to := from.Add(chunk)
		if to.After(end) {
			to = end
		}
		q := url.Values{
			"query": {expr},
			"start": {strconv.FormatFloat(float64(from.UnixMilli())/1000, 'f', 3, 64)},
			"end":   {strconv.FormatFloat(float64(to.UnixMilli())/1000, 'f', 3, 64)},
			"step":  {strconv.FormatFloat(step.Seconds(), 'f', -1, 64)},
		}
		resp, err := http.PostForm(r.prometheusURL+"/api/v1/query_range", q)
		if err != nil {
			return nil, err
		}
		var body struct {
			Status string `json:"status"`
			Error  string `json:"error"`
			Data   struct {
				ResultType string `json:"resultType"`
				Result     []struct {
					Metric map[string]string `json:"metric"`
					Values [][2]any          `json:"values"`
				} `json:"result"`
			} `json:"data"`
		}
		err = json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("decode query_range: %w", err)
		}
		if body.Status != "success" {
			return nil, fmt.Errorf("query_range: %s", body.Error)
		}

		for _, res := range body.Data.Result {
			key := fmt.Sprint(sortedLabels(res.Metric))
			rr, ok := merged[key]
			if !ok {
				rr = &rangeResult{labels: res.Metric}
				merged[key] = rr
				keys = append(keys, key)
			}
			for _, v := range res.Values {
				ts, _ := v[0].(float64)
				s, _ := v[1].(string)
				f, err := strconv.ParseFloat(s, 64)
				if err != nil {
					continue
				}
				rr.points = append(rr.points, point{int64(math.Round(ts * 1000)), f})
			}
		}
	}

	sort.Strings(keys)
	out := make([]rangeResult, 0, len(keys))
	for _, k := range keys {
		out = append(out, *merged[k])
	}
	return out, nil
}

func withLabels(base, extra map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(extra))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

func sortedLabels(m map[string]string) []prompb.Label {
	labels := make([]prompb.Label, 0, len(m))
	for k, v := range m {
		labels = append(labels, prompb.Label{Name: k, Value: v})
	}
	sort.Slice(labels, func(i, j int) bool { return labels[i].Name < labels[j].Name })
	return labels
}

func toTimeSeries(lbls map[string]string, pts []point) prompb.TimeSeries {
	samples := make([]prompb.Sample, len(pts))
	for i, p := range pts {
		samples[i] = prompb.Sample{Timestamp: p.t, Value: p.v}
	}
	return prompb.TimeSeries{Labels: sortedLabels(lbls), Samples: samples}
}

func defaultString(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
