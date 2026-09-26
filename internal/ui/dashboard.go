package ui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"mtha/internal/config"
	"mtha/internal/poll"
)

// panelGap is the literal spacer rendered between the two router panels.
const panelGap = "  "

// panelBorderWidth is the number of columns stylePanel's rounded border adds
// on top of the width passed to Style.Width (one column per side).
const panelBorderWidth = 2

// minPanelWidth keeps panels usable even in a very narrow terminal, at the
// cost of the pair no longer fitting side by side without wrapping.
const minPanelWidth = 24

// fallbackPanelWidth is used before the first tea.WindowSizeMsg arrives.
const fallbackPanelWidth = 40

// panelContentWidth returns the width to pass to stylePanel.Width so that
// the two router panels plus the gap between them fit within termWidth.
func panelContentWidth(termWidth int) int {
	if termWidth <= 0 {
		return fallbackPanelWidth
	}
	each := (termWidth - len(panelGap)) / 2
	w := each - panelBorderWidth
	if w < minPanelWidth {
		w = minPanelWidth
	}
	return w
}

func renderDashboard(m Model) string {
	var b strings.Builder

	b.WriteString(styleTitle.Render(fmt.Sprintf("mtha — %s", m.pair.Name)))
	b.WriteString("\n\n")

	haveA, haveB := m.have("a"), m.have("b")

	panelWidth := panelContentWidth(m.width)
	panelA := routerPanel("Router A", m.pair.Routers["a"], m.snapshots["a"], haveA, panelWidth)
	panelB := routerPanel("Router B", m.pair.Routers["b"], m.snapshots["b"], haveB, panelWidth)
	b.WriteString(lipgloss.JoinHorizontal(lipgloss.Top, panelA, panelGap, panelB))
	b.WriteString("\n\n")

	verdict, checks := evaluateReadiness(m.snapshots["a"], m.snapshots["b"], haveA, haveB, m.driftData, m.driftErr)
	b.WriteString(renderVerdict(verdict, checks))
	b.WriteString("\n\n")

	b.WriteString(styleStatusBar.Render(statusLine(m)))

	return b.String()
}

func routerPanel(title string, router config.RouterConfig, snap poll.Snapshot, have bool, width int) string {
	panel := stylePanel.Width(width)

	var b strings.Builder
	b.WriteString(styleTitle.Render(title))
	b.WriteString("\n")
	b.WriteString(styleMuted.Render(router.Host))
	b.WriteString("\n")

	if !have {
		b.WriteString(styleMuted.Render("waiting for first poll..."))
		return panel.Render(b.String())
	}

	if !snap.Reachable() {
		b.WriteString(styleDown.Render("unreachable"))
		if snap.Err != nil {
			b.WriteString("\n")
			b.WriteString(styleMuted.Render(snap.Err.Error()))
		}
		return panel.Render(b.String())
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

	return panel.Render(strings.TrimRight(b.String(), "\n"))
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
