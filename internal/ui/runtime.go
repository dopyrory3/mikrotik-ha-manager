package ui

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"mtha/internal/poll"
	"mtha/internal/runtime"
)

// runtimeVerifyMsg carries the outcome of a read-only Runtime screen refresh.
type runtimeVerifyMsg struct {
	status runtime.Status
	err    error
}

// runtimeActionMsg carries the outcome of a confirmed deploy or remove.
type runtimeActionMsg struct {
	result runtime.Result
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

func (m Model) startRuntimeAction(kind runtimeActionKind) (tea.Model, tea.Cmd) {
	m.runtimeFetching = true
	m.runtimePending = nil

	ctx := m.runtimeCtx()
	clientA, clientB := m.pollers["a"].Client, m.pollers["b"].Client
	plans := m.runtimePlans

	return m, func() tea.Msg {
		var result runtime.Result
		if kind == runtimeActionDeploy {
			result = runtime.Deploy(ctx, clientA, clientB, plans)
		} else {
			result = runtime.Remove(ctx, clientA, clientB, plans)
		}
		return runtimeActionMsg{result: result}
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

func (m Model) handleRuntimeKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.runtimePending != nil {
		switch msg.String() {
		case "y":
			if !m.writeMode {
				return m, nil
			}
			return m.startRuntimeAction(m.runtimePending.kind)
		case "n", "esc":
			m.runtimePending = nil
			return m, nil
		}
		return m, nil
	}

	if m.runtimePlanErr != nil || m.runtimeFetching {
		return m, nil
	}

	switch msg.String() {
	case "r":
		return m.startRuntimeVerify()
	case "d":
		m.runtimePending = &pendingRuntimeAction{kind: runtimeActionDeploy}
		return m, nil
	case "x":
		m.runtimePending = &pendingRuntimeAction{kind: runtimeActionRemove}
		return m, nil
	}
	return m, nil
}

func renderRuntime(m Model) string {
	var b strings.Builder
	b.WriteString(styleTitle.Render(fmt.Sprintf("mtha — %s — runtime", m.pair.Name)))
	b.WriteString("\n\n")

	switch {
	case m.runtimePlanErr != nil:
		b.WriteString(styleDown.Render("config error: " + m.runtimePlanErr.Error()))
	case m.runtimePending != nil:
		b.WriteString(renderRuntimeConfirm(m))
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
	hint := "r: refresh, d: deploy, x: remove, tab: overview, ?: help, q: quit"
	if m.runtimePending != nil {
		hint = "y: confirm, n/esc: cancel"
	}
	b.WriteString(styleStatusBar.Render(fmt.Sprintf(" %s | runtime | %s ", m.pair.Name, hint)))
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

func renderRuntimeConfirm(m Model) string {
	var b strings.Builder
	verb := "Deploy"
	if m.runtimePending.kind == runtimeActionRemove {
		verb = "Remove"
	}
	fmt.Fprintf(&b, "%s the following:\n\n", verb)

	for _, router := range []string{"a", "b"} {
		fmt.Fprintf(&b, "%s\n", styleTitle.Render("Router "+strings.ToUpper(router)))
		master := currentMaster(m, router)
		for _, op := range m.runtimePlans[router].Ops {
			marker := "  "
			if m.runtimePending.kind == runtimeActionRemove && master && op.Section == "interface/vrrp" {
				marker = styleDown.Render("‼ ")
			}
			fmt.Fprintf(&b, "%s%s\n", marker, op.Label)
		}
	}

	b.WriteString("\n")
	if m.runtimePending.kind == runtimeActionRemove {
		b.WriteString(styleMuted.Render("‼ marks a VRRP interface on the router currently holding master.\n"))
	}
	if !m.writeMode {
		b.WriteString(styleDown.Render("read-only — restart with -write to actually run this"))
	} else {
		b.WriteString("press y to confirm, n/esc to cancel")
	}
	return b.String()
}

// currentMaster reports whether any VRRP instance this plan manages is
// currently reporting "master" on router (used to flag a risky removal).
func currentMaster(m Model, router string) bool {
	snap := m.snapshots[poll.RouterKey(router)]
	for _, v := range snap.VRRP {
		if v.State == "master" {
			return true
		}
	}
	return false
}
