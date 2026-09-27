package ui

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"mtha/internal/config"
	"mtha/internal/diff"
	"mtha/internal/model"
	"mtha/internal/plan"
	"mtha/internal/poll"
	"mtha/internal/routeros"
	"mtha/internal/runtime"
)

// wantLines fails for every want that doesn't appear in view.
func wantLines(t *testing.T, view string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(view, w) {
			t.Errorf("view is missing %q:\n%s", w, view)
		}
	}
}

func rejectLines(t *testing.T, view string, rejects ...string) {
	t.Helper()
	for _, r := range rejects {
		if strings.Contains(view, r) {
			t.Errorf("view unexpectedly contains %q:\n%s", r, view)
		}
	}
}

func dashboardModel() Model {
	pair := &config.Pair{Name: "core", Routers: map[string]config.RouterConfig{
		"a": {Host: "10.0.0.2"},
		"b": {Host: "10.0.0.3"},
	}}
	m := New(pair, false, nil)
	m.width, m.height = 100, 30
	return m
}

// The Overview (project.md §7.1) shows each router's panel under its own
// host: waiting before the first poll, then reachable with its details or
// unreachable with the reason.
func TestDashboardRouterPanels(t *testing.T) {
	m := dashboardModel()
	wantLines(t, m.View(), "mtha — core", "Router A", "10.0.0.2", "Router B", "10.0.0.3", "waiting for first poll")

	m.snapshots["a"] = poll.Snapshot{
		Router:   "a",
		Resource: &routeros.SystemResource{Version: "7.15.3", Uptime: "1d2h", CPULoad: "4"},
		Identity: &routeros.Identity{Name: "core-a"},
		VRRP: []routeros.VRRPInstance{
			{Name: "vrrp-lan", Master: "true", Backup: "false"},
			{Name: "vrrp-wan", Master: "false", Backup: "true"},
			{Name: "vrrp-dmz"},
		},
	}
	m.snapshots["b"] = poll.Snapshot{Router: "b", Err: errors.New("i/o timeout")}

	view := m.View()
	wantLines(t, view,
		"reachable", "identity: core-a", "version:  7.15.3", "uptime:   1d2h", "cpu:      4",
		"master", "backup", "unknown",
		"unreachable", "i/o timeout",
	)
	rejectLines(t, view, "waiting for first poll")

	// A reachable router with no VRRP says so rather than showing nothing.
	m.snapshots["b"] = poll.Snapshot{Router: "b", Resource: &routeros.SystemResource{Version: "7.15.3"}}
	wantLines(t, m.View(), "no VRRP instances")
}

func TestDashboardShowsVerdict(t *testing.T) {
	m := dashboardModel()
	wantLines(t, m.View(), "Unknown")

	m.snapshots["a"] = poll.Snapshot{Router: "a", Err: errors.New("refused")}
	m.snapshots["b"] = poll.Snapshot{Router: "b", Err: errors.New("refused")}
	wantLines(t, m.View(), "Degraded")
}

func TestPanelContentWidth(t *testing.T) {
	tests := []struct{ term, want int }{
		{0, fallbackPanelWidth},
		{-1, fallbackPanelWidth},
		{100, (100-len(panelGap))/2 - panelBorderWidth},
		{20, minPanelWidth}, // too narrow: clamp rather than go negative
	}
	for _, tt := range tests {
		if got := panelContentWidth(tt.term); got != tt.want {
			t.Errorf("panelContentWidth(%d) = %d, want %d", tt.term, got, tt.want)
		}
	}
}

func TestVerdictString(t *testing.T) {
	for v, want := range map[Verdict]string{VerdictReady: "Ready", VerdictDegraded: "Degraded", VerdictUnknown: "Unknown"} {
		if got := v.String(); got != want {
			t.Errorf("Verdict(%d).String() = %q, want %q", v, got, want)
		}
	}
}

// driftViewModel is a drift screen with one section drifting three ways
// (only on A, only on B, changed), one clean and one whose fetch failed.
func driftViewModel() Model {
	pair := &config.Pair{Name: "core", Sync: config.SyncConfig{Sections: []string{"ip/firewall/filter", "ip/dns/static", "user"}}}
	m := New(pair, false, nil)
	m.screen = screenDrift
	m.driftData = map[string]diff.SectionDiff{
		"ip/firewall/filter": {Section: "ip/firewall/filter", Hunks: []diff.Hunk{
			{Identity: "allow-dns", OnA: true},
			{Identity: "allow-ntp", OnB: true},
			{Identity: "drop-rest", OnA: true, OnB: true, Changes: []model.FieldChange{{Field: "action", A: "drop", B: "reject"}}},
		}},
		"ip/dns/static": {Section: "ip/dns/static"},
	}
	m.driftErr = errors.New("fetch user from router b: 500")
	return m
}

func TestRenderDriftStates(t *testing.T) {
	pair := &config.Pair{Name: "core", Sync: config.SyncConfig{Sections: []string{"user"}}}
	m := New(pair, false, nil)
	m.screen = screenDrift

	wantLines(t, m.View(), "mtha — core — drift", "press r to fetch drift", "0 selected")

	m.driftFetching = true
	wantLines(t, m.View(), "fetching drift...")

	// Every section failed: the error alone, no lists.
	m.driftFetching = false
	m.driftErr = errors.New("connection refused")
	view := m.View()
	wantLines(t, view, "error: connection refused")
	rejectLines(t, view, "Sections")
}

func TestRenderDriftSectionAndHunkLists(t *testing.T) {
	m := driftViewModel()

	view := m.View()
	// A partial failure still shows the sections that did fetch.
	wantLines(t, view,
		"error: fetch user from router b: 500",
		"Sections",
		"> ip/firewall/filter", "3 hunk(s)",
		"ip/dns/static", "clean",
		"user", "fetch failed",
		"Hunks: ip/firewall/filter",
		"- only on A allow-dns", "+ only on B allow-ntp", "~ changed drop-rest",
	)
	// Field changes are only expanded for the focused hunk.
	rejectLines(t, view, "action:")

	m = press(m, tea.KeyMsg{Type: tea.KeyEnter}, key("j"), key("j"))
	view = m.View()
	wantLines(t, view, "action: drop -> reject", "> "+"      "+"~ changed drop-rest")
	rejectLines(t, view, "> ip/firewall/filter")

	// A selection shows its direction on the hunk and a count on the
	// section and status bar.
	m.setSelection("ip/firewall/filter", plan.RefOf(m.currentHunks()[0]), plan.BtoA)
	view = m.View()
	wantLines(t, view, "[B→A] - only on A allow-dns", "(1 selected)", "1 selected |")

	// The section whose fetch failed says so instead of claiming it's clean.
	m = press(m, tea.KeyMsg{Type: tea.KeyEsc}, key("j"), key("j"))
	wantLines(t, m.View(), "Hunks: user", "fetch failed for this section")

	m = press(m, key("k"))
	wantLines(t, m.View(), "Hunks: ip/dns/static", "no differences")
}

func TestRenderDriftWithNoSections(t *testing.T) {
	m := New(&config.Pair{Name: "core"}, false, nil)
	m.screen = screenDrift
	m.driftData = map[string]diff.SectionDiff{}

	view := m.View()
	wantLines(t, view, "(no sections configured)")
	rejectLines(t, view, "Hunks:")
}

func TestRenderRuntimeStates(t *testing.T) {
	m := New(&config.Pair{Name: "core"}, false, nil)
	m.screen = screenRuntime
	wantLines(t, m.View(), "mtha — core — runtime", "press r to check runtime status")

	m.runtimeFetching = true
	wantLines(t, m.View(), "working...")

	m.runtimeFetching = false
	m.runtimeStatus = runtime.Status{
		"a": {
			{Label: "vrrp vrrp-lan", State: runtime.StateOK},
			{Label: "netwatch 1.1.1.1", State: runtime.StateMismatched, Note: "interval differs"},
		},
		"b": {{Label: "script mtha-on-master", State: runtime.StateConflict, Note: "exists, not managed by mtha"}},
	}
	m.runtimeErr = errors.New("router b: timeout")
	m.runtimeNotice = "the Apply screen (4) is busy"
	wantLines(t, m.View(),
		"the Apply screen (4) is busy",
		"error: router b: timeout",
		"Router A", "ok  vrrp vrrp-lan", "mismatched  netwatch 1.1.1.1 (interval differs)",
		"Router B", "conflict  script mtha-on-master (exists, not managed by mtha)",
	)

	m.runtimeStatus = runtime.Status{}
	m.runtimeErr = nil
	wantLines(t, m.View(), "nothing to deploy")

	m.runtimePlanErr = errors.New(`vrrp "vrrp-lan": vrid required`)
	wantLines(t, m.View(), `config error: vrrp "vrrp-lan": vrid required`)
}

func TestStatusStyle(t *testing.T) {
	for state, want := range map[runtime.State]string{
		runtime.StateOK:         styleReady.Render("x"),
		runtime.StateConflict:   styleDown.Render("x"),
		runtime.StateMissing:    styleDegraded.Render("x"),
		runtime.StateMismatched: styleDegraded.Render("x"),
	} {
		if got := statusStyle(state).Render("x"); got != want {
			t.Errorf("statusStyle(%s) renders %q, want %q", state, got, want)
		}
	}
}

// renderApplyResult reports the outcome of a sync: how many ops ran, and
// per touched section whether re-verification found it clean.
func TestRenderApplyResultSync(t *testing.T) {
	p := plan.Plan{Ops: []plan.Op{
		{Router: "b", Method: "POST", Path: "/system/backup/save"},
		{Router: "b", Method: "PUT", Path: "/ip/firewall/filter", Section: "ip/firewall/filter"},
		{Router: "b", Method: "PUT", Path: "/ip/dns/static", Section: "ip/dns/static"},
		{Router: "b", Method: "PATCH", Path: "/user/*3", Section: "user"},
	}}
	a := applyState{
		kind:   applySync,
		stage:  applyDone,
		plan:   p,
		status: []opStatus{opDone, opDone, opDone, opDone},
		residual: map[string]diff.SectionDiff{
			"ip/firewall/filter": {Section: "ip/firewall/filter"},
			"ip/dns/static":      {Section: "ip/dns/static", Hunks: []diff.Hunk{{Identity: "nas.lan", OnA: true}}},
		},
	}

	out := strings.Join(renderApplyResult(a), "\n")
	wantLines(t, out, "applied 4/4 op(s)", "ip/firewall/filter", "clean", "1 residual", "nas.lan", "user", "not verified", "residual differences remain")

	a.status = []opStatus{opDone, opFailed, opPending, opPending}
	a.err = errors.New("PUT /ip/firewall/filter: 500")
	a.verifyErr = errors.New("fetch user: timeout")
	a.residual = map[string]diff.SectionDiff{}
	out = strings.Join(renderApplyResult(a), "\n")
	wantLines(t, out, "apply stopped after 1/4 op(s): PUT /ip/firewall/filter: 500", "verify error: fetch user: timeout")
	rejectLines(t, out, "residual differences remain")
}

// renderRuntimeVerify judges a deploy by every object being ok, and a
// remove by every object being gone (or left alone as not mtha's).
func TestRenderRuntimeVerify(t *testing.T) {
	tests := []struct {
		name      string
		kind      applyKind
		status    runtime.Status
		verifyErr error
		want      []string
		reject    []string
	}{
		{
			name:   "deploy verified",
			kind:   applyRuntimeDeploy,
			status: runtime.Status{"a": {{Label: "vrrp-lan", State: runtime.StateOK}}, "b": {{Label: "vrrp-lan", State: runtime.StateOK}}},
			want:   []string{"verified — see the Runtime screen (3)"},
		},
		{
			name:   "deploy left a mismatch",
			kind:   applyRuntimeDeploy,
			status: runtime.Status{"a": {{Label: "vrrp-lan", State: runtime.StateOK}}, "b": {{Label: "vrrp-lan", State: runtime.StateMismatched}}},
			want:   []string{"1 object(s) not as intended:", "router B: vrrp-lan mismatched"},
			reject: []string{"verified"},
		},
		{
			name:   "remove verified, foreign entry spared",
			kind:   applyRuntimeRemove,
			status: runtime.Status{"a": {{Label: "vrrp-lan", State: runtime.StateMissing}}, "b": {{Label: "vrrp-lan", State: runtime.StateConflict}}},
			want:   []string{"verified"},
		},
		{
			name:   "remove left an object behind",
			kind:   applyRuntimeRemove,
			status: runtime.Status{"a": {{Label: "vrrp-lan", State: runtime.StateOK}}},
			want:   []string{"router A: vrrp-lan ok"},
		},
		{
			// A failed verification must not be reported as verified.
			name:      "verify failed",
			kind:      applyRuntimeDeploy,
			status:    nil,
			verifyErr: errors.New("timeout"),
			reject:    []string{"verified"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := strings.Join(renderRuntimeVerify(applyState{kind: tt.kind, runtimeStatus: tt.status, verifyErr: tt.verifyErr}), "\n")
			wantLines(t, out, tt.want...)
			rejectLines(t, out, tt.reject...)
		})
	}
}

func TestApplyBodyHeight(t *testing.T) {
	tests := []struct{ term, header, footer, want int }{
		{0, 1, 1, 1 << 30}, // no size yet: show everything
		{24, 1, 2, 24 - (2 + 1 + 1 + 1 + 2 + 1 + 1)},
		{10, 2, 4, 3}, // never fewer than three plan lines
	}
	for _, tt := range tests {
		if got := applyBodyHeight(tt.term, tt.header, tt.footer); got != tt.want {
			t.Errorf("applyBodyHeight(%d, %d, %d) = %d, want %d", tt.term, tt.header, tt.footer, got, tt.want)
		}
	}
}

func TestRenderEventsRouterLine(t *testing.T) {
	m := eventsTestModel()
	if got := renderEventsRouterLine(m, "a"); !strings.Contains(got, "not read yet") {
		t.Errorf("before any read = %q, want \"not read yet\"", got)
	}
	m.eventsFetching = true
	if got := renderEventsRouterLine(m, "a"); !strings.Contains(got, "reading...") {
		t.Errorf("while reading = %q, want \"reading...\"", got)
	}
	m.eventsErrs = map[poll.RouterKey]error{"a": errors.New("timeout")}
	if got := renderEventsRouterLine(m, "a"); !strings.Contains(got, "error: timeout") {
		t.Errorf("after an error = %q, want the error", got)
	}
}

// Clock skew beyond eventsSkewWarn is pointed out in either direction;
// small or unknown skew is not (see eventsSkewWarn).
func TestRenderEventsRouterLineSkew(t *testing.T) {
	tests := []struct {
		name   string
		log    routeros.EventLog
		want   string
		reject string
	}{
		{"ahead", routeros.EventLog{Skew: 10 * time.Second, SkewKnown: true}, "clock 10s ahead of this machine", ""},
		{"behind", routeros.EventLog{Skew: -time.Minute, SkewKnown: true}, "clock 1m0s behind this machine", ""},
		{"within jitter", routeros.EventLog{Skew: time.Second, SkewKnown: true}, "0 event(s)", "clock"},
		{"unknown", routeros.EventLog{Skew: time.Hour}, "0 event(s)", "clock"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := eventsTestModel()
			m.eventsLogs = map[poll.RouterKey]routeros.EventLog{"b": tt.log}
			got := renderEventsRouterLine(m, "b")
			wantLines(t, got, "Router B: ", tt.want)
			if tt.reject != "" {
				rejectLines(t, got, tt.reject)
			}
		})
	}
}
