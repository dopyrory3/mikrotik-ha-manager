package ui

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"mtha/internal/runtime"
)

// runtimeVerifyMsg carries the outcome of a read-only Runtime screen refresh.
type runtimeVerifyMsg struct {
	status runtime.Status
	err    error
}

func (m Model) enterRuntimeScreen() (tea.Model, tea.Cmd) {
	m.screen = screenRuntime
	if m.runtimePlans == nil && m.runtimePlanErr == nil {
		plans, err := runtime.BuildPlan(m.pair)
		if err != nil {
			m.runtimePlanErr = err
			return m, nil
		}
		m.runtimePlans = plans
	}
	if m.runtimeStatus == nil && !m.runtimeFetching {
		return m.startRuntimeVerify()
	}
	return m, nil
}

func (m Model) startRuntimeVerify() (tea.Model, tea.Cmd) {
	m.runtimeFetching = true
	m.runtimeErr = nil

	ctx := m.runtimeCtx()
	clientA, clientB := m.pollers["a"].Client, m.pollers["b"].Client
	plans := m.runtimePlans

	return m, func() tea.Msg {
		status, err := runtime.Verify(ctx, clientA, clientB, plans)
		return runtimeVerifyMsg{status: status, err: err}
	}
}

// runtimeCtx mirrors startDriftFetch's fallback: Init's cancelHolderMsg cmd
// may not have resolved yet on the very first keypress.
func (m Model) runtimeCtx() context.Context {
	if m.ctx != nil {
		return m.ctx
	}
	return context.Background()
}

// handleRuntimeKey plans a deploy (d) or remove (x) and hands it to the
// Apply screen, which shows the dry run and runs it only after the same
// confirmations as a sync (y, plus Y for a possible master). Nothing is
// written from this screen.
func (m Model) handleRuntimeKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.runtimePlanErr != nil || m.runtimeFetching {
		return m, nil
	}

	switch msg.String() {
	case "r":
		m.runtimeNotice = ""
		return m.startRuntimeVerify()
	case "d", "x":
		if reason := m.writeBusy(); reason != "" {
			m.runtimeNotice = reason
			return m, nil
		}
		m.runtimeNotice = ""
		m.apply = applyState{kind: applyRuntimeDeploy}
		if msg.String() == "x" {
			m.apply.kind = applyRuntimeRemove
		}
		m.screen = screenApply
		return m.startApplyPlan(false)
	}
	return m, nil
}

func renderRuntime(m Model) string {
	var b strings.Builder
	b.WriteString(styleTitle.Render(fmt.Sprintf("mtha — %s — runtime", m.pair.Name)))
	b.WriteString("\n\n")

	if m.runtimeNotice != "" {
		b.WriteString(styleDegraded.Render(m.runtimeNotice))
		b.WriteString("\n\n")
	}

	switch {
	case m.runtimePlanErr != nil:
		b.WriteString(styleDown.Render("config error: " + m.runtimePlanErr.Error()))
	case m.runtimeFetching:
		b.WriteString(styleMuted.Render("working..."))
	case m.runtimeErr != nil:
		b.WriteString(styleDown.Render("error: " + m.runtimeErr.Error()))
		b.WriteString("\n\n")
		b.WriteString(renderRuntimeStatus(m))
	case m.runtimeStatus != nil:
		b.WriteString(renderRuntimeStatus(m))
	default:
		b.WriteString(styleMuted.Render("press r to check runtime status"))
	}

	b.WriteString("\n\n")
	b.WriteString(styleStatusBar.Render(statusLine(m, "r: refresh, d: plan deploy, x: plan remove")))
	return b.String()
}

func renderRuntimeStatus(m Model) string {
	var b strings.Builder
	for _, router := range []string{"a", "b"} {
		fmt.Fprintf(&b, "%s\n", styleTitle.Render("Router "+strings.ToUpper(router)))
		items := m.runtimeStatus[router]
		if len(items) == 0 {
			b.WriteString(styleMuted.Render("  (nothing to deploy — no vrrp instance has \"on\"/\"vrid\"/\"addresses\" set)\n"))
			continue
		}
		for _, it := range items {
			b.WriteString("  ")
			b.WriteString(statusStyle(it.State).Render(it.State.String()))
			fmt.Fprintf(&b, "  %s", it.Label)
			if it.Note != "" {
				b.WriteString(styleMuted.Render(" (" + it.Note + ")"))
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}

func statusStyle(s runtime.State) lipgloss.Style {
	switch s {
	case runtime.StateOK:
		return styleReady
	case runtime.StateConflict:
		return styleDown
	default:
		return styleDegraded
	}
}
