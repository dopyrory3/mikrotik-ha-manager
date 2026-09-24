// Package ui holds the Bubble Tea root model and per-screen views.
// Milestones 1-2 ship the Overview and Drift screens (project.md §9); Apply,
// Runtime, Failover and Events from §7.1 land in later milestones.
package ui

import (
	"context"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"mtha/internal/config"
	"mtha/internal/diff"
	"mtha/internal/poll"
)

// snapshotMsg carries a freshly polled snapshot from one of the pollers.
type snapshotMsg poll.Snapshot

// screenID selects which of §7.1's screens is active.
type screenID int

const (
	screenOverview screenID = iota
	screenDrift
)

// Model is the root Bubble Tea model.
type Model struct {
	pair      *config.Pair
	writeMode bool
	screen    screenID

	pollers map[poll.RouterKey]*poll.Poller
	cancel  context.CancelFunc

	snapshots map[poll.RouterKey]poll.Snapshot
	haveA     bool
	haveB     bool

	driftSections   []string
	driftData       map[string]diff.SectionDiff
	driftFetching   bool
	driftErr        error
	driftSection    int
	driftHunk       int
	driftFocusHunks bool

	width, height int
	quitting      bool
}

// New builds the root model for a pair. pollers must already be constructed
// (one per router key "a"/"b"); New starts them and begins listening.
func New(pair *config.Pair, writeMode bool, pollers map[poll.RouterKey]*poll.Poller) Model {
	return Model{
		pair:          pair,
		writeMode:     writeMode,
		pollers:       pollers,
		snapshots:     make(map[poll.RouterKey]poll.Snapshot),
		driftSections: pair.Sync.Sections,
	}
}

func (m Model) Init() tea.Cmd {
	ctx, cancel := context.WithCancel(context.Background())
	cmds := make([]tea.Cmd, 0, len(m.pollers)+1)
	for _, p := range m.pollers {
		p := p
		go p.Run(ctx)
		cmds = append(cmds, waitForSnapshot(p.Router, p.C))
	}
	cmds = append(cmds, func() tea.Msg { return cancelHolderMsg{cancel} })
	return tea.Batch(cmds...)
}

// cancelHolderMsg smuggles the context.CancelFunc created in Init back into
// Update, since Init can't mutate the model it returns.
type cancelHolderMsg struct {
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
		m.cancel = msg.cancel
		return m, nil

	case snapshotMsg:
		snap := poll.Snapshot(msg)
		m.snapshots[snap.Router] = snap
		if snap.Router == "a" {
			m.haveA = true
		} else if snap.Router == "b" {
			m.haveB = true
		}
		if p, ok := m.pollers[snap.Router]; ok {
			return m, waitForSnapshot(p.Router, p.C)
		}
		return m, nil

	case driftResultMsg:
		m.driftFetching = false
		m.driftErr = msg.err
		if msg.err == nil {
			m.driftData = msg.data
		}
		return m, nil

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

	case "tab":
		if m.screen == screenOverview {
			return m.enterDriftScreen()
		}
		m.screen = screenOverview
		return m, nil
	}

	if m.screen == screenDrift {
		return m.handleDriftKey(msg)
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
