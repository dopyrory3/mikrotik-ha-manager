package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pairs.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func TestLoadRejectsInvalidFiles(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{"not yaml", "pairs: [\n", "parse pair file"},
		{"wrong shape", "pairs: {name: core}\n", "parse pair file"},
		// Every pair is exactly routers "a" and "b" (project.md §5.1); the
		// poller map and every screen key off those names.
		{"missing router a", "pairs:\n  - name: core\n    routers:\n      b: { host: 10.0.0.3 }\n", `pair "core": missing router "a"`},
		{"missing router b", "pairs:\n  - name: core\n    routers:\n      a: { host: 10.0.0.2 }\n", `pair "core": missing router "b"`},
		{"negative port", "pairs:\n  - name: core\n    routers:\n      a: { host: 10.0.0.2, port: -1 }\n      b: { host: 10.0.0.3 }\n", `router "a": invalid port -1`},
		{"port too large", "pairs:\n  - name: core\n    routers:\n      a: { host: 10.0.0.2 }\n      b: { host: 10.0.0.3, port: 65536 }\n", `router "b": invalid port 65536`},
		// A later pair's error is reported even when the first is valid.
		{"second pair invalid", "pairs:\n  - name: core\n    routers:\n      a: { host: 10.0.0.2 }\n      b: { host: 10.0.0.3 }\n  - name: edge\n    routers:\n      a: { host: 10.1.0.2 }\n", `pair "edge": missing router "b"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			file, err := Load(writeFile(t, tt.yaml))
			if err == nil {
				t.Fatalf("Load = %+v, want error containing %q", file, tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.yaml")
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load of a missing file succeeded")
	}
	if !errors.Is(err, fs.ErrNotExist) || !strings.Contains(err.Error(), path) {
		t.Errorf("error = %q, want a not-exist error naming %s", err, path)
	}
}

func TestLoadAcceptsPortBounds(t *testing.T) {
	path := writeFile(t, "pairs:\n  - name: core\n    routers:\n      a: { host: 10.0.0.2 }\n      b: { host: 10.0.0.3, port: 65535, user: mtha, insecure_tls: true }\n")

	file, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	pair, err := file.Pair("core")
	if err != nil {
		t.Fatalf("Pair: %v", err)
	}
	// Port 0 (omitted) means the standard HTTPS port, not an error.
	if got := pair.Routers["a"].Port; got != 0 {
		t.Errorf("router a port = %d, want 0 when omitted", got)
	}
	want := RouterConfig{Host: "10.0.0.3", Port: 65535, User: "mtha", InsecureTLS: true}
	if got := pair.Routers["b"]; got != want {
		t.Errorf("router b = %+v, want %+v", got, want)
	}
}

func TestLoadEmptyFileHasNoPairs(t *testing.T) {
	// An empty file is not a parse error; cmd/mtha reports "no pairs".
	file, err := Load(writeFile(t, ""))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(file.Pairs) != 0 {
		t.Errorf("got %d pairs, want 0", len(file.Pairs))
	}
}

func TestPairLookup(t *testing.T) {
	f := &File{Pairs: []Pair{{Name: "core"}, {Name: "edge"}}}

	p, err := f.Pair("edge")
	if err != nil {
		t.Fatalf("Pair(edge): %v", err)
	}
	// The pointer must be into f, not a copy of the loop variable.
	if p != &f.Pairs[1] {
		t.Error("Pair(edge) did not return a pointer to the file's own entry")
	}

	if _, err := f.Pair("dmz"); err == nil || !strings.Contains(err.Error(), `"dmz"`) {
		t.Errorf("Pair(dmz) error = %v, want a not-found error naming dmz", err)
	}
}

func TestDefaultPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	got, err := DefaultPath()
	if err != nil {
		t.Fatalf("DefaultPath: %v", err)
	}
	if want := filepath.Join(home, ".config", "mtha", "pairs.yaml"); got != want {
		t.Errorf("DefaultPath = %q, want %q", got, want)
	}

	t.Setenv("HOME", "")
	if got, err := DefaultPath(); err == nil {
		t.Errorf("DefaultPath with no HOME = %q, want an error", got)
	}
}

// Load warns, without failing, when a synced referent is listed after a
// section that names its entries (docs/design-questions.md §3, option D).
func TestLoadWarnsAboutSectionOrder(t *testing.T) {
	pair := func(sections ...string) string {
		return "pairs:\n  - name: core\n    routers:\n      a: { host: 10.0.0.2 }\n      b: { host: 10.0.0.3 }\n    sync:\n      sections: [" + strings.Join(sections, ", ") + "]\n"
	}
	warn := func(referent, referrer string) string {
		return `pair "core": sync section ` + referent + " is listed after " + referrer + ", which refers to it; list " + referent + " first so its entries exist before anything that names them is created"
	}
	tests := []struct {
		name string
		yaml string
		want []string
	}{
		{"referents first", pair("ip/firewall/address-list", "ip/firewall/filter", "ip/dhcp-server", "ip/dhcp-server/lease", "system/script", "system/scheduler"), nil},
		{"lease before server", pair("ip/dhcp-server/lease", "ip/dhcp-server"), []string{warn("ip/dhcp-server", "ip/dhcp-server/lease")}},
		{"scheduler before script", pair("system/scheduler", "user", "system/script"), []string{warn("system/script", "system/scheduler")}},
		{"filter and nat before address-list", pair("ip/firewall/filter", "ip/firewall/nat", "ip/firewall/address-list"), []string{
			warn("ip/firewall/address-list", "ip/firewall/filter"),
			warn("ip/firewall/address-list", "ip/firewall/nat"),
		}},
		{"referent not synced", pair("system/scheduler", "ip/dhcp-server/lease"), nil},
		{"no sync sections", pair(), nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			file, err := Load(writeFile(t, tt.yaml))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if strings.Join(file.Warnings, "\n") != strings.Join(tt.want, "\n") {
				t.Errorf("Warnings:\n%s\nwant:\n%s", strings.Join(file.Warnings, "\n"), strings.Join(tt.want, "\n"))
			}
		})
	}
}

// testlab/pairs.yaml keeps ip/firewall/filter first on purpose (the smoke
// test drives the Drift screen to it without moving the cursor), so it
// loads with the one address-list warning and no other.
func TestLabPairFileWarnsOnlyAboutAddressList(t *testing.T) {
	file, err := Load(filepath.Join("..", "..", "testlab", "pairs.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, w := range file.Warnings {
		if !strings.Contains(w, "ip/firewall/address-list is listed after ip/firewall/") {
			t.Errorf("unexpected warning: %s", w)
		}
	}
	if len(file.Warnings) == 0 {
		t.Error("expected the address-list order warning")
	}
}
