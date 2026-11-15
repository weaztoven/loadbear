package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestPercentileNearestRank(t *testing.T) {
	samples := []time.Duration{time.Millisecond, 2 * time.Millisecond, 3 * time.Millisecond, 4 * time.Millisecond, 5 * time.Millisecond}
	for _, test := range []struct {
		name     string
		quantile float64
		want     time.Duration
	}{
		{name: "p50", quantile: 0.50, want: 3 * time.Millisecond},
		{name: "p90", quantile: 0.90, want: 5 * time.Millisecond},
		{name: "p99", quantile: 0.99, want: 5 * time.Millisecond},
		{name: "minimum", quantile: 0, want: time.Millisecond},
		{name: "maximum", quantile: 1, want: 5 * time.Millisecond},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := percentile(samples, test.quantile); got != test.want {
				t.Fatalf("percentile(%v) = %s, want %s", test.quantile, got, test.want)
			}
		})
	}
	if got := percentile(nil, 0.5); got != 0 {
		t.Fatalf("empty percentile = %s, want 0", got)
	}
}

func TestSummaryCountsAndLatency(t *testing.T) {
	results := newCollector(8)
	results.record(time.Millisecond, 200, nil)
	results.record(2*time.Millisecond, 503, nil)
	results.record(3*time.Millisecond, 0, errors.New("connection failed"))
	results.record(4*time.Millisecond, 201, nil)

	summary := results.summary("http://127.0.0.1:8080/", "GET", 10*time.Second, 2*time.Second, 4)
	if summary.TotalRequests != 4 || summary.Errors != 1 || summary.Non2xx != 1 {
		t.Fatalf("summary counts = total:%d errors:%d non2xx:%d", summary.TotalRequests, summary.Errors, summary.Non2xx)
	}
	if summary.RequestsPerSecond != 2 || summary.LatencySamples != 4 || summary.LatencyReservoirLimit != 8 {
		t.Fatalf("summary rate/sample data = rate:%v samples:%d limit:%d",
			summary.RequestsPerSecond, summary.LatencySamples, summary.LatencyReservoirLimit)
	}
	if summary.LatencyP50MS != 2 || summary.LatencyP90MS != 4 ||
		summary.LatencyP99MS != 4 || summary.LatencyMaxMS != 4 {
		t.Fatalf("summary latency = p50:%v p90:%v p99:%v max:%v",
			summary.LatencyP50MS, summary.LatencyP90MS, summary.LatencyP99MS, summary.LatencyMaxMS)
	}
}

func TestCollectorReservoirIsBounded(t *testing.T) {
	results := newCollector(3)
	for i := 0; i < 10; i++ {
		results.record(time.Duration(i)*time.Millisecond, 200, nil)
	}
	if got := len(results.latency); got != 3 {
		t.Fatalf("stored latency samples = %d, want bounded size 3", got)
	}
	summary := results.summary("http://example.test", "GET", time.Second, time.Second, 1)
	if summary.TotalRequests != 10 || summary.LatencySamples != 3 || summary.LatencyMaxMS != 9 {
		t.Fatalf("bounded summary = %#v", summary)
	}
}

func TestRunPhaseDrainsInflightRequestAfterDuration(t *testing.T) {
	started := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		started <- struct{}{}
		time.Sleep(25 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	client := &http.Client{Timeout: time.Second}
	results := newCollector(8)
	done := make(chan time.Duration, 1)
	go func() {
		done <- runPhase(client, server.URL, http.MethodGet, 1, 10*time.Millisecond, results)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("request did not start")
	}
	elapsed := <-done
	summary := results.summary(server.URL, http.MethodGet, 10*time.Millisecond, elapsed, 1)
	if summary.TotalRequests != 1 || summary.Errors != 0 || summary.Non2xx != 0 {
		t.Fatalf("in-flight request was not drained cleanly: %#v", summary)
	}
	if elapsed <= 10*time.Millisecond {
		t.Fatalf("elapsed = %s; expected to include response drain", elapsed)
	}
}
