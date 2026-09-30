//go:build lab

package labtest_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"mtha/internal/diff"
	"mtha/internal/labtest"
	"mtha/internal/model"
	"mtha/internal/routeros"
)

// Fields that change by themselves on a real router must never read as
// drift (project.md §10.1, "Read-only state is not config"). Each test here
// makes the device itself produce that churn — traffic, a login, a DHCP
// client, a failover — checks on the raw entries that the two routers now
// really disagree on the field, and only then asserts the section's drift
// is clean. The first check matters as much as the second: a clean diff of
// two routers that never diverged proves nothing.

// A firewall rule's byte and packet counters grow under traffic, and only on
// the router that saw it.
func TestLabChurnFirewallCounters(t *testing.T) {
	lab := labtest.New(t)
	ctx := context.Background()

	// B pings A's ether2 address: A's "accept icmp" input rule counts it.
	// A pings slirp's gateway out ether1: A's masquerade counts the new
	// outbound connection. Neither touches B's copies of those rules.
	if err := lab.B.Command(ctx, "/ping", map[string]string{"address": "192.168.88.2", "count": "5", "interval": "200ms"}, nil); err != nil {
		t.Fatalf("ping a from b: %v", err)
	}
	if err := lab.A.Command(ctx, "/ping", map[string]string{"address": "10.0.2.2", "count": "3", "interval": "200ms"}, nil); err != nil {
		t.Fatalf("ping out ether1 from a: %v", err)
	}

	icmp := func(e map[string]string) bool { return e["chain"] == "input" && e["protocol"] == "icmp" }
	masq := func(e map[string]string) bool { return e["comment"] == "lab: masquerade" }
	for _, c := range []struct {
		section string
		match   func(map[string]string) bool
	}{
		{"ip/firewall/filter", icmp},
		{"ip/firewall/nat", masq},
	} {
		a, b := only(t, lab.A, c.section, c.match), only(t, lab.B, c.section, c.match)
		mustDiffer(t, c.section, "packets", a, b)
		mustDiffer(t, c.section, "bytes", a, b)
		t.Logf("%s: a packets=%s bytes=%s, b packets=%s bytes=%s", c.section, a["packets"], a["bytes"], b["packets"], b["bytes"])
		requireClean(t, lab, c.section)
	}
}

// Running a script, directly or from the scheduler, bumps its run-count on
// that router alone.
func TestLabChurnScriptAndSchedulerRunCount(t *testing.T) {
	lab := labtest.New(t)
	ctx := context.Background()

	byName := func(name string) func(map[string]string) bool {
		return func(e map[string]string) bool { return e["name"] == name }
	}
	script := only(t, lab.A, "system/script", byName("lab-hello"))
	if err := lab.A.Command(ctx, "/system/script/run", map[string]string{".id": script[".id"]}, nil); err != nil {
		t.Fatalf("run lab-hello on a: %v", err)
	}

	// The scheduler has no "run now": it runs when its time comes. Move
	// lab-daily's start to a few seconds from now on both routers — the
	// same value on each, so the config stays identical — with b's copy
	// disabled for the moment it fires, then re-enabled.
	schedB := only(t, lab.B, "system/scheduler", byName("lab-daily"))
	if err := lab.B.Patch(ctx, "/system/scheduler/"+schedB[".id"], map[string]string{"disabled": "true"}, nil); err != nil {
		t.Fatal(err)
	}
	clk, err := lab.A.Clock(ctx)
	if err != nil {
		t.Fatal(err)
	}
	now, err := time.Parse("2006-01-02 15:04:05", clk.Date+" "+clk.Time)
	if err != nil {
		t.Fatalf("router a's clock %+v: %v", clk, err)
	}
	start := now.Add(10 * time.Second)
	when := map[string]string{"start-date": start.Format("2006-01-02"), "start-time": start.Format("15:04:05")}
	for _, c := range []*routeros.Client{lab.A, lab.B} {
		s := only(t, c, "system/scheduler", byName("lab-daily"))
		if err := c.Patch(ctx, "/system/scheduler/"+s[".id"], when, nil); err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, "lab-daily to run on router a", 45*time.Second, func() error {
		if s := only(t, lab.A, "system/scheduler", byName("lab-daily")); s["run-count"] == "0" {
			return fmt.Errorf("run-count still 0, next-run %s", s["next-run"])
		}
		return nil
	})
	if err := lab.B.Patch(ctx, "/system/scheduler/"+schedB[".id"], map[string]string{"disabled": "false"}, nil); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct{ section, name string }{
		{"system/script", "lab-hello"},
		{"system/scheduler", "lab-daily"},
	} {
		section, name := c.section, c.name
		a, b := only(t, lab.A, section, byName(name)), only(t, lab.B, section, byName(name))
		mustDiffer(t, section, "run-count", a, b)
		t.Logf("%s %s: run-count a=%s b=%s", section, name, a["run-count"], b["run-count"])
		requireClean(t, lab, section)
	}
}

// A DHCP client taking up the fixture's static lease turns its status,
// last-seen and expires-after from "never used" into a live binding — on
// the server that answered.
func TestLabChurnDHCPLeaseState(t *testing.T) {
	lab := labtest.New(t)
	ctx := context.Background()
	const leaseMAC = "02:00:00:00:AA:01"

	// The stock docker-routeros image runs its own udhcpd in each container,
	// on the network bridged to the guests' ether2 (the guests never use
	// them: ether2's address is static). b's client would take whichever
	// offer comes first, often one of those, so pause both for this test.
	pauseUDHCPD(t, lab.Container("a"))
	pauseUDHCPD(t, lab.Container("b"))

	// Router b becomes the client: a macvlan on ether2 carrying the static
	// lease's MAC, running a DHCP client that router a's server answers.
	if err := lab.B.Post(ctx, "/interface/macvlan", map[string]string{"name": "lab-dhcp-client", "interface": "ether2", "mac-address": leaseMAC}, nil); err != nil {
		t.Fatalf("add macvlan on b: %v", err)
	}
	if err := lab.B.Post(ctx, "/ip/dhcp-client", map[string]string{
		"interface": "lab-dhcp-client", "add-default-route": "no", "use-peer-dns": "no", "use-peer-ntp": "no",
	}, nil); err != nil {
		t.Fatalf("add dhcp-client on b: %v", err)
	}

	static := func(e map[string]string) bool { return e["comment"] == "lab: static lease" }
	eventually(t, "the static lease to be bound on router a", 60*time.Second, func() error {
		if l := only(t, lab.A, "ip/dhcp-server/lease", static); l["status"] != "bound" {
			var client []map[string]string
			_ = lab.B.Get(ctx, "/ip/dhcp-client", &client)
			return fmt.Errorf("a's lease %v; b's lease %v; b's client %v", l, only(t, lab.B, "ip/dhcp-server/lease", static), client)
		}
		return nil
	})

	a, b := only(t, lab.A, "ip/dhcp-server/lease", static), only(t, lab.B, "ip/dhcp-server/lease", static)
	t.Logf("lease on a: %v", a)
	t.Logf("lease on b: %v", b)
	for _, f := range []string{"status", "last-seen", "expires-after"} {
		mustDiffer(t, "ip/dhcp-server/lease", f, a, b)
	}
	requireClean(t, lab, "ip/dhcp-server/lease")
}

func pauseUDHCPD(t *testing.T, container string) {
	t.Helper()
	signal := func(sig string) error {
		if out, err := exec.Command("docker", "exec", container, "pkill", "-"+sig, "-x", "udhcpd").CombinedOutput(); err != nil {
			return fmt.Errorf("pkill -%s udhcpd in %s: %v %s", sig, container, err, out)
		}
		return nil
	}
	if err := signal("STOP"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := signal("CONT"); err != nil {
			t.Error(err)
		}
	})
}

// A link failure on the master moves the VIP to router b, and router a's
// routes through the lost link go inactive while b's stay active.
func TestLabChurnRouteStateWhenVIPMoves(t *testing.T) {
	lab := labtest.New(t)
	ctx := context.Background()

	tagged := func(e map[string]string) bool { return e["comment"] == "mtha:lab-route" }
	before := only(t, lab.A, "ip/route", tagged)
	dynamicBefore := dynamicRoutes(t, lab.A)

	ether2 := only(t, lab.A, "interface", func(e map[string]string) bool { return e["name"] == "ether2" })
	if err := lab.A.Patch(ctx, "/interface/"+ether2[".id"], map[string]string{"disabled": "true"}, nil); err != nil {
		t.Fatalf("take ether2 down on a: %v", err)
	}
	waitRole(t, lab.B, routeros.RoleMaster)
	eventually(t, "router a's tagged route to go inactive", 30*time.Second, func() error {
		if r := only(t, lab.A, "ip/route", tagged); r["active"] == "true" {
			return fmt.Errorf("still active: %v", r)
		}
		return nil
	})

	a, b := only(t, lab.A, "ip/route", tagged), only(t, lab.B, "ip/route", tagged)
	t.Logf("tagged route on a before: %v", before)
	t.Logf("tagged route on a after:  %v", a)
	t.Logf("tagged route on b after:  %v", b)
	mustDiffer(t, "ip/route", "active", a, b)
	// The connected routes on ether2 and vrrp-lan went with the link: the
	// dynamic entries churn too, and Select must keep them out.
	if after := dynamicRoutes(t, lab.A); after == dynamicBefore {
		t.Fatalf("router a's dynamic routes did not change: %s", after)
	} else {
		t.Logf("router a's dynamic routes: before %s, after %s", dynamicBefore, after)
	}
	requireClean(t, lab, "ip/route")
}

// dynamicRoutes lists the dst-address of c's dynamic routes, for spotting
// that they changed.
func dynamicRoutes(t *testing.T, c *routeros.Client) string {
	t.Helper()
	var routes []map[string]string
	if err := c.Get(context.Background(), "/ip/route", &routes); err != nil {
		t.Fatal(err)
	}
	var dsts []string
	for _, r := range routes {
		if r["dynamic"] == "true" {
			dsts = append(dsts, r["dst-address"])
		}
	}
	return fmt.Sprint(dsts)
}

// Logging in stamps the user's last-logged-in on the router logged in to.
func TestLabChurnUserLastLoggedIn(t *testing.T) {
	lab := labtest.New(t)
	ctx := context.Background()

	byName := func(e map[string]string) bool { return e["name"] == "lab-ro" }

	// The fixture's read-only user has not logged in anywhere (unless the
	// lab was used by hand before its golden backup: so the wait below is
	// for a change, not for the field to appear). Log in as it on router a
	// only. A REST request authenticates without counting as
	// a login (last-logged-in stays unset), so this is an SSH session, which
	// RouterOS 7.23 does stamp. The SSH port is this instance's: a fixed
	// port would log in to another instance's router, leaving this one's
	// field unset. SSH_ASKPASS answers the password prompt without a
	// terminal.
	before := only(t, lab.A, "user", byName)["last-logged-in"]
	in, err := labtest.InstanceFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	askpass := filepath.Join(t.TempDir(), "askpass.sh")
	if err := os.WriteFile(askpass, []byte("#!/bin/sh\necho lab-ro-pass\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	sshCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ssh := exec.CommandContext(sshCtx, "ssh", "-p", strconv.Itoa(in.Routers[0].SSHPort), "-T",
		"-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null",
		"-o", "PreferredAuthentications=password,keyboard-interactive", "-o", "PubkeyAuthentication=no",
		"-o", "NumberOfPasswordPrompts=1", "-o", "ConnectTimeout=10",
		"lab-ro@"+labtest.Host, ":put mtha-lab-login")
	ssh.Env = append(os.Environ(), "SSH_ASKPASS="+askpass, "SSH_ASKPASS_REQUIRE=force", "DISPLAY=none")
	if out, err := ssh.CombinedOutput(); err != nil {
		t.Fatalf("ssh to router a as lab-ro: %v\n%s", err, out)
	}

	var a, b map[string]string
	eventually(t, "lab-ro's last-logged-in on router a", 15*time.Second, func() error {
		a = only(t, lab.A, "user", byName)
		if got := a["last-logged-in"]; got == "" || got == before {
			return fmt.Errorf("not set or unchanged from %q: %v", before, a)
		}
		return nil
	})
	b = only(t, lab.B, "user", byName)
	mustDiffer(t, "user", "last-logged-in", a, b)
	t.Logf("lab-ro last-logged-in: a=%q b=%q", a["last-logged-in"], b["last-logged-in"])
	requireClean(t, lab, "user")
}

// Each router serves www-ssl with its own self-signed certificate. The pair
// exempts ip/service.certificate for exactly this.
func TestLabChurnOwnCertificateIsExempt(t *testing.T) {
	lab := labtest.New(t)
	ctx := context.Background()

	// The bootstrap already signs a separate certificate on each router,
	// under the same name. They are different certificates.
	name := func(n string) func(map[string]string) bool {
		return func(e map[string]string) bool { return e["name"] == n }
	}
	mustDiffer(t, "certificate", "fingerprint", only(t, lab.A, "certificate", name("lab")), only(t, lab.B, "certificate", name("lab")))

	// A router's own certificate is normally named for it: give b one of
	// its own and serve www-ssl with it, so the difference reaches
	// ip/service.
	if err := lab.B.Post(ctx, "/certificate", map[string]string{"name": "lab-b", "common-name": "lab-b", "key-usage": "tls-server"}, nil); err != nil {
		t.Fatal(err)
	}
	cert := only(t, lab.B, "certificate", name("lab-b"))
	if err := lab.B.Command(ctx, "/certificate/sign", map[string]string{".id": cert[".id"], "ca": "lab-ca"}, nil); err != nil {
		t.Fatalf("sign lab-b: %v", err)
	}
	eventually(t, "lab-b to be signed", 60*time.Second, func() error {
		if c := only(t, lab.B, "certificate", name("lab-b")); c["fingerprint"] == "" || c["private-key"] != "true" {
			return fmt.Errorf("not signed yet: %v", c)
		}
		return nil
	})
	www := wwwSSL(t, lab.B)
	// The response may be lost as www-ssl restarts under the new certificate.
	_ = lab.B.Patch(ctx, "/ip/service/"+www[".id"], map[string]string{"certificate": "lab-b"}, nil)
	eventually(t, "b's www-ssl to serve lab-b", 30*time.Second, func() error {
		return serviceField(lab, "certificate", "lab-b")
	})

	t.Logf("www-ssl certificate: a=%q b=%q", wwwSSL(t, lab.A)["certificate"], wwwSSL(t, lab.B)["certificate"])
	requireExemptionCarries(t, lab, "ip/service", "certificate")
}

// Router b's www-ssl listens on 8443, a on 443 — per-router by design, and
// exempt as ip/service.port.
//
// In the lab both guests listen on 443; the 8443 is docker's host-side
// publish of b's container port 443, which QEMU forwards to the guest's 443
// only. So b is moved to 8443 for real, and a dst-nat redirect on b keeps
// the forwarded 443 reaching it — REST keeps working, and the guest's
// www-ssl genuinely reads port 8443.
func TestLabChurnServicePortIsExempt(t *testing.T) {
	lab := labtest.New(t)
	ctx := context.Background()

	if got := wwwSSL(t, lab.B)["port"]; got != "443" {
		t.Fatalf("b's www-ssl port at baseline = %q, want 443 (the 8443 is the host publish)", got)
	}
	// One background script, so the redirect and the move happen together:
	// the REST connection issuing it does not survive the move.
	script := `/ip/firewall/nat/add chain=dstnat in-interface=ether1 protocol=tcp dst-port=443 action=redirect to-ports=8443 comment="lab: www-ssl on 8443"; /ip/service/set www-ssl port=8443`
	_ = lab.B.Command(ctx, "/execute", map[string]string{"script": script}, nil)
	eventually(t, "b's www-ssl to answer on 8443", 30*time.Second, func() error {
		return serviceField(lab, "port", "8443")
	})

	t.Logf("www-ssl port: a=%q b=%q", wwwSSL(t, lab.A)["port"], wwwSSL(t, lab.B)["port"])
	requireExemptionCarries(t, lab, "ip/service", "port")
}

// After a failover the degraded router's VRRP priority is whatever the
// router's own netwatch set it to. project.md §10.1: runtime state, not
// config, so it must not read as drift.
func TestLabChurnVRRPPriorityAfterFailover(t *testing.T) {
	lab := labtest.New(t)
	ctx := context.Background()

	// What the runtime's netwatch down-script does (internal/runtime,
	// netwatchDownScript), run by router a's own netwatch when a target
	// stops answering: 192.168.88.253 never does.
	if err := lab.A.Post(ctx, "/tool/netwatch", map[string]string{
		"host": "192.168.88.253", "interval": "1s", "timeout": "500ms",
		// Netwatch otherwise holds off for 5m after boot, and the reset
		// before this test has just rebooted the router.
		"startup-delay": "0s",
		"down-script":   `/interface/vrrp/set [find name="vrrp-lan"] priority=50`,
		"comment":       "lab: degrade on loss",
	}, nil); err != nil {
		t.Fatal(err)
	}
	degrade := func(e map[string]string) bool { return e["comment"] == "lab: degrade on loss" }
	eventually(t, "router a's netwatch to see its target down", 30*time.Second, func() error {
		if n := only(t, lab.A, "tool/netwatch", degrade); n["status"] != "down" {
			return fmt.Errorf("netwatch %v", n)
		}
		return nil
	})
	eventually(t, "router a's netwatch to lower its priority", 15*time.Second, func() error {
		if v := onlyVRRP(t, lab.A); v.Priority != "50" {
			var log []map[string]string
			_ = lab.A.Get(ctx, "/log", &log)
			if len(log) > 5 {
				log = log[len(log)-5:]
			}
			return fmt.Errorf("priority %s; netwatch %v; log tail %v", v.Priority, only(t, lab.A, "tool/netwatch", degrade), log)
		}
		return nil
	})
	waitRole(t, lab.B, routeros.RoleMaster)
	waitRole(t, lab.A, routeros.RoleBackup)
	a, b := onlyVRRP(t, lab.A), onlyVRRP(t, lab.B)
	t.Logf("after failover: a priority %s %s, b priority %s %s", a.Priority, a.Role(), b.Priority, b.Role())

	requireExemptionCarries(t, lab, "interface/vrrp", "priority")
	// The VIP moved by election alone this time, with the link still up.
	requireClean(t, lab, "ip/route")
}

// requireClean reads section from both routers and fails with the hunks if
// it drifts under the pair's exemptions.
func requireClean(t *testing.T, lab *labtest.Lab, section string) {
	t.Helper()
	if d := compare(t, lab, section, lab.Pair.Sync.Exempt); !d.Clean() {
		t.Errorf("%s drifts on churn alone:", section)
		for _, h := range d.Hunks {
			t.Errorf("  %s (a:%t b:%t) %+v", h.Identity, h.OnA, h.OnB, h.Changes)
		}
	}
}

// requireExemptionCarries asserts section is clean under the pair's
// exemptions, and that it is the <section>.<field> exemption doing it:
// without it, field — and only field — differs.
func requireExemptionCarries(t *testing.T, lab *labtest.Lab, section, field string) {
	t.Helper()
	exempt := section + "." + field
	var without []string
	found := false
	for _, ex := range lab.Pair.Sync.Exempt {
		if ex == exempt {
			found = true
			continue
		}
		without = append(without, ex)
	}
	if !found {
		t.Fatalf("the lab pair does not exempt %s", exempt)
	}
	d := compare(t, lab, section, without)
	if d.Clean() {
		t.Fatalf("%s is clean even without %s: the churn never reached the diff", section, exempt)
	}
	for _, h := range d.Hunks {
		if len(h.Changes) != 1 || h.Changes[0].Field != field {
			t.Errorf("without %s, %s differs in more than %s: %s %+v", exempt, section, field, h.Identity, h.Changes)
		}
	}
	requireClean(t, lab, section)
}

func compare(t *testing.T, lab *labtest.Lab, section string, exempt []string) diff.SectionDiff {
	t.Helper()
	ctx := context.Background()
	a, err := lab.A.GetSection(ctx, section)
	if err != nil {
		t.Fatalf("router a %s: %v", section, err)
	}
	b, err := lab.B.GetSection(ctx, section)
	if err != nil {
		t.Fatalf("router b %s: %v", section, err)
	}
	if len(model.Select(section, a)) == 0 {
		t.Fatalf("%s: nothing selected on router a", section)
	}
	return diff.Compare(section, a, b, exempt)
}

// only returns the single static entry of section on c that match accepts.
func only(t *testing.T, c *routeros.Client, section string, match func(map[string]string) bool) map[string]string {
	t.Helper()
	var entries []map[string]string
	if err := c.Get(context.Background(), "/"+section, &entries); err != nil {
		t.Fatalf("%s: %v", section, err)
	}
	var found []map[string]string
	for _, e := range entries {
		if e["dynamic"] != "true" && match(e) {
			found = append(found, e)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%s: %d matching entries, want 1: %v", section, len(found), found)
	}
	return found[0]
}

func wwwSSL(t *testing.T, c *routeros.Client) map[string]string {
	t.Helper()
	return only(t, c, "ip/service", func(e map[string]string) bool { return e["name"] == "www-ssl" })
}

// serviceField reports whether router b's www-ssl reads field = want, while
// www-ssl may be restarting under it: a failed request is a reason to retry,
// not to fail. Each probe is a new client, so it does not ride a keep-alive
// connection opened before the restart.
func serviceField(lab *labtest.Lab, field, want string) error {
	var services []map[string]string
	if err := lab.Pollers(time.Second)["b"].Client.Get(context.Background(), "/ip/service?name=www-ssl&dynamic=false", &services); err != nil {
		return err
	}
	if len(services) != 1 || services[0][field] != want {
		return fmt.Errorf("www-ssl %v, want %s %s", services, field, want)
	}
	return nil
}

// mustDiffer fails the test unless a and b disagree on field: the churn has
// to be real before a clean diff means anything.
func mustDiffer(t *testing.T, section, field string, a, b map[string]string) {
	t.Helper()
	if a[field] == b[field] {
		t.Fatalf("%s.%s is %q on both routers: the churn was not generated", section, field, a[field])
	}
}

// eventually retries fn every half second until it returns nil, failing the
// test with its last error after timeout.
func eventually(t *testing.T, what string, timeout time.Duration, fn func() error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		err := fn()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s: %v", timeout, what, err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}
