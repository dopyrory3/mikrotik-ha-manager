package ui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"mtha/internal/config"
	"mtha/internal/poll"
	"mtha/internal/routeros"
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

	verdict, checks := evaluateReadiness(m.snapshots["a"], m.snapshots["b"], haveA, haveB, m.driftData, m.driftErr, m.runtimeStatus, m.runtimeErr)
	b.WriteString(renderVerdict(verdict, checks, m.width))
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
		role := v.Role()
		style := styleMuted
		switch role {
		case routeros.RoleMaster:
			style = styleReady
		case routeros.RoleBackup:
			style = styleDegraded
		}
		fmt.Fprintf(&b, "vrrp %-12s %s\n", vrrpInstanceKey(v), style.Render(role.String()))
	}

	return panel.Render(strings.TrimRight(b.String(), "\n"))
}

// statusLine is a screen's bottom bar: the mode, then the given segments
// (the focused screen's own keys), then help and quit. The pair and screen
// names are in the title and navigation keys are in the ? overlay, so the
// bar fits an 80-column terminal (project.md §6).
func statusLine(m Model, segments ...string) string {
	parts := append([]string{modeLabel(m.writeMode)}, segments...)
	parts = append(parts, "?: help, q: quit")
	return " " + strings.Join(parts, " | ") + " "
}

// modeLabel is the read/write mode as the status bar shows it (project.md
// §7.1, §7.3).
func modeLabel(writeMode bool) string {
	if writeMode {
		return "write"
	}
	return "read-only"
}

// renderVerdict is the readiness verdict and one line per check, its note
// in parentheses after the label. A check that doesn't fit width (0: no
// limit) wraps under its label, the note moving to lines of its own, so
// nothing an operator needs is cut off in an 80-column terminal (§6).
func renderVerdict(v Verdict, checks []Check, width int) string {
	style := styleDegraded
	switch v {
	case VerdictReady:
		style = styleReady
	case VerdictUnknown:
		style = styleMuted
	}

	lines := []string{"Readiness: " + style.Render(v.String())}
	for _, c := range checks {
		mark := "✗"
		markStyle := styleDown
		if c.OK {
			mark = "✓"
			markStyle = styleReady
		}
		note := ""
		if c.Note != "" {
			note = "(" + c.Note + ")"
		}
		lines = append(lines, wrapPair("  "+markStyle.Render(mark)+" ", c.Label, note, styleMuted.Render, width, checkIndent)...)
	}
	return strings.Join(lines, "\n")
}

// checkIndent lines a readiness check's continuation lines up under its
// label.
const checkIndent = 4
