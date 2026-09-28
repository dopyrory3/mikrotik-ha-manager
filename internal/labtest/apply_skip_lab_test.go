//go:build lab

package labtest_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"mtha/internal/config"
	"mtha/internal/labtest"
	"mtha/internal/model"
	"mtha/internal/plan"
	"mtha/internal/poll"
)

// What the planner refuses to write (issue #13): the lock-out guards, the
// other skips, and dynamic entries. A skip must carry its reason to the dry
// run, and the plan must then write nothing for it — checked on the device.
//
// Most cases are real device state. Two cannot be: a router whose www-ssl
// is disabled cannot be read over REST at all, so it cannot be a plan's
// source, and both lab routers have the same built-in ip/service entries.
// Those are planned from real reads with the one row changed in memory.

const service = "ip/service"

// requireOnlySkips asserts the dry run writes nothing and skips
// exactly want (substrings of "<where>: <reason>" lines).
func requireOnlySkips(t *testing.T, v string, want ...string) {
	t.Helper()
	if ops := shownOps(t, v); len(ops) != 0 {
		t.Errorf("the plan writes %v, want nothing", ops)
	}
	if !strings.Contains(v, "nothing to write") {
		t.Errorf("the dry run does not say there is nothing to write")
	}
	for _, w := range want {
		if !strings.Contains(v, w) {
			t.Errorf("the dry run does not skip with %q", w)
		}
	}
	if t.Failed() {
		t.Fatalf("dry run:\n%s", v)
	}
}

// planOnly builds a plan for section from reads the caller may have
// altered, with the lab pair's settings.
func planOnly(lab *labtest.Lab, section string, a, b []model.Entry, choices map[plan.HunkRef]plan.Direction, exempt []string) plan.Plan {
	return plan.Build([]plan.SectionInput{{Section: section, A: a, B: b, Choices: choices}}, plan.Options{
		Exempt: exempt,
		Users:  map[string]string{"a": labtest.User, "b": labtest.User},
	})
}

func readSection(t *testing.T, lab *labtest.Lab, section string) (a, b []model.Entry) {
	t.Helper()
	ctx := context.Background()
	a, err := lab.A.GetSection(ctx, section)
	if err != nil {
		t.Fatal(err)
	}
	b, err = lab.B.GetSection(ctx, section)
	if err != nil {
		t.Fatal(err)
	}
	return a, b
}

// The REST API service's lock-out guard: a sync must not change www-ssl's
// port, disabled, address or certificate on the router it writes to, since
// each can cut mtha's own connection mid-apply. Each change is made on b
// and synced b→a. The lab pair exempts port and certificate (each router's
// own), so these plan with a pair that does not, as a pair file that lists
// neither would.
func TestLabApplyLockoutGuardRESTService(t *testing.T) {
	guarded := func(lab *labtest.Lab) *config.Pair {
		return withoutExempt(lab, "ip/service.port", "ip/service.certificate")
	}
	check := func(t *testing.T, lab *labtest.Lab, field string) {
		t.Helper()
		pair := guarded(lab)
		d := compare(t, lab, service, pair.Sync.Exempt)
		if len(d.Hunks) != 1 || d.Hunks[0].Identity != "www-ssl" || len(d.Hunks[0].Changes) != 1 || d.Hunks[0].Changes[0].Field != field {
			t.Fatalf("want www-ssl differing in %s alone, got %v", field, describe(d))
		}
		before := deviceState(t, lab, lab.A, []string{service})

		s := startTUI(t, lab, tuiOptions{write: true, pair: pair})
		s.drift()
		s.selectSection(service, plan.BtoA)
		requireOnlySkips(t, s.plan(), fmt.Sprintf("ip/service www-ssl [B→A]: changes %s of router a's REST API service; mtha could lose its connection mid-apply", field))
		if after := deviceState(t, lab, lab.A, []string{service}); after != before {
			t.Fatalf("router a's services changed:\nbefore:\n%s\nafter:\n%s", before, after)
		}
	}

	t.Run("address", func(t *testing.T) {
		lab := labtest.New(t)
		requireClean(t, lab, service)
		// Still every IPv4 source, so REST to b keeps working.
		patch(t, lab.B, service, wwwSSL(t, lab.B)[".id"], map[string]string{"address": "0.0.0.0/0"})
		check(t, lab, "address")
	})

	t.Run("port", func(t *testing.T) {
		lab := labtest.New(t)
		// As TestLabChurnServicePortIsExempt: b's guest really listens on
		// 8443, with a redirect keeping the forwarded 443 reaching it.
		script := `/ip/firewall/nat/add chain=dstnat in-interface=ether1 protocol=tcp dst-port=443 action=redirect to-ports=8443 comment="lab: www-ssl on 8443"; /ip/service/set www-ssl port=8443`
		_ = lab.B.Command(context.Background(), "/execute", map[string]string{"script": script}, nil)
		eventually(t, "b's www-ssl to answer on 8443", 30*time.Second, func() error { return serviceField(lab, "port", "8443") })
		check(t, lab, "port")
	})

	// Guarded since 4b4afa1 (docs/lockout-guard-findings.md §5); fails on a
	// branch without it, and plans the PATCH that would cut mtha off.
	t.Run("certificate", func(t *testing.T) {
		lab := labtest.New(t)
		ctx := context.Background()
		// As TestLabChurnOwnCertificateIsExempt: b serves www-ssl with a
		// certificate of its own.
		if err := lab.B.Post(ctx, "/certificate", map[string]string{"name": "lab-b", "common-name": "lab-b", "key-usage": "tls-server"}, nil); err != nil {
			t.Fatal(err)
		}
		cert := only(t, lab.B, "certificate", byName("lab-b"))
		if err := lab.B.Command(ctx, "/certificate/sign", map[string]string{".id": cert[".id"], "ca": "lab-ca"}, nil); err != nil {
			t.Fatalf("sign lab-b: %v", err)
		}
		eventually(t, "lab-b to be signed", 60*time.Second, func() error {
			if c := only(t, lab.B, "certificate", byName("lab-b")); c["fingerprint"] == "" || c["private-key"] != "true" {
				return fmt.Errorf("not signed yet: %v", c)
			}
			return nil
		})
		_ = lab.B.Patch(ctx, "/ip/service/"+wwwSSL(t, lab.B)[".id"], map[string]string{"certificate": "lab-b"}, nil)
		eventually(t, "b's www-ssl to serve lab-b", 30*time.Second, func() error { return serviceField(lab, "certificate", "lab-b") })
		check(t, lab, "certificate")
	})

	// Planned from real reads, b's www-ssl marked disabled in memory: a
	// router with www-ssl disabled cannot be read to plan from.
	t.Run("disabled", func(t *testing.T) {
		lab := labtest.New(t, labtest.ReadOnly())
		a, b := readSection(t, lab, service)
		marked := false
		for _, e := range b {
			if e["name"] == "www-ssl" && fmt.Sprint(e["dynamic"]) != "true" {
				e["disabled"] = "true"
				marked = true
			}
		}
		if !marked {
			t.Fatal("no static www-ssl row on b")
		}
		p := planOnly(lab, service, a, b, map[plan.HunkRef]plan.Direction{{Identity: "www-ssl"}: plan.BtoA}, guarded(lab).Sync.Exempt)
		want := "changes disabled of router a's REST API service; mtha could lose its connection mid-apply"
		if !p.Empty() || len(p.Skipped) != 1 || p.Skipped[0].Reason != want {
			t.Fatalf("want only the skip %q, got:%s", want, describePlan(p))
		}
	})
}

// The login user's lock-out guard: a sync must not delete the user mtha
// logs in to the target as, or change its group or disabled flag. Here mtha
// really logs in to b as lab-mtha (and to a as admin), so the guard is
// keyed on the pair's user for each router.
func TestLabApplyLockoutGuardLoginUser(t *testing.T) {
	const (
		user = "lab-mtha"
		pass = "lab-mtha-pass"
	)
	session := func(t *testing.T, lab *labtest.Lab) *tui {
		t.Helper()
		pair := *lab.Pair
		pair.Routers = map[string]config.RouterConfig{"a": lab.Pair.Routers["a"], "b": lab.Pair.Routers["b"]}
		rb := pair.Routers["b"]
		rb.User = user
		pair.Routers["b"] = rb
		pollers := map[poll.RouterKey]*poll.Poller{
			"a": poll.New("a", clientAs(lab, "a", labtest.User, lab.Password()), time.Second),
			"b": poll.New("b", clientAs(lab, "b", user, pass), time.Second),
		}
		return startTUI(t, lab, tuiOptions{write: true, pair: &pair, pollers: pollers})
	}
	requireLoginWorks := func(t *testing.T, lab *labtest.Lab) {
		t.Helper()
		if _, err := clientAs(lab, "b", user, pass).Identity(context.Background()); err != nil {
			t.Fatalf("mtha can no longer log in to b as %s: %v", user, err)
		}
	}
	addUser := func(t *testing.T, lab *labtest.Lab, key string, fields map[string]string) {
		t.Helper()
		body := map[string]string{"name": user, "password": pass}
		for k, v := range fields {
			body[k] = v
		}
		add(t, lab.Client(key), "user", body)
	}
	cases := []struct {
		name     string
		onA      map[string]string // nil: the user is not on a
		hunk     string
		reason   string
		fieldSet string // for the control plan: the field a's copy changes
	}{
		{"delete", nil, "lab-mtha [b]",
			"user lab-mtha [A→B]: mtha logs in to router b as this user; removing it would lock mtha out", ""},
		{"group", map[string]string{"group": "read"}, "lab-mtha [both: group]",
			"user lab-mtha [A→B]: changes group of the user mtha logs in to router b as; that could lock mtha out", "group"},
		{"disabled", map[string]string{"group": "full", "disabled": "yes"}, "lab-mtha [both: disabled]",
			"user lab-mtha [A→B]: changes disabled of the user mtha logs in to router b as; that could lock mtha out", "disabled"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			lab := labtest.New(t)
			addUser(t, lab, "b", map[string]string{"group": "full"})
			if c.onA != nil {
				addUser(t, lab, "a", c.onA)
			}
			requireHunks(t, compare(t, lab, "user", lab.Pair.Sync.Exempt), c.hunk)
			requireLoginWorks(t, lab)
			before := deviceState(t, lab, lab.B, []string{"user"})

			s := session(t, lab)
			s.drift()
			s.selectSection("user", plan.AtoB)
			requireOnlySkips(t, s.plan(), c.reason)
			if after := deviceState(t, lab, lab.B, []string{"user"}); after != before {
				t.Fatalf("b's users changed:\nbefore:\n%s\nafter:\n%s", before, after)
			}
			requireLoginWorks(t, lab)

			// Control: the guard is about the login user. Were mtha logging
			// in to b as admin, the same change would be planned.
			if c.fieldSet != "" {
				a, b := readSection(t, lab, "user")
				p := planOnly(lab, "user", a, b, map[plan.HunkRef]plan.Direction{{Identity: user}: plan.AtoB}, lab.Pair.Sync.Exempt)
				if ops := opsOf(p, plan.MethodUpdate); len(ops) != 1 || ops[0].Body[c.fieldSet] == "" {
					t.Errorf("with b's login admin, want a PATCH of %s, got:%s", c.fieldSet, describePlan(p))
				}
			}
		})
	}
}

// The other skips: creating a user (REST cannot read its password to copy),
// and adding or removing a built-in ip/service entry.
func TestLabApplyOtherSkips(t *testing.T) {
	t.Run("create user", func(t *testing.T) {
		lab := labtest.New(t)
		add(t, lab.A, "user", map[string]string{"name": "lab-new", "group": "read", "password": "lab-new-pass"})
		requireHunks(t, compare(t, lab, "user", lab.Pair.Sync.Exempt), "lab-new [a]")
		before := deviceState(t, lab, lab.B, []string{"user"})

		s := startTUI(t, lab, tuiOptions{write: true})
		s.drift()
		s.selectSection("user", plan.AtoB)
		requireOnlySkips(t, s.plan(), "user lab-new [A→B]: user passwords are not readable over REST; creating this user would leave it with no password")
		if after := deviceState(t, lab, lab.B, []string{"user"}); after != before {
			t.Fatalf("b's users changed:\nbefore:\n%s\nafter:\n%s", before, after)
		}
	})

	// Planned from real reads with b's ftp row left out in memory: both
	// lab routers have the same built-in services.
	t.Run("built-in service", func(t *testing.T) {
		lab := labtest.New(t, labtest.ReadOnly())
		a, b := readSection(t, lab, service)
		var without []model.Entry
		for _, e := range b {
			if e["name"] != "ftp" {
				without = append(without, e)
			}
		}
		if len(without) != len(b)-1 {
			t.Fatalf("want exactly one ftp row on b, have %d of %d rows left", len(without), len(b))
		}
		ref := plan.HunkRef{Identity: "ftp"}
		for _, c := range []struct {
			dir  plan.Direction
			want string
		}{
			{plan.AtoB, "entries in this section are built in; they can be changed but not added"},
			{plan.BtoA, "entries in this section are built in; they can be changed but not removed"},
		} {
			p := planOnly(lab, service, a, without, map[plan.HunkRef]plan.Direction{ref: c.dir}, lab.Pair.Sync.Exempt)
			if !p.Empty() || len(p.Skipped) != 1 || p.Skipped[0].Reason != c.want {
				t.Errorf("%s: want only the skip %q, got:%s", c.dir, c.want, describePlan(p))
			}
		}
	})
}

// Dynamic entries are never created or deleted. A DHCP server's leases are
// the trap: the dynamic ones sit in the same list as the static ones a
// sync manages. Router a hands one out; a sync of the lease section either
// way must neither copy it to b nor remove it from a.
func TestLabApplyDynamicLeasesUntouched(t *testing.T) {
	lab := labtest.New(t)
	ctx := context.Background()
	const (
		lease     = "ip/dhcp-server/lease"
		clientMAC = "02:00:00:00:BB:02"
	)

	// Only a's server answers: b's is disabled, and the images' own udhcpd
	// paused (see TestLabChurnDHCPLeaseState). b is the client, on a
	// macvlan with a MAC no static lease names.
	pauseUDHCPD(t, lab.Container("a"))
	pauseUDHCPD(t, lab.Container("b"))
	patch(t, lab.B, "ip/dhcp-server", only(t, lab.B, "ip/dhcp-server", byName("dhcp-lab"))[".id"], map[string]string{"disabled": "true"})
	add(t, lab.B, "interface/macvlan", map[string]string{"name": "lab-dhcp-client", "interface": "ether2", "mac-address": clientMAC})
	add(t, lab.B, "ip/dhcp-client", map[string]string{"interface": "lab-dhcp-client", "add-default-route": "no", "use-peer-dns": "no", "use-peer-ntp": "no"})

	dynamicOn := func(c string) []map[string]string {
		var all, out []map[string]string
		if err := lab.Client(c).Get(ctx, "/"+lease, &all); err != nil {
			t.Fatal(err)
		}
		for _, l := range all {
			if strings.EqualFold(l["mac-address"], clientMAC) {
				out = append(out, l)
			}
		}
		return out
	}
	var dyn map[string]string
	eventually(t, "router a to lease an address to b's client", 60*time.Second, func() error {
		got := dynamicOn("a")
		if len(got) != 1 || got[0]["dynamic"] != "true" {
			return fmt.Errorf("a's leases for %s: %v", clientMAC, got)
		}
		dyn = got[0]
		return nil
	})
	t.Logf("dynamic lease on a: %v", dyn)
	if got := dynamicOn("b"); len(got) != 0 {
		t.Fatalf("b has leases for the client too, so this would not test one-sided dynamic entries: %v", got)
	}
	requireClean(t, lab, lease)

	// a→b, with a static lease to sync alongside: only that is created.
	add(t, lab.A, lease, map[string]string{"address": "192.168.88.60", "mac-address": "02:00:00:00:AA:02", "server": "dhcp-lab", "comment": "lab: from a"})
	requireHunks(t, compare(t, lab, lease, lab.Pair.Sync.Exempt), "dhcp-lab|02:00:00:00:AA:02 [a]")
	s := startTUI(t, lab, tuiOptions{write: true})
	s.drift()
	s.selectSection(lease, plan.AtoB)
	v := s.plan()
	if ops := shownOps(t, v); len(ops) != 2 || ops[1].Method != "PUT" || ops[1].Body["comment"] != "lab: from a" {
		t.Fatalf("want the backup and the static lease's create alone, got %v:\n%s", ops, v)
	}
	requireApplied(t, s.confirm(false))
	if got := dynamicOn("b"); len(got) != 0 {
		t.Errorf("a's dynamic lease was copied to b: %v", got)
	}

	// b→a, with a static lease on b to sync: a's dynamic lease, which b
	// lacks, must not be removed.
	add(t, lab.B, lease, map[string]string{"address": "192.168.88.61", "mac-address": "02:00:00:00:AA:03", "server": "dhcp-lab", "comment": "lab: from b"})
	requireHunks(t, compare(t, lab, lease, lab.Pair.Sync.Exempt), "dhcp-lab|02:00:00:00:AA:03 [b]")
	s.drift()
	s.selectSection(lease, plan.BtoA)
	v = s.plan()
	if ops := shownOps(t, v); len(ops) != 2 || ops[1].Method != "PUT" || ops[1].Body["comment"] != "lab: from b" {
		t.Fatalf("want the backup and the static lease's create alone, got %v:\n%s", ops, v)
	}
	requireApplied(t, s.confirm(true))
	if got := dynamicOn("a"); len(got) != 1 || got[0][".id"] != dyn[".id"] {
		t.Errorf("a's dynamic lease %s did not survive the b→a sync: now %v", dyn[".id"], got)
	}
	requireClean(t, lab, lease)
}
