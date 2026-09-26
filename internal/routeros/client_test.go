package routeros

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
)

func TestNewUsesExplicitPort(t *testing.T) {
	var gotPath string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	host, portStr, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatalf("split host/port: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse port: %v", err)
	}

	client := New(Config{
		Host:        host,
		Port:        port,
		InsecureTLS: true,
	})

	var out map[string]bool
	if err := client.Get(context.Background(), "/system/resource", &out); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if gotPath != "/rest/system/resource" {
		t.Errorf("request path = %q, want /rest/system/resource", gotPath)
	}
	if !out["ok"] {
		t.Errorf("decoded response = %v, want ok=true", out)
	}
}
