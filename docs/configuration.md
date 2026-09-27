# Configuration

Everything `mtha` needs to know about a pair lives in one YAML file. The
default location is:

```
~/.config/mtha/pairs.yaml
```

Override it with `-config <path>`. Run `mtha -init` to write a commented
starter file to that location — that file is the most reliable reference,
because it is generated from the same source the loader uses.

**Credentials are never stored in this file.** They are read from the
environment at run time (see [Credentials](#credentials)).

## Full example

```yaml
pairs:
  - name: core
    routers:
      a: { host: 10.0.0.2, user: mtha, insecure_tls: false }
      b: { host: 10.0.0.3, port: 8443, user: mtha }
    vrrp:
      - interface: vrrp-lan
        on: ether2
        vrid: 1
        addresses: [10.0.0.1/24]
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
        - ip/route               # only routes commented "mtha:..."
        - ip/service
        - user
        - system/script
        - system/scheduler
      exempt:
        - system/identity
        - interface/vrrp.priority
        - ip/address             # per-router interface addresses
        - ip/service.certificate # each router's own self-signed cert
        - ip/service.port        # router b's REST API (www-ssl) is on 8443
        - user.last-logged-in    # updates independently on every login
        - ip/dhcp-server.disabled
        - ip/route.disabled
    runtime:
      netwatch_targets: [1.1.1.1, 8.8.8.8]
      priority_master: 200
      priority_backup: 100
      priority_degraded: 50
      toggles:
        vrrp: vrrp-lan
        dhcp_servers: [dhcp-lan]
        routes: [mtha-default]
```

## Top level

| Field | Type | Notes |
| --- | --- | --- |
| `pairs` | list of pairs | A pair file with no pairs is an error |

Multiple pairs may be defined. A file with more than one pair requires
`-pair <name>` on the command line; with exactly one pair the name is
inferred.

## Pair

| Field | Required | Notes |
| --- | --- | --- |
| `name` | yes | Used in the UI and in the credential environment variable name |
| `routers` | yes | Must define both keys `a` and `b` — the loader rejects the pair otherwise |
| `vrrp` | no | VRRP interfaces to track |
| `sync` | no | Which sections to compare, and which to ignore |
| `runtime` | no | Parameters for the VRRP interface and the netwatch/on-master/on-backup/scheduler automation the Runtime screen deploys |

### Router

```yaml
routers:
  a: { host: 10.0.0.2, user: mtha, insecure_tls: false }
  b: { host: 10.0.0.3, port: 8443, user: mtha }
```

| Field | Type | Default | Notes |
| --- | --- | --- | --- |
| `host` | string | — | IP address or hostname of the router |
| `port` | int | `443` | REST API (`www-ssl`) port. Set it only if that service has been moved off the standard HTTPS port. Must be 0–65535 |
| `user` | string | — | RouterOS user for API auth |
| `insecure_tls` | bool | `false` | Skip certificate verification for this router. Opt-in per router, for self-signed certificates — installing the router's CA is preferable |

`port` is unrelated to the legacy binary API service on 8728/8729, which
`mtha` does not speak.

### VRRP

```yaml
vrrp:
  - interface: vrrp-lan
    on: ether2
    vrid: 1
    addresses: [10.0.0.1/24]
  - interface: vrrp-wan
    on: ether1
    vrid: 2
    addresses: [203.0.113.1/29]
```

| Field | Required | Notes |
| --- | --- | --- |
| `interface` | yes | Name mtha gives the created VRRP interface; also how it's matched against what the routers report |
| `on` | for deploy | Physical interface the VRRP interface rides on (same name on both routers) |
| `vrid` | for deploy | VRRP virtual router ID |
| `addresses` | for deploy | VIP(s), CIDR notation, assigned to the VRRP interface on both routers |

Names the VRRP instances the pair should track. The dashboard lists the state
of every instance the routers actually report, and readiness requires exactly
one master per instance.

`on`, `vrid` and `addresses` are only required once you use the **Runtime**
screen (`3`) to provision the VRRP interface itself — the dashboard and drift
screens work without them, for pairs that already have VRRP configured by
hand. When runtime deploy creates the interface, router `a` starts at
`runtime.priority_master` and router `b` at `runtime.priority_backup` — a
fixed v1 convention, not something you choose per pair. Version, interval and
preemption-mode are fixed too (`3`, `1s`, `true`) rather than exposed as config.

### Sync

| Field | Type | Notes |
| --- | --- | --- |
| `sections` | list of REST paths | Config sections compared for drift |
| `exempt` | list of paths | Sections or individual fields excluded from comparison |

A `sections` entry is a REST path relative to `/rest`, e.g.
`ip/firewall/filter` for `GET /rest/ip/firewall/filter`. Sections are read
structurally over REST; `mtha` never parses `/export` text.

An `exempt` entry takes one of two forms:

| Form | Effect |
| --- | --- |
| `system/identity` | The **whole section** is skipped — it is not fetched and not diffed |
| `interface/vrrp.priority` | That **single field** is stripped from every entry of `interface/vrrp` before comparison |

Both forms may be mixed in the same list, and a section may be exempt while
still being relevant elsewhere (VRRP priority differs by design between
master and backup, so it must be exempted from drift even though VRRP state
is central to readiness).

Two field exemptions are worth adding to almost every pair, since they are
per-router state rather than config and will otherwise show up as permanent,
unresolvable drift: `ip/service.certificate` (each router holds its own
self-signed certificate for `www-ssl` unless you've deliberately installed a
shared one) and `user.last-logged-in` (updates independently every time
either router is logged into). If you sync `ip/service` and one router's
REST API is on a non-standard `port`, also exempt `ip/service.port`, as the
sample does: Apply refuses to change the target's `www-ssl` port, disabled
flag or address list (it would cut mtha off mid-apply), so that drift could
otherwise never be resolved.

### Runtime

| Field | Type | Notes |
| --- | --- | --- |
| `netwatch_targets` | list of addresses | Targets the deployed netwatch entries probe |
| `priority_master` | int | VRRP priority a healthy master holds |
| `priority_backup` | int | VRRP priority a healthy standby holds |
| `priority_degraded` | int | Priority the up/down scripts drop a router to when its targets are unreachable |

`priority_master`/`priority_backup` are the base VRRP priority of routers
`a`/`b`: what the Runtime screen creates a VRRP interface with.
`priority_degraded` and `netwatch_targets` parameterize the netwatch up/down
scripts deployed alongside it (see [Runtime screen](usage.md#runtime)):

- a target going down drops that router's priority to `priority_degraded`;
- a target coming back up restores the router's **own** base priority
  (`priority_master` on `a`, `priority_backup` on `b`), and only once every
  mtha netwatch entry on that router is up again.

The scripts only change VRRP interfaces mtha manages (comment
`mtha:vrrp:<name>`). Because netwatch (and, later, planned failover) moves
priority on purpose, deploy never resets the priority of a VRRP interface
that already exists, and verify accepts either the base or the degraded
priority as `ok`.

| Field | Type | Notes |
| --- | --- | --- |
| `toggles` | map | Optional. What the VRRP on-master/on-backup scripts switch on a transition — see below |

#### Transition toggles

```yaml
runtime:
  toggles:
    vrrp: vrrp-lan             # the instance whose transitions drive this
    dhcp_servers: [dhcp-lan]   # /ip/dhcp-server entries, by name
    routes: [mtha-default]     # /ip/route entries, by comment
```

| Field | Type | Notes |
| --- | --- | --- |
| `vrrp` | string | Required once either list is set. Must name a `vrrp` entry that has `on`/`vrid`/`addresses` set, since only those get deployed scripts |
| `dhcp_servers` | list of names | DHCP servers enabled on master, disabled on backup |
| `routes` | list of comments | Routes enabled on master, disabled on backup — typically the default route out of an uplink only the master should use |

Without `toggles`, the on-master/on-backup scripts only log the transition.
With it, the named instance's scripts also switch the listed objects, the
same way the netwatch scripts raise and lower VRRP priority:

- **on-master** enables the listed routes, then the DHCP servers — so
  routing is in place before the first lease is offered.
- **on-backup** disables the DHCP servers, then the routes.

Each line is a `find`-and-`set`, so a name or comment that matches nothing on
a router is a silent no-op rather than a script error. Check the objects
exist on both routers with exactly those names/comments.

The toggles ride on **one** instance deliberately. With several VRRP
instances, mastership can split across routers (say `vrrp-lan` master on A,
`vrrp-wan` master on B); if every instance's scripts toggled DHCP, both
routers could end up serving. Pick the instance whose VIP clients actually
use as their gateway — usually the LAN one. The generated scripts keep the
`# mtha:on-master:<name>` / `# mtha:on-backup:<name>` first line, so adding,
changing or removing `toggles` later updates mtha's own scripts in place on
the next deploy (and shows as `mismatched` until then). A hand-written
script without that marker is still reported as `conflict` and left alone.

**DHCP leases are not replicated.** Lease state is local to each router, and
mtha does not sync it (see project.md §3.2). The newly-promoted master
starts with no lease history, so after a cutover clients re-DISCOVER when
they next renew or rebind:

- With **static leases** kept in sync (`ip/dhcp-server/lease` in `sync`),
  this is harmless — each client gets its reserved address back.
- With a **dynamic pool**, the new master may hand a client a different
  address than it had, and may offer an address the old master had leased
  to someone else who hasn't renewed yet. The DHCP server's
  `conflict-detection` setting reduces but does not remove that risk. If
  stable addresses matter, use static leases.

**The standby's DHCP server must be disabled at rest.** The scripts only run
on a VRRP transition. If both routers start with the server enabled — for
example because it was configured by hand before mtha was — both will offer
leases until the standby next transitions to backup. Disable it on the
standby yourself after the first deploy (or bounce the standby's VRRP
interface so on-backup fires), and keep it that way.

Because the toggled objects are deliberately enabled on one router and
disabled on the other, their `disabled` field will always differ. If
`ip/dhcp-server` or `ip/route` is in `sync.sections`, add
`ip/dhcp-server.disabled` and `ip/route.disabled` to `exempt`, or drift
will never be clean. That exemption applies to every entry in the section,
so a route disabled by hand on only one router will no longer show as drift.

## Credentials

One environment variable per router:

```
MTHA_<PAIR>_<ROUTER>_PASSWORD
```

`<PAIR>` and `<ROUTER>` are upper-cased. For pair `core`, router `a`:

```sh
export MTHA_CORE_A_PASSWORD=...
export MTHA_CORE_B_PASSWORD=...
```

The environment is the only source: there is no credential field in the pair
file, and no keychain integration yet. If the variable is unset or empty,
`mtha` refuses to start and names the variable it wanted.

Storing these in a shell profile, a secrets manager, or a keychain-injected
environment is all fine. Because there is nowhere in the pair file to put
them, the file stays safe to commit or share.

## How drift is computed

This matters when tuning `sync`, because the comparison is deliberately not a
literal config diff.

**Dropped before comparison:**

- the router-assigned `.id` field
- any entry marked `dynamic: true`
- in `ip/route`, any route whose comment does not start with `mtha:` —
  route sync is opt-in (see below)
- read-only runtime state RouterOS reports alongside config: firewall
  `bytes`/`packets` counters, `invalid`, `running`, script and scheduler
  `run-count`/`next-run`/`owner`, lease `status`/`last-seen`, route
  `active`/`inactive`, and similar (the full list is `sectionStateFields` in
  `internal/model/normalize.go`)
- any field named in `exempt` as `<section>.<field>`

**Treated as equal:**

- a field absent on one router and present with a default-ish value on the
  other (`""`, `false`, `no`, `none`, `0`). RouterOS omits fields left at
  their default rather than writing them explicitly, so this prevents
  constant false positives.

**Entries are matched across the two routers by identity**, per section:

| Section | Identity |
| --- | --- |
| any | the `comment` field, if present and non-empty; a repeated comment becomes `web`, `web#2`, `web#3`, ... in list order |
| `ip/firewall/filter`, `nat`, `mangle`, `raw` | `chain` plus an ordinal counted from the preceding commented rule in that chain: `input@allow-ssh#2`, or `input#2` before the chain's first commented rule |
| `ip/firewall/address-list` | `list` + `address` |
| `ip/dns/static` | `address` |
| `ip/route` | `dst-address` → `gateway` |
| anything else | `name`, else `address`, else a bare ordinal |

Practical consequences:

- **Commenting your rules is the single highest-value habit here.** A
  commented rule is matched reliably. An uncommented firewall rule is matched
  by its position after the nearest commented rule above it in the same
  chain, so inserting a rule only re-identifies the uncommented rules between
  it and the next commented rule — every commented rule acts as a fixed
  anchor that stops the cascade.
- **Routes are opt-in.** Only routes you tag with a comment starting
  `mtha:` (e.g. `mtha:vpn-site-b`) are compared or synced; everything else in
  `ip/route` — connected routes, each router's own default route, anything
  per-router — is ignored entirely, so it can never show as drift or be
  overwritten by an apply. A tagged route is identified by that comment.
- Two entries on one router that resolve to the same identity are paired by
  encounter order rather than one overwriting the other.
- Section order in the drift output follows your `sections` list, and hunks
  follow router A's order, so results are stable between runs.

`mtha` reports *what* differs. Which side is the source of truth for a given
hunk is a decision made at apply time, not by the diff engine: you choose a
direction per hunk or per section on the Drift screen and review the
resulting plan on the Apply screen (see [usage.md](usage.md#apply)).
