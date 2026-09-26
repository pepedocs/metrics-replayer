package tests

import (
	"context"
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
