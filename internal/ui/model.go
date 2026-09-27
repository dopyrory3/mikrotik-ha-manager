// Package ui holds the Bubble Tea root model and per-screen views.
// Milestones 1-2-4 ship the Overview, Drift and Runtime screens (project.md
// §9) and milestone 6 the Events screen; Apply and Failover from §7.1 land in
// later milestones.
package ui

import (
	"context"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"mtha/internal/config"
	"mtha/internal/diff"
	"mtha/internal/events"
	"mtha/internal/model"
	"mtha/internal/poll"
	"mtha/internal/routeros"
	"mtha/internal/runtime"
)

// snapshotMsg carries a freshly polled snapshot from one of the pollers.
type snapshotMsg poll.Snapshot

// screenID selects which of §7.1's screens is active.
type screenID int

const (
	screenOverview screenID = iota
	screenDrift
	screenRuntime
	screenEvents
)

// runtimeActionKind identifies which of the Runtime screen's two write
// actions a pending confirmation belongs to.
type runtimeActionKind int

const (
	runtimeActionDeploy runtimeActionKind = iota
	runtimeActionRemove
)

// pendingRuntimeAction holds a computed plan awaiting a second keypress to
// confirm before it runs (project.md §7.3: every write is shown before
// execution). nil on Model means no confirmation is pending.
type pendingRuntimeAction struct {
	kind runtimeActionKind
}

// Model is the root Bubble Tea model.
type Model struct {
	pair      *config.Pair
	writeMode bool
	screen    screenID

	pollers map[poll.RouterKey]*poll.Poller
	ctx     context.Context
	cancel  context.CancelFunc

	snapshots map[poll.RouterKey]poll.Snapshot

	driftSections   []string
	driftData       map[string]diff.SectionDiff
	driftFetching   bool
	driftErr        error
	driftSection    int
	driftHunk       int
	driftFocusHunks bool

	runtimePlans    map[string]runtime.Plan
	runtimePlanErr  error
	runtimeStatus   runtime.Status
	runtimeFetching bool
	runtimeErr      error
	runtimePending  *pendingRuntimeAction

	// journal records the tool's own actions for the Events timeline
	// (project.md §5.7). It is a pointer so every copy of Model shares it;
	// later milestones' actions feed it through events.Recorder.
	journal        *events.Journal
	eventsLogs     map[poll.RouterKey]routeros.EventLog
	eventsErrs     map[poll.RouterKey]error
	eventsFetching bool
	eventsScroll   int

	width, height int
	quitting      bool
}

// New builds the root model for a pair. pollers must already be constructed
// (one per router key "a"/"b"); New starts them and begins listening.
func New(pair *config.Pair, writeMode bool, pollers map[poll.RouterKey]*poll.Poller) Model {
	driftSections := make([]string, 0, len(pair.Sync.Sections))
	for _, s := range pair.Sync.Sections {
		if !model.SectionExempt(s, pair.Sync.Exempt) {
			driftSections = append(driftSections, s)
		}
	}

	return Model{
		pair:          pair,
		writeMode:     writeMode,
		pollers:       pollers,
		snapshots:     make(map[poll.RouterKey]poll.Snapshot),
		driftSections: driftSections,
		journal:       events.NewJournal(),
	}
}

// have reports whether a snapshot has been received yet for router, so
// callers can tell "not polled yet" from a zero-value Snapshot without a
// second bool tracked alongside the map.
func (m Model) have(router poll.RouterKey) bool {
	_, ok := m.snapshots[router]
	return ok
}

func (m Model) Init() tea.Cmd {
	ctx, cancel := context.WithCancel(context.Background())
	cmds := make([]tea.Cmd, 0, len(m.pollers)+1)
	for _, p := range m.pollers {
		p := p
		go p.Run(ctx)
		cmds = append(cmds, waitForSnapshot(p.Router, p.C))
	}
	cmds = append(cmds, func() tea.Msg { return cancelHolderMsg{ctx, cancel} })
	return tea.Batch(cmds...)
}

// cancelHolderMsg smuggles the context.Context/CancelFunc pair created in
// Init back into Update, since Init can't mutate the model it returns.
type cancelHolderMsg struct {
	ctx    context.Context
	cancel context.CancelFunc
}

func waitForSnapshot(router poll.RouterKey, ch chan poll.Snapshot) tea.Cmd {
	return func() tea.Msg {
		return snapshotMsg(<-ch)
	}
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case cancelHolderMsg:
		m.ctx = msg.ctx
		m.cancel = msg.cancel
		return m, nil

	case snapshotMsg:
		snap := poll.Snapshot(msg)
		m.snapshots[snap.Router] = snap
		if p, ok := m.pollers[snap.Router]; ok {
			return m, waitForSnapshot(p.Router, p.C)
		}
		return m, nil

	case driftResultMsg:
		m.driftFetching = false
		m.driftErr = msg.err
		m.driftData = msg.data
		return m, nil

	case runtimeVerifyMsg:
		m.runtimeFetching = false
		m.runtimeErr = msg.err
		m.runtimeStatus = msg.status
		return m, nil

	case runtimeActionMsg:
		m.journal.Record(runtimeActionEvent(msg))
		m.runtimeFetching = false
		m.runtimeErr = msg.result.Err
		m.runtimeStatus = msg.result.Status
		return m, nil

	case eventsResultMsg:
		return m.applyEventsResult(msg), nil

	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "ctrl+c":
		m.quitting = true
		if m.cancel != nil {
			m.cancel()
		}
		return m, tea.Quit

	case "1":
		m.screen = screenOverview
		return m, nil

	case "2":
		return m.enterDriftScreen()

	case "3":
		return m.enterRuntimeScreen()

	case "6":
		return m.enterEventsScreen()

	case "tab":
		switch m.screen {
		case screenOverview:
			return m.enterDriftScreen()
		case screenDrift:
			return m.enterRuntimeScreen()
		case screenRuntime:
			return m.enterEventsScreen()
		default:
			m.screen = screenOverview
			return m, nil
		}
	}

	switch m.screen {
	case screenDrift:
		return m.handleDriftKey(msg)
	case screenRuntime:
		return m.handleRuntimeKey(msg)
	case screenEvents:
		return m.handleEventsKey(msg)
	}
	return m, nil
}

func (m Model) enterDriftScreen() (tea.Model, tea.Cmd) {
	m.screen = screenDrift
	if m.driftData == nil && !m.driftFetching {
		return m.startDriftFetch()
	}
	return m, nil
}

func (m Model) View() string {
	if m.quitting {
		return ""
	}
	switch m.screen {
	case screenDrift:
		return renderDrift(m)
	case screenRuntime:
		return renderRuntime(m)
	case screenEvents:
		return renderEvents(m)
	default:
		return renderDashboard(m)
	}
}

// pollInterval is the default dashboard poll interval (project.md §6).
const pollInterval = 5 * time.Second

// DefaultPollInterval exposes pollInterval for cmd/mtha to build Pollers with.
func DefaultPollInterval() time.Duration {
	return pollInterval
}
