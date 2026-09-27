//go:build lab

package labtest

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"mtha/internal/config"
)

// Without MTHA_LAB=1 a lab test must refuse before touching a router, even
// when built with -tags lab. The check re-runs a real writing test in a
// child process with the opt-in removed or wrong.
func TestLabGuardRefusesWithoutOptIn(t *testing.T) {
	for _, optIn := range []string{"", "0", "yes"} {
		t.Run("MTHA_LAB="+optIn, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLabRestoreReturnsToBaseline$", "-test.count=1")
			for _, kv := range os.Environ() {
				if !strings.HasPrefix(kv, OptInEnv+"=") {
					cmd.Env = append(cmd.Env, kv)
				}
			}
			if optIn != "" {
				cmd.Env = append(cmd.Env, OptInEnv+"="+optIn)
			}
			out, err := cmd.CombinedOutput()
			if err == nil || !strings.Contains(string(out), "refusing to run") {
				t.Fatalf("want the child to refuse (err %v), got:\n%s", err, out)
			}
		})
	}
}

// The pair file is checked against the instance's endpoints, not trusted.
func TestLabGuardRejectsNonLabPair(t *testing.T) {
	lab := config.RouterConfig{Host: Host, Port: routers[0].port, User: User, InsecureTLS: true}
	labB := config.RouterConfig{Host: Host, Port: routers[1].port, User: User, InsecureTLS: true}
	// Another instance's routers are a lab, but not the one this binary
	// was pointed at and locked.
	otherID := 2
	if instance.ID == 2 {
		otherID = 1
	}
	other, err := NewInstance(otherID)
	if err != nil {
		t.Fatal(err)
	}
	otherA := config.RouterConfig{Host: Host, Port: other.Routers[0].HTTPSPort, User: User, InsecureTLS: true}
	otherB := config.RouterConfig{Host: Host, Port: other.Routers[1].HTTPSPort, User: User, InsecureTLS: true}
	cases := map[string]map[string]config.RouterConfig{
		"another host":          {"a": {Host: "10.0.0.2", Port: lab.Port, User: User, InsecureTLS: true}, "b": labB},
		"another port":          {"a": lab, "b": {Host: Host, Port: 9443, User: User, InsecureTLS: true}},
		"another user":          {"a": {Host: Host, Port: lab.Port, User: "mtha", InsecureTLS: true}, "b": labB},
		"a and b swapped":       {"a": labB, "b": lab},
		"third router":          {"a": lab, "b": labB, "c": lab},
		"another instance":      {"a": otherA, "b": otherB},
		"half another instance": {"a": lab, "b": otherB},
	}
	for name, rs := range cases {
		if err := checkPair(&config.Pair{Name: "lab", Routers: rs}); err == nil {
			t.Errorf("%s: pair accepted", name)
		}
	}
	if err := checkPair(&config.Pair{Name: "lab", Routers: map[string]config.RouterConfig{"a": lab, "b": labB}}); err != nil {
		t.Errorf("the lab pair itself was rejected: %v", err)
	}
}

// The pair file the suite runs from is the instance's.
func TestLabPairFileIsTheInstances(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []int{1, 2, MaxInstance} {
		in, err := NewInstance(id)
		if err != nil {
			t.Fatal(err)
		}
		path, err := pairFileFor(root, in)
		if err != nil {
			t.Fatalf("instance %d: %v", id, err)
		}
		f, err := config.Load(path)
		if err != nil {
			t.Fatalf("instance %d: %v", id, err)
		}
		pair, err := f.Pair(PairName)
		if err != nil {
			t.Fatalf("instance %d: %v", id, err)
		}
		for i, r := range routersOf(in) {
			want := config.RouterConfig{Host: Host, Port: in.Routers[i].HTTPSPort, User: User, InsecureTLS: true}
			if got := pair.Routers[r.key]; got != want {
				t.Errorf("instance %d router %s = %+v, want %+v", id, r.key, got, want)
			}
		}
	}
}

// docker must show the instance's own container -- the lab image, created
// by compose as that router's service in that instance's project -- running
// and publishing the port. Anything else is not the lab.
func TestLabGuardChecksDocker(t *testing.T) {
	for _, r := range routers {
		if err := checkContainer(r); err != nil {
			t.Fatalf("the real lab container was rejected: %v", err)
		}
	}
	mutate := map[string]func(*router){
		"unpublished port":  func(r *router) { r.port = 9443 },
		"the other's port":  func(r *router) { r.port = routers[1].port },
		"missing container": func(r *router) { r.container = "mtha-lab-no-such-container" },
		"another project":   func(r *router) { r.project = "mtha-lab-elsewhere" },
		"another service":   func(r *router) { r.service = routers[1].service },
	}
	for name, m := range mutate {
		r := routers[0]
		m(&r)
		if err := checkContainer(r); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// Containers dressed up as this instance's router a -- the lab image, a
// published port, even its compose project and service labels -- that
// compose did not create as that router are refused. Each impostor's name
// and port are this instance's own (20000+100n+99 is never a lab port), so
// suites on other instances running at the same time cannot collide with it.
func TestLabGuardRefusesImpostorContainer(t *testing.T) {
	r := routers[0]
	r.container = fmt.Sprintf("%s-impostor", instance.Project)
	r.port = 20000 + 100*instance.ID + 99
	_ = exec.Command("docker", "rm", "-f", r.container).Run() // left by a killed run
	// Each is only created, not started: starting the lab entrypoint
	// would boot a guest, and checkContainer settles who a container is
	// before whether it is running.
	impostors := []struct {
		name  string
		flags []string
		want  string // in the refusal
	}{
		// Inherits whatever project and service labels compose last built
		// the image with.
		{"docker run of the lab image", nil, "not created by docker compose"},
		{"hand-labelled with the instance's project and service", []string{
			"--label", "com.docker.compose.project=" + r.project, "--label", "com.docker.compose.service=" + r.service,
		}, "not created by docker compose"},
		// Every label compose would set, but another entrypoint, so what
		// answers on the port need not be the guest.
		{"another entrypoint", []string{"--entrypoint", "socat",
			"--label", "com.docker.compose.project=" + r.project, "--label", "com.docker.compose.service=" + r.service,
			"--label", "com.docker.compose.config-hash=x", "--label", "com.docker.compose.oneoff=False",
		}, "entrypoint"},
	}
	for _, imp := range impostors {
		t.Run(imp.name, func(t *testing.T) {
			args := append([]string{"create", "--name", r.container, "-p", fmt.Sprintf("%d:443", r.port)}, imp.flags...)
			if out, err := exec.Command("docker", append(args, labImage)...).CombinedOutput(); err != nil {
				t.Fatalf("create impostor: %v\n%s", err, out)
			}
			defer func() { _ = exec.Command("docker", "rm", "-f", r.container).Run() }()
			err := checkContainer(r)
			if err == nil || !strings.Contains(err.Error(), imp.want) {
				t.Fatalf("got %v, want a refusal mentioning %q", err, imp.want)
			}
			t.Logf("refused: %v", err)
		})
	}
}

func TestLabParseUptime(t *testing.T) {
	cases := map[string]time.Duration{
		"11s":        11 * time.Second,
		"24m27s":     24*time.Minute + 27*time.Second,
		"1w2d3h4m5s": 7*24*time.Hour + 2*24*time.Hour + 3*time.Hour + 4*time.Minute + 5*time.Second,
	}
	for in, want := range cases {
		if got, err := parseUptime(in); err != nil || got != want {
			t.Errorf("parseUptime(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "12", "5x", "m"} {
		if _, err := parseUptime(bad); err == nil {
			t.Errorf("parseUptime(%q) accepted", bad)
		}
	}
}
