//go:build lab

package labtest_test

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"mtha/internal/diff"
	"mtha/internal/labtest"
	"mtha/internal/plan"
	"mtha/internal/routeros"
	"mtha/internal/ui"
)

// Scale (issue #17): a firewall filter section the size of a real edge
// router's, on both routers identically, and what drift, apply and the
// screens do with it.
//
// The fixture is scaleRules rules appended to the forward chain, after the
// baseline's own. That puts them in a chain the lab actually evaluates, and
// after the fixture's commented "lab: trusted out" run, so the identities
// are the ones a real rule set gets: one in ten rules is commented, and the
// nine uncommented rules after it are anchored to it ("forward@<comment>#n").
// Every rule matches its own dst-port, which no lab traffic uses, so none of
// them ever changes what the pair forwards.
//
// 400 rules keeps each test's setup to a couple of seconds (one /execute of
// 100 adds takes well under a second on a CHR guest) and the whole of this
// file to a few minutes, most of it the resets between tests.
const scaleRules = 400

// scaleRule is rule i (1-based) of the scale fixture, as an add body.
func scaleRule(i int) map[string]string {
	r := map[string]string{
		"chain":    "forward",
		"action":   "accept",
		"protocol": "tcp",
		"dst-port": fmt.Sprint(20000 + i),
	}
	if i%2 == 0 {
		r["protocol"] = "udp"
	}
	if i%7 == 0 {
		r["action"] = "drop"
	}
	if i%10 == 1 {
		// Long, as operators write them: this is the data that is widest on
		// screen.
		r["comment"] = fmt.Sprintf("lab-scale %03d: allow partner replication to dc2 storage tier", i)
	}
	return r
}

// scaleComment is the comment of the commented rule i (i%10 == 1).
func scaleComment(i int) string { return scaleRule(i)["comment"] }

// addScaleRules appends the scale fixture to both routers' forward chains,
// identically, and checks the pair is still drift-free.
func addScaleRules(t *testing.T, lab *labtest.Lab) {
	t.Helper()
	start := time.Now()
	for _, c := range []*routeros.Client{lab.A, lab.B} {
		// One /execute per 100 rules: one REST add each would take minutes,
		// and one script for the lot is a long request body.
		for from := 1; from <= scaleRules; from += 100 {
			var script strings.Builder
			for i := from; i < from+100 && i <= scaleRules; i++ {
				script.WriteString("/ip/firewall/filter/add")
				for _, k := range []string{"chain", "action", "protocol", "dst-port", "comment"} {
					if v, ok := scaleRule(i)[k]; ok {
						fmt.Fprintf(&script, " %s=%q", k, v)
					}
				}
				script.WriteString("\n")
			}
			if err := c.Command(context.Background(), "/execute", map[string]string{"script": script.String(), "as-string": ""}, nil); err != nil {
				t.Fatalf("add scale rules %d-: %v", from, err)
			}
		}
	}
	for _, c := range []*routeros.Client{lab.A, lab.B} {
		var rules []map[string]string
		if err := c.Get(context.Background(), "/"+filter, &rules); err != nil {
			t.Fatal(err)
		}
		if want := 11 + scaleRules; len(rules) != want {
			t.Fatalf("%d filter rules after adding the scale fixture, want %d", len(rules), want)
		}
	}
	t.Logf("scale fixture: %d rules on each router in %s", scaleRules, time.Since(start).Round(time.Millisecond))
	requireClean(t, lab, filter)
}

// scaleRuleOn returns the .id of scale rule i on c, found by its dst-port
// (unique across the whole section).
func scaleRuleOn(t *testing.T, c *routeros.Client, i int) string {
	t.Helper()
	port := fmt.Sprint(20000 + i)
	return only(t, c, filter, func(e map[string]string) bool { return e["dst-port"] == port })[".id"]
}

// ruleSignatures is c's forward chain in list order, one string per rule
// carrying every field the scale tests set, for comparing two routers'
// chains rule by rule.
func ruleSignatures(t *testing.T, c *routeros.Client, chain string) []string {
	t.Helper()
	var rules []map[string]string
	if err := c.Get(context.Background(), "/"+filter, &rules); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, r := range rules {
		if r["chain"] != chain || r["dynamic"] == "true" {
			continue
		}
		out = append(out, fmt.Sprintf("%s %s %s %s %q %s", r["action"], r["protocol"], r["dst-port"], r["connection-state"], r["comment"], r["src-address"]))
	}
	return out
}

// Drift over a large section: reading, identifying and comparing it, both
// directly and as the Drift screen does over all twelve sections, stays in
// the seconds, and a clean pair still reads clean — 400 more rules give the
// identity scheme 400 more chances to see a difference that is not there.
func TestLabScaleDriftTiming(t *testing.T) {
	lab := labtest.New(t)
	ctx := context.Background()
	addScaleRules(t, lab)

	start := time.Now()
	a, err := lab.A.GetSection(ctx, filter)
	if err != nil {
		t.Fatal(err)
	}
	b, err := lab.B.GetSection(ctx, filter)
	if err != nil {
		t.Fatal(err)
	}
	read := time.Since(start)
	start = time.Now()
	d := diff.Compare(filter, a, b, lab.Pair.Sync.Exempt)
	compared := time.Since(start)
	if !d.Clean() {
		t.Fatalf("%d identical rules drift: %v", len(a), describe(d))
	}
	t.Logf("%s: read %d+%d rules in %s, compared in %s", filter, len(a), len(b), read.Round(time.Millisecond), compared.Round(time.Microsecond))

	// One changed rule in the middle is exactly one finding, on the
	// identity that rule has.
	mid := scaleRules/2 + 5 // uncommented: the 4th after commented rule 201
	patch(t, lab.B, filter, scaleRuleOn(t, lab.B, mid), map[string]string{"src-address": "198.51.100.0/24"})
	requireHunks(t, compare(t, lab, filter, lab.Pair.Sync.Exempt),
		fmt.Sprintf("forward@%s#4 [both: src-address]", scaleComment(mid-4)))

	// The Drift screen: every section, both routers, concurrently.
	d2 := labtest.Drive(t, ui.New(lab.Pair, false, lab.Pollers(time.Second)))
	d2.Send(tea.WindowSizeMsg{Width: 80, Height: 24})
	start = time.Now()
	d2.Send(labtest.Key("2"))
	m := d2.Until("drift fetched", 30*time.Second, func(m ui.Model) bool { return !strings.Contains(m.View(), "fetching drift") })
	t.Logf("Drift screen fetched and compared %d sections in %s", len(lab.Pair.Sync.Sections), time.Since(start).Round(time.Millisecond))
	view := labtest.StripANSI(m.View())
	if !regexp.MustCompile(`> ip/firewall/filter\s+1 hunk\(s\)`).MatchString(view) {
		t.Fatalf("Drift screen does not show the one filter hunk:\n%s", view)
	}
	if n := strings.Count(view, " clean"); n != len(lab.Pair.Sync.Sections)-1 {
		t.Errorf("Drift screen shows %d clean sections, want %d:\n%s", n, len(lab.Pair.Sync.Sections)-1, view)
	}
}

// An apply that lands in the middle of a 400-rule chain: three rules created
// there (two commented and adjacent, one ending an uncommented run), one
// changed, one removed, synced a→b from the Apply screen. place-before has to put each
// create exactly where it is on a; afterwards the two chains are identical
// rule for rule, not merely drift-free.
func TestLabScaleApplyMiddleOfChain(t *testing.T) {
	lab := labtest.New(t)
	addScaleRules(t, lab)

	// On a: a commented rule before commented rule 201, and an uncommented
	// one before commented rule 301 — the last of 291's run, so nothing after
	// it is re-identified.
	newCommented := map[string]string{
		"chain": "forward", "action": "drop", "protocol": "tcp", "dst-port": "31001",
		"comment": "lab-scale mid: block legacy partner sync", "place-before": scaleRuleOn(t, lab.A, 201),
	}
	newUncommented := map[string]string{
		"chain": "forward", "action": "accept", "protocol": "udp", "dst-port": "31002",
		"place-before": scaleRuleOn(t, lab.A, 301),
	}
	add(t, lab.A, filter, newCommented)
	add(t, lab.A, filter, newUncommented)
	// And a second commented rule straight after the first: two creates
	// sharing one anchor must land in order.
	add(t, lab.A, filter, map[string]string{
		"chain": "forward", "action": "accept", "protocol": "tcp", "dst-port": "31004",
		"comment": "lab-scale mid: allow legacy partner sync from dc2", "place-before": scaleRuleOn(t, lab.A, 201),
	})
	// On b: rule 155 changed, and an extra rule ending 241's run.
	patch(t, lab.B, filter, scaleRuleOn(t, lab.B, 155), map[string]string{"action": "drop"})
	add(t, lab.B, filter, map[string]string{
		"chain": "forward", "action": "accept", "protocol": "tcp", "dst-port": "31003",
		"place-before": scaleRuleOn(t, lab.B, 251),
	})

	requireHunks(t, compare(t, lab, filter, lab.Pair.Sync.Exempt),
		"lab-scale mid: block legacy partner sync [a]",
		"lab-scale mid: allow legacy partner sync from dc2 [a]",
		fmt.Sprintf("forward@%s#10 [a]", scaleComment(291)),
		fmt.Sprintf("forward@%s#4 [both: action]", scaleComment(151)),
		fmt.Sprintf("forward@%s#10 [b]", scaleComment(241)),
	)

	// The plan the Apply screen will build, inspected before it runs: each
	// create anchored to the rule that follows it on a.
	d := compare(t, lab, filter, lab.Pair.Sync.Exempt)
	choices := map[plan.HunkRef]plan.Direction{}
	for _, h := range d.Hunks {
		choices[plan.RefOf(h)] = plan.AtoB
	}
	start := time.Now()
	p := buildPlan(t, lab, filter, choices)
	t.Logf("planned %d ops in %s:%s", len(p.Ops), time.Since(start).Round(time.Millisecond), describePlan(p))
	creates := opsOf(p, plan.MethodCreate)
	if len(creates) != 3 || len(opsOf(p, plan.MethodUpdate)) != 1 || len(opsOf(p, plan.MethodDelete)) != 1 {
		t.Fatalf("want 3 creates, 1 update and 1 delete on b:%s", describePlan(p))
	}
	for i, anchor := range []int{201, 201, 301} {
		if want := "placed before " + scaleComment(anchor); !strings.HasSuffix(creates[i].Note, want) || creates[i].Body["place-before"] != scaleRuleOn(t, lab.B, anchor) {
			t.Errorf("create %d is not %s: %s %v", i+1, want, creates[i].Note, creates[i].Body)
		}
	}

	d2 := labtest.Drive(t, ui.New(lab.Pair, true, lab.Pollers(time.Second)))
	d2.Send(tea.WindowSizeMsg{Width: 80, Height: 24})
	waitPolledBackup(t, d2)
	d2.Send(labtest.Key("2"))
	d2.Until("drift fetched", 30*time.Second, func(m ui.Model) bool {
		return regexp.MustCompile(`ip/firewall/filter\s+5 hunk\(s\)`).MatchString(labtest.StripANSI(m.View()))
	})
	d2.Send(labtest.Key("a"), labtest.Key("4"))
	d2.Until("dry run built", 30*time.Second, func(m ui.Model) bool {
		return strings.Contains(m.View(), "press y to apply")
	})
	start = time.Now()
	d2.Send(labtest.Key("y"))
	m := d2.Until("apply finished and verified", 2*time.Minute, func(m ui.Model) bool {
		v := m.View()
		return strings.Contains(v, "applied ") || strings.Contains(v, "apply stopped")
	})
	view := labtest.StripANSI(m.View())
	t.Logf("apply of %d ops, with verification, took %s", len(p.Ops), time.Since(start).Round(time.Millisecond))
	if !strings.Contains(view, fmt.Sprintf("applied %d/%d op(s)", len(p.Ops), len(p.Ops))) || !regexp.MustCompile(`ip/firewall/filter\s+clean`).MatchString(view) {
		t.Fatalf("apply did not finish clean:\n%s", view)
	}

	sigA, sigB := ruleSignatures(t, lab.A, "forward"), ruleSignatures(t, lab.B, "forward")
	if len(sigA) != len(sigB) {
		t.Fatalf("forward chain: %d rules on a, %d on b", len(sigA), len(sigB))
	}
	for i := range sigA {
		if sigA[i] != sigB[i] {
			t.Fatalf("forward chain differs at rule %d of %d:\n  a: %s\n  b: %s", i+1, len(sigA), sigA[i], sigB[i])
		}
	}
	requireClean(t, lab, filter)
}

// baselineRoles matches an Overview line showing router a polled as VRRP
// master and b as backup: the panels sit side by side, so both roles are on
// one line.
var baselineRoles = regexp.MustCompile(`vrrp vrrp-lan\s+master\b.*vrrp vrrp-lan\s+backup\b`)

// waitPolledBackup waits on the Overview until both routers are polled, a as
// master and b as backup, so one y confirms a plan that writes only to b.
func waitPolledBackup(t *testing.T, d *labtest.Driver[ui.Model]) {
	t.Helper()
	d.Send(labtest.Key("1"))
	d.Until("a polled as master and b as backup", time.Minute, func(m ui.Model) bool {
		return baselineRoles.MatchString(labtest.StripANSI(m.View()))
	})
}
