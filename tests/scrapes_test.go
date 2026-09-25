package tests

import (
	"fmt"
	replayer "github.com/pepedocs/metrics-replayer"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func setupScrapes(t *testing.T, health string) (*httptest.Server, string) {
	return setupScrapesAt(t, health, time.Now)
}

// setupScrapesAt reports targets whose last scrape happened at lastScrape().
func setupScrapesAt(t *testing.T, health string, lastScrape func() time.Time) (*httptest.Server, string) {
	t.Helper()
	dir := t.TempDir()
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/api/v1/targets" {
			return
		}
		// Report a target for every scrape config file currently on disk.
		files, _ := filepath.Glob(filepath.Join(dir, "*.yaml"))
		var targets []string
		for _, f := range files {
			job := strings.TrimSuffix(filepath.Base(f), ".yaml")
			targets = append(targets, fmt.Sprintf(`{"scrapePool":%q,"scrapeUrl":"http://x/metrics/%s","health":%q,"lastError":"connection refused","lastScrape":%q}`, job, job, health, lastScrape().Format(time.RFC3339Nano)))
		}
		fmt.Fprintf(w, `{"data":{"activeTargets":[%s]}}`, strings.Join(targets, ","))
	}))
	t.Cleanup(prom.Close)

	r := replayer.New(
		replayer.WithPrometheusURL(prom.URL),
		replayer.WithScrapesDir(dir),
		replayer.WithScrapeTarget("replayer:8081"),
		replayer.WithScrapeTimeout(2*time.Second),
	)
	srv := httptest.NewServer(r.Handler())
	t.Cleanup(srv.Close)
	return srv, dir
}

func TestRegisterWritesScrapeConfig(t *testing.T) {
	srv, dir := setupScrapes(t, "up")

	register(t, srv, "api", http.StatusCreated)

	data, err := os.ReadFile(filepath.Join(dir, "api.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	want := `scrape_configs:
  - job_name: "api"
    metrics_path: "/metrics/api"
    static_configs:
      - targets: ["replayer:8081"]
`
	if string(data) != want {
		t.Errorf("got:\n%s\nwant:\n%s", data, want)
	}
}

func TestRegisterFailsWhenTargetDown(t *testing.T) {
	srv, dir := setupScrapes(t, "down")

	resp, err := http.Post(srv.URL+"/register/api", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("got %d, want 502", resp.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(dir, "api.yaml")); !os.IsNotExist(err) {
		t.Errorf("scrape config left behind: %v", err)
	}
	// The stream was rolled back, so registering again is not a conflict.
	resp, _ = http.Post(srv.URL+"/register/api", "", nil)
	resp.Body.Close()
	if resp.StatusCode == http.StatusConflict {
		t.Errorf("stream not rolled back")
	}
}

func TestRegisterFailsWhenTargetNeverAppears(t *testing.T) {
	srv, _ := setupScrapes(t, "unknown")
	register(t, srv, "api", http.StatusBadGateway)
}

func TestRegisterRejectsUnsafeName(t *testing.T) {
	srv, _ := setupScrapes(t, "up")
	register(t, srv, ".hidden", http.StatusBadRequest)
	register(t, srv, "a%2F..%2Fb", http.StatusBadRequest)
}

func TestRegisterIgnoresStaleTargetHealth(t *testing.T) {
	// A target from before the reload reports "down"; it must not fail register.
	stale := time.Now().Add(-time.Minute)
	srv, _ := setupScrapesAt(t, "down", func() time.Time { return stale })

	start := time.Now()
	register(t, srv, "api", http.StatusBadGateway)
	if time.Since(start) < time.Second {
		t.Errorf("register failed immediately on stale health instead of waiting")
	}
}
