package tests

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gogo/protobuf/proto"
	"github.com/golang/snappy"
	"github.com/pepedocs/metrics-replayer/internal/replayer"
	"github.com/prometheus/prometheus/prompb"
)

// setupBackfill returns a replayer whose remote writes are decoded into *got.
func setupBackfill(t *testing.T) (*httptest.Server, *[]prompb.TimeSeries) {
	t.Helper()
	var got []prompb.TimeSeries
	rw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		compressed, _ := io.ReadAll(req.Body)
		data, err := snappy.Decode(nil, compressed)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var wr prompb.WriteRequest
		if err := proto.Unmarshal(data, &wr); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		got = append(got, wr.Timeseries...)
	}))
	t.Cleanup(rw.Close)
	srv := httptest.NewServer(replayer.New(replayer.WithRemoteWriteURL(rw.URL)).Handler())
	t.Cleanup(srv.Close)
	return srv, &got
}

func backfill(t *testing.T, srv *httptest.Server, query, body string, wantStatus int) string {
	t.Helper()
	resp, err := http.Post(srv.URL+"/backfill"+query, "text/plain", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != wantStatus {
		t.Fatalf("backfill: got %d (%s), want %d", resp.StatusCode, respBody, wantStatus)
	}
	return string(respBody)
}

func labelString(labels []prompb.Label) string {
	var parts []string
	for _, l := range labels {
		parts = append(parts, l.Name+"="+l.Value)
	}
	return strings.Join(parts, ",")
}

func TestBackfillGroupsSeries(t *testing.T) {
	srv, got := setupBackfill(t)

	// Interleaved series and out-of-order lines, as a producer might emit them.
	body := backfill(t, srv, "", `# TYPE req_total counter
req_total{path="/a b,c",code="500"} 5 2000
req_total{path="/a b,c",code="500"} 3 1000
temp 20.5 1000
req_total{path="/a b,c",code="500"} 9 3000
temp 21 2000
`, http.StatusOK)

	if !strings.Contains(body, "5 samples across 2 series") {
		t.Errorf("response: %q", body)
	}
	if len(*got) != 2 {
		t.Fatalf("got %d timeseries, want 2", len(*got))
	}
	req := (*got)[0]
	// Labels sorted by name; label values may contain spaces and commas.
	if s := labelString(req.Labels); s != "__name__=req_total,code=500,path=/a b,c" {
		t.Errorf("labels: %s", s)
	}
	want := []prompb.Sample{{Value: 3, Timestamp: 1000}, {Value: 5, Timestamp: 2000}, {Value: 9, Timestamp: 3000}}
	for i, s := range req.Samples {
		if s.Value != want[i].Value || s.Timestamp != want[i].Timestamp {
			t.Errorf("sample %d: got %+v, want %+v", i, s, want[i])
		}
	}
}

func TestBackfillAlignNow(t *testing.T) {
	srv, got := setupBackfill(t)

	before := time.Now().UnixMilli()
	backfill(t, srv, "?align=now", "a 1 0\na 2 30000\nb 5 60000\n", http.StatusOK)
	after := time.Now().UnixMilli()

	var a, b prompb.TimeSeries
	for _, ts := range *got {
		switch ts.Labels[0].Value {
		case "a":
			a = ts
		case "b":
			b = ts
		}
	}
	// The latest sample overall lands at now; relative spacing is kept.
	if ts := b.Samples[0].Timestamp; ts < before || ts > after {
		t.Errorf("latest sample at %d, want within [%d, %d]", ts, before, after)
	}
	if d := b.Samples[0].Timestamp - a.Samples[0].Timestamp; d != 60000 {
		t.Errorf("spacing: got %d, want 60000", d)
	}
}

func TestBackfillErrors(t *testing.T) {
	srv, _ := setupBackfill(t)

	for name, tc := range map[string]struct{ query, body string }{
		"missing timestamp": {"", "a 1\n"},
		"empty":             {"", "# nothing here\n"},
		"bad line":          {"", "a{b=} 1 1000\n"},
		"histogram":         {"", "# TYPE h histogram\nh_bucket{le=\"+Inf\"} 1 1000\nh_count 1 1000\nh_sum 1 1000\n"},
		"bad align":         {"?align=later", "a 1 1000\n"},
	} {
		t.Run(name, func(t *testing.T) {
			backfill(t, srv, tc.query, tc.body, http.StatusBadRequest)
		})
	}
}
