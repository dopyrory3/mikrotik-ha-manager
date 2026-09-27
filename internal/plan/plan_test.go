package plan

import (
	"flag"
	"os"
	"path/filepath"
	"testing"

	"mtha/internal/model"
)

// Run `go test ./internal/plan -update` to regenerate the golden files after
// an intentional change to planning or rendering; review the diff before
// committing it.
var update = flag.Bool("update", false, "rewrite golden files")

func assertGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (run with -update to create it): %v", err)
	}
	if got != string(want) {
		t.Errorf("plan does not match %s\n--- got ---\n%s--- want ---\n%s", path, got, want)
	}
}

func choose(d Direction, ids ...string) map[HunkRef]Direction {
	m := map[HunkRef]Direction{}
	for _, id := range ids {
		m[HunkRef{Identity: id}] = d
	}
	return m
}

// A new commented rule in the middle of a chain lands before the next rule
// of the same chain that the target already has — not before a rule from a
// different chain that happens to follow it in the list. A new rule with
// nothing after it in its chain is appended.
func TestBuildFirewallPlaceBefore(t *testing.T) {
	a := []model.Entry{
		{".id": "*1", "chain": "input", "comment": "allow-ssh", "action": "accept", "dst-port": "22", "protocol": "tcp", "bytes": "900", "packets": "9"},
		{".id": "*2", "chain": "input", "comment": "allow-dns", "action": "accept", "dst-port": "53", "protocol": "udp", "bytes": "10", "packets": "1"},
		{".id": "*3", "chain": "forward", "comment": "fwd-established", "action": "accept", "connection-state": "established,related"},
		{".id": "*4", "chain": "input", "comment": "drop-rest", "action": "drop"},
		{".id": "*5", "chain": "forward", "action": "drop", "log": "true", "log-prefix": ""},
	}
	b := []model.Entry{
		{".id": "*A", "chain": "input", "comment": "allow-ssh", "action": "accept", "dst-port": "22", "protocol": "tcp"},
		{".id": "*B", "chain": "input", "comment": "drop-rest", "action": "drop"},
		{".id": "*C", "chain": "forward", "comment": "fwd-established", "action": "accept", "connection-state": "established,related"},
	}

	p := Build([]SectionInput{{
		Section: "ip/firewall/filter", A: a, B: b,
		Choices: choose(AtoB, "allow-dns", "forward@fwd-established#1"),
	}}, Options{BackupName: "mtha-pre-apply-test"})

	assertGolden(t, "firewall_place_before", p.Render())
}

// Two consecutive new rules anchored on the same existing rule keep their
// source order, because creates run in source order and each is placed
// directly before the anchor.
func TestBuildConsecutiveCreatesKeepOrder(t *testing.T) {
	a := []model.Entry{
		{".id": "*1", "chain": "srcnat", "comment": "nat-vpn", "action": "accept"},
		{".id": "*2", "chain": "srcnat", "comment": "nat-lab", "action": "accept"},
		{".id": "*3", "chain": "srcnat", "comment": "masquerade", "action": "masquerade", "out-interface": "ether1"},
	}
	b := []model.Entry{
		{".id": "*9", "chain": "srcnat", "comment": "masquerade", "action": "masquerade", "out-interface": "ether1"},
	}

	// Choices listed out of order on purpose: creates follow source order.
	p := Build([]SectionInput{{
		Section: "ip/firewall/nat", A: a, B: b,
		Choices: choose(AtoB, "nat-lab", "nat-vpn"),
	}}, Options{})

	assertGolden(t, "consecutive_creates", p.Render())
}

// Deletes, updates (PATCH for set fields, unset for fields the source
// leaves at default) and creates in one section; plus a B→A choice in a
// second section, so the plan writes to both routers, each group preceded by
// its own backup.
func TestBuildMixedOperationsAndDirections(t *testing.T) {
	filterA := []model.Entry{
		{".id": "*1", "chain": "input", "comment": "allow-http", "action": "accept", "dst-port": "80", "protocol": "tcp"},
	}
	filterB := []model.Entry{
		{".id": "*11", "chain": "input", "comment": "allow-http", "action": "drop", "dst-port": "80", "protocol": "tcp", "src-address": "10.0.0.0/8"},
		{".id": "*12", "chain": "input", "comment": "legacy", "action": "accept"},
	}
	scriptA := []model.Entry{}
	scriptB := []model.Entry{
		{".id": "*S1", "name": "rotate-logs", "source": ":log info rotate", "policy": "read,write", "owner": "admin", "run-count": "4"},
	}

	p := Build([]SectionInput{
		{Section: "ip/firewall/filter", A: filterA, B: filterB, Choices: choose(AtoB, "allow-http", "legacy")},
		{Section: "system/script", A: scriptA, B: scriptB, Choices: choose(BtoA, "rotate-logs")},
	}, Options{BackupName: "bk"})

	assertGolden(t, "mixed_operations", p.Render())
	if got := p.Targets(); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("Targets() = %v, want [a b]", got)
	}
	if got := p.Sections(); len(got) != 2 || got[0] != "system/script" || got[1] != "ip/firewall/filter" {
		t.Errorf("Sections() = %v, want [system/script ip/firewall/filter]", got)
	}
}

// Rules sharing a comment are told apart by dedupe, so the patch goes to the
// second "web" rule's .id, not the first.
func TestBuildDuplicateCommentTargetsRightRule(t *testing.T) {
	a := []model.Entry{
		{".id": "*1", "chain": "forward", "comment": "web", "dst-port": "80", "action": "accept"},
		{".id": "*2", "chain": "forward", "comment": "web", "dst-port": "443", "action": "accept"},
	}
	b := []model.Entry{
		{".id": "*7", "chain": "forward", "comment": "web", "dst-port": "80", "action": "accept"},
		{".id": "*8", "chain": "forward", "comment": "web", "dst-port": "8443", "action": "accept"},
	}

	p := Build([]SectionInput{{
		Section: "ip/firewall/filter", A: a, B: b, Choices: choose(AtoB, "web#2"),
	}}, Options{BackupName: "bk"})

	assertGolden(t, "duplicate_comment", p.Render())
}

// Only mtha:-tagged routes are planned; an untagged route can't be chosen
// because it never appears in the diff, so a choice naming one is reported
// as no longer differing rather than written.
func TestBuildRouteOptIn(t *testing.T) {
	a := []model.Entry{
		{".id": "*1", "dst-address": "0.0.0.0/0", "gateway": "203.0.113.1", "active": "true"},
		{".id": "*2", "dst-address": "10.9.0.0/16", "gateway": "10.0.0.9", "comment": "mtha:vpn", "distance": "1", "active": "true", "static": "true"},
	}
	b := []model.Entry{
		{".id": "*9", "dst-address": "0.0.0.0/0", "gateway": "198.51.100.1", "active": "true"},
	}

	p := Build([]SectionInput{{
		Section: "ip/route", A: a, B: b, Choices: choose(AtoB, "mtha:vpn", "0.0.0.0/0->203.0.113.1"),
	}}, Options{BackupName: "bk"})

	assertGolden(t, "route_opt_in", p.Render())
}

// Hunks that can't be synced safely are skipped with a reason: creating a
// user (password unreadable), adding/removing built-in ip/service entries.
// Changing an ip/service entry is still planned.
func TestBuildSkipsUnsafeHunks(t *testing.T) {
	usersA := []model.Entry{{".id": "*1", "name": "alice", "group": "full"}}
	usersB := []model.Entry{}
	svcA := []model.Entry{
		{".id": "*1", "name": "www-ssl", "port": "443", "disabled": "false"},
		{".id": "*2", "name": "api", "port": "8728", "disabled": "true"},
	}
	svcB := []model.Entry{
		{".id": "*1", "name": "www-ssl", "port": "8443", "disabled": "false"},
	}

	p := Build([]SectionInput{
		{Section: "user", A: usersA, B: usersB, Choices: choose(AtoB, "alice")},
		{Section: "ip/service", A: svcA, B: svcB, Choices: choose(AtoB, "www-ssl", "api")},
	}, Options{BackupName: "bk"})

	assertGolden(t, "skips", p.Render())
}

func TestBuildNothingChosenIsEmpty(t *testing.T) {
	a := []model.Entry{{".id": "*1", "name": "x"}}
	p := Build([]SectionInput{{Section: "system/script", A: a, B: nil}}, Options{})
	if !p.Empty() {
		t.Fatalf("expected empty plan, got:\n%s", p.Render())
	}
	if p.Render() != "no operations\n" {
		t.Errorf("Render() = %q", p.Render())
	}
}

// Exempt fields are per-router: never copied into a create body, never
// patched.
func TestBuildHonoursExempt(t *testing.T) {
	a := []model.Entry{{".id": "*1", "name": "vrrp-lan", "interface": "ether2", "vrid": "1", "priority": "200"}}
	b := []model.Entry{}

	p := Build([]SectionInput{{
		Section: "interface/vrrp", A: a, B: b, Choices: choose(AtoB, "vrrp-lan"),
	}}, Options{Exempt: []string{"interface/vrrp.priority"}})

	if len(p.Ops) != 2 {
		t.Fatalf("expected backup + create, got:\n%s", p.Render())
	}
	if _, ok := p.Ops[1].Body["priority"]; ok {
		t.Errorf("exempt field copied into create body: %v", p.Ops[1].Body)
	}
}
