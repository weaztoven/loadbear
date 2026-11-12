package backend

import (
	"sync"
	"testing"
	"time"
)

func TestNewValidationAndDefaults(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		weight  int
		wantErr bool
	}{
		{name: "valid", raw: "http://127.0.0.1:8080", weight: 3},
		{name: "invalid URL", raw: "http://[::1", weight: 1, wantErr: true},
		{name: "missing scheme", raw: "127.0.0.1:8080", weight: 1, wantErr: true},
		{name: "missing host", raw: "http:///path", weight: 1, wantErr: true},
		{name: "non-positive weight", raw: "http://127.0.0.1:8080", weight: 0, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			b, err := New(test.raw, test.weight)
			if (err != nil) != test.wantErr {
				t.Fatalf("New error = %v; wantErr %v", err, test.wantErr)
			}
			if err == nil {
				if b.Name != "127.0.0.1:8080" || b.Weight != 3 || !b.IsHealthy() ||
					b.CircuitState != CircuitClosed || b.CircuitFailureThreshold != 5 ||
					b.CircuitCooldown != 30*time.Second {
					t.Fatalf("unexpected defaults: %+v", b)
				}
			}
		})
	}
}

func TestHealthAndAtomicCounters(t *testing.T) {
	b, err := New("http://127.0.0.1:8080", 1)
	if err != nil {
		t.Fatal(err)
	}
	b.SetHealthy(false)
	if b.IsHealthy() {
		t.Fatal("backend should be unhealthy")
	}
	b.SetHealthy(true)
	if !b.IsHealthy() {
		t.Fatal("backend should be healthy")
	}
	b.IncrementActive()
	b.RecordRequest()
	b.RecordFailure()
	if got := b.ActiveConnections.Load(); got != 1 {
		t.Fatalf("active connections = %d", got)
	}
	if got := b.TotalRequests.Load(); got != 1 {
		t.Fatalf("requests = %d", got)
	}
	if got := b.TotalFailures.Load(); got != 1 {
		t.Fatalf("failures = %d", got)
	}
	b.DecrementActive()
	if got := b.ActiveConnections.Load(); got != 0 {
		t.Fatalf("active connections after decrement = %d", got)
	}
}

func TestCircuitBreakerTransitions(t *testing.T) {
	b, err := New("http://127.0.0.1:8080", 1)
	if err != nil {
		t.Fatal(err)
	}
	b.CircuitFailureThreshold = 2
	b.CircuitCooldown = time.Hour
	if !b.AllowRequest() {
		t.Fatal("closed circuit denied request")
	}
	b.RecordFailure()
	if b.CircuitState != CircuitClosed {
		t.Fatalf("first failure state = %q", b.CircuitState)
	}
	b.RecordFailure()
	if b.CircuitState != CircuitOpen || b.AllowRequest() {
		t.Fatalf("expected open circuit to deny requests, state=%q", b.CircuitState)
	}

	b.CircuitOpenedAt = time.Now().Add(-2 * time.Hour)
	if !b.AllowRequest() || b.CircuitState != CircuitHalfOpen {
		t.Fatalf("expected half-open state, got %q", b.CircuitState)
	}
	b.RecordFailure()
	if b.CircuitState != CircuitOpen {
		t.Fatalf("failed half-open probe state = %q", b.CircuitState)
	}
	b.CircuitOpenedAt = time.Now().Add(-2 * time.Hour)
	if !b.AllowRequest() {
		t.Fatal("expired open circuit denied half-open probe")
	}
	b.RecordSuccess()
	if b.CircuitState != CircuitClosed {
		t.Fatalf("successful half-open probe state = %q", b.CircuitState)
	}
	b.RecordFailure()
	b.CircuitOpenedAt = time.Now().Add(-2 * time.Hour)
	if !b.AllowRequest() {
		t.Fatal("open circuit denied after cooldown")
	}
	b.RecordSuccess()
	if b.CircuitState != CircuitClosed {
		t.Fatalf("successful expired-open recovery state = %q", b.CircuitState)
	}
}

func TestExport(t *testing.T) {
	b, err := New("http://127.0.0.1:8080", 4)
	if err != nil {
		t.Fatal(err)
	}
	b.IncrementActive()
	b.RecordRequest()
	b.RecordFailure()
	exported := b.Export()
	if exported["url"] != b.URL.String() || exported["name"] != b.Name ||
		exported["healthy"] != true || exported["active_connections"] != int64(1) ||
		exported["total_requests"] != uint64(1) || exported["total_failures"] != uint64(1) ||
		exported["circuit_state"] != string(CircuitClosed) || exported["weight"] != 4 {
		t.Fatalf("unexpected export: %#v", exported)
	}
}

func TestConcurrentCountersAndHealth(t *testing.T) {
	b, err := New("http://127.0.0.1:8080", 1)
	if err != nil {
		t.Fatal(err)
	}
	const workers = 20
	const iterations = 100
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range iterations {
				b.IncrementActive()
				b.RecordRequest()
				b.SetHealthy(false)
				b.SetHealthy(true)
				b.DecrementActive()
			}
		}()
	}
	wg.Wait()
	if got := b.TotalRequests.Load(); got != workers*iterations {
		t.Fatalf("request count = %d", got)
	}
	if got := b.ActiveConnections.Load(); got != 0 {
		t.Fatalf("active connections = %d", got)
	}
	if !b.IsHealthy() {
		t.Fatal("backend should be healthy after concurrent updates")
	}
}
