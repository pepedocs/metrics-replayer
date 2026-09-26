package replayer

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// validStreamName keeps stream names safe to use as file names and job names.
var validStreamName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

// addScrapeConfig writes a scrape config for the stream, reloads Prometheus and
// waits until Prometheus reports the new target as up. On failure the file is
// removed again.
func (r *Replayer) addScrapeConfig(name string) error {
	if r.prometheusURL == "" {
		return fmt.Errorf("prometheus URL not configured")
	}
	config := fmt.Sprintf(`scrape_configs:
  - job_name: %q
    metrics_path: %q
    static_configs:
      - targets: [%q]
`, name, "/metrics/"+name, r.scrapeTarget)

	path := filepath.Join(r.scrapesDir, name+".yaml")
	if err := os.WriteFile(path, []byte(config), 0644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	reloaded := time.Now()
	err := r.reloadPrometheus()
	if err == nil {
		err = r.waitForTarget(name, reloaded)
	}
	if err != nil {
		os.Remove(path)
		r.reloadPrometheus()
		return err
	}
	return nil
}

// waitForTarget polls the Prometheus targets API until the stream's job has
// been scraped successfully, so a wrong --scrape-target fails loudly. Scrapes
// from before the reload are ignored: a target left over from a previous run
// can still report a stale "down".
func (r *Replayer) waitForTarget(job string, since time.Time) error {
	deadline := time.Now().Add(r.scrapeTimeout)
	lastState := "not found"
	for {
		health, scrapeURL, lastError, lastScrape, err := r.targetHealth(job)
		switch {
		case err != nil:
			lastState = err.Error()
		case lastScrape.Before(since):
			lastState = "waiting for first scrape"
		case health == "up":
			return nil
		case health == "down":
			return fmt.Errorf("prometheus cannot scrape %s: %s (check --scrape-target)", scrapeURL, lastError)
		case health != "":
			lastState = health
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("target %q not up after %s (last state: %s)", job, r.scrapeTimeout, lastState)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func (r *Replayer) targetHealth(job string) (health, scrapeURL, lastError string, lastScrape time.Time, err error) {
	resp, err := http.Get(r.prometheusURL + "/api/v1/targets?state=active")
	if err != nil {
		return "", "", "", time.Time{}, err
	}
	defer resp.Body.Close()

	var result struct {
		Data struct {
			ActiveTargets []struct {
				ScrapePool string    `json:"scrapePool"`
				ScrapeURL  string    `json:"scrapeUrl"`
				Health     string    `json:"health"`
				LastError  string    `json:"lastError"`
				LastScrape time.Time `json:"lastScrape"`
			} `json:"activeTargets"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", "", "", time.Time{}, fmt.Errorf("decode targets: %w", err)
	}
	for _, t := range result.Data.ActiveTargets {
		if t.ScrapePool == job {
			return t.Health, t.ScrapeURL, t.LastError, t.LastScrape, nil
		}
	}
	return "", "", "", time.Time{}, nil
}
