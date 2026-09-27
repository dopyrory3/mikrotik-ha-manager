package runtime

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"mtha/internal/config"
	"mtha/internal/model"
	"mtha/internal/plan"
	"mtha/internal/routeros"
)

func testClient(t *testing.T, handler http.Handler) *routeros.Client {
	t.Helper()
	srv := httptest.NewTLSServer(handler)
	t.Cleanup(srv.Close)
	return routeros.New(routeros.Config{
		Host:        strings.TrimPrefix(srv.URL, "https://"),
		User:        "mtha",
		Password:    "pw",
		InsecureTLS: true,
		Timeout:     2 * time.Second,
	})
}

func simpleOp() Op {
	return Op{
		Section:    "tool/netwatch",
		MatchField: "comment",
		MatchValue: "mtha:netwatch:1.1.1.1",
		Label:      "netwatch 1.1.1.1",
		Fields: map[string]string{
			"host":    "1.1.1.1",
			"comment": "mtha:netwatch:1.1.1.1",
		},
	}
}

// entries decodes a GET /rest/<section> payload the way GetSection does.
func entries(t *testing.T, payload string) []model.Entry {
	t.Helper()
	var out []model.Entry
	if err := json.Unmarshal([]byte(payload), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestDeployWritesCreatesMissing(t *testing.T) {
	ops, skips := deployWrites("a", simpleOp(), nil)
	if len(skips) != 0 || len(ops) != 1 {
		t.Fatalf("ops = %v skips = %v, want one create", ops, skips)
	}
	op := ops[0]
	if op.Router != "a" || op.Method != plan.MethodCreate || op.Path != "/tool/netwatch" || op.Body["host"] != "1.1.1.1" {
		t.Errorf("create = %s", op)
	}
}

func TestDeployWritesNothingWhenAlreadyMatching(t *testing.T) {
	ops, skips := deployWrites("a", simpleOp(), entries(t, `[{".id":"*1","host":"1.1.1.1","comment":"mtha:netwatch:1.1.1.1"}]`))
	if len(ops) != 0 || len(skips) != 0 {
		t.Errorf("ops = %v skips = %v, want nothing for a matching entry", ops, skips)
	}
}

func TestDeployWritesPatchesMismatchedField(t *testing.T) {
	ops, _ := deployWrites("a", simpleOp(), entries(t, `[{".id":"*1","host":"1.2.3.4","comment":"mtha:netwatch:1.1.1.1"}]`))
	want := "PATCH /tool/netwatch/*1 {\"host\":\"1.1.1.1\"}"
	if len(ops) != 1 || ops[0].String() != want {
		t.Errorf("ops = %v, want %s (comment already matches, not re-sent)", ops, want)
	}
}

func vrrpOpWithGuard() Op {
	return Op{
		Section:    "interface/vrrp",
		MatchField: "name",
		MatchValue: "vrrp-lan",
		Label:      "vrrp interface vrrp-lan",
		Fields: map[string]string{
			"name":      "vrrp-lan",
			"priority":  "200",
			"on-master": onMasterScript("vrrp-lan", config.TogglesConfig{}),
		},
		Guarded: map[string]string{
			"on-master": onMasterMarker("vrrp-lan"),
		},
	}
}

func TestDeployWritesLeavesForeignGuardedFieldAlone(t *testing.T) {
	ops, skips := deployWrites("a", vrrpOpWithGuard(), entries(t, `[{".id":"*1","name":"vrrp-lan","priority":"100","on-master":":log info \"hand written\""}]`))
	if len(ops) != 1 {
		t.Fatalf("ops = %v, want one patch", ops)
	}
	if _, ok := ops[0].Body["on-master"]; ok {
		t.Errorf("patch body = %v, must not overwrite a foreign on-master script", ops[0].Body)
	}
	if ops[0].Body["priority"] != "200" {
		t.Errorf("patch body = %v, want the non-conflicting priority field still patched", ops[0].Body)
	}
	if len(skips) != 1 || !strings.Contains(skips[0], "on-master") {
		t.Errorf("skips = %v, want the untouched on-master explained", skips)
	}
}

func TestDeployWritesTreatsOwnScriptAsNoConflict(t *testing.T) {
	current, _ := json.Marshal([]map[string]string{{
		".id": "*1", "name": "vrrp-lan", "priority": "200",
		"on-master": onMasterScript("vrrp-lan", config.TogglesConfig{}),
	}})
	ops, skips := deployWrites("a", vrrpOpWithGuard(), entries(t, string(current)))
	if len(ops) != 0 || len(skips) != 0 {
		t.Errorf("ops = %v skips = %v, want nothing when the script is already mtha's own", ops, skips)
	}
}

// A pre-toggles (log-only) mtha script carries the marker, so enabling
// runtime.toggles later upgrades it in place rather than reporting conflict.
func TestDeployWritesUpgradesOwnLogOnlyScriptToToggles(t *testing.T) {
	current, _ := json.Marshal([]map[string]string{{
		".id": "*1", "name": "vrrp-lan", "priority": "200",
		"on-master": onMasterScript("vrrp-lan", config.TogglesConfig{}),
	}})
	op := vrrpOpWithGuard()
	want := onMasterScript("vrrp-lan", config.TogglesConfig{VRRP: "vrrp-lan", DHCPServers: []string{"dhcp-lan"}})
	op.Fields["on-master"] = want

	ops, skips := deployWrites("a", op, entries(t, string(current)))
	if len(ops) != 1 || ops[0].Body["on-master"] != want || len(skips) != 0 {
		t.Errorf("ops = %v skips = %v, want on-master upgraded to the toggling script", ops, skips)
	}
}

func mutablePriorityOp() Op {
	op := simpleOp()
	op.Fields["priority"] = "200"
	op.Mutable = map[string][]string{"priority": {"200", "50"}}
	return op
}

// A degraded router's priority is healthy runtime state: deploy leaves it
// alone (patching it back to base would preempt master onto a router whose
// uplink is down). A priority outside the accepted set isn't patched
// either, but the dry run says so.
func TestDeployWritesLeavesMutablePriorityAlone(t *testing.T) {
	for priority, wantSkip := range map[string]bool{"50": false, "200": false, "150": true} {
		ops, skips := deployWrites("a", mutablePriorityOp(), entries(t,
			`[{".id":"*1","host":"9.9.9.9","comment":"mtha:netwatch:1.1.1.1","priority":"`+priority+`"}]`))
		if len(ops) != 1 || ops[0].String() != `PATCH /tool/netwatch/*1 {"host":"1.1.1.1"}` {
			t.Errorf("priority %s: ops = %v, want only the static field patched", priority, ops)
		}
		if got := len(skips) == 1 && strings.Contains(skips[0], "priority is "+priority); got != wantSkip {
			t.Errorf("priority %s: skips = %v, want skip = %v", priority, skips, wantSkip)
		}
	}
}

func TestCheckAcceptsMutableValues(t *testing.T) {
	for priority, want := range map[string]State{"200": StateOK, "50": StateOK, "150": StateMismatched} {
		mux := http.NewServeMux()
		mux.HandleFunc("/rest/tool/netwatch", func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, `[{".id":"*1","host":"1.1.1.1","comment":"mtha:netwatch:1.1.1.1","priority":"`+priority+`"}]`)
		})
		state, _, err := check(context.Background(), testClient(t, mux), mutablePriorityOp())
		if err != nil {
			t.Fatalf("check: %v", err)
		}
		if state != want {
			t.Errorf("priority %s: state = %v, want %v", priority, state, want)
		}
	}
}

func TestCheckStates(t *testing.T) {
	cases := []struct {
		name string
		body string
		want State
	}{
		{"missing", `[]`, StateMissing},
		{"mismatched", `[{".id":"*1","host":"9.9.9.9","comment":"mtha:netwatch:1.1.1.1"}]`, StateMismatched},
		{"ok", `[{".id":"*1","host":"1.1.1.1","comment":"mtha:netwatch:1.1.1.1"}]`, StateOK},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/rest/tool/netwatch", func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Fatalf("check must not write, got %s", r.Method)
				}
				io.WriteString(w, tc.body)
			})
			client := testClient(t, mux)

			state, _, err := check(context.Background(), client, simpleOp())
			if err != nil {
				t.Fatalf("check: %v", err)
			}
			if state != tc.want {
				t.Errorf("state = %v, want %v", state, tc.want)
			}
		})
	}
}

func TestVerifyToleratesPartialFailure(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/rest/tool/netwatch", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `[{".id":"*1","host":"1.1.1.1","comment":"mtha:netwatch:1.1.1.1"}]`)
	})
	mux.HandleFunc("/rest/system/scheduler", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	client := testClient(t, mux)

	plans := map[string]Plan{
		"a": {Router: "a", Ops: []Op{simpleOp(), schedulerOp()}},
		"b": {Router: "b"},
	}

	status, err := Verify(context.Background(), client, client, plans)
	if err == nil {
		t.Fatal("expected an error from the failing section")
	}
	items := status["a"]
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2 (partial results)", len(items))
	}
	if items[0].State != StateOK {
		t.Errorf("netwatch item state = %v, want OK despite the scheduler failure", items[0].State)
	}
}

func TestRemoveWritesDeletesMatchedEntry(t *testing.T) {
	ops, skips := removeWrites("b", simpleOp(), entries(t, `[{".id":"*1","host":"1.1.1.1","comment":"mtha:netwatch:1.1.1.1"}]`))
	if len(ops) != 1 || ops[0].String() != "DELETE /tool/netwatch/*1" || ops[0].Router != "b" || len(skips) != 0 {
		t.Errorf("ops = %v skips = %v", ops, skips)
	}
	if ops, _ := removeWrites("b", simpleOp(), nil); len(ops) != 0 {
		t.Errorf("ops = %v, want nothing to remove when it isn't there", ops)
	}
}

// Writes reads both routers and plans a deploy: each router's writes start
// with its backup, and the VRRP interface is created before its address.
// Remove deletes in reverse, the address before the interface.
func TestWritesPlansBackupFirstAndRemovesInReverse(t *testing.T) {
	plans, err := BuildPlan(&config.Pair{VRRP: []config.VRRPInstance{fullInstance()}, Runtime: testRuntimeConfig()})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	client := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("Writes must only read, got %s %s", r.Method, r.URL.Path)
		}
		io.WriteString(w, `[]`)
	}))

	p, err := Writes(context.Background(), client, client, plans, ActionDeploy, "bk")
	if err != nil {
		t.Fatalf("Writes: %v", err)
	}
	var got []string
	for _, op := range p.Ops {
		got = append(got, op.Router+" "+string(op.Method)+" "+op.Path)
	}
	want := []string{
		"a POST /system/backup/save", "a PUT /interface/vrrp", "a PUT /ip/address", "a PUT /tool/netwatch", "a PUT /system/scheduler",
		"b POST /system/backup/save", "b PUT /interface/vrrp", "b PUT /ip/address", "b PUT /tool/netwatch", "b PUT /system/scheduler",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("deploy plan =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if p.Ops[1].Body["priority"] != "200" || p.Ops[6].Body["priority"] != "100" {
		t.Errorf("create priorities = %s / %s, want each router's base", p.Ops[1].Body["priority"], p.Ops[6].Body["priority"])
	}

	reads := map[string]map[string][]model.Entry{"a": {
		"interface/vrrp": entries(t, `[{".id":"*1","name":"vrrp-lan","comment":"mtha:vrrp:vrrp-lan"}]`),
		"ip/address":     entries(t, `[{".id":"*2","comment":"mtha:vrrp:vrrp-lan:10.0.0.1/24"}]`),
	}}
	rm := buildWrites(plans, reads, ActionRemove, "bk")
	if r := rm.Render(); !strings.Contains(r, "2. DELETE /ip/address/*2") || !strings.Contains(r, "3. DELETE /interface/vrrp/*1") {
		t.Errorf("remove plan should delete the address before the interface:\n%s", r)
	}
}

func TestWritesFailsOnReadError(t *testing.T) {
	plans := map[string]Plan{"a": {Router: "a", Ops: []Op{simpleOp()}}, "b": {Router: "b"}}
	client := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	if _, err := Writes(context.Background(), client, client, plans, ActionDeploy, "bk"); err == nil {
		t.Error("Writes should fail rather than plan from a partial read")
	}
}

func TestStatusCleanAndFirstIssue(t *testing.T) {
	clean := Status{"a": {{Label: "x", State: StateOK}}, "b": {{Label: "x", State: StateOK}}}
	if !clean.Clean() {
		t.Error("Clean() = false for an all-OK status")
	}
	if clean.FirstIssue() != "" {
		t.Errorf("FirstIssue() = %q, want empty for a clean status", clean.FirstIssue())
	}

	dirty := Status{"a": {{Label: "x", State: StateOK}}, "b": {{Label: "netwatch 1.1.1.1", State: StateMissing}}}
	if dirty.Clean() {
		t.Error("Clean() = true despite a missing item")
	}
	if got := dirty.FirstIssue(); got == "" || !strings.Contains(got, "netwatch 1.1.1.1") {
		t.Errorf("FirstIssue() = %q, want it to name the missing item", got)
	}
}

// A hand-made VRRP interface with the name mtha would use, but without its
// tag, is not mtha's: verify reports it as a conflict, and neither deploy
// nor remove plans any write to it.
func TestUntaggedSameNamedInterfaceIsNeverTouched(t *testing.T) {
	const payload = `[{".id":"*5","name":"vrrp-lan","comment":"hand-made, production","priority":"150"}]`
	mux := http.NewServeMux()
	mux.HandleFunc("/rest/interface/vrrp", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, payload)
	})
	client := testClient(t, mux)
	op := vrrpOp(fullInstance(), "a", testRuntimeConfig())
	plans := map[string]Plan{"a": {Router: "a", Ops: []Op{op}}, "b": {Router: "b"}}

	status, err := Verify(context.Background(), client, client, plans)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if it := status["a"][0]; it.State != StateConflict || it.Note != foreignNote {
		t.Errorf("verify = %+v, want conflict %q", it, foreignNote)
	}

	for _, action := range []Action{ActionDeploy, ActionRemove} {
		p, err := Writes(context.Background(), client, client, plans, action, "bk")
		if err != nil {
			t.Fatalf("Writes: %v", err)
		}
		if !p.Empty() || len(p.Skipped) != 1 || !strings.Contains(p.Skipped[0].Reason, foreignNote) {
			t.Errorf("%s plan = %s, want no writes and the interface reported as not mtha's", action, p.Render())
		}
	}
}

// An interface carrying mtha's tag is found by it, whatever else it holds.
func TestTaggedInterfaceIsMatchedByTag(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/rest/interface/vrrp", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `[{".id":"*1","name":"vrrp-lan","comment":"mtha:vrrp:vrrp-lan","priority":"50"}]`)
	})
	op := vrrpOp(fullInstance(), "a", testRuntimeConfig())

	state, note, err := check(context.Background(), testClient(t, mux), op)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if state != StateMismatched || note != "" {
		t.Errorf("state = %v note = %q, want mismatched (fields differ), not a conflict", state, note)
	}
}
