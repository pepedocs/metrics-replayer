package main

import (
	"flag"
	"log"
	"net/http"

	replayer "github.com/pepedocs/metrics-replayer"
)

func main() {
	port := flag.String("port", "8081", "listen port")
	remoteWriteURL := flag.String("remote-write-url", "", "Prometheus remote write URL (enables /backfill)")
	prometheusURL := flag.String("prometheus-url", "", "Prometheus base URL (enables /rules)")
	rulesDir := flag.String("rules-dir", "", "directory for rule files (shared with Prometheus)")
	bearerToken := flag.String("bearer-token", "", "bearer token for remote write auth")
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
	if *bearerToken != "" {
		opts = append(opts, replayer.WithBearerToken(*bearerToken))
	}

	r := replayer.New(opts...)

	addr := ":" + *port
	log.Printf("metrics-replayer listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, r.Handler()))
}
