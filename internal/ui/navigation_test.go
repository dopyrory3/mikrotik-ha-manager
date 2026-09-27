package ui

import (
	"io"
	"net/http"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"mtha/internal/config"
	"mtha/internal/poll"
)

// pollableMux answers the four reads a poll makes.
func pollableMux(identity string) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/rest/system/resource", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"version":"7.15.3"}`)
	})
	mux.HandleFunc("/rest/system/identity", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"name":"`+identity+`"}`)
	})
	mux.HandleFunc("/rest/interface/vrrp", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `[]`)
	})
	mux.HandleFunc("/rest/tool/netwatch", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `[]`)
	})
	return mux
}

// batchCmds runs a tea.Batch command and returns the commands it wraps.
func batchCmds(t *testing.T, cmd tea.Cmd) []tea.Cmd {
	t.Helper()
	if cmd == nil {
		t.Fatal("got a nil command, want a batch")
	}
	batch, ok := cmd().(tea.BatchMsg)
	if !ok {
		t.Fatal("command is not a tea.Batch")
	}
	return batch
}

// Init starts every poller and listens on each; the first snapshots land in
// the model and re-arm the listener, and quitting cancels the pollers'
// context (project.md §6: polling stops with the TUI).
func TestInitStartsPollersAndQuitStopsThem(t *testing.T) {
	pollers := map[poll.RouterKey]*poll.Poller{
		"a": poll.New("a", testClient(t, pollableMux("core-a")), pollInterval),
		"b": poll.New("b", testClient(t, pollableMux("core-b")), pollInterval),
	}
	m := New(&config.Pair{Name: "core"}, false, pollers)

	cmds := batchCmds(t, m.Init())
	if len(cmds) != len(pollers)+1 {
		t.Fatalf("Init batch has %d commands, want one per poller plus the cancel holder", len(cmds))
	}
	for _, cmd := range cmds {
		msg := cmd()
		next, rearm := m.Update(msg)
		m = next.(Model)
		if _, isSnap := msg.(snapshotMsg); isSnap && rearm == nil {
			t.Error("a snapshot did not re-arm its poller's listener")
		}
	}

	for key, want := range map[poll.RouterKey]string{"a": "core-a", "b": "core-b"} {
		snap, ok := m.snapshots[key]
		if !ok {
			t.Fatalf("no snapshot for router %q", key)
		}
		if snap.Identity == nil || snap.Identity.Name != want {
			t.Errorf("router %q identity = %+v, want %s", key, snap.Identity, want)
		}
	}
	if m.ctx == nil || m.cancel == nil {
		t.Fatal("cancelHolderMsg did not reach the model")
	}

	ctx := m.ctx
	next, cmd := m.Update(key("q"))
	m = next.(Model)
	if !m.quitting || cmd == nil {
		t.Fatalf("q: quitting = %v, cmd = %v; want quitting with tea.Quit", m.quitting, cmd)
	}
	if ctx.Err() == nil {
		t.Error("q did not cancel the pollers' context")
	}
	if m.View() != "" {
		t.Errorf("View after quit = %q, want empty", m.View())
	}
}

func TestSnapshotRearmsOnlyKnownPollers(t *testing.T) {
	pollers := map[poll.RouterKey]*poll.Poller{"a": poll.New("a", nil, pollInterval)}
	m := New(&config.Pair{Name: "core"}, false, pollers)

	next, cmd := m.Update(snapshotMsg{Router: "a"})
	m = next.(Model)
	if !m.have("a") || cmd == nil {
		t.Errorf("router a: have = %v, cmd = %v; want stored and re-armed", m.have("a"), cmd)
	}

	// The re-armed command delivers the poller's next snapshot.
	pollers["a"].C <- poll.Snapshot{Router: "a", Err: io.EOF}
	if msg, ok := cmd().(snapshotMsg); !ok || msg.Err != io.EOF {
		t.Errorf("re-armed command returned %#v, want the next snapshot", msg)
	}

	next, cmd = m.Update(snapshotMsg{Router: "z"})
	m = next.(Model)
	if !m.have("z") || cmd != nil {
		t.Errorf("unknown router: have = %v, cmd = %v; want stored, not re-armed", m.have("z"), cmd)
	}
}

// tab walks the screens in §7.1 order and wraps back to the Overview; each
// screen is entered through the same path as its number key, so its data
// is fetched on the way.
func TestTabCyclesScreens(t *testing.T) {
	m, _, _ := applyFixture(t, false, "backup")

	for _, want := range []screenID{screenDrift, screenRuntime, screenApply, screenEvents, screenOverview} {
		m = drive(t, m, tea.KeyMsg{Type: tea.KeyTab})
		if m.screen != want {
			t.Fatalf("after tab: screen = %s, want %s", screenNames[m.screen], screenNames[want])
		}
	}

	if m.driftData == nil || m.driftFetching {
		t.Errorf("drift not fetched on entry: data = %v, fetching = %v", m.driftData, m.driftFetching)
	}
	if m.runtimeStatus == nil && m.runtimeErr == nil {
		t.Error("runtime status not verified on entry")
	}
	if m.eventsFetching || (m.eventsLogs == nil && m.eventsErrs == nil) {
		t.Error("events not read on entry")
	}
}

func TestNumberKeysSelectScreens(t *testing.T) {
	m, _, _ := applyFixture(t, false, "backup")

	m = drive(t, m, key("2"))
	if m.screen != screenDrift || m.driftData == nil {
		t.Fatalf("2: screen = %s, driftData = %v", screenNames[m.screen], m.driftData)
	}
	m = drive(t, m, key("1"))
	if m.screen != screenOverview {
		t.Fatalf("1: screen = %s, want overview", screenNames[m.screen])
	}

	// Drift is fetched once on first entry, then on r only.
	next, cmd := m.Update(key("2"))
	if next.(Model).screen != screenDrift || cmd != nil {
		t.Errorf("re-entering drift: cmd = %v, want no refetch", cmd)
	}
	if _, cmd := next.(Model).Update(key("r")); cmd == nil {
		t.Error("r on drift did not refetch")
	}

	// Events re-read on every entry (see enterEventsScreen).
	m = drive(t, m, key("6"))
	if m.screen != screenEvents {
		t.Fatalf("6: screen = %s, want events", screenNames[m.screen])
	}
	m = drive(t, m, key("1"))
	if _, cmd := m.Update(key("6")); cmd == nil {
		t.Error("re-entering events did not re-read the logs")
	}
}

func TestApplyScrollAndReplan(t *testing.T) {
	m, _, _ := applyFixture(t, true, "backup")
	m = drive(t, m, key("4"))
	if m.apply.stage != applyReview {
		t.Fatalf("stage = %v, want review", m.apply.stage)
	}

	m = drive(t, m, key("k"))
	if m.apply.scroll != 0 {
		t.Errorf("k at the top scrolled to %d", m.apply.scroll)
	}
	m = drive(t, m, key("j"))
	if m.apply.scroll != 1 {
		t.Errorf("j scrolled to %d, want 1", m.apply.scroll)
	}
	for range renderPlanOps(m.apply) {
		m = drive(t, m, key("j"))
	}
	if last := len(renderPlanOps(m.apply)) - 1; m.apply.scroll != last {
		t.Errorf("j past the end scrolled to %d, want clamped at %d", m.apply.scroll, last)
	}
	m = drive(t, m, key("k"))
	if last := len(renderPlanOps(m.apply)) - 1; m.apply.scroll != last-1 {
		t.Errorf("k scrolled to %d, want %d", m.apply.scroll, last-1)
	}

	m.apply.notice = "stale"
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.apply.notice != "" || m.apply.stage != applyReview {
		t.Errorf("esc: notice = %q, stage = %v; want notice cleared, still reviewing", m.apply.notice, m.apply.stage)
	}

	next, cmd := m.Update(key("r"))
	if next.(Model).apply.stage != applyPlanning || cmd == nil {
		t.Errorf("r: stage = %v, cmd = %v; want re-planning", next.(Model).apply.stage, cmd)
	}
}

func TestDefaultPollInterval(t *testing.T) {
	if got := DefaultPollInterval(); got != pollInterval {
		t.Errorf("DefaultPollInterval = %v, want %v", got, pollInterval)
	}
}
