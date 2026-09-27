//go:build lab

package labtest_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"mtha/internal/diff"
	"mtha/internal/labtest"
	"mtha/internal/model"
	"mtha/internal/routeros"
)

// The pair is drift-free at baseline in every section it syncs, and the
// fixture gives each of those sections something to compare: an empty
// section would pass the diff without exercising it.
func TestLabBaselineIsDriftFree(t *testing.T) {
	lab := labtest.New(t, labtest.ReadOnly())
	ctx := context.Background()

	for _, section := range lab.Pair.Sync.Sections {
		a, err := lab.A.GetSection(ctx, section)
		if err != nil {
			t.Fatalf("router a %s: %v", section, err)
		}
		b, err := lab.B.GetSection(ctx, section)
		if err != nil {
			t.Fatalf("router b %s: %v", section, err)
		}
		if len(model.Select(section, a)) == 0 {
			t.Errorf("%s: nothing selected on router a; the fixture should populate it", section)
		}
		if d := diff.Compare(section, a, b, lab.Pair.Sync.Exempt); !d.Clean() {
			t.Errorf("%s drifts at baseline: %+v", section, d.Hunks)
		}
	}
}

// A disabled VRRP instance carries neither role flag, so Role() has only
// RoleUnknown to give — the safe answer.
func TestLabDisabledVRRPHasNoRoleFlags(t *testing.T) {
	lab := labtest.New(t)
	ctx := context.Background()

	v := onlyVRRP(t, lab.B)
	if err := lab.B.Patch(ctx, "/interface/vrrp/"+v.ID, map[string]string{"disabled": "true"}, nil); err != nil {
		t.Fatal(err)
	}
	var raw map[string]string
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(500 * time.Millisecond) {
		if err := lab.B.Get(ctx, "/interface/vrrp/"+v.ID, &raw); err != nil {
			t.Fatal(err)
		}
		_, master := raw["master"]
		_, backup := raw["backup"]
		if raw["disabled"] == "true" && !master && !backup {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("disabled instance still carries a role flag: %v", raw)
		}
	}
	t.Logf("disabled instance: running=%q invalid=%q", raw["running"], raw["invalid"])
	if got := onlyVRRP(t, lab.B).Role(); got != routeros.RoleUnknown {
		t.Errorf("Role() of a disabled instance = %s, want unknown", got)
	}
}

// A section of thousands of entries comes back whole from one GET: no
// paging, no truncation, in configured order.
func TestLabLargeSectionComesBackWhole(t *testing.T) {
	lab := labtest.New(t)
	ctx := context.Background()
	const n = 3000

	// A chain nothing jumps to, so the rules never match a packet. One
	// script adds them all; over REST one PUT each would take minutes.
	script := fmt.Sprintf(`:for i from=1 to=%d do={/ip/firewall/filter/add chain=lab-bulk action=accept protocol=tcp dst-port=(10000+$i) comment=("lab-bulk-" . $i)}`, n)
	if err := lab.A.Command(ctx, "/execute", map[string]string{"script": script, "as-string": ""}, nil); err != nil {
		t.Fatal(err)
	}

	var rules []map[string]string
	start := time.Now()
	if err := lab.A.Get(ctx, "/ip/firewall/filter", &rules); err != nil {
		t.Fatal(err)
	}
	t.Logf("GET of %d rules took %s", len(rules), time.Since(start).Round(time.Millisecond))

	var bulk []map[string]string
	for _, r := range rules {
		if r["chain"] == "lab-bulk" {
			bulk = append(bulk, r)
		}
	}
	if len(bulk) != n {
		t.Fatalf("one GET returned %d of the %d lab-bulk rules (%d rules in all)", len(bulk), n, len(rules))
	}
	for i, r := range bulk {
		if want := fmt.Sprintf("lab-bulk-%d", i+1); r["comment"] != want {
			t.Fatalf("rule %d is %q, want %q: not in configured order", i, r["comment"], want)
		}
	}
}
