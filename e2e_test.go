package replayer

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestE2EFullLifecycle(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/register/backend", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register: %d", resp.StatusCode)
	}

	metrics := `# TYPE provision_total counter
provision_total{phase="succeeded"} 10
provision_total{phase="failed"} 3
`
	resp, err = http.Post(srv.URL+"/push/backend", "text/plain", strings.NewReader(metrics))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("push: %d", resp.StatusCode)
	}

	resp, err = http.Get(srv.URL + "/metrics/backend")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != metrics {
		t.Fatalf("first scrape mismatch:\ngot:  %q\nwant: %q", string(body), metrics)
	}

	resp, err = http.Get(srv.URL + "/metrics/backend")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if len(body) != 0 {
		t.Fatalf("second scrape should be empty, got %q", string(body))
	}
}

func TestE2EMultipleStreamsIsolated(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()

	streams := []struct {
		name    string
		metrics string
	}{
		{"frontend", "http_requests_total 100\n"},
		{"backend", "grpc_requests_total 200\n"},
		{"worker", "jobs_processed_total 50\n"},
	}

	for _, s := range streams {
		resp, _ := http.Post(srv.URL+"/register/"+s.name, "", nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("register %s: %d", s.name, resp.StatusCode)
		}
	}

	for _, s := range streams {
		resp, _ := http.Post(srv.URL+"/push/"+s.name, "text/plain", strings.NewReader(s.metrics))
		resp.Body.Close()
	}

	for _, s := range streams {
		resp, _ := http.Get(srv.URL + "/metrics/" + s.name)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if string(body) != s.metrics {
			t.Errorf("stream %s: got %q, want %q", s.name, string(body), s.metrics)
		}
	}

	for _, s := range streams {
		resp, _ := http.Get(srv.URL + "/metrics/" + s.name)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if len(body) != 0 {
			t.Errorf("stream %s not consumed: %q", s.name, string(body))
		}
	}
}

func TestE2EConcurrentPushAndScrape(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()

	const n = 20
	for i := range n {
		name := fmt.Sprintf("stream-%d", i)
		resp, _ := http.Post(srv.URL+"/register/"+name, "", nil)
		resp.Body.Close()
	}

	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := fmt.Sprintf("stream-%d", i)
			metrics := fmt.Sprintf("counter_%d 1\n", i)

			resp, err := http.Post(srv.URL+"/push/"+name, "text/plain", strings.NewReader(metrics))
			if err != nil {
				t.Errorf("push %s: %v", name, err)
				return
			}
			resp.Body.Close()

			resp, err = http.Get(srv.URL + "/metrics/" + name)
			if err != nil {
				t.Errorf("scrape %s: %v", name, err)
				return
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()

			got := string(body)
			if got != metrics && got != "" {
				t.Errorf("stream %s: unexpected %q", name, got)
			}
		}(i)
	}
	wg.Wait()
}

func TestE2EPushWithoutRegister(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()

	resp, _ := http.Post(srv.URL+"/push/ghost", "text/plain", strings.NewReader("data\n"))
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("push unregistered: got %d, want %d", resp.StatusCode, http.StatusNotFound)
	}

	resp, _ = http.Get(srv.URL + "/metrics/ghost")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("scrape unregistered: got %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
}

func TestE2ERepeatPushScrape(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()

	resp, _ := http.Post(srv.URL+"/register/slo", "", nil)
	resp.Body.Close()

	for i := range 5 {
		metrics := fmt.Sprintf("error_rate %.1f\n", float64(i)*0.1)

		resp, _ := http.Post(srv.URL+"/push/slo", "text/plain", strings.NewReader(metrics))
		resp.Body.Close()

		resp, _ = http.Get(srv.URL + "/metrics/slo")
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if string(body) != metrics {
			t.Errorf("iteration %d: got %q, want %q", i, string(body), metrics)
		}

		resp, _ = http.Get(srv.URL + "/metrics/slo")
		body, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		if len(body) != 0 {
			t.Errorf("iteration %d: not consumed", i)
		}
	}
}
