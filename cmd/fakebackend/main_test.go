package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestFakeBackendModes(t *testing.T) {
	tests := []struct {
		name        string
		mode        string
		wantStatus  int
		wantReadErr bool
	}{
		{name: "normal", mode: "normal", wantStatus: http.StatusOK},
		{name: "drop mid response", mode: "drop-mid", wantStatus: http.StatusOK, wantReadErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(newHandler("test-backend", test.mode))
			defer server.Close()
			resp, err := http.Get(server.URL + "/")
			if err != nil {
				t.Fatal(err)
			}
			body, readErr := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != test.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, test.wantStatus)
			}
			if (readErr != nil) != test.wantReadErr {
				t.Fatalf("read error = %v, wantErr %v (body %q)", readErr, test.wantReadErr, body)
			}
		})
	}
}

func TestFakeBackendCloseAndHangModes(t *testing.T) {
	server := httptest.NewServer(newHandler("test-backend", "normal"))
	defer server.Close()

	if _, err := http.Get(server.URL + "/?mode=close"); err == nil {
		t.Fatal("close mode returned response headers")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/?mode=hang", nil)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := http.DefaultClient.Do(req); err == nil {
		t.Fatal("hang mode unexpectedly responded")
	}
	if time.Since(start) > time.Second {
		t.Fatal("hang mode did not stop after request context cancellation")
	}
}

func TestFakeBackendSlowModeCanBeCancelled(t *testing.T) {
	server := httptest.NewServer(newHandler("test-backend", "normal"))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/?mode=slow", nil)
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := http.DefaultClient.Do(req)
		result <- err
	}()
	cancel()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("cancelled slow mode unexpectedly returned a response")
		}
	case <-time.After(time.Second):
		t.Fatal("slow mode did not stop after cancellation")
	}
}
