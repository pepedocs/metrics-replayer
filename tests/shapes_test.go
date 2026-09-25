package tests

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/pepedocs/metrics-replayer/shapes"
)

func mustNew(t *testing.T, name, params string) shapes.Shape {
	t.Helper()
	s, err := shapes.New(name, params)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestStep(t *testing.T) {
	s := mustNew(t, "step", "baseline=0.01,peak=0.3,start=-2h,duration=30m")
	for _, tc := range []struct {
		t    time.Duration
		want float64
	}{
		{-3 * time.Hour, 0.01},
		{-2 * time.Hour, 0.3},
		{-100 * time.Minute, 0.3},
		{-90 * time.Minute, 0.01},
		{time.Hour, 0.01},
	} {
		if got := s(tc.t); !near(got, tc.want) {
			t.Errorf("step(%s) = %v, want %v", tc.t, got, tc.want)
		}
	}
	if forever := mustNew(t, "step", "start=-1h,duration=0,peak=1"); forever(time.Hour) != 1 {
		t.Errorf("duration=0 should never roll back")
	}
}

func TestLaunch(t *testing.T) {
	s := mustNew(t, "launch", "baseline=0.01,peak=0.5,start=-3h,peak_at=20m,sigma=1")
	if got := s(-4 * time.Hour); !near(got, 0.01) {
		t.Errorf("before start: %v", got)
	}
	peakT := -3*time.Hour + 20*time.Minute
	if got := s(peakT); !near(got, 0.5) {
		t.Errorf("at peak_at: %v, want 0.5", got)
	}
	// Rises faster than it decays: 10m before the peak is lower than 10m after.
	if before, after := s(peakT-10*time.Minute), s(peakT+10*time.Minute); before >= after {
		t.Errorf("expected skewed curve: before=%v after=%v", before, after)
	}
	if got := s(0); got <= 0.01 || got >= 0.5 {
		t.Errorf("3h later should still be decaying above baseline: %v", got)
	}
}

func TestSpikes(t *testing.T) {
	s := mustNew(t, "spikes", "baseline=0,peak=1,every=1h,width=30s")
	for _, tc := range []struct {
		t    time.Duration
		want float64
	}{
		{0, 1},
		{20 * time.Second, 1},
		{30 * time.Second, 0},
		{-time.Hour, 1},
		{-time.Hour + 45*time.Second, 0},
		{-30 * time.Minute, 0},
	} {
		if got := s(tc.t); got != tc.want {
			t.Errorf("spikes(%s) = %v, want %v", tc.t, got, tc.want)
		}
	}
}

func TestDrift(t *testing.T) {
	s := mustNew(t, "drift", "baseline=0.1,a=0.02,b=2,start=-10h,unit=1h")
	if got := s(-11 * time.Hour); !near(got, 0.1) {
		t.Errorf("before start: %v", got)
	}
	// 5h elapsed: 0.1 + 0.02 * 5^2 = 0.6
	if got := s(-5 * time.Hour); !near(got, 0.6) {
		t.Errorf("after 5h: %v, want 0.6", got)
	}
}

func TestFlap(t *testing.T) {
	s := mustNew(t, "flap", "offset=0.1,amplitude=0.2,period=10m")
	if got := s(150 * time.Second); !near(got, 0.3) {
		t.Errorf("quarter period: %v, want 0.3", got)
	}
	if got := s(450 * time.Second); got != 0 {
		t.Errorf("trough should floor at 0: %v", got)
	}
}

func TestNewErrors(t *testing.T) {
	for params, wantErr := range map[string]string{
		"peak=abc":      "not a number",
		"start=soon":    "not a duration",
		"typo=1":        `unknown parameter "typo"`,
		"missing-equal": "want key=value",
	} {
		if _, err := shapes.New("step", params); err == nil || !strings.Contains(err.Error(), wantErr) {
			t.Errorf("%q: got %v, want %q", params, err, wantErr)
		}
	}
	if _, err := shapes.New("nope", ""); err == nil || !strings.Contains(err.Error(), "available") {
		t.Errorf("unknown shape: %v", err)
	}
	if _, err := shapes.New("spikes", "every=0s"); err == nil {
		t.Errorf("every=0 should fail")
	}
}
