package replayer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gogo/protobuf/proto"
	"github.com/golang/snappy"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/prompb"
)

type Replayer struct {
	mu             sync.RWMutex
	streams        map[string][]string
	mux            *http.ServeMux
	remoteWriteURL string
	prometheusURL  string
	rulesDir       string
	bearerToken    string
	stripFor       bool
	scrapesDir     string
	scrapeTarget   string
	scrapeTimeout  time.Duration
}

type Option func(*Replayer)

func WithRemoteWriteURL(url string) Option {
	return func(r *Replayer) { r.remoteWriteURL = url }
}

func WithBearerToken(token string) Option {
	return func(r *Replayer) { r.bearerToken = token }
}

func WithPrometheusURL(url string) Option {
	return func(r *Replayer) { r.prometheusURL = url }
}

func WithRulesDir(dir string) Option {
	return func(r *Replayer) { r.rulesDir = dir }
}

func WithStripFor(strip bool) Option {
	return func(r *Replayer) { r.stripFor = strip }
}

// WithScrapesDir enables writing a Prometheus scrape config file per stream
// on register. Requires WithPrometheusURL for the reload.
func WithScrapesDir(dir string) Option {
	return func(r *Replayer) { r.scrapesDir = dir }
}

// WithScrapeTarget sets the host:port Prometheus uses to reach the replayer.
func WithScrapeTarget(target string) Option {
	return func(r *Replayer) { r.scrapeTarget = target }
}

// WithScrapeTimeout sets how long register waits for the new target to be up.
func WithScrapeTimeout(d time.Duration) Option {
	return func(r *Replayer) { r.scrapeTimeout = d }
}

func New(opts ...Option) *Replayer {
	r := &Replayer{
		streams:       make(map[string][]string),
		mux:           http.NewServeMux(),
		scrapeTimeout: 30 * time.Second,
	}
	for _, o := range opts {
		o(r)
	}
	r.mux.HandleFunc("/register/", r.handleRegister)
	r.mux.HandleFunc("/push/", r.handlePush)
	r.mux.HandleFunc("/metrics/", r.handleMetrics)
	r.mux.HandleFunc("/backfill", r.handleBackfill)
	r.mux.HandleFunc("/rules/", r.handleRules)
	r.mux.HandleFunc("/rules", r.handleRulesList)
	return r
}

func nameFromPath(prefix, path string) string {
	return strings.TrimPrefix(path, prefix)
}

func (r *Replayer) Handler() http.Handler {
	return r.mux
}

func (r *Replayer) handleRegister(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	name := nameFromPath("/register/", req.URL.Path)
	if name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}

	if r.scrapesDir != "" && !validStreamName.MatchString(name) {
		http.Error(w, "invalid name: must match "+validStreamName.String(), http.StatusBadRequest)
		return
	}

	r.mu.Lock()
	if _, exists := r.streams[name]; exists {
		r.mu.Unlock()
		http.Error(w, "already registered", http.StatusConflict)
		return
	}
	r.streams[name] = nil
	r.mu.Unlock()

	// The stream must exist before Prometheus starts scraping it, and the lock
	// must not be held while waiting for that scrape.
	if r.scrapesDir != "" {
		if err := r.addScrapeConfig(name); err != nil {
			r.mu.Lock()
			delete(r.streams, name)
			r.mu.Unlock()
			http.Error(w, "add scrape config: "+err.Error(), http.StatusBadGateway)
			return
		}
	}

	log.Printf("registered stream %q", name)
	w.WriteHeader(http.StatusCreated)
}

func (r *Replayer) handlePush(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	name := nameFromPath("/push/", req.URL.Path)
	if name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}

	body, err := io.ReadAll(req.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.streams[name]; !exists {
		http.Error(w, "not registered", http.StatusNotFound)
		return
	}
	r.streams[name] = append(r.streams[name], string(body))
	log.Printf("pushed %d bytes to stream %q (queue depth: %d)", len(body), name, len(r.streams[name]))
	w.WriteHeader(http.StatusOK)
}

func (r *Replayer) handleMetrics(w http.ResponseWriter, req *http.Request) {
	name := nameFromPath("/metrics/", req.URL.Path)
	if name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}

	r.mu.Lock()
	queue, exists := r.streams[name]
	var data string
	if exists && len(queue) > 0 {
		data = queue[0]
		r.streams[name] = queue[1:]
	}
	r.mu.Unlock()

	if !exists {
		http.Error(w, "not registered", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	w.Write([]byte(data))
}

// handleBackfill writes historical samples to Prometheus via remote write.
// The body is Prometheus text format where every line carries a timestamp:
//
//	metric_name{labels} value timestamp_ms
//
// With ?align=now, all timestamps are shifted so the latest sample lands at
// the current time, which is useful for replaying captured data.
func (r *Replayer) handleBackfill(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if r.remoteWriteURL == "" {
		http.Error(w, "remote write not configured", http.StatusServiceUnavailable)
		return
	}
	align := req.URL.Query().Get("align")
	if align != "" && align != "now" {
		http.Error(w, `invalid align: only "now" is supported`, http.StatusBadRequest)
		return
	}

	series, err := parseBackfill(req.Body)
	if err != nil {
		http.Error(w, "invalid metrics: "+err.Error(), http.StatusBadRequest)
		return
	}

	if align == "now" {
		var latest int64 = math.MinInt64
		for _, ts := range series {
			latest = max(latest, ts.Samples[len(ts.Samples)-1].Timestamp)
		}
		shift := time.Now().UnixMilli() - latest
		for _, ts := range series {
			for i := range ts.Samples {
				ts.Samples[i].Timestamp += shift
			}
		}
	}

	total := 0
	for _, ts := range series {
		total += len(ts.Samples)
	}
	if err := r.remoteWrite(series); err != nil {
		http.Error(w, "remote write failed: "+err.Error(), http.StatusBadGateway)
		return
	}

	log.Printf("backfilled %d samples across %d series", total, len(series))
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "backfilled %d samples across %d series\n", total, len(series))
}

// parseBackfill parses timestamped Prometheus text into one TimeSeries per
// series, with samples sorted by time and labels sorted by name as remote
// write requires.
func parseBackfill(body io.Reader) ([]prompb.TimeSeries, error) {
	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(body)
	if err != nil {
		return nil, err
	}

	bySeries := map[string]*prompb.TimeSeries{}
	var keys []string
	for name, mf := range families {
		for _, m := range mf.GetMetric() {
			var value float64
			switch {
			case m.Counter != nil:
				value = m.GetCounter().GetValue()
			case m.Gauge != nil:
				value = m.GetGauge().GetValue()
			case m.Untyped != nil:
				value = m.GetUntyped().GetValue()
			default:
				return nil, fmt.Errorf("%s: only counter, gauge and untyped are supported; send histogram and summary series as untyped _bucket/_sum/_count lines", name)
			}
			if m.TimestampMs == nil {
				return nil, fmt.Errorf("%s: every sample needs a timestamp", name)
			}

			labels := []prompb.Label{{Name: model.MetricNameLabel, Value: name}}
			for _, l := range m.GetLabel() {
				labels = append(labels, prompb.Label{Name: l.GetName(), Value: l.GetValue()})
			}
			sort.Slice(labels, func(i, j int) bool { return labels[i].Name < labels[j].Name })

			key := fmt.Sprint(labels)
			ts, ok := bySeries[key]
			if !ok {
				ts = &prompb.TimeSeries{Labels: labels}
				bySeries[key] = ts
				keys = append(keys, key)
			}
			ts.Samples = append(ts.Samples, prompb.Sample{Value: value, Timestamp: m.GetTimestampMs()})
		}
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("no samples found")
	}

	sort.Strings(keys)
	series := make([]prompb.TimeSeries, 0, len(keys))
	for _, key := range keys {
		ts := bySeries[key]
		sort.Slice(ts.Samples, func(i, j int) bool { return ts.Samples[i].Timestamp < ts.Samples[j].Timestamp })
		series = append(series, *ts)
	}
	return series, nil
}

func (r *Replayer) remoteWrite(timeseries []prompb.TimeSeries) error {
	req := &prompb.WriteRequest{Timeseries: timeseries}
	data, err := proto.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	compressed := snappy.Encode(nil, data)
	httpReq, err := http.NewRequest(http.MethodPost, r.remoteWriteURL, bytes.NewReader(compressed))
	if err != nil {
		return fmt.Errorf("request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/x-protobuf")
	httpReq.Header.Set("Content-Encoding", "snappy")
	httpReq.Header.Set("X-Prometheus-Remote-Write-Version", "0.1.0")
	if r.bearerToken != "" {
		httpReq.Header.Set("Authorization", "Bearer "+r.bearerToken)
	}

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("post: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

func (r *Replayer) handleRules(w http.ResponseWriter, req *http.Request) {
	name := nameFromPath("/rules/", req.URL.Path)
	if name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}

	switch req.Method {
	case http.MethodPost:
		r.handleRulesCreate(w, req, name)
	case http.MethodDelete:
		r.handleRulesDelete(w, req, name)
	default:
		http.Error(w, "POST or DELETE only", http.StatusMethodNotAllowed)
	}
}

func (r *Replayer) handleRulesCreate(w http.ResponseWriter, req *http.Request, name string) {
	if r.rulesDir == "" || r.prometheusURL == "" {
		http.Error(w, "rules not configured", http.StatusServiceUnavailable)
		return
	}

	body, err := io.ReadAll(req.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if r.stripFor {
		body = stripForFromRules(body)
	}

	// ?backfill=<duration>[&step=<duration>] also evaluates the rules over
	// history. Parse it before touching any files so bad input changes nothing.
	var backfill, step time.Duration
	if v := req.URL.Query().Get("backfill"); v != "" {
		d, err := model.ParseDuration(v)
		if err != nil || d <= 0 {
			http.Error(w, "invalid backfill duration", http.StatusBadRequest)
			return
		}
		backfill = time.Duration(d)
		step = 30 * time.Second
		if v := req.URL.Query().Get("step"); v != "" {
			d, err := model.ParseDuration(v)
			if err != nil || d <= 0 {
				http.Error(w, "invalid step", http.StatusBadRequest)
				return
			}
			step = time.Duration(d)
		}
		if r.remoteWriteURL == "" {
			http.Error(w, "rule backfill requires remote write", http.StatusServiceUnavailable)
			return
		}
	}
	// Backfilled results end where live evaluation after the reload begins.
	loadedAt := time.Now()

	path := filepath.Join(r.rulesDir, name+".yaml")
	if err := os.WriteFile(path, body, 0644); err != nil {
		http.Error(w, "write rule file: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if err := r.reloadPrometheus(); err != nil {
		os.Remove(path)
		http.Error(w, "prometheus reload failed: "+err.Error(), http.StatusBadGateway)
		return
	}

	log.Printf("created rule %q (%d bytes)", name, len(body))

	if backfill > 0 {
		samples, err := r.backfillRules(body, loadedAt.Add(-step), backfill, step)
		if err != nil {
			http.Error(w, "rules loaded, but backfill failed: "+err.Error(), http.StatusBadGateway)
			return
		}
		log.Printf("backfilled rule %q: %d samples over %s (step=%s)", name, samples, backfill, step)
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, "backfilled %d rule samples over %s\n", samples, backfill)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

func (r *Replayer) handleRulesDelete(w http.ResponseWriter, req *http.Request, name string) {
	if r.rulesDir == "" || r.prometheusURL == "" {
		http.Error(w, "rules not configured", http.StatusServiceUnavailable)
		return
	}

	path := filepath.Join(r.rulesDir, name+".yaml")
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, "remove rule file: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if err := r.reloadPrometheus(); err != nil {
		log.Printf("prometheus reload after delete failed: %v", err)
	}

	log.Printf("deleted rule %q", name)
	w.WriteHeader(http.StatusOK)
}

func (r *Replayer) handleRulesList(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	if r.rulesDir == "" {
		http.Error(w, "rules not configured", http.StatusServiceUnavailable)
		return
	}

	entries, err := os.ReadDir(r.rulesDir)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".yaml") {
			names = append(names, strings.TrimSuffix(e.Name(), ".yaml"))
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(names)
}

func stripForFromRules(data []byte) []byte {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		// not JSON, try YAML-style line removal
		var lines []string
		for _, line := range strings.Split(string(data), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "for:") {
				continue
			}
			lines = append(lines, line)
		}
		return []byte(strings.Join(lines, "\n"))
	}
	// JSON path (unlikely but handle it)
	return data
}

func (r *Replayer) reloadPrometheus() error {
	resp, err := http.Post(r.prometheusURL+"/-/reload", "", nil)
	if err != nil {
		return fmt.Errorf("reload request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}
	return nil
}
