# Usage

## Command line

```
mtha [flags]
```

| Flag | Default | Description |
| --- | --- | --- |
| `-config` | `~/.config/mtha/pairs.yaml` | Path to the pair config file |
| `-pair` | inferred | Pair name. Required when the config defines more than one pair |
| `-write` | `false` | Allow write operations. The session is read-only without it |
| `-init` | `false` | Write a commented sample pair file to `-config` and exit |

With `-init`, nothing else runs: the sample is written, the credential
variables to set are printed, and the process exits. It refuses to overwrite
an existing file, so it is safe to run twice.

```sh
mtha -init                                  # scaffold the default config
mtha -init -config ./pairs.yaml             # scaffold somewhere else
mtha -pair core                             # pick a pair from a multi-pair file
```

Errors go to stderr prefixed with `mtha:` and the process exits non-zero —
including a missing config file, an unparseable one, a pair that is missing
router `a` or `b`, and an unset credential variable.

`-write` gates the Runtime screen's deploy/remove actions and the Apply
screen, shown in the status bar as `read-only` or `write`; without it you can
still preview what deploy, remove or apply would do, just not confirm it.
Failover, which will use it too, arrives in a later milestone.

## Screens

The interface is a TUI. It works in a standard 80×24 terminal and uses more
space when it is there.

### Overview

The landing screen. Two panels, one per router:

- **Reachability** — `reachable` or `unreachable`, with the error underneath
  when unreachable.
- **Identity**, **version**, **uptime**, **CPU** for each router.
- **VRRP state** per instance, coloured by state (`master` green, `backup`
  yellow).

Below the panels is a single **readiness verdict** with the individual checks
that produced it. The status bar shows the pair name, the read/write mode, and
the available keys.

Before the first poll completes, a panel shows `waiting for first poll...`.
The dashboard polls every 5 seconds.

### Drift

Press `2` (or `tab`) to open it. The first entry starts a fetch immediately;
after that, press `r` to refresh.

Drift fetches every configured, non-exempt sync section from both routers
concurrently. A section that fails to fetch is skipped and marked
`fetch failed` rather than aborting the run — the sections that succeeded are
still shown, alongside the first error.

The screen is split:

- **Sections** — one row per section, marked `clean`, `N hunk(s)`, or
  `fetch failed`. The cursor starts here.
- **Hunks** — the differences in the selected section. Press `enter` to move
  the cursor into this list, `esc` to go back to sections.

Hunk markers:

| Marker | Meaning |
| --- | --- |
| `- only on A` | Entry exists on router A and not on router B |
| `+ only on B` | Entry exists on router B and not on router A |
| `~ changed` | Entry exists on both with differing fields |

For a changed entry, the individual field differences (`field: a -> b`) are
expanded when that hunk has the cursor.

What counts as a difference, and which entries are matched to each other, is
described in [configuration.md](configuration.md#how-drift-is-computed).
Changing a rule's comment — or inserting an uncommented firewall rule — can
shift identities (up to the next commented rule in that chain) and produce
diffs that look larger than the underlying change.

**Selecting hunks to sync.** In the hunk list, `space` cycles the hunk under
the cursor through `[A→B]` (make B match A), `[B→A]` (make A match B) and
unselected. `a` / `b` select every hunk in the current section A→B / B→A, and
`c` clears the section's selection. The section list shows how many hunks are
selected in each section. Then press `4` to review the plan on the Apply
screen. Selections survive a refresh as long as the hunk still differs.

### Runtime

Press `3` (or `tab` from Drift) to open it. Like Drift, the first entry
verifies immediately; after that, press `r` to refresh.

Runtime provisions the VRRP interface(s) a pair's `vrrp` entries describe
(once they have `on`/`vrid`/`addresses` set — see
[configuration.md](configuration.md#vrrp)), plus the netwatch entries,
on-master/on-backup scripts and periodic snapshot scheduler job project.md
§5.5 describes. Every object it manages is tagged (`mtha:...` as a comment,
or a leading `# mtha:...` comment line inside a script body) so it can be
found again and cleanly removed, and so a hand-written on-master/on-backup
script is never silently overwritten — that shows as `conflict` instead.

The on-master/on-backup scripts log the transition and, if
`runtime.toggles` is set, enable (master) or disable (backup) the named DHCP
servers and routes — see
[configuration.md](configuration.md#transition-toggles), including why the
standby's DHCP server must be disabled at rest.

The screen lists, per router, every object it manages and its state:

| State | Meaning |
| --- | --- |
| `missing` | Not present on that router yet |
| `mismatched` | Present, but one or more fields differ from the desired config |
| `ok` | Present and matching |
| `conflict` | A guarded field (on-master/on-backup) already holds a non-mtha value; deploy won't overwrite it |

`d` (deploy) and `x` (remove) both show a confirmation listing exactly what
will run before anything happens — visible even without `-write`, so you can
preview it read-only. Confirming (`y`) only actually writes with `-write` set;
without it you'll see `read-only — restart with -write to actually run this`.
Removing flags any VRRP interface deletion on a router currently holding VRRP
master with a `‼`, since it can drop a live VIP.

### Apply

Press `4` (or `tab` from Runtime) to open it. Every time you enter the screen
it reads the selected sections fresh from both routers and builds a **dry
run**: the exact REST operations it would send, numbered and grouped by the
router they write to, each with a one-line explanation.

- Each router's operations start with `POST /system/backup/save` (named
  `mtha-pre-apply-<timestamp>`), so there is a backup to restore from.
- Then per section: `DELETE`s, `PATCH`es (plus `POST .../unset` for fields
  the source router leaves at their default), and `PUT`s (RouterOS REST's
  "add") in the source router's order.
- New firewall rules carry `place-before`, anchored to the next rule in the
  same chain that already exists on the target, so they land in the same
  position as on the source.
- Selected hunks that won't be written are listed under **Skipped** with a
  reason: the hunk no longer differs, the entry is a user (REST can't read
  passwords, so creating one would leave it passwordless), or it would add
  or remove a built-in `ip/service` entry.

Press `y` to apply. Without `-write` this only tells you the session is
read-only. If the plan writes to a router that currently holds VRRP master —
or whose VRRP state isn't known (not polled yet, unreachable) — you must then
also press `Y` (shift+y); `n`/`esc` cancels. Just before running, mtha
re-reads both routers and rebuilds the plan; if anything changed, nothing is
written and the updated plan is shown for review instead.

Operations run one at a time with a progress marker (`…` running, `✓` done,
`✗` failed). The run stops at the first failure — a failed backup means
nothing else is written to that router. Afterwards (success or failure) drift
detection re-runs over the touched sections and each is reported `clean` or
with its residual differences; the Drift screen and readiness verdict are
updated with the result. Press `r` to re-plan.

Long plans scroll with `j`/`k`.

### Events

Press `6` (or `tab` from Apply) to open it. Both routers' logs are re-read
every time you enter the screen; press `r` to re-read them while on it.

Events is a single timeline, newest first, merging:

- **Router A and router B logs** (`/log`), filtered to entries with the
  `vrrp` or `netwatch` topic, and entries whose message starts with `mtha:`
  (written by the on-master/on-backup scripts Runtime deploys).
- **The tool's own actions** from this session, shown with source `tool`.
  Today that is Runtime deploy/remove. The Apply screen's operations are not
  fed into the journal yet, and failover actions will follow once that
  milestone lands. A failed action's kind is shown in red with its error
  appended.

| Column | Meaning |
| --- | --- |
| `time` | When it happened, in this machine's local time |
| `src` | `A`, `B`, or `tool` |
| `kind` | `vrrp`, `netwatch` or `mtha` for router entries; `sync`, `failover` or `runtime` for tool actions |
| `message` | The log message or action summary, truncated to fit |

Above the timeline, one line per router shows how many events were read and
when, or the error if its log couldn't be read. A router that fails keeps the
events from its last successful read.

Router log timestamps are local wall-clock strings, often without a date or
year. `mtha` reads each router's clock straight after its log and places every
entry relative to that, so both routers and the tool's actions line up on this
machine's clock even when the routers' clocks are wrong or in different time
zones. The cost is up to about a second of jitter between routers (RouterOS
logs have 1-second resolution). If a router's clock is more than 3 seconds off
this machine's, the router line says so: worth fixing with NTP on an HA pair,
even though the timeline already corrects for it.

Only what is still in each router's in-memory log is shown (RouterOS keeps
1000 lines by default, across all topics). Nothing is saved to disk: tool
actions are forgotten when you quit.

## Keybindings

Global, on any screen:

| Key | Action |
| --- | --- |
| `1` | Overview |
| `2` | Drift |
| `3` | Runtime |
| `4` | Apply |
| `6` | Events |
| `tab` | Cycle Overview → Drift → Runtime → Apply → Events → Overview |
| `?` | Open/close the help overlay |
| `q`, `ctrl+c` | Quit |

Drift screen:

| Key | Action |
| --- | --- |
| `r` | Re-fetch drift |
| `enter` | Move the cursor from the section list into the hunks |
| `esc` | Move the cursor back to the section list |
| `up`, `k` | Move up |
| `down`, `j` | Move down |
| `space` | Cycle the hunk's selection: A→B, B→A, unselected |
| `a` / `b` | Select every hunk in the section A→B / B→A |
| `c` | Clear the section's selection |

Runtime screen:

| Key | Action |
| --- | --- |
| `r` | Re-verify runtime status |
| `d` | Show a deploy confirmation |
| `x` | Show a remove confirmation |
| `y` | Confirm the pending deploy/remove (requires `-write`) |
| `n`, `esc` | Cancel the pending confirmation |

Apply screen:

| Key | Action |
| --- | --- |
| `r` | Re-read both routers and rebuild the plan |
| `y` | Apply the plan (requires `-write`) |
| `Y` | Second confirmation when writing to the current master or a router whose VRRP state is unknown |
| `n`, `esc` | Cancel a pending confirmation |
| `up`, `k` / `down`, `j` | Scroll the plan |

Events screen:

| Key | Action |
| --- | --- |
| `r` | Re-read both routers' logs |
| `down`, `j` | Scroll towards older events |
| `up`, `k` | Scroll towards newer events |
| `g` | Jump to the newest event |
| `G` | Jump to the oldest event |

Help overlay:

| Key | Action |
| --- | --- |
| `?`, `esc` | Close the overlay |
| `q`, `ctrl+c` | Quit |

`?` opens a help overlay on any screen, listing the global keys plus the
ones for the screen you opened it from. While it is open every other key is
ignored — so, for instance, a stray `y` can't confirm a pending Runtime
deploy/remove underneath it. Closing it returns you to the same screen,
with any pending confirmation still pending.

## Readiness checks

The verdict is `Ready` only when **every** check passes. `Degraded` lists the
failing ones with a note. Before the first poll, the verdict is `Unknown`.

| Check | Passes when |
| --- | --- |
| Both routers reachable | Both polled successfully via the API |
| RouterOS versions match | The `version` strings are identical |
| No unresolved drift in synced sections | Drift has been fetched and every section is clean |
| Exactly one master per VRRP instance | For each VRRP instance seen on either router, exactly one side reports `master` |
| Runtime logic present and identical on both routers | Runtime has been verified and every managed object is `ok` on both routers |
| Standby netwatch targets up | Every netwatch entry on both routers reports status `up` |

Two of these behave in ways worth knowing:

- **Drift is not fetched automatically.** Until you open the Drift screen, the
  drift check fails with the note `press 2 to check drift`, which holds the
  verdict at `Degraded` on an otherwise healthy pair. Open Drift once to
  satisfy it.
- **Runtime is not verified automatically either**, the same way. Until you
  open the Runtime screen, this check fails with `press 3 to check runtime`.
  Once verified, its note names the first non-`ok` item if any remain.

The "standby netwatch targets up" check currently inspects netwatch entries on
*both* routers rather than only the standby.

## Exit codes

| Code | Meaning |
| --- | --- |
| `0` | Quit normally, or `-init` succeeded |
| `1` | Startup or runtime error, printed to stderr as `mtha: <error>` |
