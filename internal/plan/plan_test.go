package plan

import (
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"mtha/internal/config"
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
// user (password unreadable), adding/removing built-in ip/service entries,
// and anything that could lock mtha out of the target mid-apply — moving
// its REST API service (www-ssl), or removing, regrouping or disabling the
// user it logs in as. Other ip/service and user changes are still planned.
func TestBuildSkipsUnsafeHunks(t *testing.T) {
	usersA := []model.Entry{
		{".id": "*1", "name": "alice", "group": "full"},
		{".id": "*2", "name": "admin", "group": "full"},
	}
	usersB := []model.Entry{
		{".id": "*1", "name": "api-b", "group": "full"},
		{".id": "*2", "name": "admin", "group": "read"},
		{".id": "*3", "name": "bob", "group": "full"},
	}
	svcA := []model.Entry{
		{".id": "*1", "name": "www-ssl", "port": "443", "disabled": "false"},
		{".id": "*2", "name": "api", "port": "8728", "disabled": "true"},
		{".id": "*3", "name": "ssh", "port": "22"},
	}
	svcB := []model.Entry{
		{".id": "*1", "name": "www-ssl", "port": "8443", "disabled": "false"},
		{".id": "*3", "name": "ssh", "port": "2222"},
	}

	userChoices := choose(AtoB, "alice", "api-b", "bob")
	userChoices[HunkRef{Identity: "admin"}] = BtoA // regroups router a's API user

	p := Build([]SectionInput{
		{Section: "user", A: usersA, B: usersB, Choices: userChoices},
		{Section: "ip/service", A: svcA, B: svcB, Choices: choose(AtoB, "www-ssl", "api", "ssh")},
	}, Options{BackupName: "bk", Users: map[string]string{"a": "admin", "b": "api-b"}})

	assertGolden(t, "skips", p.Render())
}

// Moving the REST API, narrowing who may reach it, disabling it or swapping
// its certificate is refused, along with anything else in the same hunk; a
// change to www-ssl's other fields is still planned.
func TestBuildRefusesRESTServiceLockout(t *testing.T) {
	cases := []struct {
		name     string
		b        model.Entry
		wantSkip bool
	}{
		{"port", model.Entry{".id": "*1", "name": "www-ssl", "port": "8443"}, true},
		{"address", model.Entry{".id": "*1", "name": "www-ssl", "address": "10.9.9.0/24"}, true},
		{"disabled", model.Entry{".id": "*1", "name": "www-ssl", "disabled": "true"}, true},
		{"certificate", model.Entry{".id": "*1", "name": "www-ssl", "certificate": "other-cert"}, true},
		{"certificate with other field", model.Entry{".id": "*1", "name": "www-ssl", "certificate": "other-cert", "tls-version": "only-1.2"}, true},
		{"other field", model.Entry{".id": "*1", "name": "www-ssl", "tls-version": "only-1.2"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := []model.Entry{{".id": "*1", "name": "www-ssl"}}
			p := Build([]SectionInput{{
				Section: "ip/service", A: a, B: []model.Entry{tc.b}, Choices: choose(AtoB, "www-ssl"),
			}}, Options{})
			if skipped := len(p.Skipped) == 1 && p.Empty(); skipped != tc.wantSkip {
				t.Errorf("skipped = %v, want %v:\n%s", skipped, tc.wantSkip, p.Render())
			}
		})
	}
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

func TestBuildNeverWritesVRRPRoleFlags(t *testing.T) {
	a := []model.Entry{{".id": "*1", "name": "vrrp-lan", "interface": "ether2", "vrid": "1", "master": "true"}}
	b := []model.Entry{}
	p := Build([]SectionInput{{
		Section: "interface/vrrp", A: a, B: b, Choices: choose(AtoB, "vrrp-lan"),
	}}, Options{})
	if len(p.Ops) != 2 {
		t.Fatalf("expected backup + create, got:\n%s", p.Render())
	}
	for _, flag := range []string{"master", "backup"} {
		if _, ok := p.Ops[1].Body[flag]; ok {
			t.Errorf("role flag %q copied into create body: %v", flag, p.Ops[1].Body)
		}
	}
}

// A user whose comment differs between the routers is one changed hunk,
// since users are identified by name, not by comment (docs/design-questions.md
// question 1). Syncing it A→B patches the comment on B's user in place: it
// must never delete the user, because the create that would follow is
// skipped (REST can't read the password) and B would be left without it.
func TestBuildUserCommentChangePatchesInPlace(t *testing.T) {
	a := []model.Entry{{".id": "*1", "name": "ops", "group": "full", "comment": "on-call"}}
	b := []model.Entry{{".id": "*7", "name": "ops", "group": "full", "comment": "ops team"}}

	p := Build([]SectionInput{{
		Section: "user", A: a, B: b,
		Choices: choose(AtoB, "ops"),
	}}, Options{BackupName: "bk", Users: map[string]string{"a": "admin", "b": "admin"}})

	var writes []string
	for _, op := range p.Ops {
		switch op.Method {
		case MethodDelete, MethodCreate:
			t.Errorf("%s would drop or recreate the user: %s", op.Method, op)
		case MethodUpdate:
			writes = append(writes, op.Router+" "+op.String())
		}
	}
	want := `b PATCH /user/*7 {"comment":"on-call"}`
	if len(writes) != 1 || writes[0] != want {
		t.Errorf("updates = %v, want [%s]", writes, want)
	}
	if len(p.Skipped) != 0 {
		t.Errorf("skipped = %+v, want none", p.Skipped)
	}
}

// The reference check (docs/design-questions.md §3): a body naming an
// object the target lacks warns, unless an earlier op in the plan creates
// it; the op itself is still planned.
func TestBuildWarnsAboutMissingReferents(t *testing.T) {
	server := func(id, name, pool string) model.Entry {
		return model.Entry{".id": id, "name": name, "interface": "bridge", "address-pool": pool}
	}
	// A lease is identified by server|mac-address, so a lease whose server
	// differs is a remove plus a create, never a changed field: the check
	// sees its server in a create body. (The PATCH path is exercised by the
	// user group case below.)
	lease := func(id, mac, srv string) model.Entry {
		return model.Entry{".id": id, "mac-address": mac, "address": "10.0.0.5", "server": srv}
	}
	pools := map[Read][]model.Entry{
		{Router: "b", Section: "ip/pool"}:   {{".id": "*1", "name": "pool-lan"}},
		{Router: "b", Section: "interface"}: {{".id": "*1", "name": "bridge"}},
	}

	cases := []struct {
		name      string
		inputs    []SectionInput
		referents map[Read][]model.Entry
		want      []string
	}{
		{
			name: "lease naming a server the target lacks",
			inputs: []SectionInput{
				{Section: "ip/dhcp-server/lease", A: []model.Entry{lease("*1", "AA", "dhcp-guest")}, Choices: choose(AtoB, "dhcp-guest|AA")},
			},
			referents: map[Read][]model.Entry{{Router: "b", Section: "ip/dhcp-server"}: {server("*1", "dhcp-lan", "pool-lan")}},
			want:      []string{`ip/dhcp-server/lease dhcp-guest|AA [router B]: server=dhcp-guest names ip/dhcp-server name "dhcp-guest", which router B does not have and this plan does not create first`},
		},
		{
			name: "lease moved to a server the target lacks: remove plus create",
			inputs: []SectionInput{{
				Section: "ip/dhcp-server/lease",
				A:       []model.Entry{lease("*1", "AA", "dhcp-guest")},
				B:       []model.Entry{lease("*4", "AA", "dhcp-lan")},
				Choices: choose(AtoB, "dhcp-guest|AA", "dhcp-lan|AA"),
			}},
			referents: map[Read][]model.Entry{{Router: "b", Section: "ip/dhcp-server"}: {server("*1", "dhcp-lan", "pool-lan")}},
			want:      []string{`ip/dhcp-server/lease dhcp-guest|AA [router B]: server=dhcp-guest names ip/dhcp-server name "dhcp-guest", which router B does not have and this plan does not create first`},
		},
		{
			name: "lease naming a server the target has",
			inputs: []SectionInput{
				{Section: "ip/dhcp-server", A: []model.Entry{server("*1", "dhcp-lan", "pool-lan")}, B: []model.Entry{server("*9", "dhcp-lan", "pool-lan")}},
				{Section: "ip/dhcp-server/lease", A: []model.Entry{lease("*1", "AA", "dhcp-lan")}, Choices: choose(AtoB, "dhcp-lan|AA")},
			},
		},
		{
			name: "server created earlier in the plan",
			inputs: []SectionInput{
				{Section: "ip/dhcp-server", A: []model.Entry{server("*1", "dhcp-guest", "pool-lan")}, Choices: choose(AtoB, "dhcp-guest")},
				{Section: "ip/dhcp-server/lease", A: []model.Entry{lease("*1", "AA", "dhcp-guest")}, Choices: choose(AtoB, "dhcp-guest|AA")},
			},
			referents: pools,
		},
		{
			name: "server created later in the plan",
			inputs: []SectionInput{
				{Section: "ip/dhcp-server/lease", A: []model.Entry{lease("*1", "AA", "dhcp-guest")}, Choices: choose(AtoB, "dhcp-guest|AA")},
				{Section: "ip/dhcp-server", A: []model.Entry{server("*1", "dhcp-guest", "pool-lan")}, Choices: choose(AtoB, "dhcp-guest")},
			},
			referents: pools,
			want:      []string{`ip/dhcp-server/lease dhcp-guest|AA [router B]: server=dhcp-guest names ip/dhcp-server name "dhcp-guest", which this plan only creates later (op 3)`},
		},
		{
			name: "half-selection: the server's create is not selected",
			inputs: []SectionInput{
				{Section: "ip/dhcp-server", A: []model.Entry{server("*1", "dhcp-guest", "pool-lan")}},
				{Section: "ip/dhcp-server/lease", A: []model.Entry{lease("*1", "AA", "dhcp-guest")}, Choices: choose(AtoB, "dhcp-guest|AA")},
			},
			want: []string{`ip/dhcp-server/lease dhcp-guest|AA [router B]: server=dhcp-guest names ip/dhcp-server name "dhcp-guest", which router B does not have and this plan does not create first`},
		},
		{
			name: "server removed earlier in the plan",
			inputs: []SectionInput{
				{Section: "ip/dhcp-server", B: []model.Entry{server("*9", "dhcp-old", "pool-lan")}, Choices: choose(AtoB, "dhcp-old")},
				{Section: "ip/dhcp-server/lease", A: []model.Entry{lease("*1", "AA", "dhcp-old")}, Choices: choose(AtoB, "dhcp-old|AA")},
			},
			want: []string{`ip/dhcp-server/lease dhcp-old|AA [router B]: server=dhcp-old names ip/dhcp-server name "dhcp-old", which this plan removes first`},
		},
		{
			name: "lease server all names nothing",
			inputs: []SectionInput{
				{Section: "ip/dhcp-server/lease", A: []model.Entry{lease("*1", "AA", "all")}, Choices: choose(AtoB, "all|AA")},
			},
		},
		{
			name: "server naming an unsynced pool the target lacks",
			inputs: []SectionInput{
				{Section: "ip/dhcp-server", A: []model.Entry{server("*1", "dhcp-guest", "pool-guest")}, Choices: choose(AtoB, "dhcp-guest")},
			},
			referents: pools,
			want:      []string{`ip/dhcp-server dhcp-guest [router B]: address-pool=pool-guest names ip/pool name "pool-guest", which router B does not have and this plan does not create first`},
		},
		{
			name: "unsynced referent not read",
			inputs: []SectionInput{
				{Section: "ip/dhcp-server", A: []model.Entry{server("*1", "dhcp-guest", "pool-lan")}, Choices: choose(AtoB, "dhcp-guest")},
			},
			want: []string{
				`ip/dhcp-server dhcp-guest [router B]: address-pool=pool-lan names ip/pool name "pool-lan", which was not read, so it could not be checked`,
				`ip/dhcp-server dhcp-guest [router B]: interface=bridge names interface name "bridge", which was not read, so it could not be checked`,
			},
		},
		{
			name: "PATCH setting a user group the target lacks",
			inputs: []SectionInput{{
				Section: "user",
				A:       []model.Entry{{".id": "*1", "name": "ops", "group": "noc"}},
				B:       []model.Entry{{".id": "*7", "name": "ops", "group": "read"}},
				Choices: choose(AtoB, "ops"),
			}},
			referents: map[Read][]model.Entry{{Router: "b", Section: "user/group"}: {{"name": "read"}, {"name": "full"}}},
			want:      []string{`user ops [router B]: group=noc names user/group name "noc", which router B does not have and this plan does not create first`},
		},
		{
			name: "firewall rule naming an empty address list, negated",
			inputs: []SectionInput{
				{Section: "ip/firewall/filter", A: []model.Entry{{".id": "*1", "chain": "input", "action": "drop", "src-address-list": "!trusted", "comment": "drop-untrusted"}}, Choices: choose(AtoB, "drop-untrusted")},
				{Section: "ip/firewall/address-list", A: []model.Entry{{".id": "*1", "list": "trusted", "address": "10.0.0.9"}}},
			},
			want: []string{`ip/firewall/filter drop-untrusted [router B]: src-address-list=trusted names ip/firewall/address-list list "trusted", which router B does not have and this plan does not create first`},
		},
		{
			name: "address list filled earlier in the plan",
			inputs: []SectionInput{
				{Section: "ip/firewall/address-list", A: []model.Entry{{".id": "*1", "list": "blocklist", "address": "198.51.100.7"}}, Choices: choose(AtoB, "blocklist|198.51.100.7")},
				{Section: "ip/firewall/filter", A: []model.Entry{{".id": "*1", "chain": "input", "action": "drop", "src-address-list": "blocklist", "comment": "drop-blocked"}}, Choices: choose(AtoB, "drop-blocked")},
			},
		},
		{
			name: "dynamic address-list entries count",
			inputs: []SectionInput{
				{Section: "ip/firewall/filter", A: []model.Entry{{".id": "*1", "chain": "input", "action": "drop", "src-address-list": "scanners", "comment": "drop-scanners"}}, Choices: choose(AtoB, "drop-scanners")},
				{Section: "ip/firewall/address-list", B: []model.Entry{{".id": "*5", "list": "scanners", "address": "203.0.113.4", "dynamic": "true"}}},
			},
		},
		{
			name: "scheduler naming a script, and one with inline source",
			inputs: []SectionInput{
				{Section: "system/scheduler", A: []model.Entry{
					{".id": "*1", "name": "nightly", "on-event": "backup-job"},
					{".id": "*2", "name": "inline", "on-event": "/system backup save name=x"},
				}, Choices: choose(AtoB, "nightly", "inline")},
				{Section: "system/script", B: []model.Entry{{".id": "*3", "name": "other"}}},
			},
			want: []string{`system/scheduler nightly [router B]: on-event=backup-job names system/script name "backup-job", which router B does not have and this plan does not create first`},
		},
		{
			name: "route via an interface gateway and a table",
			inputs: []SectionInput{
				{Section: "ip/route", A: []model.Entry{
					{".id": "*1", "dst-address": "0.0.0.0/0", "gateway": "10.0.0.1%wan2", "routing-table": "isp2", "comment": "mtha:isp2"},
					{".id": "*2", "dst-address": "10.9.0.0/16", "gateway": "10.0.0.254", "comment": "mtha:lan"},
				}, Choices: choose(AtoB, "mtha:isp2", "mtha:lan")},
			},
			referents: map[Read][]model.Entry{
				{Router: "b", Section: "interface"}:     {{"name": "wan1"}},
				{Router: "b", Section: "routing/table"}: {{"name": "main"}},
			},
			want: []string{
				`ip/route mtha:isp2 [router B]: gateway=wan2 names interface name "wan2", which router B does not have and this plan does not create first`,
				`ip/route mtha:isp2 [router B]: routing-table=isp2 names routing/table name "isp2", which router B does not have and this plan does not create first`,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := Build(tc.inputs, Options{Referents: tc.referents})
			var got []string
			for _, w := range p.Warnings {
				got = append(got, w.String())
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("warnings:\n%s\nwant:\n%s\nplan:\n%s", strings.Join(got, "\n"), strings.Join(tc.want, "\n"), p.Render())
			}
			if p.Empty() || len(p.Skipped) > 0 {
				t.Errorf("a warning must not stop the op being planned:\n%s", p.Render())
			}
		})
	}
}

// ReferenceReads asks for exactly the referent sections a planned body
// names that the inputs don't carry, on the router written to, and none
// when no body refers to anything.
func TestReferenceReads(t *testing.T) {
	cases := []struct {
		name   string
		inputs []SectionInput
		want   []Read
	}{
		{
			name: "no reference fields",
			inputs: []SectionInput{
				{Section: "ip/dns/static", A: []model.Entry{{".id": "*1", "name": "nas", "address": "10.0.0.5"}}, Choices: choose(AtoB, "nas|A|10.0.0.5")},
			},
		},
		{
			name: "referent among the inputs needs no read",
			inputs: []SectionInput{
				{Section: "system/script", A: []model.Entry{{".id": "*1", "name": "job"}}},
				{Section: "system/scheduler", B: []model.Entry{{".id": "*1", "name": "nightly", "on-event": "job"}}, Choices: choose(BtoA, "nightly")},
			},
		},
		{
			name: "unsynced and unselected referents, on the target",
			inputs: []SectionInput{
				{Section: "ip/dhcp-server", B: []model.Entry{{".id": "*1", "name": "dhcp-guest", "interface": "bridge", "address-pool": "pool-guest"}}, Choices: choose(BtoA, "dhcp-guest")},
				{Section: "ip/dhcp-server/lease", B: []model.Entry{{".id": "*1", "mac-address": "AA", "server": "dhcp-guest"}}, Choices: choose(BtoA, "dhcp-guest|AA")},
				{Section: "user", A: []model.Entry{{".id": "*1", "name": "ops", "group": "read"}}, B: []model.Entry{{".id": "*1", "name": "ops", "group": "noc"}}, Choices: choose(BtoA, "ops")},
			},
			want: []Read{{Router: "a", Section: "ip/pool"}, {Router: "a", Section: "interface"}, {Router: "a", Section: "user/group"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ReferenceReads(tc.inputs, Options{}); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ReferenceReads = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestReferenceNames(t *testing.T) {
	cases := []struct {
		fn   func(string) []string
		in   string
		want []string
	}{
		{gatewayInterfaces, "10.0.0.1", nil},
		{gatewayInterfaces, "ether2", []string{"ether2"}},
		{gatewayInterfaces, "fe80::1%ether1", []string{"ether1"}},
		{gatewayInterfaces, "10.0.0.1@main", nil},
		{gatewayInterfaces, "10.0.0.1,pppoe-out1", []string{"pppoe-out1"}},
		{scriptName, "backup-job", []string{"backup-job"}},
		{scriptName, ":log info x", nil},
		{scriptName, "/system script run x", nil},
		{negatable, "!trusted", []string{"trusted"}},
	}
	for _, tc := range cases {
		if got := tc.fn(tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("names(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// config's section-order check (docs/design-questions.md §3, option D) is
// the synced half of the reference table here: every reference between two
// sections the sample syncs must be in it, and nothing else.
func TestConfigSectionReferentsMatchReferences(t *testing.T) {
	file, err := config.Load(writeSample(t))
	if err != nil {
		t.Fatal(err)
	}
	synced := map[string]bool{}
	for _, s := range file.Pairs[0].Sync.Sections {
		synced[s] = true
	}
	for _, s := range []string{"ip/firewall/mangle", "ip/firewall/raw"} {
		synced[s] = true // not in the sample, but syncable firewall lists
	}

	want := map[string]map[string]bool{}
	for _, r := range references {
		if synced[r.section] && synced[r.referent] {
			if want[r.section] == nil {
				want[r.section] = map[string]bool{}
			}
			want[r.section][r.referent] = true
		}
	}
	for section := range synced {
		got := map[string]bool{}
		for _, ref := range config.SectionReferents(section) {
			got[ref] = true
		}
		if len(got) == 0 && len(want[section]) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, want[section]) {
			t.Errorf("config.SectionReferents(%q) = %v, reference table says %v", section, got, want[section])
		}
	}
}

func writeSample(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pairs.yaml")
	if err := config.WriteSample(path); err != nil {
		t.Fatal(err)
	}
	return path
}
