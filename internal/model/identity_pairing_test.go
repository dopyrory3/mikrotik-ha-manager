package model_test

import (
	"reflect"
	"testing"

	"mtha/internal/diff"
	"mtha/internal/model"
)

// A natural key is not unique on every router. docs/design-questions.md §1
// relies on diff.Compare pairing repeated identities by occurrence rather
// than BuildIdentities deduplicating them; this pins that for each section
// whose identity can repeat.
func TestRepeatedNaturalKeyPairsByOccurrence(t *testing.T) {
	cases := []struct {
		section string
		a, b    []model.Entry
		want    []diff.Hunk
	}{
		{
			section: "ip/firewall/address-list",
			a: []model.Entry{
				{"list": "blocked", "address": "198.51.100.1", "comment": "one"},
				{"list": "blocked", "address": "198.51.100.1", "comment": "two"},
			},
			b: []model.Entry{
				{"list": "blocked", "address": "198.51.100.1", "comment": "one"},
			},
			want: []diff.Hunk{
				{Identity: "blocked|198.51.100.1", Occurrence: 1, OnA: true},
			},
		},
		{
			section: "ip/dns/static",
			a: []model.Entry{
				{"name": "example", "type": "TXT", "text": "one"},
				{"name": "example", "type": "TXT", "text": "two"},
			},
			b: []model.Entry{
				{"name": "example", "type": "TXT", "text": "one"},
				{"name": "example", "type": "TXT", "text": "changed"},
			},
			want: []diff.Hunk{
				{Identity: "example|TXT", Occurrence: 1, OnA: true, OnB: true, Changes: []model.FieldChange{
					{Field: "text", A: "two", B: "changed"},
				}},
			},
		},
		{
			section: "ip/dhcp-server/lease",
			a: []model.Entry{
				{"server": "dhcp-lab", "mac-address": "02:00:00:00:AA:01", "address": "192.168.88.50"},
			},
			b: []model.Entry{
				{"server": "dhcp-lab", "mac-address": "02:00:00:00:AA:01", "address": "192.168.88.50"},
				{"server": "dhcp-lab", "mac-address": "02:00:00:00:AA:01", "address": "192.168.88.51"},
			},
			want: []diff.Hunk{
				{Identity: "dhcp-lab|02:00:00:00:AA:01", Occurrence: 1, OnB: true},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.section, func(t *testing.T) {
			got := diff.Compare(tc.section, tc.a, tc.b, nil).Hunks
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}
