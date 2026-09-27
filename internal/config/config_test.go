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
