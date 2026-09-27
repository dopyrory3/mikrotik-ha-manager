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

func TestBuildIdentitiesDNSStaticUsesAddress(t *testing.T) {
	entries := []Entry{
		{"name": "host1", "address": "10.0.0.5"},
	}
	got := BuildIdentities("ip/dns/static", entries)
	want := []string{"10.0.0.5"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestBuildIdentitiesDefaultUsesName(t *testing.T) {
	entries := []Entry{
		{"name": "backup-config"},
	}
	got := BuildIdentities("system/scheduler", entries)
	want := []string{"backup-config"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestBuildIdentitiesAddressListUsesListAndAddress(t *testing.T) {
	entries := []Entry{
		{"list": "blocklist", "address": "10.0.0.5"},
		{"list": "vpn-allowed", "address": "10.0.0.5"},
	}
	got := BuildIdentities("ip/firewall/address-list", entries)
	want := []string{"blocklist|10.0.0.5", "vpn-allowed|10.0.0.5"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestBuildIdentitiesRoute(t *testing.T) {
	entries := []Entry{
		{"dst-address": "0.0.0.0/0", "gateway": "10.0.0.1"},
	}
	got := BuildIdentities("ip/route", entries)
	want := []string{"0.0.0.0/0->10.0.0.1"}
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
		{"name": "a", "comment": "web"},
		{"name": "b", "comment": "web"},
		{"name": "c", "comment": "web#2"},
		{"name": "d", "comment": "web"},
	}
	got := BuildIdentities("system/script", entries)
	want := []string{"web", "web#2", "web#2#2", "web#3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestBuildIdentitiesDedupeAppliesToEverySection(t *testing.T) {
	entries := []Entry{
		{"name": "one", "comment": "shared"},
		{"name": "two", "comment": "shared"},
	}
	got := BuildIdentities("system/scheduler", entries)
	want := []string{"shared", "shared#2"}
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

func TestBuildIdentitiesRoutePrefersTagComment(t *testing.T) {
	entries := []Entry{
		{"dst-address": "10.9.0.0/16", "gateway": "10.0.0.9", "comment": "mtha:vpn"},
	}
	got := BuildIdentities("ip/route", entries)
	want := []string{"mtha:vpn"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// Current behaviour, recorded for docs/design-questions.md (question 1): a
// DNS record with no address (a CNAME) falls to the bare section ordinal, so
// inserting another address-less record ahead of it re-identifies it.
func TestBuildIdentitiesDNSStaticCNAMEIsPositional(t *testing.T) {
	before := []Entry{
		{"name": "svc.example", "type": "A", "address": "192.0.2.10"},
		{"name": "www.example", "type": "CNAME", "cname": "svc.example"},
	}
	after := []Entry{
		{"name": "mail.example", "type": "CNAME", "cname": "svc.example"},
		{"name": "svc.example", "type": "A", "address": "192.0.2.10"},
		{"name": "www.example", "type": "CNAME", "cname": "svc.example"},
	}
	if got, want := BuildIdentities("ip/dns/static", before), []string{"192.0.2.10", "#1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("before: got %v, want %v", got, want)
	}
	if got, want := BuildIdentities("ip/dns/static", after), []string{"#1", "192.0.2.10", "#2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("after: got %v, want %v", got, want)
	}
}

// Current behaviour, recorded for docs/design-questions.md (question 1):
// comment wins over a section's natural key, so entries sharing a comment
// are told apart only by encounter order. Inserting one more entry with that
// comment at the front re-identifies every later one, even though list and
// address would have identified each of them uniquely.
func TestBuildIdentitiesSharedCommentOverridesNaturalKey(t *testing.T) {
	before := []Entry{
		{"list": "blocked", "address": "198.51.100.1", "comment": "feed"},
		{"list": "blocked", "address": "198.51.100.2", "comment": "feed"},
	}
	after := []Entry{
		{"list": "blocked", "address": "198.51.100.9", "comment": "feed"},
		{"list": "blocked", "address": "198.51.100.1", "comment": "feed"},
		{"list": "blocked", "address": "198.51.100.2", "comment": "feed"},
	}
	if got, want := BuildIdentities("ip/firewall/address-list", before), []string{"feed", "feed#2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("before: got %v, want %v", got, want)
	}
	if got, want := BuildIdentities("ip/firewall/address-list", after), []string{"feed", "feed#2", "feed#3"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("after: got %v, want %v", got, want)
	}
}
