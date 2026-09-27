package labtest

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// Instance 1 is the original lab, value for value, so every existing
// instruction and the suite's own guard keep meaning what they meant.
func TestInstanceOneIsTheOriginalLab(t *testing.T) {
	in, err := NewInstance(1)
	if err != nil {
		t.Fatal(err)
	}
	want := Instance{ID: 1, Project: "mtha-lab", Routers: [2]InstanceRouter{
		{Service: "router1", Container: "mikrotik-router1", HTTPSPort: 443, SSHPort: 2211, Volume: "mtha-lab-router1-data",
			DefaultMAC: "02:00:00:00:01:10", BridgeMAC: "02:00:00:00:01:11"},
		{Service: "router2", Container: "mikrotik-router2", HTTPSPort: 8443, SSHPort: 2212, Volume: "mtha-lab-router2-data",
			DefaultMAC: "02:00:00:00:02:10", BridgeMAC: "02:00:00:00:02:11"},
	}}
	if in != want {
		t.Errorf("instance 1 = %+v\nwant %+v", in, want)
	}
}

// No two instances, and no two routers, share anything that would collide
// on one host: a name, a port, a volume or a MAC.
func TestInstancesNeverCollide(t *testing.T) {
	seen := map[string]string{}
	claim := func(kind, v, owner string) {
		t.Helper()
		if prev, ok := seen[kind+" "+v]; ok {
			t.Errorf("%s %s used by %s and %s", kind, v, prev, owner)
		}
		seen[kind+" "+v] = owner
	}
	for id := 1; id <= MaxInstance; id++ {
		in, err := NewInstance(id)
		if err != nil {
			t.Fatal(err)
		}
		claim("project", in.Project, in.Project)
		for _, r := range in.Routers {
			owner := in.Project + "/" + r.Service
			claim("container", r.Container, owner)
			claim("port", strconv.Itoa(r.HTTPSPort), owner)
			claim("port", strconv.Itoa(r.SSHPort), owner)
			claim("volume", r.Volume, owner)
			claim("mac", r.DefaultMAC, owner)
			claim("mac", r.BridgeMAC, owner)
			if id > 1 && (r.HTTPSPort < 1024 || r.HTTPSPort > 65535) {
				t.Errorf("%s: HTTPS port %d outside 1024-65535", owner, r.HTTPSPort)
			}
		}
	}
	for _, bad := range []int{0, -1, MaxInstance + 1} {
		if _, err := NewInstance(bad); err == nil {
			t.Errorf("instance %d accepted", bad)
		}
	}
}

func TestInstanceFromEnv(t *testing.T) {
	for in, want := range map[string]int{"": 1, "1": 1, "2": 2, "99": 99} {
		t.Setenv(InstanceEnv, in)
		got, err := InstanceFromEnv()
		if err != nil || got.ID != want {
			t.Errorf("%s=%q: instance %d, %v; want %d", InstanceEnv, in, got.ID, err, want)
		}
	}
	for _, bad := range []string{"0", "02", "+2", " 2", "2x", "100", "443", "localhost:443"} {
		t.Setenv(InstanceEnv, bad)
		if in, err := InstanceFromEnv(); err == nil {
			t.Errorf("%s=%q accepted as instance %d", InstanceEnv, bad, in.ID)
		}
	}
}

// testlab/lab.sh derives the same identity as NewInstance, and between them
// they set every variable testlab/docker-compose.yml reads.
func TestInstanceMatchesLabScript(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	compose, err := os.ReadFile(filepath.Join(root, "testlab", "docker-compose.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var composeVars []string
	for _, m := range regexp.MustCompile(`\$\{(MTHA_LAB_[A-Z0-9_]+):-`).FindAllStringSubmatch(string(compose), -1) {
		if !slices.Contains(composeVars, m[1]) {
			composeVars = append(composeVars, m[1])
		}
	}
	slices.Sort(composeVars)

	for _, id := range []int{1, 2, 3, 10, MaxInstance} {
		in, err := NewInstance(id)
		if err != nil {
			t.Fatal(err)
		}
		want := in.ComposeEnv()
		slices.Sort(want)

		out, err := exec.Command("sh", filepath.Join(root, "testlab", "lab.sh"), "env", strconv.Itoa(id)).Output()
		if err != nil {
			t.Fatalf("lab.sh env %d: %v", id, err)
		}
		var got []string
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			got = append(got, strings.TrimPrefix(line, "export "))
		}
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("instance %d:\nlab.sh env: %v\nComposeEnv: %v", id, got, want)
		}

		var names []string
		for _, kv := range want {
			names = append(names, kv[:strings.IndexByte(kv, '=')])
		}
		if !slices.Equal(names, composeVars) {
			t.Errorf("ComposeEnv sets %v; docker-compose.yml reads %v", names, composeVars)
		}
	}
}
