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

func TestEnsureCreatesMissing(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody map[string]string

	mux := http.NewServeMux()
	mux.HandleFunc("/rest/tool/netwatch", func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		if r.Method == http.MethodGet {
			io.WriteString(w, `[]`)
			return
		}
		json.NewDecoder(r.Body).Decode(&gotBody)
		io.WriteString(w, `{}`)
	})
	client := testClient(t, mux)

	state, err := ensure(context.Background(), client, simpleOp())
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if state != StateOK {
		t.Errorf("state = %v, want OK", state)
	}
	if gotMethod != http.MethodPut {
		t.Errorf("create method = %s, want PUT", gotMethod)
	}
	if gotPath != "/rest/tool/netwatch" {
		t.Errorf("create path = %s, want /rest/tool/netwatch", gotPath)
	}
	if gotBody["host"] != "1.1.1.1" {
		t.Errorf("create body = %v, want host=1.1.1.1", gotBody)
	}
}

func TestEnsureNoopWhenAlreadyMatching(t *testing.T) {
	writeCalled := false
	mux := http.NewServeMux()
	mux.HandleFunc("/rest/tool/netwatch", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			io.WriteString(w, `[{".id":"*1","host":"1.1.1.1","comment":"mtha:netwatch:1.1.1.1"}]`)
			return
		}
		writeCalled = true
	})
	client := testClient(t, mux)

	state, err := ensure(context.Background(), client, simpleOp())
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if state != StateOK {
		t.Errorf("state = %v, want OK", state)
	}
	if writeCalled {
		t.Error("ensure wrote when the entry already matched")
	}
}

func TestEnsurePatchesMismatchedField(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody map[string]string

	mux := http.NewServeMux()
	mux.HandleFunc("/rest/tool/netwatch/", func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		json.NewDecoder(r.Body).Decode(&gotBody)
		io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/rest/tool/netwatch", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `[{".id":"*1","host":"1.2.3.4","comment":"mtha:netwatch:1.1.1.1"}]`)
	})
	client := testClient(t, mux)

	state, err := ensure(context.Background(), client, simpleOp())
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if state != StateOK {
		t.Errorf("state = %v, want OK", state)
	}
	if gotMethod != http.MethodPatch {
		t.Errorf("patch method = %s, want PATCH", gotMethod)
	}
	if gotPath != "/rest/tool/netwatch/*1" {
		t.Errorf("patch path = %s, want /rest/tool/netwatch/*1", gotPath)
	}
	if gotBody["host"] != "1.1.1.1" {
		t.Errorf("patch body = %v, want host corrected to 1.1.1.1", gotBody)
	}
	if _, ok := gotBody["comment"]; ok {
		t.Errorf("patch body = %v, comment already matched and shouldn't be re-sent", gotBody)
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

func TestEnsureLeavesForeignGuardedFieldAlone(t *testing.T) {
	var gotBody map[string]string
	mux := http.NewServeMux()
	mux.HandleFunc("/rest/interface/vrrp/", func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotBody)
		io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/rest/interface/vrrp", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `[{".id":"*1","name":"vrrp-lan","priority":"100","on-master":":log info \"hand written\""}]`)
	})
	client := testClient(t, mux)

	state, err := ensure(context.Background(), client, vrrpOpWithGuard())
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if state != StateConflict {
		t.Errorf("state = %v, want Conflict", state)
	}
	if _, ok := gotBody["on-master"]; ok {
		t.Errorf("patch body = %v, must not overwrite a foreign on-master script", gotBody)
	}
	if gotBody["priority"] != "200" {
		t.Errorf("patch body = %v, want the non-conflicting priority field still patched", gotBody)
	}
}

func TestEnsureTreatsOwnScriptAsNoConflict(t *testing.T) {
	writeCalled := false
	mux := http.NewServeMux()
	mux.HandleFunc("/rest/interface/vrrp/", func(w http.ResponseWriter, r *http.Request) {
		writeCalled = true
		io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/rest/interface/vrrp", func(w http.ResponseWriter, r *http.Request) {
		body, _ := json.Marshal(map[string]string{
			".id": "*1", "name": "vrrp-lan", "priority": "200",
			"on-master": onMasterScript("vrrp-lan", config.TogglesConfig{}),
		})
		w.Write([]byte("[" + string(body) + "]"))
	})
	client := testClient(t, mux)

	state, err := ensure(context.Background(), client, vrrpOpWithGuard())
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if state != StateOK {
		t.Errorf("state = %v, want OK when the current script is already mtha's own", state)
	}
	if writeCalled {
		t.Error("ensure wrote when everything already matched")
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
// uplink is down) and verify reports it OK.
func TestEnsureLeavesMutablePriorityAlone(t *testing.T) {
	cases := []struct {
		name      string
		priority  string
		wantState State
	}{
		{"degraded", "50", StateOK},
		{"base", "200", StateOK},
		{"neither", "150", StateMismatched},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var patched map[string]string
			mux := http.NewServeMux()
			mux.HandleFunc("/rest/tool/netwatch/", func(w http.ResponseWriter, r *http.Request) {
				json.NewDecoder(r.Body).Decode(&patched)
				io.WriteString(w, `{}`)
			})
			mux.HandleFunc("/rest/tool/netwatch", func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, `[{".id":"*1","host":"9.9.9.9","comment":"mtha:netwatch:1.1.1.1","priority":"`+tc.priority+`"}]`)
			})
			client := testClient(t, mux)

			state, _, err := check(context.Background(), client, mutablePriorityOp())
			if err != nil {
				t.Fatalf("check: %v", err)
			}
			if state != StateMismatched {
				t.Errorf("check state = %v, want mismatched (host differs)", state)
			}

			state, err = ensure(context.Background(), client, mutablePriorityOp())
			if err != nil {
				t.Fatalf("ensure: %v", err)
			}
			if _, ok := patched["priority"]; ok {
				t.Errorf("patch body = %v, must never patch a mutable field on an existing entry", patched)
			}
			if patched["host"] != "1.1.1.1" {
				t.Errorf("patch body = %v, want the static field still corrected", patched)
			}
			if state != tc.wantState {
				t.Errorf("ensure state = %v, want %v", state, tc.wantState)
			}
		})
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

func TestRemoveDeletesMatchedEntry(t *testing.T) {
	var deletedPath string
	mux := http.NewServeMux()
	mux.HandleFunc("/rest/tool/netwatch/", func(w http.ResponseWriter, r *http.Request) {
		deletedPath = r.URL.Path
	})
	mux.HandleFunc("/rest/tool/netwatch", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `[{".id":"*1","host":"1.1.1.1","comment":"mtha:netwatch:1.1.1.1"}]`)
	})
	client := testClient(t, mux)

	plans := map[string]Plan{
		"a": {Router: "a", Ops: []Op{simpleOp()}},
		"b": {Router: "b"},
	}
	result := Remove(context.Background(), client, client, plans)
	if result.Err != nil {
		t.Fatalf("Remove: %v", result.Err)
	}
	if deletedPath != "/rest/tool/netwatch/*1" {
		t.Errorf("deleted path = %s, want /rest/tool/netwatch/*1", deletedPath)
	}
	if result.Status["a"][0].State != StateMissing {
		t.Errorf("post-remove state = %v, want Missing", result.Status["a"][0].State)
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

// A pre-toggles (log-only) mtha script carries the marker, so enabling
// runtime.toggles later upgrades it in place rather than reporting conflict.
func TestEnsureUpgradesOwnLogOnlyScriptToToggles(t *testing.T) {
	var gotBody map[string]string
	mux := http.NewServeMux()
	mux.HandleFunc("/rest/interface/vrrp/", func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotBody)
		io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/rest/interface/vrrp", func(w http.ResponseWriter, r *http.Request) {
		body, _ := json.Marshal(map[string]string{
			".id": "*1", "name": "vrrp-lan", "priority": "200",
			"on-master": onMasterScript("vrrp-lan", config.TogglesConfig{}),
		})
		w.Write([]byte("[" + string(body) + "]"))
	})
	client := testClient(t, mux)

	op := vrrpOpWithGuard()
	want := onMasterScript("vrrp-lan", config.TogglesConfig{VRRP: "vrrp-lan", DHCPServers: []string{"dhcp-lan"}})
	op.Fields["on-master"] = want

	state, err := ensure(context.Background(), client, op)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if state != StateOK {
		t.Errorf("state = %v, want OK", state)
	}
	if gotBody["on-master"] != want {
		t.Errorf("patch body = %v, want on-master upgraded to the toggling script", gotBody)
	}
}

// A hand-made VRRP interface with the name mtha would use, but without its
// tag, is not mtha's: verify reports it as a conflict, deploy neither
// adopts nor patches it, and remove never deletes it.
func TestUntaggedSameNamedInterfaceIsNeverTouched(t *testing.T) {
	var writes []string
	mux := http.NewServeMux()
	mux.HandleFunc("/rest/interface/vrrp", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writes = append(writes, r.Method+" "+r.URL.Path)
		}
		io.WriteString(w, `[{".id":"*5","name":"vrrp-lan","comment":"hand-made, production","priority":"150"}]`)
	})
	mux.HandleFunc("/rest/interface/vrrp/", func(w http.ResponseWriter, r *http.Request) {
		writes = append(writes, r.Method+" "+r.URL.Path)
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

	if res := Deploy(context.Background(), client, client, plans); res.Err != nil || res.Status["a"][0].State != StateConflict {
		t.Errorf("deploy = %+v, want conflict without error", res)
	}
	if res := Remove(context.Background(), client, client, plans); res.Err != nil || res.Status["a"][0].State != StateConflict {
		t.Errorf("remove = %+v, want conflict without error", res)
	}
	if len(writes) != 0 {
		t.Errorf("writes = %v, want none to an interface mtha didn't create", writes)
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
