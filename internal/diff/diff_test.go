package diff

import "mtha/internal/model"

import "testing"

func TestCompareCleanWhenIdentical(t *testing.T) {
	a := []model.Entry{{"name": "svc-api", "port": "443"}}
	b := []model.Entry{{"name": "svc-api", "port": "443"}}

	sd := Compare("ip/service", a, b, nil)
	if !sd.Clean() {
		t.Fatalf("expected no hunks, got %+v", sd.Hunks)
	}
}

func TestCompareVRRPMasterAndBackupClean(t *testing.T) {
	a := []model.Entry{{".id": "*1", "name": "vrrp-lan", "interface": "ether2", "vrid": "1", "master": "true", "running": "true"}}
	b := []model.Entry{{".id": "*2", "name": "vrrp-lan", "interface": "ether2", "vrid": "1", "backup": "true", "running": "false"}}

	if got := Compare("interface/vrrp", a, b, nil); !got.Clean() {
		t.Fatalf("master/backup role should not produce drift: %+v", got.Hunks)
	}
}

func TestCompareDetectsOnlyOnA(t *testing.T) {
	a := []model.Entry{{"name": "svc-api"}}
	var b []model.Entry

	sd := Compare("ip/service", a, b, nil)
	if len(sd.Hunks) != 1 {
		t.Fatalf("expected 1 hunk, got %d", len(sd.Hunks))
	}
	h := sd.Hunks[0]
	if !h.OnA || h.OnB {
		t.Fatalf("expected only-on-A hunk, got %+v", h)
	}
	if h.Identity != "svc-api" {
		t.Fatalf("expected identity svc-api, got %q", h.Identity)
	}
}

func TestCompareDetectsOnlyOnB(t *testing.T) {
	var a []model.Entry
	b := []model.Entry{{"name": "svc-api"}}

	sd := Compare("ip/service", a, b, nil)
	if len(sd.Hunks) != 1 {
		t.Fatalf("expected 1 hunk, got %d", len(sd.Hunks))
	}
	h := sd.Hunks[0]
	if h.OnA || !h.OnB {
		t.Fatalf("expected only-on-B hunk, got %+v", h)
	}
}

func TestCompareDetectsFieldChange(t *testing.T) {
	a := []model.Entry{{"name": "svc-api", "port": "443"}}
	b := []model.Entry{{"name": "svc-api", "port": "8443"}}

	sd := Compare("ip/service", a, b, nil)
	if len(sd.Hunks) != 1 {
		t.Fatalf("expected 1 hunk, got %d", len(sd.Hunks))
	}
	h := sd.Hunks[0]
	if !h.OnA || !h.OnB {
		t.Fatalf("expected changed hunk present on both sides, got %+v", h)
	}
	if len(h.Changes) != 1 || h.Changes[0].Field != "port" {
		t.Fatalf("expected single port change, got %+v", h.Changes)
	}
}

func TestCompareIgnoresDynamicEntries(t *testing.T) {
	a := []model.Entry{{"name": "learned", "dynamic": "true"}}
	b := []model.Entry{}

	sd := Compare("ip/service", a, b, nil)
	if !sd.Clean() {
		t.Fatalf("expected dynamic-only entry to produce no hunks, got %+v", sd.Hunks)
	}
}

func TestCompareRespectsFieldExempt(t *testing.T) {
	a := []model.Entry{{"name": "vrrp-lan", "priority": "200"}}
	b := []model.Entry{{"name": "vrrp-lan", "priority": "100"}}

	sd := Compare("interface/vrrp", a, b, []string{"interface/vrrp.priority"})
	if !sd.Clean() {
		t.Fatalf("expected priority difference to be exempted, got %+v", sd.Hunks)
	}
}

func TestCompareDoesNotDropEntriesOnIdentityCollision(t *testing.T) {
	// Both entries share the address-list identity "blocklist|10.0.0.5"
	// (same list+address, no comment to disambiguate); a naive id->entry
	// map would let the second overwrite the first and silently lose any
	// diff against it.
	a := []model.Entry{
		{"list": "blocklist", "address": "10.0.0.5", "timeout": "1h"},
		{"list": "blocklist", "address": "10.0.0.5", "timeout": "2h"},
	}
	b := []model.Entry{
		{"list": "blocklist", "address": "10.0.0.5", "timeout": "1h"},
		{"list": "blocklist", "address": "10.0.0.5", "timeout": "3h"},
	}

	sd := Compare("ip/firewall/address-list", a, b, nil)
	if len(sd.Hunks) != 1 {
		t.Fatalf("expected 1 hunk for the second colliding entry, got %d: %+v", len(sd.Hunks), sd.Hunks)
	}
	if !sd.Hunks[0].OnA || !sd.Hunks[0].OnB {
		t.Fatalf("expected a changed hunk present on both sides, got %+v", sd.Hunks[0])
	}
}

func TestCompareFirewallUsesChainOrdinalWhenNoComment(t *testing.T) {
	a := []model.Entry{
		{"chain": "input", "action": "accept"},
		{"chain": "input", "action": "drop"},
	}
	b := []model.Entry{
		{"chain": "input", "action": "accept"},
		{"chain": "input", "action": "reject"},
	}

	sd := Compare("ip/firewall/filter", a, b, nil)
	if len(sd.Hunks) != 1 {
		t.Fatalf("expected 1 hunk for the second input rule, got %d: %+v", len(sd.Hunks), sd.Hunks)
	}
	if sd.Hunks[0].Identity != "input#2" {
		t.Fatalf("expected identity input#2, got %q", sd.Hunks[0].Identity)
	}
}

func TestCompareCollisionReportsOccurrence(t *testing.T) {
	a := []model.Entry{
		{"list": "blocklist", "address": "10.0.0.5", "timeout": "1h"},
		{"list": "blocklist", "address": "10.0.0.5", "timeout": "2h"},
	}
	b := []model.Entry{
		{"list": "blocklist", "address": "10.0.0.5", "timeout": "1h"},
	}

	sd := Compare("ip/firewall/address-list", a, b, nil)
	if len(sd.Hunks) != 1 || sd.Hunks[0].Occurrence != 1 || !sd.Hunks[0].OnA || sd.Hunks[0].OnB {
		t.Fatalf("expected one only-on-A hunk at occurrence 1, got %+v", sd.Hunks)
	}
}

// Two rules sharing a comment used to collapse onto one identity; after
// dedupe each is matched (and reported) individually.
func TestCompareDisambiguatesDuplicateComments(t *testing.T) {
	a := []model.Entry{
		{"chain": "forward", "comment": "web", "dst-port": "80"},
		{"chain": "forward", "comment": "web", "dst-port": "443"},
	}
	b := []model.Entry{
		{"chain": "forward", "comment": "web", "dst-port": "80"},
		{"chain": "forward", "comment": "web", "dst-port": "8443"},
	}

	sd := Compare("ip/firewall/filter", a, b, nil)
	if len(sd.Hunks) != 1 || sd.Hunks[0].Identity != "web#2" || sd.Hunks[0].Occurrence != 0 {
		t.Fatalf("expected a single change on web#2, got %+v", sd.Hunks)
	}
}

// Inserting an uncommented rule after "allow-ssh" re-identifies only that
// block: the rule after "drop-rest" still matches and produces no hunk.
func TestCompareAnchoredInsertionIsLocal(t *testing.T) {
	a := []model.Entry{
		{"chain": "input", "comment": "allow-ssh", "action": "accept"},
		{"chain": "input", "action": "accept", "protocol": "icmp"},
		{"chain": "input", "comment": "drop-rest", "action": "drop"},
		{"chain": "input", "action": "log"},
	}
	b := []model.Entry{
		{"chain": "input", "comment": "allow-ssh", "action": "accept"},
		{"chain": "input", "comment": "drop-rest", "action": "drop"},
		{"chain": "input", "action": "log"},
	}

	sd := Compare("ip/firewall/filter", a, b, nil)
	if len(sd.Hunks) != 1 || sd.Hunks[0].Identity != "input@allow-ssh#1" || !sd.Hunks[0].OnA || sd.Hunks[0].OnB {
		t.Fatalf("expected only the inserted rule to differ, got %+v", sd.Hunks)
	}
}

func TestCompareIgnoresUntaggedRoutes(t *testing.T) {
	a := []model.Entry{
		{"dst-address": "0.0.0.0/0", "gateway": "203.0.113.1"},
		{"dst-address": "10.9.0.0/16", "gateway": "10.0.0.9", "comment": "mtha:vpn"},
	}
	b := []model.Entry{
		{"dst-address": "0.0.0.0/0", "gateway": "198.51.100.1"},
	}

	sd := Compare("ip/route", a, b, nil)
	if len(sd.Hunks) != 1 || sd.Hunks[0].Identity != "mtha:vpn" {
		t.Fatalf("expected only the tagged route to be diffed, got %+v", sd.Hunks)
	}
}

// Current behaviour, recorded for docs/design-questions.md (question 2):
// commented rules match by comment regardless of position, so the same two
// rules in the opposite order on each router compare clean even though
// RouterOS evaluates a chain in list order and the policies differ.
func TestCompareIgnoresCommentedRuleOrder(t *testing.T) {
	allow := model.Entry{"chain": "forward", "action": "accept", "src-address": "192.0.2.10", "comment": "allow-host"}
	drop := model.Entry{"chain": "forward", "action": "drop", "src-address": "192.0.2.0/24", "comment": "drop-subnet"}

	a := []model.Entry{allow, drop}
	b := []model.Entry{drop, allow}

	if sd := Compare("ip/firewall/filter", a, b, nil); !sd.Clean() {
		t.Fatalf("reordered commented rules now produce hunks (order drift detected?): %+v", sd.Hunks)
	}
}
