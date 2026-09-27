package ui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"mtha/internal/config"
	"mtha/internal/events"
	"mtha/internal/poll"
	"mtha/internal/routeros"
)

// runtimeFixture is a pair with one deployable VRRP instance and one
// netwatch target, two empty routers, and router A polled as master.
func runtimeFixture(t *testing.T, writeMode bool) (Model, *fakeRouter, *fakeRouter) {
	t.Helper()
	ra := newFakeRouter(map[string][]map[string]any{})
	rb := newFakeRouter(map[string][]map[string]any{})
	pollers := map[poll.RouterKey]*poll.Poller{
		"a": poll.New("a", testClient(t, ra), pollInterval),
		"b": poll.New("b", testClient(t, rb), pollInterval),
	}
	pair := &config.Pair{
		Name: "core",
		VRRP: []config.VRRPInstance{{Interface: "vrrp-lan", On: "ether2", VRID: 1, Addresses: []string{"10.0.0.1/24"}}},
		Runtime: config.RuntimeConfig{
			NetwatchTargets: []string{"1.1.1.1"},
			PriorityMaster:  200, PriorityBackup: 100, PriorityDegraded: 50,
		},
	}
	m := New(pair, writeMode, pollers)
	m.snapshots["a"] = poll.Snapshot{Router: "a", VRRP: []routeros.VRRPInstance{{Name: "vrrp-lan", Master: "true", Backup: "false"}}}
	m.snapshots["b"] = poll.Snapshot{Router: "b", VRRP: []routeros.VRRPInstance{{Name: "vrrp-lan", Master: "false", Backup: "true"}}}
	return m, ra, rb
}

func noWrites(t *testing.T, routers ...*fakeRouter) {
	t.Helper()
	for _, r := range routers {
		if w := r.writeLog(); len(w) != 0 {
			t.Fatalf("unexpected router writes: %v", w)
		}
	}
}

// d plans the deploy from fresh reads and shows it as a dry run on the
// Apply screen — every REST call with its body, each router's starting with
// a backup — before anything is written.
func TestRuntimeDeployShowsDryRunBeforeWriting(t *testing.T) {
	m, ra, rb := runtimeFixture(t, true)

	m = drive(t, m, key("3"))
	m = drive(t, m, key("d"))

	if m.screen != screenApply || m.apply.kind != applyRuntimeDeploy || m.apply.stage != applyReview {
		t.Fatalf("screen = %v kind = %v stage = %v err = %v, want the deploy dry run", m.screen, m.apply.kind, m.apply.stage, m.apply.err)
	}
	seen := map[string]bool{}
	for _, op := range m.apply.plan.Ops {
		if !seen[op.Router] && op.Path != "/system/backup/save" {
			t.Errorf("router %s's first op is %s, want the backup", op.Router, op)
		}
		seen[op.Router] = true
	}
	if !seen["a"] || !seen["b"] {
		t.Errorf("deploy should write to both routers:\n%s", m.apply.plan.Render())
	}
	view := renderApply(m)
	for _, want := range []string{"runtime deploy", "POST /system/backup/save", `PUT /interface/vrrp {"comment":"mtha:vrrp:vrrp-lan"`, "PUT /tool/netwatch"} {
		if !strings.Contains(view, want) {
			t.Errorf("dry run is missing %q:\n%s", want, view)
		}
	}
	noWrites(t, ra, rb)
}

func TestRuntimeReadOnlyRefusesToWrite(t *testing.T) {
	m, ra, rb := runtimeFixture(t, false)

	m = drive(t, m, key("3"))
	m = drive(t, m, key("d"))
	m = drive(t, m, key("y"))

	if m.apply.stage != applyReview || !strings.Contains(m.apply.notice, "read-only") {
		t.Fatalf("stage = %v notice = %q, want review with a read-only notice", m.apply.stage, m.apply.notice)
	}
	noWrites(t, ra, rb)
}

// Deploy writes to both routers, one of them master, so y alone must not
// write; Y runs the plan (backup first on each router), then re-verifies
// and journals one runtime action per router.
func TestRuntimeDeployToMasterRequiresSecondConfirmation(t *testing.T) {
	m, ra, rb := runtimeFixture(t, true)

	m = drive(t, m, key("3"))
	m = drive(t, m, key("d"))
	m = drive(t, m, key("y"))
	if m.apply.stage != applyConfirmMaster {
		t.Fatalf("stage = %v, want the master confirmation", m.apply.stage)
	}
	m = drive(t, m, key("y"))
	noWrites(t, ra, rb)

	m = drive(t, m, key("Y"))
	if m.apply.stage != applyDone || m.apply.err != nil {
		t.Fatalf("stage = %v err = %v, want done", m.apply.stage, m.apply.err)
	}
	want := "POST /system/backup/save,PUT /interface/vrrp,PUT /ip/address,PUT /tool/netwatch,PUT /system/scheduler"
	for name, r := range map[string]*fakeRouter{"a": ra, "b": rb} {
		if got := strings.Join(r.writeLog(), ","); got != want {
			t.Errorf("router %s writes = %s, want %s", name, got, want)
		}
	}
	if !m.runtimeStatus.Clean() || m.writing {
		t.Errorf("runtime status = %+v writing = %v, want verified clean and the write lock released", m.runtimeStatus, m.writing)
	}
	acts := m.journal.Actions()
	if len(acts) != 2 || acts[0].Kind != events.ActionRuntime || acts[0].Summary != "deploy runtime logic: 5/5 ops" {
		t.Errorf("journal = %+v, want one runtime action per router", acts)
	}
}

// If a router changes between the dry run and the confirmation, the recheck
// shows the new plan instead of running the confirmed one.
func TestRuntimeRecheckRefusesChangedPlan(t *testing.T) {
	m, ra, rb := runtimeFixture(t, true)

	m = drive(t, m, key("3"))
	m = drive(t, m, key("d"))
	rb.set("tool/netwatch", []map[string]any{{".id": "*9", "host": "9.9.9.9", "comment": "mtha:netwatch:1.1.1.1"}})
	m = drive(t, m, key("y"))
	m = drive(t, m, key("Y"))

	if m.apply.stage != applyReview || !strings.Contains(m.apply.notice, "changed") {
		t.Fatalf("stage = %v notice = %q, want review with a changed-state notice", m.apply.stage, m.apply.notice)
	}
	noWrites(t, ra, rb)
	if !strings.Contains(m.apply.plan.Render(), "PATCH /tool/netwatch/*9") {
		t.Errorf("updated plan should patch the changed entry:\n%s", m.apply.plan.Render())
	}
}

// Remove backs up first, deletes the VIP before its interface, and leaves
// an untagged same-named interface alone.
func TestRuntimeRemoveBacksUpAndSparesUnmanaged(t *testing.T) {
	m, ra, rb := runtimeFixture(t, true)
	ra.set("interface/vrrp", []map[string]any{{".id": "*1", "name": "vrrp-lan", "comment": "mtha:vrrp:vrrp-lan"}})
	ra.set("ip/address", []map[string]any{{".id": "*2", "address": "10.0.0.1/24", "comment": "mtha:vrrp:vrrp-lan:10.0.0.1/24"}})
	rb.set("interface/vrrp", []map[string]any{{".id": "*1", "name": "vrrp-lan", "comment": "hand-made"}})

	m = drive(t, m, key("3"))
	m = drive(t, m, key("x"))
	if !strings.Contains(renderApply(m), "exists, not managed by mtha") {
		t.Errorf("dry run should list the untagged interface as skipped:\n%s", renderApply(m))
	}
	m = drive(t, m, key("y"))
	m = drive(t, m, key("Y"))

	if got := strings.Join(ra.writeLog(), ","); got != "POST /system/backup/save,DELETE /ip/address/*2,DELETE /interface/vrrp/*1" {
		t.Errorf("router a writes = %s", got)
	}
	noWrites(t, rb)
}

// While a sync apply is running, the Runtime screen refuses to start a
// deploy, says why, and leaves the running apply alone.
func TestWriteLockRefusesRuntimeWhileApplyRuns(t *testing.T) {
	m, ra, rb := applyFixture(t, true, "backup")
	m = drive(t, m, key("4"))

	// Confirm and run the recheck, but hold the first write's command, as
	// if it were still in flight.
	next, cmd := m.Update(key("y"))
	m = next.(Model)
	next, cmd = m.Update(cmd())
	m = next.(Model)
	if m.apply.stage != applyRunning || !m.writing {
		t.Fatalf("stage = %v writing = %v, want an apply in flight", m.apply.stage, m.writing)
	}

	m = drive(t, m, key("3"))
	m = drive(t, m, key("d"))
	if m.screen != screenRuntime || m.apply.kind != applySync || m.apply.stage != applyRunning {
		t.Fatalf("screen = %v kind = %v stage = %v, want the deploy refused", m.screen, m.apply.kind, m.apply.stage)
	}
	if !strings.Contains(renderRuntime(m), "already running") {
		t.Errorf("runtime screen should say why:\n%s", renderRuntime(m))
	}

	for cmd != nil {
		var next tea.Model
		next, cmd = m.Update(cmd())
		m = next.(Model)
	}
	if m.apply.stage != applyDone || m.writing {
		t.Fatalf("stage = %v writing = %v, want the apply finished and the lock released", m.apply.stage, m.writing)
	}
	if got := len(rb.writeLog()); got != 2 {
		t.Errorf("router b writes = %v, want only the apply's two", rb.writeLog())
	}
	noWrites(t, ra)

	m = drive(t, m, key("3"))
	m = drive(t, m, key("d"))
	if m.screen != screenApply || m.apply.kind != applyRuntimeDeploy {
		t.Errorf("screen = %v kind = %v, want deploy allowed once the apply finished", m.screen, m.apply.kind)
	}
}
