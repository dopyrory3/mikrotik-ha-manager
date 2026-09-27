package plan

import (
	"fmt"
	"net"
	"strings"

	"mtha/internal/model"
)

// The reference check (docs/design-questions.md §3, option B). Some fields
// name an object in another section: a lease's server, a DHCP server's
// address pool, a scheduler's script. Build does not reorder across
// sections to satisfy them (§10.1's per-section plan order stands), and
// many referents — ip/pool, user/group, interfaces — are never synced, so
// no ordering could. Instead every create or PATCH body that sets such a
// field is checked against the target router: the referent must be there
// already and not removed earlier in the plan, or be created earlier in it.
// If not, the dry run carries a Warning. It is a warning rather than a
// Skip because RouterOS accepts at least some dangling references (a
// firewall rule naming an empty address list, a scheduler naming a script
// that doesn't exist yet), leaving the object inert until the referent
// appears; which references it rejects outright is not yet surveyed.

// Warning is a planned write that names an object the target router may
// not have when the write runs.
type Warning struct {
	Router   string
	Section  string
	Identity string
	Message  string
}

func (w Warning) String() string {
	return fmt.Sprintf("%s %s [router %s]: %s", w.Section, w.Identity, strings.ToUpper(w.Router), w.Message)
}

// Read is one section read from one router.
type Read struct {
	Router  string
	Section string
}

// reference is one field whose value names an object elsewhere.
type reference struct {
	section  string // the referrer
	field    string
	referent string // the section the name must exist in
	key      string // the referent's field holding the name
	// names extracts the referenced names from the field's value; nil
	// means the value is one name.
	names func(string) []string
	// builtin are values that name nothing (or a built-in object).
	builtin []string
}

// references is the static table from docs/design-questions.md §3. Left
// out: a firewall rule's jump-target (a chain in the same section, whose
// rules are usually created after the jump into them, so it would warn on
// every new custom chain).
var references = func() []reference {
	refs := []reference{
		{section: "ip/dhcp-server", field: "address-pool", referent: "ip/pool", key: "name", builtin: []string{"static-only"}},
		{section: "ip/dhcp-server", field: "interface", referent: "interface", key: "name"},
		{section: "ip/dhcp-server/lease", field: "server", referent: "ip/dhcp-server", key: "name", builtin: []string{"all"}},
		{section: "ip/route", field: "gateway", referent: "interface", key: "name", names: gatewayInterfaces},
		{section: "ip/route", field: "routing-table", referent: "routing/table", key: "name", builtin: []string{"main"}},
		{section: "user", field: "group", referent: "user/group", key: "name"},
		{section: "system/scheduler", field: "on-event", referent: "system/script", key: "name", names: scriptName},
		{section: "ip/service", field: "certificate", referent: "certificate", key: "name", builtin: []string{"none"}},
	}
	for _, fw := range []string{"ip/firewall/filter", "ip/firewall/nat", "ip/firewall/mangle", "ip/firewall/raw"} {
		refs = append(refs,
			reference{section: fw, field: "src-address-list", referent: "ip/firewall/address-list", key: "list", names: negatable},
			reference{section: fw, field: "dst-address-list", referent: "ip/firewall/address-list", key: "list", names: negatable},
			reference{section: fw, field: "in-interface", referent: "interface", key: "name", names: negatable},
			reference{section: fw, field: "out-interface", referent: "interface", key: "name", names: negatable},
		)
	}
	return refs
}()

// referentSections are the sections some reference points into.
var referentSections = func() map[string]string {
	out := map[string]string{}
	for _, r := range references {
		out[r.referent] = r.key
	}
	return out
}()

// namesIn returns the names r's field value refers to.
func (r reference) namesIn(value string) []string {
	var names []string
	if r.names != nil {
		names = r.names(value)
	} else {
		names = []string{value}
	}
	out := names[:0]
	for _, n := range names {
		if n != "" && !contains(r.builtin, n) {
			out = append(out, n)
		}
	}
	return out
}

// negatable strips a firewall matcher's "!" negation: "!blocklist" still
// names the list.
func negatable(v string) []string {
	return []string{strings.TrimPrefix(v, "!")}
}

// gatewayInterfaces returns the interfaces a route's gateway names: a bare
// interface name, or the part after "%" in "10.0.0.1%ether1". Gateway
// addresses (optionally "@table"-scoped) name no object.
func gatewayInterfaces(v string) []string {
	var out []string
	for _, gw := range strings.Split(v, ",") {
		gw = strings.TrimSpace(gw)
		if i := strings.LastIndex(gw, "%"); i >= 0 {
			out = append(out, gw[i+1:])
			continue
		}
		host := gw
		if i := strings.Index(host, "@"); i >= 0 {
			host = host[:i]
		}
		if net.ParseIP(host) == nil {
			out = append(out, gw)
		}
	}
	return out
}

// scriptName returns on-event's value when it is a bare word, which
// RouterOS runs as the script of that name; anything else is inline source
// and names nothing.
func scriptName(v string) []string {
	if v == "" || strings.ContainsAny(v, " \t\n/;:[]{}()$\"=") {
		return nil
	}
	return []string{v}
}

// ReferenceReads lists the reads Build's reference check needs beyond the
// inputs: for each planned body naming an object in a section the inputs
// don't carry, that section on the router written to. They are read-only
// GETs; the caller makes them and passes them back in Options.Referents.
func ReferenceReads(inputs []SectionInput, opts Options) []Read {
	opts.Referents = nil
	have := inputSections(inputs)
	var out []Read
	seen := map[Read]bool{}
	for _, op := range Build(inputs, opts).Ops {
		for _, u := range usesOf(op) {
			rd := Read{Router: op.Router, Section: u.ref.referent}
			if !have[rd.Section] && !seen[rd] {
				seen[rd] = true
				out = append(out, rd)
			}
		}
	}
	return out
}

func inputSections(inputs []SectionInput) map[string]bool {
	have := map[string]bool{}
	for _, in := range inputs {
		have[in.Section] = true
	}
	return have
}

// use is one name a body refers to.
type use struct {
	ref  reference
	name string
}

// usesOf lists the names op's body refers to: only creates and PATCHes set
// referring fields.
func usesOf(op Op) []use {
	if op.Method != MethodCreate && op.Method != MethodUpdate {
		return nil
	}
	var out []use
	for _, r := range references {
		if r.section != op.Section {
			continue
		}
		v, ok := op.Body[r.field]
		if !ok {
			continue
		}
		for _, n := range r.namesIn(v) {
			out = append(out, use{ref: r, name: n})
		}
	}
	return out
}

// referentState counts, per router and referent section, the objects of
// each name the target has at this point in the plan. A count rather than
// a set: many address-list entries share one list name.
type referentState struct {
	inputs    []SectionInput
	extra     map[Read][]model.Entry
	count     map[Read]map[string]int
	byID      map[Read]map[string]string // ".id" to name, for DELETE/PATCH
	loaded    map[Read]bool
	removed   map[Read]map[string]bool
	createdAt map[Read]map[string]int // op index of the first create of a name
}

// load reads rd's entries from the inputs or the extra reads, once; false
// if neither has them.
func (s *referentState) load(rd Read) bool {
	if done, ok := s.loaded[rd]; ok {
		return done
	}
	key := referentSections[rd.Section]
	entries, ok := s.extra[rd]
	for _, in := range s.inputs {
		if in.Section == rd.Section {
			entries, ok = in.A, true
			if rd.Router == "b" {
				entries = in.B
			}
		}
	}
	s.loaded[rd] = ok
	if !ok {
		return false
	}
	s.count[rd] = map[string]int{}
	s.byID[rd] = map[string]string{}
	for _, e := range entries {
		name := stringOf(e[key])
		s.count[rd][name]++
		s.byID[rd][idOf(e)] = name
	}
	return true
}

// checkReferences walks ops in execution order and warns about every name
// a body refers to that the target lacks at that point.
func checkReferences(ops []Op, inputs []SectionInput, extra map[Read][]model.Entry) []Warning {
	s := &referentState{
		inputs: inputs, extra: extra,
		count: map[Read]map[string]int{}, byID: map[Read]map[string]string{},
		loaded: map[Read]bool{}, removed: map[Read]map[string]bool{}, createdAt: map[Read]map[string]int{},
	}
	for i, op := range ops {
		key, ok := referentSections[op.Section]
		if !ok || op.Method != MethodCreate {
			continue
		}
		rd := Read{Router: op.Router, Section: op.Section}
		if s.createdAt[rd] == nil {
			s.createdAt[rd] = map[string]int{}
		}
		if _, dup := s.createdAt[rd][op.Body[key]]; !dup {
			s.createdAt[rd][op.Body[key]] = i
		}
	}

	var warnings []Warning
	for i, op := range ops {
		for _, u := range usesOf(op) {
			if msg := s.missing(i, op.Router, u); msg != "" {
				warnings = append(warnings, Warning{Router: op.Router, Section: op.Section, Identity: op.Identity, Message: msg})
			}
		}
		s.apply(op)
	}
	return warnings
}

// missing is why u's referent may not exist when ops[i] runs, or "".
func (s *referentState) missing(i int, router string, u use) string {
	rd := Read{Router: router, Section: u.ref.referent}
	what := fmt.Sprintf("%s=%s names %s %s %q", u.ref.field, u.name, u.ref.referent, u.ref.key, u.name)
	if !s.load(rd) {
		return fmt.Sprintf("%s, which was not read, so it could not be checked", what)
	}
	if s.count[rd][u.name] > 0 {
		return ""
	}
	switch at, later := s.createdAt[rd][u.name]; {
	case later && at > i:
		return fmt.Sprintf("%s, which this plan only creates later (op %d)", what, at+1)
	case s.removed[rd][u.name]:
		return fmt.Sprintf("%s, which this plan removes first", what)
	}
	return fmt.Sprintf("%s, which router %s does not have and this plan does not create first", what, strings.ToUpper(router))
}

// apply records what op does to the names in a referent section.
func (s *referentState) apply(op Op) {
	key, ok := referentSections[op.Section]
	if !ok {
		return
	}
	rd := Read{Router: op.Router, Section: op.Section}
	if !s.load(rd) {
		return
	}
	id := op.Path[strings.LastIndex(op.Path, "/")+1:]
	switch op.Method {
	case MethodCreate:
		s.count[rd][op.Body[key]]++
	case MethodDelete:
		s.drop(rd, s.byID[rd][id])
	case MethodUpdate:
		if name, renamed := op.Body[key]; renamed {
			s.drop(rd, s.byID[rd][id])
			s.count[rd][name]++
			s.byID[rd][id] = name
		}
	}
}

func (s *referentState) drop(rd Read, name string) {
	if s.count[rd][name] > 0 {
		s.count[rd][name]--
	}
	if s.count[rd][name] == 0 {
		if s.removed[rd] == nil {
			s.removed[rd] = map[string]bool{}
		}
		s.removed[rd][name] = true
	}
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
