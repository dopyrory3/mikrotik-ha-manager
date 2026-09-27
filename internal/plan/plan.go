package plan

import (
	"fmt"
	"sort"
	"strings"

	"mtha/internal/diff"
	"mtha/internal/model"
)

// Direction is which router is the source of truth for a hunk (project.md
// §5.3: "Direction is explicit: A→B or B→A").
type Direction int

const (
	// AtoB copies router A's version of an entry onto router B.
	AtoB Direction = iota
	// BtoA copies router B's version of an entry onto router A.
	BtoA
)

func (d Direction) String() string {
	if d == BtoA {
		return "B→A"
	}
	return "A→B"
}

// Target is the router key ("a" or "b") this direction writes to.
func (d Direction) Target() string {
	if d == BtoA {
		return "a"
	}
	return "b"
}

// Source is the router key ("a" or "b") this direction reads from.
func (d Direction) Source() string {
	if d == BtoA {
		return "b"
	}
	return "a"
}

// HunkRef identifies one hunk within a section across re-reads: its
// identity plus its occurrence within that identity (see diff.Hunk).
type HunkRef struct {
	Identity   string
	Occurrence int
}

// RefOf returns the HunkRef for a diff hunk.
func RefOf(h diff.Hunk) HunkRef {
	return HunkRef{Identity: h.Identity, Occurrence: h.Occurrence}
}

func (r HunkRef) String() string {
	if r.Occurrence == 0 {
		return r.Identity
	}
	return fmt.Sprintf("%s (occurrence %d)", r.Identity, r.Occurrence+1)
}

// SectionInput is one section's fresh raw reads from both routers (as
// returned by routeros.Client.GetSection, still carrying ".id") plus the
// hunks the operator chose to sync and in which direction.
type SectionInput struct {
	Section string
	A, B    []model.Entry
	Choices map[HunkRef]Direction
}

// Options are plan-wide settings.
type Options struct {
	// Exempt is the pair's sync.exempt list, applied exactly as drift
	// detection applies it so the plan and the diff agree.
	Exempt []string
	// BackupName is the file name passed to /system/backup/save on each
	// target router. The caller stamps it (e.g. with the time) so repeated
	// applies don't overwrite each other's backups; empty means
	// DefaultBackupName.
	BackupName string
	// Users is the REST user mtha logs in to each router ("a", "b") as.
	// Build refuses to delete that user on that router, or change its group
	// or disabled, since the rest of the apply (and the next session) could
	// no longer connect.
	Users map[string]string
}

// DefaultBackupName is used when Options.BackupName is empty.
const DefaultBackupName = "mtha-pre-apply"

// Method is the HTTP verb an Op sends. RouterOS REST maps its console verbs
// onto HTTP as: add = PUT, set = PATCH, remove = DELETE, and every other
// command (backup, unset, move, ...) = POST.
type Method string

const (
	MethodCreate  Method = "PUT"
	MethodUpdate  Method = "PATCH"
	MethodDelete  Method = "DELETE"
	MethodCommand Method = "POST"
)

// Op is one REST write against one router.
type Op struct {
	// Router is the target router key, "a" or "b".
	Router string
	Method Method
	// Path is relative to /rest, with a leading slash, e.g.
	// "/ip/firewall/filter/*1A".
	Path string
	// Body is the JSON body; nil for DELETE.
	Body map[string]string
	// Section and Identity say which config entry this Op is for; both
	// are empty for the pre-apply backup.
	Section  string
	Identity string
	// Note is a short human explanation shown under the Op in the dry run.
	Note string
}

// Skip is a selected hunk that produced no operations, and why.
type Skip struct {
	Section   string
	Ref       HunkRef
	Direction Direction
	Reason    string
	// Router is set, instead of Direction, for a skip that isn't a synced
	// hunk: a Runtime object left alone on that router.
	Router string
}

// Where names what was skipped, for the dry run: "section ref [A→B]" for a
// sync hunk, "ref [router A]" for a Runtime object.
func (s Skip) Where() string {
	if s.Router != "" {
		return fmt.Sprintf("%s [router %s]", s.Ref, strings.ToUpper(s.Router))
	}
	return fmt.Sprintf("%s %s [%s]", s.Section, s.Ref, s.Direction)
}

// Plan is the ordered list of writes for one apply, grouped by target
// router ("a" first), each group starting with its backup.
type Plan struct {
	Ops     []Op
	Skipped []Skip
}

// Empty reports whether the plan has nothing to write.
func (p Plan) Empty() bool {
	return len(p.Ops) == 0
}

// Targets returns the router keys the plan writes to, in execution order.
func (p Plan) Targets() []string {
	var out []string
	seen := map[string]bool{}
	for _, op := range p.Ops {
		if !seen[op.Router] {
			seen[op.Router] = true
			out = append(out, op.Router)
		}
	}
	return out
}

// Sections returns the config sections the plan writes to, in first-seen
// order — the sections to re-diff for post-apply verification.
func (p Plan) Sections() []string {
	var out []string
	seen := map[string]bool{}
	for _, op := range p.Ops {
		if op.Section != "" && !seen[op.Section] {
			seen[op.Section] = true
			out = append(out, op.Section)
		}
	}
	return out
}

// patchOnlySections have a fixed set of entries built into RouterOS: they
// can be changed but entries can't be added or removed.
var patchOnlySections = map[string]bool{
	"ip/service": true,
}

// Build plans the selected hunks. See the package doc for ordering rules.
func Build(inputs []SectionInput, opts Options) Plan {
	backupName := opts.BackupName
	if backupName == "" {
		backupName = DefaultBackupName
	}

	var skipped []Skip
	byTarget := map[string][]Op{}

	for _, in := range inputs {
		if len(in.Choices) == 0 {
			continue
		}
		ops, skips := buildSection(in, opts)
		skipped = append(skipped, skips...)
		for _, op := range ops {
			byTarget[op.Router] = append(byTarget[op.Router], op)
		}
	}

	var p Plan
	for _, target := range []string{"a", "b"} {
		ops := byTarget[target]
		if len(ops) == 0 {
			continue
		}
		p.Ops = append(p.Ops, BackupOp(target, backupName))
		p.Ops = append(p.Ops, ops...)
	}
	p.Skipped = skipped
	return p
}

// BackupOp is the pre-apply /system/backup/save that starts every router's
// writes in a plan (project.md §5.4); empty backupName means
// DefaultBackupName.
func BackupOp(router, backupName string) Op {
	if backupName == "" {
		backupName = DefaultBackupName
	}
	return Op{
		Router: router,
		Method: MethodCommand,
		Path:   "/system/backup/save",
		Body:   map[string]string{"name": backupName},
		Note:   "pre-apply backup of router " + router,
	}
}

// row is one selected entry on one router: its raw form (for ".id") and its
// normalised form (for bodies), matched by identity and occurrence.
type row struct {
	ref  HunkRef
	raw  model.Entry
	norm model.Entry
}

// side indexes one router's entries for a section.
type side struct {
	rows  []row
	byRef map[HunkRef]int
}

// index mirrors diff.Compare's matching exactly — model.Select and
// model.Normalize agree on which entries survive and in what order, and
// BuildIdentities runs on the normalised form — so a HunkRef from the diff
// resolves to the same entry here, now with its ".id" available.
func index(section string, raw []model.Entry, exempt []string) side {
	kept := model.Select(section, raw)
	norm := model.Normalize(section, raw, exempt)
	ids := model.BuildIdentities(section, norm)

	s := side{rows: make([]row, len(kept)), byRef: make(map[HunkRef]int, len(kept))}
	seen := map[string]int{}
	for i := range kept {
		ref := HunkRef{Identity: ids[i], Occurrence: seen[ids[i]]}
		seen[ids[i]]++
		s.rows[i] = row{ref: ref, raw: kept[i], norm: norm[i]}
		s.byRef[ref] = i
	}
	return s
}

func (s side) get(ref HunkRef) (row, int, bool) {
	i, ok := s.byRef[ref]
	if !ok {
		return row{}, -1, false
	}
	return s.rows[i], i, true
}

// selected is one chosen hunk that still differs on a fresh read.
type selected struct {
	hunk diff.Hunk
	dir  Direction
}

func buildSection(in SectionInput, opts Options) ([]Op, []Skip) {
	exempt := opts.Exempt
	sd := diff.Compare(in.Section, in.A, in.B, exempt)
	sides := map[string]side{
		"a": index(in.Section, in.A, exempt),
		"b": index(in.Section, in.B, exempt),
	}

	var chosen []selected
	stillDiffers := map[HunkRef]bool{}
	for _, h := range sd.Hunks {
		ref := RefOf(h)
		stillDiffers[ref] = true
		if dir, ok := in.Choices[ref]; ok {
			chosen = append(chosen, selected{hunk: h, dir: dir})
		}
	}

	var skips []Skip
	for _, ref := range sortedRefs(in.Choices) {
		if !stillDiffers[ref] {
			skips = append(skips, Skip{
				Section: in.Section, Ref: ref, Direction: in.Choices[ref],
				Reason: "no longer differs (resolved, or changed since it was selected)",
			})
		}
	}

	var deletes, updates []Op
	type pendingCreate struct {
		srcIndex int
		op       Op
	}
	var creates []pendingCreate

	for _, c := range chosen {
		ref := RefOf(c.hunk)
		src, tgt := sides[c.dir.Source()], sides[c.dir.Target()]
		target := c.dir.Target()
		skip := func(reason string) {
			skips = append(skips, Skip{Section: in.Section, Ref: ref, Direction: c.dir, Reason: reason})
		}

		switch {
		case onSide(c.hunk, c.dir.Source()) && !onSide(c.hunk, target):
			// Present on the source only: create it on the target.
			if patchOnlySections[in.Section] {
				skip("entries in this section are built in; they can be changed but not added")
				continue
			}
			if in.Section == "user" {
				skip("user passwords are not readable over REST; creating this user would leave it with no password")
				continue
			}
			srcRow, srcIdx, _ := src.get(ref)
			body := createBody(srcRow.norm)
			note := fmt.Sprintf("create %s (from router %s)", ref, c.dir.Source())
			if model.IsFirewallSection(in.Section) {
				if anchor, ok := placeBefore(src, srcIdx, tgt); ok {
					body["place-before"] = idOf(anchor.raw)
					note += ", placed before " + anchor.ref.String()
				} else {
					note += ", appended at end of chain"
				}
			}
			creates = append(creates, pendingCreate{srcIndex: srcIdx, op: Op{
				Router: target, Method: MethodCreate, Path: "/" + in.Section, Body: body,
				Section: in.Section, Identity: ref.Identity, Note: note,
			}})

		case !onSide(c.hunk, c.dir.Source()) && onSide(c.hunk, target):
			// Present on the target only: remove it.
			if patchOnlySections[in.Section] {
				skip("entries in this section are built in; they can be changed but not removed")
				continue
			}
			tgtRow, _, _ := tgt.get(ref)
			if in.Section == "user" && isAPIUser(tgtRow, target, opts.Users) {
				skip(fmt.Sprintf("mtha logs in to router %s as this user; removing it would lock mtha out", target))
				continue
			}
			deletes = append(deletes, Op{
				Router: target, Method: MethodDelete, Path: "/" + in.Section + "/" + idOf(tgtRow.raw),
				Section: in.Section, Identity: ref.Identity,
				Note: fmt.Sprintf("remove %s (not on router %s)", ref, c.dir.Source()),
			})

		default:
			// On both, with field differences: make the target match.
			srcRow, _, _ := src.get(ref)
			tgtRow, _, _ := tgt.get(ref)
			if reason := lockoutReason(in.Section, target, tgtRow, c.hunk.Changes, opts.Users); reason != "" {
				skip(reason)
				continue
			}
			updates = append(updates, updateOps(in.Section, target, ref, c.hunk.Changes, srcRow, tgtRow, c.dir)...)
		}
	}

	sort.SliceStable(creates, func(i, j int) bool { return creates[i].srcIndex < creates[j].srcIndex })

	ops := append(deletes, updates...)
	for _, c := range creates {
		ops = append(ops, c.op)
	}
	return ops, skips
}

// lockoutReason is why updating the target's entry with changes could cut
// mtha off from that router mid-apply, or "" if it can't: moving, disabling
// or narrowing the address list of the REST API service (www-ssl), or
// changing the group or disabled flag of the user mtha logs in as.
func lockoutReason(section, target string, tgt row, changes []model.FieldChange, users map[string]string) string {
	var guarded map[string]bool
	var reason string
	switch {
	case section == "ip/service" && stringOf(tgt.raw["name"]) == "www-ssl":
		guarded = map[string]bool{"port": true, "disabled": true, "address": true}
		reason = fmt.Sprintf("changes %%s of router %s's REST API service; mtha could lose its connection mid-apply", target)
	case section == "user" && isAPIUser(tgt, target, users):
		guarded = map[string]bool{"group": true, "disabled": true}
		reason = fmt.Sprintf("changes %%s of the user mtha logs in to router %s as; that could lock mtha out", target)
	default:
		return ""
	}
	var fields []string
	for _, ch := range changes {
		if guarded[ch.Field] {
			fields = append(fields, ch.Field)
		}
	}
	if len(fields) == 0 {
		return ""
	}
	return fmt.Sprintf(reason, strings.Join(fields, ", "))
}

// isAPIUser reports whether tgt is the user mtha logs in to target as.
func isAPIUser(tgt row, target string, users map[string]string) bool {
	name := stringOf(tgt.raw["name"])
	return name != "" && name == users[target]
}

// updateOps makes the target entry match the source: one PATCH setting every
// field the source has a value for, then one unset per field the source
// leaves at default (absent or empty), since PATCHing "" isn't valid for
// every RouterOS property but unset is.
func updateOps(section, target string, ref HunkRef, changes []model.FieldChange, src, tgt row, dir Direction) []Op {
	id := idOf(tgt.raw)
	set := map[string]string{}
	var unset []string
	for _, ch := range changes {
		v, present := src.norm[ch.Field]
		if s := stringOf(v); present && s != "" {
			set[ch.Field] = s
		} else {
			unset = append(unset, ch.Field)
		}
	}

	var ops []Op
	if len(set) > 0 {
		ops = append(ops, Op{
			Router: target, Method: MethodUpdate, Path: "/" + section + "/" + id, Body: set,
			Section: section, Identity: ref.Identity,
			Note: fmt.Sprintf("update %s to match router %s", ref, dir.Source()),
		})
	}
	for _, field := range unset {
		ops = append(ops, Op{
			Router: target, Method: MethodCommand, Path: "/" + section + "/unset",
			Body:    map[string]string{"numbers": id, "value-name": field},
			Section: section, Identity: ref.Identity,
			Note: fmt.Sprintf("unset %s on %s (default on router %s)", field, ref, dir.Source()),
		})
	}
	return ops
}

// placeBefore finds where a rule created from src.rows[srcIdx] belongs on
// the target: before the next rule in the same chain on the source that
// already exists on the target. (Such a rule is on both routers, so this
// plan never deletes it; it may be patched, which keeps its ".id".) Only
// the same chain matters — RouterOS evaluates each chain in list order, and
// the interleaving of different chains in the list is cosmetic. No anchor
// means the rule is the last of its chain, so appending is correct.
func placeBefore(src side, srcIdx int, tgt side) (row, bool) {
	chain := stringOf(src.rows[srcIdx].norm["chain"])
	for _, r := range src.rows[srcIdx+1:] {
		if stringOf(r.norm["chain"]) != chain {
			continue
		}
		if t, _, ok := tgt.get(r.ref); ok {
			return t, true
		}
	}
	return row{}, false
}

// createBody is the source entry's normalised fields — already stripped of
// ".id", read-only state and exempt (per-router) fields — minus empty
// values, which RouterOS treats as default on create anyway and rejects for
// some typed properties.
func createBody(norm model.Entry) map[string]string {
	body := make(map[string]string, len(norm))
	for k, v := range norm {
		if s := stringOf(v); s != "" {
			body[k] = s
		}
	}
	return body
}

func onSide(h diff.Hunk, router string) bool {
	if router == "a" {
		return h.OnA
	}
	return h.OnB
}

func idOf(e model.Entry) string {
	return stringOf(e[".id"])
}

func stringOf(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

func sortedRefs(m map[HunkRef]Direction) []HunkRef {
	refs := make([]HunkRef, 0, len(m))
	for r := range m {
		refs = append(refs, r)
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].Identity != refs[j].Identity {
			return refs[i].Identity < refs[j].Identity
		}
		return refs[i].Occurrence < refs[j].Occurrence
	})
	return refs
}
