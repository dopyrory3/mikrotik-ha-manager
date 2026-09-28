//go:build lab

package labtest_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"mtha/internal/labtest"
	"mtha/internal/ui"
)

// Every screen at 80×24 (project.md §6: "Works over SSH in a standard 80×24
// terminal") with real device data at its widest: the 400-rule scale fixture
// with operator-length comments, and drift of every shape the screens draw —
// rules only on one side, a changed rule, a block of rules in a different
// order, address-list and DNS entries with long names. #4 held the status
// bars to 80 columns against a synthetic model; this holds every line.
//
// A line wider than the terminal is cut off by Bubble Tea's renderer, and a
// view taller than it loses its top lines, title and cursor first.
//
// It fails today, on purpose (issue #17): the Overview's runtime readiness
// line is 82 columns even at baseline; the Drift screen neither scrolls nor
// fits identities built from real comments (and an order finding is one
// line of every moved identity); the Apply screen's ops and notes run to
// their full JSON and identities. outageSession finds the same for error
// text (a Drift fetch error is one unwrapped line).
func TestLabRenderEveryScreenAt80x24(t *testing.T) {
	lab := labtest.New(t)
	addScaleRules(t, lab)

	// Rules only on a, ending 341's run: identities as wide as the fixture's
	// comments make them.
	for i := 0; i < 20; i++ {
		add(t, lab.A, filter, map[string]string{
			"chain": "forward", "action": "accept", "protocol": "tcp", "dst-port": fmt.Sprint(32000 + i),
			"place-before": scaleRuleOn(t, lab.A, 351),
		})
	}
	// A changed rule with a long value.
	patch(t, lab.B, filter, scaleRuleOn(t, lab.B, 205), map[string]string{"src-address-list": "partner-replication-sources-dc2-storage-tier"})
	// Rules 391-400, a commented rule and its run, moved as a block before
	// 381 on b: an order finding naming ten long identities.
	var block []string
	for i := 391; i <= 400; i++ {
		block = append(block, scaleRuleOn(t, lab.B, i))
	}
	if err := lab.B.Command(t.Context(), "/ip/firewall/filter/move", map[string]string{
		"numbers": strings.Join(block, ","), "destination": scaleRuleOn(t, lab.B, 381),
	}, nil); err != nil {
		t.Fatalf("move rules 391-400 on b: %v", err)
	}
	for i := 1; i <= 3; i++ {
		add(t, lab.A, "ip/firewall/address-list", map[string]string{"list": "partner-replication-sources-dc2-storage-tier", "address": fmt.Sprintf("198.51.100.%d", i)})
	}
	add(t, lab.A, "ip/dns/static", map[string]string{"name": "replication-endpoint-01.storage-tier.dc2.partner.example.internal", "address": "192.0.2.10"})
	t.Logf("filter drift: %s", strings.Join(describe(compare(t, lab, filter, lab.Pair.Sync.Exempt)), "\n  "))

	d := labtest.Drive(t, ui.New(lab.Pair, true, lab.Pollers(time.Second)))
	d.Send(tea.WindowSizeMsg{Width: 80, Height: 24})
	var failures []string
	check := func(screen string) {
		failures = append(failures, fits(screen, d.Model().View(), 80, 24)...)
	}

	waitPolledBackup(t, d)
	check("overview")

	d.Send(labtest.Key("2"))
	d.Until("drift fetched", 30*time.Second, func(m ui.Model) bool {
		v := labtest.StripANSI(m.View())
		return !strings.Contains(v, "fetching drift") && strings.Contains(v, "Hunks:")
	})
	check("drift: sections")
	d.Send(labtest.Key("enter"))
	check("drift: hunks")
	for i := 0; i < 40 && !strings.Contains(labtest.StripANSI(d.Model().View()), "src-address-list:"); i++ {
		d.Send(labtest.Key("down"))
	}
	if !strings.Contains(labtest.StripANSI(d.Model().View()), "src-address-list:") {
		t.Fatalf("never reached the changed rule's hunk:\n%s", d.Model().View())
	}
	check("drift: changed hunk expanded")
	d.Send(labtest.Key("esc"), labtest.Key("down"), labtest.Key("down"))
	check("drift: address-list")

	d.Send(labtest.Key("up"), labtest.Key("up"), labtest.Key("a"), labtest.Key("4"))
	d.Until("dry run built", 30*time.Second, func(m ui.Model) bool { return strings.Contains(m.View(), "press y to apply") })
	check("apply: review")

	d.Send(labtest.Key("3"), labtest.Key("r"))
	d.Until("runtime checked", 30*time.Second, func(m ui.Model) bool {
		return strings.Contains(labtest.StripANSI(m.View()), "Router B")
	})
	check("runtime")

	d.Send(labtest.Key("6"))
	d.Until("both logs read", 30*time.Second, func(m ui.Model) bool {
		return strings.Count(labtest.StripANSI(m.View()), "event(s), read") == 2
	})
	check("events")

	d.Send(labtest.Key("?"))
	check("help")
	d.Send(labtest.Key("esc"))

	if len(failures) > 0 {
		t.Errorf("%d screen(s) do not fit 80×24 with this data:\n%s", len(failures), strings.Join(failures, "\n"))
	}
}

// fits reports how view overflows a width×height terminal: each line wider
// than width, and the line count if taller. Nothing means it fits.
func fits(screen, view string, width, height int) []string {
	var wide []string
	lines := strings.Split(view, "\n")
	for i, l := range lines {
		if w := lipgloss.Width(l); w > width {
			wide = append(wide, fmt.Sprintf("    line %d, %d columns: %s", i+1, w, labtest.StripANSI(l)))
		}
	}
	var out []string
	if len(lines) > height {
		out = append(out, fmt.Sprintf("  %s: %d lines, the terminal has %d", screen, len(lines), height))
	}
	if len(wide) > 0 {
		out = append(out, fmt.Sprintf("  %s: %d line(s) wider than %d columns:\n%s", screen, len(wide), width, strings.Join(wide, "\n")))
	}
	return out
}
