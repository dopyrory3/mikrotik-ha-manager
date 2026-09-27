package runtime

import (
	"strings"
	"testing"

	"mtha/internal/config"
)

func fullInstance() config.VRRPInstance {
	return config.VRRPInstance{
		Interface: "vrrp-lan",
		On:        "ether2",
		VRID:      1,
		Addresses: []string{"10.0.0.1/24"},
	}
}

func testRuntimeConfig() config.RuntimeConfig {
	return config.RuntimeConfig{
		NetwatchTargets:  []string{"1.1.1.1"},
		PriorityMaster:   200,
		PriorityBackup:   100,
		PriorityDegraded: 50,
	}
}

func TestBuildPlanSkipsInstanceWithoutDeployFields(t *testing.T) {
	pair := &config.Pair{VRRP: []config.VRRPInstance{{Interface: "vrrp-lan"}}}

	plans, err := BuildPlan(pair)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	for _, router := range routers {
		for _, op := range plans[router].Ops {
			if op.Section == "interface/vrrp" {
				t.Errorf("router %s: got a vrrp Op for an instance with no deploy fields set", router)
			}
		}
	}
}

func TestBuildPlanErrorsOnPartialInstance(t *testing.T) {
	pair := &config.Pair{VRRP: []config.VRRPInstance{{Interface: "vrrp-lan", On: "ether2"}}}

	_, err := BuildPlan(pair)
	if err == nil {
		t.Fatal("expected an error for a partially-configured instance")
	}
	if !strings.Contains(err.Error(), "vrid") {
		t.Errorf("error = %q, want it to name the missing field", err)
	}
}

func TestBuildPlanFullInstanceProducesExpectedOps(t *testing.T) {
	pair := &config.Pair{
		VRRP:    []config.VRRPInstance{fullInstance()},
		Runtime: testRuntimeConfig(),
	}

	plans, err := BuildPlan(pair)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}

	for _, router := range routers {
		ops := plans[router].Ops
		var sections []string
		for _, op := range ops {
			sections = append(sections, op.Section)
		}
		want := []string{"interface/vrrp", "ip/address", "tool/netwatch", "system/scheduler"}
		for _, w := range want {
			found := false
			for _, s := range sections {
				if s == w {
					found = true
				}
			}
			if !found {
				t.Errorf("router %s: missing an Op for section %s, got %v", router, w, sections)
			}
		}
	}

	aVRRP := findOp(t, plans["a"].Ops, "interface/vrrp")
	bVRRP := findOp(t, plans["b"].Ops, "interface/vrrp")
	if aVRRP.Fields["priority"] != "200" {
		t.Errorf("router a priority = %q, want 200 (priority_master)", aVRRP.Fields["priority"])
	}
	if bVRRP.Fields["priority"] != "100" {
		t.Errorf("router b priority = %q, want 100 (priority_backup)", bVRRP.Fields["priority"])
	}
	if !strings.HasPrefix(aVRRP.Fields["on-master"], onMasterMarker("vrrp-lan")) {
		t.Errorf("on-master = %q, want it to start with the marker", aVRRP.Fields["on-master"])
	}
}

func findOp(t *testing.T, ops []Op, section string) Op {
	t.Helper()
	for _, op := range ops {
		if op.Section == section {
			return op
		}
	}
	t.Fatalf("no Op for section %s", section)
	return Op{}
}

func togglePair() *config.Pair {
	wan := config.VRRPInstance{Interface: "vrrp-wan", On: "ether1", VRID: 2, Addresses: []string{"203.0.113.1/29"}}
	rt := testRuntimeConfig()
	rt.Toggles = config.TogglesConfig{
		VRRP:        "vrrp-lan",
		DHCPServers: []string{"dhcp-lan"},
		Routes:      []string{"mtha-default"},
	}
	return &config.Pair{VRRP: []config.VRRPInstance{fullInstance(), wan}, Runtime: rt}
}

func vrrpOpNamed(t *testing.T, ops []Op, name string) Op {
	t.Helper()
	for _, op := range ops {
		if op.Section == "interface/vrrp" && op.MatchValue == name {
			return op
		}
	}
	t.Fatalf("no vrrp Op for %s", name)
	return Op{}
}

func TestBuildPlanRendersTogglesIntoNamedInstanceOnly(t *testing.T) {
	plans, err := BuildPlan(togglePair())
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}

	for _, router := range routers {
		lan := vrrpOpNamed(t, plans[router].Ops, "vrrp-lan")
		master, backup := lan.Fields["on-master"], lan.Fields["on-backup"]

		wantMaster := []string{
			`:foreach i in=[/ip/route find where comment="mtha-default"] do={/ip/route set $i disabled=no}`,
			`:foreach i in=[/ip/dhcp-server find where name="dhcp-lan"] do={/ip/dhcp-server set $i disabled=no}`,
		}
		wantBackup := []string{
			`:foreach i in=[/ip/dhcp-server find where name="dhcp-lan"] do={/ip/dhcp-server set $i disabled=yes}`,
			`:foreach i in=[/ip/route find where comment="mtha-default"] do={/ip/route set $i disabled=yes}`,
		}
		assertLinesInOrder(t, router+" on-master", master, wantMaster)
		assertLinesInOrder(t, router+" on-backup", backup, wantBackup)

		if !strings.HasPrefix(master, onMasterMarker("vrrp-lan")+"\n") {
			t.Errorf("router %s on-master = %q, want the marker as its first line", router, master)
		}
		if !strings.HasPrefix(backup, onBackupMarker("vrrp-lan")+"\n") {
			t.Errorf("router %s on-backup = %q, want the marker as its first line", router, backup)
		}

		wan := vrrpOpNamed(t, plans[router].Ops, "vrrp-wan")
		if strings.Contains(wan.Fields["on-master"], "dhcp-server") || strings.Contains(wan.Fields["on-backup"], "ip/route") {
			t.Errorf("router %s: toggles leaked into vrrp-wan's scripts: %q / %q",
				router, wan.Fields["on-master"], wan.Fields["on-backup"])
		}
	}
}

// assertLinesInOrder checks want appear in script as whole lines, in order.
func assertLinesInOrder(t *testing.T, what, script string, want []string) {
	t.Helper()
	lines := strings.Split(script, "\n")
	next := 0
	for _, l := range lines {
		if next < len(want) && l == want[next] {
			next++
		}
	}
	if next != len(want) {
		t.Errorf("%s = %q, want lines %q in that order", what, script, want)
	}
}

func TestBuildPlanWithoutTogglesIsLogOnly(t *testing.T) {
	pair := &config.Pair{VRRP: []config.VRRPInstance{fullInstance()}, Runtime: testRuntimeConfig()}

	plans, err := BuildPlan(pair)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	op := vrrpOpNamed(t, plans["a"].Ops, "vrrp-lan")
	want := onMasterMarker("vrrp-lan") + "\n:log info \"mtha: vrrp-lan transitioned to master\""
	if op.Fields["on-master"] != want {
		t.Errorf("on-master = %q, want log-only %q", op.Fields["on-master"], want)
	}
}

func TestBuildPlanRejectsBadToggles(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*config.Pair)
		wantErr string
	}{
		{"no instance named", func(p *config.Pair) { p.Runtime.Toggles.VRRP = "" }, `"vrrp" is required`},
		{"unknown instance", func(p *config.Pair) { p.Runtime.Toggles.VRRP = "vrrp-nope" }, "not defined"},
		{"tracked-only instance", func(p *config.Pair) {
			p.VRRP = append(p.VRRP, config.VRRPInstance{Interface: "vrrp-mgmt"})
			p.Runtime.Toggles.VRRP = "vrrp-mgmt"
		}, "never deployed"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pair := togglePair()
			tc.mutate(pair)
			_, err := BuildPlan(pair)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("err = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestQuoteEscapesScriptMetacharacters(t *testing.T) {
	got := quote(`a"b$c\d`)
	want := `"a\"b\$c\\d"`
	if got != want {
		t.Errorf("quote = %s, want %s", got, want)
	}
}

// Each router's netwatch scripts drop it to priority_degraded and restore
// its own base priority — B to priority_backup, never promoting it to
// priority_master — and only ever touch mtha-tagged VRRP interfaces.
func TestBuildPlanNetwatchScriptsArePerRouter(t *testing.T) {
	rt := testRuntimeConfig()
	rt.NetwatchTargets = []string{"1.1.1.1", "8.8.8.8"}
	pair := &config.Pair{VRRP: []config.VRRPInstance{fullInstance()}, Runtime: rt}

	plans, err := BuildPlan(pair)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}

	const (
		managed = `[/interface/vrrp find where comment~"^mtha:vrrp:"]`
		allUp   = `:if ([/tool/netwatch print count-only where comment~"^mtha:netwatch:" disabled=no status=down] = 0) do=`
	)
	wantUp := map[string]string{"a": "priority=200", "b": "priority=100"}
	for _, router := range routers {
		n := 0
		for _, op := range plans[router].Ops {
			if op.Section != "tool/netwatch" {
				continue
			}
			n++
			up, down := op.Fields["up-script"], op.Fields["down-script"]
			target := op.Fields["host"]

			if !strings.HasPrefix(up, netwatchMarker(target)+"\n") || !strings.HasPrefix(down, netwatchMarker(target)+"\n") {
				t.Errorf("router %s %s: scripts must start with the marker: %q / %q", router, target, up, down)
			}
			if !strings.Contains(up, wantUp[router]+"}") || strings.Count(up, "priority=") != 1 {
				t.Errorf("router %s %s up-script = %q, want it to restore %s only", router, target, up, wantUp[router])
			}
			if !strings.Contains(up, allUp) {
				t.Errorf("router %s %s up-script = %q, want it gated on every mtha netwatch being up", router, target, up)
			}
			if !strings.Contains(down, "priority=50}") || strings.Count(down, "priority=") != 1 {
				t.Errorf("router %s %s down-script = %q, want priority_degraded (50)", router, target, down)
			}
			for _, script := range []string{up, down} {
				if !strings.Contains(script, managed) || strings.Contains(script, "[/interface/vrrp find]") {
					t.Errorf("router %s %s script = %q, want it limited to mtha-managed VRRP interfaces", router, target, script)
				}
			}
		}
		if n != 2 {
			t.Errorf("router %s: %d netwatch ops, want 2", router, n)
		}
	}
}

// Priority is created at the router's base value but accepted at either
// base or degraded afterwards, since netwatch moves it on purpose.
func TestBuildPlanVRRPPriorityIsMutable(t *testing.T) {
	pair := &config.Pair{VRRP: []config.VRRPInstance{fullInstance()}, Runtime: testRuntimeConfig()}
	plans, err := BuildPlan(pair)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	want := map[string][]string{"a": {"200", "50"}, "b": {"100", "50"}}
	for _, router := range routers {
		got := vrrpOpNamed(t, plans[router].Ops, "vrrp-lan").Mutable["priority"]
		if strings.Join(got, ",") != strings.Join(want[router], ",") {
			t.Errorf("router %s accepted priorities = %v, want %v", router, got, want[router])
		}
	}
}
