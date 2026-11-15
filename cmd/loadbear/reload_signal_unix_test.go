//go:build linux || darwin

package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestSIGHUPReloadSignalPath(t *testing.T) {
	first := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer second.Close()
	initial := reloadTestConfig("round_robin", first)
	manager, handler, _ := newReloadTestManager(t, initial, nil)
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := fmt.Sprintf("strategy: ip_hash\nbackends:\n  - url: %q\n    weight: 1\n", second.URL)
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	manager.configPath = path

	signals, stop := registerReloadSignal()
	defer stop()
	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	select {
	case <-signals:
	case <-time.After(time.Second):
		t.Fatal("registered reload signal did not receive SIGHUP")
	}
	if err := manager.ReloadFromFile(); err != nil {
		t.Fatal(err)
	}
	if handler.StrategyName() != "ip_hash" || len(handler.CurrentBackends()) != 1 ||
		handler.CurrentBackends()[0].URL.String() != second.URL {
		t.Fatalf("SIGHUP reload path did not apply the config: strategy=%q backends=%v",
			handler.StrategyName(), handler.CurrentBackends())
	}
}
