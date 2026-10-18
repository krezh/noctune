package main

import (
	"context"
	"log/slog"
	"net/http"
	"testing"
)

func TestHTTPServerTimeouts(t *testing.T) {
	server := newHTTPServer(":8080", http.NotFoundHandler())
	if server.ReadHeaderTimeout <= 0 {
		t.Fatal("ReadHeaderTimeout must be bounded")
	}
	if server.ReadTimeout <= 0 {
		t.Fatal("ReadTimeout must be bounded")
	}
	if server.IdleTimeout <= 0 {
		t.Fatal("IdleTimeout must be bounded")
	}
	if server.WriteTimeout != 0 {
		t.Fatal("WriteTimeout must remain disabled for SSE")
	}
}
func TestConfigureLoggingSetsLevel(t *testing.T) {
	previous := slog.Default()
	t.Cleanup(func() { slog.SetDefault(previous) })

	configureLogging("debug")
	if !slog.Default().Enabled(context.Background(), slog.LevelDebug) {
		t.Fatal("debug logging was not enabled")
	}
	configureLogging("info")
	if slog.Default().Enabled(context.Background(), slog.LevelDebug) {
		t.Fatal("debug logging remained enabled at info level")
	}
}
