# Lab REST contract

What the two lab routers (`testlab/`, RouterOS **7.23.7 long-term**, CHR)
actually return over `GET /rest/...`. This page records **shape, not values**:
field names, types, which fields come and go, and `.id` forms. It is the
ground truth that the fixtures in `internal/**/_test.go` should match.

It was first surveyed read-only (GET only) on 2026-09-27, against a lab
holding only the VRRP pair, and re-surveyed the same day against the
**populated lab**: the fixture `testlab/provision.sh` now adds to every
synced section (see [Second survey](#second-survey-populated-lab)). A third
pass on 2026-09-28 probed the write side the design questions needed
(record shapes, uniqueness, `move`, references; see
[Write probes](#write-probes-design-questions)). Another
agent may write to the same lab at any time, so any value seen may be
transient. Values quoted here are illustrative only.

## How it was gathered

- `curl -k -u admin:... https://localhost:443/rest/<section>` (router A) and
  `:8443` (router B), for every section below, repeated every 10 s for about
  40 minutes. A copy was kept whenever a response changed.
- Error behaviour was learned from GETs against bad paths and IDs, and from
  one GET with a wrong password.
- In the first survey no `PUT`/`PATCH`/`POST`/`DELETE` was sent. One side effect: the
  wrong-password GET wrote a `system,error,critical` "login failure" line
  to router A's log.

**Coverage caveat (first survey).** The lab then held only an address on
`ether2`, a `vrrp-lan` instance and its VIP. `ip/route`, `ip/service`,
`user` and `interface/vrrp` had entries; thirteen other sections returned
`[]` on both routers (the list said "twelve" but named thirteen). The
second survey covers all of them.

### Second survey (populated lab)

- The fixture is `populate_fixture` in `testlab/provision.sh`, applied
  identically to both routers and captured in the harness's golden backup.
  It covers every section this page describes except dynamic DHCP leases
  (below). `internal/labtest` holds each router to the fixture's entry
  counts, and `TestLabBaselineIsDriftFree` runs the real `diff.Compare`
  over every section `testlab/pairs.yaml` syncs: **the pair is drift-free
  at baseline in all twelve.**
- The same GET-every-section sweep as the first survey, on both routers,
  after `make test-lab` had restored the baseline.
- Shapes that need a write (a disabled VRRP instance, a disabled or
  unreachable route, an address-list entry with a timeout, a script after
  a run, a firewall rule written with explicit defaults, thousands of
  rules) were probed from throwaway lab tests inside the harness, so each
  was undone by a golden-backup restore. The two that answer an open
  question are now permanent tests: `TestLabDisabledVRRPHasNoRoleFlags`
  and `TestLabLargeSectionComesBackWhole` (`internal/labtest`).
- **Third pass (write probes), 2026-09-28.** Throwaway lab tests on router
  A of instance 1, inside the harness, each undone by its golden-backup
  restore; the instance was then checked back at baseline
  (`TestLabBaselineIsDriftFree`, and the fixture's entry counts on both
  routers). Results are in [Write probes](#write-probes-design-questions).
- **Still not populated: a dynamic (client-bound) DHCP lease.** The lab has
  no DHCP client on `ether2`, and a hand-made lease would be static, which
  the fixture already has. Nothing else is left empty.

## Rules that hold across every section

| Rule | Observed |
|---|---|
| Value types | **Every value is a JSON string**, with no exceptions in any response surveyed. That includes numbers (`"200"`, `"1500"`, `"8291"`), booleans (`"true"`/`"false"`), durations (`"1s"`, `"10m"`, `"24m26s"`) and timestamps (`"YYYY-MM-DD HH:MM:SS"`). No JSON `true`, number or `null` ever appears. |
| Boolean spelling | Always `"true"`/`"false"` for booleans, never `"yes"`/`"no"`. `yes`/`no` are console and script spellings, not REST ones. The one `"no"` seen is `ip/dhcp-server` `use-radius`, which is an enum (`yes`/`no`/`accounting`), not a boolean. Writes accept `"yes"`/`"no"` and read back as `"true"`/`"false"`. |
| Default-valued fields | **Usually returned explicitly** (`"disabled":"false"`, `"invalid":"false"`, `"address":""`, `"on-master":""`, `"password":""`). **Exceptions:** `disabled` is absent at default on firewall rules, static routes and netwatch entries, as are `log` and `log-prefix` on firewall rules. On firewall rules, whether those keys appear depends on how the rule was written, not on its value (see [firewall](#ipfirewallfilter-nat-mangle-raw)). |
| Conditional fields | Some fields are **absent** rather than `"false"`. They are status flags that appear only when set (`master`, `backup`, `ecmp`, `connect`, `dhcp`, `static`), properties that only exist for some kinds of entry (see each section), and state that only exists once something has happened (`last-started`, `last-logged-in`, netwatch `since`). |
| `comment` | Omitted when there is no comment, in every section, never `""`. Present as a plain string otherwise; two entries may carry the same one. |
| `.id` | A string `*<hex>`, e.g. `*1`, `*E`, `*20183040`, `*80000001`. The size of the number says where the entry came from (see below). |
| Collection vs item | `GET /rest/<section>` returns a JSON array (`[]` when empty). `GET /rest/<section>/<id>` returns a single object. |
| Lookup by name | `GET /rest/<section>/<name>` works as well as `/<id>` in sections with a `name` (`/rest/user/admin`, `/rest/ip/service/www-ssl`). In a section without one it is **400** `"no such command prefix"` (`/rest/ip/firewall/filter/<comment>`); filter with `?comment=` instead. |
| Query filters | `?field=value` filters (`?master=true`, `?name=www-ssl`, `?dynamic=false`) and returns an array. `?.proplist=a,b` limits fields but **always includes `.id`**. |
| Singletons | `system/resource`, `system/identity` and `system/clock` return a bare object, not an array. |

### `.id` shapes

| Shape | Seen on |
|---|---|
| `*0`, `*1` ... `*F`, `*1A` | Static config in every section surveyed, and dynamic `ip/service` connection rows. `system/scheduler`'s first entry is `*0`. |
| `*2018xxxx` (high 32-bit value) | Dynamic connected routes (`ip/route` with `connect`) |
| `*8000xxxx` | **Every non-connected route**: static routes and the DHCP-client default route share one counter. The DHCP route was `*80000001` before the fixture existed, and `*80000004` after a restore, once the fixture's static routes held `*80000002`/`*80000003`. |

`.id` values are not stable enough to key on:

- The dynamic `reverse-proxy` rows in `ip/service` get a new `.id` on nearly
  every REST request, from a steadily increasing counter.
- The same object has **different** `.id`s on A and B only by accident.
  `vrrp-lan` happened to be `*4` on both routers.
- A `*0` ID exists (`ip/service` `telnet`, the first log line), so code
  must not treat `*0` as "unset".

## Per section

Legend: **S** = always present on this kind of entry. **C** = conditional.
"state" = runtime or read-only, not configuration.

### `interface/vrrp`

Fields on a static instance (both routers):

`.id` S · `name` S · `interface` S · `vrid` S · `priority` S · `interval` S
· `version` S · `v3-protocol` S · `preemption-mode` S · `authentication` S
· `password` S · `arp` S · `arp-timeout` S · `mtu` S · `mac-address` S ·
`group-authority` S · `connection-tracking-mode` S ·
`sync-connection-tracking` S · `on-master` S · `on-backup` S · `on-fail` S
· `disabled` S · `invalid` S (state) · `running` S (state) · **`master` C**
· **`backup` C**

- **Role flags.** On the master, `"master":"true"` is present and there is
  **no `backup` key**. On the backup, `"backup":"true"` is present and there
  is **no `master` key**. Neither flag was ever seen with the value
  `"false"`.
- **A disabled instance carries neither flag**, and also has **no
  `invalid` key**; `running` is `"false"`, `disabled` `"true"`, and every
  configuration field is still returned. `Role()` gives `RoleUnknown` for
  it (`TestLabDisabledVRRPHasNoRoleFlags`). The peer stays `master:"true"`.
- **Priority 255 cannot be configured** on 7.23: a `PUT` with
  `priority=255` is `400 "value of priority out of range (1..254)"`, so the
  "address owner" state does not arise from configuration. `init` was not
  observed (it is too brief to catch by polling).
- **`vrrp-state` is never returned.** No such field exists.
- **`running` is not a role signal.** It is `"true"` on the master and
  `"false"` on the backup, because a backup's VRRP interface is not
  running. The same state appears in `/interface` (`running`) and on the VIP
  in `/ip/address` (`invalid:"true"` on the backup).
- `on-master`/`on-backup`/`on-fail` are returned as `""` when unset, not
  omitted. A multi-line script comes back as one JSON string with
  embedded `\n` (verified on `system/script` `source`, the same kind of
  property).
- `password` is returned in the clear (empty in the lab). Unlike a user
  password, a VRRP password **is** readable over REST.
- The instance has **no `address` field**, and RouterOS rejects one on write
  (`400 unknown parameter address`). The VIP is a separate `ip/address` row
  whose `interface` is the VRRP interface's name.

### `ip/address` (read by Runtime, not a sync section)

`.id` S · `address` S · `network` S (state) · `interface` S ·
`actual-interface` S (state) · `vrf` S · `disabled` S · `dynamic` S · `invalid`
S (state) · `slave` S (state) · `comment` C

- `dynamic`, `invalid` and `slave` are always present as `"true"`/`"false"`.
- **On the backup router, the VIP row on `vrrp-lan` is `invalid:"true"`.**
  It is still there and still `disabled:"false"`. This is normal VRRP
  behaviour, not a fault.
- The DHCP-client address on `ether1` is `dynamic:"true"`.

### `ip/route`

Static route (the fixture's `mtha:lab-route`, and an untagged one; the same
shape): `.id` · `dst-address` · `gateway` · `immediate-gw` (state) ·
`distance` · `scope` · `target-scope` · `routing-table` · `dynamic`
(`"false"`) · `static` (`"true"`, state) · `active` (state) · `inactive`
(state) · `ecmp` C (state) · `comment` C

- There is **no `disabled` key** on an enabled static route, and **no
  `suppress-hw-offload`, `vrf-interface`, `pref-src`, `check-gateway` or
  `blackhole`** unless set. `local-address` never appears on a static route.
- **The same route differs between the pair at rest.** On the master,
  a route whose gateway is on the VIP's subnet has `"ecmp":"true"` and
  `immediate-gw` lists two paths (`<gw>%ether2,<gw>%vrrp-lan`); on the
  backup there is no `ecmp` and one path. Both fields are state and must
  be stripped (they are).
- **A disabled static route is a much smaller object:** `.id`,
  `dst-address`, `gateway`, `disabled:"true"`, `inactive:"false"`,
  `static:"true"`, `comment` C. It has **no `dynamic` key** (so
  `?dynamic=false` does not return it), and no `distance`,
  `routing-table`, `scope`, `target-scope`, `active` or `immediate-gw`.
- An enabled route whose gateway is unreachable has `inactive:"true"`, **no
  `active` key**, and `immediate-gw:""`.
- A blackhole route has `blackhole:""` (an empty-string flag, not
  `"true"`), no `gateway`, no `scope`/`target-scope`, and
  `immediate-gw:""`.

Dynamic connected route: `.id` · `dst-address` · `gateway` · `immediate-gw` ·
`local-address` · `distance` · `scope` · `target-scope` · `routing-table` ·
`dynamic` · `active` · `inactive` · `connect` C · `ecmp` C

Dynamic DHCP route: the same, minus `connect` and `local-address`, plus
`dhcp` C and `vrf-interface` C.

- `dynamic`, `active` and `inactive` are always present as strings.
  `connect`, `dhcp` and `ecmp` appear **only when true**.
- `immediate-gw` and `local-address` carry an `%interface` suffix
  (`<addr>%ether2`).
- The connected route for the VIP subnet via `vrrp-lan` exists **only on the
  master**. The backup has one route fewer, and on the backup the
  `ether2` connected route is not `ecmp`. This is expected, but it means
  route *counts* differ between the pair at rest.

### `ip/service`

Static rows: `.id` · `name` · `port` · `proto` · `address` · `disabled` ·
`dynamic` · `invalid` · `max-sessions` · `vrf`. TLS services
(`www-ssl`, `api-ssl`, `reverse-proxy`) also have `certificate` and
`tls-version`.

Dynamic rows (`dynamic:"true"`): `.id` · `name` · `port` · `proto` ·
`disabled` · `invalid`. They have **no** `address`, `max-sessions` or `vrf`.

- **The section is not a fixed built-in list.** Alongside the static
  services it returns dynamic rows: `dhcp` (only while a DHCP server is
  configured), `dhcpclient`, `btest` and `discover`. It also returns **one
  dynamic row per open HTTPS connection**, carrying `connection:"true"`,
  `local` and `remote:"<ip>:<ephemeral port>"`. **The REST client's own
  request shows up in the response it is reading.** Several can appear at
  once (seven during a `make test-lab` run). Their `.id` and `remote`
  change on every request.
- 7.23 has a **static `reverse-proxy` service on port 443**, the same port
  as `www-ssl`.
- **Which name the connection rows carry changed between the surveys.** In
  the first survey they were `reverse-proxy`. In the second, on both
  routers and across several reboots, they were **`www-ssl`**, and no
  dynamic `reverse-proxy` row appeared. What decides it was not found (the
  lab was rebooted by golden-backup restores between the surveys; nothing
  in `ip/service` itself differed). Either way a service name appears
  **more than once** in one response: once static and once or more
  dynamic.
- `certificate` is `"none"` when unset on a TLS service, and absent on
  non-TLS services.
- **There is no `comment` on `ip/service`.** It is never returned, it is
  not among `set`'s arguments (`/console/inspect`), and a `PATCH` of it is
  `400 "unknown parameter comment"`. See
  [write probe 3](#3-ipservice-and-comment).
- Inside the lab VMs, `www-ssl` is on port 443 on **both** routers. Router
  B's `8443` is a host port-forward, so the device's `port` does not differ
  between A and B.

### `user`

`.id` · `name` · `group` · `address` · `disabled` · `expired` (state) ·
`inactivity-policy` · `inactivity-timeout` · `last-logged-in` (state) ·
`comment` C

- **`password` is never returned**, not even when asked for by name with
  `?.proplist=password` (that returns just `{".id":...}`).
- `inactivity-policy` and `inactivity-timeout` are RouterOS 7 fields that
  hold configuration, so they will take part in drift.
- `last-logged-in` is **absent** on a user who has never logged in (the
  fixture's `lab-ro`), not `""`.
- `last-logged-in` did **not** move during the survey, despite hundreds of
  authenticated REST GETs. REST basic-auth requests do not update it. The
  value it held was set by the image's bootstrap, which used the binary
  API.

### `system/resource`, `system/identity`, `system/clock`, `log`

- `system/resource.version` includes the channel: `"7.23.7 (long-term)"`.
  All other fields are strings as well, including `cpu-load` and memory
  sizes.
- `system/identity` is `{"name": ...}`. **Both lab routers report the
  same identity** (`CHR`, the CHR default).
- `system/clock`: `time` (`HH:MM:SS`), `date` (`YYYY-MM-DD`), `gmt-offset`
  (`+HH:MM`), `time-zone-name`, `time-zone-autodetect`, `dst-active`.
- `log` entries: `.id`, `time` (`"YYYY-MM-DD HH:MM:SS"`), `topics`
  (comma-separated), `message` and **`extra-info`**.
  - Account lines carry key/value metadata such as `app=`, `duser=` and
    `outcome=`, and that metadata is in `extra-info`, not in `message`.
  - VRRP transitions log as topics `vrrp,info` with the message
    `"<name> now MASTER, ..."` or `"<name> now BACKUP"`.
  - REST requests do **not** log one login line each. Hundreds of GETs
    during the survey produced a single `logged in ... via rest-api` line,
    so logins appear to be logged once per HTTP keep-alive connection or
    session.

### `ip/firewall/filter`, `nat`, `mangle`, `raw`

The four tables share one shape. Rule (fixture, both routers):

`.id` S · `chain` S · `action` S · `bytes` S (state) · `packets` S (state) ·
`dynamic` S · `invalid` S (state) · the rule's own matchers and action
parameters C (`protocol`, `dst-port`, `in-interface`, `out-interface`,
`connection-state`, `src-address-list`, `to-addresses`, `to-ports`,
`new-connection-mark`, `passthrough`, ...) · `disabled` C · `log` C ·
`log-prefix` C · `comment` C

- **Matchers and parameters appear only when set.** An unset matcher is
  absent, never `""`.
- **`disabled`, `log` and `log-prefix` are present or absent depending on
  how the rule was written, not on their value:**
  - a rule added without them has no `disabled`, `log` or `log-prefix` key;
  - a rule added with `disabled=no` returns `"disabled":"false"`, and one
    added with `log=no log-prefix=""` returns `"log":"false"` and
    `"log-prefix":""`;
  - a rule whose `disabled` was ever `PATCH`ed keeps `"disabled":"false"`
    after being re-enabled.
  So two routers holding the same rule can differ in which of these keys
  they send. A disabled rule always has `"disabled":"true"`.
- `bytes`/`packets` are present on every rule, including a disabled one
  (`"0"`).
- Values keep the console's list form: `connection-state` is
  `"established,related"`.
- `passthrough` (mangle) is returned as `"true"` when set to `yes`.
- `action` is always present. `chain` accepts any name (the large-section
  test adds rules to an unreferenced `lab-bulk` chain).
- **`invalid` is not a usable signal on rules.** In a fresh unreferenced
  chain the first rule read `invalid:"false"` and every later identical
  `passthrough` rule `invalid:"true"`; a rule with an unknown `jump-target`
  or `src-address-list` was `"true"` as well. It is state and is stripped.
- Order is changed only by the `move` command
  ([write probe 4](#4-the-rest-move-command)); `place-before` is
  create-only (`PATCH` with it is `400 "unknown parameter place-before"`).
- The fixture's commented/uncommented layout (uncommented rules before each
  chain's first commented rule, two rules sharing `lab: block smb`, a
  disabled rule, a logging rule) produces identities that match between
  the routers: the baseline diff is clean.

### `ip/firewall/address-list`

`.id` · `list` · `address` · `creation-time` (state, `"YYYY-MM-DD
HH:MM:SS"`) · `disabled` · `dynamic` · `comment` C · `timeout` C

- `creation-time` is always present. `disabled` is present at default here,
  unlike the firewall tables.
- The same `address` in two `list`s is two independent entries.
- **An entry added with a `timeout` is `dynamic:"true"`** and carries
  `timeout` (the configured value, e.g. `"1h"`). A static entry has no
  `timeout` key. `model.Select` therefore drops every timed entry.

### `ip/dns/static`

`.id` · `name` · `type` · `ttl` · `disabled` · `dynamic` · `address` C ·
`cname` C · `comment` C

- `type` is always present (`"A"` by default, `"CNAME"`, ...), and `ttl`
  is always present (`"1d"` by default).
- The value lives in a **per-type field**, and only that type's fields are
  returned (table in [write probe 1](#1-dns-static-record-shapes)): `A` and
  `AAAA` have `address`, `CNAME` `cname`, `MX` `mx-exchange` and
  `mx-preference`, `SRV` `srv-target`, `srv-port`, `srv-priority` and
  `srv-weight`, `TXT` `text`, `NS` `ns`, `FWD` `forward-to`, and
  `NXDOMAIN` none.
- A regexp record has **`regexp` instead of `name`** (no `name` key); a
  record cannot have both.
- One `name` with two addresses is two entries with the same `name`. The
  device refuses a second **enabled** record with the same name, type and
  value ([write probe 2](#2-uniqueness-routeros-enforces)).

### `ip/pool`

`.id` · `name` · `ranges` · `total` (state) · `used` (state) · `available`
(state)

- `total`, `used` and `available` are counters (`"100"`, `"0"`, `"100"`).
  `next-pool` and `comment` are absent unless set.

### `ip/dhcp-server`

`.id` · `name` · `interface` · `address-pool` · `lease-time` · `lease-script`
· `address-lists` · `add-dns-entries-suffix` · `dynamic-lease-identifiers`
· `support-broadband-tr101` · `use-radius` · `use-reconfigure` ·
`disabled` · `dynamic` · `invalid` (state) · `comment` C

- `use-radius` is `"no"` (an enum, see the rules table).
- If its pool is deleted, `address-pool` reads back as the dead pool's
  `.id` (`"*1"`), not its name.
- `add-dns-entries-suffix` defaults to `"lan"`, and
  `dynamic-lease-identifiers` to `"client-mac,client-id"`: RouterOS 7
  fields, returned explicitly.

### `ip/dhcp-server/network`

`.id` · `address` · `gateway` · `dns-server` · `ntp-server` · `wins-server`
· `caps-manager` · `dhcp-option` · `dynamic` · `comment` C

- No `disabled` key: a network cannot be disabled.
- Unset list properties are `""`.

### `ip/dhcp-server/lease` (static lease only)

`.id` · `address` · `mac-address` · `server` · `address-lists` ·
`dhcp-option` · `agent-circuit-id` · `agent-remote-id` ·
`active-agent-circuit-id` (state) · `active-agent-remote-id` (state) ·
`status` (state) · `last-seen` (state) · `blocked` (state) · `radius`
(state) · `disabled` · `dynamic` · `comment` C

- A static lease that has never been bound has `status:"waiting"` and
  `last-seen:"never"` (a word, not a timestamp), and **no
  `active-address`, `active-mac-address`, `active-client-id`,
  `active-server`, `host-name` or `expires-after` keys**.
- `active-agent-circuit-id`/`active-agent-remote-id` are present and `""`
  even so.
- **`server` is absent when it is `all`**, not `"all"`, and `?server=all`
  matches nothing. When the named server is deleted, `server` reads back as
  the dead server's `.id` (`"*1"`) and `active-server:"*FFFFFFFF"` appears
  ([write probe 5](#5-hard-and-soft-references)).
- **A bound or dynamic lease was not observed**: the lab has no DHCP client
  on `ether2`. Its `status` values and `active-*` fields remain unsurveyed.

### `system/script`

`.id` · `name` · `source` · `policy` · `owner` (state) · `run-count`
(state) · `dont-require-permissions` · `invalid` (state) · `comment` C ·
`last-started` C (state)

- `dont-require-permissions` defaults to `"false"`.
- `policy` is one comma-separated string (`"ftp,reboot,read,..."`).
- `source` is one JSON string with embedded `\n` and `\"`.
- `last-started` is **absent until the script first runs**, then
  `"YYYY-MM-DD HH:MM:SS"`. `run-count` counts from `"0"`.
- `owner` is the user that created the script.

### `system/scheduler`

`.id` · `name` · `on-event` · `interval` · `start-date` · `start-time` ·
`next-run` (state) · `policy` · `owner` (state) · `run-count` (state) ·
`disabled` · `comment` C

- Formats: `start-date` `"YYYY-MM-DD"`, `start-time` `"HH:MM:SS"`,
  `next-run` `"YYYY-MM-DD HH:MM:SS"`, `interval` a duration (`"1d"`).
- **`start-date` defaults to the day the job was created**, and is
  configuration, not state (see contradiction 12).
- There is no `dynamic` key.

### `tool/netwatch`

`.id` · `host` · `type` · `interval` · `status` (state) · `comment` C ·
`since` C (state) · `done-tests` C (state) · `failed-tests` C (state) ·
`.about` C

- `type` defaults to `"simple"` and is returned. **There is no `disabled`
  key** on an enabled entry.
- `status` is **`"unknown"` for the first five minutes after boot**, with
  a `.about` warning (`"Warning: probe waiting startup-delay=5m; ...
  remaining"`) and no `since` or counters. Then it is `"up"` or `"down"`,
  and `since` (`"YYYY-MM-DD HH:MM:SS"`), `done-tests` and `failed-tests`
  appear. All three are state and would need stripping if netwatch were
  ever synced.

## Write probes (design questions)

The five probes `docs/design-questions.md` lists under "Needs the lab",
run on 2026-09-28 against router A of lab instance 1 (see
[How it was gathered](#how-it-was-gathered)). Each result below is the
device's own response: status, and the `detail` of a 400, verbatim.

### 1. DNS static record shapes

Each record was created with `PUT /rest/ip/dns/static` and read back. The
type's value fields are the only ones returned. `ttl` (`"1d"`), `type`,
`disabled` and `dynamic` are always present, as before.

| `type` | Value field(s) returned | Created with |
|---|---|---|
| `A` (default) | `address` | `address` |
| `AAAA` | `address` | `"type":"AAAA","address":"2001:db8::1"` |
| `CNAME` | `cname` | `cname` |
| `MX` | `mx-exchange`, `mx-preference` | `mx-exchange` (+ optional `mx-preference`, which defaults to `"0"` and is then returned) |
| `SRV` | `srv-target`, `srv-port`, `srv-priority`, `srv-weight` | `srv-target`, `srv-port` (`srv-priority`/`srv-weight` default to `"0"` and are returned) |
| `TXT` | `text` | `text` |
| `NS` | `ns` | `ns` |
| `FWD` | `forward-to` | `forward-to` |
| `NXDOMAIN` | none | `"type":"NXDOMAIN"` only |

- `add`'s arguments (`/console/inspect`) are exactly `address`,
  `address-list`, `cname`, `comment`, `copy-from`, `disabled`, `forward-to`,
  `match-subdomain`, `mx-exchange`, `mx-preference`, `name`, `ns`,
  `place-before`, `regexp`, `srv-port`, `srv-priority`, `srv-target`,
  `srv-weight`, `text`, `ttl` and `type`.
- **A field for another type is dropped or refused, depending on the type.**
  A `CNAME` sent with an `address` was created, and the `address` silently
  dropped. An `MX` with only an `address` was `400 "failure: bad MX data"`.
- **Regexp records have no `name`.** `{"regexp":"rx\\.probe","address":...}`
  returns `regexp` and `type:"A"` and no `name` key. Sending both `name`
  and `regexp` is `400 "failure: only name or regexp allowed"`.

### 2. Uniqueness RouterOS enforces

Each probe re-added an existing entry, then varied one field at a time.

| Section | A second **enabled** entry is refused when it matches on | 400 `detail` | Allowed |
|---|---|---|---|
| `ip/firewall/address-list` | `list` + `address` (`10.10.10.10/32` counts as `10.10.10.10`) | `failure: already have such entry` | the same pair **disabled**; overlapping prefixes (`10.10.10.0/24` beside `10.10.10.10`). A different comment is still a duplicate, and so is the same pair with a `timeout` |
| `ip/dhcp-server/network` | `address`, exactly | `failure: such network already exists` | an overlapping network (`192.168.88.0/25` beside `/24`). An address with host bits (`192.168.88.1/24`) is `failure: invalid network` |
| `ip/dhcp-server/lease` (static) | `mac-address` within one `server` (case-insensitive: `aa` = `AA`); `server=all` clashes with every server, and every server with `all` | `failure: already have static lease for this client` | the same MAC on a **different** named server; the same server+MAC **disabled** |
| `ip/dhcp-server/lease` (static) | `address`, **across all servers** | `failure: already have static lease with this IP address` | nothing: the same IP on another server was also refused |
| `ip/dns/static` | `name` + `type` + the whole value (every value field in the table above; tried for `A`, `AAAA`, `CNAME`, `MX`, `SRV`, `TXT`, `FWD` and `NXDOMAIN`, not `NS`) | `failure: entry already exists` | the same record **disabled**; a name differing only in case (`SVC.lab.example`), since `name` is case-sensitive; the same name with another type; two `CNAME`s for one name with different targets; a `CNAME` and an `A` for one name; `MX` differing only in `mx-preference`, `SRV` in any one of `srv-port`/`srv-priority`/`srv-weight`/`srv-target`, `TXT` in case (`a`/`A`) |
| `ip/dns/static` | (not part of the key) `ttl`, `comment`, `match-subdomain` | `failure: entry already exists` | nothing: changing only these still clashes |
| `ip/dns/static` (regexp) | `regexp` + `address` | `failure: entry already exists` | |
| `ip/dhcp-server` | `name` | `failure: server with such name already exists` | |

So on one router the natural keys collide only when **at least one of the
entries is disabled** (and never for DHCP networks, which cannot be
disabled).

### 3. `ip/service` and `comment`

`ip/service` does **not** take a `comment` and never returns one.

- `PATCH /rest/ip/service/*0 {"comment":"probe comment"}` is
  `400 "unknown parameter comment"`, and so is `{"comment":""}`. A GET
  afterwards has no `comment`.
- `set`'s arguments (`/console/inspect`) are `address`, `certificate`,
  `disabled`, `max-sessions`, `numbers`, `port`, `tls-version` and `vrf`:
  no `comment`.
- **A PATCH by name is refused differently.** `PATCH /rest/ip/service/ftp`
  with `comment` is `400 "missing or invalid resource identifier"`, while
  the same URL with `{"max-sessions":"20"}` succeeds. So the name did
  resolve, and the misleading `detail` is really the unknown parameter. By
  contrast `PATCH /rest/user/lab-ro`, `/system/script/lab-hello` and
  `/ip/dhcp-server/dhcp-lab` with a `comment` all succeed by name. mtha
  always PATCHes by `.id`, where the error is the plain "unknown parameter".

### 4. The REST `move` command

`move`'s arguments (`/console/inspect`) are `numbers` and `destination`,
and nothing else.

```
POST /rest/ip/firewall/filter/move
{"numbers": "*F", "destination": "*C"}
-> 200 []
```

This moves rule `*F` to **immediately before** `*C`.

| Probe | Response | Effect |
|---|---|---|
| `numbers` = one `.id`, `destination` = another | `200 []` | moved before `destination`. **The moved rule keeps its `.id`** (`*F` stayed `*F` across every move) |
| `.id` instead of `numbers` | `200 []` | the same: `.id` is accepted as the key |
| `numbers` = `"*D,*E"` / `"*E,*D"` | `200 []` | both moved as a block before `destination`, **in the order given** (not table order) |
| no `destination` | `200 []` | moved to the **end of the whole table**, after every chain |
| `destination` = an unknown `.id` (`*FFFF`) | **`200 []`** | **moved to the end of the table**, silently, exactly as with no `destination` |
| `numbers` = an unknown `.id` | `404 {"error":404,"message":"Not Found"}` | nothing |
| `numbers` = `destination` | `400 "failure: can not move object before itself"` | nothing |
| `{}` or only `destination` | `400 "missing =.id="` | nothing |
| `numbers` or `destination` = `"0"` | `200 []` | a bare number is the console's **position** in the whole table: `"numbers":"0"` moved the table's first rule (`input`), and `"destination":"0"` moved a rule to the very top |
| `numbers` = a comment (`"probe-4"`) | `200 []` | moved the rule carrying that comment. With two rules sharing the comment it picked the first; an unmatched comment is `404` |
| `destination` in another chain | `200 []` | allowed: the table is one list, and a rule can sit between another chain's rules |
| `PATCH` with `place-before` | `400 "unknown parameter place-before"` | nothing: `place-before` is create-only, so `move` is the only way to reorder |

The same command exists on `nat` (`POST /rest/ip/firewall/nat/move` with
an unknown `numbers` is the same `404`).

### 5. Hard and soft references

**Hard**: create refused with a 400 naming the field. **Soft**: accepted,
and the object is inert until the referent exists.

| Referrer: field = unknown value | Result |
|---|---|
| `ip/dhcp-server` `address-pool` = `nosuch-pool` | **Hard.** `400 "input does not match any value of address-pool"` |
| `ip/dhcp-server` `interface` = `nosuch-if` | **Hard.** `400 "input does not match any value of interface"` |
| `ip/dhcp-server/lease` `server` = `nosuch-server` | **Hard.** `400 "input does not match any value of server"` |
| `ip/dhcp-server/lease` `server` = `all` | Accepted: `all` is a value. The lease comes back with **no `server` key**, and a `PATCH` of an existing lease to `server=all` likewise drops the key |
| `user` `group` = `nosuch-group` | **Hard.** `400 "input does not match any value of group"` |
| `ip/route` `routing-table` = `nosuch-table` | **Hard.** `400 "input does not match any value of routing-table"` |
| `ip/route` `gateway` = `nosuch-if` (an interface name) | **Hard.** `400 "invalid or unexpected argument base"` |
| firewall `in-interface` = `nosuch-if` | **Hard.** `400 "input does not match any value of interface"` |
| firewall `in-interface-list` = `nosuch-list` | **Hard.** `400 "input does not match any value of interface-list"` |
| firewall `jump-target` = `nosuch-chain` | **Soft.** Created (`invalid:"true"`, which is not a reliable signal, see firewall) |
| firewall `src-address-list` = `nosuch-list` | **Soft.** Created (as the fixture already showed) |
| `system/scheduler` `on-event` = `nosuch-script` | **Soft.** Created |

Deleting a referent that something still names:

| Delete | Result | What the referrer reads afterwards |
|---|---|---|
| `ip/dhcp-server` `dhcp-lab`, while static leases name it | **Allowed** (`200`) | each lease's `server` is `"*1"` (the dead server's `.id`), plus `active-server:"*FFFFFFFF"`. **Re-creating a server named `dhcp-lab` does not re-link them**: they still read `"*1"` |
| `ip/pool` `lab-pool`, while `dhcp-lab` names it | **Allowed** | the server's `address-pool` is `"*1"` |
| `system/script` `lab-hello`, while `lab-daily` names it in `on-event` | **Allowed** | `on-event` still reads `"lab-hello"`: it is plain text, not a link |
| `user/group` with a member | **Refused.** `400 "failure: group has some users"` | unchanged |

So the create-time check is by name and strict, while the delete-time
check mostly isn't: the link is stored by `.id` and left dangling.

## Errors

| Case | Status | Body |
|---|---|---|
| Unknown ID in a valid section (`/ip/firewall/filter/*1`) | 404 | `{"error":404,"message":"Not Found"}` (no `detail`) |
| Unknown path (`/nonexistent/path`) | 400 | `{"detail":"no such command or directory (nonexistent)","error":400,"message":"Bad Request"}` |
| Command path via GET (`/ip/firewall/filter/print`, `/console/inspect`) | 400 | `{"detail":"no such command",...}` |
| Wrong or missing credentials | 401 | `{"error":401,"message":"Unauthorized"}`. A wrong password also logs `system,error,critical` "login failure". |
| `HEAD` on a section | 501 | JSON body |
| Lookup by a key the section has no `name` for (`/ip/firewall/filter/<comment>`) | 400 | `{"detail":"no such command prefix",...}` |

Error bodies are JSON with `error` (number, the one non-string value in the
whole API), `message` and, on 400, usually `detail`. The `detail` is the
useful part.

Write-side errors known from earlier work (the write probes add many more,
listed with each probe under [Write probes](#write-probes-design-questions)):

- `PUT /interface/vrrp` with `address` returns
  `400 {"detail":"unknown parameter address",...}`.
- `PUT /interface/vrrp` with `priority=255` returns
  `400 {"detail":"value of priority out of range (1..254)",...}` (second
  survey).
- A `PATCH` of a user's `password` returns **400 while still applying the
  change**. For at least that case, a 400 does not mean "nothing happened".

## Assumptions the code makes that this contradicts

These are ordered by how much they matter. Each one says what the code
assumes and what the device actually does.

1. **`vrrp-state` does not exist, so half of `VRRPInstance.Role` is dead
   code, and some fixtures are fiction.**
   - Code: `internal/routeros/types.go`. The `State` field and the
     `fromState` branch in `Role()`.
   - Fixtures that use shapes the device never sends:
     - `internal/routeros/rest_contract_test.go`: the `vrrp-state` fixture
       and the `Role` cases.
     - `internal/ui/apply_test.go`: `TestApplyMasterCheckFailsClosedOnVRRPPayload`
       feeds `vrrp-state`, and also `"master":"false","backup":"true"`.
   - The device emits exactly one of `master`/`backup`, only as `"true"`,
     and omits the other. `Role()` still decides correctly on the real
     shape, since an absent flag reads as not-true.
   - No test uses the one shape that actually occurs. For example the
     backup payload has `backup:"true"`, no `master` key, and
     `running:"false"`.
   - Neither flag present: **confirmed for a disabled instance** in the
     second survey, where `Role()` gives `RoleUnknown`, the safe answer.
     `init` is still unobserved.

2. **`ip/service` is not a fixed set of built-in entries.**
   - Code: `internal/plan/plan.go` (`patchOnlySections`: "a fixed set of
     entries built into RouterOS").
   - The section also returns dynamic rows. That includes a per-connection
     `reverse-proxy` row for mtha's own request, with a new `.id` and
     `remote` every time, and a duplicate `name`.
   - Drift survives this only because `model.Select` drops
     `dynamic:"true"` first. Any reader that skips `Select` would see
     permanent, self-inflicted flapping drift. That includes a future
     "raw" view, and tests with hand-written fixtures.
   - On `name`-keyed identity (`BuildIdentities` default branch), the
     static and dynamic `reverse-proxy` rows would collide.

3. **The REST lockout guard protects `www-ssl`, but on 7.23 the REST
   connections are listed under `reverse-proxy`.**
   - Code: `internal/plan/plan.go` (`lockoutReason`). It refuses to
     change `port`/`disabled`/`address` only on the `www-ssl` row.
   - On this device a static `reverse-proxy` service also listens on 443,
     and every live REST connection is listed as a dynamic `reverse-proxy`
     row.
   - Whether disabling or narrowing `reverse-proxy` would cut mtha off
     can't be proven read-only. The listing strongly suggests it could.
   - Worth a lab write test (by the harness), and probably a guard on
     `reverse-proxy` too.
   - **Second survey: not reproduced.** The REST connection rows were
     named `www-ssl` on both routers throughout (see `ip/service`), so on
     the populated lab the `www-ssl` guard protects the row the connections
     are listed under. The first survey's `reverse-proxy` rows are not
     explained; until they are, treat both names as possible and guard
     both.

4. **"Absent equals default" is rarely what happens. The real hazard is
   the other way round.**
   - Code: `internal/model/normalize.go` (`defaultLikeValues`) and the
     `EntriesEqual` docs ("routers omit fields left at default").
   - The device returns defaults explicitly (`disabled:"false"`,
     `address:""`, `on-master:""`, `certificate:"none"`), so the
     absent-versus-default branch almost never fires for configuration.
   - What does come and go are *state flags that appear only when true*:
     `master`, `backup`, `connect`, `dhcp`, `ecmp`.
   - The fold is harmless for those, since absent and `"false"` both mean
     false. But `defaultLikeValues` also folds `"none"` and `"0"` into
     absent. If a property is present on one router and absent on the
     other, that would hide a real `0`/`none` value. No such case was seen,
     but the sections that could show one were all empty (see below).
   - When a field is present on both sides, the code compares literally,
     so `""` vs `"none"` on `certificate` is reported as drift even though
     both mean unset.
   - **Second survey: the absent-versus-default branch does fire, and the
     fold is load-bearing.** In the firewall tables, `disabled`, `log` and
     `log-prefix` are present or absent according to how the rule was
     written (see [firewall](#ipfirewallfilter-nat-mangle-raw)), so the
     same rule on two routers can be `"disabled":"false"` on one and
     keyless on the other, or `"log-prefix":""` against nothing. Without
     `defaultLikeValues` folding `"false"` and `""` into absent, every
     rule mtha did not itself write would drift. The fixture's rules were
     written the same way on both routers, so the lab does not show this
     at baseline; a pair configured by hand would.
   - The `"none"`/`"0"` hazard is still unobserved: no populated section
     had a field present as `"0"` or `"none"` on one side only.

5. **`interface/vrrp` has device-reported state fields.**
   - Normalisation strips `master`, `backup` and `mac-address` through
     `sectionStateFields`; the common state rule already strips `running`
     and `invalid`. The role flags otherwise differ permanently across a
     healthy pair and could enter a write body. The remaining returned
     fields are settable configuration, with `priority` handled by the
     existing per-pair exemption.

6. **`.id` has three shapes, not two.**
   - Code: nothing in the code keys on `.id`, which is correct, but
     issue #10 (and any future test) assumes `*1` vs `*80000001`.
   - Connected routes are `*2018xxxx`. `*0` exists and is a real ID
     (`ip/service` `telnet`, `system/scheduler`'s first job).
   - `*8000xxxx` is not "the DHCP route": static routes share the counter,
     and the DHCP route's number moves (`*80000001`, later `*80000004`).

7. **The two routers are not distinguishable by identity out of the box.**
   - Code: `internal/ui/dashboard.go` (the dashboa…

   > **Text lost.** Item 7's body was truncated mid-word in the commit that
   > added this page (97372d7), and the start of item 8 was spliced into
   > it. What survives is reproduced here unchanged in substance; the rest
   > could not be recovered from the repository. From the per-section notes
   > above: both routers report `system/identity` `CHR`, and the dashboard
   > prints `identity: <name>` for each router, so the two panes show the
   > same identity until one is set.

8. **`last-logged-in` is not moved by REST access.** This is not a
   contradiction, but the explanation behind the exemption is wrong.
   - Code: `internal/model/normalize.go` (already in
     `sectionStateFields["user"]`) and the sample config (also exempts it,
     commented "updates independently on every login").
   - REST basic-auth requests left it unchanged on both routers. Its value
     matches the bootstrap's binary-API login, so that is the kind of login
     that moves it. mtha's own polling does not.
   - Stripping it is still right, because the two routers' values will
     differ. It is also absent on a user who has never logged in.

   > **Orphaned fragment**, from an item whose start was lost in the same
   > splice (its subject appears to be REST logins in `/log`): "… writes
   > an account log line. Polling at the dashboard's rate turns each
   > router's `/log` into mostly mtha logins. `events` filters by topic,
   > so the timeline is fine, but the router's own log buffer rolls over
   > faster and older VRRP transitions fall out of it sooner." This sits
   > uneasily with the `log` notes above (one login line per keep-alive
   > connection, not per request); it was not re-examined in the second
   > survey.

9. **Error bodies differ between 400 and 404, and a 400 is not atomic.**
   - Code: `internal/routeros/client.go` (`do` flattens every status ≥ 300
     into one `fmt.Errorf` with the raw body) and `internal/plan/execute.go`
     (stops the plan on the first error).
   - The body is always JSON, and `detail` (the actionable part) is only
     present on 400s.
   - The confirmed password case shows a 400 can still have applied. After
     a failed op, "stop and re-read drift" (as `Execute`'s doc says) is
     the only correct response, never "retry" or "assume unchanged".
   - Nothing parses `detail`, so the UI shows the raw JSON.

10. **A disabled route drifts against its enabled twin in more than
    `disabled`.**
    - Code: `internal/model/normalize.go` (`sectionStateFields["ip/route"]`)
      together with the sample config's `ip/route.disabled` exemption and
      `runtime.toggles.routes`, which disables tagged routes on the standby
      by design.
    - A disabled static route drops `distance`, `routing-table`, `scope`
      and `target-scope` (and `dynamic`) from its REST object. On the
      enabled side they are present with non-default-like values (`"1"`,
      `"main"`, `"30"`, `"10"`), so `EntriesEqual` reports all four as
      drift even with `disabled` exempt, and a sync would try to write
      them onto the disabled route.
    - Not reproduced as a diff here: the lab pair has no toggles, and the
      fixture's routes are enabled on both routers. The shape was observed
      directly.

11. **`ip/route` needs `ecmp` and `immediate-gw` stripped for a VRRP pair to
    be clean.** This is a confirmation with a warning: the master's static
    routes through the VIP subnet are `ecmp:"true"` with a two-path
    `immediate-gw`, the backup's are not. Both are in
    `sectionStateFields["ip/route"]` today; removing either would make
    every tagged route drift at rest.

12. **`system/scheduler` `start-date` defaults to the creation day.**
    - Code: `internal/model/normalize.go` strips `next-run`, `owner` and
      `run-count`, not `start-date` (correctly: it is configuration).
    - Two routers whose jobs were created on different days, without an
      explicit `start-date`, drift on it. The lab's fixture is created the
      same day on both, so it is clean. Worth knowing when reading drift
      on a real pair; not a code defect.

13. **Identity fallbacks meet shapes they do not expect.**
    - Code: `internal/model/identity.go` (`BuildIdentities`).
    - `ip/dns/static`: an uncommented `CNAME` (or any non-`A` record) has
      no `address`, so it falls to the bare ordinal `#1`, `#2`, ...,
      shifted by any other address-less record before it. Two names
      sharing one address collide on the address key and are paired by
      encounter order.
    - `ip/dhcp-server/network` and `ip/dhcp-server/lease` have no `name`
      and fall to `address`, which is unique in practice.
    - None of these drift on the lab (every fixture DNS record with an
      address is distinct, and the CNAME is the only address-less one),
      but they are the cases issue #12 should test.

14. **Sections whose state fields are not stripped.** Only a hazard if
    they are ever synced; neither is in the sample config.
    - `ip/pool`: `total`, `used`, `available` (counters; `used` would
      differ as soon as either router hands out a lease).
    - `tool/netwatch`: `status`, `since`, `done-tests`, `failed-tests`
      (`.about` is already in `commonStateFields`).

15. **Netwatch reports `unknown` for five minutes after every boot.**
    - Code: `internal/runtime/templates.go` (`netwatchDownAny` counts
      `status=down`) and anything that reads `NetwatchEntry.Status`.
    - After a reboot (a golden restore here, a crash or upgrade in
      production) every netwatch entry is `"unknown"` with a `.about`
      warning until its 5-minute `startup-delay` passes. A check for
      `down` treats that window as healthy; a check for "not up" would
      treat it as failed. Which is right is a design choice; the code
      should make it deliberately.
    - `NetwatchEntry.Disabled` reads `""` on every enabled entry (there is
      no `disabled` key), which is fine only while callers compare with
      `"true"`.

16. **The identity and reference code predates the write probes.** Each
    point says what the code does and what the device does (see
    [Write probes](#write-probes-design-questions)); none is fixed here.
    - `internal/model/identity.go` `dnsValueFields` knows only `A` and
      `CNAME`, so every other type is identified `name|type`. Two `MX`,
      `TXT`, `SRV`, `FWD` or `AAAA` records under one name therefore pair
      by occurrence, although the device has a value for each (probe 1).
      A regexp record has no `name` at all and is identified `|A|<address>`.
    - Leases: `server|mac-address` reads a missing `server` as `""`, which
      is what a `server=all` lease looks like. A lease whose server was
      deleted reads `server:"*1"`, so it is identified `*1|<mac>` and shows
      as a remove plus an add against the peer's `dhcp-lab|<mac>`, not as a
      `server` change.
    - `internal/plan/references.go` warns, and its comment says which
      references RouterOS rejects "is not yet surveyed". It is now (probe 5):
      a missing pool, interface, interface list, server, group or routing
      table is a certain 400, so that op will stop the apply partway; only
      address lists, jump targets and scheduler scripts are soft.
    - `internal/plan/plan.go` skips order hunks with "move is not
      implemented", pending the lab. The `move` body and `.id` behaviour
      are now known (probe 4).

Things the code assumes that this survey **confirms**:

- All values are strings. `model.Entry` stays `map[string]any` and every
  reader goes through `stringField`/`stringOf`. `Flag` accepts a JSON
  boolean too, but the device never sends one.
- `dynamic` is always present as `"true"`/`"false"` where it exists, so
  `isDynamic` is sound.
- The VIP lives on `ip/address` with `interface=<vrrp name>`, which is
  exactly what `runtime.addressOp` writes.
- The log time format `YYYY-MM-DD HH:MM:SS` and clock `date` `YYYY-MM-DD`
  match the first layouts `ParseLogTime` tries.
- `ip/route`'s dynamic routes are all `dynamic:"true"`, so opt-in route
  selection never sees them.
- Second survey, against the populated lab: comment-first identity with
  `#n` dedupe, chain-anchored ordinals for uncommented firewall rules
  (including rules before a chain's first commented rule), `list|address`
  for address lists, opt-in `mtha:` routes, and the per-section state
  fields together give a **clean diff at baseline in all twelve synced
  sections** (`TestLabBaselineIsDriftFree`).
- Timed address-list entries are `dynamic:"true"`, so `Select` drops them.
- A section of 3,000+ entries comes back whole, in configured order, in one
  GET (see below).

## Not determinable read-only

These need a write, a client, or more time. Items the second survey
answered are marked.

- **Entry shapes of the empty sections.** *Answered*: every section is now
  populated and described above, except:
  - `ip/dhcp-server/lease`: a **bound or dynamic lease** (`status` values
    other than `waiting`, the `active-*` fields, `expires-after`,
    `host-name`). It needs a DHCP client on `ether2`, which the lab does
    not have. The static-lease shape is above.
- **`ip/route`: every field of a static, commented route.** *Answered*
  (see `ip/route`): `static`, `active`, `inactive`, `scope` and
  `target-scope` are returned; `suppress-hw-offload` is not, unless set.
- **The VRRP role flags in other states.** *Disabled: answered* (neither
  flag, no `invalid`). Priority-255 owner: *not configurable* on 7.23.
  `init`: still unobserved.
- **Completeness.** *Answered*: 3,011 filter rules came back whole and in
  order from one GET, in about 2.7 s on the lab (a 10 s client timeout
  leaves headroom, but not unlimited: time grows with the table).
  `TestLabLargeSectionComesBackWhole` keeps it checked. No paging or
  truncation marker exists in the response.
- **The write schema.** Which fields the planner and Runtime send that
  RouterOS rejects (beyond `address` on `interface/vrrp`), which read-only
  fields a `PATCH` would echo back as errors, and which other writes return
  400 but still apply. *Partly answered* by the write probes: `comment` on
  `ip/service` and `place-before` in a `PATCH` are rejected, the `move`
  body is known, and the 400s for unknown references and duplicates are
  listed. The rest is not surveyed.
- **Whether disabling `reverse-proxy` severs REST.** See contradiction 3.
  Not surveyed; the connection rows' naming now makes it less pressing but
  not settled.
