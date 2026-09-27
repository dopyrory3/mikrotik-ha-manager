package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"mtha/internal/config"
	"mtha/internal/poll"
	"mtha/internal/ui"
)

func parse(t *testing.T, args ...string) (options, error) {
	t.Helper()
	fs := flag.NewFlagSet("mtha", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return parseFlags(fs, args, "/default/pairs.yaml")
}

func TestParseFlags(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want options
	}{
		{"defaults", nil, options{configPath: "/default/pairs.yaml"}},
		{
			"all set",
			[]string{"-config", "/tmp/p.yaml", "-pair", "edge", "-write", "-init", "-version"},
			options{configPath: "/tmp/p.yaml", pairName: "edge", write: true, initConfig: true, showVer: true},
		},
		// -write is the one switch between read-only and write sessions
		// (project.md §7.3); it must not be picked up by anything else.
		{"write only", []string{"-write"}, options{configPath: "/default/pairs.yaml", write: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parse(t, tt.args...)
			if err != nil {
				t.Fatalf("parseFlags: %v", err)
			}
			if got != tt.want {
				t.Errorf("parseFlags(%q) = %+v, want %+v", tt.args, got, tt.want)
			}
		})
	}
}

func TestParseFlagsRejectsUnknownFlag(t *testing.T) {
	if _, err := parse(t, "-writ"); err == nil {
		t.Fatal("parseFlags accepted -writ, want an error")
	}
}

func TestSetupVersion(t *testing.T) {
	old := version
	version = "v1.2.3"
	t.Cleanup(func() { version = old })

	var out bytes.Buffer
	// -version wins over everything else, even a config that doesn't exist.
	model, err := setup(options{showVer: true, configPath: "/nonexistent"}, &out)
	if err != nil || model != nil {
		t.Fatalf("setup = %v, %v; want nil model, nil error", model, err)
	}
	if got := out.String(); got != "mtha v1.2.3\n" {
		t.Errorf("output = %q, want %q", got, "mtha v1.2.3\n")
	}
}

func TestSetupInitWritesSample(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mtha", "pairs.yaml")

	var out bytes.Buffer
	model, err := setup(options{initConfig: true, configPath: path}, &out)
	if err != nil || model != nil {
		t.Fatalf("setup = %v, %v; want nil model, nil error", model, err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("sample not written: %v", err)
	}
	if string(data) != config.SampleYAML {
		t.Error("written file is not config.SampleYAML")
	}
	for _, want := range []string{path, "MTHA_<PAIR>_<ROUTER>_PASSWORD"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output %q does not mention %q", out.String(), want)
		}
	}

	// A second -init must refuse rather than clobber the operator's file.
	if _, err := setup(options{initConfig: true, configPath: path}, io.Discard); err == nil {
		t.Error("second -init succeeded, want refusal to overwrite")
	}
}

// writeConfig writes a pair file and returns its path.
func writeConfig(t *testing.T, yaml string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pairs.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

const twoPairs = `pairs:
  - name: core
    routers:
      a: { host: 10.0.0.2, user: mtha }
      b: { host: 10.0.0.3, user: mtha }
  - name: edge
    routers:
      a: { host: 10.1.0.2, user: mtha }
      b: { host: 10.1.0.3, user: mtha }
`

func TestSetupErrors(t *testing.T) {
	t.Setenv("MTHA_CORE_A_PASSWORD", "pa")
	t.Setenv("MTHA_CORE_B_PASSWORD", "")

	tests := []struct {
		name    string
		yaml    string // "" means no file at all
		pair    string
		wantErr string
	}{
		{"missing file", "", "", "read pair file"},
		{"no pairs", "pairs: []\n", "", "no pairs defined"},
		{"multiple pairs without -pair", twoPairs, "", "multiple pairs defined"},
		{"unknown -pair", twoPairs, "dmz", `pair "dmz" not found`},
		// Router b's variable is set but empty: that is "not set", and the
		// error must name the variable the operator needs to export.
		{"missing password", twoPairs, "core", "MTHA_CORE_B_PASSWORD"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "absent.yaml")
			if tt.yaml != "" {
				path = writeConfig(t, tt.yaml)
			}
			model, err := setup(options{configPath: path, pairName: tt.pair}, io.Discard)
			if err == nil {
				t.Fatalf("setup succeeded (model %v), want error containing %q", model, tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestSetupSelectsPairAndForwardsWrite(t *testing.T) {
	for _, env := range []string{"MTHA_CORE_A_PASSWORD", "MTHA_CORE_B_PASSWORD", "MTHA_EDGE_A_PASSWORD", "MTHA_EDGE_B_PASSWORD"} {
		t.Setenv(env, "pw")
	}
	single := writeConfig(t, strings.SplitAfter(twoPairs, "b: { host: 10.0.0.3, user: mtha }\n")[0])
	multi := writeConfig(t, twoPairs)

	tests := []struct {
		name     string
		opts     options
		wantPair string
		wantMode string
	}{
		{"single pair is implied", options{configPath: single}, "core", "read-only"},
		{"-pair picks from several", options{configPath: multi, pairName: "edge"}, "edge", "read-only"},
		{"-write reaches the UI", options{configPath: multi, pairName: "core", write: true}, "core", "write"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model, err := setup(tt.opts, io.Discard)
			if err != nil {
				t.Fatalf("setup: %v", err)
			}
			if model == nil {
				t.Fatal("setup returned a nil model")
			}
			// The model's own view is the only observable outside package
			// ui: the title names the pair and the status bar opens with
			// the mode (project.md §7.1).
			view := model.View()
			if !strings.Contains(view, "mtha — "+tt.wantPair) {
				t.Errorf("view title does not name pair %q:\n%s", tt.wantPair, view)
			}
			lines := strings.Split(view, "\n")
			bar := lines[len(lines)-1]
			if !strings.Contains(bar, " "+tt.wantMode+" | ") {
				t.Errorf("status bar = %q, want mode %q", bar, tt.wantMode)
			}
		})
	}
}

// fakeRouter records the basic-auth credentials each request carries.
type fakeRouter struct {
	srv *httptest.Server

	mu    sync.Mutex
	auths []string
}

func newFakeRouter(t *testing.T) *fakeRouter {
	t.Helper()
	r := &fakeRouter{}
	r.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.auths = append(r.auths, req.Header.Get("Authorization"))
		r.mu.Unlock()
		io.WriteString(w, `{"version":"7.15.3"}`)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *fakeRouter) hostPort(t *testing.T) (string, int) {
	t.Helper()
	u, err := url.Parse(r.srv.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("parse server port: %v", err)
	}
	return u.Hostname(), port
}

func basicAuth(user, password string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+password))
}

// buildPollers must give each router key its own client, pointed at that
// router's host and port with that router's user and password
// (MTHA_<PAIR>_<ROUTER>_PASSWORD, project.md §5.1). A swapped key or
// credential would otherwise go unnoticed until a live pair.
func TestBuildPollersWiresEachRouter(t *testing.T) {
	routerA, routerB := newFakeRouter(t), newFakeRouter(t)
	hostA, portA := routerA.hostPort(t)
	hostB, portB := routerB.hostPort(t)

	pair := &config.Pair{
		Name: "core",
		Routers: map[string]config.RouterConfig{
			"a": {Host: hostA, Port: portA, User: "user-a", InsecureTLS: true},
			"b": {Host: hostB, Port: portB, User: "user-b", InsecureTLS: true},
		},
	}
	t.Setenv("MTHA_CORE_A_PASSWORD", "secret-a")
	t.Setenv("MTHA_CORE_B_PASSWORD", "secret-b")

	pollers, err := buildPollers(pair)
	if err != nil {
		t.Fatalf("buildPollers: %v", err)
	}
	if len(pollers) != 2 {
		t.Fatalf("got %d pollers, want 2", len(pollers))
	}

	for key, want := range map[poll.RouterKey]struct {
		router *fakeRouter
		auth   string
	}{
		"a": {routerA, basicAuth("user-a", "secret-a")},
		"b": {routerB, basicAuth("user-b", "secret-b")},
	} {
		p := pollers[key]
		if p == nil {
			t.Fatalf("no poller for router %q", key)
		}
		if p.Router != key {
			t.Errorf("pollers[%q].Router = %q", key, p.Router)
		}
		if p.Interval != ui.DefaultPollInterval() {
			t.Errorf("pollers[%q].Interval = %v, want %v", key, p.Interval, ui.DefaultPollInterval())
		}
		if _, err := p.Client.SystemResource(context.Background()); err != nil {
			t.Fatalf("pollers[%q] client: %v", key, err)
		}
		want.router.mu.Lock()
		auths := want.router.auths
		want.router.mu.Unlock()
		if len(auths) != 1 || auths[0] != want.auth {
			t.Errorf("router %q saw auth %q, want one request with %q", key, auths, want.auth)
		}
	}
}

func TestBuildPollersRequiresEveryPassword(t *testing.T) {
	pair := &config.Pair{
		Name: "core",
		Routers: map[string]config.RouterConfig{
			"a": {Host: "10.0.0.2"},
			"b": {Host: "10.0.0.3"},
		},
	}
	t.Setenv("MTHA_CORE_A_PASSWORD", "secret-a")
	t.Setenv("MTHA_CORE_B_PASSWORD", "")

	pollers, err := buildPollers(pair)
	if err == nil {
		t.Fatalf("buildPollers = %v, want an error for router b's missing password", pollers)
	}
	if !strings.Contains(err.Error(), "MTHA_CORE_B_PASSWORD") {
		t.Errorf("error = %q, want it to name MTHA_CORE_B_PASSWORD", err)
	}
}
