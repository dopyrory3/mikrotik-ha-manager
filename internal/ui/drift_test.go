package ui

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"mtha/internal/config"
	"mtha/internal/routeros"
)

func testClient(t *testing.T, handler http.Handler) *routeros.Client {
	t.Helper()
	srv := httptest.NewTLSServer(handler)
	t.Cleanup(srv.Close)
	return routeros.New(routeros.Config{
		Host:        strings.TrimPrefix(srv.URL, "https://"),
		User:        "mtha",
		Password:    "pw",
		InsecureTLS: true,
		Timeout:     2 * time.Second,
	})
}

// One section failing must not discard the sections that already succeeded
// (see fetchDrift's doc comment).
func TestFetchDriftReturnsPartialResultsOnSectionError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/rest/ip/service", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `[{"name":"api","port":"443"}]`)
	})
	mux.HandleFunc("/rest/ip/dhcp-server", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	client := testClient(t, mux)

	result := fetchDrift(context.Background(), client, client, []string{"ip/service", "ip/dhcp-server"}, nil)

	if result.err == nil {
		t.Fatal("expected an error from the failing section")
	}
	sd, ok := result.data["ip/service"]
	if !ok {
		t.Fatal("expected ip/service to still be present in the partial results")
	}
	if !sd.Clean() {
		t.Errorf("ip/service should be clean (identical on both sides), got %+v", sd.Hunks)
	}
	if _, ok := result.data["ip/dhcp-server"]; ok {
		t.Error("failing section should not have an entry in the result data")
	}
}

func TestFetchDriftSkipsExemptSections(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/rest/ip/service", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `[]`)
	})
	client := testClient(t, mux)

	result := fetchDrift(context.Background(), client, client,
		[]string{"ip/service", "system/identity"}, []string{"system/identity"})

	if result.err != nil {
		t.Fatalf("unexpected error: %v", result.err)
	}
	if _, ok := result.data["system/identity"]; ok {
		t.Error("exempt section should not be fetched")
	}
}

// driftSections drives what the drift screen renders; it must agree with
// fetchDrift about which sections are whole-section exempt, or an exempt
// section renders as falsely "clean" (see renderSectionList).
func TestNewFiltersExemptSectionsFromDriftSections(t *testing.T) {
	pair := &config.Pair{
		Sync: config.SyncConfig{
			Sections: []string{"ip/service", "system/identity"},
			Exempt:   []string{"system/identity"},
		},
	}
	m := New(pair, false, nil)

	if len(m.driftSections) != 1 || m.driftSections[0] != "ip/service" {
		t.Errorf("driftSections = %v, want [ip/service]", m.driftSections)
	}
}
