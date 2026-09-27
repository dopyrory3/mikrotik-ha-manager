//go:build lab

package labtest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"mtha/internal/config"
	"mtha/internal/poll"
	"mtha/internal/routeros"
)

// The lab's fixed coordinates. They are constants, not configuration, so
// that nothing short of editing this file can point the suite at another
// router; the pair file is checked against them rather than trusted.
const (
	Host     = "localhost"
	User     = "admin"
	PairName = "lab"

	// OptInEnv must be "1" for any lab test to run.
	OptInEnv = "MTHA_LAB"
	// PasswordEnv overrides the lab admin password, like provision.sh's
	// ROUTER_PASS; the default is the one testlab/bootstrap-guest.py sets.
	PasswordEnv     = "MTHA_LAB_PASSWORD"
	defaultPassword = "London12"

	// GoldenBackup is the /system/backup/save name of the baseline each
	// test is restored to.
	GoldenBackup = "mtha-lab-golden"
	// dirtyMarker is a file (not configuration, so a restore does not
	// remove it) present on a router while a writing test runs. Finding
	// one at startup means an earlier run died before its cleanup.
	dirtyMarker = "mtha-lab-dirty.txt"

	// The documented baseline (testlab/provision.sh).
	VRRPName = "vrrp-lan"
	VIP      = "192.168.88.1/24"
)

// fixtureCounts is how many static (dynamic != "true") entries
// provision.sh's fixture leaves in each section, the same on both routers.
// checkBaseline holds each router to it, so a test that adds or removes an
// entry and claims to be ReadOnly is caught, and a half-provisioned or
// doubled fixture is refused. Keep it in step with populate_fixture.
var fixtureCounts = map[string]int{
	"ip/firewall/filter":       11,
	"ip/firewall/nat":          2,
	"ip/firewall/mangle":       1,
	"ip/firewall/raw":          1,
	"ip/firewall/address-list": 3,
	"ip/dns/static":            3,
	"ip/route":                 2,
	"ip/pool":                  1,
	"ip/dhcp-server":           1,
	"ip/dhcp-server/network":   1,
	"ip/dhcp-server/lease":     1,
	"system/script":            1,
	"system/scheduler":         1,
	"user":                     2,
	"tool/netwatch":            2,
}

// router is one lab router: where it is and what baseline looks like on it.
type router struct {
	key       string // pair file key, "a" or "b"
	container string // docker-compose container_name
	port      int    // host port published for its www-ssl
	ether2    string // provision.sh's address on ether2
	priority  string // provision.sh's vrrp-lan priority
	role      routeros.VRRPRole
}

var routers = []router{
	{key: "a", container: "mikrotik-router1", port: 443, ether2: "192.168.88.2/24", priority: "200", role: routeros.RoleMaster},
	{key: "b", container: "mikrotik-router2", port: 8443, ether2: "192.168.88.3/24", priority: "100", role: routeros.RoleBackup},
}

// Timeouts for the slow parts of a reset. A restore reboots the CHR guest,
// which is back in roughly 15-20s under KVM; VRRP then needs a few seconds
// to elect router a (priority 200, preempting).
const (
	rebootTimeout   = 3 * time.Minute
	baselineTimeout = 90 * time.Second
	commandTimeout  = 10 * time.Minute // provision.sh, docker compose
)

// Lab is a test's handle on the pair, at baseline when New returns.
type Lab struct {
	t testing.TB

	// Pair is the "lab" pair from testlab/pairs.yaml.
	Pair *config.Pair
	// PairFile is that file's path, for runs of the mtha binary.
	PairFile string
	// A and B are REST clients for the two routers, for arranging a test
	// and asserting on device state.
	A, B *routeros.Client

	root     string
	password string
}

// Option adjusts how New treats a test.
type Option func(*options)

type options struct {
	readOnly bool
	recreate bool
}

// ReadOnly declares that the test never writes to a router: its cleanup
// only checks the baseline still holds (restoring if it does not) instead
// of rebooting both routers.
func ReadOnly() Option { return func(o *options) { o.readOnly = true } }

// RecreateOnCleanup resets by recreating both containers instead of
// restoring the golden backup. It is the exception path, for tests that
// leave a router unable to answer REST at all (a lockout, a disabled
// www-ssl): a backup restore needs a working login to be issued. It costs
// about a minute plus provision.sh, so use it only where a restore cannot
// work.
func RecreateOnCleanup() Option { return func(o *options) { o.recreate = true } }

// suite is shared by every test in one test binary.
var suite struct {
	once     sync.Once
	setupErr error
	root     string
	pairFile string
	password string

	// files is each router's file names when the golden backup was taken.
	// A backup does not cover the filesystem, so a reset deletes any file
	// created since (a pre-apply backup, an export) to keep it from
	// accumulating across runs.
	files map[string]map[string]bool

	mu     sync.Mutex
	broken error // a reset failed and could not be recovered
}

// New checks the guard and the target, establishes the baseline once per
// test binary, and registers the reset that returns both routers to it when
// the test ends — pass or fail.
func New(t testing.TB, opts ...Option) *Lab {
	t.Helper()
	var o options
	for _, opt := range opts {
		opt(&o)
	}

	if err := checkOptIn(); err != nil {
		t.Fatal(err)
	}
	suite.once.Do(func() { suite.setupErr = setup() })
	if suite.setupErr != nil {
		t.Fatalf("lab setup: %v", suite.setupErr)
	}
	suite.mu.Lock()
	broken := suite.broken
	suite.mu.Unlock()
	if broken != nil {
		t.Fatalf("lab left off baseline by an earlier test, not running: %v", broken)
	}

	pair, err := loadPair(suite.pairFile)
	if err != nil {
		t.Fatal(err)
	}
	l := &Lab{
		t:        t,
		Pair:     pair,
		PairFile: suite.pairFile,
		A:        client(routers[0], suite.password, 10*time.Second),
		B:        client(routers[1], suite.password, 10*time.Second),
		root:     suite.root,
		password: suite.password,
	}

	if o.readOnly {
		t.Cleanup(l.checkUntouched)
		return l
	}
	if err := eachRouter(func(r router) error {
		return markDirty(client(r, l.password, 10*time.Second), t.Name())
	}); err != nil {
		t.Fatalf("mark lab in use: %v", err)
	}
	t.Cleanup(func() { l.reset(o.recreate) })
	return l
}

// Client returns the REST client for router "a" or "b".
func (l *Lab) Client(key string) *routeros.Client {
	switch key {
	case "a":
		return l.A
	case "b":
		return l.B
	}
	l.t.Fatalf("no lab router %q", key)
	return nil
}

// Pollers builds real pollers for both routers, as cmd/mtha does, for
// driving ui.Model.
func (l *Lab) Pollers(interval time.Duration) map[poll.RouterKey]*poll.Poller {
	pollers := make(map[poll.RouterKey]*poll.Poller, len(routers))
	for _, r := range routers {
		pollers[poll.RouterKey(r.key)] = poll.New(poll.RouterKey(r.key), client(r, l.password, 10*time.Second), interval)
	}
	return pollers
}

// Restore returns both routers to the golden backup now, mid-test. The
// cleanup still restores again at the end.
func (l *Lab) Restore() {
	l.t.Helper()
	if err := restoreGolden(l.password); err != nil {
		l.t.Fatalf("restore golden backup: %v", err)
	}
	if err := eachRouter(func(r router) error {
		return markDirty(client(r, l.password, 10*time.Second), l.t.Name())
	}); err != nil {
		l.t.Fatalf("mark lab in use: %v", err)
	}
}

// Env is the environment mtha resolves the "lab" pair's passwords from
// (MTHA_LAB_A_PASSWORD, ...), for runs of the binary.
func (l *Lab) Env() []string {
	env := make([]string, 0, len(routers))
	for _, r := range routers {
		env = append(env, fmt.Sprintf("MTHA_%s_%s_PASSWORD=%s", strings.ToUpper(PairName), strings.ToUpper(r.key), l.password))
	}
	return env
}

// reset is every writing test's cleanup. It restores the golden backup;
// if that fails (the router no longer answers, the login was changed, the
// backup file was deleted) it falls back to recreating the containers, so
// the next test still starts at baseline. Only if both fail is the lab
// marked broken, which fails every later test fast rather than letting it
// run against unknown state.
func (l *Lab) reset(recreateFirst bool) {
	start := time.Now()
	var err error
	if recreateFirst {
		err = recreate(l.root, l.password)
	} else if err = restoreGolden(l.password); err != nil {
		l.t.Errorf("restore golden backup failed, falling back to recreating the containers: %v", err)
		err = recreate(l.root, l.password)
	}
	if err != nil {
		suite.mu.Lock()
		suite.broken = err
		suite.mu.Unlock()
		l.t.Errorf("LAB NOT AT BASELINE: %v\nrecover by hand: docker compose -f testlab/docker-compose.yml up -d --force-recreate && ./testlab/provision.sh", err)
		return
	}
	l.t.Logf("lab reset to baseline in %s", time.Since(start).Round(100*time.Millisecond))
}

// checkUntouched is a ReadOnly test's cleanup.
func (l *Lab) checkUntouched() {
	if err := eachRouter(func(r router) error {
		return checkBaseline(context.Background(), client(r, l.password, 10*time.Second), r)
	}); err != nil {
		l.t.Errorf("read-only test left the lab off baseline: %v", err)
		l.reset(false)
	}
}

// checkOptIn is the first half of the guard; the "lab" build tag on this
// file is the other.
func checkOptIn() error {
	if os.Getenv(OptInEnv) != "1" {
		return fmt.Errorf("refusing to run: lab tests write to routers and need %s=1 (use `make test-lab`)", OptInEnv)
	}
	return nil
}

// setup runs once per test binary: it proves the target is the lab, takes
// an exclusive lock on it, recovers from an interrupted earlier run, runs
// provision.sh and saves the golden backup.
func setup() error {
	root, err := repoRoot()
	if err != nil {
		return err
	}
	suite.root = root
	suite.pairFile = filepath.Join(root, "testlab", "pairs.yaml")
	suite.password = os.Getenv(PasswordEnv)
	if suite.password == "" {
		suite.password = defaultPassword
	}

	if _, err := loadPair(suite.pairFile); err != nil {
		return err
	}

	// Test binaries for different packages run concurrently unless -p 1;
	// the lock serialises them, since each resets both routers. It is held
	// until the process exits.
	if err := lockLab(); err != nil {
		return err
	}

	if err := verifyTarget(suite.password); err != nil {
		return fmt.Errorf("target is not the lab: %w", err)
	}

	dirty, err := anyDirty(suite.password)
	if err != nil {
		return err
	}
	if dirty != "" {
		// A previous run died mid-test; its golden backup is the way back.
		fmt.Fprintf(os.Stderr, "labtest: %s; restoring the golden backup before starting\n", dirty)
		if err := restoreGolden(suite.password); err != nil {
			return fmt.Errorf("recover from interrupted run: %w", err)
		}
	}

	if err := provision(root, suite.password); err != nil {
		return err
	}
	if err := waitBaseline(suite.password); err != nil {
		return fmt.Errorf("lab is not at the documented baseline after provision.sh (recreate it: docker compose -f testlab/docker-compose.yml up -d --force-recreate && ./testlab/provision.sh): %w", err)
	}
	if err := eachRouter(func(r router) error { return saveGolden(client(r, suite.password, 30*time.Second)) }); err != nil {
		return err
	}
	return captureFiles(suite.password)
}

// loadPair loads the lab pair and refuses it unless every router in it is
// one of the lab's fixed endpoints.
func loadPair(path string) (*config.Pair, error) {
	f, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	pair, err := f.Pair(PairName)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := checkPair(pair); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return pair, nil
}

func checkPair(pair *config.Pair) error {
	if len(pair.Routers) != len(routers) {
		return fmt.Errorf("pair %q has %d routers, the lab has %d", pair.Name, len(pair.Routers), len(routers))
	}
	for _, r := range routers {
		got, ok := pair.Routers[r.key]
		want := config.RouterConfig{Host: Host, Port: r.port, User: User, InsecureTLS: true}
		if !ok || got != want {
			return fmt.Errorf("pair %q router %q is %+v; lab tests only run against %+v", pair.Name, r.key, got, want)
		}
	}
	return nil
}

// verifyTarget proves each endpoint is a lab container: docker must report
// the named container running and publishing that very port, and what
// answers there must be a RouterOS CHR guest.
func verifyTarget(password string) error {
	return eachRouter(func(r router) error {
		if err := checkContainer(r); err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), rebootTimeout)
		defer cancel()
		res, err := waitFor(ctx, func() (*routeros.SystemResource, error) {
			return client(r, password, 5*time.Second).SystemResource(ctx)
		})
		if err != nil {
			return fmt.Errorf("router %s (https://%s:%d) does not answer as %s: %w", r.key, Host, r.port, User, err)
		}
		if !strings.HasPrefix(res.BoardName, "CHR") {
			return fmt.Errorf("router %s (https://%s:%d) is a %q, not a CHR guest", r.key, Host, r.port, res.BoardName)
		}
		return nil
	})
}

func checkContainer(r router) error {
	out, err := exec.Command("docker", "inspect", r.container).Output()
	if err != nil {
		return fmt.Errorf("docker inspect %s: %w (is the lab up? docker compose -f testlab/docker-compose.yml up -d --build)", r.container, err)
	}
	var info []struct {
		State struct {
			Running bool
		}
		NetworkSettings struct {
			Ports map[string][]struct{ HostPort string }
		}
	}
	if err := json.Unmarshal(out, &info); err != nil || len(info) != 1 {
		return fmt.Errorf("docker inspect %s: unexpected output: %v", r.container, err)
	}
	if !info[0].State.Running {
		return fmt.Errorf("container %s is not running", r.container)
	}
	for _, b := range info[0].NetworkSettings.Ports["443/tcp"] {
		if b.HostPort == strconv.Itoa(r.port) {
			return nil
		}
	}
	return fmt.Errorf("container %s does not publish its www-ssl on host port %d", r.container, r.port)
}

// restoreGolden loads the golden backup on both routers at once, waits for
// each to come back from the reboot a load implies, and then for the pair
// to reach baseline again (VRRP re-elected).
func restoreGolden(password string) error {
	if err := eachRouter(func(r router) error { return loadGolden(r, password) }); err != nil {
		return err
	}
	if err := waitBaseline(password); err != nil {
		return fmt.Errorf("after restore: %w", err)
	}
	return eachRouter(func(r router) error {
		c := client(r, password, 10*time.Second)
		if err := pruneFiles(c, r); err != nil {
			return err
		}
		return clearDirty(c)
	})
}

func loadGolden(r router, password string) error {
	ctx, cancel := context.WithTimeout(context.Background(), rebootTimeout)
	defer cancel()
	c := client(r, password, 10*time.Second)

	if ok, err := fileExists(ctx, c, GoldenBackup+".backup"); err != nil {
		return fmt.Errorf("router %s: %w", r.key, err)
	} else if !ok {
		return fmt.Errorf("router %s: golden backup %s.backup is missing", r.key, GoldenBackup)
	}

	issued := time.Now()
	loadErr := c.Command(ctx, "/system/backup/load", map[string]string{"name": GoldenBackup + ".backup", "password": ""}, nil)
	// The response can be lost to the reboot the load triggers, so an error
	// here is not yet a failure: the uptime settles it. A router whose
	// uptime is shorter than the time since the load was issued has booted
	// since; the pre-load instance's uptime is always longer than that.
	_, err := waitFor(ctx, func() (struct{}, error) {
		res, err := client(r, password, 3*time.Second).SystemResource(ctx)
		if err != nil {
			return struct{}{}, err
		}
		up, err := parseUptime(res.Uptime)
		if err != nil {
			return struct{}{}, err
		}
		if up > time.Since(issued) {
			return struct{}{}, fmt.Errorf("not rebooted yet (uptime %s)", res.Uptime)
		}
		return struct{}{}, nil
	})
	if err != nil {
		return fmt.Errorf("router %s did not reboot into the golden backup (load: %v): %w", r.key, loadErr, err)
	}
	return nil
}

func saveGolden(c *routeros.Client) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := c.Command(ctx, "/system/backup/save", map[string]string{"name": GoldenBackup, "dont-encrypt": "yes"}, nil); err != nil {
		return fmt.Errorf("save golden backup: %w", err)
	}
	_, err := waitFor(ctx, func() (struct{}, error) {
		ok, err := fileExists(ctx, c, GoldenBackup+".backup")
		if err == nil && !ok {
			err = errors.New("backup file not written yet")
		}
		return struct{}{}, err
	})
	return err
}

// recreate is the exception path: fresh containers (a fresh guest system
// disk, so configuration and files are gone), provision.sh, and a new
// golden backup.
func recreate(root, password string) error {
	if err := run(root, nil, "docker", "compose", "-f", filepath.Join("testlab", "docker-compose.yml"),
		"up", "-d", "--force-recreate", "router1", "router2"); err != nil {
		return err
	}
	if err := verifyTarget(password); err != nil {
		return err
	}
	if err := provision(root, password); err != nil {
		return err
	}
	if err := waitBaseline(password); err != nil {
		return fmt.Errorf("after recreate: %w", err)
	}
	if err := eachRouter(func(r router) error { return saveGolden(client(r, password, 30*time.Second)) }); err != nil {
		return err
	}
	return captureFiles(password)
}

func provision(root, password string) error {
	return run(root, []string{"ROUTER_USER=" + User, "ROUTER_PASS=" + password,
		fmt.Sprintf("A_HTTPS=https://%s:%d", Host, routers[0].port),
		fmt.Sprintf("B_HTTPS=https://%s:%d", Host, routers[1].port),
	}, "sh", filepath.Join("testlab", "provision.sh"))
}

func run(dir string, env []string, name string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, out)
	}
	return nil
}

// waitBaseline waits until both routers match the documented baseline.
func waitBaseline(password string) error {
	ctx, cancel := context.WithTimeout(context.Background(), baselineTimeout)
	defer cancel()
	return eachRouter(func(r router) error {
		_, err := waitFor(ctx, func() (struct{}, error) {
			return struct{}{}, checkBaseline(ctx, client(r, password, 5*time.Second), r)
		})
		return err
	})
}

// checkBaseline compares one router with what provision.sh establishes: its
// ether2 address, vrrp-lan with its priority, the VIP on vrrp-lan, the VRRP
// role that priority wins, and the fixture's entry counts.
func checkBaseline(ctx context.Context, c *routeros.Client, r router) error {
	for section, want := range fixtureCounts {
		var entries []map[string]string
		if err := c.Get(ctx, "/"+section, &entries); err != nil {
			return fmt.Errorf("router %s: %w", r.key, err)
		}
		got := 0
		for _, e := range entries {
			if e["dynamic"] != "true" {
				got++
			}
		}
		if got != want {
			return fmt.Errorf("router %s: %s has %d static entries, the fixture has %d", r.key, section, got, want)
		}
	}

	var addrs []map[string]string
	if err := c.Get(ctx, "/ip/address", &addrs); err != nil {
		return fmt.Errorf("router %s: %w", r.key, err)
	}
	want := map[string]string{r.ether2: "ether2", VIP: VRRPName}
	for _, a := range addrs {
		if want[a["address"]] == a["interface"] && a["disabled"] != "true" {
			delete(want, a["address"])
		}
	}
	if len(want) > 0 {
		return fmt.Errorf("router %s: missing addresses %v", r.key, want)
	}

	vrrp, err := c.VRRP(ctx)
	if err != nil {
		return fmt.Errorf("router %s: %w", r.key, err)
	}
	if len(vrrp) != 1 {
		return fmt.Errorf("router %s: %d VRRP instances, want 1", r.key, len(vrrp))
	}
	v := vrrp[0]
	if v.Name != VRRPName || v.Interface != "ether2" || v.Priority != r.priority || v.Disabled == "true" {
		return fmt.Errorf("router %s: VRRP instance %+v, want %s on ether2 priority %s", r.key, v, VRRPName, r.priority)
	}
	if v.Role() != r.role {
		return fmt.Errorf("router %s: VRRP role %s, want %s", r.key, v.Role(), r.role)
	}
	return nil
}

func captureFiles(password string) error {
	files := make(map[string]map[string]bool, len(routers))
	var mu sync.Mutex
	err := eachRouter(func(r router) error {
		names, err := listFiles(client(r, password, 10*time.Second))
		if err != nil {
			return fmt.Errorf("router %s: %w", r.key, err)
		}
		set := make(map[string]bool, len(names))
		for _, n := range names {
			set[n] = true
		}
		mu.Lock()
		files[r.key] = set
		mu.Unlock()
		return nil
	})
	suite.files = files
	return err
}

// pruneFiles deletes the files created on r since captureFiles, except
// directories. Before the first capture (recovering an interrupted run at
// startup) there is nothing to compare with, so it does nothing.
func pruneFiles(c *routeros.Client, r router) error {
	keep := suite.files[r.key]
	if keep == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var files []map[string]string
	if err := c.Get(ctx, "/file", &files); err != nil {
		return fmt.Errorf("router %s: %w", r.key, err)
	}
	for _, f := range files {
		if keep[f["name"]] || f["type"] == "directory" {
			continue
		}
		if err := c.Delete(ctx, "/file/"+f[".id"]); err != nil {
			return fmt.Errorf("router %s: delete %s: %w", r.key, f["name"], err)
		}
	}
	return nil
}

func listFiles(c *routeros.Client) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var files []map[string]string
	if err := c.Get(ctx, "/file", &files); err != nil {
		return nil, err
	}
	names := make([]string, len(files))
	for i, f := range files {
		names[i] = f["name"]
	}
	return names, nil
}

func markDirty(c *routeros.Client, testName string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if ok, err := fileExists(ctx, c, dirtyMarker); err != nil || ok {
		return err
	}
	return c.Post(ctx, "/file", map[string]string{"name": dirtyMarker, "contents": testName}, nil)
}

func clearDirty(c *routeros.Client) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if ok, err := fileExists(ctx, c, dirtyMarker); err != nil || !ok {
		return err
	}
	return c.Delete(ctx, "/file/"+dirtyMarker)
}

// anyDirty reports which router carries the in-use marker, if any.
func anyDirty(password string) (string, error) {
	for _, r := range routers {
		ok, err := fileExists(context.Background(), client(r, password, 10*time.Second), dirtyMarker)
		if err != nil {
			return "", fmt.Errorf("router %s: %w", r.key, err)
		}
		if ok {
			return fmt.Sprintf("router %s was left mid-test by an earlier run", r.key), nil
		}
	}
	return "", nil
}

func fileExists(ctx context.Context, c *routeros.Client, name string) (bool, error) {
	var files []map[string]string
	if err := c.Get(ctx, "/file?name="+name, &files); err != nil {
		return false, err
	}
	return len(files) > 0, nil
}

func client(r router, password string, timeout time.Duration) *routeros.Client {
	return routeros.New(routeros.Config{
		Host: Host, Port: r.port, User: User, Password: password,
		InsecureTLS: true, Timeout: timeout,
	})
}

// eachRouter runs fn for both routers concurrently and joins their errors.
func eachRouter(fn func(router) error) error {
	errs := make([]error, len(routers))
	var wg sync.WaitGroup
	for i, r := range routers {
		wg.Add(1)
		go func(i int, r router) {
			defer wg.Done()
			errs[i] = fn(r)
		}(i, r)
	}
	wg.Wait()
	return errors.Join(errs...)
}

// waitFor retries fn every half second until it succeeds or ctx ends,
// returning the last error on timeout.
func waitFor[T any](ctx context.Context, fn func() (T, error)) (T, error) {
	for {
		v, err := fn()
		if err == nil {
			return v, nil
		}
		select {
		case <-ctx.Done():
			return v, fmt.Errorf("timed out: %w", err)
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// parseUptime parses RouterOS's "1w2d3h4m5s" form.
func parseUptime(s string) (time.Duration, error) {
	units := map[byte]time.Duration{'w': 7 * 24 * time.Hour, 'd': 24 * time.Hour, 'h': time.Hour, 'm': time.Minute, 's': time.Second}
	var total time.Duration
	n := 0
	digits := false
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c >= '0' && c <= '9':
			n = n*10 + int(c-'0')
			digits = true
		case units[c] != 0 && digits:
			total += time.Duration(n) * units[c]
			n, digits = 0, false
		default:
			return 0, fmt.Errorf("unparseable uptime %q", s)
		}
	}
	if digits || s == "" {
		return 0, fmt.Errorf("unparseable uptime %q", s)
	}
	return total, nil
}

var lockFile *os.File

// lockLab takes an exclusive lock that lives as long as this process.
func lockLab() error {
	path := filepath.Join(os.TempDir(), "mtha-lab.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open lab lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("lock lab: %w", err)
	}
	// Kept reachable (a collected *os.File is closed by its finalizer, which
	// would drop the lock) and never closed: the lock is released when the
	// process exits.
	lockFile = f
	return nil
}

// repoRoot finds the module root (go test runs in the package directory).
func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("cannot find go.mod above the test's directory")
		}
		dir = parent
	}
}
