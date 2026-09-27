# Lab REST contract

What the two lab routers (`testlab/`, RouterOS **7.23.7 long-term**, CHR)
actually return over `GET /rest/...`. This page records **shape, not values**:
field names, types, which fields come and go, and `.id` forms. It is the
ground truth that the fixtures in `internal/**/_test.go` should match.

It was surveyed read-only (GET only) on 2026-09-27. Another agent may write
to the same lab at any time (though no config changed during the survey), so any value seen may be
transient. Values quoted here are illustrative only.

## How it was gathered

- `curl -k -u admin:... https://localhost:443/rest/<section>` (router A) and
  `:8443` (router B), for every section below, repeated every 10 s for about
  40 minutes. A copy was kept whenever a response changed.
- Error behaviour was learned from GETs against bad paths and IDs, and from
  one GET with a wrong password.
- No `PUT`/`PATCH`/`POST`/`DELETE` was sent. One side effect: the
  wrong-password GET wrote a `system,error,critical` "login failure" line
  to router A's log.

**Coverage caveat.** At survey time the lab held only what
`testlab/provision.sh` creates: an address on `ether2`, a `vrrp-lan`
instance and its VIP. `ip/route`, `ip/service`, `user` and
`interface/vrrp` had entries. **The other twelve sections returned `[]` on
both routers for the whole survey.** Their entry shapes are listed under
[Not determinable read-only](#not-determinable-read-only). They need a
populated lab, which is the harness's job.

## Rules that hold across every section

| Rule | Observed |
|---|---|
| Value types | **Every value is a JSON string**, with no exceptions in any response surveyed. That includes numbers (`"200"`, `"1500"`, `"8291"`), booleans (`"true"`/`"false"`), durations (`"1s"`, `"10m"`, `"24m26s"`) and timestamps (`"YYYY-MM-DD HH:MM:SS"`). No JSON `true`, number or `null` ever appears. |
| Boolean spelling | Always `"true"`/`"false"`, never `"yes"`/`"no"`. `yes`/`no` are console and script spellings, not REST ones. |
| Default-valued fields | **Returned explicitly** for configurable properties: `"disabled":"false"`, `"invalid":"false"`, `"address":""`, `"on-master":""`, `"password":""`, `"comment"` when set. A property left at its default is present, not omitted. |
| Conditional fields | Some fields are **absent** rather than `"false"`. They are status flags that appear only when set (`master`, `backup`, `ecmp`, `connect`, `dhcp`), plus properties that only exist for some kinds of entry (see each section). |
| `comment` | Omitted when there is no comment. `user` `*1` carries one (`"system default user"`); no other surveyed entry has a `comment` key at all. |
| `.id` | A string `*<hex>`, e.g. `*1`, `*E`, `*20183040`, `*80000001`. The size of the number says where the entry came from (see below). |
| Collection vs item | `GET /rest/<section>` returns a JSON array (`[]` when empty). `GET /rest/<section>/<id>` returns a single object. |
| Lookup by name | `GET /rest/<section>/<name>` works as well as `/<id>`. For example `/rest/user/admin` and `/rest/ip/service/www-ssl` each return the single object. |
| Query filters | `?field=value` filters (`?master=true`, `?name=www-ssl`, `?dynamic=false`) and returns an array. `?.proplist=a,b` limits fields but **always includes `.id`**. |
| Singletons | `system/resource`, `system/identity` and `system/clock` return a bare object, not an array. |

### `.id` shapes

| Shape | Seen on |
|---|---|
| `*0`, `*1` ... `*F`, `*1A` | Static config (`user`, `ip/service`, `ip/address`, `interface/vrrp`), and dynamic `ip/service` connection rows |
| `*2018xxxx` (high 32-bit value) | Dynamic connected routes (`ip/route` with `connect`) |
| `*80000001` | The DHCP-client default route (`ip/route` with `dhcp`) |

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
- **`vrrp-state` is never returned.** No such field exists.
- **`running` is not a role signal.** It is `"true"` on the master and
  `"false"` on the backup, because a backup's VRRP interface is not
  running. The same state appears in `/interface` (`running`) and on the VIP
  in `/ip/address` (`invalid:"true"` on the backup).
- `on-master`/`on-backup`/`on-fail` are returned as `""` when unset, not
  omitted. A multi-line script would come back as one JSON string with
  embedded newlines. That is not verified here, because no script was set.
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

A static route was never observed. Every route in the lab is dynamic.

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
  services it returns dynamic rows: `dhcp`, `dhcpclient`, `btest` and
  `discover`. It also returns **one dynamic `reverse-proxy` row per open
  HTTPS connection**, carrying `connection:"true"`, `local` and
  `remote:"<ip>:<ephemeral port>"`. **The REST client's own request shows up
  in the response it is reading.** Several can appear at once. Their `.id`
  and `remote` change on every request.
- 7.23 has a **static `reverse-proxy` service on port 443**, the same port
  as `www-ssl`, and the REST connections are listed under `reverse-proxy`,
  not `www-ssl`.
- The name `reverse-proxy` therefore appears **more than once** in one
  response: once static and once or more dynamic.
- `certificate` is `"none"` when unset on a TLS service, and absent on
  non-TLS services.
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

### Sections that were empty for the whole survey

`ip/firewall/filter`, `ip/firewall/nat`, `ip/firewall/mangle`,
`ip/firewall/raw`, `ip/firewall/address-list`, `ip/dhcp-server`,
`ip/dhcp-server/network`, `ip/dhcp-server/lease`, `ip/dns/static`,
`system/script`, `system/scheduler`, `tool/netwatch`, and `ip/pool`.

On both routers these return `200 []`. There are no hidden defaults and no
dynamic entries, so on this lab the firewall starts genuinely empty. An
empty section is **`[]`, never `null` or an empty body**.

## Errors

| Case | Status | Body |
|---|---|---|
| Unknown ID in a valid section (`/ip/firewall/filter/*1`) | 404 | `{"error":404,"message":"Not Found"}` (no `detail`) |
| Unknown path (`/nonexistent/path`) | 400 | `{"detail":"no such command or directory (nonexistent)","error":400,"message":"Bad Request"}` |
| Command path via GET (`/ip/firewall/filter/print`, `/console/inspect`) | 400 | `{"detail":"no such command",...}` |
| Wrong or missing credentials | 401 | `{"error":401,"message":"Unauthorized"}`. A wrong password also logs `system,error,critical` "login failure". |
| `HEAD` on a section | 501 | JSON body |

Error bodies are JSON with `error` (number, the one non-string value in the
whole API), `message` and, on 400, usually `detail`. The `detail` is the
useful part.

Two write-side errors are known from earlier work, not from this survey:

- `PUT /interface/vrrp` with `address` returns
  `400 {"detail":"unknown parameter address",...}`.
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
   - Neither flag present (say a disabled or `init` instance) can't be
     confirmed read-only. `RoleUnknown` there is still the safe answer.

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

5. **`interface/vrrp` in `sync.sections` would drift forever.**
   - Code: `internal/model/normalize.go` (`sectionStateFields` has no
     `interface/vrrp` entry).
   - `commonStateFields` strips `running` and `invalid`, but not `master`,
     `backup` or `mac-address`.
   - Between a healthy pair, `master` is only on A and `backup` only on B,
     so the diff shows both as changes. An A→B sync would then `PATCH
     master=true` onto B. RouterOS will presumably reject that as a
     read-only property; that is not verified here.
   - `docs/configuration.md` shows `interface/vrrp.priority` as an exempt
     field, which implies the section is expected to be syncable. It
     isn't, until `master`/`backup` are stripped.

6. **`.id` has three shapes, not two.**
   - Code: nothing in the code keys on `.id`, which is correct, but
     issue #10 (and any future test) assumes `*1` vs `*80000001`.
   - Connected routes are `*2018xxxx`. `*0` exists and is a real ID.

7. **The two routers are not distinguishable by identity out of the box.**
   - Code: `internal/ui/dashboard.go` (the dashboa8. **`last-logged-in` is not moved by REST access.** This is not a
   contradiction, but the explanation behind the exemption is wrong.
   - Code: `internal/model/normalize.go` (already in
     `sectionStateFields["user"]`) and the sample config (also exempts it,
     commented "updates independently on every login").
   - REST basic-auth requests left it unchanged on both routers. Its value
     matches the bootstrap's binary-API login, so that is the kind of login
     that moves it. mtha's own polling does not.
   - Stripping it is still right, because the two routers' values will
     differ.

 writes an account log line. Polling at the dashboard's rate turns
     each router's `/log` into mostly mtha logins. `events` filters by
     topic, so the timeline is fine, but the router's own log buffer rolls
     over faster and older VRRP transitions fall out of it sooner.

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

## Not determinable read-only

These need a populated lab or a write. They belong to the harness tests
(issue #10, second half).

- **Entry shapes of the twelve empty sections.** This is every firewall
  table, the address list, all three DHCP sections, DNS static, scripts,
  schedulers and netwatch. The questions to answer there:
  - Firewall rules: whether `log`, `log-prefix`, `bytes`, `packets` and
    `invalid` are returned for a rule at defaults.
  - `address-list`: whether `creation-time` and `timeout` are returned.
  - `dhcp-server/lease`: which `status`/`active-*` fields a static lease
    has.
  - `system/script`: `owner`, `run-count`, `last-started`, and the
    `dont-require-permissions` default.
  - `system/scheduler`: `next-run`, `start-date` and `start-time` formats.
  - `tool/netwatch`: the `status` values (`up`/`down`/`unknown`?), and
    whether `since`, `done-tests` and the other counters would need
    stripping.
  - `ip/route`: every field of a **static, commented** route (the only
    kind mtha syncs). Among other things, whether `static`, `active`,
    `inactive`, `scope`, `target-scope` and `suppress-hw-offload` are
    returned.
- **The VRRP role flags in other states.** Disabled, `init`, or a
  priority-255 owner.
- **Completeness.** Whether a large section (thousands of rules) comes back
  whole in one GET or is truncated. The lab never held enough entries to
  tell.
- **The write schema.** Which fields the planner and Runtime send that
  RouterOS rejects (beyond `address` on `interface/vrrp`), which read-only
  fields a `PATCH` would echo back as errors, and which other writes return
  400 but still apply.
- **Whether disabling `reverse-proxy` severs REST.** See contradiction 3.
