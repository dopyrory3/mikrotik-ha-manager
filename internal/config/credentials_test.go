package config

import (
	"strings"
	"testing"
)

// ResolvePassword is the only way a secret enters mtha: one environment
// variable per router, MTHA_<PAIR>_<ROUTER>_PASSWORD with both parts
// upper-cased, never the pair file (project.md §5.1).
func TestResolvePassword(t *testing.T) {
	t.Setenv("MTHA_CORE_A_PASSWORD", "secret-a")
	t.Setenv("MTHA_CORE_B_PASSWORD", "secret-b")
	t.Setenv("MTHA_EDGE_A_PASSWORD", "")

	tests := []struct {
		name, pair, router string
		want               string
		wantErr            string // the variable the error must name
	}{
		{"router a", "core", "a", "secret-a", ""},
		{"router b is its own variable", "core", "b", "secret-b", ""},
		{"names are upper-cased", "Core", "B", "secret-b", ""},
		{"set but empty counts as missing", "edge", "a", "", "MTHA_EDGE_A_PASSWORD"},
		{"unset", "edge", "b", "", "MTHA_EDGE_B_PASSWORD"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolvePassword(tt.pair, tt.router)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("ResolvePassword(%q, %q): %v", tt.pair, tt.router, err)
				}
				if got != tt.want {
					t.Errorf("ResolvePassword(%q, %q) = %q, want %q", tt.pair, tt.router, got, tt.want)
				}
				return
			}
			if err == nil {
				t.Fatalf("ResolvePassword(%q, %q) = %q, want an error", tt.pair, tt.router, got)
			}
			if got != "" {
				t.Errorf("ResolvePassword returned %q alongside its error, want \"\"", got)
			}
			// The operator must be told exactly which variable to export.
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to name %s", err, tt.wantErr)
			}
		})
	}
}
