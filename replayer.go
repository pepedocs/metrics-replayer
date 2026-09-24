package replayer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gogo/protobuf/proto"
	"github.com/golang/snappy"
	"github.com/prometheus/prometheus/prompb"
)

type Replayer struct {
	mu             sync.RWMutex
	streams        map[string]string
	mux            *http.ServeMux
	remoteWriteURL string
	prometheusURL  string
	rulesDir       string
	bearerToken    string
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

func New(opts ...Option) *Replayer {
	r := &Replayer{
		streams: make(map[string]string),
		mux:     http.NewServeMux(),
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

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.streams[name]; exists {
		http.Error(w, "already registered", http.StatusConflict)
		return
	}
	r.streams[name] = ""
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
	r.streams[name] = string(body)
	log.Printf("pushed %d bytes to stream %q", len(body), name)
	w.WriteHeader(http.StatusOK)
}

func (r *Replayer) handleMetrics(w http.ResponseWriter, req *http.Request) {
	name := nameFromPath("/metrics/", req.URL.Path)
	if name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}

	r.mu.Lock()
	data, exists := r.streams[name]
	if exists {
		r.streams[name] = ""
	}
	r.mu.Unlock()

	if !exists {
		http.Error(w, "not registered", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	w.Write([]byte(data))
}

type BackfillRequest struct {
	Metrics  string `json:"metrics"`
	Duration string `json:"duration"`
	Step     string `json:"step"`
}

func (r *Replayer) handleBackfill(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if r.remoteWriteURL == "" {
		http.Error(w, "remote write not configured", http.StatusServiceUnavailable)
		return
	}

	var bfreq BackfillRequest
	if err := json.NewDecoder(req.Body).Decode(&bfreq); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	duration, err := time.ParseDuration(bfreq.Duration)
	if err != nil {
		http.Error(w, "invalid duration: "+err.Error(), http.StatusBadRequest)
		return
	}
	step, err := time.ParseDuration(bfreq.Step)
	if err != nil {
		http.Error(w, "invalid step: "+err.Error(), http.StatusBadRequest)
		return
	}

	samples, err := parseMetricsText(bfreq.Metrics)
	if err != nil {
		http.Error(w, "invalid metrics: "+err.Error(), http.StatusBadRequest)
		return
	}

	now := time.Now()
	start := now.Add(-duration)
	var batch []prompb.TimeSeries

	for t := start; t.Before(now); t = t.Add(step) {
		ts := t.UnixMilli()
		for _, s := range samples {
			batch = append(batch, prompb.TimeSeries{
				Labels:  s.labels,
				Samples: []prompb.Sample{{Value: s.value, Timestamp: ts}},
			})
		}
	}

	if err := r.remoteWrite(batch); err != nil {
		http.Error(w, "remote write failed: "+err.Error(), http.StatusBadGateway)
		return
	}

	steps := int(duration / step)
	log.Printf("backfilled %d steps (%s, step=%s) with %d metrics", steps, duration, step, len(samples))
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "backfilled %d steps, %d total samples\n", steps, len(batch))
}

type parsedSample struct {
	labels []prompb.Label
	value  float64
}

func parseMetricsText(text string) ([]parsedSample, error) {
	var samples []parsedSample
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		var nameLabels string
		var value float64
		if _, err := fmt.Sscanf(line, "%s %f", &nameLabels, &value); err != nil {
			return nil, fmt.Errorf("parse %q: %w", line, err)
		}

		labels := []prompb.Label{}
		name := nameLabels
		if idx := strings.Index(nameLabels, "{"); idx >= 0 {
			name = nameLabels[:idx]
			labelStr := nameLabels[idx+1 : len(nameLabels)-1]
			for _, pair := range strings.Split(labelStr, ",") {
				kv := strings.SplitN(pair, "=", 2)
				if len(kv) == 2 {
					labels = append(labels, prompb.Label{
						Name:  kv[0],
						Value: strings.Trim(kv[1], `"`),
					})
				}
			}
		}
		labels = append([]prompb.Label{{Name: "__name__", Value: name}}, labels...)
		samples = append(samples, parsedSample{labels: labels, value: value})
	}
	if len(samples) == 0 {
		return nil, fmt.Errorf("no metric lines found")
	}
	return samples, nil
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
