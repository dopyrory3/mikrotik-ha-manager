package model

import "testing"

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
