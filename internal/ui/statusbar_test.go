package ui

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"mtha/internal/config"
)

// Every screen's status bar, in both modes, every Apply stage and with the
// help overlay open, must fit an 80-column terminal (project.md §6) however
// long the pair name is.
func TestStatusBarsFit80Columns(t *testing.T) {
	screens := []screenID{screenOverview, screenDrift, screenRuntime, screenApply, screenEvents}
	stages := []applyStage{applyIdle, applyPlanning, applyReview, applyConfirmMaster, applyRechecking, applyRunning, applyVerifying, applyDone}
	kinds := []applyKind{applySync, applyRuntimeDeploy, applyRuntimeRemove}

	for _, writeMode := range []bool{false, true} {
		for _, screen := range screens {
			for _, help := range []bool{false, true} {
				m := New(&config.Pair{Name: "a-rather-long-pair-name-for-a-datacentre-edge"}, writeMode, nil)
				next, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
				m = next.(Model)
				m.screen = screen
				m.showHelp = help

				name := fmt.Sprintf("%s/write=%v/help=%v", screenNames[screen], writeMode, help)
				if screen != screenApply {
					checkStatusBarWidth(t, name, m.View())
					continue
				}
				for _, kind := range kinds {
					for _, stage := range stages {
						m.apply = applyState{kind: kind, stage: stage}
						checkStatusBarWidth(t, fmt.Sprintf("%s/%s/stage=%d", name, kind.title(), stage), m.View())
					}
				}
			}
		}
	}
}

func checkStatusBarWidth(t *testing.T, name, view string) {
	t.Helper()
	lines := strings.Split(view, "\n")
	bar := lines[len(lines)-1]
	if w := lipgloss.Width(bar); w > 80 {
		t.Errorf("%s: status bar is %d columns, want ≤ 80: %q", name, w, bar)
	}
}
