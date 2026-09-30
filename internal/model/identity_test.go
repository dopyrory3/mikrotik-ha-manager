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
			// A type the probe did not return has no known value field, so
			// it keys on name and type and is paired by occurrence.
			name: "unknown type has no value",
			entries: []Entry{
				{"name": "example", "type": "CAA", "value": "one"},
				{"name": "example", "type": "CAA", "value": "two"},
			},
			want: []string{"example|CAA", "example|CAA"},
		},
	})
}

// One case per record type in write probe 1 of docs/lab-rest-contract.md,
// each shaped as the device returns it (only the type's value fields, plus
// ttl, which is not part of the key). Each case puts two records under one
// name that RouterOS accepts side by side (write probe 2), so each must get
// its own identity rather than share "name|type".
func TestBuildIdentitiesDNSStaticPerType(t *testing.T) {
	runIdentityCases(t, "ip/dns/static", []identityCase{
		{
			name: "A",
			entries: []Entry{
				{"name": "h.example", "type": "A", "ttl": "1d", "address": "192.0.2.1"},
				{"name": "h.example", "type": "A", "ttl": "1d", "address": "192.0.2.2"},
			},
			want: []string{"h.example|A|192.0.2.1", "h.example|A|192.0.2.2"},
		},
		{
			// AAAA shares A's field, not a field of its own.
			name: "AAAA",
			entries: []Entry{
				{"name": "h.example", "type": "AAAA", "ttl": "1d", "address": "2001:db8::1"},
				{"name": "h.example", "type": "AAAA", "ttl": "1d", "address": "2001:db8::2"},
			},
			want: []string{"h.example|AAAA|2001:db8::1", "h.example|AAAA|2001:db8::2"},
		},
		{
			name: "CNAME",
			entries: []Entry{
				{"name": "h.example", "type": "CNAME", "ttl": "1d", "cname": "one.example"},
				{"name": "h.example", "type": "CNAME", "ttl": "1d", "cname": "two.example"},
			},
			want: []string{"h.example|CNAME|one.example", "h.example|CNAME|two.example"},
		},
		{
			// Two MX records differing only in preference are two records.
			name: "MX",
			entries: []Entry{
				{"name": "h.example", "type": "MX", "ttl": "1d", "mx-exchange": "mail.example", "mx-preference": "0"},
				{"name": "h.example", "type": "MX", "ttl": "1d", "mx-exchange": "mail.example", "mx-preference": "10"},
				{"name": "h.example", "type": "MX", "ttl": "1d", "mx-exchange": "mx2.example", "mx-preference": "10"},
			},
			want: []string{
				"h.example|MX|0|mail.example",
				"h.example|MX|10|mail.example",
				"h.example|MX|10|mx2.example",
			},
		},
		{
			// Any one of the four SRV fields makes a different record.
			name: "SRV",
			entries: []Entry{
				{"name": "_sip._udp.example", "type": "SRV", "ttl": "1d", "srv-target": "sip.example", "srv-port": "5060", "srv-priority": "0", "srv-weight": "0"},
				{"name": "_sip._udp.example", "type": "SRV", "ttl": "1d", "srv-target": "sip.example", "srv-port": "5060", "srv-priority": "1", "srv-weight": "0"},
				{"name": "_sip._udp.example", "type": "SRV", "ttl": "1d", "srv-target": "sip.example", "srv-port": "5060", "srv-priority": "0", "srv-weight": "5"},
				{"name": "_sip._udp.example", "type": "SRV", "ttl": "1d", "srv-target": "sip.example", "srv-port": "5061", "srv-priority": "0", "srv-weight": "0"},
				{"name": "_sip._udp.example", "type": "SRV", "ttl": "1d", "srv-target": "sip2.example", "srv-port": "5060", "srv-priority": "0", "srv-weight": "0"},
			},
			want: []string{
				"_sip._udp.example|SRV|0|0|5060|sip.example",
				"_sip._udp.example|SRV|1|0|5060|sip.example",
				"_sip._udp.example|SRV|0|5|5060|sip.example",
				"_sip._udp.example|SRV|0|0|5061|sip.example",
				"_sip._udp.example|SRV|0|0|5060|sip2.example",
			},
		},
		{
			// TXT is case-sensitive on the device: "a" and "A" are two
			// records, so the text is not folded.
			name: "TXT",
			entries: []Entry{
				{"name": "h.example", "type": "TXT", "ttl": "1d", "text": "a"},
				{"name": "h.example", "type": "TXT", "ttl": "1d", "text": "A"},
			},
			want: []string{"h.example|TXT|a", "h.example|TXT|A"},
		},
		{
			name: "NS",
			entries: []Entry{
				{"name": "sub.example", "type": "NS", "ttl": "1d", "ns": "ns1.example"},
				{"name": "sub.example", "type": "NS", "ttl": "1d", "ns": "ns2.example"},
			},
			want: []string{"sub.example|NS|ns1.example", "sub.example|NS|ns2.example"},
		},
		{
			name: "FWD",
			entries: []Entry{
				{"name": "corp.example", "type": "FWD", "ttl": "1d", "forward-to": "10.0.0.53"},
				{"name": "corp.example", "type": "FWD", "ttl": "1d", "forward-to": "10.0.0.54"},
			},
			want: []string{"corp.example|FWD|10.0.0.53", "corp.example|FWD|10.0.0.54"},
		},
		{
			// NXDOMAIN has no value field: name and type are the whole key,
			// as they are on the device.
			name: "NXDOMAIN",
			entries: []Entry{
				{"name": "blocked.example", "type": "NXDOMAIN", "ttl": "1d"},
				{"name": "other.example", "type": "NXDOMAIN", "ttl": "1d"},
			},
			want: []string{"blocked.example|NXDOMAIN", "other.example|NXDOMAIN"},
		},
		{
			// A regexp record has regexp and no name key, and RouterOS
			// still reports type A. It keys on regexp + address, and does
			// not share an identity with a name record spelled the same.
			name: "regexp",
			entries: []Entry{
				{"regexp": `rx\.probe`, "type": "A", "ttl": "1d", "address": "192.0.2.1"},
				{"regexp": `rx\.probe`, "type": "A", "ttl": "1d", "address": "192.0.2.2"},
				{"name": `rx\.probe`, "type": "A", "ttl": "1d", "address": "192.0.2.1"},
			},
			want: []string{
				`regexp:rx\.probe|A|192.0.2.1`,
				`regexp:rx\.probe|A|192.0.2.2`,
				`rx\.probe|A|192.0.2.1`,
			},
		},
		{
			// A field of another type is not part of the key: RouterOS
			// drops it (a CNAME sent with an address), so a record that
			// carries one keys as if it did not. ttl, comment and
			// match-subdomain are not part of the device's key either.
			name: "only the type's own fields",
			entries: []Entry{
				{"name": "h.example", "type": "CNAME", "cname": "one.example", "address": "192.0.2.1", "ttl": "5m", "comment": "c", "match-subdomain": "true"},
			},
			want: []string{"h.example|CNAME|one.example"},
		},
		{
			// name is case-sensitive on the device.
			name: "name case",
			entries: []Entry{
				{"name": "svc.lab.example", "type": "A", "address": "192.0.2.1"},
				{"name": "SVC.lab.example", "type": "A", "address": "192.0.2.1"},
			},
			want: []string{"svc.lab.example|A|192.0.2.1", "SVC.lab.example|A|192.0.2.1"},
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
