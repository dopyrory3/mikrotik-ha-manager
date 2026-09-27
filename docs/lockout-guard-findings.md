# Lock-out guard vs. `reverse-proxy`: does it protect the right service?

**Status: Phase 1 (static analysis) only.** The lab experiment (Phase 2) has
**not** been run — another worker holds the lab, and disabling the wrong
service there would destroy its state. Nothing in `internal/**` was changed.
This document is the deliverable; it says what is proven statically, what is
hypothesised, the exact experiment that would settle it, and the minimal fix to
apply if it confirms.

Question: does `internal/plan`'s Apply lock-out guard protect the service that
actually carries mtha's REST connection, and is further action required?

## 1. What the guard does today

`internal/plan/plan.go` (`lockoutReason`) refuses a field change only when the
target row's `name` is `www-ssl`, and only for `port`, `disabled` and `address`:

```go
case section == "ip/service" && stringOf(tgt.raw["name"]) == "www-ssl":
    guarded = map[string]bool{"port": true, "disabled": true, "address": true}
```

Supporting facts:

- Adding or removing an `ip/service` entry is already impossible: the section is
  in `patchOnlySections`, so creates/deletes are skipped before the guard runs.
- The guard is reached only from the `default` (entry on both sides) branch; the
  changes it sees are the diff's field changes on the **static** row. Dynamic
  rows are dropped by `model.Select` before either the diff or the guard, so the
  duplicate `reverse-proxy` name (static + one per live connection) cannot make
  the guard match the wrong row. Match-keying on `name` is therefore sound.
- `ip/service` is in the shipped `sync.sections` (sample.go, testlab/pairs.yaml).
  The sample exempts `ip/service.port` and `ip/service.certificate`, so a sync
  **does** still consider `disabled` and `address` on every service, including
  `reverse-proxy`.

## 2. Which `/ip/service` entries can cut REST on 7.23

mtha speaks HTTPS only (`internal/routeros/client.go` builds `https://…/rest`),
to the port in the pair file (443 by default). So only services on that
HTTPS/REST port can lock it out.

- **`www-ssl` — proven.** `internal/labtest/reset_lab_test.go`
  (`TestLabRecreateRecoversUnreachableRouter`) disables `www-ssl` on router B and
  asserts a *fresh* REST client stops answering. That is the empirical proof
  the guard is built on.
- **`reverse-proxy` (static, port 443) — suspected, not yet proven.**
  `docs/lab-rest-contract.md` §`ip/service` records that 7.23 exposes a static
  `reverse-proxy` service on 443, the same port as `www-ssl`, and that every live
  REST connection is listed as a **dynamic `reverse-proxy` row** (with
  `connection:"true"`), not under `www-ssl`. Whether disabling/narrowing it cuts
  REST cannot be answered read-only — that is the open question.
- **The rest cannot cut mtha.** `www` is plain HTTP (:80) and mtha never uses it;
  `api`/`api-ssl` are the legacy binary API (:8728/:8729), unused; `ssh`,
  `telnet`, `ftp`, `winbox`, `dhcp`, `btest`, `discover` are unrelated. Changing
  them cannot stop the HTTPS REST listener mtha talks to.
- **Still unknown:** whether `www-ssl` and `reverse-proxy` are independent
  listeners on 443, or whether enabling `www-ssl` gates a `reverse-proxy`
  listener (i.e. `www-ssl` is the config and `reverse-proxy` the runtime). The
  two-attribution evidence (dynamic rows under `reverse-proxy`, none under
  `www-ssl`) plus `www-ssl`-disable killing REST is consistent with either. Phase
  2 disambiguates it.

Neither the guard nor the docs encode *which name* serves REST; the guard just
hardcodes `www-ssl`.

## 3. The hole (concrete, and conditional on Phase 2)

Assume Phase 2 confirms that a change to the static `reverse-proxy` row severs
REST (the whole point of the experiment). Then this is a sync the guard allows
but should refuse:

Router A (source) and router B (target) both sync `ip/service`, and both run
`www-ssl` enabled. They differ on the static `reverse-proxy` row — e.g. it was
hardened on A:

```
A: {".id":"*5","name":"reverse-proxy","port":"443","proto":"tcp","address":"","disabled":"true", "dynamic":"false"}
B: {".id":"*A","name":"reverse-proxy","port":"443","proto":"tcp","address":"","disabled":"false","dynamic":"false"}
```

Diff hunk: identity `reverse-proxy`, field `disabled` (A=true / B=false). The
operator picks **A→B**. `Build` calls `lockoutReason("ip/service","b",tgt=A's
row, changes=[disabled])`; the name is `reverse-proxy`, not `www-ssl`, so the
`default` branch returns `""` and no skip is emitted. `updateOps` then emits:

```
PATCH /ip/service/*A {"disabled":"true"}
```

If `reverse-proxy` carries REST, that PATCH kills the connection that is running
the plan: the rest of the plan fails, the post-apply drift re-read fails, and
mtha cannot reconnect. Same story for `address` (narrowing the allowed sources
to a subnet that excludes mtha) — the guard already blocks both of those on
`www-ssl`. `port` is equally dangerous but rarely reaches the guard because the
sample exempts `ip/service.port`.

The direction that bites is "narrowed source → open target"; the reverse
(enabling) is harmless. That is exactly the outcome the guard exists to prevent;
today it only prevents it for the wrong name.

## 4. Phase 2 — the experiment (run only when the lab is free)

One router at a time; router **B** is enough (host port 8443).

1. **Read-only attribution.** Fresh Docker-host client:
   `curl -sk -u admin:London12 'https://localhost:8443/rest/ip/service?name=reverse-proxy&dynamic=false'`
   — expect exactly one static row; record `.id`, `port`, `disabled`, `address`.
   Then `GET /rest/ip/service` and confirm the dynamic `reverse-proxy` rows carry
   `connection:"true"` and that no dynamic `www-ssl` row exists. `www-ssl`'s own
   row is the control.
2. **Apply what a sync would emit.** `PATCH /ip/service/<static-id> {"disabled":"true"}`
   on B. The response may be lost as the service goes down; ignore it.
3. **Probe with a fresh client**, not the keep-alive one (an open connection
   outlives the service): new client each try, `GET /system/identity`, every
   0.5 s for up to ~20 s.
   - answers within the window → `reverse-proxy` does **not** carry REST; the
     `www-ssl` name is right on 7.23 and there is no hole.
   - never answers → `reverse-proxy` **does** carry REST (or gates it); the hole
     is real.
4. **Disambiguate before recovering.** Over SSH (`ssh -p 2212 admin@localhost`,
   password `London12` — port 22 is published by docker-compose) run
   `/ip service print detail`. If `www-ssl` is still `disabled=no` while REST is
   dead, `reverse-proxy` is the listener; if disabling `reverse-proxy` also
   cleared `www-ssl`, the two are coupled. While SSH is up, re-enable:
   `/ip service set [find name=reverse-proxy] disabled=no` and re-probe REST —
   this avoids a container recreate.
5. **Recovery if SSH is unusable.** A `docker compose restart` is **not**
   sufficient: it restarts the existing container and preserves the guest's
   system disk, so the config survives. A recreate is what resets a CHR guest
   (README "Testing against real RouterOS"; `internal/labtest/lab.go`):
   `docker compose -f testlab/docker-compose.yml up -d --force-recreate router1 router2`,
   then `./testlab/provision.sh`. `bootstrap-guest.py` re-enables `www-ssl` on
   the fresh guest; wait for HTTPS before proceeding.
6. **Optional, after restoring:** repeat 2–3 for `address` (e.g.
   `{"address":"192.0.2.1/32"}`, a subnet that excludes mtha) and for `port`
   (e.g. `{"port":"9443"}`); and re-confirm the control that disabling
   `www-ssl` still severs REST. Each variant that severs REST needs the same
   recovery, so run them only if step 3 was conclusive.

A permanent version of this belongs in the harness
(`internal/labtest/*_lab_test.go`, `//go:build lab`, `labtest.New(t)`), guarded
so its cleanup recreates the containers.

## 5. Minimal fix if Phase 2 confirms

It belongs to **mtha's planner**, not to the lab harness and not to the contract
survey: the survey only records shape; the guard is production behaviour.

- **File:** `internal/plan/plan.go`.
- Replace the single-name test with membership in the set of services that can
  terminate mtha's own connection. E.g. a package-level

  ```go
  // restServices can terminate mtha's REST connection when their port,
  // disabled or address changes. On RouterOS 7.23 the HTTPS REST listener is
  // exposed as a static reverse-proxy service on 443 alongside www-ssl, and
  // live REST connections are listed under it.
  var restServices = map[string]bool{"www-ssl": true, "reverse-proxy": true}
  ```

  and the case becomes
  `case section == "ip/service" && restServices[stringOf(tgt.raw["name"])]:`.
- Keep the guarded fields exactly `{port, disabled, address}` and the existing
  "changes %s of router %s's REST API service" reason; the skip only fires when
  one of those fields actually changed (existing logic).
- No change is needed for add/remove (already blocked by `patchOnlySections`) or
  for the dynamic rows (already dropped before the guard).
- **Docs to update:** `internal/plan/doc.go`, `project.md` §10.1 (the Apply
  bullet), `docs/usage.md` (the "lock mtha out" bullet), the `lockoutReason`
  comment, and the anti-example `docs/lab-rest-contract.md` (contradiction 3 and
  the "Not determinable read-only" item, which this closes).
- **Tests:** extend `TestBuildRefusesRESTServiceLockout` with a `reverse-proxy`
  row (`disabled`/`address` skipped, an unrelated field still planned); the
  `skips` golden gains a line and must be regenerated. The causal fact stays
  proven by one lab test (Phase 2 step 5), so a future RouterOS that renames the
  service fails loudly instead of silently reopening the hole.

Not recommended as the minimal fix: deriving the guard from a hardcoded port.
The pair file's `port` is the *host* port (router B is `8443` while the guest's
`www-ssl` is still `443`), so it does not reliably equal the device service
`port`. An explicit name set is honest about the 7.23 fact. A more durable
future option is to discover the name from live attribution — the service under
which mtha's own `connection:"true"` row appears — but that needs I/O, which the
pure planner does not do.

## 6. What would falsify the finding

If Phase 2 step 3 shows REST still answering with `reverse-proxy` disabled, then
on 7.23 `www-ssl` genuinely is the REST service, the guard's name is correct,
and there is no hole to fix — record that in the contract survey as the resolved
answer, and leave the guard alone. The residual risk is only that a future
RouterOS moves/replaces the REST service name; that is a maintenance watch, not a
present bug.
