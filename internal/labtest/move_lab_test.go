//go:build lab

package labtest_test

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"mtha/internal/labtest"
	"mtha/internal/plan"
	"mtha/internal/routeros"
)

// A real reorder, planned as RouterOS moves and applied (issue #5,
// docs/design-questions.md §2). Router b gets two kinds of order drift:
//
//   - input: "lab: allow ssh" re-created at the end of the chain, so it has
//     a new .id on b and is the last rule, the case where a moved rule has
//     no rule after it to go before;
//   - forward: the "lab: trusted out" block (the commented rule and the
//     uncommented rule that follows it) moved above "lab: block smb", so
//     the follower has to travel with it.
//
// input is synced A→B and forward B→A, so both routers are written. The
// plan must be moves only, each naming a destination that is on its target
// right now; applied, both chains must read in the same order on both
// routers, with no rule re-created.
func TestLabRuleOrderMoveApplied(t *testing.T) {
	lab := labtest.New(t)
	ctx := context.Background()
	requireClean(t, lab, filter)

	ssh := only(t, lab.B, filter, byComment("lab: allow ssh"))
	remove(t, lab.B, filter, ssh[".id"])
	restoreRule(t, lab.B, ssh, head(t, lab.B, filter, "forward")[".id"])
	newSSH := only(t, lab.B, filter, byComment("lab: allow ssh"))[".id"]
	if got := chainOrder(t, lab.B, filter, "input"); got[len(got)-1] != newSSH {
		t.Fatalf("lab: allow ssh (%s) is not last in b's input chain: %v", newSSH, got)
	}
	if newSSH == only(t, lab.A, filter, byComment("lab: allow ssh"))[".id"] {
		t.Fatalf("lab: allow ssh has the same .id (%s) on both routers; the test needs them to differ", newSSH)
	}

	trusted := only(t, lab.B, filter, byComment("lab: trusted out"))[".id"]
	follower := only(t, lab.B, filter, func(e map[string]string) bool { return e["chain"] == "forward" && e["dst-port"] == "25" })[".id"]
	smb := firstWithComment(t, lab.B, filter, "lab: block smb")[".id"]
	if err := lab.B.Command(ctx, "/ip/firewall/filter/move", map[string]string{
		"numbers": trusted + "," + follower, "destination": smb,
	}, nil); err != nil {
		t.Fatalf("move the lab: trusted out block on b: %v", err)
	}

	// The drift is real: the chains differ in order, and in nothing else.
	for _, chain := range []string{"input", "forward"} {
		if a, b := ruleSeq(t, lab.A, chain), ruleSeq(t, lab.B, chain); strings.Join(a, "\n") == strings.Join(b, "\n") {
			t.Fatalf("chain %s is in the same order on both routers after the churn:\n%s", chain, strings.Join(a, "\n"))
		}
	}
	d := compare(t, lab, filter, lab.Pair.Sync.Exempt)
	if len(d.Hunks) != 0 || len(d.Order) != 2 {
		t.Fatalf("want two order findings and no hunks, got %v", describe(d))
	}
	t.Logf("findings: %v", describe(d))

	before := map[string][]string{}
	for _, r := range []string{"a", "b"} {
		for _, chain := range []string{"input", "forward"} {
			before[r+" "+chain] = sorted(chainOrder(t, lab.Client(r), filter, chain))
		}
	}

	choices := map[plan.HunkRef]plan.Direction{}
	for _, o := range d.Order {
		dir := plan.AtoB
		if o.Chain == "forward" {
			dir = plan.BtoA
		}
		choices[plan.OrderRef(o)] = dir
	}
	p := buildPlan(t, lab, filter, choices)
	t.Logf("plan:\n%s", p.Render())
	if len(p.Skipped) != 0 {
		t.Fatalf("the order findings were skipped: %s", describePlan(p))
	}
	moves := 0
	for _, op := range p.Ops {
		if op.Section == "" {
			continue // the backups
		}
		if op.Method != plan.MethodCommand || op.Path != "/"+filter+"/move" {
			t.Fatalf("a reorder planned %s %s", op.Method, op.Path)
		}
		ids := chainOrder(t, lab.Client(op.Router), filter, "input")
		ids = append(ids, chainOrder(t, lab.Client(op.Router), filter, "forward")...)
		for _, field := range []string{"numbers", "destination"} {
			if !contains(ids, op.Body[field]) {
				t.Fatalf("move %v: %s %q is not a rule on router %s: %v", op.Body, field, op.Body[field], op.Router, ids)
			}
		}
		moves++
	}
	if moves == 0 || !contains(p.Targets(), "a") || !contains(p.Targets(), "b") {
		t.Fatalf("want moves on both routers: %s", describePlan(p))
	}

	execute(t, lab, p)

	for _, chain := range []string{"input", "forward"} {
		a, b := ruleSeq(t, lab.A, chain), ruleSeq(t, lab.B, chain)
		if strings.Join(a, "\n") != strings.Join(b, "\n") {
			t.Errorf("chain %s differs in order after the apply:\n  a: %s\n  b: %s", chain, strings.Join(a, "\n     "), strings.Join(b, "\n     "))
		}
		for _, r := range []string{"a", "b"} {
			if got := sorted(chainOrder(t, lab.Client(r), filter, chain)); strings.Join(got, ",") != strings.Join(before[r+" "+chain], ",") {
				t.Errorf("router %s chain %s has different rules after the moves: %v, was %v", r, chain, got, before[r+" "+chain])
			}
		}
	}
	if d := compare(t, lab, filter, lab.Pair.Sync.Exempt); !d.Clean() {
		t.Errorf("%s still drifts after the apply: %v", filter, describe(d))
	}
}

// ruleSeq lists c's static rules in chain, in list order, each as the
// fields that tell the fixture's rules apart.
func ruleSeq(t *testing.T, c *routeros.Client, chain string) []string {
	t.Helper()
	var rules []map[string]string
	if err := c.Get(context.Background(), "/"+filter, &rules); err != nil {
		t.Fatalf("%s: %v", filter, err)
	}
	var out []string
	for _, r := range rules {
		if r["dynamic"] == "true" || r["chain"] != chain {
			continue
		}
		out = append(out, fmt.Sprintf("%s %s %s/%s %s %s %s", r["comment"], r["action"], r["protocol"], r["dst-port"], r["connection-state"], r["src-address-list"], r["in-interface"]))
	}
	return out
}

func sorted(s []string) []string {
	out := append([]string(nil), s...)
	sort.Strings(out)
	return out
}

func contains(s []string, x string) bool {
	for _, v := range s {
		if v == x {
			return true
		}
	}
	return false
}
