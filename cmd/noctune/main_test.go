package main

import (
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
