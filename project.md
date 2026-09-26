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
3. **Apply** — planner, dry run, backup, apply, verify. `--write` gate.
4. **Runtime** — templates, deploy, verify, remove. Done — extended beyond the original §5.5 scope to also provision the VRRP interface(s) and VIP(s) themselves (`interface/vrrp`, `ip/address`), not only the automation layered on top of one.
5. **Failover** — pre-flight, action, live view, VIP probe.
6. **Events** — log merge, timeline.
7. **Polish** — multi-pair, help, release pipeline.

Milestones 1–2 deliver value on their own and de-risk the hardest part (normalisation).

## 10. Open questions

- Which sections need custom identity rules beyond `comment`/`name`? Firewall rules without comments are the known hard case.
- Should `ip/route` sync be opt-in per route (by comment tag) rather than whole-section with exemptions?
- Does the DHCP server on the standby stay enabled with a split scope, or get toggled by the VRRP script? Affects runtime template.
- Multi-pair from day one, or add after milestone 2?

## 11. Future (v2+)

- `mtha watch --pair <name>` daemon mode with webhook/notification on readiness change.
- DHCP lease sync (script-based or per-router container agent).
- Scheduled drift reports.
- Support for more than two routers (active/standby chains).

