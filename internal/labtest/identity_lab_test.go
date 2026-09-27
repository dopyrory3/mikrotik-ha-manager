//go:build lab

package labtest_test

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"mtha/internal/diff"
	"mtha/internal/labtest"
	"mtha/internal/model"
	"mtha/internal/plan"
	"mtha/internal/routeros"
)

// Identity matching on real rule sets (project.md §10.1, "Firewall rule
// identity"; docs/design-questions.md §1). Each test changes one router
// (almost always b, leaving a at baseline) the way an operator would, checks
// on the raw entries that the change really landed, and then asserts the
// exact set of findings the diff reports for it: which identities, on which
// side, and which fields. An exact set is the point — "one addition" is only
// proved by there being nothing else.
//
// The fixture (testlab/provision.sh) is the rule set. Its filter chains, as
// identified at baseline:
//
//	input:   input#1 (accept established), input#2 (accept icmp),
//	         "lab: allow ssh", "lab: drop alt telnet",
//	         "input@lab: drop alt telnet#1" (drop udp 1900)
//	forward: forward#1 (accept established), forward#2 (drop invalid),
//	         "lab: block smb" (tcp 445), "lab: block smb#2" (udp 445),
//	         "lab: trusted out", "forward@lab: trusted out#1" (drop tcp 25)

const filter = "ip/firewall/filter"

// Inserting a rule at the top of a chain whose head is a commented rule is
// exactly one addition: every rule already there keeps its identity. This
// is what anchoring buys over a pure chain ordinal, and it holds through a
// sync — the one create lands at the top of the other router's chain.
//
// The fixture's own filter chains both start with uncommented rules, where
// a top insertion re-identifies that leading block instead; that is pinned
// in TestLabIdentityRulesBeforeFirstCommentedRule.
func TestLabIdentityInsertAtTopOfChain(t *testing.T) {
	lab := labtest.New(t)
	requireClean(t, lab, "ip/firewall/nat")
	requireClean(t, lab, filter)

	// nat srcnat starts with the commented "lab: masquerade". An uncommented
	// rule above it is srcnat#1, and nothing else moves.
	masq := byComment("lab: masquerade")
	id := add(t, lab.B, "ip/firewall/nat", map[string]string{
		"chain": "srcnat", "action": "masquerade", "out-interface": "ether2",
		"place-before": only(t, lab.B, "ip/firewall/nat", masq)[".id"],
	})
	requireHead(t, lab.B, "ip/firewall/nat", "srcnat", id)
	requireHunks(t, compare(t, lab, "ip/firewall/nat", lab.Pair.Sync.Exempt), "srcnat#1 [b]")
	remove(t, lab.B, "ip/firewall/nat", id)
	requireClean(t, lab, "ip/firewall/nat")

	// A filter chain in the shape operators are told to keep: give input a
	// commented head on both routers, identically.
	for _, c := range []*routeros.Client{lab.A, lab.B} {
		add(t, c, filter, map[string]string{
			"chain": "input", "action": "accept", "in-interface": "lo", "comment": "lab: top",
			"place-before": head(t, c, filter, "input")[".id"],
		})
	}
	requireClean(t, lab, filter)

	// A commented rule above it: one addition, by its comment.
	id = add(t, lab.B, filter, map[string]string{
		"chain": "input", "action": "drop", "src-address": "203.0.113.0/24", "comment": "lab: drop bogons",
		"place-before": head(t, lab.B, filter, "input")[".id"],
	})
	requireHead(t, lab.B, filter, "input", id)
	requireHunks(t, compare(t, lab, filter, lab.Pair.Sync.Exempt), "lab: drop bogons [b]")
	remove(t, lab.B, filter, id)
	requireClean(t, lab, filter)

	// An uncommented rule above it: one addition, input#1. "lab: top" and
	// the rules anchored to it (input@lab: top#1, #2) are untouched.
	id = add(t, lab.B, filter, map[string]string{
		"chain": "input", "action": "drop", "src-address": "198.51.100.0/24",
		"place-before": head(t, lab.B, filter, "input")[".id"],
	})
	requireHead(t, lab.B, filter, "input", id)
	d := compare(t, lab, filter, lab.Pair.Sync.Exempt)
	requireHunks(t, d, "input#1 [b]")

	// Syncing that one hunk b→a is one create, placed before a's "lab: top",
	// after which the chains are identical and in the same order.
	p := buildPlan(t, lab, filter, map[plan.HunkRef]plan.Direction{plan.RefOf(d.Hunks[0]): plan.BtoA})
	if creates := opsOf(p, plan.MethodCreate); len(creates) != 1 || len(p.Ops) != 2 {
		t.Fatalf("want the backup and one create, got %s", describePlan(p))
	}
	execute(t, lab, p)
	requireClean(t, lab, filter)
	requireHead(t, lab.A, filter, "input", only(t, lab.A, filter, func(e map[string]string) bool {
		return e["src-address"] == "198.51.100.0/24"
	})[".id"])
}

// Deleting a rule from the middle of a chain is exactly one removal when
// the rule is the last of its anchored block: nothing else is anchored to
// it. Deleting a commented rule that uncommented rules are anchored to
// re-anchors them to the previous commented rule — the documented bounded
// cascade, pinned here.
func TestLabIdentityDeleteFromMiddleOfChain(t *testing.T) {
	lab := labtest.New(t)
	requireClean(t, lab, filter)

	// "lab: allow ssh", third of five input rules, followed by another
	// commented rule.
	ssh := only(t, lab.B, filter, byComment("lab: allow ssh"))
	remove(t, lab.B, filter, ssh[".id"])
	requireHunks(t, compare(t, lab, filter, lab.Pair.Sync.Exempt), "lab: allow ssh [a]")
	// Put it back where it was: the chain reads clean again.
	restoreRule(t, lab.B, ssh, only(t, lab.B, filter, byComment("lab: drop alt telnet"))[".id"])
	requireClean(t, lab, filter)

	// forward#2 (drop invalid), second of six forward rules, the last of
	// the chain's leading uncommented block.
	invalid := only(t, lab.B, filter, func(e map[string]string) bool {
		return e["chain"] == "forward" && e["connection-state"] == "invalid"
	})
	remove(t, lab.B, filter, invalid[".id"])
	requireHunks(t, compare(t, lab, filter, lab.Pair.Sync.Exempt), "forward#2 [a]")
	restoreRule(t, lab.B, invalid, firstWithComment(t, lab.B, filter, "lab: block smb")[".id"])
	requireClean(t, lab, filter)

	// "lab: drop alt telnet", fourth of five, with the uncommented udp 1900
	// drop anchored to it. That rule re-anchors to "lab: allow ssh": it
	// shows as a removal plus an addition next to the one real removal.
	telnet := only(t, lab.B, filter, byComment("lab: drop alt telnet"))
	remove(t, lab.B, filter, telnet[".id"])
	requireHunks(t, compare(t, lab, filter, lab.Pair.Sync.Exempt),
		"lab: drop alt telnet [a]",
		"input@lab: drop alt telnet#1 [a]",
		"input@lab: allow ssh#1 [b]",
	)
}

// Rules before a chain's first commented rule have no anchor but the chain
// start (input#1, input#2). Adding one at the end of that block is one
// addition. Adding one at the very top of the chain re-identifies the
// whole block — the cascade anchoring bounds but cannot remove — and
// stops at the first commented rule.
func TestLabIdentityRulesBeforeFirstCommentedRule(t *testing.T) {
	lab := labtest.New(t)
	requireClean(t, lab, filter)
	ssh := only(t, lab.B, filter, byComment("lab: allow ssh"))[".id"]

	// Between input#2 and "lab: allow ssh": input#3, nothing else.
	id := add(t, lab.B, filter, map[string]string{
		"chain": "input", "action": "accept", "protocol": "tcp", "dst-port": "8291", "place-before": ssh,
	})
	requireHunks(t, compare(t, lab, filter, lab.Pair.Sync.Exempt), "input#3 [b]")
	remove(t, lab.B, filter, id)
	requireClean(t, lab, filter)

	// An uncommented rule at the very top of input: it becomes input#1, so
	// the old input#1 and #2 are compared against the rules that now hold
	// those ordinals, and the last of the block is new. Three findings for
	// one rule, and none past "lab: allow ssh".
	id = add(t, lab.B, filter, map[string]string{
		"chain": "input", "action": "accept", "protocol": "tcp", "dst-port": "8291",
		"place-before": head(t, lab.B, filter, "input")[".id"],
	})
	requireHead(t, lab.B, filter, "input", id)
	requireHunks(t, compare(t, lab, filter, lab.Pair.Sync.Exempt),
		"input#1 [both: connection-state,dst-port,protocol]",
		"input#2 [both: connection-state,protocol]",
		"input#3 [b]",
	)
	remove(t, lab.B, filter, id)
	requireClean(t, lab, filter)

	// A commented rule at the very top: the block re-anchors to it, so its
	// two rules are removals under their old identities and additions under
	// new ones. Five findings for one rule, and none past "lab: allow ssh".
	id = add(t, lab.B, filter, map[string]string{
		"chain": "input", "action": "drop", "src-address": "203.0.113.0/24", "comment": "lab: drop bogons",
		"place-before": head(t, lab.B, filter, "input")[".id"],
	})
	requireHead(t, lab.B, filter, "input", id)
	requireHunks(t, compare(t, lab, filter, lab.Pair.Sync.Exempt),
		"input#1 [a]",
		"input#2 [a]",
		"lab: drop bogons [b]",
		"input@lab: drop bogons#1 [b]",
		"input@lab: drop bogons#2 [b]",
	)
}

// The fixture's two "lab: block smb" rules are "lab: block smb" and
// "lab: block smb#2". A change to either is a hunk of its own, a change to
// both is two hunks, and syncing both is two PATCHes of two different rules.
func TestLabIdentitySharedComment(t *testing.T) {
	lab := labtest.New(t)
	requireClean(t, lab, filter)

	smb := byComment("lab: block smb")
	tcp := func(e map[string]string) bool { return smb(e) && e["protocol"] == "tcp" }
	udp := func(e map[string]string) bool { return smb(e) && e["protocol"] == "udp" }
	for _, c := range []*routeros.Client{lab.A, lab.B} {
		requireIdentity(t, lab, c, filter, tcp, "lab: block smb")
		requireIdentity(t, lab, c, filter, udp, "lab: block smb#2")
	}

	patch(t, lab.B, filter, only(t, lab.B, filter, udp)[".id"], map[string]string{"dst-port": "139"})
	requireHunks(t, compare(t, lab, filter, lab.Pair.Sync.Exempt), "lab: block smb#2 [both: dst-port]")

	patch(t, lab.B, filter, only(t, lab.B, filter, tcp)[".id"], map[string]string{"dst-port": "135"})
	d := compare(t, lab, filter, lab.Pair.Sync.Exempt)
	requireHunks(t, d, "lab: block smb [both: dst-port]", "lab: block smb#2 [both: dst-port]")

	choices := map[plan.HunkRef]plan.Direction{}
	for _, h := range d.Hunks {
		choices[plan.RefOf(h)] = plan.AtoB
	}
	p := buildPlan(t, lab, filter, choices)
	patches := opsOf(p, plan.MethodUpdate)
	if len(p.Ops) != 3 || len(patches) != 2 || patches[0].Path == patches[1].Path {
		t.Fatalf("want the backup and two PATCHes of two different rules, got %s", describePlan(p))
	}
	execute(t, lab, p)
	requireClean(t, lab, filter)
}

// The fixture has 10.10.10.10 in lab-trusted (uncommented) and lab-blocked
// (commented). Each is its own entry, keyed list|address, and a comment is
// an ordinary field.
func TestLabIdentityAddressInTwoLists(t *testing.T) {
	lab := labtest.New(t)
	const section = "ip/firewall/address-list"
	requireClean(t, lab, section)

	inList := func(list string) func(map[string]string) bool {
		return func(e map[string]string) bool { return e["list"] == list && e["address"] == "10.10.10.10" }
	}
	for _, c := range []*routeros.Client{lab.A, lab.B} {
		requireIdentity(t, lab, c, section, inList("lab-trusted"), "lab-trusted|10.10.10.10")
		requireIdentity(t, lab, c, section, inList("lab-blocked"), "lab-blocked|10.10.10.10")
	}

	remove(t, lab.B, section, only(t, lab.B, section, inList("lab-blocked"))[".id"])
	requireHunks(t, compare(t, lab, section, lab.Pair.Sync.Exempt), "lab-blocked|10.10.10.10 [a]")

	patch(t, lab.B, section, only(t, lab.B, section, inList("lab-trusted"))[".id"], map[string]string{"comment": "lab: now commented"})
	requireHunks(t, compare(t, lab, section, lab.Pair.Sync.Exempt),
		"lab-blocked|10.10.10.10 [a]",
		"lab-trusted|10.10.10.10 [both: comment]",
	)

	add(t, lab.B, section, map[string]string{"list": "lab-third", "address": "10.10.10.10", "comment": "lab: in two lists"})
	requireHunks(t, compare(t, lab, section, lab.Pair.Sync.Exempt),
		"lab-blocked|10.10.10.10 [a]",
		"lab-third|10.10.10.10 [b]",
		"lab-trusted|10.10.10.10 [both: comment]",
	)
}

// svc.lab.example carries two A records sharing a comment. Each is
// name|type|value; a third record under the name is one addition, dropping
// one is one removal, and a comment edit is a field change.
func TestLabIdentityDNSNameWithSeveralAddresses(t *testing.T) {
	lab := labtest.New(t)
	const section = "ip/dns/static"
	requireClean(t, lab, section)

	record := func(addr string) func(map[string]string) bool {
		return func(e map[string]string) bool { return e["name"] == "svc.lab.example" && e["address"] == addr }
	}
	cname := func(e map[string]string) bool { return e["name"] == "www.lab.example" }
	for _, c := range []*routeros.Client{lab.A, lab.B} {
		requireIdentity(t, lab, c, section, record("192.168.88.10"), "svc.lab.example|A|192.168.88.10")
		requireIdentity(t, lab, c, section, record("192.168.88.11"), "svc.lab.example|A|192.168.88.11")
		requireIdentity(t, lab, c, section, cname, "www.lab.example|CNAME|svc.lab.example")
	}

	add(t, lab.B, section, map[string]string{"name": "svc.lab.example", "address": "192.168.88.12", "comment": "lab: two addresses"})
	requireHunks(t, compare(t, lab, section, lab.Pair.Sync.Exempt), "svc.lab.example|A|192.168.88.12 [b]")

	remove(t, lab.B, section, only(t, lab.B, section, record("192.168.88.10"))[".id"])
	requireHunks(t, compare(t, lab, section, lab.Pair.Sync.Exempt),
		"svc.lab.example|A|192.168.88.10 [a]",
		"svc.lab.example|A|192.168.88.12 [b]",
	)

	patch(t, lab.B, section, only(t, lab.B, section, record("192.168.88.11"))[".id"], map[string]string{"comment": "lab: renamed comment"})
	requireHunks(t, compare(t, lab, section, lab.Pair.Sync.Exempt),
		"svc.lab.example|A|192.168.88.10 [a]",
		"svc.lab.example|A|192.168.88.11 [both: comment]",
		"svc.lab.example|A|192.168.88.12 [b]",
	)
}

// Scripts and schedulers are keyed on name, so a rename is the old name
// removed and the new one added — and nothing else.
func TestLabIdentityRenameScriptAndScheduler(t *testing.T) {
	lab := labtest.New(t)

	for _, c := range []struct{ section, from, to string }{
		{"system/script", "lab-hello", "lab-hello-renamed"},
		{"system/scheduler", "lab-daily", "lab-nightly"},
	} {
		requireClean(t, lab, c.section)
		patch(t, lab.B, c.section, only(t, lab.B, c.section, byName(c.from))[".id"], map[string]string{"name": c.to})
		only(t, lab.B, c.section, byName(c.to))
		requireHunks(t, compare(t, lab, c.section, lab.Pair.Sync.Exempt), c.from+" [a]", c.to+" [b]")
	}
}

// Only mtha:-tagged routes are compared. Untagged static routes (added,
// re-pointed, or commented without the tag), connected routes and other
// dynamic routes that exist on one router only never reach the diff. A
// change to the tagged route does, as exactly one hunk.
func TestLabIdentityRoutesOutsideTheDiff(t *testing.T) {
	lab := labtest.New(t)
	ctx := context.Background()
	const section = "ip/route"
	requireClean(t, lab, section)

	add(t, lab.B, section, map[string]string{"dst-address": "10.97.0.0/16", "gateway": "192.168.88.254"})
	add(t, lab.B, section, map[string]string{"dst-address": "10.96.0.0/16", "gateway": "192.168.88.254", "comment": "lab: not tagged"})
	untagged := func(e map[string]string) bool { return e["dst-address"] == "10.98.0.0/16" }
	patch(t, lab.B, section, only(t, lab.B, section, untagged)[".id"], map[string]string{"gateway": "192.168.88.253"})
	// An address on b alone brings a connected (dynamic) route on b alone.
	add(t, lab.B, "ip/address", map[string]string{"address": "172.31.255.1/24", "interface": "ether2", "comment": "lab: b only"})

	// The churn is real: b now has routes a has not, and the two routers
	// disagree on the untagged route they share.
	eventually(t, "b's connected route for 172.31.255.0/24", 15*time.Second, func() error {
		var routes []map[string]string
		if err := lab.B.Get(ctx, "/ip/route?dst-address=172.31.255.0/24", &routes); err != nil {
			return err
		}
		if len(routes) != 1 || routes[0]["dynamic"] != "true" || routes[0]["connect"] != "true" {
			return fmt.Errorf("routes %v", routes)
		}
		return nil
	})
	aRoutes, bRoutes := routeTable(t, lab.A), routeTable(t, lab.B)
	for _, dst := range []string{"10.97.0.0/16", "10.96.0.0/16", "172.31.255.0/24"} {
		if _, ok := aRoutes[dst]; ok {
			t.Fatalf("router a has a route to %s too", dst)
		}
		if _, ok := bRoutes[dst]; !ok {
			t.Fatalf("router b has no route to %s: %v", dst, bRoutes)
		}
	}
	if aRoutes["10.98.0.0/16"] == bRoutes["10.98.0.0/16"] {
		t.Fatalf("the untagged 10.98.0.0/16 route is %q on both routers", aRoutes["10.98.0.0/16"])
	}
	t.Logf("routes on a: %v", aRoutes)
	t.Logf("routes on b: %v", bRoutes)
	requireClean(t, lab, section)

	// The tagged route is compared: one change, one hunk.
	tagged := byComment("mtha:lab-route")
	patch(t, lab.B, section, only(t, lab.B, section, tagged)[".id"], map[string]string{"distance": "5"})
	requireHunks(t, compare(t, lab, section, lab.Pair.Sync.Exempt), "mtha:lab-route [both: distance]")
}

// routeTable maps each of c's routes' dst-address to its gateway and flags,
// static and dynamic alike.
func routeTable(t *testing.T, c *routeros.Client) map[string]string {
	t.Helper()
	var routes []map[string]string
	if err := c.Get(context.Background(), "/ip/route", &routes); err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, r := range routes {
		out[r["dst-address"]] += fmt.Sprintf("[gw=%s dynamic=%s connect=%s comment=%q]", r["gateway"], r["dynamic"], r["connect"], r["comment"])
	}
	return out
}

// A permutation of commented rules — every identity still matching — was
// invisible to the diff when the issue was written. Order detection
// (docs/design-questions.md §2) now reports it as one order finding for the
// chain, with no hunks, and the planner skips it with a reason instead of
// writing anything.
func TestLabIdentityReorderIsAnOrderFinding(t *testing.T) {
	lab := labtest.New(t)
	ctx := context.Background()
	requireClean(t, lab, filter)

	// Move "lab: allow ssh" to the end of b's input chain: before the first
	// forward rule, which follows the input rules in the list. The rule
	// after it keeps its anchor ("lab: drop alt telnet"), so every
	// identity is unchanged.
	ssh := only(t, lab.B, filter, byComment("lab: allow ssh"))[".id"]
	if err := lab.B.Command(ctx, "/ip/firewall/filter/move", map[string]string{
		"numbers": ssh, "destination": head(t, lab.B, filter, "forward")[".id"],
	}, nil); err != nil {
		t.Fatalf("move lab: allow ssh on b: %v", err)
	}
	if got := chainOrder(t, lab.B, filter, "input"); got[len(got)-1] != ssh {
		t.Fatalf("lab: allow ssh (%s) is not last in b's input chain: %v", ssh, got)
	}

	d := compare(t, lab, filter, lab.Pair.Sync.Exempt)
	requireHunks(t, d, "order input: 1 of 5 moved: lab: allow ssh")

	p := buildPlan(t, lab, filter, map[plan.HunkRef]plan.Direction{plan.OrderRef(d.Order[0]): plan.AtoB})
	if !p.Empty() {
		t.Fatalf("an order finding planned writes: %s", describePlan(p))
	}
	if len(p.Skipped) != 1 || p.Skipped[0].Ref.Chain != "input" || !strings.Contains(p.Skipped[0].Reason, "move is not implemented") {
		t.Fatalf("want the input order finding skipped with a reason, got %s", describePlan(p))
	}
	t.Logf("skipped: %s: %s", p.Skipped[0].Where(), p.Skipped[0].Reason)
}

// requireHunks fails unless d's findings are exactly want, each written
//
//	<identity> [a] | [b] | [both: <fields, sorted>]
//	order <chain>: <moved> of <rules> moved: <identities>
//
// in any order, with " (occurrence n)" after an identity whose occurrence
// is not the first.
func requireHunks(t *testing.T, d diff.SectionDiff, want ...string) {
	t.Helper()
	got := describe(d)
	sort.Strings(want)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("%s findings:\n  got:  %s\n  want: %s", d.Section, strings.Join(got, "\n        "), strings.Join(want, "\n        "))
	}
}

func describe(d diff.SectionDiff) []string {
	out := []string{}
	for _, h := range d.Hunks {
		id := ref(h.Identity, h.Occurrence)
		switch {
		case h.OnA && h.OnB:
			fields := make([]string, len(h.Changes))
			for i, c := range h.Changes {
				fields[i] = c.Field
			}
			sort.Strings(fields)
			out = append(out, fmt.Sprintf("%s [both: %s]", id, strings.Join(fields, ",")))
		case h.OnA:
			out = append(out, id+" [a]")
		default:
			out = append(out, id+" [b]")
		}
	}
	for _, o := range d.Order {
		moved := make([]string, len(o.Moved))
		for i, r := range o.Moved {
			moved[i] = ref(r.Identity, r.Occurrence)
		}
		out = append(out, fmt.Sprintf("order %s: %d of %d moved: %s", o.Chain, len(o.Moved), o.Rules, strings.Join(moved, ", ")))
	}
	sort.Strings(out)
	return out
}

func ref(identity string, occurrence int) string {
	if occurrence == 0 {
		return identity
	}
	return fmt.Sprintf("%s (occurrence %d)", identity, occurrence+1)
}

// requireIdentity asserts that the one entry of section on c that match
// accepts is identified as want, as the diff identifies it.
func requireIdentity(t *testing.T, lab *labtest.Lab, c *routeros.Client, section string, match func(map[string]string) bool, want string) {
	t.Helper()
	raw, err := c.GetSection(context.Background(), section)
	if err != nil {
		t.Fatal(err)
	}
	// Normalize keeps what Select keeps, in the same order, so the two
	// lists are parallel.
	kept := model.Select(section, raw)
	ids := model.BuildIdentities(section, model.Normalize(section, raw, lab.Pair.Sync.Exempt))
	var found []string
	for i, e := range kept {
		if match(strs(e)) {
			found = append(found, ids[i])
		}
	}
	if len(found) != 1 || found[0] != want {
		t.Fatalf("%s: matching entries are identified %q, want [%q]", section, found, want)
	}
}

func strs(e model.Entry) map[string]string {
	out := make(map[string]string, len(e))
	for k, v := range e {
		out[k] = fmt.Sprint(v)
	}
	return out
}

func byComment(comment string) func(map[string]string) bool {
	return func(e map[string]string) bool { return e["comment"] == comment }
}

func byName(name string) func(map[string]string) bool {
	return func(e map[string]string) bool { return e["name"] == name }
}

// chainOrder lists the .id of c's static rules in chain, in list order.
func chainOrder(t *testing.T, c *routeros.Client, section, chain string) []string {
	t.Helper()
	var rules []map[string]string
	if err := c.Get(context.Background(), "/"+section, &rules); err != nil {
		t.Fatalf("%s: %v", section, err)
	}
	var ids []string
	for _, r := range rules {
		if r["dynamic"] != "true" && r["chain"] == chain {
			ids = append(ids, r[".id"])
		}
	}
	return ids
}

// head is the first static rule of chain on c.
func head(t *testing.T, c *routeros.Client, section, chain string) map[string]string {
	t.Helper()
	ids := chainOrder(t, c, section, chain)
	if len(ids) == 0 {
		t.Fatalf("%s: chain %s is empty", section, chain)
	}
	return only(t, c, section, func(e map[string]string) bool { return e[".id"] == ids[0] })
}

// requireHead asserts the rule id really is at the top of chain on c: the
// churn has to be where the test says before its diff means anything.
func requireHead(t *testing.T, c *routeros.Client, section, chain, id string) {
	t.Helper()
	if got := chainOrder(t, c, section, chain); len(got) == 0 || got[0] != id {
		t.Fatalf("%s: %s is not at the top of chain %s: %v", section, id, chain, got)
	}
}

// firstWithComment is the first static entry of section on c carrying
// comment, for a comment the fixture shares between entries.
func firstWithComment(t *testing.T, c *routeros.Client, section, comment string) map[string]string {
	t.Helper()
	var entries []map[string]string
	if err := c.Get(context.Background(), "/"+section, &entries); err != nil {
		t.Fatalf("%s: %v", section, err)
	}
	for _, e := range entries {
		if e["dynamic"] != "true" && e["comment"] == comment {
			return e
		}
	}
	t.Fatalf("%s: no entry commented %q", section, comment)
	return nil
}

// add creates an entry and returns its .id.
func add(t *testing.T, c *routeros.Client, section string, body map[string]string) string {
	t.Helper()
	var created map[string]string
	if err := c.Post(context.Background(), "/"+section, body, &created); err != nil {
		t.Fatalf("add to %s %v: %v", section, body, err)
	}
	if created[".id"] == "" {
		t.Fatalf("add to %s %v: no .id in the response %v", section, body, created)
	}
	return created[".id"]
}

func remove(t *testing.T, c *routeros.Client, section, id string) {
	t.Helper()
	if err := c.Delete(context.Background(), "/"+section+"/"+id); err != nil {
		t.Fatalf("remove %s %s: %v", section, id, err)
	}
}

func patch(t *testing.T, c *routeros.Client, section, id string, body map[string]string) {
	t.Helper()
	if err := c.Patch(context.Background(), "/"+section+"/"+id, body, nil); err != nil {
		t.Fatalf("patch %s %s %v: %v", section, id, body, err)
	}
}

// restoreRule re-creates a removed firewall rule from its read form, before
// the rule id, so the chain is as it was.
func restoreRule(t *testing.T, c *routeros.Client, rule map[string]string, before string) {
	t.Helper()
	body := map[string]string{"place-before": before}
	for _, f := range []string{"chain", "action", "protocol", "dst-port", "connection-state", "log", "log-prefix", "comment", "in-interface", "src-address-list", "disabled"} {
		if v := rule[f]; v != "" {
			body[f] = v
		}
	}
	add(t, c, filter, body)
}

// buildPlan plans choices for section from fresh reads, as the sync screen
// does.
func buildPlan(t *testing.T, lab *labtest.Lab, section string, choices map[plan.HunkRef]plan.Direction) plan.Plan {
	t.Helper()
	ctx := context.Background()
	a, err := lab.A.GetSection(ctx, section)
	if err != nil {
		t.Fatal(err)
	}
	b, err := lab.B.GetSection(ctx, section)
	if err != nil {
		t.Fatal(err)
	}
	return plan.Build([]plan.SectionInput{{Section: section, A: a, B: b, Choices: choices}}, plan.Options{
		Exempt:     lab.Pair.Sync.Exempt,
		BackupName: "mtha-lab-identity",
		Users:      map[string]string{"a": labtest.User, "b": labtest.User},
	})
}

func execute(t *testing.T, lab *labtest.Lab, p plan.Plan) {
	t.Helper()
	for _, op := range p.Ops {
		if err := plan.Execute(context.Background(), lab.Client(op.Router), op); err != nil {
			t.Fatalf("%s %s %v: %v", op.Method, op.Path, op.Body, err)
		}
	}
}

func opsOf(p plan.Plan, m plan.Method) []plan.Op {
	var out []plan.Op
	for _, op := range p.Ops {
		if op.Method == m && op.Section != "" {
			out = append(out, op)
		}
	}
	return out
}

func describePlan(p plan.Plan) string {
	var b strings.Builder
	for _, op := range p.Ops {
		fmt.Fprintf(&b, "\n  %s %s %s %v (%s)", op.Router, op.Method, op.Path, op.Body, op.Note)
	}
	for _, s := range p.Skipped {
		fmt.Fprintf(&b, "\n  skipped %s: %s", s.Where(), s.Reason)
	}
	return b.String()
}
