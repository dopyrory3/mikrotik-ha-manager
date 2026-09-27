package ui

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"mtha/internal/config"
	"mtha/internal/events"
	"mtha/internal/plan"
	"mtha/internal/poll"
	"mtha/internal/routeros"
	"mtha/internal/runtime"
)

func eventsMux(logBody string) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/rest/log", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, logBody)
	})
	mux.HandleFunc("/rest/system/clock", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"date":"2026-09-27","time":"12:00:00","gmt-offset":"+00:00"}`)
	})
	return mux
}

// One router failing must not discard the other's log (see fetchEvents).
func TestFetchEventsReturnsPartialResultsOnRouterError(t *testing.T) {
	good := testClient(t, eventsMux(`[{".id":"*1","time":"11:59:00","topics":"vrrp,info","message":"vrrp-lan now MASTER"}]`))
	bad := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))

	msg := fetchEvents(context.Background(), map[poll.RouterKey]*routeros.Client{"a": good, "b": bad})

	if len(msg.logs["a"].Events) != 1 {
		t.Errorf("router a events = %+v, want 1", msg.logs["a"].Events)
	}
	if _, ok := msg.logs["b"]; ok {
		t.Error("failed router b should have no log entry")
	}
	if msg.errs["b"] == nil || msg.errs["a"] != nil {
		t.Errorf("errs = %v, want only router b", msg.errs)
	}
}

func eventsTestModel() Model {
	m := New(&config.Pair{Name: "core"}, false, nil)
	m.screen = screenEvents
	m.width, m.height = 100, 24
	return m
}

func TestApplyEventsResultKeepsPreviousLogOnError(t *testing.T) {
	m := eventsTestModel()
	first := routeros.EventLog{Events: []routeros.LogEvent{{LogEntry: routeros.LogEntry{Message: "old b"}}}}
	m = m.applyEventsResult(eventsResultMsg{logs: map[poll.RouterKey]routeros.EventLog{"b": first}})

	m = m.applyEventsResult(eventsResultMsg{
		logs: map[poll.RouterKey]routeros.EventLog{"a": {}},
		errs: map[poll.RouterKey]error{"b": errors.New("timeout")},
	})

	if got := m.eventsLogs["b"].Events; len(got) != 1 || got[0].Message != "old b" {
		t.Errorf("router b log = %+v, want the previous read kept", got)
	}
	if m.eventsErrs["b"] == nil {
		t.Error("router b error not recorded")
	}
	if m.eventsFetching {
		t.Error("eventsFetching still set after a result")
	}
}

// Runtime deploy/remove run through the Apply pipeline but stay labelled as
// runtime actions on the timeline, with a failed router's error.
func TestRuntimeActionIsRecordedOnTimeline(t *testing.T) {
	m := eventsTestModel()
	m.apply = applyState{
		kind:  applyRuntimeRemove,
		stage: applyVerifying,
		plan: plan.Plan{Ops: []plan.Op{
			plan.BackupOp("a", "bk"),
			{Router: "a", Method: plan.MethodDelete, Path: "/tool/netwatch/*1", Section: "tool/netwatch"},
		}},
		status: []opStatus{opDone, opFailed},
		err:    errors.New("boom"),
	}
	updated, _ := m.Update(applyVerifyMsg{runtimeStatus: runtime.Status{}})
	m = updated.(Model)

	tl := m.timeline()
	if len(tl) != 1 {
		t.Fatalf("timeline = %+v, want one tool event", tl)
	}
	e := tl[0]
	if e.Source != events.SourceTool || e.Kind != string(events.ActionRuntime) || !e.Failed {
		t.Errorf("event = %+v", e)
	}
	if e.Message != "→ A: remove runtime logic: 1/2 ops: boom" {
		t.Errorf("message = %q", e.Message)
	}
}

func TestRenderEventsShowsNewestFirst(t *testing.T) {
	m := eventsTestModel()
	base := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	m = m.applyEventsResult(eventsResultMsg{logs: map[poll.RouterKey]routeros.EventLog{
		"a": {Events: []routeros.LogEvent{{LogEntry: routeros.LogEntry{Message: "older-on-a"}, Kind: routeros.LogKindVRRP, At: base}}},
		"b": {Events: []routeros.LogEvent{{LogEntry: routeros.LogEntry{Message: "newer-on-b"}, Kind: routeros.LogKindVRRP, At: base.Add(time.Minute)}}},
	}})
	m.journal.Record(events.Action{At: base.Add(2 * time.Minute), Kind: events.ActionSync, Summary: "newest-tool"})

	out := renderEvents(m)
	iTool, iB, iA := strings.Index(out, "newest-tool"), strings.Index(out, "newer-on-b"), strings.Index(out, "older-on-a")
	if iTool < 0 || iB < 0 || iA < 0 {
		t.Fatalf("missing rows in:\n%s", out)
	}
	if !(iTool < iB && iB < iA) {
		t.Errorf("rows not newest first:\n%s", out)
	}
}

func TestEventsScrollIsClamped(t *testing.T) {
	m := eventsTestModel()
	m.height = eventsOverhead + 2 // two visible rows
	for i := 0; i < 5; i++ {
		m.journal.Record(events.Action{Kind: events.ActionSync, Summary: "x"})
	}

	press := func(k string) {
		updated, _ := m.handleEventsKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)})
		m = updated.(Model)
	}

	press("k")
	if m.eventsScroll != 0 {
		t.Errorf("scroll above top = %d, want 0", m.eventsScroll)
	}
	press("G")
	if m.eventsScroll != 3 {
		t.Errorf("G scroll = %d, want 3 (5 rows, 2 visible)", m.eventsScroll)
	}
	press("j")
	if m.eventsScroll != 3 {
		t.Errorf("scroll past bottom = %d, want 3", m.eventsScroll)
	}
	press("g")
	if m.eventsScroll != 0 {
		t.Errorf("g scroll = %d, want 0", m.eventsScroll)
	}
}

func TestTruncateRunes(t *testing.T) {
	if got := truncateRunes("héllo", 10); got != "héllo" {
		t.Errorf("short string changed: %q", got)
	}
	if got := truncateRunes("héllo world", 5); got != "héll…" {
		t.Errorf("truncateRunes = %q, want héll…", got)
	}
}
