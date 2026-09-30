// Package plan is the dry-run planner and executor behind the Apply screen
// (project.md §5.4, §7.2, milestone 3 in §9). It is the single write path:
// every write mtha makes — config sync, and Runtime deploy/remove (whose
// Ops internal/runtime builds) — is an Op in a Plan, and every Plan is shown
// to the operator before it runs (§7.3).
//
// Build turns the operator's selected diff hunks, each with an explicit
// direction (A→B or B→A, §5.3), into an ordered list of REST operations
// against the target router(s):
//
//   - a pre-apply backup (POST /system/backup/save) first on every router
//     the plan writes to;
//   - then, per section in the order given: deletes, then updates (PATCH for
//     changed fields, including "" or "false" for a field the source leaves
//     at its default; POST …/unset for a firewall matcher it leaves unset), then
//     moves (POST …/move, reordering a firewall chain), then creates (PUT,
//     RouterOS REST's "add") in source order;
//   - creates into firewall rule lists carry place-before, anchored to the
//     next rule in the same chain on the source that already exists on the
//     target, so the rule lands in the same relative position (§5.4).
//
// Build re-diffs fresh reads rather than trusting the hunks as selected, so
// a hunk that has since been resolved or changed is reported in
// Plan.Skipped instead of producing a stale write. Hunks that can't be
// synced safely (creating users, whose passwords REST can't read; adding or
// removing entries in fixed-set sections like ip/service; changes that could
// lock mtha out of the target, such as moving its www-ssl service or
// removing the user it logs in as) are skipped with a reason too.
//
// A selected order finding (a firewall chain whose shared rules are in a
// different order, docs/design-questions.md §2) plans RouterOS moves on the
// target, never a delete and re-create: each moved rule goes before the
// next rule, in source order, that is already in order on both routers, by
// the ".id"s read from the target (a move keeps the moved rule's ".id").
// RouterOS silently moves a rule to the end of the table when the
// destination is unknown, so every move names one, every ".id" is checked
// against the target's read, and the moves are replayed on that read; a
// chain that fails any of it is skipped whole with the reason (see
// move.go).
//
// Build does not reorder across sections: sections run in the order
// given. Instead, every create or PATCH body that names an object in
// another section (a lease's server, a DHCP server's pool, a scheduler's
// script, a firewall rule's address list or interface) is checked against
// the target, and a referent that isn't there, and isn't created earlier in
// the plan, becomes a Plan.Warnings entry in the dry run (§3). Referents
// outside the inputs are read by the caller: ReferenceReads lists them and
// Options.Referents carries them in.
//
// Build is pure — no I/O — which keeps the planner golden-testable. Execute
// performs a single Op; the caller (the Apply screen) runs a plan one Op at a
// time, stops at the first failure, and then re-runs drift detection to
// report what still differs.
package plan
