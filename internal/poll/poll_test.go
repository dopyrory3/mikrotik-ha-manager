package poll

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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
		io.WriteString(w, `[{"name":"vrrp-lan","master":"true","backup":"false"}]`)
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
	if !snap.Reachable() || snap.Err != nil {
		t.Fatalf("Reachable = %v, Err = %v; want reachable with no error", snap.Reachable(), snap.Err)
	}
	if snap.Resource == nil || snap.Resource.Version != "7.15.3" {
		t.Errorf("Resource = %+v, want version 7.15.3", snap.Resource)
	}
	if snap.Identity == nil || snap.Identity.Name != "core-a" {
		t.Errorf("Identity = %+v, want core-a", snap.Identity)
	}
	if len(snap.VRRP) != 1 || snap.VRRP[0].Role() != routeros.RoleMaster {
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

	if snap.Reachable() {
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
	if !snap.Reachable() || snap.Err != nil {
		t.Fatalf("Reachable = %v, Err = %v; want reachable, no error", snap.Reachable(), snap.Err)
	}
	if snap.Resource == nil {
		t.Error("Resource = nil, want the successful system/resource read")
	}
	if snap.VRRP != nil {
		t.Errorf("VRRP = %+v, want nil when its endpoint fails", snap.VRRP)
	}
	if snap.VRRPErr == nil {
		t.Error("VRRPErr = nil, want the sub-endpoint's error so callers can tell failure from empty")
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

func TestPollRecordsEachSubEndpointFailure(t *testing.T) {
	fail := func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusInternalServerError) }
	mux := http.NewServeMux()
	mux.HandleFunc("/rest/system/resource", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"version":"7.15.3"}`)
	})
	mux.HandleFunc("/rest/system/identity", fail)
	mux.HandleFunc("/rest/interface/vrrp", fail)
	mux.HandleFunc("/rest/tool/netwatch", fail)

	client, _ := newTLSClient(t, mux)
	snap := New("a", client, time.Hour).pollOnce(context.Background())

	if !snap.Reachable() {
		t.Fatalf("Err = %v; sub-endpoint failures must not mark the router unreachable", snap.Err)
	}
	// Each failure is recorded against its own endpoint, so the readiness
	// checks fail closed on exactly what couldn't be read (project.md §10.1).
	if snap.IdentityErr == nil || snap.Identity != nil {
		t.Errorf("Identity = %+v, IdentityErr = %v; want nil and an error", snap.Identity, snap.IdentityErr)
	}
	if snap.VRRPErr == nil || snap.VRRP != nil {
		t.Errorf("VRRP = %+v, VRRPErr = %v; want nil and an error", snap.VRRP, snap.VRRPErr)
	}
	if snap.NetwatchErr == nil || snap.Netwatch != nil {
		t.Errorf("Netwatch = %+v, NetwatchErr = %v; want nil and an error", snap.Netwatch, snap.NetwatchErr)
	}
}

// runPoller starts p.Run and returns a channel closed when it returns.
func runPoller(ctx context.Context, p *Poller) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.Run(ctx)
	}()
	return done
}

func waitDone(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
}

func recvSnapshot(t *testing.T, p *Poller) Snapshot {
	t.Helper()
	select {
	case snap := <-p.C:
		return snap
	case <-time.After(5 * time.Second):
		t.Fatal("no snapshot within 5s")
		return Snapshot{}
	}
}

// Run polls once straight away (the dashboard shouldn't wait a whole
// interval for its first data), then on every tick until cancelled.
func TestRunPollsImmediatelyThenOnEachTick(t *testing.T) {
	var resourceCalls atomic.Int64
	mux := healthyRouterMux()
	counted := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/rest/system/resource" {
			resourceCalls.Add(1)
		}
		mux.ServeHTTP(w, r)
	})

	client, _ := newTLSClient(t, counted)

	// With an hour's interval, only the immediate poll can arrive.
	ctx, cancel := context.WithCancel(context.Background())
	slow := New("a", client, time.Hour)
	done := runPoller(ctx, slow)
	if snap := recvSnapshot(t, slow); !snap.Reachable() || snap.Router != "a" {
		t.Errorf("first snapshot = %+v, want reachable router a", snap)
	}
	cancel()
	waitDone(t, done)
	if got := resourceCalls.Load(); got != 1 {
		t.Errorf("polls with an hour's interval = %d, want exactly the immediate one", got)
	}

	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	fast := New("b", client, 10*time.Millisecond)
	done = runPoller(ctx, fast)
	for i := 0; i < 3; i++ {
		if snap := recvSnapshot(t, fast); snap.Router != "b" {
			t.Errorf("snapshot %d router = %q, want b", i, snap.Router)
		}
	}
	cancel()
	waitDone(t, done)
}

// A poll in flight when the TUI quits is abandoned rather than left to run
// out the client's timeout: the context reaches the HTTP request.
func TestRunCancelsInFlightPoll(t *testing.T) {
	started := make(chan struct{})
	var once sync.Once
	client, _ := newTLSClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(started) })
		<-r.Context().Done()
	}))
	p := New("a", client, time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	done := runPoller(ctx, p)

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("poll never reached the router")
	}
	cancel()
	waitDone(t, done)

	snap := recvSnapshot(t, p)
	if !errors.Is(snap.Err, context.Canceled) {
		t.Errorf("Err = %v, want context.Canceled", snap.Err)
	}
}
