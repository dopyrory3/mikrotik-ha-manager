//go:build lab

package labtest

import (
	"context"
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

// The pair file is checked against the lab's fixed endpoints, not trusted.
func TestLabGuardRejectsNonLabPair(t *testing.T) {
	lab := config.RouterConfig{Host: Host, Port: 443, User: User, InsecureTLS: true}
	labB := config.RouterConfig{Host: Host, Port: 8443, User: User, InsecureTLS: true}
	cases := map[string]map[string]config.RouterConfig{
		"another host":    {"a": {Host: "10.0.0.2", Port: 443, User: User, InsecureTLS: true}, "b": labB},
		"another port":    {"a": lab, "b": {Host: Host, Port: 9443, User: User, InsecureTLS: true}},
		"another user":    {"a": {Host: Host, Port: 443, User: "mtha", InsecureTLS: true}, "b": labB},
		"a and b swapped": {"a": labB, "b": lab},
		"third router":    {"a": lab, "b": labB, "c": lab},
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

// A port docker does not publish for the container is not the lab.
func TestLabGuardChecksDockerPublishesThePort(t *testing.T) {
	r := routers[0]
	if err := checkContainer(r); err != nil {
		t.Fatalf("the real lab container was rejected: %v", err)
	}
	r.port = 9443
	if err := checkContainer(r); err == nil {
		t.Error("a port the container does not publish was accepted")
	}
	r.container = "mtha-lab-no-such-container"
	if err := checkContainer(r); err == nil {
		t.Error("a missing container was accepted")
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
