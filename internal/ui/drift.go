package ui

import (
	"context"
	"fmt"
	"strings"

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

	clientA := m.pollers["a"].Client
	clientB := m.pollers["b"].Client
	sections := m.pair.Sync.Sections
	exempt := m.pair.Sync.Exempt

	return m, func() tea.Msg {
		return fetchDrift(clientA, clientB, sections, exempt)
	}
}

func fetchDrift(clientA, clientB *routeros.Client, sections, exempt []string) driftResultMsg {
	ctx := context.Background()
	data := make(map[string]diff.SectionDiff, len(sections))

	for _, section := range sections {
		if model.SectionExempt(section, exempt) {
			continue
		}

		aEntries, err := clientA.GetSection(ctx, section)
		if err != nil {
			return driftResultMsg{err: fmt.Errorf("fetch %s from router a: %w", section, err)}
		}
		bEntries, err := clientB.GetSection(ctx, section)
		if err != nil {
			return driftResultMsg{err: fmt.Errorf("fetch %s from router b: %w", section, err)}
		}

		data[section] = diff.Compare(section, aEntries, bEntries, exempt)
	}

	return driftResultMsg{data: data}
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
	case m.driftErr != nil:
		b.WriteString(styleDown.Render("error: " + m.driftErr.Error()))
	case m.driftData == nil:
		b.WriteString(styleMuted.Render("press r to fetch drift"))
	default:
		b.WriteString(renderSectionList(m))
		b.WriteString("\n\n")
		b.WriteString(renderHunkList(m))
	}

	b.WriteString("\n\n")
	b.WriteString(styleStatusBar.Render(fmt.Sprintf(" %s | drift | r: refresh, enter: hunks, esc: back, tab: overview, q: quit ", m.pair.Name)))
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
		sd := m.driftData[section]
		cursor := "  "
		if i == m.driftSection && !m.driftFocusHunks {
			cursor = "> "
		}

		status := styleReady.Render("clean")
		if !sd.Clean() {
			status = styleDegraded.Render(fmt.Sprintf("%d hunk(s)", len(sd.Hunks)))
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
	sd := m.driftData[section]

	b.WriteString(styleTitle.Render("Hunks: " + section))
	b.WriteString("\n")

	if sd.Clean() {
		b.WriteString(styleReady.Render("  no differences\n"))
		return b.String()
	}

	for i, h := range sd.Hunks {
		cursor := "  "
		if i == m.driftHunk && m.driftFocusHunks {
			cursor = "> "
		}

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
