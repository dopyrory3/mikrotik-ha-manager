package ui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// keyBinding is one row of the help overlay (project.md §7.1: "? for
// help"). docs/usage.md's Keybindings tables mirror these.
type keyBinding struct {
	keys, action string
}

var globalBindings = []keyBinding{
	{"1", "Overview"},
	{"2", "Drift"},
	{"3", "Runtime"},
	{"tab", "Cycle Overview → Drift → Runtime"},
	{"?", "Toggle this help"},
	{"q, ctrl+c", "Quit"},
}

// screenBindings lists each screen's own keys; a screen with none (the
// Overview) shows only the global ones.
var screenBindings = map[screenID][]keyBinding{
	screenDrift: {
		{"r", "Re-fetch drift"},
		{"enter", "Move into the hunk list"},
		{"esc", "Back to the section list"},
		{"up, k", "Move up"},
		{"down, j", "Move down"},
	},
	screenRuntime: {
		{"r", "Re-verify runtime status"},
		{"d", "Show a deploy confirmation"},
		{"x", "Show a remove confirmation"},
		{"y", "Confirm pending deploy/remove (needs -write)"},
		{"n, esc", "Cancel the pending confirmation"},
	},
}

var screenNames = map[screenID]string{
	screenOverview: "overview",
	screenDrift:    "drift",
	screenRuntime:  "runtime",
}

// handleHelpKey runs while the overlay is open: only closing it or quitting
// does anything, so a stray keypress can't act on the screen underneath
// (e.g. "y" confirming a pending runtime action).
func (m Model) handleHelpKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "?", "esc":
		m.showHelp = false
	case "q", "ctrl+c":
		m.showHelp = false
		return m.handleKey(msg)
	}
	return m, nil
}

// renderHelp shows the global keys plus the ones for the screen the overlay
// was opened from, keeping it inside an 80×24 terminal (project.md §6).
func renderHelp(m Model) string {
	var b strings.Builder
	name := screenNames[m.screen]
	b.WriteString(styleTitle.Render(fmt.Sprintf("mtha — %s — help", m.pair.Name)))
	b.WriteString("\n\n")

	writeBindings(&b, "Global", globalBindings)
	if bindings := screenBindings[m.screen]; len(bindings) > 0 {
		b.WriteString("\n")
		writeBindings(&b, strings.ToUpper(name[:1])+name[1:], bindings)
	}

	b.WriteString("\n")
	b.WriteString(styleStatusBar.Render(fmt.Sprintf(" %s | %s | ?/esc: close help, q: quit ", m.pair.Name, name)))
	return b.String()
}

func writeBindings(b *strings.Builder, title string, bindings []keyBinding) {
	b.WriteString(styleTitle.Render(title))
	b.WriteString("\n")
	for _, kb := range bindings {
		fmt.Fprintf(b, "  %-10s %s\n", kb.keys, styleMuted.Render(kb.action))
	}
}
