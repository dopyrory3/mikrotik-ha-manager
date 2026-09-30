package ui

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"mtha/internal/config"
	"mtha/internal/diff"
	"mtha/internal/model"
	"mtha/internal/plan"
	"mtha/internal/poll"
	"mtha/internal/routeros"
)

func TestWrapText(t *testing.T) {
	tests := []struct {
		s           string
		first, rest int
		want        []string
	}{
		{"fits", 10, 10, []string{"fits"}},
		{"no size yet stays whole", 0, 0, []string{"no size yet stays whole"}},
		{"breaks at the last space that fits", 12, 12, []string{"breaks at", "the last", "space that", "fits"}},
		{"first line narrower than the rest", 5, 20, []string{"first", "line narrower than", "the rest"}},
		// A word wider than the space is broken, not dropped.
		{`POST {"comment":"allow-partner-replication"}`, 16, 16, []string{"POST", `{"comment":"allo`, `w-partner-replic`, `ation"}`}},
	}
	for _, tt := range tests {
		got := wrapText(tt.s, tt.first, tt.rest)
		if strings.Join(got, "|") != strings.Join(tt.want, "|") {
			t.Errorf("wrapText(%q, %d, %d) = %q, want %q", tt.s, tt.first, tt.rest, got, tt.want)
		}
	}
}

// longIdentity is an identity as wide as operator comments make them.
func longIdentity(i int) string {
	return fmt.Sprintf("forward@lab-scale %d: allow partner replication to dc2 storage tier from the backup network#%d", i, i)
}

// fitsTerminal fails for each line of view wider than width, and if view
// is taller than height.
func fitsTerminal(t *testing.T, screen, view string, width, height int) {
	t.Helper()
	lines := strings.Split(view, "\n")
	if len(lines) > height {
		t.Errorf("%s: %d lines, want ≤ %d:\n%s", screen, len(lines), height, view)
	}
	for i, l := range lines {
		if w := lipgloss.Width(l); w > width {
			t.Errorf("%s: line %d is %d columns, want ≤ %d: %q", screen, i+1, w, width, l)
		}
	}
}

func sized(m Model, w, h int) Model {
	next, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	return next.(Model)
}

// The Overview at baseline, runtime not yet checked, fits 80×24: its
// runtime readiness line was 82 columns (issue #27).
func TestOverviewFits80x24(t *testing.T) {
	pair := &config.Pair{Name: "lab", Routers: map[string]config.RouterConfig{
		"a": {Host: "https://localhost:20243"}, "b": {Host: "https://localhost:20244"},
	}}
	m := sized(New(pair, false, nil), 80, 24)
	for _, r := range []poll.RouterKey{"a", "b"} {
		m.snapshots[r] = poll.Snapshot{
			Router:   r,
			Resource: &routeros.SystemResource{Version: "7.15.3 (stable)", Uptime: "1h2m3s", CPULoad: "4"},
			Identity: &routeros.Identity{Name: "mtha-lab-router-" + string(r)},
			VRRP:     []routeros.VRRPInstance{{Name: "vrrp-lan", Master: "true"}, {Name: "vrrp-wan", Backup: "true"}},
		}
	}
	view := m.View()
	fitsTerminal(t, "overview", view, 80, 24)
	wantLines(t, view, "Runtime logic present and identical on both routers", "(press 3 to check runtime)")
}

// driftScaleModel is a Drift screen at 80×24 with more hunks than fit,
// every identity wider than the terminal, and a ten-rule order finding.
func driftScaleModel() (Model, []string) {
	pair := &config.Pair{Name: "lab", Sync: config.SyncConfig{Sections: []string{"ip/firewall/filter", "ip/firewall/address-list"}}}
	m := New(pair, false, nil)
	m.screen = screenDrift
	var hunks []diff.Hunk
	var names []string
	hunks = append(hunks, diff.Hunk{Identity: longIdentity(201), OnA: true, OnB: true, Changes: []model.FieldChange{
		{Field: "src-address-list", A: "", B: "partner-replication-sources-dc2-storage-tier"},
	}})
	names = append(names, longIdentity(201))
	for i := 0; i < 20; i++ {
		hunks = append(hunks, diff.Hunk{Identity: longIdentity(341 + i), OnA: true})
		names = append(names, longIdentity(341+i))
	}
	var moved []diff.RuleRef
	for i := 391; i <= 400; i++ {
		moved = append(moved, diff.RuleRef{Identity: longIdentity(i)})
		names = append(names, longIdentity(i))
	}
	m.driftData = map[string]diff.SectionDiff{
		"ip/firewall/filter":       {Section: "ip/firewall/filter", Hunks: hunks, Order: []diff.OrderHunk{{Chain: "forward", Moved: moved, Rules: 406}}},
		"ip/firewall/address-list": {Section: "ip/firewall/address-list"},
	}
	return sized(m, 80, 24), names
}

// The Drift screen fits 80×24 however much drift there is, and scrolls:
// every identity, including the order finding's (which was one line of
// 779 columns, issue #27), can be brought on screen whole.
func TestDriftFitsAndScrolls80x24(t *testing.T) {
	m, names := driftScaleModel()
	fitsTerminal(t, "drift: sections", m.View(), 80, 24)

	m = press(m, tea.KeyMsg{Type: tea.KeyEnter})
	seen := strings.Join(strings.Fields(m.View()), " ")
	for i := 0; i < 80; i++ {
		fitsTerminal(t, fmt.Sprintf("drift: after %d down", i), m.View(), 80, 24)
		if !strings.Contains(m.View(), "> ") {
			// Scrolled past the last hunk to the order finding.
			break
		}
		m = press(m, key("j"))
		seen += " " + strings.Join(strings.Fields(m.View()), " ")
	}
	for i := 0; i < 40; i++ {
		m = press(m, key("j"))
		seen += " " + strings.Join(strings.Fields(m.View()), " ")
	}
	for _, n := range names {
		// Each identity wraps to lines of its own; joined back up it reads
		// whole, the wrap point a single space.
		if !strings.Contains(seen, n) {
			t.Errorf("identity never shown whole while scrolling: %q", n)
		}
	}
	if !strings.Contains(m.View(), "↑") || strings.Contains(m.View(), "↓") {
		t.Errorf("scrolled to the bottom, want only lines above marked:\n%s", m.View())
	}

	// The cursor brings its hunk back into view.
	m = press(m, key("k"))
	if !strings.Contains(m.View(), "> ") {
		t.Errorf("cursor off screen after moving up:\n%s", m.View())
	}

	// The changed hunk's expansion is on screen with it.
	for m.driftHunk > 0 {
		m = press(m, key("k"))
	}
	wantLines(t, m.View(), "src-address-list:")
}

// The Apply screen wraps ops and notes to the terminal rather than running
// them off it (issue #27), scrolling them as before.
func TestApplyFits80x24(t *testing.T) {
	pair := &config.Pair{Name: "lab"}
	m := New(pair, true, nil)
	m.screen = screenApply
	var ops []plan.Op
	for i := 381; i <= 390; i++ {
		ops = append(ops, plan.Op{Router: "b", Method: "POST", Path: "/ip/firewall/filter/move",
			Body: map[string]string{"numbers": fmt.Sprintf("*%X", i), "destination": "*1F4"},
			Note: "move " + longIdentity(i) + " before " + longIdentity(391)})
	}
	m.apply = applyState{stage: applyReview, plan: plan.Plan{Ops: ops}}
	m = sized(m, 80, 24)
	fitsTerminal(t, "apply: review", m.View(), 80, 24)
	wantLines(t, m.View(), "press y to apply")
	if n := len(renderPlanOps(m.apply, 0)); len(renderPlanOps(m.apply, 80)) <= n {
		t.Errorf("plan lines at 80 columns = %d, want more than unwrapped %d", len(renderPlanOps(m.apply, 80)), n)
	}
}
