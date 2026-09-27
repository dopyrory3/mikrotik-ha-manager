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
