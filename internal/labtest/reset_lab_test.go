//go:build lab

package labtest_test

import (
	"context"
	"os"
	"testing"
	"time"

	"mtha/internal/labtest"
	"mtha/internal/routeros"
)

// Proves the reset: mutate both routers in ways a real test might — a
// firewall rule, the identity, a VRRP priority that hands router b
// mastership, and a file — then restore mid-test and check every one is undone and the
// pair has re-elected router a.
func TestLabRestoreReturnsToBaseline(t *testing.T) {
	lab := labtest.New(t)
	ctx := context.Background()

	idA, err := lab.A.Identity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := lab.A.Post(ctx, "/ip/firewall/filter", map[string]string{
		"chain": "input", "action": "drop", "protocol": "tcp", "dst-port": "2323", "comment": "mtha-lab-reset-probe",
	}, nil); err != nil {
		t.Fatal(err)
	}
	if err := lab.A.Command(ctx, "/system/identity/set", map[string]string{"name": "mtha-lab-mutated"}, nil); err != nil {
		t.Fatal(err)
	}
	vrrpB := onlyVRRP(t, lab.B)
	if err := lab.B.Patch(ctx, "/interface/vrrp/"+vrrpB.ID, map[string]string{"priority": "250"}, nil); err != nil {
		t.Fatal(err)
	}
	waitRole(t, lab.B, routeros.RoleMaster) // the mutation really took effect on the device
	// Files are outside a backup's scope; the reset deletes new ones.
	if err := lab.A.Post(ctx, "/file", map[string]string{"name": "mtha-lab-probe.txt", "contents": "x"}, nil); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	lab.Restore()
	t.Logf("restore of both routers took %s", time.Since(start).Round(100*time.Millisecond))

	var rules []map[string]string
	if err := lab.A.Get(ctx, "/ip/firewall/filter?comment=mtha-lab-reset-probe", &rules); err != nil {
		t.Fatal(err)
	}
	if len(rules) != 0 {
		t.Errorf("probe rule survived the restore: %v", rules)
	}
	var files []map[string]string
	if err := lab.A.Get(ctx, "/file?name=mtha-lab-probe.txt", &files); err != nil || len(files) != 0 {
		t.Errorf("probe file survived the restore: %v, %v", files, err)
	}
	if got, err := lab.A.Identity(ctx); err != nil || got.Name != idA.Name {
		t.Errorf("identity after restore = %v, %v; want %q", got, err, idA.Name)
	}
	if v := onlyVRRP(t, lab.B); v.Priority != "100" || v.Role() != routeros.RoleBackup {
		t.Errorf("router b after restore: priority %s role %s, want 100 backup", v.Priority, v.Role())
	}
	if v := onlyVRRP(t, lab.A); v.Role() != routeros.RoleMaster {
		t.Errorf("router a after restore: role %s, want master", v.Role())
	}
}

func onlyVRRP(t *testing.T, c *routeros.Client) routeros.VRRPInstance {
	t.Helper()
	vrrp, err := c.VRRP(context.Background())
	if err != nil || len(vrrp) != 1 {
		t.Fatalf("VRRP = %+v, %v; want one instance", vrrp, err)
	}
	return vrrp[0]
}

func waitRole(t *testing.T, c *routeros.Client, want routeros.VRRPRole) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		v := onlyVRRP(t, c)
		if v.Role() == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("VRRP role %s, want %s", v.Role(), want)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// Proves the exception path: a test that leaves a router unable to answer
// REST at all (www-ssl disabled, so not even a backup load can be issued)
// is recovered by recreating the containers. It takes about a minute, so it
// only runs with MTHA_LAB_RECREATE=1.
func TestLabRecreateRecoversUnreachableRouter(t *testing.T) {
	if os.Getenv("MTHA_LAB_RECREATE") != "1" {
		t.Skip("recreates both lab containers (~1 min); set MTHA_LAB_RECREATE=1 to run")
	}
	// Cleanups run last-registered first, so registering this before New
	// runs it after New's reset: the recreated pair must be answering and
	// back at baseline.
	var lab *labtest.Lab
	t.Cleanup(func() {
		if lab == nil || t.Failed() {
			return
		}
		if v := onlyVRRP(t, lab.B); v.Role() != routeros.RoleBackup {
			t.Errorf("router b after recreate: role %s, want backup", v.Role())
		}
	})
	lab = labtest.New(t, labtest.RecreateOnCleanup())
	ctx := context.Background()

	var services []map[string]string
	if err := lab.B.Get(ctx, "/ip/service?name=www-ssl&dynamic=false", &services); err != nil || len(services) != 1 {
		t.Fatalf("www-ssl service on b: %v, %v", services, err)
	}
	// The response may be lost as the service goes down under it.
	_ = lab.B.Patch(ctx, "/ip/service/"+services[0][".id"], map[string]string{"disabled": "true"}, nil)
	// A fresh client each probe: an open keep-alive connection outlives
	// the service being disabled.
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(500 * time.Millisecond) {
		if _, err := lab.Pollers(time.Second)["b"].Client.Identity(ctx); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("router b still answers with www-ssl disabled")
		}
	}
}
