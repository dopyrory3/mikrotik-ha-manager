package model

import (
	"reflect"
	"testing"
)

func TestBuildIdentitiesPrefersComment(t *testing.T) {
	entries := []Entry{
		{"chain": "input", "comment": "allow-ssh"},
	}
	got := BuildIdentities("ip/firewall/filter", entries)
	want := []string{"allow-ssh"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// Rules before the first commented rule in their chain keep a plain chain
// ordinal, counted per chain.
func TestBuildIdentitiesFirewallFallsBackToChainOrdinal(t *testing.T) {
	entries := []Entry{
		{"chain": "input"},
		{"chain": "input"},
		{"chain": "forward"},
	}
	got := BuildIdentities("ip/firewall/filter", entries)
	want := []string{"input#1", "input#2", "forward#1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestBuildIdentitiesDedupesRepeatedComment(t *testing.T) {
	entries := []Entry{
		{"chain": "forward", "comment": "web"},
		{"chain": "input", "comment": "web"},
		{"chain": "forward", "comment": "web"},
		{"chain": "input", "comment": "ssh"},
	}
	got := BuildIdentities("ip/firewall/filter", entries)
	want := []string{"web", "web#2", "web#3", "ssh"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// A literal comment that looks like a dedupe suffix must not collide with a
// deduplicated identity handed out before it (or after it).
func TestBuildIdentitiesDedupeAvoidsLiteralSuffixCollision(t *testing.T) {
	entries := []Entry{
		{"chain": "input", "comment": "web"},
		{"chain": "input", "comment": "web"},
		{"chain": "input", "comment": "web#2"},
		{"chain": "input", "comment": "web"},
	}
	got := BuildIdentities("ip/firewall/filter", entries)
	want := []string{"web", "web#2", "web#2#2", "web#3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestBuildIdentitiesAnchorsUncommentedRulesToPrecedingComment(t *testing.T) {
	entries := []Entry{
		{"chain": "input"},
		{"chain": "input", "comment": "allow-ssh"},
		{"chain": "input"},
		{"chain": "forward"},
		{"chain": "input"},
		{"chain": "input", "comment": "drop-rest"},
		{"chain": "input"},
	}
	got := BuildIdentities("ip/firewall/filter", entries)
	want := []string{
		"input#1",
		"allow-ssh",
		"input@allow-ssh#1",
		"forward#1",
		"input@allow-ssh#2",
		"drop-rest",
		"input@drop-rest#1",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// The point of anchoring: inserting an uncommented rule re-identifies only
// the uncommented block after it, up to the next commented rule. Everything
// from that commented rule onward keeps its identity.
func TestBuildIdentitiesInsertionOnlyReidentifiesFollowingBlock(t *testing.T) {
	before := []Entry{
		{"chain": "input", "comment": "allow-ssh"},
		{"chain": "input", "action": "accept"},
		{"chain": "input", "comment": "drop-rest"},
		{"chain": "input", "action": "log"},
	}
	after := []Entry{
		{"chain": "input", "comment": "allow-ssh"},
		{"chain": "input", "action": "inserted"},
		{"chain": "input", "action": "accept"},
		{"chain": "input", "comment": "drop-rest"},
		{"chain": "input", "action": "log"},
	}

	gotBefore := BuildIdentities("ip/firewall/filter", before)
	gotAfter := BuildIdentities("ip/firewall/filter", after)

	if gotBefore[3] != gotAfter[4] || gotAfter[4] != "input@drop-rest#1" {
		t.Fatalf("rule after the next commented rule was re-identified: before %v, after %v", gotBefore, gotAfter)
	}
	if gotAfter[1] != "input@allow-ssh#1" || gotAfter[2] != "input@allow-ssh#2" {
		t.Fatalf("unexpected identities for the affected block: %v", gotAfter)
	}
}

// A deduplicated comment is also what later uncommented rules anchor to.
func TestBuildIdentitiesAnchorsToDedupedComment(t *testing.T) {
	entries := []Entry{
		{"chain": "input", "comment": "web"},
		{"chain": "input", "comment": "web"},
		{"chain": "input"},
	}
	got := BuildIdentities("ip/firewall/nat", entries)
	want := []string{"web", "web#2", "input@web#2#1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// identityCase is one section's input and the identities BuildIdentities
// must give it.
type identityCase struct {
	name    string
	entries []Entry
	want    []string
}

func runIdentityCases(t *testing.T, section string, cases []identityCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := BuildIdentities(section, tc.entries)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("%s: got %v, want %v", section, got, tc.want)
			}
		})
	}
}

// The sections below follow docs/design-questions.md §1, Recommendation B:
// a natural key where the section has one, with comment an ordinary field.

func TestBuildIdentitiesAddressList(t *testing.T) {
	runIdentityCases(t, "ip/firewall/address-list", []identityCase{
		{
			name: "list and address",
			entries: []Entry{
				{"list": "blocklist", "address": "10.0.0.5"},
				{"list": "vpn-allowed", "address": "10.0.0.5"},
			},
			want: []string{"blocklist|10.0.0.5", "vpn-allowed|10.0.0.5"},
		},
		{
			name: "comment ignored",
			entries: []Entry{
				{"list": "blocked", "address": "198.51.100.1", "comment": "feed"},
			},
			want: []string{"blocked|198.51.100.1"},
		},
		{
			// The case comment-first got wrong: a shared comment made the
			// identity positional. Each entry now keeps its own key however
			// many share the comment or are inserted ahead of it.
			name: "shared comment",
			entries: []Entry{
				{"list": "blocked", "address": "198.51.100.9", "comment": "feed"},
				{"list": "blocked", "address": "198.51.100.1", "comment": "feed"},
				{"list": "blocked", "address": "198.51.100.2", "comment": "feed"},
			},
			want: []string{"blocked|198.51.100.9", "blocked|198.51.100.1", "blocked|198.51.100.2"},
		},
		{
			name: "repeated key is not deduplicated",
			entries: []Entry{
				{"list": "blocked", "address": "198.51.100.1"},
				{"list": "blocked", "address": "198.51.100.1"},
			},
			want: []string{"blocked|198.51.100.1", "blocked|198.51.100.1"},
		},
	})
}

func TestBuildIdentitiesNameKeyedSections(t *testing.T) {
	for _, section := range []string{"ip/dhcp-server", "ip/service", "user", "system/script", "system/scheduler"} {
		t.Run(section, func(t *testing.T) {
			runIdentityCases(t, section, []identityCase{
				{
					name:    "name",
					entries: []Entry{{"name": "backup-config"}},
					want:    []string{"backup-config"},
				},
				{
					// A comment edit must be a patch, not a delete + create
					// (for user, a delete that REST cannot undo).
					name: "comment ignored",
					entries: []Entry{
						{"name": "one", "comment": "shared"},
						{"name": "two", "comment": "shared"},
					},
					want: []string{"one", "two"},
				},
				{
					name:    "address not used",
					entries: []Entry{{"name": "lan", "address": "192.0.2.1"}},
					want:    []string{"lan"},
				},
			})
		})
	}
}

func TestBuildIdentitiesDHCPNetwork(t *testing.T) {
	runIdentityCases(t, "ip/dhcp-server/network", []identityCase{
		{
			name: "address, comment ignored",
			entries: []Entry{
				{"address": "192.168.88.0/24", "gateway": "192.168.88.1", "comment": "lab: lan"},
				{"address": "192.168.89.0/24", "comment": "lab: lan"},
			},
			want: []string{"192.168.88.0/24", "192.168.89.0/24"},
		},
	})
}

func TestBuildIdentitiesDHCPLease(t *testing.T) {
	runIdentityCases(t, "ip/dhcp-server/lease", []identityCase{
		{
			name: "server and mac-address",
			entries: []Entry{
				{"server": "dhcp-lab", "mac-address": "02:00:00:00:AA:01", "address": "192.168.88.50", "comment": "lab: static lease"},
				{"server": "dhcp-guest", "mac-address": "02:00:00:00:AA:01", "address": "192.168.89.50"},
			},
			want: []string{"dhcp-lab|02:00:00:00:AA:01", "dhcp-guest|02:00:00:00:AA:01"},
		},
		{
			name: "address when mac-address is empty",
			entries: []Entry{
				{"server": "dhcp-lab", "mac-address": "", "address": "192.168.88.51"},
				{"server": "dhcp-lab", "address": "192.168.88.52"},
			},
			want: []string{"192.168.88.51", "192.168.88.52"},
		},
	})
}

func TestBuildIdentitiesDNSStatic(t *testing.T) {
	runIdentityCases(t, "ip/dns/static", []identityCase{
		{
			// The lab fixture: two A records under one name sharing a
			// comment, and a CNAME.
			name: "A and CNAME",
			entries: []Entry{
				{"name": "svc.lab.example", "type": "A", "address": "192.168.88.10", "comment": "lab: two addresses"},
				{"name": "svc.lab.example", "type": "A", "address": "192.168.88.11", "comment": "lab: two addresses"},
				{"name": "www.lab.example", "type": "CNAME", "cname": "svc.lab.example"},
			},
			want: []string{
				"svc.lab.example|A|192.168.88.10",
				"svc.lab.example|A|192.168.88.11",
				"www.lab.example|CNAME|svc.lab.example",
			},
		},
		{
			// What comment-first plus the address fallback got wrong: a
			// CNAME was a bare ordinal, re-identified by any address-less
			// record inserted ahead of it.
			name: "CNAME is not positional",
			entries: []Entry{
				{"name": "mail.example", "type": "CNAME", "cname": "svc.example"},
				{"name": "svc.example", "type": "A", "address": "192.0.2.10"},
				{"name": "www.example", "type": "CNAME", "cname": "svc.example"},
			},
			want: []string{"mail.example|CNAME|svc.example", "svc.example|A|192.0.2.10", "www.example|CNAME|svc.example"},
		},
		{
			name:    "missing type is A",
			entries: []Entry{{"name": "host1", "address": "10.0.0.5"}},
			want:    []string{"host1|A|10.0.0.5"},
		},
		{
			name: "unsurveyed type has no value",
			entries: []Entry{
				{"name": "example", "type": "TXT", "text": "one"},
				{"name": "example", "type": "TXT", "text": "two"},
			},
			want: []string{"example|TXT", "example|TXT"},
		},
	})
}

func TestBuildIdentitiesRoute(t *testing.T) {
	runIdentityCases(t, "ip/route", []identityCase{
		{
			name:    "tag comment",
			entries: []Entry{{"dst-address": "10.9.0.0/16", "gateway": "10.0.0.9", "comment": "mtha:vpn"}},
			want:    []string{"mtha:vpn"},
		},
		{
			name:    "dst-address and gateway",
			entries: []Entry{{"dst-address": "0.0.0.0/0", "gateway": "10.0.0.1"}},
			want:    []string{"0.0.0.0/0->10.0.0.1"},
		},
	})
}

// A section with no entry in the table keeps the old fallback, except that
// comment moves from first to last before the ordinal.
func TestBuildIdentitiesFallback(t *testing.T) {
	runIdentityCases(t, "tool/netwatch", []identityCase{
		{
			name:    "name before comment",
			entries: []Entry{{"name": "backup-config", "comment": "nightly"}},
			want:    []string{"backup-config"},
		},
		{
			name:    "address before comment",
			entries: []Entry{{"address": "192.0.2.1", "comment": "gw"}},
			want:    []string{"192.0.2.1"},
		},
		{
			name: "comment before ordinal, deduplicated",
			entries: []Entry{
				{"host": "192.168.88.1", "comment": "lab: vip"},
				{"host": "192.168.88.2"},
				{"host": "192.168.88.3", "comment": "lab: vip"},
				{"host": "192.168.88.4"},
			},
			want: []string{"lab: vip", "#1", "lab: vip#2", "#2"},
		},
	})
}
