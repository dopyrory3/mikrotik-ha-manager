//go:build lab

package labtest_test

import (
	"regexp"
	"strings"
	"testing"

	"mtha/internal/labtest"
	"mtha/internal/plan"
)

// Per-operation coverage of the apply (issue #13). Each test changes one
// router the way an operator would, confirms the drift is exactly that
// change, applies it through the Apply screen, and asserts over REST that
// the other router now has it — in the same place — and that the pair is
// drift-free again.

// Create: a rule added on a mid-chain is created on b before the same
// neighbour, so it lands at the same position.
func TestLabApplyCreate(t *testing.T) {
	lab := labtest.New(t)
	requireClean(t, lab, filter)

	// Between "lab: block smb#2" and "lab: trusted out" in forward: a
	// commented rule, so no other rule's identity moves.
	newRule := byComment("lab: apply new")
	add(t, lab.A, filter, map[string]string{
		"chain": "forward", "action": "drop", "protocol": "tcp", "dst-port": "3389", "comment": "lab: apply new",
		"place-before": only(t, lab.A, filter, byComment("lab: trusted out"))[".id"],
	})
	requireHunks(t, compare(t, lab, filter, lab.Pair.Sync.Exempt), "lab: apply new [a]")

	s := startTUI(t, lab, tuiOptions{write: true})
	s.drift()
	s.selectSection(filter, plan.AtoB)
	v := s.plan()
	ops := shownOps(t, v)
	if len(ops) != 2 || ops[1].Method != "PUT" || ops[1].Body["place-before"] != only(t, lab.B, filter, byComment("lab: trusted out"))[".id"] {
		t.Fatalf("want the backup and one PUT placed before b's \"lab: trusted out\", got %v:\n%s", ops, v)
	}
	requireApplied(t, s.confirm(false))

	requireSamePosition(t, lab, filter, "forward", newRule)
	got := only(t, lab.B, filter, newRule)
	for k, want := range map[string]string{"action": "drop", "protocol": "tcp", "dst-port": "3389"} {
		if got[k] != want {
			t.Errorf("b's rule %s = %q, want %q", k, got[k], want)
		}
	}
	requireClean(t, lab, filter)
}

// Delete: a rule removed on a is removed on b, and nothing else in its
// chain moves.
func TestLabApplyDelete(t *testing.T) {
	lab := labtest.New(t)
	requireClean(t, lab, filter)

	ssh := byComment("lab: allow ssh")
	remove(t, lab.A, filter, only(t, lab.A, filter, ssh)[".id"])
	requireHunks(t, compare(t, lab, filter, lab.Pair.Sync.Exempt), "lab: allow ssh [b]")
	wantB := chainOrder(t, lab.B, filter, "input")
	wantB = drop(wantB, only(t, lab.B, filter, ssh)[".id"])

	s := startTUI(t, lab, tuiOptions{write: true})
	s.drift()
	s.selectSection(filter, plan.AtoB)
	v := s.plan()
	if ops := shownOps(t, v); len(ops) != 2 || ops[1].Method != "DELETE" {
		t.Fatalf("want the backup and one DELETE, got %v:\n%s", ops, v)
	}
	requireApplied(t, s.confirm(false))

	if pos := chainPos(t, lab.B, filter, "input", ssh); pos != -1 {
		t.Fatalf("\"lab: allow ssh\" is still on b, at position %d", pos)
	}
	if got := chainOrder(t, lab.B, filter, "input"); strings.Join(got, ",") != strings.Join(wantB, ",") {
		t.Errorf("b's input chain is %v, want %v (the rest untouched, in order)", got, wantB)
	}
	requireClean(t, lab, filter)
}

// Change a field: a PATCH in place, so b's rule keeps its .id and position.
func TestLabApplyChangeField(t *testing.T) {
	lab := labtest.New(t)
	requireClean(t, lab, filter)

	ssh := byComment("lab: allow ssh")
	patch(t, lab.A, filter, only(t, lab.A, filter, ssh)[".id"], map[string]string{"dst-port": "2222"})
	requireHunks(t, compare(t, lab, filter, lab.Pair.Sync.Exempt), "lab: allow ssh [both: dst-port]")
	before := only(t, lab.B, filter, ssh)
	posBefore := chainPos(t, lab.B, filter, "input", ssh)

	s := startTUI(t, lab, tuiOptions{write: true})
	s.drift()
	s.selectSection(filter, plan.AtoB)
	v := s.plan()
	if ops := shownOps(t, v); len(ops) != 2 || ops[1].Method != "PATCH" || ops[1].Body["dst-port"] != "2222" || len(ops[1].Body) != 1 {
		t.Fatalf("want the backup and one PATCH of dst-port alone, got %v:\n%s", ops, v)
	}
	requireApplied(t, s.confirm(false))

	after := only(t, lab.B, filter, ssh)
	if after["dst-port"] != "2222" {
		t.Errorf("b's dst-port = %q, want 2222", after["dst-port"])
	}
	if after[".id"] != before[".id"] {
		t.Errorf("b's rule was recreated (.id %s -> %s), not patched", before[".id"], after[".id"])
	}
	if pos := chainPos(t, lab.B, filter, "input", ssh); pos != posBefore {
		t.Errorf("b's rule moved from position %d to %d", posBefore, pos)
	}
	requireSamePosition(t, lab, filter, "input", ssh)
	requireClean(t, lab, filter)
}

// Reset to default: a leaves a field at its default while b sets it, so
// syncing a→b plans whatever RouterOS accepts to clear that field, and the
// device must then show the default. One case per field, each its own apply
// of that one hunk, so every refused field is reported rather than hidden
// behind the first. (TestLabSyncFieldBackToDefault covers a firewall rule's
// disabled and log.)
//
// The mechanism differs per field (issue #25, docs/lab-rest-contract.md):
// RouterOS 7.23.7 accepts the unset command for a firewall rule's
// src-address and refuses "" there, but refuses unset for its log-prefix
// and for a lease's address-lists ("input does not match any value of
// value-name", 400), where PATCHing "" is accepted and reads back as the
// default. The dry run must show the operation that is actually sent.
func TestLabApplyUnsetToDefault(t *testing.T) {
	lab := labtest.New(t)

	cases := []struct {
		name, section, field, value string
		find                        func(map[string]string) bool
		unset                       bool // the unset command, rather than a PATCH to ""
	}{
		{"firewall src-address", filter, "src-address", "10.0.0.0/8", byComment("lab: allow ssh"), true},
		{"firewall log-prefix", filter, "log-prefix", "lab-x", byComment("lab: trusted out"), false},
		{"dhcp lease address-lists", "ip/dhcp-server/lease", "address-lists", "lab-x", byComment("lab: static lease"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			requireClean(t, lab, c.section)
			if v := only(t, lab.A, c.section, c.find)[c.field]; v != "" {
				t.Fatalf("a's %s is %q, want it at its default (unset)", c.field, v)
			}
			patch(t, lab.B, c.section, only(t, lab.B, c.section, c.find)[".id"], map[string]string{c.field: c.value})
			d := compare(t, lab, c.section, lab.Pair.Sync.Exempt)
			if len(d.Hunks) != 1 || len(d.Hunks[0].Changes) != 1 || d.Hunks[0].Changes[0].Field != c.field {
				t.Fatalf("want one hunk changing %s, got %v", c.field, describe(d))
			}

			s := startTUI(t, lab, tuiOptions{write: true})
			s.drift()
			s.selectHunk(c.section, 0, plan.AtoB)
			v := s.plan()
			ops := shownOps(t, v)
			if len(ops) != 2 {
				t.Fatalf("want the backup and one reset of %s, got %v:\n%s", c.field, ops, v)
			}
			if c.unset {
				if ops[1].Method != "POST" || !strings.HasSuffix(ops[1].Path, "/unset") || ops[1].Body["value-name"] != c.field {
					t.Fatalf("want one unset of %s, got %v:\n%s", c.field, ops[1], v)
				}
			} else if value, ok := ops[1].Body[c.field]; ops[1].Method != "PATCH" || !ok || value != "" || len(ops[1].Body) != 1 {
				t.Fatalf("want one PATCH of %s alone to \"\", got %v:\n%s", c.field, ops[1], v)
			}
			v = s.confirm(false)
			if got := only(t, lab.B, c.section, c.find)[c.field]; got != "" {
				t.Fatalf("b's %s is still %q after the apply, want the default; the apply reported:\n%s", c.field, got, resultLines(v))
			}
			requireApplied(t, v)
			requireClean(t, lab, c.section)
		})
	}
}

// The script comment: system/script has no unset command at all on
// RouterOS 7.23.7 ("no such command", 400), so a script's field is synced
// back to its default by PATCHing "".
func TestLabApplyUnsetScriptComment(t *testing.T) {
	lab := labtest.New(t)
	const section = "system/script"
	requireClean(t, lab, section)

	// a's copy loses its comment; b's keeps it.
	patch(t, lab.A, section, only(t, lab.A, section, byName("lab-hello"))[".id"], map[string]string{"comment": ""})
	requireHunks(t, compare(t, lab, section, lab.Pair.Sync.Exempt), "lab-hello [both: comment]")

	s := startTUI(t, lab, tuiOptions{write: true})
	s.drift()
	s.selectSection(section, plan.AtoB)
	v := s.plan()
	ops := shownOps(t, v)
	if len(ops) != 2 || ops[1].Method != "PATCH" || len(ops[1].Body) != 1 {
		t.Fatalf("want the backup and one PATCH of comment alone, got %v:\n%s", ops, v)
	}
	if value, ok := ops[1].Body["comment"]; !ok || value != "" {
		t.Fatalf("want comment PATCHed to \"\", got %v:\n%s", ops[1], v)
	}
	v = s.confirm(false)
	if got := only(t, lab.B, section, byName("lab-hello"))["comment"]; got != "" {
		t.Fatalf("b's lab-hello comment is still %q, want none; the apply reported:\n%s", got, resultLines(v))
	}
	requireApplied(t, v)
	requireClean(t, lab, section)
}

// place-before whose anchor is not on the target: the create falls back to
// appending at the end of the chain.
func TestLabApplyPlaceBeforeAnchorAbsent(t *testing.T) {
	lab := labtest.New(t)

	// A chain of commented rules only, so identities are just comments:
	//   a: first, new, anchor
	//   b: first, b-only
	// "new" belongs before "anchor", which b does not have.
	const chain = "lab-apply"
	rule := func(comment, port string) map[string]string {
		return map[string]string{"chain": chain, "action": "drop", "protocol": "tcp", "dst-port": port, "comment": comment}
	}
	add(t, lab.A, filter, rule("lab: first", "7001"))
	add(t, lab.A, filter, rule("lab: new", "7002"))
	add(t, lab.A, filter, rule("lab: anchor", "7003"))
	add(t, lab.B, filter, rule("lab: first", "7001"))
	add(t, lab.B, filter, rule("lab: b-only", "7004"))
	requireHunks(t, compare(t, lab, filter, lab.Pair.Sync.Exempt), "lab: anchor [a]", "lab: b-only [b]", "lab: new [a]")

	s := startTUI(t, lab, tuiOptions{write: true})
	s.drift()
	s.selectHunk(filter, hunkIndex(t, lab, filter, "lab: new"), plan.AtoB)
	v := s.plan()
	ops := shownOps(t, v)
	if len(ops) != 2 || ops[1].Method != "PUT" || ops[1].Body["comment"] != "lab: new" {
		t.Fatalf("want the backup and the one create, got %v:\n%s", ops, v)
	}
	if pb, ok := ops[1].Body["place-before"]; ok {
		t.Fatalf("the create is placed before %s, but its anchor is not on b: it should append", pb)
	}
	if !strings.Contains(v, "appended at end of chain") {
		t.Errorf("the dry run does not say the rule is appended:\n%s", v)
	}
	// The hunks left unselected remain, and are reported as residual.
	v = s.confirm(false)
	if !strings.Contains(v, "applied 2/2 op(s)") || !regexp.MustCompile(`ip/firewall/filter\s+2 residual lab: (anchor|b-only), lab: (anchor|b-only)`).MatchString(v) {
		t.Fatalf("want both ops applied and only the unselected hunks residual:\n%s", resultLines(v))
	}

	got := chainOrder(t, lab.B, filter, chain)
	want := []string{
		only(t, lab.B, filter, byComment("lab: first"))[".id"],
		only(t, lab.B, filter, byComment("lab: b-only"))[".id"],
		only(t, lab.B, filter, byComment("lab: new"))[".id"],
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("b's chain %s is %v, want first, b-only, new (appended): %v", chain, got, want)
	}
	// What was not selected still differs; "new" does not, and the two
	// rules both routers share are in the same order.
	requireHunks(t, compare(t, lab, filter, lab.Pair.Sync.Exempt), "lab: anchor [a]", "lab: b-only [b]")
}

// Reverse direction: a rule added on b is created on a (the VRRP master, so
// the second confirmation is needed), at the same position.
func TestLabApplyBToA(t *testing.T) {
	lab := labtest.New(t)
	requireClean(t, lab, filter)

	newRule := byComment("lab: apply from b")
	add(t, lab.B, filter, map[string]string{
		"chain": "input", "action": "accept", "protocol": "tcp", "dst-port": "8291", "comment": "lab: apply from b",
		"place-before": only(t, lab.B, filter, byComment("lab: allow ssh"))[".id"],
	})
	requireHunks(t, compare(t, lab, filter, lab.Pair.Sync.Exempt), "lab: apply from b [b]")

	s := startTUI(t, lab, tuiOptions{write: true})
	s.drift()
	s.selectSection(filter, plan.BtoA)
	v := s.plan()
	ops := shownOps(t, v)
	if len(ops) != 2 || ops[0].Router != "a" || ops[1].Method != "PUT" || ops[1].Body["place-before"] != only(t, lab.A, filter, byComment("lab: allow ssh"))[".id"] {
		t.Fatalf("want a's backup and one PUT on a placed before its \"lab: allow ssh\", got %v:\n%s", ops, v)
	}
	requireApplied(t, s.confirm(true))

	requireSamePosition(t, lab, filter, "input", newRule)
	if n := len(preApplyBackups(t, lab.B)); n != 0 {
		t.Errorf("router b, the source, has %d pre-apply backups: it was written to", n)
	}
	requireClean(t, lab, filter)
}

// Multi-section ordering: per section deletes, then updates, then creates;
// sections in the configured order. The order is the shipped sample's,
// which lists ip/firewall/address-list before filter and nat so a rule's
// new address list exists before the rule does.
func TestLabApplyMultiSectionOrder(t *testing.T) {
	lab := labtest.New(t)
	const (
		addrList = "ip/firewall/address-list"
		nat      = "ip/firewall/nat"
	)
	var order []string
	for _, s := range sampleSections(t) {
		if s == addrList || s == filter || s == nat {
			order = append(order, s)
		}
	}
	if strings.Join(order, ",") != strings.Join([]string{addrList, filter, nat}, ",") {
		t.Fatalf("the sample orders the firewall sections %v; this test expects address-list first", order)
	}
	// The sample's whole section list, so the Drift screen is as a new
	// user's would be.
	pair := withSections(lab, sampleSections(t))

	// On a, in each section: one entry removed, one changed, one added.
	byList := func(list, addr string) func(map[string]string) bool {
		return func(e map[string]string) bool { return e["list"] == list && e["address"] == addr }
	}
	remove(t, lab.A, addrList, only(t, lab.A, addrList, byComment("lab: in two lists"))[".id"])
	patch(t, lab.A, addrList, only(t, lab.A, addrList, byList("lab-trusted", "10.10.10.10"))[".id"], map[string]string{"comment": "lab: apply changed"})
	add(t, lab.A, addrList, map[string]string{"list": "lab-apply", "address": "10.20.30.40"})

	remove(t, lab.A, filter, only(t, lab.A, filter, byComment("lab: allow ssh"))[".id"])
	patch(t, lab.A, filter, only(t, lab.A, filter, byComment("lab: drop alt telnet"))[".id"], map[string]string{"dst-port": "2324"})
	add(t, lab.A, filter, map[string]string{"chain": "forward", "action": "drop", "src-address-list": "lab-apply", "comment": "lab: apply uses list"})

	dstnat := func(e map[string]string) bool { return e["chain"] == "dstnat" }
	remove(t, lab.A, nat, only(t, lab.A, nat, dstnat)[".id"])
	patch(t, lab.A, nat, only(t, lab.A, nat, byComment("lab: masquerade"))[".id"], map[string]string{"src-address": "192.168.88.0/24"})
	add(t, lab.A, nat, map[string]string{"chain": "dstnat", "action": "dst-nat", "protocol": "tcp", "dst-port": "8081",
		"to-addresses": "192.168.88.11", "to-ports": "80", "comment": "lab: apply nat"})

	s := startTUI(t, lab, tuiOptions{write: true, pair: pair})
	s.drift()
	// Selected in the opposite order, so any ordering the plan shows is its
	// own.
	s.selectSection(nat, plan.AtoB)
	s.selectSection(filter, plan.AtoB)
	s.selectSection(addrList, plan.AtoB)
	v := s.plan()
	ops := shownOps(t, v)

	var got []string
	for _, op := range ops {
		if op.Router != "b" {
			t.Errorf("op on router %s: %s", op.Router, op)
		}
		if sec := op.Section(); sec != "" {
			got = append(got, sec+" "+op.Method)
		}
	}
	want := []string{
		addrList + " DELETE", addrList + " PATCH", addrList + " PUT",
		filter + " DELETE", filter + " PATCH", filter + " PUT",
		nat + " DELETE", nat + " PATCH", nat + " PUT",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("plan order:\n  got:  %s\n  want: %s\n%s", strings.Join(got, "\n        "), strings.Join(want, "\n        "), v)
	}
	if ops[0].Path != "/system/backup/save" {
		t.Fatalf("the plan does not start with the backup: %v", ops[0])
	}
	if strings.Contains(v, "Warnings") {
		t.Errorf("the new list is created before the rule naming it, yet the plan warns:\n%s", v)
	}
	requireApplied(t, s.confirm(false))

	for _, sec := range []string{addrList, filter, nat} {
		requireClean(t, lab, sec)
	}
}

func drop(ids []string, id string) []string {
	var out []string
	for _, x := range ids {
		if x != id {
			out = append(out, x)
		}
	}
	return out
}

// resultLines is the Apply screen's outcome: from the "applied" or "apply
// stopped" line down.
func resultLines(v string) string {
	if loc := regexp.MustCompile(`applied \d+/|apply stopped`).FindStringIndex(v); loc != nil {
		return v[loc[0]:]
	}
	return v
}
