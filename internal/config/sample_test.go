package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteSampleParses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mtha", "pairs.yaml")

	if err := WriteSample(path); err != nil {
		t.Fatalf("WriteSample: %v", err)
	}

	file, err := Load(path)
	if err != nil {
		t.Fatalf("Load sample: %v", err)
	}
	if len(file.Pairs) != 1 {
		t.Fatalf("got %d pairs, want 1", len(file.Pairs))
	}

	pair, err := file.Pair("core")
	if err != nil {
		t.Fatalf("Pair: %v", err)
	}
	if pair.Routers["a"].Host != "10.0.0.2" {
		t.Errorf("router a host = %q, want 10.0.0.2", pair.Routers["a"].Host)
	}
	if pair.Routers["b"].Port != 8443 {
		t.Errorf("router b port = %d, want 8443", pair.Routers["b"].Port)
	}
	if len(pair.Sync.Sections) == 0 {
		t.Error("sample has no sync sections")
	}
	if pair.Runtime.PriorityMaster != 200 {
		t.Errorf("priority_master = %d, want 200", pair.Runtime.PriorityMaster)
	}
	exemptPort := false
	for _, e := range pair.Sync.Exempt {
		exemptPort = exemptPort || e == "ip/service.port"
	}
	if !exemptPort {
		t.Error("sample syncs ip/service with router b's REST API on 8443, so it must exempt ip/service.port")
	}
	toggles := pair.Runtime.Toggles
	if toggles.VRRP != "vrrp-lan" || !toggles.Enabled() {
		t.Errorf("runtime.toggles = %+v, want vrrp-lan with toggles set", toggles)
	}
}

func TestWriteSampleRefusesOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pairs.yaml")

	if err := WriteSample(path); err != nil {
		t.Fatalf("WriteSample: %v", err)
	}
	if err := os.WriteFile(path, []byte("pairs: []\n"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	if err := WriteSample(path); err == nil {
		t.Fatal("second WriteSample succeeded, want error")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "pairs: []\n" {
		t.Errorf("existing file was modified: %q", data)
	}
}

func TestWriteSampleReportsStatError(t *testing.T) {
	// A path "under" a regular file fails to stat with ENOTDIR, which is
	// not "does not exist" and so must not be treated as free to write.
	parent := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(parent, nil, 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	if err := WriteSample(filepath.Join(parent, "pairs.yaml")); err == nil {
		t.Fatal("WriteSample under a regular file succeeded, want error")
	}
}

func TestWriteSampleReportsWriteError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := filepath.Join(t.TempDir(), "ro")
	if err := os.Mkdir(dir, 0o500); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := WriteSample(filepath.Join(dir, "pairs.yaml")); err == nil {
		t.Fatal("WriteSample into a read-only directory succeeded, want error")
	}
}
