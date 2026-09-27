package ui

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"mtha/internal/events"
	"mtha/internal/poll"
	"mtha/internal/routeros"
)

// eventsResultMsg carries one read of both routers' event logs. A router
// that failed has an entry in errs and none in logs.
type eventsResultMsg struct {
	logs map[poll.RouterKey]routeros.EventLog
	errs map[poll.RouterKey]error
}

// eventsSkewWarn is the router clock skew above which the Events screen
// points it out. ReadEvents already corrects for skew; the warning is there
// because unsynchronised clocks on an HA pair are worth knowing about, and
// it sits above the ~1s jitter inherent in RouterOS's 1s timestamps.
const eventsSkewWarn = 3 * time.Second

// enterEventsScreen re-reads the logs on every entry, not only the first as
// Drift does: a timeline is only useful if it is current.
func (m Model) enterEventsScreen() (tea.Model, tea.Cmd) {
	m.screen = screenEvents
	if !m.eventsFetching {
		return m.startEventsFetch()
	}
	return m, nil
}

func (m Model) startEventsFetch() (tea.Model, tea.Cmd) {
	m.eventsFetching = true

	ctx := m.runtimeCtx()
	clients := map[poll.RouterKey]*routeros.Client{
		"a": m.pollers["a"].Client,
		"b": m.pollers["b"].Client,
	}
	return m, func() tea.Msg {
		return fetchEvents(ctx, clients)
	}
}

// fetchEvents reads every router's event log concurrently. One router
// failing doesn't discard the other's result, matching fetchDrift.
func fetchEvents(ctx context.Context, clients map[poll.RouterKey]*routeros.Client) eventsResultMsg {
	msg := eventsResultMsg{
		logs: make(map[poll.RouterKey]routeros.EventLog, len(clients)),
		errs: make(map[poll.RouterKey]error),
	}
	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)
	for key, client := range clients {
		wg.Add(1)
		go func(key poll.RouterKey, client *routeros.Client) {
			defer wg.Done()
			log, err := client.ReadEvents(ctx)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				msg.errs[key] = fmt.Errorf("read log from router %s: %w", key, err)
				return
			}
			msg.logs[key] = log
		}(key, client)
	}
	wg.Wait()
	return msg
}

// applyEventsResult folds a fetch into the model. A router that failed keeps
// its previous log, so a transient error doesn't blank its half of the
// timeline; the error is shown alongside it instead.
func (m Model) applyEventsResult(msg eventsResultMsg) Model {
	m.eventsFetching = false
	if m.eventsLogs == nil {
		m.eventsLogs = make(map[poll.RouterKey]routeros.EventLog)
	}
	m.eventsErrs = msg.errs
	for key, log := range msg.logs {
		m.eventsLogs[key] = log
	}
	return m
}

// applyActionEvents describes a finished run for the Events timeline
// (project.md §5.7): one action per target router, saying what ran — the
// sections a sync wrote, or a Runtime deploy/remove, labelled as a runtime
// action — and how many of that router's ops ran. A router whose ops did
// not all run — the one that failed, or one the run stopped before
// reaching — carries the run's error.
func applyActionEvents(a applyState) []events.Action {
	var out []events.Action
	for _, target := range a.plan.Targets() {
		var sections []string
		seen := map[string]bool{}
		total, done := 0, 0
		for i, op := range a.plan.Ops {
			if op.Router != target {
				continue
			}
			total++
			if i < len(a.status) && a.status[i] == opDone {
				done++
			}
			if op.Section != "" && !seen[op.Section] {
				seen[op.Section] = true
				sections = append(sections, op.Section)
			}
		}
		action := events.Action{
			Kind:    events.ActionSync,
			Target:  target,
			Summary: fmt.Sprintf("apply %s: %d/%d ops", strings.Join(sections, ","), done, total),
		}
		if a.kind != applySync {
			action.Kind = events.ActionRuntime
			action.Summary = fmt.Sprintf("%s runtime logic: %d/%d ops", a.kind.runtimeAction(), done, total)
		}
		if done < total {
			action.Err = a.err
		}
		out = append(out, action)
	}
	return out
}

// timeline merges both routers' logs and the session's tool actions, oldest
// first (project.md §5.7).
func (m Model) timeline() []events.Event {
	var actions []events.Action
	if m.journal != nil {
		actions = m.journal.Actions()
	}
	return events.Merge(
		events.FromLog(events.SourceA, m.eventsLogs["a"].Events),
		events.FromLog(events.SourceB, m.eventsLogs["b"].Events),
		events.FromActions(actions),
	)
}

// eventsOverhead is the number of lines renderEvents spends on everything
// but timeline rows: title, blank, two router lines, blank, column header,
// blank, status bar.
const eventsOverhead = 8

// eventsRows is how many timeline rows fit on screen.
func (m Model) eventsRows() int {
	h := m.height
	if h == 0 {
		h = 24 // before the first WindowSizeMsg; §6's minimum terminal
	}
	return max(h-eventsOverhead, 1)
}

// clampEventsScroll keeps the scroll offset within the timeline, so the last
// page stays full rather than scrolling off into blank rows.
func (m Model) clampEventsScroll(scroll, total int) int {
	return max(min(scroll, total-m.eventsRows()), 0)
}

func (m Model) handleEventsKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	total := len(m.timeline())
	switch msg.String() {
	case "r":
		if !m.eventsFetching {
			return m.startEventsFetch()
		}
	case "down", "j":
		m.eventsScroll = m.clampEventsScroll(m.eventsScroll+1, total)
	case "up", "k":
		m.eventsScroll = m.clampEventsScroll(m.eventsScroll-1, total)
	case "g":
		m.eventsScroll = 0
	case "G":
		m.eventsScroll = m.clampEventsScroll(total, total)
	}
	return m, nil
}

func renderEvents(m Model) string {
	var b strings.Builder
	b.WriteString(styleTitle.Render(fmt.Sprintf("mtha — %s — events", m.pair.Name)))
	b.WriteString("\n\n")

	for _, key := range []poll.RouterKey{"a", "b"} {
		b.WriteString(renderEventsRouterLine(m, key))
		b.WriteString("\n")
	}
	b.WriteString("\n")

	tl := m.timeline()
	switch {
	case len(tl) == 0 && m.eventsFetching:
		b.WriteString(styleMuted.Render("reading router logs..."))
	case len(tl) == 0:
		b.WriteString(styleMuted.Render("no vrrp, netwatch or mtha: events in either router's log"))
	default:
		b.WriteString(styleMuted.Render(fmt.Sprintf("%-15s  %-4s  %-8s  %s", "time", "src", "kind", "message")))
		b.WriteString("\n")
		b.WriteString(renderTimeline(m, tl))
	}

	b.WriteString("\n\n")
	b.WriteString(styleStatusBar.Render(fmt.Sprintf(" %s | events | r: refresh, j/k: scroll, g/G: newest/oldest, q: quit ", m.pair.Name)))
	return b.String()
}

// renderEventsRouterLine summarises one router's last log read.
func renderEventsRouterLine(m Model, key poll.RouterKey) string {
	label := "Router " + strings.ToUpper(string(key)) + ": "
	if err := m.eventsErrs[key]; err != nil {
		return label + styleDown.Render("error: "+err.Error())
	}
	log, ok := m.eventsLogs[key]
	if !ok {
		if m.eventsFetching {
			return label + styleMuted.Render("reading...")
		}
		return label + styleMuted.Render("not read yet")
	}
	line := fmt.Sprintf("%d event(s), read %s", len(log.Events), log.FetchedAt.Local().Format("15:04:05"))
	if log.SkewKnown && (log.Skew > eventsSkewWarn || log.Skew < -eventsSkewWarn) {
		dir := "ahead of"
		skew := log.Skew
		if skew < 0 {
			dir, skew = "behind", -skew
		}
		return label + line + ", " + styleDegraded.Render(fmt.Sprintf("clock %s %s this machine (timeline corrected)", skew, dir))
	}
	return label + line
}

// renderTimeline draws one screenful of tl, newest first so the latest
// events are visible without scrolling.
func renderTimeline(m Model, tl []events.Event) string {
	width := m.width
	if width == 0 {
		width = 80
	}
	// Matches the column header: time(15) + src(4) + kind(8) + 3 gaps of 2.
	msgWidth := max(width-33, 10)

	scroll := m.clampEventsScroll(m.eventsScroll, len(tl))
	rows := m.eventsRows()

	var b strings.Builder
	for i := scroll; i < len(tl) && i < scroll+rows; i++ {
		e := tl[len(tl)-1-i]

		at := "--"
		if !e.At.IsZero() {
			at = e.At.Local().Format("Jan 02 15:04:05")
		}
		src := strings.ToUpper(string(e.Source))
		if e.Source == events.SourceTool {
			src = styleTitle.Render(fmt.Sprintf("%-4s", string(e.Source)))
		} else {
			src = fmt.Sprintf("%-4s", src)
		}
		kind := fmt.Sprintf("%-8s", e.Kind)
		if e.Failed {
			kind = styleDown.Render(kind)
		}
		if i > scroll {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "%-15s  %s  %s  %s", at, src, kind, truncateRunes(e.Message, msgWidth))
	}
	return b.String()
}

// truncateRunes shortens s to at most n runes, marking the cut with "…".
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
