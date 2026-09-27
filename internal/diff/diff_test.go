package diff

import (
	"reflect"
	"testing"

	"mtha/internal/model"
)

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

// Order drift (docs/design-questions.md §2): commented rules match by
// comment regardless of position, so a permutation of them is invisible to
// the hunks. It is reported per chain instead, over the rules on both
// routers, and naming a smallest set of rules that would have to move.
func TestCompareDetectsRuleOrder(t *testing.T) {
	rule := func(chain, comment string) model.Entry {
		return model.Entry{"chain": chain, "action": "accept", "comment": comment}
	}
	plain := func(chain, port string) model.Entry {
		return model.Entry{"chain": chain, "action": "accept", "dst-port": port}
	}
	allow := model.Entry{"chain": "forward", "action": "accept", "src-address": "192.0.2.10", "comment": "allow-host"}
	drop := model.Entry{"chain": "forward", "action": "drop", "src-address": "192.0.2.0/24", "comment": "drop-subnet"}

	cases := []struct {
		name      string
		section   string
		a, b      []model.Entry
		want      []OrderHunk
		wantHunks int
	}{
		{
			name:    "same order is clean",
			section: "ip/firewall/filter",
			a:       []model.Entry{allow, drop},
			b:       []model.Entry{allow, drop},
		},
		{
			name:    "swapped commented rules",
			section: "ip/firewall/filter",
			a:       []model.Entry{allow, drop},
			b:       []model.Entry{drop, allow},
			want:    []OrderHunk{{Chain: "forward", Moved: []RuleRef{{Identity: "allow-host"}}, Rules: 2}},
		},
		{
			name:    "one rule moved from the end to the front moves only that rule",
			section: "ip/firewall/filter",
			a:       []model.Entry{rule("input", "r1"), rule("input", "r2"), rule("input", "r3"), rule("input", "r4")},
			b:       []model.Entry{rule("input", "r4"), rule("input", "r1"), rule("input", "r2"), rule("input", "r3")},
			want:    []OrderHunk{{Chain: "input", Moved: []RuleRef{{Identity: "r4"}}, Rules: 4}},
		},
		{
			name:    "a block keeps its uncommented followers",
			section: "ip/firewall/filter",
			a:       []model.Entry{rule("input", "x"), plain("input", "22"), rule("input", "y"), plain("input", "53")},
			b:       []model.Entry{rule("input", "y"), plain("input", "53"), rule("input", "x"), plain("input", "22")},
			want:    []OrderHunk{{Chain: "input", Moved: []RuleRef{{Identity: "x"}, {Identity: "input@x#1"}}, Rules: 4}},
		},
		{
			name:    "different interleaving of chains is cosmetic",
			section: "ip/firewall/filter",
			a:       []model.Entry{rule("input", "i1"), rule("forward", "f1"), rule("input", "i2"), rule("forward", "f2")},
			b:       []model.Entry{rule("forward", "f1"), rule("forward", "f2"), rule("input", "i1"), rule("input", "i2")},
		},
		{
			name:    "order is per chain",
			section: "ip/firewall/nat",
			a:       []model.Entry{rule("srcnat", "s1"), rule("dstnat", "d1"), rule("srcnat", "s2"), rule("dstnat", "d2")},
			b:       []model.Entry{rule("srcnat", "s1"), rule("dstnat", "d2"), rule("srcnat", "s2"), rule("dstnat", "d1")},
			want:    []OrderHunk{{Chain: "dstnat", Moved: []RuleRef{{Identity: "d1"}}, Rules: 2}},
		},
		{
			name:      "rules on one router only are hunks, not order",
			section:   "ip/firewall/filter",
			a:         []model.Entry{rule("input", "new"), rule("input", "r1"), rule("input", "r2")},
			b:         []model.Entry{rule("input", "r1"), rule("input", "r2"), rule("input", "old")},
			wantHunks: 2,
		},
		{
			name:      "a rule whose chain differs is a field change, not order",
			section:   "ip/firewall/filter",
			a:         []model.Entry{rule("input", "r1"), rule("input", "r2")},
			b:         []model.Entry{rule("forward", "r2"), rule("input", "r1")},
			wantHunks: 1,
		},
		{
			name:      "swapped uncommented rules are field changes",
			section:   "ip/firewall/filter",
			a:         []model.Entry{plain("input", "22"), plain("input", "53")},
			b:         []model.Entry{plain("input", "53"), plain("input", "22")},
			wantHunks: 2,
		},
		{
			name:    "dynamic rules are ignored",
			section: "ip/firewall/filter",
			a:       []model.Entry{{"chain": "input", "comment": "dyn", "dynamic": "true"}, rule("input", "r1")},
			b:       []model.Entry{rule("input", "r1"), {"chain": "input", "comment": "dyn", "dynamic": "true"}},
		},
		{
			name:    "non-firewall sections have no order",
			section: "ip/firewall/address-list",
			a:       []model.Entry{{"list": "l", "address": "10.0.0.1", "comment": "one"}, {"list": "l", "address": "10.0.0.2", "comment": "two"}},
			b:       []model.Entry{{"list": "l", "address": "10.0.0.2", "comment": "two"}, {"list": "l", "address": "10.0.0.1", "comment": "one"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sd := Compare(tc.section, tc.a, tc.b, nil)
			if !reflect.DeepEqual(sd.Order, tc.want) {
				t.Errorf("Order = %+v, want %+v", sd.Order, tc.want)
			}
			if len(sd.Hunks) != tc.wantHunks {
				t.Errorf("got %d hunk(s), want %d: %+v", len(sd.Hunks), tc.wantHunks, sd.Hunks)
			}
			if clean := tc.want == nil && tc.wantHunks == 0; sd.Clean() != clean {
				t.Errorf("Clean() = %v, want %v", sd.Clean(), clean)
			}
		})
	}
}

// The smallest set to move is the complement of a longest increasing
// subsequence.
func TestLongestIncreasing(t *testing.T) {
	cases := []struct {
		seq  []int
		want int
	}{
		{nil, 0},
		{[]int{0}, 1},
		{[]int{0, 1, 2, 3}, 4},
		{[]int{3, 2, 1, 0}, 1},
		{[]int{3, 0, 1, 2}, 3},
		{[]int{1, 0, 3, 2, 5, 4}, 3},
		{[]int{2, 5, 3, 7, 11, 8, 10, 13, 6}, 6},
	}
	for _, tc := range cases {
		member := longestIncreasing(tc.seq)
		var picked []int
		for i, in := range member {
			if in {
				picked = append(picked, tc.seq[i])
			}
		}
		if len(picked) != tc.want {
			t.Errorf("longestIncreasing(%v) picked %v, want length %d", tc.seq, picked, tc.want)
		}
		for i := 1; i < len(picked); i++ {
			if picked[i] <= picked[i-1] {
				t.Errorf("longestIncreasing(%v) picked %v, not increasing", tc.seq, picked)
			}
		}
	}
}
