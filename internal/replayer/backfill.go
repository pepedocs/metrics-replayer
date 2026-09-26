package replayer

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"sort"
	"time"

	"github.com/gogo/protobuf/proto"
	"github.com/golang/snappy"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/prompb"
)

// handleBackfill writes historical samples to Prometheus via remote write.
// The body is Prometheus text format where every line carries a timestamp:
//
//	metric_name{labels} value timestamp_ms
//
// With ?align=now, all timestamps are shifted so the latest sample lands at
// the current time, which is useful for replaying captured data.
func (r *Replayer) handleBackfill(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if r.remoteWriteURL == "" {
		http.Error(w, "remote write not configured", http.StatusServiceUnavailable)
		return
	}
	align := req.URL.Query().Get("align")
	if align != "" && align != "now" {
		http.Error(w, `invalid align: only "now" is supported`, http.StatusBadRequest)
		return
	}

	series, err := parseBackfill(req.Body)
	if err != nil {
		http.Error(w, "invalid metrics: "+err.Error(), http.StatusBadRequest)
		return
	}

	if align == "now" {
		var latest int64 = math.MinInt64
		for _, ts := range series {
			latest = max(latest, ts.Samples[len(ts.Samples)-1].Timestamp)
		}
		shift := time.Now().UnixMilli() - latest
		for _, ts := range series {
			for i := range ts.Samples {
				ts.Samples[i].Timestamp += shift
			}
		}
	}

	total := 0
	for _, ts := range series {
		total += len(ts.Samples)
	}
	if err := r.remoteWrite(series); err != nil {
		http.Error(w, "remote write failed: "+err.Error(), http.StatusBadGateway)
		return
	}

	log.Printf("backfilled %d samples across %d series", total, len(series))
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "backfilled %d samples across %d series\n", total, len(series))
}

// parseBackfill parses timestamped Prometheus text into one TimeSeries per
// series, with samples sorted by time and labels sorted by name as remote
// write requires.
func parseBackfill(body io.Reader) ([]prompb.TimeSeries, error) {
	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(body)
	if err != nil {
		return nil, err
	}

	bySeries := map[string]*prompb.TimeSeries{}
	var keys []string
	for name, mf := range families {
		for _, m := range mf.GetMetric() {
			var value float64
			switch {
			case m.Counter != nil:
				value = m.GetCounter().GetValue()
			case m.Gauge != nil:
				value = m.GetGauge().GetValue()
			case m.Untyped != nil:
				value = m.GetUntyped().GetValue()
			default:
				return nil, fmt.Errorf("%s: only counter, gauge and untyped are supported; send histogram and summary series as untyped _bucket/_sum/_count lines", name)
			}
			if m.TimestampMs == nil {
				return nil, fmt.Errorf("%s: every sample needs a timestamp", name)
			}

			labels := []prompb.Label{{Name: model.MetricNameLabel, Value: name}}
			for _, l := range m.GetLabel() {
				labels = append(labels, prompb.Label{Name: l.GetName(), Value: l.GetValue()})
			}
			sort.Slice(labels, func(i, j int) bool { return labels[i].Name < labels[j].Name })

			key := fmt.Sprint(labels)
			ts, ok := bySeries[key]
			if !ok {
				ts = &prompb.TimeSeries{Labels: labels}
				bySeries[key] = ts
				keys = append(keys, key)
			}
			ts.Samples = append(ts.Samples, prompb.Sample{Value: value, Timestamp: m.GetTimestampMs()})
		}
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("no samples found")
	}

	sort.Strings(keys)
	series := make([]prompb.TimeSeries, 0, len(keys))
	for _, key := range keys {
		ts := bySeries[key]
		sort.Slice(ts.Samples, func(i, j int) bool { return ts.Samples[i].Timestamp < ts.Samples[j].Timestamp })
		series = append(series, *ts)
	}
	return series, nil
}

func (r *Replayer) remoteWrite(timeseries []prompb.TimeSeries) error {
	req := &prompb.WriteRequest{Timeseries: timeseries}
	data, err := proto.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	compressed := snappy.Encode(nil, data)
	httpReq, err := http.NewRequest(http.MethodPost, r.remoteWriteURL, bytes.NewReader(compressed))
	if err != nil {
		return fmt.Errorf("request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/x-protobuf")
	httpReq.Header.Set("Content-Encoding", "snappy")
	httpReq.Header.Set("X-Prometheus-Remote-Write-Version", "0.1.0")
	if r.bearerToken != "" {
		httpReq.Header.Set("Authorization", "Bearer "+r.bearerToken)
	}

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("post: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}
	return nil
}
