package tests

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	emitOnce sync.Once
	emitBin  string
	emitErr  error
)

// buildEmit compiles cmd/emit once per test run.
func buildEmit(t *testing.T) string {
	t.Helper()
	emitOnce.Do(func() {
		dir, err := os.MkdirTemp("", "emit-test-")
		if err != nil {
			emitErr = err
			return
		}
		emitBin = filepath.Join(dir, "emit")
		out, err := exec.Command("go", "build", "-o", emitBin, "../cmd/emit").CombinedOutput()
		if err != nil {
			emitErr = err
			emitBin = string(out)
		}
	})
	if emitErr != nil {
		t.Fatalf("build emit: %v\n%s", emitErr, emitBin)
	}
	return emitBin
}

// fakeReplayer records what emit sends. Register sleeps to mimic waiting for
// Prometheus to scrape the new target.
type fakeReplayer struct {
	registerDelay time.Duration
	mu            sync.Mutex
	backfills     []string
	pushes        chan string
}

func (f *fakeReplayer) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	switch {
	case strings.HasPrefix(req.URL.Path, "/register/"):
		time.Sleep(f.registerDelay)
		w.WriteHeader(http.StatusCreated)
	case req.URL.Path == "/backfill":
		f.mu.Lock()
		f.backfills = append(f.backfills, string(body))
		f.mu.Unlock()
	case strings.HasPrefix(req.URL.Path, "/push/"):
		select {
		case f.pushes <- string(body):
		default:
		}
	}
}

func writeTemplate(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "t.tmpl")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// Regression: in --realtime mode the window clock started before registering
// the stream, so the first part of the window was skipped while register
// waited for Prometheus.
func TestEmitRealtimeStartsAtWindowStart(t *testing.T) {
	bin := buildEmit(t)
	fake := &fakeReplayer{registerDelay: 1500 * time.Millisecond, pushes: make(chan string, 1)}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin,
		"--replayer", srv.URL, "--shape", "constant",
		"--template", writeTemplate(t, "window_t {{ .T }}\n"),
		"--realtime", "--window", "10s", "--interval", "300ms")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()

	var first string
	select {
	case first = <-fake.pushes:
	case <-ctx.Done():
		t.Fatal("no live push received")
	}
	fields := strings.Fields(first)
	tval, err := strconv.ParseFloat(fields[len(fields)-1], 64)
	if err != nil {
		t.Fatalf("parse %q: %v", first, err)
	}
	// The first push is one interval (0.3s) into the 10s window, so T ≈ -9.7.
	// Before the fix, the 1.5s register delay was skipped too: T ≈ -8.2.
	if tval > -9.4 {
		t.Errorf("first push at T=%v, want about -9.7: the start of the window was skipped", tval)
	}
}

func TestEmitBackfillFormat(t *testing.T) {
	bin := buildEmit(t)
	fake := &fakeReplayer{pushes: make(chan string, 1)}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	tmpl := writeTemplate(t, `# TYPE reqs_total counter
reqs_total {{ counter "reqs" 2 }}
level {{ .Y }}
`)
	out, err := exec.Command(bin,
		"--replayer", srv.URL, "--shape", "constant", "--params", "baseline=0.3",
		"--template", tmpl, "--window", "2m", "--step", "30s", "--live=false").CombinedOutput()
	if err != nil {
		t.Fatalf("emit: %v\n%s", err, out)
	}
	if len(fake.backfills) != 1 {
		t.Fatalf("got %d backfill requests, want 1", len(fake.backfills))
	}

	lines := strings.Split(strings.TrimSpace(fake.backfills[0]), "\n")
	// 2m at 30s: 5 ticks, 2 sample lines each. Comment lines are dropped,
	// because repeating # TYPE for every tick is invalid.
	if len(lines) != 10 {
		t.Fatalf("got %d lines, want 10:\n%s", len(lines), fake.backfills[0])
	}
	var prevTS, prevCount float64
	for i, line := range lines {
		f := strings.Fields(line)
		if strings.HasPrefix(line, "#") || len(f) != 3 {
			t.Fatalf("line %d: want `name value timestamp`, got %q", i, line)
		}
		ts, _ := strconv.ParseFloat(f[2], 64)
		if f[0] == "reqs_total" {
			// 2/s over 30s steps: +60 per tick, never resetting.
			v, _ := strconv.ParseFloat(f[1], 64)
			if v != prevCount+60 {
				t.Errorf("tick %d: counter %v, want %v", i/2, v, prevCount+60)
			}
			if prevTS != 0 && ts-prevTS != 30000 {
				t.Errorf("tick %d: spacing %vms, want 30000", i/2, ts-prevTS)
			}
			prevCount, prevTS = v, ts
		} else if f[1] != "0.3" {
			// .Y prints without float noise.
			t.Errorf("level = %q, want 0.3", f[1])
		}
	}
}

func TestEmitLogsRandomSeedForNoise(t *testing.T) {
	bin := buildEmit(t)
	out, err := exec.Command(bin, "--shape", "constant", "--params", "noise=0.1",
		"--template", writeTemplate(t, "x {{ .Y }}\n"), "--window", "1m", "--dry-run").CombinedOutput()
	if err != nil {
		t.Fatalf("emit: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "seed=") {
		t.Errorf("noise without a seed should log the chosen seed:\n%s", out)
	}
}

// fakeProm serves query_range for ALERTS with two firing runs relative to the
// query's end: 50m..40m before it, and 20m..15m before it.
func fakeProm(t *testing.T, gotQuery *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		req.ParseForm()
		*gotQuery = req.Form.Get("query")
		end, _ := strconv.ParseFloat(req.Form.Get("end"), 64)
		var vals []string
		for _, r := range [][2]float64{{50, 40}, {20, 15}} {
			for m := r[0]; m >= r[1]; m -= 0.5 {
				vals = append(vals, fmt.Sprintf(`[%f,"1"]`, end-m*60))
			}
		}
		fmt.Fprintf(w, `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{},"values":[%s]}]}}`, strings.Join(vals, ","))
	}))
}

func TestEmitScenarioReport(t *testing.T) {
	bin := buildEmit(t)
	rep := &fakeReplayer{pushes: make(chan string, 1)}
	rsrv := httptest.NewServer(rep)
	defer rsrv.Close()
	var query string
	psrv := fakeProm(t, &query)
	defer psrv.Close()

	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "t.tmpl"), []byte(`x{name="{{ .Name }}"} {{ .Y }}`+"\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "scenario.yaml"), []byte(`window: 2h
step: 30s
template: t.tmpl
rules_backfill: 2h
instances:
  - name: inc-a
    shape: constant
    alert: 'MyAlert{severity="page",name="{{ .Name }}"}'
    incident: {start: -1h, end: -30m}
`), 0o644)

	out, err := exec.Command(bin, "--replayer", rsrv.URL, "--prometheus", psrv.URL,
		"--scenario", filepath.Join(dir, "scenario.yaml"), "--report", "--markdown").CombinedOutput()
	if err != nil {
		t.Fatalf("emit: %v\n%s", err, out)
	}
	if len(rep.backfills) != 1 || !strings.Contains(rep.backfills[0], `x{name="inc-a"}`) {
		t.Errorf("backfill should render .Name: %.80q", rep.backfills)
	}
	if want := `ALERTS{alertname="MyAlert",alertstate="firing",severity="page",name="inc-a"}`; query != want {
		t.Errorf("query = %s, want %s", query, want)
	}
	// Incident from -60m to -30m; firing runs start at -50m and -20m.
	if !strings.Contains(string(out), "| inc-a | 30m | yes | 10m | 2 | 10m after it ended |") {
		t.Errorf("unexpected report:\n%s", out)
	}

	// --report-only reuses the recorded run without replaying.
	rep.backfills = nil
	out, err = exec.Command(bin, "--prometheus", psrv.URL,
		"--scenario", filepath.Join(dir, "scenario.yaml"), "--report-only").CombinedOutput()
	if err != nil || len(rep.backfills) != 0 || !strings.Contains(string(out), "inc-a") {
		t.Errorf("report-only: err=%v backfills=%d\n%s", err, len(rep.backfills), out)
	}
}
