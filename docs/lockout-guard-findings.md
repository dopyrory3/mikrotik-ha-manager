# Lock-out guard vs. `reverse-proxy`: does it protect the right service?

**Status: Phase 2 (lab experiment) run 2026-09-28 against the populated lab,
RouterOS 7.23.7 (long-term), router B. Verdict: the hole is not real.** The
section-3 hypothesis is **refuted**. The static `reverse-proxy` service does not
carry mtha's REST connection. Changing its `disabled`, `address` or `port`
leaves REST answering to fresh clients. That holds even while the device lists
the live REST connections under the name `reverse-proxy`. `www-ssl` is the
listener. The guard's hardcoded name is correct on 7.23, and no fix is needed
for `reverse-proxy`.

One adjacent gap turned up: `www-ssl`'s **`certificate`** also severs REST, and
the guard does not cover it (section 5). Nothing in `internal/**` was changed.

Question: does `internal/plan`'s Apply lock-out guard protect the service that
actually carries mtha's REST connection, and is further action required?

## 1. What the guard does today

`internal/plan/plan.go` (`lockoutReason`) refuses a field change only when the
target row's `name` is `www-ssl`, and only for `port`, `disabled` and `address`:

```go
case section == "ip/service" && stringOf(tgt.raw["name"]) == "www-ssl":
    guarded = map[string]bool{"port": true, "disabled": true, "address": true}
```

Supporting facts (unchanged by Phase 2):

- Adding or removing an `ip/service` entry is already impossible: the section is
  in `patchOnlySections`, so creates and deletes are skipped before the guard
  runs.
- The guard is reached only from the `default` (entry on both sides) branch; the
  changes it sees are the diff's field changes on the **static** row. Dynamic
  rows are dropped by `model.Select` before either the diff or the guard, so a
  duplicated name (static + dynamic connection rows) cannot make the guard match
  the wrong row. Match-keying on `name` is therefore sound.
- `ip/service` is in the shipped `sync.sections` (sample.go, testlab/pairs.yaml).
  Both exempt `ip/service.port` and `ip/service.certificate`, so a sync **does**
  still consider `disabled` and `address` on every service, including
  `reverse-proxy`. Phase 2 shows that is harmless for `reverse-proxy`.

## 2. Which `/ip/service` entries can cut REST on 7.23 (measured)

mtha speaks HTTPS only (`internal/routeros/client.go` builds `https://…/rest`),
to the port in the pair file (443 by default).

| Service | Field changed on router B | Fresh-client REST | Guarded today |
|---|---|---|---|
| `www-ssl` | `disabled: true` | **severed** (0/40, 0/30) | yes |
| `www-ssl` | `certificate: none` | **severed** (0/30) | **no** (exempt in the shipped configs only) |
| `www-ssl` | `port`, `address` | severs by construction¹ | yes |
| `reverse-proxy` (static, 443) | `disabled: true` | answers (40/40, 40/40) | no, not needed |
| `reverse-proxy` | `address: 192.0.2.1/32` | answers (30/30, 30/30, 20/20) | no, not needed |
| `reverse-proxy` | `port: 9443` | answers (30/30, 30/30, 20/20) | no, not needed |

¹ Not re-run here. Moving the listener off the forwarded port, or dropping
mtha's source address, cuts REST for the same reason `disabled` does. The guard
already refuses both.

- **`www-ssl` is the REST listener.** Disabling it severs REST while
  `reverse-proxy` stays enabled on the same port 443. `reverse-proxy` does not
  take over: the TLS handshake fails (`curl`: "TLS alert, decode error").
- **`reverse-proxy` is independent of `www-ssl`, not a gate on it.** With
  `reverse-proxy` disabled, `/ip service print detail` over SSH still shows
  `www-ssl` enabled and REST answering. Disabling `reverse-proxy` flags its own
  row `invalid` and touches nothing else. `/ip reverse-proxy` (its rule table)
  is empty on the lab. With `certificate=none` it could not terminate TLS for
  REST anyway.
- **The name on the dynamic connection rows is a label, not the listener.** It
  follows whichever 443 service was enabled most recently:
  - after a boot (golden-backup restore), the connection rows are `www-ssl`;
  - after `www-ssl` is disabled and re-enabled while `reverse-proxy` is on, they
    are `reverse-proxy` (reproduced three times);
  - after `reverse-proxy` is toggled, they go back to `www-ssl` (or no REST row
    is listed at all).

  In the `reverse-proxy`-named state, disabling or narrowing `reverse-proxy`
  still left REST answering, and disabling `www-ssl` still severed it (0/30).
  The rows are also unreliable as a per-request record: several fresh `curl`
  processes in a row were listed with the same `remote` port.
- **This probably explains the contract survey's two readings.**
  `testlab/bootstrap-guest.py` enables `www-ssl` at runtime on a fresh guest,
  where `reverse-proxy` is already on. That fits "`reverse-proxy` rows" in the
  first survey (fresh containers) and "`www-ssl` rows" in the second (after
  restore reboots). It is consistent with the toggle result above; it was not
  re-checked with a container recreate.
- **The rest cannot cut mtha.** `www` is plain HTTP (:80); `api`/`api-ssl` are
  the binary API (:8728/:8729); `ssh`, `telnet`, `ftp`, `winbox`, `dhcp`,
  `btest` and `discover` are unrelated. Not tested, since mtha never uses them.

## 3. The suspected hole — refuted

Phase 1 assumed that a change to the static `reverse-proxy` row might sever
REST. It proposed this as a sync the guard allows but should refuse:

```
A: {".id":"*5","name":"reverse-proxy","port":"443","address":"","disabled":"true", "dynamic":"false"}
B: {".id":"*A","name":"reverse-proxy","port":"443","address":"","disabled":"false","dynamic":"false"}
```

The diff hunk has identity `reverse-proxy` and field `disabled`, and the
operator picks A→B. `lockoutReason` returns `""` because the name is not
`www-ssl`, so `updateOps` emits:

```
PATCH /ip/service/*A {"disabled":"true"}
```

**Refuted.** That exact PATCH was sent to router B. It returned 200, and every
fresh-connection probe for the next 20 s answered `GET /system/identity` with
200. The same held for `address` narrowed to a subnet that excludes mtha
(`192.0.2.1/32`) and for `port` moved to 9443. Each variant was run in both
connection-naming states (section 2). The guard allowing these changes is
correct: they cannot lock mtha out on 7.23.

## 4. Phase 2 — what was run

The lab was confirmed free and at baseline first: `make test-lab` passed, both
guests had just rebooted from the golden restore, there was no dirty marker, and
`/ip/service` was identical on A and B. Router A was never modified. All writes
went to router B (host port 8443, SSH 2212).

1. **Attribution.** B had static `reverse-proxy` `*A` (port 443,
   `disabled=false`, `address=""`, `certificate=none`) and `www-ssl` `*6`
   (port 443, `certificate=lab`). No dynamic `reverse-proxy` row was listed; the
   REST connection rows were `www-ssl`.
2. **Sync-shaped write.** `PATCH /ip/service/*A {"disabled":"true"}`
   returned 200.
3. **Probe.** A new `curl` process (new TCP + TLS, `Connection: close`) ran
   `GET /system/identity` every 0.5 s for 20 s: 40/40 answered.
4. **Disambiguation over SSH.** `/ip service print detail` showed
   `reverse-proxy` `X` (disabled) and `www-ssl` still enabled. Recovery was
   `/ip service set [find name=reverse-proxy and dynamic=no] disabled=no`, then
   a re-probe.
5. **Variants.** `address` and `port` on `reverse-proxy` were each applied by
   PATCH, probed, and restored over SSH.
6. **Control.** `PATCH /ip/service/*6 {"disabled":"true"}` (`www-ssl`) severed
   REST (0/40). Recovery was `/ip service enable [find name=www-ssl and
   dynamic=no]` over SSH.
7. **Naming state.** Toggling `www-ssl` over SSH put the connection rows under
   `reverse-proxy`. Steps 2–6 were then repeated in that state, with the same
   results.
8. **Certificate.** `PATCH /ip/service/*6 {"certificate":"none"}` severed REST
   (0/30, even with verification off). Recovery was `certificate=lab` over SSH.

Every step recovered over SSH; no container was recreated. After each batch,
B's static `/ip/service` rows matched A's field for field, with only the list
order changed by the re-enables. `make test-lab` was run after the experiment:
its golden restores return both routers to the exact baseline, and it passed.

A permanent version still belongs in the harness
(`internal/labtest/*_lab_test.go`, `//go:build lab`, `labtest.New(t)`). It
should assert that disabling `reverse-proxy` leaves REST up. Then a future
RouterOS that moves REST onto that service fails loudly instead of quietly
opening the hole. The test can recover through a golden restore, because REST
stays up.

## 5. Fix

**For `reverse-proxy`: none.** Do not add it to the guard. The guard's name is
the right one, and guarding `reverse-proxy` would refuse harmless syncs.

**Adjacent gap, measured: `www-ssl.certificate`.** Setting it to `none` severs
REST, and the guard does not cover the field. Today only configuration keeps it
out of a sync: both shipped configs exempt `ip/service.certificate`, but an
operator can remove that exemption. `port` has the same exemption, and the guard
covers it anyway.

In a sync, the realistic trigger is narrower than `none`. A's `www-ssl` must
serve REST, so A cannot hold `none`. A's certificate name also has to exist on
B, otherwise the PATCH fails. The damaging case is a same-named certificate that
B's REST clients reject under verified TLS (`InsecureTLS` off, the default).

The minimal hardening, queued for a separate decision:

- **File:** `internal/plan/plan.go` (`lockoutReason`). Add `"certificate": true`
  to the `www-ssl` guarded set, and extend its comment.
- **Tests:** add a `certificate` case to `TestBuildRefusesRESTServiceLockout`
  (skipped, with an unrelated field still planned), then regenerate the `skips`
  golden.
- **Docs:** `internal/plan/doc.go`, `project.md` §10.1 (the Apply bullet) and
  `docs/usage.md` (the "lock mtha out" bullet) list the guarded fields.

**Done** (branch `dopyrory3/lockout-cert-guard`), with three corrections to the
sketch above, where it disagreed with the code:

- A guarded change refuses the *whole* hunk, unrelated fields included; nothing
  in it is planned. The test's `certificate with other field` case pins that.
  "Still planned" holds only for a hunk that touches no guarded field (the
  existing `other field` case).
- The `skips` golden does not change: its fixture has no certificate
  difference, and `-update` rewrites it byte-identical.
- `internal/plan/doc.go` names no fields ("such as moving its www-ssl
  service"), so it needed no edit. `project.md` §10.1 and `docs/usage.md` did.

A `port` case was also missing from `TestBuildRefusesRESTServiceLockout`
(only the `skips` golden covered it), so it was added: one case per guarded field.

**Doc follow-up (not done here, outside this change's target):**
`docs/lab-rest-contract.md` contradiction 3 and its "Whether disabling
`reverse-proxy` severs REST" item can now be closed with this result and the
connection-naming explanation.

## 6. Residual risk

- A future RouterOS could move REST onto `reverse-proxy` or rename the
  service. That is a maintenance watch, not a present bug, and the lab test in
  section 4 is how it would be caught.
- Outside `ip/service`, other synced sections can still lock mtha out, and the
  guard does not cover them. Examples are `user` (the admin account's password,
  group or `disabled`) and `ip/firewall/filter` (an input drop on 443). These
  were not in scope and were not tested.
