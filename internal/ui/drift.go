package ui

import (
	"context"
	"fmt"
	"strings"
	"sync"

	tea "github.com/charmbracelet/bubbletea"

	"mtha/internal/diff"
	"mtha/internal/model"
	"mtha/internal/plan"
	"mtha/internal/routeros"
)

// driftResultMsg carries the outcome of a full drift fetch across every
// configured sync section.
type driftResultMsg struct {
	data map[string]diff.SectionDiff
	err  error
}

func (m Model) startDriftFetch() (tea.Model, tea.Cmd) {
	m.driftFetching = true
	m.driftErr = nil

	ctx := m.ctx
	if ctx == nil {
		// Init's cancelHolderMsg cmd hasn't resolved yet; fall back rather
		// than pass a nil context. The fetch just won't be cancellable by
		// quitting in this narrow window.
		ctx = context.Background()
	}
	clientA := m.pollers["a"].Client
	clientB := m.pollers["b"].Client
	sections := m.pair.Sync.Sections
	exempt := m.pair.Sync.Exempt

	return m, func() tea.Msg {
		return fetchDrift(ctx, clientA, clientB, sections, exempt)
	}
}

// fetchDrift fetches every configured, non-exempt section from both routers
// concurrently. A section that fails to fetch is skipped rather than
// aborting the whole run, so sections that did succeed are still returned
// alongside the first error encountered (picked by configured section order,
// not fetch completion order, so it stays deterministic).
func fetchDrift(ctx context.Context, clientA, clientB *routeros.Client, sections, exempt []string) driftResultMsg {
	toFetch := make([]string, 0, len(sections))
	for _, section := range sections {
		if !model.SectionExempt(section, exempt) {
			toFetch = append(toFetch, section)
		}
	}

	results := make([]struct {
		diff diff.SectionDiff
		err  error
	}, len(toFetch))

	var wg sync.WaitGroup
	wg.Add(len(toFetch))
	for i, section := range toFetch {
		go func(i int, section string) {
			defer wg.Done()

			aEntries, bEntries, err := fetchSectionPair(ctx, clientA, clientB, section)
			if err != nil {
				results[i].err = err
				return
			}
			results[i].diff = diff.Compare(section, aEntries, bEntries, exempt)
		}(i, section)
	}
	wg.Wait()

	data := make(map[string]diff.SectionDiff, len(toFetch))
	var firstErr error
	for i, section := range toFetch {
		if results[i].err != nil {
			if firstErr == nil {
				firstErr = results[i].err
			}
			continue
		}
		data[section] = results[i].diff
	}

	return driftResultMsg{data: data, err: firstErr}
}

// fetchSectionPair reads one section's raw entries from both routers. Drift
// diffs them; the Apply screen plans from them (it needs the raw ".id"s).
func fetchSectionPair(ctx context.Context, clientA, clientB *routeros.Client, section string) (a, b []model.Entry, err error) {
	a, err = clientA.GetSection(ctx, section)
	if err != nil {
		return nil, nil, fmt.Errorf("fetch %s from router a: %w", section, err)
	}
	b, err = clientB.GetSection(ctx, section)
	if err != nil {
		return nil, nil, fmt.Errorf("fetch %s from router b: %w", section, err)
	}
	return a, b, nil
}

func (m Model) handleDriftKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "r":
		return m.startDriftFetch()

	case "enter":
		if !m.driftFocusHunks && len(m.driftSections) > 0 {
			m.driftFocusHunks = true
			m.driftHunk = 0
		}
		return m, nil

	case "esc":
		m.driftFocusHunks = false
		return m, nil

	case "up", "k":
		if m.driftFocusHunks {
			if m.driftHunk > 0 {
				m.driftHunk--
			}
		} else if m.driftSection > 0 {
			m.driftSection--
		}
		return m, nil

	case "down", "j":
		if m.driftFocusHunks {
			if hunks := m.currentHunks(); m.driftHunk < len(hunks)-1 {
				m.driftHunk++
			}
		} else if m.driftSection < len(m.driftSections)-1 {
			m.driftSection++
		}
		return m, nil

	case " ":
		if hunks := m.currentHunks(); m.driftFocusHunks && m.driftHunk < len(hunks) {
			m.cycleHunkSelection(hunks[m.driftHunk])
		}
		return m, nil

	case "a", "b":
		dir := plan.AtoB
		if msg.String() == "b" {
			dir = plan.BtoA
		}
		m.selectSection(dir)
		return m, nil

	case "c":
		if len(m.driftSections) > 0 {
			delete(m.driftSelected, m.driftSections[m.driftSection])
		}
		return m, nil
	}
	return m, nil
}

// Hunk selection (project.md §5.3: "hunk-level selection", direction "chosen
// per hunk or per section"). Selections are keyed by section and
// plan.HunkRef, which survive a drift refresh; the Apply screen turns them
// into a plan against fresh reads.

// cycleHunkSelection steps one hunk through unselected → A→B → B→A →
// unselected.
func (m *Model) cycleHunkSelection(h diff.Hunk) {
	section := m.driftSections[m.driftSection]
	ref := plan.RefOf(h)
	dir, selected := m.driftSelected[section][ref]
	switch {
	case !selected:
		m.setSelection(section, ref, plan.AtoB)
	case dir == plan.AtoB:
		m.setSelection(section, ref, plan.BtoA)
	default:
		delete(m.driftSelected[section], ref)
	}
}

// selectSection selects every hunk in the current section in one
// direction, and its chains' order findings, which the planner reports as
// skipped until it can plan a move.
func (m *Model) selectSection(dir plan.Direction) {
	if len(m.driftSections) == 0 {
		return
	}
	section := m.driftSections[m.driftSection]
	for _, h := range m.currentHunks() {
		m.setSelection(section, plan.RefOf(h), dir)
	}
	for _, o := range m.driftData[section].Order {
		m.setSelection(section, plan.OrderRef(o), dir)
	}
}

func (m *Model) setSelection(section string, ref plan.HunkRef, dir plan.Direction) {
	if m.driftSelected == nil {
		m.driftSelected = map[string]map[plan.HunkRef]plan.Direction{}
	}
	if m.driftSelected[section] == nil {
		m.driftSelected[section] = map[plan.HunkRef]plan.Direction{}
	}
	m.driftSelected[section][ref] = dir
}

// pruneDriftSelection drops selections for hunks that no longer appear in
// freshly fetched drift data (resolved, e.g. by an apply). Sections missing
// from data (fetch failed) keep their selections.
func (m *Model) pruneDriftSelection(data map[string]diff.SectionDiff) {
	for section, refs := range m.driftSelected {
		sd, ok := data[section]
		if !ok {
			continue
		}
		present := make(map[plan.HunkRef]bool, len(sd.Hunks))
		for _, h := range sd.Hunks {
			present[plan.RefOf(h)] = true
		}
		for _, o := range sd.Order {
			present[plan.OrderRef(o)] = true
		}
		for ref := range refs {
			if !present[ref] {
				delete(refs, ref)
			}
		}
		if len(refs) == 0 {
			delete(m.driftSelected, section)
		}
	}
}

// selectedCount is the number of hunks selected across all sections.
func (m Model) selectedCount() int {
	n := 0
	for _, refs := range m.driftSelected {
		n += len(refs)
	}
	return n
}

func (m Model) currentHunks() []diff.Hunk {
	if m.driftData == nil || m.driftSection >= len(m.driftSections) {
		return nil
	}
	return m.driftData[m.driftSections[m.driftSection]].Hunks
}

func renderDrift(m Model) string {
	var b strings.Builder
	b.WriteString(styleTitle.Render(fmt.Sprintf("mtha — %s — drift", m.pair.Name)))
	b.WriteString("\n\n")

	switch {
	case m.driftFetching:
		b.WriteString(styleMuted.Render("fetching drift..."))
	case m.driftData != nil:
		if m.driftErr != nil {
			b.WriteString(styleDown.Render("error: " + m.driftErr.Error()))
			b.WriteString("\n\n")
		}
		b.WriteString(renderSectionList(m))
		b.WriteString("\n\n")
		b.WriteString(renderHunkList(m))
	case m.driftErr != nil:
		b.WriteString(styleDown.Render("error: " + m.driftErr.Error()))
	default:
		b.WriteString(styleMuted.Render("press r to fetch drift"))
	}

	b.WriteString("\n\n")
	b.WriteString(styleStatusBar.Render(statusLine(m, fmt.Sprintf("%d selected", m.selectedCount()), "space/a/b: select, c: clear")))
	return b.String()
}

func renderSectionList(m Model) string {
	var b strings.Builder
	b.WriteString(styleTitle.Render("Sections"))
	b.WriteString("\n")

	if len(m.driftSections) == 0 {
		b.WriteString(styleMuted.Render("  (no sections configured)\n"))
		return b.String()
	}

	for i, section := range m.driftSections {
		sd, ok := m.driftData[section]
		cursor := cursorPrefix(i == m.driftSection && !m.driftFocusHunks)

		var status string
		switch {
		case !ok:
			status = styleDown.Render("fetch failed")
		case !sd.Clean():
			status = styleDegraded.Render(fmt.Sprintf("%d hunk(s)", sd.Count()))
		default:
			status = styleReady.Render("clean")
		}
		if n := len(m.driftSelected[section]); n > 0 {
			status += styleAccent.Render(fmt.Sprintf(" (%d selected)", n))
		}
		fmt.Fprintf(&b, "%s%-30s %s\n", cursor, section, status)
	}
	return b.String()
}

func renderHunkList(m Model) string {
	var b strings.Builder
	if len(m.driftSections) == 0 {
		return ""
	}

	section := m.driftSections[m.driftSection]
	sd, ok := m.driftData[section]

	b.WriteString(styleTitle.Render("Hunks: " + section))
	b.WriteString("\n")

	if !ok {
		b.WriteString(styleDown.Render("  fetch failed for this section; see error above\n"))
		return b.String()
	}
	if sd.Clean() {
		b.WriteString(styleReady.Render("  no differences\n"))
		return b.String()
	}

	for i, h := range sd.Hunks {
		cursor := cursorPrefix(i == m.driftHunk && m.driftFocusHunks) + selectionMarker(m.driftSelected[section], h)

		switch {
		case h.OnA && !h.OnB:
			fmt.Fprintf(&b, "%s%s %s\n", cursor, styleDown.Render("- only on A"), h.Identity)
		case !h.OnA && h.OnB:
			fmt.Fprintf(&b, "%s%s %s\n", cursor, styleReady.Render("+ only on B"), h.Identity)
		default:
			fmt.Fprintf(&b, "%s%s %s\n", cursor, styleDegraded.Render("~ changed"), h.Identity)
			if i == m.driftHunk && m.driftFocusHunks {
				for _, c := range h.Changes {
					fmt.Fprintf(&b, "      %s: %s -> %s\n", c.Field, styleMuted.Render(c.A), styleMuted.Render(c.B))
				}
			}
		}
	}
	// Order findings (docs/design-questions.md §2) are listed after the
	// hunks. The cursor doesn't reach them; a/b select them with the rest
	// of the section, and the plan reports them as skipped.
	for _, o := range sd.Order {
		marker := "      "
		if dir, ok := m.driftSelected[section][plan.OrderRef(o)]; ok {
			marker = styleAccent.Render("["+dir.String()+"]") + " "
		}
		names := make([]string, len(o.Moved))
		for i, r := range o.Moved {
			names[i] = plan.HunkRef{Identity: r.Identity, Occurrence: r.Occurrence}.String()
		}
		fmt.Fprintf(&b, "  %s%s chain %s: %d of %d rule(s) in a different order: %s\n", marker, styleDegraded.Render("↕ order"), o.Chain, len(o.Moved), o.Rules, strings.Join(names, ", "))
	}
	return b.String()
}

// selectionMarker shows a hunk's selected sync direction, if any.
func selectionMarker(selected map[plan.HunkRef]plan.Direction, h diff.Hunk) string {
	dir, ok := selected[plan.RefOf(h)]
	if !ok {
		return "      "
	}
	return styleAccent.Render("["+dir.String()+"]") + " "
}
