package ui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"mtha/internal/config"
	"mtha/internal/events"
	"mtha/internal/plan"
	"mtha/internal/poll"
	"mtha/internal/routeros"
)

// fakeRouter is a tiny stateful RouterOS REST stand-in: GET lists a
// section, PUT adds (honouring place-before), PATCH/DELETE edit by .id, and
// POST commands are recorded. Every write is logged in order.
type fakeRouter struct {
	mu       sync.Mutex
	sections map[string][]map[string]any
	writes   []string
	failPath string // a write to this path returns 500
	nextID   int
	// perGet, if set, returns rows appended to one GET of a section and
	// never stored, like ip/service's per-connection reverse-proxy rows.
	perGet func(section string) []map[string]any
}

func newFakeRouter(sections map[string][]map[string]any) *fakeRouter {
	return &fakeRouter{sections: sections, nextID: 100}
}

func (f *fakeRouter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	path := strings.TrimPrefix(r.URL.Path, "/rest/")
	if r.Method == http.MethodGet {
		entries := f.sections[path]
		if f.perGet != nil {
			entries = append(append([]map[string]any(nil), entries...), f.perGet(path)...)
		}
		if entries == nil {
			entries = []map[string]any{}
		}
		json.NewEncoder(w).Encode(entries)
		return
	}

	f.writes = append(f.writes, r.Method+" /"+path)
	if "/"+path == f.failPath {
		http.Error(w, `{"detail":"boom"}`, http.StatusInternalServerError)
		return
	}

	var body map[string]any
	json.NewDecoder(r.Body).Decode(&body)

	switch r.Method {
	case http.MethodPut:
		f.nextID++
		body[".id"] = fmt.Sprintf("*%d", f.nextID)
		before, _ := body["place-before"].(string)
		delete(body, "place-before")
		list := f.sections[path]
		at := len(list)
		for i, e := range list {
			if e[".id"] == before {
				at = i
			}
		}
		list = append(list[:at], append([]map[string]any{body}, list[at:]...)...)
		f.sections[path] = list
	case http.MethodPatch, http.MethodDelete:
		section, id := path[:strings.LastIndex(path, "/")], path[strings.LastIndex(path, "/")+1:]
		list := f.sections[section]
		for i, e := range list {
			if e[".id"] != id {
				continue
			}
			if r.Method == http.MethodDelete {
				f.sections[section] = append(list[:i], list[i+1:]...)
			} else {
				for k, v := range body {
					e[k] = v
				}
			}
		}
	}
	w.Write([]byte(`{}`))
}

func (f *fakeRouter) set(section string, entries []map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sections[section] = entries
}

func (f *fakeRouter) writeLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.writes...)
}

const filter = "ip/firewall/filter"

// vrrpAs is one vrrp-lan instance in role ("master" or "backup") as the poller
// decodes the device's payload: that role's flag is "true" and the other
// flag is absent, never "false" (docs/lab-rest-contract.md).
func vrrpAs(role string) []routeros.VRRPInstance {
	v := routeros.VRRPInstance{Name: "vrrp-lan", Interface: "ether2"}
	switch role {
	case "master":
		v.Master = "true"
	case "backup":
		v.Backup = "true"
	}
	return []routeros.VRRPInstance{v}
}

// applyFixture is a pair where router A has an "allow-dns" rule router B
// lacks, with the drift hunk for it already selected A→B. bRole is router
// B's VRRP role; A is master.
func applyFixture(t *testing.T, writeMode bool, bRole string) (Model, *fakeRouter, *fakeRouter) {
	t.Helper()
	ra := newFakeRouter(map[string][]map[string]any{filter: {
		{".id": "*1", "chain": "input", "comment": "allow-dns", "action": "accept"},
		{".id": "*2", "chain": "input", "comment": "drop-rest", "action": "drop"},
	}})
	rb := newFakeRouter(map[string][]map[string]any{filter: {
		{".id": "*7", "chain": "input", "comment": "drop-rest", "action": "drop"},
	}})

	pollers := map[poll.RouterKey]*poll.Poller{
		"a": poll.New("a", testClient(t, ra), pollInterval),
		"b": poll.New("b", testClient(t, rb), pollInterval),
	}
	pair := &config.Pair{Name: "core", Sync: config.SyncConfig{Sections: []string{filter}}}
	m := New(pair, writeMode, pollers)

	m.snapshots["a"] = poll.Snapshot{Router: "a", VRRP: vrrpAs("master")}
	m.snapshots["b"] = poll.Snapshot{Router: "b", VRRP: vrrpAs(bRole)}
	m.setSelection(filter, plan.HunkRef{Identity: "allow-dns"}, plan.AtoB)
	return m, ra, rb
}

// drive feeds msg to the model and then runs every resulting command
// synchronously until the model settles, standing in for the Bubble Tea
// runtime.
func drive(t *testing.T, m Model, msg tea.Msg) Model {
	t.Helper()
	next, cmd := m.Update(msg)
	m = next.(Model)
	for cmd != nil {
		next, cmd = m.Update(cmd())
		m = next.(Model)
	}
	return m
}

func key(s string) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func TestApplyEntryBuildsDryRunWithoutWriting(t *testing.T) {
	m, ra, rb := applyFixture(t, true, "backup")

	m = drive(t, m, key("4"))

	if m.apply.stage != applyReview {
		t.Fatalf("stage = %v, want review (err %v)", m.apply.stage, m.apply.err)
	}
	want := []string{"POST /system/backup/save", "PUT /ip/firewall/filter"}
	if len(m.apply.plan.Ops) != 2 || m.apply.plan.Ops[0].Path != "/system/backup/save" || m.apply.plan.Ops[1].Body["place-before"] != "*7" {
		t.Fatalf("unexpected plan, want %v:\n%s", want, m.apply.plan.Render())
	}
	if w := append(ra.writeLog(), rb.writeLog()...); len(w) != 0 {
		t.Fatalf("building the dry run wrote to a router: %v", w)
	}
	if !strings.Contains(renderApply(m), "PUT /ip/firewall/filter") {
		t.Error("dry run should list the PUT operation")
	}
}

func TestApplyReadOnlyRefusesToWrite(t *testing.T) {
	m, _, rb := applyFixture(t, false, "backup")

	m = drive(t, m, key("4"))
	m = drive(t, m, key("y"))

	if m.apply.stage != applyReview || !strings.Contains(m.apply.notice, "read-only") {
		t.Fatalf("stage = %v notice = %q, want review with a read-only notice", m.apply.stage, m.apply.notice)
	}
	if w := rb.writeLog(); len(w) != 0 {
		t.Fatalf("read-only session wrote: %v", w)
	}
	if !strings.Contains(renderApply(m), "read-only") {
		t.Error("status bar should show read-only mode")
	}
}

// Backup first, then the planned op, then drift re-verification reports the
// section clean and the resolved selection is dropped.
func TestApplyBacksUpAppliesAndVerifies(t *testing.T) {
	m, ra, rb := applyFixture(t, true, "backup")

	m = drive(t, m, key("4"))
	m = drive(t, m, key("y"))

	if m.apply.stage != applyDone || m.apply.err != nil {
		t.Fatalf("stage = %v err = %v, want done without error", m.apply.stage, m.apply.err)
	}
	want := []string{"POST /system/backup/save", "PUT /ip/firewall/filter"}
	if got := rb.writeLog(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("router b writes = %v, want %v", got, want)
	}
	if w := ra.writeLog(); len(w) != 0 {
		t.Fatalf("router a (source) must not be written: %v", w)
	}
	if sd := m.apply.residual[filter]; !sd.Clean() {
		t.Fatalf("expected no residual drift, got %+v", sd.Hunks)
	}
	if m.selectedCount() != 0 {
		t.Errorf("resolved selection should be pruned, %d left", m.selectedCount())
	}
	// place-before put the rule ahead of drop-rest on router b.
	if first := rb.sections[filter][0]["comment"]; first != "allow-dns" {
		t.Errorf("rule order on b: first rule is %v, want allow-dns", first)
	}
}

// Writing to the router that holds VRRP master takes y and then a distinct
// Y; a second y alone must not write.
func TestApplyToMasterRequiresSecondConfirmation(t *testing.T) {
	m, _, rb := applyFixture(t, true, "master")

	m = drive(t, m, key("4"))
	m = drive(t, m, key("y"))
	if m.apply.stage != applyConfirmMaster {
		t.Fatalf("stage = %v, want master confirmation", m.apply.stage)
	}
	m = drive(t, m, key("y"))
	if w := rb.writeLog(); len(w) != 0 || m.apply.stage != applyConfirmMaster {
		t.Fatalf("second y must not write; stage %v writes %v", m.apply.stage, w)
	}

	m = drive(t, m, key("n"))
	if m.apply.stage != applyReview {
		t.Fatalf("n should cancel back to review, stage = %v", m.apply.stage)
	}

	m = drive(t, m, key("y"))
	m = drive(t, m, key("Y"))
	if m.apply.stage != applyDone || len(rb.writeLog()) != 2 {
		t.Fatalf("after Y: stage %v writes %v", m.apply.stage, rb.writeLog())
	}
}

// An unpolled target router counts as a possible master.
func TestApplyUnknownVRRPStateRequiresSecondConfirmation(t *testing.T) {
	m, _, _ := applyFixture(t, true, "backup")
	delete(m.snapshots, "b")

	m = drive(t, m, key("4"))
	m = drive(t, m, key("y"))
	if m.apply.stage != applyConfirmMaster {
		t.Fatalf("stage = %v, want master confirmation for unknown VRRP state", m.apply.stage)
	}
}

// The master check fails closed on the VRRP payload itself: only a target
// whose every entry positively decodes as backup gets a single-y apply.
// Payloads are decoded as the poller would decode them.
//
// The first three are what RouterOS 7.23.7 sends (docs/lab-rest-contract.md):
// one role flag, "true", the other key absent, and running:"false" on the
// backup. The rest are deliberately hypothetical — no device capture backs
// them — and keep the defensive paths covered: an entry with neither flag
// (a disabled or init instance could plausibly look like this, but that was
// not observable read-only), and the vrrp-state fallback, a field the
// device does not send.
func TestApplyMasterCheckFailsClosedOnVRRPPayload(t *testing.T) {
	const (
		master = `{".id":"*4","name":"vrrp-lan","interface":"ether2","vrid":"1","priority":"200","disabled":"false","invalid":"false","running":"true","master":"true"}`
		backup = `{".id":"*4","name":"vrrp-lan","interface":"ether2","vrid":"1","priority":"100","disabled":"false","invalid":"false","running":"false","backup":"true"}`
	)
	cases := []struct {
		name        string
		payload     string
		wantConfirm bool
	}{
		{"device master", "[" + master + "]", true},
		{"device backup", "[" + backup + "]", false},
		{"no vrrp entries", `[]`, true},

		{"hypothetical: no role reported", `[{"name":"vrrp-lan","running":"false","disabled":"true"}]`, true},
		{"hypothetical: one entry unknown", "[" + backup + `,{"name":"vrrp-wan","running":"false","disabled":"true"}]`, true},
		{"hypothetical: vrrp-state backup", `[{"name":"vrrp-lan","vrrp-state":"backup"}]`, false},
		{"hypothetical: vrrp-state init", "[" + backup + `,{"name":"vrrp-wan","vrrp-state":"init"}]`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, _, rb := applyFixture(t, true, "backup")
			var vrrp []routeros.VRRPInstance
			if err := json.Unmarshal([]byte(tc.payload), &vrrp); err != nil {
				t.Fatal(err)
			}
			m.snapshots["b"] = poll.Snapshot{Router: "b", VRRP: vrrp}

			m = drive(t, m, key("4"))
			m = drive(t, m, key("y"))

			if tc.wantConfirm {
				if m.apply.stage != applyConfirmMaster || len(rb.writeLog()) != 0 {
					t.Fatalf("stage = %v writes = %v, want the Y confirmation and nothing written", m.apply.stage, rb.writeLog())
				}
				return
			}
			if m.apply.stage != applyDone || len(rb.writeLog()) != 2 {
				t.Fatalf("stage = %v writes = %v, want a single-y apply to a positive backup", m.apply.stage, rb.writeLog())
			}
		})
	}
}

// ip/service on the device (docs/lab-rest-contract.md) is not a fixed list:
// alongside the static built-ins it returns dynamic rows, including one
// reverse-proxy row per open HTTPS connection whose .id and remote change on
// every request. Those must be dropped before diffing, or mtha's own polling
// would show up as drift that never resolves. telnet is *0 on the device, a
// real id the PATCH must address.
func TestApplyIgnoresDynamicServiceRows(t *testing.T) {
	static := func(id, name, port, disabled string) map[string]any {
		return map[string]any{".id": id, "name": name, "port": port, "proto": "tcp", "address": "",
			"disabled": disabled, "dynamic": "false", "invalid": "false", "max-sessions": "20", "vrf": "main"}
	}
	dynamic := func(id, name, port string) map[string]any {
		return map[string]any{".id": id, "name": name, "port": port, "proto": "tcp",
			"disabled": "false", "dynamic": "true", "invalid": "false"}
	}
	const svc = "ip/service"
	ra := newFakeRouter(map[string][]map[string]any{svc: {
		static("*0", "telnet", "23", "true"),
		static("*2", "ssh", "22", "false"),
		dynamic("*B", "btest", "2000"),
	}})
	rb := newFakeRouter(map[string][]map[string]any{svc: {
		static("*0", "telnet", "23", "false"),
		static("*2", "ssh", "22", "false"),
		dynamic("*C", "dhcpclient", "68"),
	}})
	// Each GET sees a fresh connection row, as the REST client's own request
	// is listed in the response it reads. The .id counter only increases.
	conn := 0x10
	connections := func(section string) []map[string]any {
		if section != svc {
			return nil
		}
		conn++
		row := dynamic(fmt.Sprintf("*%X", conn), "reverse-proxy", "443")
		row["connection"] = "true"
		row["local"] = "172.17.0.2:443"
		row["remote"] = fmt.Sprintf("172.17.0.1:%d", 50000+conn)
		return []map[string]any{row}
	}
	ra.perGet, rb.perGet = connections, connections

	pollers := map[poll.RouterKey]*poll.Poller{
		"a": poll.New("a", testClient(t, ra), pollInterval),
		"b": poll.New("b", testClient(t, rb), pollInterval),
	}
	pair := &config.Pair{Name: "core", Sync: config.SyncConfig{Sections: []string{svc}}}
	m := New(pair, true, pollers)
	m.snapshots["a"] = poll.Snapshot{Router: "a", VRRP: vrrpAs("master")}
	m.snapshots["b"] = poll.Snapshot{Router: "b", VRRP: vrrpAs("backup")}
	m.setSelection(svc, plan.HunkRef{Identity: "telnet"}, plan.AtoB)

	m = drive(t, m, key("4"))
	if m.apply.stage != applyReview {
		t.Fatalf("stage = %v, want review (err %v)", m.apply.stage, m.apply.err)
	}
	if ops := m.apply.plan.Ops; len(ops) != 2 || ops[1].Path != "/ip/service/*0" || ops[1].Body["disabled"] != "true" || len(ops[1].Body) != 1 {
		t.Fatalf("want the backup and one PATCH of telnet's disabled at *0:\n%s", m.apply.plan.Render())
	}
	if len(m.apply.plan.Skipped) != 0 {
		t.Fatalf("dynamic rows should not reach the plan:\n%s", m.apply.plan.Render())
	}

	m = drive(t, m, key("y"))
	want := []string{"POST /system/backup/save", "PATCH /ip/service/*0"}
	if got := rb.writeLog(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("router b writes = %v, want %v", got, want)
	}
	if sd := m.apply.residual[svc]; !sd.Clean() {
		t.Fatalf("dynamic service rows read as drift: %+v", sd.Hunks)
	}
}

// If the routers change between showing the plan and confirming it, the
// pre-run recheck shows the new plan instead of running the old one.
func TestApplyRecheckRefusesStalePlan(t *testing.T) {
	m, _, rb := applyFixture(t, true, "backup")

	m = drive(t, m, key("4"))
	rb.set(filter, []map[string]any{
		{".id": "*8", "chain": "input", "comment": "drop-rest", "action": "drop"},
	})
	m = drive(t, m, key("y"))

	if m.apply.stage != applyReview || !strings.Contains(m.apply.notice, "changed") {
		t.Fatalf("stage = %v notice = %q, want review with a changed-state notice", m.apply.stage, m.apply.notice)
	}
	if w := rb.writeLog(); len(w) != 0 {
		t.Fatalf("stale plan was executed: %v", w)
	}
	if m.apply.plan.Ops[1].Body["place-before"] != "*8" {
		t.Errorf("updated plan should target the new .id:\n%s", m.apply.plan.Render())
	}
}

// A failed backup stops the run before any config write.
func TestApplyStopsWhenBackupFails(t *testing.T) {
	m, _, rb := applyFixture(t, true, "backup")
	rb.failPath = "/system/backup/save"

	m = drive(t, m, key("4"))
	m = drive(t, m, key("y"))

	if m.apply.stage != applyDone || m.apply.err == nil {
		t.Fatalf("stage = %v err = %v, want done with an error", m.apply.stage, m.apply.err)
	}
	if got := rb.writeLog(); len(got) != 1 || got[0] != "POST /system/backup/save" {
		t.Fatalf("writes = %v, want only the failed backup", got)
	}
	if m.apply.status[0] != opFailed || m.apply.status[1] != opPending {
		t.Errorf("status = %v, want [failed pending]", m.apply.status)
	}
	if sd := m.apply.residual[filter]; sd.Clean() {
		t.Error("verification should still report the unresolved hunk")
	}
}

// The Apply screen passes each router's configured REST user to the
// planner, so a hunk that would delete the user mtha logs in to the target
// as is skipped rather than planned.
func TestApplyRefusesToDeleteTargetAPIUser(t *testing.T) {
	ra := newFakeRouter(map[string][]map[string]any{"user": {}})
	rb := newFakeRouter(map[string][]map[string]any{"user": {
		{".id": "*1", "name": "api-b", "group": "full"},
	}})
	pollers := map[poll.RouterKey]*poll.Poller{
		"a": poll.New("a", testClient(t, ra), pollInterval),
		"b": poll.New("b", testClient(t, rb), pollInterval),
	}
	pair := &config.Pair{
		Name:    "core",
		Routers: map[string]config.RouterConfig{"a": {User: "api-a"}, "b": {User: "api-b"}},
		Sync:    config.SyncConfig{Sections: []string{"user"}},
	}
	m := New(pair, true, pollers)
	m.setSelection("user", plan.HunkRef{Identity: "api-b"}, plan.AtoB)

	m = drive(t, m, key("4"))

	if !m.apply.plan.Empty() || len(m.apply.plan.Skipped) != 1 || !strings.Contains(m.apply.plan.Skipped[0].Reason, "lock mtha out") {
		t.Fatalf("want the delete skipped as a lockout:\n%s", m.apply.plan.Render())
	}
}

// A finished apply is journalled for the Events timeline: one sync action
// per target router, with its op count, whether it succeeded or stopped.
func TestApplyIsRecordedOnTimeline(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		m, _, _ := applyFixture(t, true, "backup")
		m = drive(t, m, key("4"))
		m = drive(t, m, key("y"))

		got := m.journal.Actions()
		if len(got) != 1 {
			t.Fatalf("actions = %+v, want one", got)
		}
		a := got[0]
		if a.Kind != events.ActionSync || a.Target != "b" || a.Summary != "apply ip/firewall/filter: 2/2 ops" || a.Err != nil {
			t.Errorf("action = %+v", a)
		}
	})

	t.Run("stopped", func(t *testing.T) {
		m, _, rb := applyFixture(t, true, "backup")
		rb.failPath = "/system/backup/save"
		m = drive(t, m, key("4"))
		m = drive(t, m, key("y"))

		got := m.journal.Actions()
		if len(got) != 1 || got[0].Summary != "apply ip/firewall/filter: 0/2 ops" || got[0].Err == nil {
			t.Fatalf("actions = %+v, want one failed sync action", got)
		}
		if tl := m.timeline(); len(tl) != 1 || !tl[0].Failed || tl[0].Kind != string(events.ActionSync) {
			t.Errorf("timeline = %+v", tl)
		}
	})

	t.Run("not confirmed", func(t *testing.T) {
		m, _, _ := applyFixture(t, false, "backup")
		m = drive(t, m, key("4"))
		m = drive(t, m, key("y"))
		if got := m.journal.Actions(); len(got) != 0 {
			t.Errorf("actions = %+v, want none for a plan that never ran", got)
		}
	})
}

func TestApplyWithoutSelectionExplainsHowToSelect(t *testing.T) {
	m, _, _ := applyFixture(t, true, "backup")
	m.driftSelected = nil

	m = drive(t, m, key("4"))

	if m.apply.stage != applyIdle || !strings.Contains(renderApply(m), "no hunks selected") {
		t.Fatalf("stage = %v, view:\n%s", m.apply.stage, renderApply(m))
	}
}

func TestScrollWindowClipsAndMarksHiddenLines(t *testing.T) {
	lines := []string{"1", "2", "3", "4", "5"}
	got := scrollWindow(lines, 10, 3)
	if len(got) != 3 || got[0] != "3" || got[2] != "5" {
		t.Fatalf("offset past the end should clamp to the last page, got %v", got)
	}
	got = scrollWindow(lines, 0, 3)
	if len(got) != 3 || !strings.Contains(got[2], "more line") {
		t.Fatalf("expected a more-lines marker, got %v", got)
	}
}
