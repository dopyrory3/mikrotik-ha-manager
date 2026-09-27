package ui

import (
	"context"
	"fmt"
	"strings"
	"sync"

	tea "github.com/charmbracelet/bubbletea"

	"mtha/internal/diff"
	"mtha/internal/model"
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

			aEntries, err := clientA.GetSection(ctx, section)
			if err != nil {
				results[i].err = fmt.Errorf("fetch %s from router a: %w", section, err)
				return
			}
			bEntries, err := clientB.GetSection(ctx, section)
			if err != nil {
				results[i].err = fmt.Errorf("fetch %s from router b: %w", section, err)
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
	}
	return m, nil
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
	b.WriteString(styleStatusBar.Render(fmt.Sprintf(" %s | drift | r: refresh, enter: hunks, esc: back, tab: runtime, ?: help, q: quit ", m.pair.Name)))
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
			status = styleDegraded.Render(fmt.Sprintf("%d hunk(s)", len(sd.Hunks)))
		default:
			status = styleReady.Render("clean")
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
		cursor := cursorPrefix(i == m.driftHunk && m.driftFocusHunks)

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
	return b.String()
}
