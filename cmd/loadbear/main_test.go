package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRecoverMiddlewareReturnsInternalServerError(t *testing.T) {
	handler := recoverMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("test panic")
	}))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d; want 500", recorder.Code)
	}
	if got, want := recorder.Body.String(), "internal server error\n"; got != want {
		t.Fatalf("body = %q; want %q", got, want)
	}
}

func TestNewLoggerSupportsErrorLevel(t *testing.T) {
	logger := newLogger("error", "text")
	if logger.Enabled(context.Background(), slog.LevelInfo) {
		t.Fatal("error logger unexpectedly enables info")
	}
	if !logger.Enabled(context.Background(), slog.LevelError) {
		t.Fatal("error logger does not enable error")
	}
}
