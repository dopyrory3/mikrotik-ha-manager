package ui

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// Real device data — identities built from operator comments, REST bodies,
// error text — is often wider than the 80-column terminal project.md §6
// promises to work in, and Bubble Tea cuts off whatever overflows. Screens
// therefore wrap such text rather than clip it, and scroll when the wrapped
// result is taller than the terminal.

// wrapText breaks plain (unstyled) text into chunks for a line whose first
// chunk has first columns available and whose continuation chunks have rest,
// breaking at spaces where it can and mid-word only where a single word is
// wider than the space. Callers put their own prefix and indent around the
// chunks and style each one. A width of zero or less (no terminal size yet)
// leaves the text whole.
func wrapText(s string, first, rest int) []string {
	if first <= 0 {
		return []string{s}
	}
	rest = max(rest, 1)
	var chunks []string
	limit := first
	for ansi.StringWidth(s) > limit {
		head := ansi.Truncate(s, limit, "")
		if head == "" {
			// Not even one character fits the first chunk: start on the
			// next one.
			chunks = append(chunks, "")
			limit = rest
			continue
		}
		cut := len(head)
		if s[cut] != ' ' {
			if i := strings.LastIndexByte(head, ' '); i > 0 {
				cut = i
			}
		}
		chunks = append(chunks, strings.TrimRight(s[:cut], " "))
		s = strings.TrimLeft(s[cut:], " ")
		limit = rest
	}
	return append(chunks, s)
}

// wrapLines renders text as prefix plus its first chunk, then each further
// chunk on its own line under indent spaces, applying style to the text of
// every chunk. prefix may be styled; its width is measured without its
// escapes.
func wrapLines(prefix, text string, style func(...string) string, width, indent int) []string {
	first := 0
	if width > 0 {
		first = max(width-ansi.StringWidth(prefix), 1)
	}
	chunks := wrapText(text, first, width-indent)
	pad := strings.Repeat(" ", indent)
	lines := make([]string, len(chunks))
	for i, c := range chunks {
		if i == 0 {
			lines[i] = prefix + style(c)
			continue
		}
		lines[i] = pad + style(c)
	}
	return lines
}

// plain is wrapLines' style for unstyled text.
func plain(s ...string) string { return strings.Join(s, " ") }

// wrapPair renders prefix, head (unstyled) and then tail in tailStyle: on
// head's last line if it fits there, otherwise on lines of its own, both
// wrapped to width with continuation lines under indent spaces.
func wrapPair(prefix, head, tail string, tailStyle func(...string) string, width, indent int) []string {
	lines := wrapLines(prefix, head, plain, width, indent)
	if tail == "" {
		return lines
	}
	last := len(lines) - 1
	if width <= 0 || ansi.StringWidth(lines[last])+1+ansi.StringWidth(tail) <= width {
		lines[last] += tailStyle(" " + tail)
		return lines
	}
	return append(lines, wrapLines(strings.Repeat(" ", indent), tail, tailStyle, width, indent)...)
}
