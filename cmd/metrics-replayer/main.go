package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"path/filepath"

	replayer "github.com/pepedocs/metrics-replayer"
)

func main() {
	port := flag.String("port", "8081", "listen port")
	remoteWriteURL := flag.String("remote-write-url", "", "Prometheus remote write URL (enables /backfill)")
	prometheusURL := flag.String("prometheus-url", "", "Prometheus base URL (enables /rules)")
	rulesDir := flag.String("rules-dir", "", "directory for rule files (shared with Prometheus)")
	bearerToken := flag.String("bearer-token", "", "bearer token for remote write auth")
	scrapesDir := flag.String("scrapes-dir", "", "directory for per-stream scrape config files (shared with Prometheus)")
	scrapeTarget := flag.String("scrape-target", "", "host:port Prometheus uses to reach the replayer (default: localhost:<port>)")
	stripFor := flag.Bool("strip-for", false, "remove 'for' durations from alert rules (alerts fire immediately)")
	flag.Parse()

	var opts []replayer.Option
	if *remoteWriteURL != "" {
		opts = append(opts, replayer.WithRemoteWriteURL(*remoteWriteURL))
		log.Printf("remote write enabled: %s", *remoteWriteURL)
	}
	if *prometheusURL != "" && *rulesDir != "" {
		opts = append(opts, replayer.WithPrometheusURL(*prometheusURL))
		opts = append(opts, replayer.WithRulesDir(*rulesDir))
		log.Printf("rules enabled: dir=%s prometheus=%s", *rulesDir, *prometheusURL)
	}
	if *scrapesDir != "" {
		if *prometheusURL == "" {
			log.Fatal("--scrapes-dir requires --prometheus-url")
		}
		target := *scrapeTarget
		if target == "" {
			target = "localhost:" + *port
		}
		// Streams live in memory, so config files from a previous run point
		// at streams that no longer exist.
		stale, _ := filepath.Glob(filepath.Join(*scrapesDir, "*.yaml"))
		for _, f := range stale {
			os.Remove(f)
		}
		opts = append(opts, replayer.WithPrometheusURL(*prometheusURL))
		opts = append(opts, replayer.WithScrapesDir(*scrapesDir))
		opts = append(opts, replayer.WithScrapeTarget(target))
		log.Printf("scrape configs enabled: dir=%s target=%s", *scrapesDir, target)
	}
	if *bearerToken != "" {
		opts = append(opts, replayer.WithBearerToken(*bearerToken))
	}
	if *stripFor {
		opts = append(opts, replayer.WithStripFor(true))
		log.Println("strip-for enabled: alert 'for' durations will be removed")
	}

	r := replayer.New(opts...)

	addr := ":" + *port
	log.Printf("metrics-replayer listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, r.Handler()))
}
