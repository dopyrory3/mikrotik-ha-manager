package ui

import "github.com/charmbracelet/lipgloss"

var (
	colorReady    = lipgloss.Color("2")  // green
	colorDegraded = lipgloss.Color("3")  // yellow
	colorDown     = lipgloss.Color("1")  // red
	colorMuted    = lipgloss.Color("8")  // gray
	colorAccent   = lipgloss.Color("12") // blue

	styleTitle = lipgloss.NewStyle().Bold(true).Foreground(colorAccent)

	styleReady    = lipgloss.NewStyle().Bold(true).Foreground(colorReady)
	styleDegraded = lipgloss.NewStyle().Bold(true).Foreground(colorDegraded)
	styleDown     = lipgloss.NewStyle().Bold(true).Foreground(colorDown)
	styleMuted    = lipgloss.NewStyle().Foreground(colorMuted)

	stylePanel = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colorMuted).
			Padding(0, 1)

	styleStatusBar = lipgloss.NewStyle().
			Foreground(lipgloss.Color("0")).
			Background(colorMuted).
			Padding(0, 1)
)

// cursorPrefix renders the two-column list-selection marker shared by every
// cursor-navigable list on the drift screen.
func cursorPrefix(active bool) string {
	if active {
		return "> "
	}
	return "  "
}
