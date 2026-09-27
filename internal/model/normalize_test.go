package model

import "testing"

func TestNormalizeDropsVRRPRoleFlags(t *testing.T) {
	tests := []struct {
		name string
		raw  Entry
	}{
		{"master", Entry{"name": "vrrp-lan", "interface": "ether1", "master": "true", "mac-address": "00:00:5e:00:01:01"}},
		{"backup", Entry{"name": "vrrp-lan", "interface": "ether1", "backup": "true", "mac-address": "00:00:5e:00:01:01"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out := Normalize("interface/vrrp", []Entry{tc.raw}, nil)
			for _, flag := range []string{"master", "backup", "mac-address"} {
				if _, ok := out[0][flag]; ok {
					t.Errorf("%s role flag should be dropped, got %v", flag, out[0])
				}
			}
		})
	}
}

func TestNormalizeDropsIDAndDynamic(t *testing.T) {
	raw := []Entry{
		{".id": "*1", "name": "static-rule", "comment": "keep"},
		{".id": "*2", "name": "learned-rule", "dynamic": "true"},
	}

	out := Normalize("ip/firewall/filter", raw, nil)

	if len(out) != 1 {
		t.Fatalf("expected 1 entry after dropping dynamic, got %d", len(out))
	}
	if _, ok := out[0][".id"]; ok {
		t.Fatalf(".id should have been dropped, got %v", out[0])
	}
	if out[0]["comment"] != "keep" {
		t.Fatalf("expected comment field preserved, got %v", out[0])
	}
}

func TestNormalizeDropsFieldExemptEntries(t *testing.T) {
	raw := []Entry{
		{"name": "vrrp-lan", "priority": "200", "interface": "ether1"},
	}

	out := Normalize("interface/vrrp", raw, []string{"interface/vrrp.priority"})

	if _, ok := out[0]["priority"]; ok {
		t.Fatalf("priority should have been exempted, got %v", out[0])
	}
	if out[0]["interface"] != "ether1" {
		t.Fatalf("non-exempt field should survive, got %v", out[0])
	}
}

func TestSectionExempt(t *testing.T) {
	exempt := []string{"system/identity", "interface/vrrp.priority"}

	if !SectionExempt("system/identity", exempt) {
		t.Error("expected system/identity to be a whole-section exemption")
	}
	if SectionExempt("interface/vrrp", exempt) {
		t.Error("interface/vrrp.priority is a field exemption, not a whole-section one")
	}
}

func TestEntriesEqualTreatsAbsentAsDefault(t *testing.T) {
	a := Entry{"name": "rule", "disabled": "false"}
	b := Entry{"name": "rule"} // disabled omitted == default

	equal, changes := EntriesEqual(a, b)
	if !equal {
		t.Fatalf("expected entries equal treating absent field as default, got changes: %+v", changes)
	}
}

func TestEntriesEqualDetectsRealDifference(t *testing.T) {
	a := Entry{"name": "rule", "action": "accept"}
	b := Entry{"name": "rule", "action": "drop"}

	equal, changes := EntriesEqual(a, b)
	if equal {
		t.Fatal("expected entries to differ")
	}
	if len(changes) != 1 || changes[0].Field != "action" {
		t.Fatalf("expected single action change, got %+v", changes)
	}
}

func TestEntriesEqualMissingNonDefaultDiffers(t *testing.T) {
	a := Entry{"name": "rule", "comment": "important"}
	b := Entry{"name": "rule"}

	equal, changes := EntriesEqual(a, b)
	if equal {
		t.Fatal("expected entries to differ: comment is present and non-default on one side only")
	}
	if len(changes) != 1 || changes[0].Field != "comment" {
		t.Fatalf("expected single comment change, got %+v", changes)
	}
}

func TestSelectRouteIsOptInByTag(t *testing.T) {
	raw := []Entry{
		{".id": "*1", "dst-address": "0.0.0.0/0", "gateway": "203.0.113.1"},
		{".id": "*2", "dst-address": "10.9.0.0/16", "gateway": "10.0.0.9", "comment": "mtha:vpn"},
		{".id": "*3", "dst-address": "10.8.0.0/16", "gateway": "10.0.0.8", "comment": "site-b"},
		{".id": "*4", "dst-address": "10.0.0.0/24", "dynamic": "true", "comment": "mtha:connected"},
	}

	got := Select("ip/route", raw)
	if len(got) != 1 || got[0][".id"] != "*2" {
		t.Fatalf("expected only the mtha:-tagged static route, got %v", got)
	}

	norm := Normalize("ip/route", raw, nil)
	if len(norm) != 1 || norm[0]["comment"] != "mtha:vpn" {
		t.Fatalf("Normalize must apply the same selection, got %v", norm)
	}
}

// The tag filter is specific to ip/route; other sections keep untagged
// entries.
func TestSelectKeepsUntaggedOutsideRoutes(t *testing.T) {
	raw := []Entry{{"chain": "input"}, {"chain": "input", "comment": "x"}}
	if got := Select("ip/firewall/filter", raw); len(got) != 2 {
		t.Fatalf("expected both firewall rules selected, got %v", got)
	}
}

func TestNormalizeDropsStateFields(t *testing.T) {
	raw := []Entry{{
		".id": "*1", "chain": "input", "action": "accept",
		"bytes": "123", "packets": "4", "invalid": "false", "dynamic": "false",
	}}

	out := Normalize("ip/firewall/filter", raw, nil)
	for _, f := range []string{".id", "bytes", "packets", "invalid", "dynamic"} {
		if _, ok := out[0][f]; ok {
			t.Errorf("state field %q should have been dropped, got %v", f, out[0])
		}
	}
	if out[0]["action"] != "accept" {
		t.Errorf("config field should survive, got %v", out[0])
	}
}

func TestEntriesEqualSortsChanges(t *testing.T) {
	a := Entry{"z": "1", "a": "1", "m": "1"}
	b := Entry{"z": "2", "a": "2", "m": "2"}

	_, changes := EntriesEqual(a, b)
	if len(changes) != 3 || changes[0].Field != "a" || changes[1].Field != "m" || changes[2].Field != "z" {
		t.Fatalf("expected changes sorted by field, got %+v", changes)
	}
}
