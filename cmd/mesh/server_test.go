package main

import (
	"strings"
	"testing"
)

func TestCertHostWarning(t *testing.T) {
	cases := []struct {
		name  string
		addr  string
		hosts []string
		want  bool
	}{
		// The case that produces "certificate is valid for 127.0.0.1, ::1" on
		// the node: every interface, but nothing the certificate could match.
		{name: "wildcard without hosts", addr: "0.0.0.0:8443", want: true},
		{name: "wildcard v6 without hosts", addr: "[::]:8443", want: true},
		{name: "all interfaces without hosts", addr: ":8443", want: true},
		{name: "wildcard with hosts", addr: "0.0.0.0:8443", hosts: []string{"192.168.3.237"}},
		// A concrete listen address is added to the certificate by runServer.
		{name: "specific address", addr: "192.168.3.237:8443"},
		{name: "loopback", addr: "127.0.0.1:8443"},
		{name: "localhost", addr: "localhost:8443"},
		{name: "unparseable", addr: "not-an-address"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := certHostWarning(tc.addr, tc.hosts)
			if (got != "") != tc.want {
				t.Fatalf("certHostWarning(%q, %v) = %q, want warning=%v", tc.addr, tc.hosts, got, tc.want)
			}
		})
	}
}

func TestResolveToken(t *testing.T) {
	if got, err := resolveToken("abc123", strings.NewReader("ignored")); err != nil || got != "abc123" {
		t.Fatalf("a literal token returned %q (err %v)", got, err)
	}
	if got, err := resolveToken("-", strings.NewReader("  from-stdin\n")); err != nil || got != "from-stdin" {
		t.Fatalf("stdin token returned %q (err %v)", got, err)
	}
	if _, err := resolveToken("-", strings.NewReader("   \n")); err == nil {
		t.Fatal("an empty stdin was accepted as a token")
	}
}

func TestReachableAddr(t *testing.T) {
	cases := []struct {
		addr  string
		hosts []string
		want  string
	}{
		// A wildcard must never be printed as the address a client should dial.
		{addr: "https://[::]:8443", hosts: []string{"192.168.3.237"}, want: "https://192.168.3.237:8443"},
		{addr: "https://0.0.0.0:8443", hosts: []string{"gateway.local"}, want: "https://gateway.local:8443"},
		{addr: "https://[::]:8443", want: "https://127.0.0.1:8443"},
		{addr: "https://127.0.0.1:8443", want: "https://127.0.0.1:8443"},
	}
	for _, tc := range cases {
		if got := reachableAddr(tc.addr, tc.hosts); got != tc.want {
			t.Fatalf("reachableAddr(%q, %v) = %q, want %q", tc.addr, tc.hosts, got, tc.want)
		}
	}
}
