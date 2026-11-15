package metrics

import (
	"strings"
	"testing"
	"time"

	"github.com/weaztoven/loadbear/internal/backend"
)

func TestEscapeLabel(t *testing.T) {
	const value = "backend\"\\\nname"
	const want = `backend\"\\\nname`
	if got := escapeLabel(value); got != want {
		t.Fatalf("escaped label = %q, want %q", got, want)
	}
}

func TestTunnelMetricBackendLabelIsEscaped(t *testing.T) {
	b, err := backend.New("http://127.0.0.1:8081", 1)
	if err != nil {
		t.Fatal(err)
	}
	b.URL.RawQuery = "q=quote\"\\\n"
	collector := New([]*backend.Backend{b})
	collector.RecordTunnelOpened(b)
	collector.RecordTunnelClosed(b, TunnelReasonUpstreamError, 2*time.Second)
	text := collector.PrometheusText()
	want := `loadbear_backend_active_tunnels{backend="` + escapeLabel(b.URL.String()) + `"} 0`
	if !strings.Contains(text, want) {
		t.Fatalf("escaped backend label missing %q:\n%s", want, text)
	}
	want = `loadbear_tunnel_closed_total{backend="` + escapeLabel(b.URL.String()) + `",reason="upstream_error"} 1`
	if !strings.Contains(text, want) {
		t.Fatalf("escaped tunnel label missing %q:\n%s", want, text)
	}
}
