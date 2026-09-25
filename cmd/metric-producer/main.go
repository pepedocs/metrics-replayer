package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"strings"
	"time"
)

func main() {
	replayerURL := flag.String("replayer", "http://metrics-replayer:8081", "replayer base URL")
	stream := flag.String("stream", "default", "stream name")
	interval := flag.Duration("interval", 2*time.Second, "push interval")
	backfill := flag.Duration("backfill", 0, "backfill duration via replayer API (e.g. 168h)")
	backfillStep := flag.Duration("backfill-step", 30*time.Second, "backfill step interval")
	flag.Parse()

	c := coffees{brewed: 100, spilled: 12, stolen: 5}
	if *backfill > 0 {
		c = runBackfill(*replayerURL, *backfill, *backfillStep)
	}

	submitRules(*replayerURL)
	runScrapeMode(*replayerURL, *stream, *interval, c)
}

// coffees holds the office_coffees_total counters by outcome.
type coffees struct{ brewed, spilled, stolen float64 }

// tick advances the counters by one push interval's worth of random events.
func (c *coffees) tick() {
	c.brewed += float64(rand.IntN(6) + 1)
	if rand.IntN(4) == 0 {
		c.spilled++
	}
	if rand.IntN(8) == 0 {
		c.stolen++
	}
}

// runBackfill sends a history of increasing counters ending now and returns
// the final counts, so live pushes continue the same series without a reset.
func runBackfill(replayerURL string, duration, step time.Duration) coffees {
	c := coffees{brewed: 100, spilled: 12, stolen: 5}
	var body strings.Builder
	end := time.Now()
	for t := end.Add(-duration); !t.After(end); t = t.Add(step) {
		c.tick()
		ms := t.UnixMilli()
		fmt.Fprintf(&body, "office_coffees_total{outcome=\"brewed\"} %.0f %d\n", c.brewed, ms)
		fmt.Fprintf(&body, "office_coffees_total{outcome=\"spilled\"} %.0f %d\n", c.spilled, ms)
		fmt.Fprintf(&body, "office_coffees_total{outcome=\"stolen_from_fridge\"} %.0f %d\n", c.stolen, ms)
	}

	log.Printf("backfilling %s of data (step=%s) via replayer", duration, step)

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Post(replayerURL+"/backfill", "text/plain", strings.NewReader(body.String()))
		if err != nil {
			log.Printf("backfill not ready, retrying: %v", err)
			time.Sleep(2 * time.Second)
			continue
		}
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode == http.StatusOK {
			log.Printf("backfill complete: %s", strings.TrimSpace(string(respBody)))
			return c
		}
		log.Printf("backfill returned %d: %s, retrying", resp.StatusCode, strings.TrimSpace(string(respBody)))
		time.Sleep(2 * time.Second)
	}
	log.Printf("backfill timed out, continuing with live push")
	return c
}

func submitRules(replayerURL string) {
	recordingRules := `groups:
  - name: coffee_recording_rules
    rules:
      - record: office_coffees:brewed:rate5m
        expr: rate(office_coffees_total{outcome="brewed"}[5m])
      - record: office_coffees:spilled:rate5m
        expr: rate(office_coffees_total{outcome="spilled"}[5m])
      - record: office_coffees:spill_ratio:5m
        expr: |
          rate(office_coffees_total{outcome="spilled"}[5m])
          / on() rate(office_coffees_total{outcome="brewed"}[5m])
`
	alertRules := `groups:
  - name: coffee_alerts
    rules:
      - alert: CoffeeSpillRateHigh
        expr: office_coffees:spill_ratio:5m > 0.3
        for: 2m
        labels:
          severity: warning
        annotations:
          summary: "Coffee spill rate is above 30%"
      - alert: CoffeeTheftDetected
        expr: rate(office_coffees_total{outcome="stolen_from_fridge"}[10m]) > 0
        for: 1m
        labels:
          severity: critical
        annotations:
          summary: "Someone is stealing coffee from the fridge"
`
	rules := map[string]string{
		"coffee-recording": recordingRules,
		"coffee-alerts":    alertRules,
	}

	for name, body := range rules {
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			resp, err := http.Post(replayerURL+"/rules/"+name, "application/yaml", strings.NewReader(body))
			if err != nil {
				log.Printf("rules %s not ready, retrying: %v", name, err)
				time.Sleep(2 * time.Second)
				continue
			}
			resp.Body.Close()
			if resp.StatusCode == http.StatusCreated {
				log.Printf("submitted rule %q", name)
				break
			}
			log.Printf("rules %s returned %d, retrying", name, resp.StatusCode)
			time.Sleep(2 * time.Second)
		}
	}
}

func runScrapeMode(replayerURL, stream string, interval time.Duration, c coffees) {
	url := replayerURL + "/register/" + stream
	resp, err := http.Post(url, "", nil)
	if err != nil {
		log.Fatalf("register: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusConflict {
		log.Fatalf("register: HTTP %d", resp.StatusCode)
	}
	log.Printf("registered stream %q", stream)

	for {
		c.tick()
		brewed, spilled, stolen := c.brewed, c.spilled, c.stolen

		metrics := fmt.Sprintf(`# HELP office_coffees_total Total office coffee events.
# TYPE office_coffees_total counter
office_coffees_total{outcome="brewed"} %.0f
office_coffees_total{outcome="spilled"} %.0f
office_coffees_total{outcome="stolen_from_fridge"} %.0f
`, brewed, spilled, stolen)

		url := replayerURL + "/push/" + stream
		resp, err := http.Post(url, "text/plain", strings.NewReader(metrics))
		if err != nil {
			log.Printf("push error: %v", err)
		} else {
			resp.Body.Close()
			log.Printf("pushed: brewed=%.0f spilled=%.0f stolen=%.0f", brewed, spilled, stolen)
		}

		time.Sleep(interval)
	}
}
