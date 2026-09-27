# mtha — MikroTik HA Pair Manager

**Status:** Draft v0.1 **Owner:** Rory **Working name:** `mtha` (rename freely)

---

## 1. Problem statement

RouterOS provides VRRP and nothing else for high availability. Two routers sharing a virtual IP are still two independently managed devices: configuration drifts, there is no view of whether the standby can actually take over, and planned failover is a manual, nerve-wracking exercise. Connection tracking, DHCP leases and configuration are not synchronised natively.

The goal is a tool that makes a MikroTik pair **behave like a unit**: visible readiness, kept in step, and safe to fail over on purpose.

## 2. Architectural principle

**Runtime failover logic lives on the routers. The tool is on-demand.**

Anything that must happen *during* a failure runs natively on RouterOS (VRRP, netwatch, scripts, schedulers) with no dependency on an external controller. The TUI is an operator tool used to design, deploy, verify, synchronise and rehearse — nothing breaks if it is not running.

Analogy: `kubectl`, not a control plane.

## 3. Scope

### 3.1 In scope (v1)

- Define a **pair**: two RouterOS 7 devices, credentials, VRRP instances of interest, sync sections, per-router exemptions.
- **Readiness dashboard**: side-by-side health, VRRP state, version match, drift status, single readiness verdict.
- **Drift detection**: structured, section-by-section config diff between the two routers.
- **Sync / apply**: selective push of config hunks in either direction, with dry run, pre-apply backup and post-apply verification.
- **Runtime logic deployment**: template and push netwatch entries, VRRP `on-master` / `on-backup` scripts and schedulers to both routers; verify presence and equality.
- **Planned failover / failback**: adjust VRRP priority/preemption with pre-flight checks, live transition view, optional VIP reachability measurement.
- **Events**: timeline of VRRP transitions, sync operations and health changes, sourced from router logs.
- Read-only by default; write mode is explicit.

### 3.2 Out of scope (v1)

- Connection-tracking / firewall state sync (not possible on RouterOS).
- DHCP lease synchronisation (v2 candidate).
- Continuous daemon / alerting mode (v2 candidate: `mtha watch`).
- Running the tool on the router itself (containers) — revisit only if lease sync warrants a per-router agent.
- More than two routers per pair.
- RouterOS 6 support.

### 3.3 Non-goals

- Replacing Winbox/WebFig for general configuration.
- General-purpose config management / backup for MikroTik fleets.

## 4. Users and usage model

Single operator (network/infra engineer) running the tool from a laptop or jump host with API reachability to both routers. Sessions are short and task-driven: "is the pair healthy", "what has drifted", "push this", "fail over now".

Pairs are defined in a local config file. Multiple pairs may be defined; the tool opens on a pair list when more than one exists.

## 5. Functional requirements

### 5.1 Pair definition

Config file (YAML), default location `~/.config/mtha/pairs.yaml`.

```yaml
pairs:
  - name: core
    routers:
      a: { host: 10.0.0.2, user: mtha, insecure_tls: false }
      b: { host: 10.0.0.3, port: 8443, user: mtha } # port optional, default 443
    vrrp:
      - interface: vrrp-lan
        on: ether2       # physical interface the VRRP interface rides on
        vrid: 1
        addresses: [10.0.0.1/24]   # VIP(s), assigned to the VRRP interface
      - interface: vrrp-wan
        on: ether1
        vrid: 2
        addresses: [203.0.113.1/29]
    sync:
      sections:
        - ip/firewall/filter
        - ip/firewall/nat
        - ip/firewall/address-list
        - ip/dhcp-server
        - ip/dhcp-server/network
        - ip/dhcp-server/lease   # static only
        - ip/dns/static
        - ip/route               # excluding per-router routes
        - ip/service
        - user
        - system/script
        - system/scheduler
      exempt:
        - system/identity
        - interface/vrrp.priority
        - ip/address              # per-router interface addresses
        - ip/service.certificate  # each router's own self-signed cert
        - user.last-logged-in     # updates independently on every login
    runtime:
      netwatch_targets: [1.1.1.1, 8.8.8.8]
      priority_master: 200
      priority_backup: 100
      priority_degraded: 50

```

Credentials come from environment variables or the OS keychain, never the pair file.

### 5.2 Dashboard / readiness

For each router: reachability, RouterOS version, uptime, CPU, memory, VRRP state per instance, netwatch status, last sync time.

Readiness verdict is **Ready** only when all of the following hold:

- Both routers reachable via API.
- RouterOS versions match.
- No unresolved drift in synced sections.
- Exactly one master per VRRP instance.
- Runtime logic present and identical on both routers.
- Standby router's netwatch targets are up.

Anything else shows **Degraded** with the specific reasons listed.

### 5.3 Drift detection

- Read each configured section via REST (`GET /rest/<section>`), not via `/export` text.
- Normalise: drop `.id`, dynamic entries, and fields listed as exempt; treat absent and default-valued fields as equal.
- Match entries on stable identity per section (e.g. `comment` if present, otherwise chain + ordinal for firewall rules, `name` for scripts/schedulers, `address` for DNS static).
- Present per-section diff with added / removed / changed entries, colour-coded, and hunk-level selection.
- Direction is explicit: A→B or B→A, chosen per hunk or per section.

### 5.4 Sync / apply

- Dry run lists every REST operation (POST / PATCH / DELETE with target path and body) before anything is written.
- Take `/system/backup/save` on the target router before applying.
- Preserve firewall rule ordering using `place-before`.
- Re-run drift detection after apply and report residual differences.
- Pushing **to the current VRRP master** requires a second explicit confirmation.
- Write mode requires `--write` flag or an in-app toggle; default session is read-only.

### 5.5 Runtime logic deployment

Also provisions the VRRP interface(s) and VIP(s) a pair's `vrrp` entries
describe (`interface/vrrp`, `ip/address`) — extended beyond this section's
original scope so the tool can set a pair up from nothing, not only manage
automation around an interface created by hand.

Templates, rendered from the pair definition and pushed to both routers:

- `/tool/netwatch` entries per target with `up-script` / `down-script` that raise/lower VRRP priority (`priority_master` ↔ `priority_degraded`).
- VRRP `on-master` / `on-backup` scripts: log the transition; optionally enable/disable DHCP server, adjust routes.
- `/system/scheduler` entry that periodically exports config to a file (`mtha-snapshot.rsc`) so a snapshot exists without the tool running.

All entries are tagged with a `mtha:` comment prefix so they can be identified, verified and cleanly removed. Deployment is idempotent.

### 5.6 Planned failover / failback

Pre-flight (must all pass, or the action is refused):

- Target standby is reachable and healthy.
- No unresolved drift.
- Runtime logic verified on both routers.

Action: lower current master's VRRP priority below the standby's (or raise standby's), confirm preemption settings, watch state change. Failback reverses it.

Live view: VRRP state per instance updated on a short poll; optional ICMP probe against the VIP reporting seconds of loss.

### 5.7 Events

Pull `/log` from both routers filtered on `vrrp`, `netwatch` and `mtha:` prefixed entries; merge into a single timeline. Append the tool's own sync/failover actions. No local persistence beyond the session in v1.

## 6. Non-functional requirements

- Single static binary per platform: Linux (amd64/arm64), macOS (arm64), Windows (amd64).
- Zero runtime dependencies on the host.
- All API traffic over HTTPS; `insecure_tls` is an explicit per-router opt-in.
- Never store credentials in plaintext config.
- Any destructive operation (apply, failover) is preceded by a visible dry run and explicit confirmation.
- Works over SSH in a standard 80×24 terminal; better at larger sizes.
- Poll interval configurable; default 5s on dashboard, 1s during failover view.

## 7. Design

### 7.1 Screens


| Screen   | Purpose                                                                 |
| -------- | ----------------------------------------------------------------------- |
| Pairs    | List of defined pairs with summary readiness (skipped if only one pair) |
| Overview | Side-by-side router status and readiness verdict                        |
| Drift    | Section list → per-section diff with hunk selection                     |
| Apply    | Dry-run operation list, confirm, progress, verification                 |
| Runtime  | Status of deployed runtime logic; deploy / re-verify / remove           |
| Failover | Pre-flight checklist, action, live transition view                      |
| Events   | Merged timeline                                                         |


Navigation: tab/number keys between screens, vim-style movement within lists, `?` for help. Status bar shows pair name, read/write mode, last poll.

### 7.2 Internal architecture

```
cmd/mtha            entrypoint, flags
internal/config     pair file loading, credential resolution
internal/routeros   REST client, typed section readers/writers, log reader
internal/model      canonical config model, normalisation rules per section
internal/diff       comparison engine, identity matching, hunk generation
internal/plan       dry-run planner: diff hunks → ordered REST operations
internal/runtime    templates for netwatch/VRRP scripts/schedulers, verify
internal/poll       per-router polling goroutines → snapshot messages
internal/ui         Bubble Tea models per screen, shared styles

```

Data flow: `poll` goroutines emit `SnapshotMsg` per router → root model updates state → screens render from state. Actions (apply, failover) run as `tea.Cmd`s and emit progress messages.

### 7.3 Safety model

- Read-only unless `--write`.
- Every write goes through `plan` and is shown before execution.
- Backup before apply.
- Master-write double confirm.
- Failover refuses on failed pre-flight.
- All tool-created router objects carry the `mtha:` comment tag.

## 8. Stack


| Concern       | Choice                                        | Notes                                                        |
| ------------- | --------------------------------------------- | ------------------------------------------------------------ |
| Language      | Go 1.23+                                      | Static binaries, trivial cross-compile, stdlib HTTP/JSON/TLS |
| TUI framework | Bubble Tea                                    | Elm-style model/update/view                                  |
| Components    | Bubbles                                       | table, list, viewport, spinner, textinput                    |
| Styling       | Lip Gloss                                     | Layout and theming                                           |
| Config        | [`gopkg.in/yaml.v3`](http://gopkg.in/yaml.v3) | Pair file                                                    |
| Credentials   | env vars; `zalando/go-keyring` optional       | Never in pair file                                           |
| Router API    | RouterOS 7 REST (`/rest/...`)                 | Basic auth over HTTPS; no third-party client                 |
| Diff display  | `sergi/go-diff` or hand-rolled                | Only for changed-field rendering                             |
| Build         | `go build`, GoReleaser                        | Cross-platform release binaries                              |
| Testing       | stdlib `testing` + golden files               | Normalisation and planner are the critical units             |


## 9. Milestones

1. **Skeleton** — pair config, REST client, one-screen dashboard showing both routers' basic status and VRRP state. Done.
2. **Drift** — section readers, normaliser, diff engine, drift screen (read-only). Done.
3. **Apply** — planner, dry run, backup, apply, verify. `--write` gate. Done — hunk selection with per-hunk / per-section direction on the Drift screen; `internal/plan` builds the ordered REST operations (backup first, then deletes, updates, creates; `place-before` for firewall creates) from fresh reads; the Apply screen shows the dry run, confirms (twice for the current VRRP master), re-checks the plan is still current, runs it op by op, and re-runs drift to report residuals. Also settled the identity rules the planner depends on (§10.1).
4. **Runtime** — templates, deploy, verify, remove. Done — extended beyond the original §5.5 scope to also provision the VRRP interface(s) and VIP(s) themselves (`interface/vrrp`, `ip/address`), not only the automation layered on top of one.
5. **Failover** — pre-flight, action, live view, VIP probe.
6. **Events** — log merge, timeline.
7. **Polish** — in-app help (`?`). Multi-pair and the release pipeline are dropped (§10.1).

Milestones 1–2 deliver value on their own and de-risk the hardest part (normalisation). Milestone 3's write path (`internal/plan`: a visible plan, explicit confirmation, master double-confirm, stop-on-first-failure execution, post-write verification) is the foundation milestone 5 builds on.

## 10. Decisions and open questions

### 10.1 Settled

- **Firewall rule identity.** Comment first. A comment repeated within a section is deduplicated in list order: `web`, `web#2`, `web#3`. Uncommented firewall rules (filter/nat/mangle/raw) use an ordinal anchored to the preceding commented rule in the same chain — `input@allow-ssh#2` is the second uncommented `input` rule after `allow-ssh`; rules before a chain's first commented rule are `input#1`, `input#2`, … An insertion therefore only re-identifies the uncommented block after it, up to the next commented rule. Pure positional identity was rejected: it turns every insertion into a cascade of changes.
- **`ip/route` sync is opt-in by tag.** Only routes whose comment starts with `mtha:` are selected; untagged routes (connected, per-router defaults, anything hand-managed) never enter the diff or a plan. Identity stays comment-first, else `dst-address->gateway`. This replaces "whole section with exemptions" (the §5.1 sample's `# excluding per-router routes` means exactly this).
- **Read-only state is not config.** Normalisation strips fields RouterOS reports but that describe runtime state (firewall byte/packet counters, script `run-count`, lease `status`, route `active`, …) — they would otherwise be permanent drift and are rejected in write bodies.
- **DHCP cutover is toggled by the VRRP scripts.** The standby's DHCP server is enabled/disabled by the `on-master` / `on-backup` scripts; a persistently enabled standby with a split scope is dropped. (The runtime templates do not toggle DHCP yet; that is a Runtime template change, tracked with §5.5.)
- **Multi-pair is dropped.** One pair per session. A pair file may still define several pairs, selected with `-pair`; the Pairs screen (§7.1) and pair list (§4) will not be built.
- **The GoReleaser release pipeline is dropped.** Binaries are built with `go build` (plain `GOOS`/`GOARCH` cross-compilation, §6); §8's GoReleaser entry is superseded.
- **Apply plan semantics (milestone 3).**
  - Operations are shown with the HTTP verb actually sent. RouterOS REST maps add → `PUT`, set → `PATCH`, remove → `DELETE`, and every other command (backup, unset) → `POST`; §5.4's "POST" for creates means this `PUT`.
  - The pre-apply `/system/backup/save` is itself an operation in the plan — it is a write, so it is shown like one — and is the first op for every router the plan writes to.
  - Per section: deletes, then updates (`PATCH` for fields the source sets, `POST …/unset` for fields it leaves at default), then creates in the source's order. A firewall create is placed before the next same-chain rule on the source that already exists on the target, else appended.
  - Selected hunks are re-diffed against fresh reads at plan time; hunks that no longer differ are skipped, not written. Creating users (REST cannot read passwords) and adding/removing built-in `ip/service` entries are skipped with a reason.
  - The plan is rebuilt immediately before execution and refused if it differs from the one confirmed.
  - Execution is sequential and stops at the first failure; drift is re-run afterwards either way.
  - "Current VRRP master" for the second confirmation includes a router whose VRRP state is unknown (not polled, unreachable, or the read failed).

### 10.2 Open

- Which other sections need custom identity rules beyond `comment`/`name`?
- Rule *order* drift: two commented rules present on both routers but in a different relative order match by identity and show no difference. Detecting (and planning a `move` for) reordered rules is not implemented.
- Cross-section dependencies on apply (e.g. a DHCP server referencing a pool that isn't synced) follow configured section order only; there is no dependency analysis.

## 11. Future (v2+)

- `mtha watch --pair <name>` daemon mode with webhook/notification on readiness change.
- DHCP lease sync (script-based or per-router container agent).
- Scheduled drift reports.
- Support for more than two routers (active/standby chains).

