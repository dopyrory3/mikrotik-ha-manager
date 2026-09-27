package ui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"mtha/internal/config"
	"mtha/internal/diff"
	"mtha/internal/plan"
)

func selectionModel() Model {
	pair := &config.Pair{Name: "core", Sync: config.SyncConfig{Sections: []string{"ip/service", "user"}}}
	m := New(pair, false, nil)
	m.screen = screenDrift
	m.driftData = map[string]diff.SectionDiff{
		"ip/service": {Section: "ip/service", Hunks: []diff.Hunk{
			{Identity: "www-ssl", OnA: true, OnB: true},
			{Identity: "api", OnA: true},
		}},
		"user": {Section: "user"},
	}
	return m
}

func press(m Model, keys ...tea.KeyMsg) Model {
	for _, k := range keys {
		next, _ := m.Update(k)
		m = next.(Model)
	}
	return m
}

// space cycles a hunk: unselected → A→B → B→A → unselected.
func TestDriftSpaceCyclesHunkDirection(t *testing.T) {
	m := press(selectionModel(), tea.KeyMsg{Type: tea.KeyEnter})
	space := tea.KeyMsg{Type: tea.KeySpace, Runes: []rune(" ")}
	ref := plan.HunkRef{Identity: "www-ssl"}

	m = press(m, space)
	if d, ok := m.driftSelected["ip/service"][ref]; !ok || d != plan.AtoB {
		t.Fatalf("after one space: %v", m.driftSelected)
	}
	m = press(m, space)
	if d := m.driftSelected["ip/service"][ref]; d != plan.BtoA {
		t.Fatalf("after two: %v", m.driftSelected)
	}
	m = press(m, space)
	if _, ok := m.driftSelected["ip/service"][ref]; ok {
		t.Fatalf("after three the hunk should be unselected: %v", m.driftSelected)
	}
}

func TestDriftSectionSelectAndClear(t *testing.T) {
	m := press(selectionModel(), key("b"))
	if m.selectedCount() != 2 {
		t.Fatalf("b should select both hunks, got %d", m.selectedCount())
	}
	for _, d := range m.driftSelected["ip/service"] {
		if d != plan.BtoA {
			t.Fatalf("expected B→A, got %v", m.driftSelected)
		}
	}
	m = press(m, key("c"))
	if m.selectedCount() != 0 {
		t.Fatalf("c should clear the section, got %d", m.selectedCount())
	}
}

// A drift refresh drops selections for hunks that no longer differ, but
// keeps those for sections whose fetch failed.
func TestDriftRefreshPrunesResolvedSelections(t *testing.T) {
	m := press(selectionModel(), key("a"))
	m.setSelection("user", plan.HunkRef{Identity: "alice"}, plan.AtoB)

	next, _ := m.Update(driftResultMsg{data: map[string]diff.SectionDiff{
		"ip/service": {Section: "ip/service", Hunks: []diff.Hunk{{Identity: "api", OnA: true}}},
	}})
	m = next.(Model)

	if _, ok := m.driftSelected["ip/service"][plan.HunkRef{Identity: "www-ssl"}]; ok {
		t.Error("resolved www-ssl selection should be pruned")
	}
	if _, ok := m.driftSelected["ip/service"][plan.HunkRef{Identity: "api"}]; !ok {
		t.Error("still-differing api selection should be kept")
	}
	if _, ok := m.driftSelected["user"][plan.HunkRef{Identity: "alice"}]; !ok {
		t.Error("selection in a section missing from the refresh should be kept")
	}
}
