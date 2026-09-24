package ui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"mtha/internal/config"
	"mtha/internal/poll"
)

func renderDashboard(m Model) string {
	var b strings.Builder

	b.WriteString(styleTitle.Render(fmt.Sprintf("mtha — %s", m.pair.Name)))
	b.WriteString("\n\n")

	panelA := routerPanel("Router A", m.pair.Routers["a"], m.snapshots["a"], m.haveA)
	panelB := routerPanel("Router B", m.pair.Routers["b"], m.snapshots["b"], m.haveB)
	b.WriteString(lipgloss.JoinHorizontal(lipgloss.Top, panelA, "  ", panelB))
	b.WriteString("\n\n")

	verdict, checks := evaluateReadiness(m.snapshots["a"], m.snapshots["b"], m.haveA, m.haveB, m.driftData, m.driftErr)
	b.WriteString(renderVerdict(verdict, checks))
	b.WriteString("\n\n")

	b.WriteString(styleStatusBar.Render(statusLine(m)))

	return b.String()
}

func routerPanel(title string, router config.RouterConfig, snap poll.Snapshot, have bool) string {
	var b strings.Builder
	b.WriteString(styleTitle.Render(title))
	b.WriteString("\n")
	b.WriteString(styleMuted.Render(router.Host))
	b.WriteString("\n")

	if !have {
		b.WriteString(styleMuted.Render("waiting for first poll..."))
		return stylePanel.Render(b.String())
	}

	if !snap.Reachable {
		b.WriteString(styleDown.Render("unreachable"))
		if snap.Err != nil {
			b.WriteString("\n")
			b.WriteString(styleMuted.Render(snap.Err.Error()))
		}
		return stylePanel.Render(b.String())
	}

	b.WriteString(styleReady.Render("reachable"))
	b.WriteString("\n")

	if snap.Identity != nil {
		fmt.Fprintf(&b, "identity: %s\n", snap.Identity.Name)
	}
	if snap.Resource != nil {
		fmt.Fprintf(&b, "version:  %s\n", snap.Resource.Version)
		fmt.Fprintf(&b, "uptime:   %s\n", snap.Resource.Uptime)
		fmt.Fprintf(&b, "cpu:      %s\n", snap.Resource.CPULoad)
	}

	if len(snap.VRRP) == 0 {
		b.WriteString(styleMuted.Render("no VRRP instances\n"))
	}
	for _, v := range snap.VRRP {
		state := v.State
		style := styleMuted
		switch state {
		case "master":
			style = styleReady
		case "backup":
			style = styleDegraded
		}
		fmt.Fprintf(&b, "vrrp %-12s %s\n", v.Interface, style.Render(state))
	}

	return stylePanel.Render(strings.TrimRight(b.String(), "\n"))
}

func statusLine(m Model) string {
	mode := "read-only"
	if m.writeMode {
		mode = "write"
	}
	return fmt.Sprintf(" %s | %s | 2: drift, q: quit ", m.pair.Name, mode)
}

func renderVerdict(v Verdict, checks []Check) string {
	style := styleDegraded
	switch v {
	case VerdictReady:
		style = styleReady
	case VerdictUnknown:
		style = styleMuted
	}

	var b strings.Builder
	b.WriteString("Readiness: ")
	b.WriteString(style.Render(v.String()))
	b.WriteString("\n")
	for _, c := range checks {
		mark := "✗"
		markStyle := styleDown
		if c.OK {
			mark = "✓"
			markStyle = styleReady
		}
		line := fmt.Sprintf("  %s %s", markStyle.Render(mark), c.Label)
		if c.Note != "" {
			line += styleMuted.Render(" (" + c.Note + ")")
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}
