# Open design questions (issue #5)

The three questions listed as open in `project.md` §10.2, each worked
through to a decision someone can make: the evidence, the real options, the
cost and risk of each, and a recommendation. **Nothing here is decided or
implemented.** Decisions belong in `project.md` §10.1 once made.

Evidence comes from two places only: the code at the commit this page was
written against, and `docs/lab-rest-contract.md` (the REST survey of the two
RouterOS 7.23.7 lab routers, "the contract" below). Where neither settles a
point, it is listed under [Needs the lab](#needs-the-lab) rather than
guessed.

The sections in scope are the twelve that ship in the sample
(`internal/config/sample.go`) and in `testlab/pairs.yaml`, in this order:

```
ip/firewall/filter, ip/firewall/nat, ip/firewall/address-list,
ip/dhcp-server, ip/dhcp-server/network, ip/dhcp-server/lease,
ip/dns/static, ip/route, ip/service, user, system/script, system/scheduler
```

## Summary

| # | Question | Recommendation | Lab needed before building? |
|---|---|---|---|
| 1 | Custom identity rules | Keep comment-first **only** where there is no natural key (firewall rules, tagged routes). Everywhere else identify by the natural key and treat `comment` as an ordinary compared field. Specifically: `ip/dns/static` gets `name\|type\|value`, which removes its bare ordinal. | Partly: non-`A`/`CNAME` DNS record shapes, and which natural keys RouterOS enforces as unique. The change itself can be built and unit-tested now. |
| 2 | Rule order drift | **Detect it now**, per chain, over rules present on both routers; show it as one order hunk per chain. **Plan `move` later**, after the lab confirms the REST `move` call. Until then an order hunk is shown but skipped with a reason. | Detection: no. Planning `move`: yes. |
| 3 | Cross-section dependencies | **A warning, not a reorder.** A small static table of reference fields, checked at plan time against the target (plus objects created earlier in the same plan); a missing referent becomes a dry-run warning. No dependency graph. | To decide which references are hard (rejected) vs soft (accepted, inert): yes. The warning itself does not depend on it. |

---

## 1. Which sections need custom identity rules?

### How identity works today

`BuildIdentities` (`internal/model/identity.go:47`) does, for every section:

1. **Comment first.** A non-empty `comment` is the identity. Repeats within
   the section are deduplicated in encounter order: `c`, `c#2`, `c#3`
   (`identity.go:64`, `commentDeduper`).
2. Otherwise a per-section fallback: anchored chain ordinal (firewall),
   `list|address` (address-list), `address` (dns/static), `dst->gw` (route),
   and for everything else `name`, then `address`, then a **bare section
   ordinal** `#n` (`identity.go:104`).

`diff.Compare` then matches entries on (identity, occurrence) and reports a
changed identity as one remove plus one add, never as a field change
(`internal/diff/diff.go:40`). That is the key consequence: **whatever is in
the identity cannot be updated in place, only deleted and recreated.**

### Section by section

Field shapes are from the contract's per-section notes. "Natural key" is
the field (or fields) that identifies the object to an operator and that
RouterOS itself uses for lookup where it has one (contract, "Lookup by
name").

| Section | Natural key (from the shape) | Identity today, uncommented | Identity today, commented | Stable? |
|---|---|---|---|---|
| `ip/firewall/filter`, `nat` | none: rules have no `name` (a lookup by comment is a 400) | anchored chain ordinal | comment | **Yes, settled** (§10.1). Order is question 2. |
| `ip/firewall/address-list` | `list` + `address` (the same address in two lists is two entries) | `list\|address` | comment, deduped by position | Uncommented yes. **Commented no** when a comment is shared (below). |
| `ip/dhcp-server` | `name` | `name` | comment | Stable, but a comment edit is delete + create. |
| `ip/dhcp-server/network` | `address` (a network cannot be disabled; no `name`) | `address` | comment | As above. |
| `ip/dhcp-server/lease` (static) | `server` + `mac-address` (the reservation is for a client) | `address` | comment | As above; `address` makes a re-addressed reservation a delete + create. |
| `ip/dns/static` | `name` + `type` + value (`address` for `A`, `cname` for `CNAME`); one name may have several records | `address`, else **bare ordinal** | comment | **No.** Any non-`A` record is positional. |
| `ip/route` (tagged only) | the `mtha:` tag comment is the opt-in (§10.1) | `dst->gw` (never reached: `Select` drops untagged routes) | comment | **Yes, settled.** |
| `ip/service` | `name` (built-in rows; dynamic rows are dropped by `Select`) | `name` | comment | Stable while uncommented. The contract lists no `comment` field for this section, so whether one can arise is unknown. |
| `user` | `name` | `name` | comment | Stable, but a comment edit is **destructive on sync** (below). |
| `system/script` | `name` | `name` | comment | Stable, but a comment edit is delete + create. |
| `system/scheduler` | `name` | `name` | comment | As above. |

So, strictly, **only one shipped section falls to a bare ordinal**:
`ip/dns/static`, for any record without `address` (contract item 13; the
lab's own `CNAME` is identified `#1`). Inserting another address-less
record ahead of it re-identifies it
(`TestBuildIdentitiesDNSStaticCNAMEIsPositional`).

But the audit turns up two wider problems with comment-first in the
non-firewall sections, and they matter more than the ordinal:

**(a) A shared comment makes the identity positional.** Comments are free
text and are routinely shared. The lab fixture itself does it (`lab: two
addresses` on two DNS records, contract "Second survey"), and address
lists fed by a script typically carry one comment on every entry. Dedupe
turns those into `c`, `c#2`, `c#3` in list order, which is exactly the bare
ordinal the question worries about: an insertion ahead of them
re-identifies every later one, although `list|address` would have
identified each uniquely
(`TestBuildIdentitiesSharedCommentOverridesNaturalKey`).

**(b) In a name-keyed section, a comment edit is a delete + create, which is
wrong, and for `user` it deletes the user.** If a user's comment differs
between routers, the two sides get different identities and the diff shows
an add and a remove. Syncing both A→B plans a `DELETE` of the user on B and
skips the create, because REST cannot read passwords
(`internal/plan/plan.go:337`). B loses the user, where a `PATCH` of
`comment` was all that was needed. The lockout guard only protects the one
user mtha logs in as. This is pinned by
`TestBuildUserCommentChangeDeletesWithoutRecreate`. The same shape in
`system/script`/`scheduler` loses the script's `.id` and its `run-count`,
and in `ip/service` it produces an add/remove pair that
`patchOnlySections` refuses both halves of, so the drift can never be
resolved.

### Options

- **A. Leave it; fix only DNS.** Add a DNS key and nothing else. Cheapest.
  Leaves (a) and (b) in place; (b) is a data-loss path on `user`.
- **B. Natural key first, comment ignored for identity, in every section
  that has one.** Comment becomes a compared field like any other, so a
  comment change is a `PATCH`. Firewall rules and routes keep comment-first
  because they have no natural key (routes additionally use the tag as the
  opt-in). Cost: one switch in `BuildIdentities` and its tests. Risk: one
  behavioural change to watch, below.
- **C. Comment first, but dedupe shared comments with the natural key**
  (`feed|blocked|198.51.100.1` instead of `feed#2`). Fixes (a), not (b).
  More complex than B for less.

### Recommendation: B

Per section:

| Section | Identity |
|---|---|
| `ip/firewall/filter`, `nat` (and `mangle`, `raw`) | unchanged: comment, else anchored chain ordinal |
| `ip/route` | unchanged: tag comment, else `dst-address->gateway` |
| `ip/firewall/address-list` | `list\|address`, always |
| `ip/dhcp-server`, `ip/service`, `user`, `system/script`, `system/scheduler` | `name`, always |
| `ip/dhcp-server/network` | `address`, always |
| `ip/dhcp-server/lease` | `server\|mac-address`, falling back to `address` if `mac-address` is empty |
| `ip/dns/static` | `name\|type\|<value>`, where value is `address` for `A` and `cname` for `CNAME` (the two types surveyed); `name\|type` for any type whose value field is not yet known (collisions then pair by occurrence, as today) |
| any other section | unchanged fallback, but comment should be **last** before the ordinal, not first |

Notes:

- A natural-key collision is already handled: `diff.Compare` pairs repeated
  identities by occurrence (`diff.go`, `Hunk.Occurrence`), so B never
  silently drops an entry.
- The one behaviour to call out when deciding: under B, re-addressing a DNS
  record or changing a network's `address` is a delete + create rather
  than a patch. That is correct for DNS, because the value is what tells two
  records under one name apart, and harmless for a network, which has
  nothing else to it.
- Lease keyed on `server|mac-address` means renaming a DHCP server
  re-identifies its leases. Given that a server's own identity is its name,
  that rename is already a delete + create of the server, so the leases
  following it is consistent.
- The lab baseline stays clean under B: every fixture entry's natural key is
  distinct, and the two DNS records sharing a comment have different
  addresses.

Needs the lab: the value fields of DNS types other than `A` and `CNAME`
(the contract did not survey them), and whether RouterOS enforces
uniqueness of `list+address`, a network's `address` and a lease's
`server+mac-address` (it only matters for how often the occurrence pairing
is exercised, not for correctness).

---

## 2. Rule order drift

### What is and isn't detected today

The anchored identity already catches most reordering, indirectly:

- **Uncommented rules swapped within their block** keep their identities
  (`input@x#1`, `input@x#2`), which now point at different contents: they
  show as field changes, and syncing them `PATCH`es the contents across,
  which reorders in effect.
- **An uncommented rule moved past a commented one** changes anchor, so it
  shows as a remove plus an add, and the add is placed with `place-before`.

What is invisible is a **permutation of commented rules**, each carrying its
uncommented followers with it (the anchors travel with the block): every
identity still matches, so `diff.Compare` reports nothing. Two routers can
read clean while one allows a host before dropping its subnet and the other
drops first (`TestCompareIgnoresCommentedRuleOrder`). The same holds for
two rules in different chains, which is correct: only order within a chain
matters (`placeBefore`'s own comment, `plan.go:473`).

### Detection

For each firewall section, per chain: take the identities present on both
routers (identity plus occurrence, exactly what `plan.index` uses), in A's
order and in B's. They are unique, so the minimal set of rules that must
move is the complement of the longest increasing subsequence of B-positions
taken in A order: O(n log n), no problem at the 3,000-rule size the contract
showed comes back whole in one GET. If that set is non-empty, the chain has
order drift.

Surfacing it needs one new thing in the diff model: an order finding per
chain (e.g. `SectionDiff.Order []OrderHunk{Chain, Moved []HunkRef}`), not a
fake identity in `Hunk`, so it cannot collide with a comment and the UI can
show it as "chain `forward`: 2 rules in a different order", with the moved
rules listed.

### False-positive risk: low, if scoped as above

| Source of a spurious report | Handled by |
|---|---|
| Different interleaving of chains in the list | comparing per chain only; interleaving is cosmetic |
| A rule present on one router only | comparing only rules on both sides; the add/remove is already its own hunk |
| Dynamic rules RouterOS inserts | `Select` drops `dynamic:"true"` before identity |
| Uncommented rules within a block | positional identity already turns these into field changes, so they never reach the order check |
| Two rules sharing a comment | deduped by position, so a swap is again a field change, not an order finding |
| Reordering two rules that cannot both match a packet | **not handled**: it is reported although the policy is equivalent. Telling equivalent orders apart needs matcher-overlap analysis, which is not worth building. It is still a real config difference, and reporting it matches what the tool does everywhere else. |

The lab baseline is identical on both routers, so it cannot show a false
positive either way. That is not evidence of absence on a real pair.

### Planning a `move`, and how it meets `place-before`

RouterOS has a `move` command (the plan's own doc lists it among the POST
commands, `plan.go:96`); the contract did not survey its REST body or
whether a moved rule keeps its `.id`.

Order of operations within a firewall section would become **deletes,
updates, moves, creates**:

- Moves must come **before** creates. `placeBefore` anchors a new rule to
  the next same-chain source rule that already exists on the target. If the
  target's existing rules are out of order, the new rule lands correctly
  next to its anchor but in the wrong place relative to everything else.
  Moving first puts the anchors in source order, and then every create
  lands where it belongs.
- Moves after creates would need the new rules' `.id`s, which `Build`
  cannot know: it is pure, and the IDs only exist after the `PUT`.
- Each move can be planned like a create: move rule X before the next
  same-chain source rule that is already in order on the target, applied in
  source order. The `.id` of that anchor is known at plan time, provided a
  move preserves `.id` (lab).

Risks:

- **Transient policy.** A sequence of single moves passes through orders
  that are neither router's. On an `input` chain that can transiently put a
  drop above the rule admitting mtha's own REST connection, the same
  lockout class `lockoutReason` guards for `www-ssl`. Creates already carry
  this risk; moves widen it. Mitigation: the dry run says so, and the
  existing master double-confirmation applies.
- **Partial apply.** Execution stops at the first failure
  (`internal/plan/execute.go`), so a failed move leaves a half-reordered
  chain. Drift is re-run afterwards and will show it, which is the existing
  contract for every other op.

### Is it worth doing?

Detection: **yes.** Firewall filter is the first section in the shipped
list, and "reads clean while the policy differs" is the one outcome a drift
tool must not have: it is a false negative, and a failover is exactly when
the standby's policy starts to matter. Detection is pure code with no
device risk.

Planning the move: **yes, but second**, and only after the lab confirms the
REST call. Until then, an order finding is shown and, if selected, skipped
with a reason ("reorder this chain by hand; move is not implemented"), the
same way unsafe hunks are skipped today. Delete-and-recreate is not an
acceptable stand-in: it leaves a window with the rule missing, resets its
counters and changes its `.id`.

---

## 3. Cross-section dependencies on apply

### What happens today

`buildApplyPlan` (`internal/ui/apply.go`) passes the sections in configured
order; `plan.Build` emits, per router, a backup and then each section's
deletes, updates and creates in that order (`plan.go:388`). Nothing looks
at references between sections, and the operator can select any subset of
hunks, including a referrer without its referent.

### Which references exist between the shipped sections

From the field lists in the contract. "Target" is where the name has to
exist.

| Referrer field | Refers to | In the sync list? | Behaviour when missing |
|---|---|---|---|
| firewall `src-address-list`, `dst-address-list` | `ip/firewall/address-list` `list` | yes, after `filter`/`nat` | **Accepted.** `testlab/provision.sh` adds the filter rule using `lab-trusted` before any `lab-trusted` entry exists, the script stops on any non-2xx, and `internal/labtest` finds all 11 rules. The rule is inert until the list fills. |
| firewall `in-interface`, `out-interface` | `interface` | no (per-router hardware) | not surveyed |
| firewall `jump-target` | a chain in the same section | same section | not surveyed |
| `ip/dhcp-server` `address-pool` | `ip/pool` `name` | **no** | not surveyed |
| `ip/dhcp-server` `interface` | `interface` | no | not surveyed |
| `ip/dhcp-server/lease` `server` | `ip/dhcp-server` `name` | yes, before `lease` | not surveyed (the fixture always adds the server first) |
| `ip/route` `gateway` (when an interface name), `routing-table` | `interface`, routing tables | no | not surveyed |
| `user` `group` | `user/group` | **no** | not surveyed |
| `system/scheduler` `on-event` | `system/script` `name` (or inline source) | yes, after `script` | **Accepted.** The fixture adds `lab-daily` with `on-event` naming the script before the script exists. It would fail when it runs. |
| `ip/service` `certificate` | certificates | no (and exempt in the sample) | not surveyed |

`ip/dhcp-server/network` and `ip/dns/static` refer to addresses and names,
not to other objects.

What this shows:

- For **creates**, the shipped order already puts every referent in the
  sync list before its referrer (`dhcp-server` → `lease`, `script` →
  `scheduler`). The one exception, `filter`/`nat` before `address-list`, is
  a soft reference the device accepts. For the length of the apply it
  leaves a list-based rule inert, which for a drop rule on a blocklist means
  it fails open until the list is written. A few requests later it is in
  place.
- For **deletes**, the order is the reverse of safe: a DHCP server's
  deletes run (section 4) before its leases' (section 6), and a script's
  before its scheduler's. What RouterOS does when a lease's server
  disappears is not surveyed.
- The **real exposure is outside the sync list**: `ip/pool`, `user/group`,
  interfaces and routing tables are referenced but never synced. A DHCP
  server copied to a router that lacks its pool is the issue's own example,
  and no ordering can fix it, because the pool is never in the plan.
- **Half-selection**: choosing a lease hunk without the new server it
  names.

### Options

- **A. Nothing.** An apply that hits a hard reference stops at the failing
  op, the backup is there, and drift re-runs. That is safe, but the operator
  learns about it only by failing.
- **B. Warning pass.** A static table of the reference fields above. At plan
  time, for each create or `PATCH` body that sets one, check the referent
  exists on the target, or is created earlier in the same plan. If not, add
  a warning to the dry run (a new `Plan.Warnings`, not a `Skip`, since the
  device may well accept it). Referenced sections outside the sync list
  (`ip/pool`, `user/group`, `interface`) need one extra read-only GET each
  at plan time, only when a body refers to them. Cost: a table, a lookup
  and the extra reads. Risk: none to the device.
- **C. Real ordering.** Topologically sort ops across sections, or at least
  emit all deletes in reverse section order before any update or create.
  That fixes delete order and nothing else, and cannot help references to
  unsynced sections, which are the main exposure. It also changes the
  documented per-section plan semantics (§10.1).
- **D. Validate section order at config load.** Warn if a synced referent
  is configured after its referrer (`lease` before `dhcp-server`,
  `scheduler` before `script`). Cheap, and it covers the create-order half
  of C for free.

### Recommendation: B, plus D

A warning is the minimal guard that is actually useful: it catches the
unsynced-referent and half-selection cases, which are the real ones, and
ordering cannot. D stops a pair file from breaking create order. Hold C
back until the lab shows a delete-order case that RouterOS rejects or
leaves broken (a lease whose server was deleted first). If it does, the
narrow form of C, deletes in reverse section order, is the fix, and it is
small.

Independently of this decision: listing `ip/firewall/address-list` before
`filter`/`nat` in the sample would close the fail-open window above. That
is a change to the shipped sample, so it is left for whoever decides.
`testlab/pairs.yaml` keeps `filter` first on purpose (its comment: the
smoke test depends on it).

---

## Needs the lab

None of these can be settled from the code or the contract. All are
read-only or undone by the harness's golden restore.

1. **DNS static record shapes** for types other than `A` and `CNAME`
   (`AAAA`, `MX`, `TXT`, `SRV`, `FWD`, `NXDOMAIN`): which field holds the
   value. (Q1)
2. **Uniqueness RouterOS enforces** on `list+address`, a DHCP network's
   `address`, a lease's `server+mac-address`, and duplicate DNS
   `name+type+address`. (Q1)
3. **Whether `ip/service` accepts and returns `comment`.** (Q1)
4. **REST `move`**: the body `POST /rest/ip/firewall/filter/move` takes
   (`numbers`, `destination`?), and whether the moved rule keeps its `.id`.
   (Q2)
5. **Hard or soft references**: create a DHCP server with an unknown
   `address-pool`, a lease with an unknown `server` (and whether `all` is a
   value), a user with an unknown `group`, a route with an unknown
   `routing-table`, a firewall rule with an unknown `in-interface`; and
   delete a DHCP server that a static lease still names. (Q3)

## Tests added with this page

They pin the current behaviour described above and are expected to change
when a decision is implemented:

- `internal/model/identity_test.go`: `TestBuildIdentitiesDNSStaticCNAMEIsPositional`,
  `TestBuildIdentitiesSharedCommentOverridesNaturalKey` (Q1)
- `internal/plan/plan_test.go`: `TestBuildUserCommentChangeDeletesWithoutRecreate` (Q1)
- `internal/diff/diff_test.go`: `TestCompareIgnoresCommentedRuleOrder` (Q2)
