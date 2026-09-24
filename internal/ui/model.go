// Package ui holds the Bubble Tea root model and per-screen views.
// Milestone 1 ships a single dashboard screen (project.md §9); the Drift,
// Apply, Runtime, Failover and Events screens from §7.1 land in later
// milestones.
package ui

import (
	"context"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"mtha/internal/config"
	"mtha/internal/poll"
)

// snapshotMsg carries a freshly polled snapshot from one of the pollers.
type snapshotMsg poll.Snapshot

// Model is the root Bubble Tea model.
type Model struct {
	pair      *config.Pair
	writeMode bool

	pollers map[poll.RouterKey]*poll.Poller
	cancel  context.CancelFunc

	snapshots map[poll.RouterKey]poll.Snapshot
	haveA     bool
	haveB     bool

	width, height int
	quitting      bool
}

// New builds the root model for a pair. pollers must already be constructed
// (one per router key "a"/"b"); New starts them and begins listening.
func New(pair *config.Pair, writeMode bool, pollers map[poll.RouterKey]*poll.Poller) Model {
	return Model{
		pair:      pair,
		writeMode: writeMode,
		pollers:   pollers,
		snapshots: make(map[poll.RouterKey]poll.Snapshot),
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

	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			m.quitting = true
			if m.cancel != nil {
				m.cancel()
			}
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m Model) View() string {
	if m.quitting {
		return ""
	}
	return renderDashboard(m)
}

// pollInterval is the default dashboard poll interval (project.md §6).
const pollInterval = 5 * time.Second

// DefaultPollInterval exposes pollInterval for cmd/mtha to build Pollers with.
func DefaultPollInterval() time.Duration {
	return pollInterval
}
