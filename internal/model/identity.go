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
// recorded in §10.
//
// Comment first: an entry with a comment is identified by it. A comment
// repeated within the section is deduplicated in encounter order — the first
// "web" stays "web", later ones become "web#2", "web#3" — so two rules that
// share a comment can still be told apart (and targeted individually by the
// planner) instead of colliding on one key.
//
// Otherwise a per-section fallback applies:
//
//   - Firewall rules (filter/nat/mangle/raw) use an ordinal anchored to the
//     preceding commented rule in the same chain: "input@allow-ssh#2" is the
//     second uncommented input rule after the rule identified "allow-ssh".
//     Rules before any commented rule in their chain use "input#1",
//     "input#2", ... Anchoring means inserting a rule only re-identifies the
//     uncommented block that follows it up to the next commented rule, not
//     every later rule in the chain.
//   - Firewall address-list entries use "list|address" (they have no name).
//   - DNS static entries use their address.
//   - Routes use "dst-address->gateway". Only mtha:-tagged routes reach
//     this point at all (see Select), so in practice a route is identified
//     by its tag comment.
//   - Everything else uses name, then address, then a bare ordinal.
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
		if c, ok := stringField(e, "comment"); ok && c != "" {
			ids[i] = comments.identity(c)
			if IsFirewallSection(section) {
				chain := chainOf(e)
				anchor[chain] = ids[i]
				sinceAnchor[chain] = 0
			}
			continue
		}

		switch {
		case IsFirewallSection(section):
			chain := chainOf(e)
			sinceAnchor[chain]++
			if a, ok := anchor[chain]; ok {
				ids[i] = fmt.Sprintf("%s@%s#%d", chain, a, sinceAnchor[chain])
			} else {
				ids[i] = fmt.Sprintf("%s#%d", chain, sinceAnchor[chain])
			}

		case section == "ip/firewall/address-list":
			list, _ := stringField(e, "list")
			addr, _ := stringField(e, "address")
			if list != "" || addr != "" {
				ids[i] = list + "|" + addr
				continue
			}
			ids[i] = nextPlain()

		case section == "ip/dns/static":
			if addr, ok := stringField(e, "address"); ok && addr != "" {
				ids[i] = addr
				continue
			}
			ids[i] = nextPlain()

		case section == "ip/route":
			dst, _ := stringField(e, "dst-address")
			gw, _ := stringField(e, "gateway")
			ids[i] = dst + "->" + gw

		default:
			if name, ok := stringField(e, "name"); ok && name != "" {
				ids[i] = name
				continue
			}
			if addr, ok := stringField(e, "address"); ok && addr != "" {
				ids[i] = addr
				continue
			}
			ids[i] = nextPlain()
		}
	}

	return ids
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
