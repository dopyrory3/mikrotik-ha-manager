//go:build lab

package labtest_test

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"mtha/internal/diff"
	"mtha/internal/labtest"
	"mtha/internal/model"
	"mtha/internal/routeros"
)

// Sections mtha syncs when a pair lists them that testlab/pairs.yaml does
// not: the fixture populates them, so the baseline must hold for them too.
// tool/netwatch is left out on purpose: it is Runtime's, and its state
// (status, since, done-tests, failed-tests) is not stripped, so it is not
// comparable (docs/lab-rest-contract.md, tool/netwatch).
var unlistedSections = []string{"ip/firewall/mangle", "ip/firewall/raw", "interface/vrrp"}

// The pair is drift-free at baseline in every section it syncs, and the
// fixture gives each of those sections something to compare: an empty
// section would pass the diff without exercising it. Drift is then run a
// second time with nothing changed, and each router's normalised entries
// must be identical to the first run's: a field that changes on its own
// would otherwise show as drift that comes and goes.
func TestLabBaselineIsDriftFree(t *testing.T) {
	lab := labtest.New(t, labtest.ReadOnly())
	ctx := context.Background()
	sections := append(slices.Clone(lab.Pair.Sync.Sections), unlistedSections...)
	exempt := lab.Pair.Sync.Exempt

	read := func() (a, b map[string][]model.Entry) {
		a, b = map[string][]model.Entry{}, map[string][]model.Entry{}
		for _, section := range sections {
			var err error
			if a[section], err = lab.A.GetSection(ctx, section); err != nil {
				t.Fatalf("router a %s: %v", section, err)
			}
			if b[section], err = lab.B.GetSection(ctx, section); err != nil {
				t.Fatalf("router b %s: %v", section, err)
			}
		}
		return a, b
	}

	a1, b1 := read()
	for _, section := range sections {
		if len(model.Select(section, a1[section])) == 0 {
			t.Errorf("%s: nothing selected on router a; the fixture should populate it", section)
		}
		if d := diff.Compare(section, a1[section], b1[section], exempt); !d.Clean() {
			t.Errorf("%s drifts at baseline: %+v", section, d.Hunks)
		}
	}

	// Long enough for firewall counters and VRRP adverts to move.
	time.Sleep(5 * time.Second)
	a2, b2 := read()
	for _, section := range sections {
		if d1, d2 := diff.Compare(section, a1[section], b1[section], exempt), diff.Compare(section, a2[section], b2[section], exempt); !reflect.DeepEqual(d1, d2) {
			t.Errorf("%s: drift differs between two runs with nothing changed:\nfirst  %+v\nsecond %+v", section, d1, d2)
		}
		for _, r := range []struct {
			key           string
			first, second []model.Entry
		}{{"a", a1[section], a2[section]}, {"b", b1[section], b2[section]}} {
			if n1, n2 := model.Normalize(section, r.first, exempt), model.Normalize(section, r.second, exempt); !reflect.DeepEqual(n1, n2) {
				t.Errorf("router %s %s: normalised entries changed on their own:\nfirst  %v\nsecond %v", r.key, section, n1, n2)
			}
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
