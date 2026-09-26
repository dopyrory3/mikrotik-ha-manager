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
