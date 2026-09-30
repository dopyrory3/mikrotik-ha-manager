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

	case "esc":
		m.driftFocusHunks = false

	case "up", "k":
		switch {
		case m.driftFocusHunks && m.driftHunk > 0:
			m.driftHunk--
		case !m.driftFocusHunks && m.driftSection > 0:
			m.driftSection--
		default:
			// Already at the top of the list: scroll up to what is above.
			m.driftScroll = max(m.driftScroll-1, 0)
			return m, nil
		}

	case "down", "j":
		switch {
		case m.driftFocusHunks && m.driftHunk < len(m.currentHunks())-1:
			m.driftHunk++
		case !m.driftFocusHunks && m.driftSection < len(m.driftSections)-1:
			m.driftSection++
		default:
			// At the bottom of the list: scroll on to what the cursor
			// doesn't reach, the order findings below the last hunk.
			lines, _, _ := driftBody(m)
			if h := m.driftBodyHeight(); len(lines) > h {
				// h-1: driftWindow's last line is its scroll marker.
				m.driftScroll = min(m.driftScroll+1, len(lines)-(h-1))
			}
			return m, nil
		}

	case " ":
		if hunks := m.currentHunks(); m.driftFocusHunks && m.driftHunk < len(hunks) {
			m.cycleHunkSelection(hunks[m.driftHunk])
		}

	case "a", "b":
		dir := plan.AtoB
		if msg.String() == "b" {
			dir = plan.BtoA
		}
		m.selectSection(dir)

	case "c":
		if len(m.driftSections) > 0 {
			delete(m.driftSelected, m.driftSections[m.driftSection])
		}

	default:
		return m, nil
	}
	m.driftScroll = m.followDriftCursor()
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

	lines, _, _ := driftBody(m)
	b.WriteString(strings.Join(driftWindow(lines, m.driftScroll, m.driftBodyHeight()), "\n"))

	b.WriteString("\n\n")
	b.WriteString(styleStatusBar.Render(statusLine(m, fmt.Sprintf("%d selected", m.selectedCount()), "space/a/b: select, c: clear")))
	return b.String()
}

// driftBody is everything the Drift screen draws between its title and its
// status bar, wrapped to the terminal's width, and the lines [start, end)
// the cursor's item takes up (a hunk with its expanded changes).
func driftBody(m Model) (lines []string, start, end int) {
	errLines := func() []string {
		return wrapLines("", "error: "+m.driftErr.Error(), styleDown.Render, m.width, 2)
	}
	switch {
	case m.driftFetching:
		return []string{styleMuted.Render("fetching drift...")}, 0, 1
	case m.driftData != nil:
		if m.driftErr != nil {
			lines = append(errLines(), "")
		}
		sections, cursor := renderSectionList(m)
		if !m.driftFocusHunks {
			start, end = len(lines)+cursor, len(lines)+cursor+1
		}
		lines = append(append(lines, sections...), "")
		hunks, hs, he := renderHunkList(m)
		if m.driftFocusHunks {
			start, end = len(lines)+hs, len(lines)+he
		}
		return append(lines, hunks...), start, end
	case m.driftErr != nil:
		return errLines(), 0, 1
	default:
		return []string{styleMuted.Render("press r to fetch drift")}, 0, 1
	}
}

// driftBodyHeight is how many lines of driftBody fit between the title
// (and the blank under it) and the status bar (and the blank above it).
func (m Model) driftBodyHeight() int {
	if m.height <= 0 {
		return 1 << 30 // no size yet: show everything
	}
	return max(m.height-4, 3)
}

// followDriftCursor is the scroll offset that brings the cursor's item into
// view, moving the current one as little as it can.
func (m Model) followDriftCursor() int {
	lines, start, end := driftBody(m)
	h := m.driftBodyHeight()
	if len(lines) <= h {
		return 0
	}
	h-- // driftWindow's last line is its scroll marker
	off := m.driftScroll
	if end > off+h {
		off = end - h
	}
	if start < off {
		off = start
	}
	return min(max(off, 0), len(lines)-h)
}

// driftWindow is the height lines of the body shown from offset (clamped),
// the last of them a marker saying how much is above and below when it
// doesn't all fit.
func driftWindow(lines []string, offset, height int) []string {
	if len(lines) <= height {
		return lines
	}
	h := height - 1
	offset = min(max(offset, 0), len(lines)-h)
	window := append([]string{}, lines[offset:offset+h]...)
	var more []string
	if offset > 0 {
		more = append(more, fmt.Sprintf("↑ %d line(s) above", offset))
	}
	if below := len(lines) - offset - h; below > 0 {
		more = append(more, fmt.Sprintf("↓ %d below", below))
	}
	return append(window, styleMuted.Render("  … "+strings.Join(more, ", ")+" — j/k to scroll"))
}

// renderSectionList is the section list's lines and which of them the
// section cursor is on.
func renderSectionList(m Model) (lines []string, cursorLine int) {
	lines = []string{styleTitle.Render("Sections")}

	if len(m.driftSections) == 0 {
		return append(lines, styleMuted.Render("  (no sections configured)")), 1
	}

	for i, section := range m.driftSections {
		sd, ok := m.driftData[section]
		if i == m.driftSection {
			cursorLine = len(lines)
		}
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
		lines = append(lines, fmt.Sprintf("%s%-30s %s", cursor, section, status))
	}
	return lines, cursorLine
}

// hunkIndent is where a wrapped identity or order finding continues:
// under the text after the cursor and selection marker.
const hunkIndent = 8

// renderHunkList is the current section's hunk list, identities wrapped to
// the terminal's width, and the lines [start, end) of the hunk under the
// cursor.
func renderHunkList(m Model) (lines []string, start, end int) {
	if len(m.driftSections) == 0 {
		return nil, 0, 0
	}

	section := m.driftSections[m.driftSection]
	sd, ok := m.driftData[section]

	lines = []string{styleTitle.Render("Hunks: " + section)}

	if !ok {
		return append(lines, styleDown.Render("  fetch failed for this section; see error above")), 0, 1
	}
	if sd.Clean() {
		return append(lines, styleReady.Render("  no differences")), 0, 1
	}

	start, end = 0, 1
	for i, h := range sd.Hunks {
		focused := i == m.driftHunk && m.driftFocusHunks
		prefix := cursorPrefix(focused) + selectionMarker(m.driftSelected[section], h)
		if focused {
			start = len(lines)
		}

		switch {
		case h.OnA && !h.OnB:
			lines = append(lines, wrapLines(prefix+styleDown.Render("- only on A")+" ", h.Identity, plain, m.width, hunkIndent)...)
		case !h.OnA && h.OnB:
			lines = append(lines, wrapLines(prefix+styleReady.Render("+ only on B")+" ", h.Identity, plain, m.width, hunkIndent)...)
		default:
			lines = append(lines, wrapLines(prefix+styleDegraded.Render("~ changed")+" ", h.Identity, plain, m.width, hunkIndent)...)
			if focused {
				for _, c := range h.Changes {
					lines = append(lines, wrapLines("      "+c.Field+": ", c.A+" -> "+c.B, styleMuted.Render, m.width, hunkIndent+2)...)
				}
			}
		}
		if focused {
			end = len(lines)
		}
	}
	// Order findings (docs/design-questions.md §2) are listed after the
	// hunks, one moved rule to a line. The cursor doesn't reach them (j/k
	// past the last hunk scroll to them); a/b select them with the rest of
	// the section, and the plan reports them as skipped.
	for _, o := range sd.Order {
		marker := "      "
		if dir, ok := m.driftSelected[section][plan.OrderRef(o)]; ok {
			marker = styleAccent.Render("["+dir.String()+"]") + " "
		}
		summary := fmt.Sprintf("chain %s: %d of %d rule(s) in a different order:", o.Chain, len(o.Moved), o.Rules)
		lines = append(lines, wrapLines("  "+marker+styleDegraded.Render("↕ order")+" ", summary, plain, m.width, hunkIndent)...)
		for _, r := range o.Moved {
			name := plan.HunkRef{Identity: r.Identity, Occurrence: r.Occurrence}.String()
			lines = append(lines, wrapLines(strings.Repeat(" ", hunkIndent+2), name, plain, m.width, hunkIndent+4)...)
		}
	}
	return lines, start, end
}

// selectionMarker shows a hunk's selected sync direction, if any.
func selectionMarker(selected map[plan.HunkRef]plan.Direction, h diff.Hunk) string {
	dir, ok := selected[plan.RefOf(h)]
	if !ok {
		return "      "
	}
	return styleAccent.Render("["+dir.String()+"]") + " "
}
