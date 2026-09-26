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

`-write` is accepted today but no write operations exist yet; the mode is
shown in the status bar as `read-only` or `write`. The apply, deploy and
failover screens that use it arrive in later milestones.

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
Changing a rule's comment — or inserting an uncommented firewall rule near the
top of a chain — can shift identities and produce diffs that look larger than
the underlying change.

## Keybindings

Global, on any screen:

| Key | Action |
| --- | --- |
| `1` | Overview |
| `2` | Drift |
| `tab` | Toggle between Overview and Drift |
| `q`, `ctrl+c` | Quit |

Drift screen:

| Key | Action |
| --- | --- |
| `r` | Re-fetch drift |
| `enter` | Move the cursor from the section list into the hunks |
| `esc` | Move the cursor back to the section list |
| `up`, `k` | Move up |
| `down`, `j` | Move down |

There is no in-app help overlay yet (`?` is not wired up); this document is
the keybinding reference.

## Readiness checks

The verdict is `Ready` only when **every** check passes. `Degraded` lists the
failing ones with a note. Before the first poll, the verdict is `Unknown`.

| Check | Passes when |
| --- | --- |
| Both routers reachable | Both polled successfully via the API |
| RouterOS versions match | The `version` strings are identical |
| No unresolved drift in synced sections | Drift has been fetched and every section is clean |
| Exactly one master per VRRP instance | For each VRRP instance seen on either router, exactly one side reports `master` |
| Runtime logic present and identical on both routers | Never, in this build |
| Standby netwatch targets up | Every netwatch entry on both routers reports status `up` |

Two of these behave in ways worth knowing:

- **Drift is not fetched automatically.** Until you open the Drift screen, the
  drift check fails with the note `press 2 to check drift`, which holds the
  verdict at `Degraded` on an otherwise healthy pair. Open Drift once to
  satisfy it.
- **The runtime-logic check is hard-coded to fail** until milestone 4, with
  the note `runtime deployment not yet implemented`. This is deliberate: the
  verdict refuses to claim `Ready` when one of its criteria cannot actually be
  evaluated yet. So in the current build, the best possible verdict is
  `Degraded` with that single reason, even on a perfectly healthy pair.

The "standby netwatch targets up" check currently inspects netwatch entries on
*both* routers rather than only the standby.

## Exit codes

| Code | Meaning |
| --- | --- |
| `0` | Quit normally, or `-init` succeeded |
| `1` | Startup or runtime error, printed to stderr as `mtha: <error>` |
