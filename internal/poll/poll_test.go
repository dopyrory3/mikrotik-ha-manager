package poll

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"mtha/internal/routeros"
)

// newTLSClient builds a real routeros.Client (as cmd/mtha does) pointed at a
// TLS httptest server. TLS is required because routeros.New hardcodes
// https://<host>/rest and the client's base URL is unexported across
// packages; a self-signed httptest cert plus InsecureTLS matches how the
// tool talks to routers with insecure_tls enabled.
func newTLSClient(t *testing.T, handler http.Handler) (*routeros.Client, *httptest.Server) {
	t.Helper()

	srv := httptest.NewTLSServer(handler)
	t.Cleanup(srv.Close)

	return routeros.New(routeros.Config{
		Host:        strings.TrimPrefix(srv.URL, "https://"),
		User:        "mtha",
		Password:    "pw",
		InsecureTLS: true,
		Timeout:     2 * time.Second,
	}), srv
}

// routerMux is a fake router; vrrp lets a test override that one endpoint to
// simulate a partial failure without re-registering a duplicate pattern.
func routerMux(vrrp http.HandlerFunc) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/rest/system/resource", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"version":"7.15.3","uptime":"1d"}`)
	})
	mux.HandleFunc("/rest/system/identity", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"name":"core-a"}`)
	})
	mux.HandleFunc("/rest/interface/vrrp", vrrp)
	mux.HandleFunc("/rest/tool/netwatch", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `[{"host":"1.1.1.1","status":"up"}]`)
	})
	return mux
}

func healthyRouterMux() *http.ServeMux {
	return routerMux(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `[{"name":"vrrp-lan","vrrp-state":"master"}]`)
	})
}

// poll is unexported and side-effect-free apart from the channel send, so
// tests drive one cycle directly instead of racing Run's ticker.
func (p *Poller) pollOnce(ctx context.Context) Snapshot {
	p.poll(ctx)
	return <-p.C
}

func TestPollEmitsReachableSnapshot(t *testing.T) {
	client, _ := newTLSClient(t, healthyRouterMux())
	p := New("a", client, time.Hour)

	snap := p.pollOnce(context.Background())

	if snap.Router != "a" {
		t.Errorf("Router = %q, want a", snap.Router)
	}
	if !snap.Reachable || snap.Err != nil {
		t.Fatalf("Reachable = %v, Err = %v; want reachable with no error", snap.Reachable, snap.Err)
	}
	if snap.Resource == nil || snap.Resource.Version != "7.15.3" {
		t.Errorf("Resource = %+v, want version 7.15.3", snap.Resource)
	}
	if snap.Identity == nil || snap.Identity.Name != "core-a" {
		t.Errorf("Identity = %+v, want core-a", snap.Identity)
	}
	if len(snap.VRRP) != 1 || snap.VRRP[0].State != "master" {
		t.Errorf("VRRP = %+v, want one master", snap.VRRP)
	}
	if len(snap.Netwatch) != 1 || snap.Netwatch[0].Status != "up" {
		t.Errorf("Netwatch = %+v, want one up entry", snap.Netwatch)
	}
	if snap.PolledAt.IsZero() {
		t.Error("PolledAt not set")
	}
}

func TestPollMarksUnreachableOnConnectionFailure(t *testing.T) {
	srv := httptest.NewTLSServer(healthyRouterMux())
	host := strings.TrimPrefix(srv.URL, "https://")
	srv.Close() // nothing listening now

	client := routeros.New(routeros.Config{
		Host: host, User: "mtha", Password: "pw",
		InsecureTLS: true, Timeout: time.Second,
	})
	snap := New("a", client, time.Hour).pollOnce(context.Background())

	if snap.Reachable {
		t.Error("Reachable = true for a dead router")
	}
	if snap.Err == nil {
		t.Error("Err = nil, want the connection error")
	}
	if snap.Resource != nil {
		t.Errorf("Resource = %+v, want nil when unreachable", snap.Resource)
	}
}

func TestPollToleratesPartialEndpointFailure(t *testing.T) {
	mux := routerMux(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	client, _ := newTLSClient(t, mux)
	snap := New("b", client, time.Hour).pollOnce(context.Background())

	// /system/resource succeeded, so the router is reachable; a single
	// failing sub-read must not fail the whole snapshot (see poll.poll).
	if !snap.Reachable || snap.Err != nil {
		t.Fatalf("Reachable = %v, Err = %v; want reachable, no error", snap.Reachable, snap.Err)
	}
	if snap.Resource == nil {
		t.Error("Resource = nil, want the successful system/resource read")
	}
	if snap.VRRP != nil {
		t.Errorf("VRRP = %+v, want nil when its endpoint fails", snap.VRRP)
	}
}

func TestEmitKeepsOnlyTheFreshestSnapshot(t *testing.T) {
	var calls atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/rest/system/resource", func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		io.WriteString(w, `{"version":"7.15.`+string(rune('0'+n))+`"}`)
	})

	client, _ := newTLSClient(t, mux)
	p := New("a", client, time.Hour)
	ctx := context.Background()

	// Two polls without a reader: the buffer holds one, so the stale first
	// snapshot must be dropped in favour of the second.
	p.poll(ctx)
	p.poll(ctx)

	if got := len(p.C); got != 1 {
		t.Fatalf("buffered snapshots = %d, want 1", got)
	}
	snap := <-p.C
	if snap.Resource == nil || snap.Resource.Version != "7.15.2" {
		t.Errorf("kept snapshot version = %+v, want the second poll (7.15.2)", snap.Resource)
	}
}
