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
