package model

import "fmt"

// firewallSections are the ordered, chain-structured sections whose
// uncommented rules fall back to an anchored chain ordinal, and whose rule
// order the planner preserves with place-before (project.md §5.4).
var firewallSections = map[string]bool{
	"ip/firewall/filter": true,
	"ip/firewall/nat":    true,
	"ip/firewall/mangle": true,
	"ip/firewall/raw":    true,
}

// IsFirewallSection reports whether section is one of the ordered firewall
// rule lists (filter, nat, mangle, raw), where evaluation order is part of
// the config and must be preserved on apply.
func IsFirewallSection(section string) bool {
	return firewallSections[section]
}

// BuildIdentities computes the stable per-entry identity used to match rows
// between two routers, per project.md §5.3 and the identity decisions
// recorded in §10.1, using the per-section table from
// docs/design-questions.md §1 (Recommendation B): a section with a natural
// key is identified by it, and its comment is an ordinary compared field, so
// a comment edit is a PATCH rather than a delete + create.
//
//   - Firewall rules (filter/nat/mangle/raw) have no natural key, so they
//     stay comment first. A comment repeated within the section is
//     deduplicated in encounter order — the first "web" stays "web", later
//     ones become "web#2", "web#3" — so two rules that share a comment can
//     still be told apart. An uncommented rule uses an ordinal anchored to
//     the preceding commented rule in the same chain: "input@allow-ssh#2" is
//     the second uncommented input rule after the rule identified
//     "allow-ssh". Rules before any commented rule in their chain use
//     "input#1", "input#2", ... Anchoring means inserting a rule only
//     re-identifies the uncommented block that follows it up to the next
//     commented rule, not every later rule in the chain.
//   - Routes stay comment first, else "dst-address->gateway". Only
//     mtha:-tagged routes reach this point at all (see Select), so in
//     practice a route is identified by its tag comment.
//   - ip/firewall/address-list uses "list|address".
//   - ip/dhcp-server, ip/service, user, system/script and system/scheduler
//     use name.
//   - ip/dhcp-server/network uses address.
//   - ip/dhcp-server/lease uses "server|mac-address", or address when the
//     lease has no mac-address.
//   - ip/dns/static uses "name|type|value", with regexp in place of name
//     for a regexp record (see dnsStaticIdentity).
//   - Any other section uses name, then address, then comment (deduplicated
//     as above), then a bare ordinal.
//
// A natural key is not guaranteed unique on a router. Entries that share an
// identity are paired by occurrence in diff.Compare, so a collision never
// drops an entry; it is not resolved here.
func BuildIdentities(section string, entries []Entry) []string {
	ids := make([]string, len(entries))
	comments := newCommentDeduper()

	// Per chain: identity of the most recent commented rule, and the count
	// of uncommented rules seen since it (or since the chain's start).
	anchor := map[string]string{}
	sinceAnchor := map[string]int{}

	plainOrdinal := 0
	nextPlain := func() string {
		plainOrdinal++
		return fmt.Sprintf("#%d", plainOrdinal)
	}

	for i, e := range entries {
		comment, _ := stringField(e, "comment")

		switch {
		case IsFirewallSection(section):
			chain := chainOf(e)
			if comment != "" {
				ids[i] = comments.identity(comment)
				anchor[chain] = ids[i]
				sinceAnchor[chain] = 0
				continue
			}
			sinceAnchor[chain]++
			if a, ok := anchor[chain]; ok {
				ids[i] = fmt.Sprintf("%s@%s#%d", chain, a, sinceAnchor[chain])
			} else {
				ids[i] = fmt.Sprintf("%s#%d", chain, sinceAnchor[chain])
			}

		case section == "ip/route":
			if comment != "" {
				ids[i] = comments.identity(comment)
				continue
			}
			dst, _ := stringField(e, "dst-address")
			gw, _ := stringField(e, "gateway")
			ids[i] = dst + "->" + gw

		case section == "ip/firewall/address-list":
			list, _ := stringField(e, "list")
			addr, _ := stringField(e, "address")
			ids[i] = list + "|" + addr

		case nameKeyedSections[section]:
			ids[i], _ = stringField(e, "name")

		case section == "ip/dhcp-server/network":
			// Changing a network's address is a delete + create, not a
			// patch. That is harmless: a network has nothing else to it.
			ids[i], _ = stringField(e, "address")

		case section == "ip/dhcp-server/lease":
			// Keyed on the client the reservation is for, so re-addressing
			// it is a patch. Renaming the server re-identifies its leases,
			// consistent with the server itself being keyed on its name.
			server, _ := stringField(e, "server")
			if mac, ok := stringField(e, "mac-address"); ok && mac != "" {
				ids[i] = server + "|" + mac
				continue
			}
			ids[i], _ = stringField(e, "address")

		case section == "ip/dns/static":
			ids[i] = dnsStaticIdentity(e)

		default:
			if name, ok := stringField(e, "name"); ok && name != "" {
				ids[i] = name
				continue
			}
			if addr, ok := stringField(e, "address"); ok && addr != "" {
				ids[i] = addr
				continue
			}
			if comment != "" {
				ids[i] = comments.identity(comment)
				continue
			}
			ids[i] = nextPlain()
		}
	}

	return ids
}

// nameKeyedSections are the sections identified by name alone: RouterOS
// looks their entries up by name, and the name is what an operator means by
// "this object".
var nameKeyedSections = map[string]bool{
	"ip/dhcp-server":   true,
	"ip/service":       true,
	"user":             true,
	"system/script":    true,
	"system/scheduler": true,
}

// dnsValueFields maps a DNS static record type to the fields holding its
// value, in the order they are joined into the identity. The shapes are the
// device's own (docs/lab-rest-contract.md, write probe 1): RouterOS returns
// only the record type's value fields, and refuses a second enabled record
// with the same name, type and every one of these fields (write probe 2), so
// together they are the record's natural key.
//
// Where a type has several fields, the free-text one goes last so the joined
// value stays unambiguous: MX preference and SRV priority/weight/port are
// numbers, the exchange and target are names. MX and SRV numbers are always
// returned (RouterOS fills in "0"), so a record created without them still
// keys the same on both routers.
//
// NXDOMAIN has no value field: its key is the name and type alone, which is
// also what RouterOS enforces, so "name|NXDOMAIN" is the full key rather
// than the unknown-type fallback.
var dnsValueFields = map[string][]string{
	"A":        {"address"},
	"AAAA":     {"address"},
	"CNAME":    {"cname"},
	"MX":       {"mx-preference", "mx-exchange"},
	"SRV":      {"srv-priority", "srv-weight", "srv-port", "srv-target"},
	"TXT":      {"text"},
	"NS":       {"ns"},
	"FWD":      {"forward-to"},
	"NXDOMAIN": {},
}

// dnsRegexpPrefix marks the identity of a regexp record. A regexp record
// has regexp in place of name (RouterOS refuses both), and "a.b" as a
// regexp is a different record from the name "a.b", so the two must not
// share an identity.
const dnsRegexpPrefix = "regexp:"

// dnsStaticIdentity identifies a DNS static record as "name|type|value",
// where value is the type's value fields joined with "|" (see
// dnsValueFields). One name can carry several records (two A records for
// one name is two entries), and the value is what tells them apart, so the
// value is part of the identity: re-addressing a record is a delete + create
// rather than a patch, by design. Name and value are compared exactly:
// RouterOS treats both name and TXT text as case-sensitive.
//
// A regexp record is "regexp:<regexp>|type|value". RouterOS always reports
// type ("A" by default, also for a regexp record); an entry without one is
// treated as A. A type not in dnsValueFields is identified "name|type" and
// relies on diff.Compare's occurrence pairing.
func dnsStaticIdentity(e Entry) string {
	name, _ := stringField(e, "name")
	if re, ok := stringField(e, "regexp"); ok && re != "" && name == "" {
		name = dnsRegexpPrefix + re
	}
	typ, _ := stringField(e, "type")
	if typ == "" {
		typ = "A"
	}
	fields, ok := dnsValueFields[typ]
	if !ok {
		return name + "|" + typ
	}
	id := name + "|" + typ
	for _, f := range fields {
		v, _ := stringField(e, f)
		id += "|" + v
	}
	return id
}

func chainOf(e Entry) string {
	chain, _ := stringField(e, "chain")
	if chain == "" {
		return "_"
	}
	return chain
}

// commentDeduper hands out unique identities for comments within one
// section: the first use of a comment is the comment itself, later uses get
// a "#<n>" suffix. A suffix is bumped past any identity already handed out,
// so a literal comment "web#2" and a deduplicated second "web" can't collide.
type commentDeduper struct {
	count map[string]int
	used  map[string]bool
}

func newCommentDeduper() *commentDeduper {
	return &commentDeduper{count: map[string]int{}, used: map[string]bool{}}
}

func (d *commentDeduper) identity(comment string) string {
	d.count[comment]++
	id := comment
	if d.count[comment] > 1 {
		id = fmt.Sprintf("%s#%d", comment, d.count[comment])
	}
	for d.used[id] {
		d.count[comment]++
		id = fmt.Sprintf("%s#%d", comment, d.count[comment])
	}
	d.used[id] = true
	return id
}
