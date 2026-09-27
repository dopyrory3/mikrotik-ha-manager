package ui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"mtha/internal/config"
)

func helpTestModel() Model {
	return New(&config.Pair{Name: "core"}, false, nil)
}

func press(t *testing.T, m Model, key string) (Model, tea.Cmd) {
	t.Helper()
	var msg tea.KeyMsg
	switch key {
	case "esc":
		msg = tea.KeyMsg{Type: tea.KeyEsc}
	default:
		msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}
	}
	next, cmd := m.Update(msg)
	return next.(Model), cmd
}

func TestHelpToggles(t *testing.T) {
	m := helpTestModel()

	m, _ = press(t, m, "?")
	if !m.showHelp {
		t.Fatal("? did not open the help overlay")
	}
	if view := m.View(); !strings.Contains(view, "Global") || !strings.Contains(view, "Toggle this help") {
		t.Errorf("help view missing the global keybindings:\n%s", view)
	}

	m, _ = press(t, m, "?")
	if m.showHelp {
		t.Error("? did not close the help overlay")
	}

	m, _ = press(t, m, "?")
	m, _ = press(t, m, "esc")
	if m.showHelp {
		t.Error("esc did not close the help overlay")
	}
}

func TestHelpShowsCurrentScreenKeys(t *testing.T) {
	m := helpTestModel()
	m.screen = screenRuntime
	m, _ = press(t, m, "?")

	view := m.View()
	if !strings.Contains(view, "Show a deploy confirmation") {
		t.Errorf("runtime help missing runtime keys:\n%s", view)
	}
	if strings.Contains(view, "Re-fetch drift") {
		t.Errorf("runtime help shows drift keys:\n%s", view)
	}
	if lines := strings.Count(view, "\n") + 1; lines > 24 {
		t.Errorf("help is %d lines, want it to fit an 80×24 terminal", lines)
	}
}

// With the overlay open, a key meant for the screen underneath must not
// act on it — above all "y" confirming a pending runtime write.
func TestHelpSwallowsScreenKeys(t *testing.T) {
	m := helpTestModel()
	m.writeMode = true
	m.screen = screenRuntime
	m.runtimePending = &pendingRuntimeAction{kind: runtimeActionRemove}

	m, _ = press(t, m, "?")
	m, cmd := press(t, m, "y")
	if cmd != nil || m.runtimePending == nil || m.runtimeFetching {
		t.Error("y while help was open acted on the pending runtime confirmation")
	}
	m, _ = press(t, m, "1")
	if m.screen != screenRuntime {
		t.Errorf("screen = %v, want number keys ignored while help is open", m.screen)
	}
	if !m.showHelp {
		t.Error("an unrelated key closed the help overlay")
	}
}

func TestHelpQuitStillQuits(t *testing.T) {
	m := helpTestModel()
	m, _ = press(t, m, "?")
	m, cmd := press(t, m, "q")
	if !m.quitting || cmd == nil {
		t.Error("q while help was open did not quit")
	}
}
