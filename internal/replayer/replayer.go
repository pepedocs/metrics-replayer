package replayer

import (
	"net/http"
	"strings"
	"sync"
	"time"
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
