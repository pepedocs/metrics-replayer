package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func replayerURL() string {
	if u := os.Getenv("REPLAYER_URL"); u != "" {
		return u
	}
	return "http://localhost:8081"
}

func prometheusURL() string {
	if u := os.Getenv("PROMETHEUS_URL"); u != "" {
		return u
	}
	return "http://localhost:9091"
}

func TestDockerE2E(t *testing.T) {
	replayer := replayerURL()
	prom := prometheusURL()

	t.Log("registering stream 'default'")
	resp, err := http.Post(replayer+"/register/default", "", nil)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusConflict {
		t.Fatalf("register: %d", resp.StatusCode)
	}

	metrics := `# HELP test_counter_total A test counter.
# TYPE test_counter_total counter
test_counter_total{env="e2e"} 42
`
	t.Log("pushing metrics")
	resp, err = http.Post(replayer+"/push/default", "text/plain", strings.NewReader(metrics))
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("push: %d", resp.StatusCode)
	}

	t.Log("waiting for Prometheus to scrape and ingest")
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		query := fmt.Sprintf(`%s/api/v1/query?query=test_counter_total{env="e2e"}`, prom)
		resp, err = http.Get(query)
		if err != nil {
			time.Sleep(time.Second)
			continue
		}

		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		var result struct {
			Status string `json:"status"`
			Data   struct {
				Result []struct {
					Value []any `json:"value"`
				} `json:"result"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &result); err != nil {
			time.Sleep(time.Second)
			continue
		}

		if result.Status == "success" && len(result.Data.Result) > 0 {
			val, ok := result.Data.Result[0].Value[1].(string)
			if !ok {
				t.Fatalf("unexpected value type: %T", result.Data.Result[0].Value[1])
			}
			t.Logf("Prometheus returned test_counter_total{env=\"e2e\"} = %s", val)
			if val != "42" {
				t.Errorf("got %s, want 42", val)
			}
			return
		}

		time.Sleep(time.Second)
	}
	t.Fatal("timed out waiting for metric in Prometheus")
}
