package tests

import (
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/gogo/protobuf/proto"
	"github.com/golang/snappy"
	replayer "github.com/pepedocs/metrics-replayer"
	"github.com/prometheus/prometheus/prompb"
)

// fakeRuleProm serves reloads, range queries and remote writes. A query for
// "active_expr" returns a value only at steps 2..7 of the requested range;
// any other query returns the step index at every step.
type fakeRuleProm struct {
	mu      sync.Mutex
	written []prompb.TimeSeries
	queries int
}

func (f *fakeRuleProm) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	switch req.URL.Path {
	case "/-/reload":
	case "/api/v1/query_range":
		req.ParseForm()
		start, _ := strconv.ParseFloat(req.Form.Get("start"), 64)
		end, _ := strconv.ParseFloat(req.Form.Get("end"), 64)
		step, _ := strconv.ParseFloat(req.Form.Get("step"), 64)
		f.mu.Lock()
		f.queries++
		f.mu.Unlock()
		var values []string
		for i, t := 0, start; t <= end+1e-9; i, t = i+1, t+step {
			if req.Form.Get("query") == "active_expr" && (i < 2 || i > 7) {
				continue
			}
			values = append(values, fmt.Sprintf(`[%.3f,"%d"]`, t, i))
		}
		fmt.Fprintf(w, `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"job":"x"},"values":[%s]}]}}`, strings.Join(values, ","))
	case "/api/v1/write":
		compressed, _ := io.ReadAll(req.Body)
		data, _ := snappy.Decode(nil, compressed)
		var wr prompb.WriteRequest
		proto.Unmarshal(data, &wr)
		f.mu.Lock()
		f.written = append(f.written, wr.Timeseries...)
		f.mu.Unlock()
	}
}

func (f *fakeRuleProm) series(match string) []prompb.TimeSeries {
	var out []prompb.TimeSeries
	for _, ts := range f.written {
		if strings.Contains(labelString(ts.Labels), match) {
			out = append(out, ts)
		}
	}
	return out
}

func setupRuleBackfill(t *testing.T) (*httptest.Server, *fakeRuleProm) {
	t.Helper()
	fake := &fakeRuleProm{}
	prom := httptest.NewServer(fake)
	t.Cleanup(prom.Close)
	r := replayer.New(
		replayer.WithPrometheusURL(prom.URL),
		replayer.WithRemoteWriteURL(prom.URL+"/api/v1/write"),
		replayer.WithRulesDir(t.TempDir()),
	)
	srv := httptest.NewServer(r.Handler())
	t.Cleanup(srv.Close)
	return srv, fake
}

const backfillRules = `groups:
  - name: g
    rules:
      - record: job:ratio
        expr: some_expr
        labels:
          team: a
      - alert: TooHigh
        expr: active_expr
        for: 1m
        labels:
          severity: warning
`

func postRules(t *testing.T, srv *httptest.Server, query string, wantStatus int) string {
	t.Helper()
	resp, err := http.Post(srv.URL+"/rules/r"+query, "application/yaml", strings.NewReader(backfillRules))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != wantStatus {
		t.Fatalf("got %d (%s), want %d", resp.StatusCode, body, wantStatus)
	}
	return string(body)
}

func TestRuleBackfillRecordingRule(t *testing.T) {
	srv, fake := setupRuleBackfill(t)
	postRules(t, srv, "?backfill=5m&step=30s", http.StatusCreated)

	rec := fake.series("__name__=job:ratio")
	if len(rec) != 1 {
		t.Fatalf("got %d recorded series, want 1", len(rec))
	}
	if got := labelString(rec[0].Labels); got != "__name__=job:ratio,job=x,team=a" {
		t.Errorf("labels: %s", got)
	}
	// 5m at 30s = 11 steps, plus a staleness marker after the last one.
	s := rec[0].Samples
	if len(s) != 12 || !math.IsNaN(s[11].Value) {
		t.Errorf("got %d samples (last %v), want 11 values + stale marker", len(s), s[len(s)-1].Value)
	}
}

func TestRuleBackfillAlertStates(t *testing.T) {
	srv, fake := setupRuleBackfill(t)
	postRules(t, srv, "?backfill=5m&step=30s", http.StatusCreated)

	count := func(state string) (values, stale int) {
		for _, ts := range fake.series("alertstate=" + state) {
			if !strings.Contains(labelString(ts.Labels), "__name__=ALERTS,alertname=TooHigh,alertstate="+state+",job=x,severity=warning") {
				t.Errorf("labels: %s", labelString(ts.Labels))
			}
			for _, s := range ts.Samples {
				if math.IsNaN(s.Value) {
					stale++
				} else {
					values++
				}
			}
		}
		return
	}
	// Active at steps 2..7 with for: 1m: pending at 2,3 (0s, 30s held), firing at 4..7.
	if v, s := count("pending"); v != 2 || s != 1 {
		t.Errorf("pending: %d values, %d stale; want 2, 1", v, s)
	}
	if v, s := count("firing"); v != 4 || s != 1 {
		t.Errorf("firing: %d values, %d stale; want 4, 1", v, s)
	}
}

func TestRuleBackfillChunksLongRanges(t *testing.T) {
	srv, fake := setupRuleBackfill(t)
	// 7d at 30s = 20161 steps: needs 3 range queries per rule.
	postRules(t, srv, "?backfill=7d", http.StatusCreated)
	if fake.queries != 6 {
		t.Errorf("got %d range queries, want 6", fake.queries)
	}
	if n := len(fake.series("__name__=job:ratio")[0].Samples); n != 20162 {
		t.Errorf("got %d samples, want 20161 + stale marker", n)
	}
}

func TestRuleBackfillInvalidParams(t *testing.T) {
	srv, _ := setupRuleBackfill(t)
	postRules(t, srv, "?backfill=nope", http.StatusBadRequest)
	postRules(t, srv, "?backfill=1h&step=0s", http.StatusBadRequest)
}

func TestRulesWithoutBackfillUnchanged(t *testing.T) {
	srv, fake := setupRuleBackfill(t)
	postRules(t, srv, "", http.StatusCreated)
	if fake.queries != 0 || len(fake.written) != 0 {
		t.Errorf("plain create should not query or write: %d queries, %d series", fake.queries, len(fake.written))
	}
}
