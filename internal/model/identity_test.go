package model

import (
	"reflect"
	"testing"
)

func TestBuildIdentitiesPrefersComment(t *testing.T) {
	entries := []Entry{
		{"chain": "input", "comment": "allow-ssh"},
	}
	got := BuildIdentities("ip/firewall/filter", entries)
	want := []string{"allow-ssh"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestBuildIdentitiesFirewallFallsBackToChainOrdinal(t *testing.T) {
	entries := []Entry{
		{"chain": "input"},
		{"chain": "input"},
		{"chain": "forward"},
	}
	got := BuildIdentities("ip/firewall/filter", entries)
	want := []string{"input#1", "input#2", "forward#1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestBuildIdentitiesDNSStaticUsesAddress(t *testing.T) {
	entries := []Entry{
		{"name": "host1", "address": "10.0.0.5"},
	}
	got := BuildIdentities("ip/dns/static", entries)
	want := []string{"10.0.0.5"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestBuildIdentitiesDefaultUsesName(t *testing.T) {
	entries := []Entry{
		{"name": "backup-config"},
	}
	got := BuildIdentities("system/scheduler", entries)
	want := []string{"backup-config"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestBuildIdentitiesAddressListUsesListAndAddress(t *testing.T) {
	entries := []Entry{
		{"list": "blocklist", "address": "10.0.0.5"},
		{"list": "vpn-allowed", "address": "10.0.0.5"},
	}
	got := BuildIdentities("ip/firewall/address-list", entries)
	want := []string{"blocklist|10.0.0.5", "vpn-allowed|10.0.0.5"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestBuildIdentitiesRoute(t *testing.T) {
	entries := []Entry{
		{"dst-address": "0.0.0.0/0", "gateway": "10.0.0.1"},
	}
	got := BuildIdentities("ip/route", entries)
	want := []string{"0.0.0.0/0->10.0.0.1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}
