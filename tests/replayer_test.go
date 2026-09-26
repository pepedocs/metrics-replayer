package tests

import (
	"github.com/pepedocs/metrics-replayer/internal/replayer"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func setup(t *testing.T) (*httptest.Server, func()) {
	t.Helper()
	r := replayer.New()
	srv := httptest.NewServer(r.Handler())
	return srv, srv.Close
}

func register(t *testing.T, srv *httptest.Server, name string, wantStatus int) {
	t.Helper()
	resp, err := http.Post(srv.URL+"/register/"+name, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != wantStatus {
		t.Fatalf("register %q: got %d, want %d", name, resp.StatusCode, wantStatus)
	}
}

func push(t *testing.T, srv *httptest.Server, name, body string, wantStatus int) {
	t.Helper()
	resp, err := http.Post(srv.URL+"/push/"+name, "text/plain", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != wantStatus {
		t.Fatalf("push %q: got %d, want %d", name, resp.StatusCode, wantStatus)
	}
}

func scrape(t *testing.T, srv *httptest.Server, name string) (int, string) {
	t.Helper()
	resp, err := http.Get(srv.URL + "/metrics/" + name)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func TestRegisterAndScrapeEmpty(t *testing.T) {
	srv, cleanup := setup(t)
	defer cleanup()

	register(t, srv, "test", http.StatusCreated)

	status, body := scrape(t, srv, "test")
	if status != http.StatusOK {
		t.Fatalf("scrape got %d", status)
	}
	if body != "" {
		t.Errorf("expected empty, got %q", body)
	}
}

func TestPushThenScrape(t *testing.T) {
	srv, cleanup := setup(t)
	defer cleanup()

	register(t, srv, "backend", http.StatusCreated)

	metrics := "test_total{phase=\"ok\"} 42\n"
	push(t, srv, "backend", metrics, http.StatusOK)

	status, body := scrape(t, srv, "backend")
	if status != http.StatusOK {
		t.Fatalf("scrape got %d", status)
	}
	if body != metrics {
		t.Errorf("got %q, want %q", body, metrics)
	}
}

func TestConsumedAfterScrape(t *testing.T) {
	srv, cleanup := setup(t)
	defer cleanup()

	register(t, srv, "s1", http.StatusCreated)
	push(t, srv, "s1", "gauge 1\n", http.StatusOK)

	scrape(t, srv, "s1")

	_, body := scrape(t, srv, "s1")
	if body != "" {
		t.Errorf("expected empty after scrape, got %q", body)
	}
}

func TestMultipleStreams(t *testing.T) {
	srv, cleanup := setup(t)
	defer cleanup()

	register(t, srv, "alpha", http.StatusCreated)
	register(t, srv, "beta", http.StatusCreated)

	push(t, srv, "alpha", "alpha_metric 1\n", http.StatusOK)
	push(t, srv, "beta", "beta_metric 2\n", http.StatusOK)

	_, a := scrape(t, srv, "alpha")
	_, b := scrape(t, srv, "beta")

	if a != "alpha_metric 1\n" {
		t.Errorf("alpha: got %q", a)
	}
	if b != "beta_metric 2\n" {
		t.Errorf("beta: got %q", b)
	}
}

func TestPushQueuesFIFO(t *testing.T) {
	srv, cleanup := setup(t)
	defer cleanup()

	register(t, srv, "s1", http.StatusCreated)
	push(t, srv, "s1", "first 1\n", http.StatusOK)
	push(t, srv, "s1", "second 2\n", http.StatusOK)

	_, body1 := scrape(t, srv, "s1")
	if body1 != "first 1\n" {
		t.Errorf("first scrape: got %q, want %q", body1, "first 1\n")
	}

	_, body2 := scrape(t, srv, "s1")
	if body2 != "second 2\n" {
		t.Errorf("second scrape: got %q, want %q", body2, "second 2\n")
	}

	_, body3 := scrape(t, srv, "s1")
	if body3 != "" {
		t.Errorf("third scrape (empty queue): got %q, want %q", body3, "")
	}
}

func TestDuplicateRegister(t *testing.T) {
	srv, cleanup := setup(t)
	defer cleanup()

	register(t, srv, "dup", http.StatusCreated)
	register(t, srv, "dup", http.StatusConflict)
}

func TestPushUnregistered(t *testing.T) {
	srv, cleanup := setup(t)
	defer cleanup()

	push(t, srv, "nope", "data\n", http.StatusNotFound)
}

func TestScrapeUnregistered(t *testing.T) {
	srv, cleanup := setup(t)
	defer cleanup()

	status, _ := scrape(t, srv, "nope")
	if status != http.StatusNotFound {
		t.Errorf("got %d, want %d", status, http.StatusNotFound)
	}
}

func TestRegisterRejectsGet(t *testing.T) {
	srv, cleanup := setup(t)
	defer cleanup()

	resp, err := http.Get(srv.URL + "/register/foo")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("got %d, want %d", resp.StatusCode, http.StatusMethodNotAllowed)
	}
}

func TestPushRejectsGet(t *testing.T) {
	srv, cleanup := setup(t)
	defer cleanup()

	resp, err := http.Get(srv.URL + "/push/foo")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("got %d, want %d", resp.StatusCode, http.StatusMethodNotAllowed)
	}
}
