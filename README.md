# mtha — MikroTik HA Pair Manager

`mtha` is an on-demand operator TUI for a pair of MikroTik RouterOS 7 routers
running VRRP. It answers the questions a two-router HA setup can't answer on
its own:

- Is the pair actually healthy, and could the standby take over right now?
- What configuration has drifted between the two routers?
- Push selected config differences in either direction, with a dry run,
  a pre-apply backup and post-apply verification.
- Deploy the runtime failover logic, and (later milestone) rehearse a
  planned failover.

It is `kubectl`, not a control plane. Nothing that happens *during* a failure
depends on `mtha` running — VRRP, netwatch and the scripts that adjust
priority all live on the routers themselves. The tool is for designing,
deploying, verifying, syncing and rehearsing.

> **Status: early.** Milestones 1–4 and 6 are built: Overview, Drift, Runtime
> (VRRP interface provisioning, netwatch/on-master/on-backup/scheduler
> automation), Apply (selective sync with dry run, backup and verification),
> and Events (merged router log and tool-action timeline). Planned failover is
> the remaining milestone. See [Status](#status) below.

## Requirements

- Go 1.27.1 (the version pinned in `go.mod` and `.mise.toml`)
- Two RouterOS 7 routers with the REST API (`www-ssl`) reachable over HTTPS
- A user on each router with read permissions; the API user needs
  `read` at minimum, and `write`/`policy` to use `-write` (required for the
  Runtime screen's deploy/remove and the Apply screen)

`mtha` speaks the RouterOS 7 REST API only. It does **not** speak the legacy
binary API on ports 8728/8729.

## Install

```sh
git clone <repo> && cd mikrotik-ha-manager
go build ./cmd/mtha        # or: make build
```

This produces an `mtha` binary in the current directory. The build is a
static binary with no runtime dependencies; cross-compiling is a plain
`GOOS`/`GOARCH` pair.

## Quickstart

```sh
# 1. Write a commented starter pair file to the default location
mtha -init

# 2. Set one password per router — never stored in the pair file
export MTHA_CORE_A_PASSWORD=...
export MTHA_CORE_B_PASSWORD=...

# 3. Edit the pair file to match your routers
$EDITOR ~/.config/mtha/pairs.yaml

# 4. Run it (read-only by default)
mtha
```

The session opens on the **Overview** screen: side-by-side status for both
routers plus a single readiness verdict. Press `2` for **Drift**, `3` for
**Runtime**, `4` for **Apply**, `6` for **Events**, or `?` for help on any
screen: select hunks on Drift (`space`, or `a`/`b` for a whole section), then
review and run the plan on Apply.

See [docs/usage.md](docs/usage.md) for screens and keybindings, and
[docs/configuration.md](docs/configuration.md) for the pair file reference.

## Safety model

- **Read-only unless you ask.** Write operations require `-write`; the mode is
  shown in the status bar. The Runtime screen's deploy/remove actions always
  show what they'd do first, even read-only — only confirming with `-write`
  set actually writes anything.
- Credentials are resolved from the environment at run time and never read
  from, or written to, the pair file.
- All API traffic is HTTPS. `insecure_tls` is an explicit per-router opt-in
  for self-signed router certificates — prefer installing the router's CA
  certificate instead where you can.
- Every object Runtime deploys is tagged (`mtha:` comments, or a leading
  `# mtha:` line in a script body) so it can be verified and removed cleanly,
  and so it never silently overwrites a hand-written on-master/on-backup
  script — that surfaces as `conflict` instead. Removing a VRRP interface
  currently holding master is flagged before you confirm.
- Apply shows every REST operation before anything is written, starts each
  router's writes with `/system/backup/save`, and re-checks the plan against
  fresh reads just before running — if the routers changed, nothing is
  written and the new plan is shown instead. Writing to the current VRRP
  master (or a router whose VRRP state is unknown) takes a second, distinct
  confirmation (`Y`). Execution stops at the first failure, and drift is
  re-run afterwards to report anything still different. Failover will reuse
  the same pattern.

## Status

| Milestone | Scope | State |
| --- | --- | --- |
| 1. Skeleton | Pair config, REST client, dashboard with basic status and VRRP | Done |
| 2. Drift | Section readers, normaliser, diff engine, drift screen | Done |
| 3. Apply | Planner, dry run, backup, apply, verify | Done |
| 4. Runtime | Templates, deploy, verify, remove | Done |
| 5. Failover | Pre-flight, action, live view, VIP probe | Not started |
| 6. Events | Log merge, timeline | Done |
| 7. Polish | In-app help | Done |

Multi-pair (a pair picker screen) and a GoReleaser release pipeline are
dropped from scope (project.md §10.1). A pair file may still define several
pairs; pick one with `-pair <name>`. Builds are plain `go build`.

## Documentation

| Document | Contents |
| --- | --- |
| [docs/configuration.md](docs/configuration.md) | Pair file reference, credentials, drift and normalisation rules |
| [docs/usage.md](docs/usage.md) | Command-line flags, screens, keybindings, readiness checks |
| [project.md](project.md) | Full product spec, architecture and milestones |

## Development

```sh
make check     # vet + tests with the race detector — the pre-commit gate
make test      # go test ./...
make vet       # go vet ./...
make cover     # coverage summary
make build     # go build ./cmd/mtha
```

CI runs `go mod verify`, `go vet ./...` and `go test -race ./...` on every
push to `main` and every pull request, so `make check` is the same gate you
should pass locally.

The normaliser (`internal/model`), the diff engine (`internal/diff`) and the
planner (`internal/plan`) are the critical units; changes there should come
with test cases. The planner's dry-run output is pinned by golden files in
`internal/plan/testdata` — after an intentional change, regenerate them with
`go test ./internal/plan -update` and review the diff.
