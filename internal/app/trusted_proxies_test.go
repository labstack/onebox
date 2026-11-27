package app

import (
	"errors"
	"strings"
	"testing"
)

func TestTrustedProxiesValidation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entries string
		want    string   // error text; empty means valid
		path    string   // public error path
		result  []string // normalised list when valid
	}{
		{"ipv4 and ipv6", `["192.0.2.0/24", "2001:db8::/32"]`, "", "", []string{"192.0.2.0/24", "2001:db8::/32"}},
		{"single address as /32 and /128", `["192.0.2.7/32", "2001:db8::7/128"]`, "", "", []string{"192.0.2.7/32", "2001:db8::7/128"}},
		{"sorted, ipv4 first", `["2001:db8::/32", "198.51.100.0/24", "192.0.2.0/24", "192.0.2.0/25"]`, "", "",
			[]string{"192.0.2.0/24", "192.0.2.0/25", "198.51.100.0/24", "2001:db8::/32"}},
		{"duplicates collapse", `["192.0.2.0/24", "2001:db8::/32", "192.0.2.0/24"]`, "", "", []string{"192.0.2.0/24", "2001:db8::/32"}},
		{"empty list", `[]`, "", "", nil},
		{"host bits set", `["10.20.0.5/24"]`, "write 10.20.0.0/24", "spec.proxy.trustedProxies[0]", nil},
		{"ipv6 host bits set", `["2001:db8::1/32"]`, "write 2001:db8::/32", "spec.proxy.trustedProxies[0]", nil},
		{"ipv6 not compressed", `["2001:0db8:0:0:0:0:0:0/32"]`, "write 2001:db8::/32", "spec.proxy.trustedProxies[0]", nil},
		{"ipv6 upper case", `["2001:DB8::/32"]`, "write 2001:db8::/32", "spec.proxy.trustedProxies[0]", nil},
		{"bare ipv4", `["192.0.2.0/24", "192.0.2.7"]`, "write 192.0.2.7/32", "spec.proxy.trustedProxies[1]", nil},
		{"bare ipv6", `["2001:db8::7"]`, "write 2001:db8::7/128", "spec.proxy.trustedProxies[0]", nil},
		{"ipv4 everything", `["0.0.0.0/0"]`, "trust every peer", "spec.proxy.trustedProxies[0]", nil},
		{"ipv6 everything", `["::/0"]`, "trust every peer", "spec.proxy.trustedProxies[0]", nil},
		{"ipv4-mapped ipv6", `["::ffff:0.0.0.0/96"]`, "IPv4-mapped", "spec.proxy.trustedProxies[0]", nil},
		{"not a prefix", `["cdn.example.com"]`, "not a CIDR prefix", "spec.proxy.trustedProxies[0]", nil},
		{"zone", `["fe80::%eth0/64"]`, "not a CIDR prefix", "spec.proxy.trustedProxies[0]", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := loadFixtureBytes([]byte(min+"proxy: {trusted_proxies: "+tc.entries+"}\n"), "ob.yml")
			if tc.want == "" {
				if err != nil {
					t.Fatalf("valid trusted proxies: %v", err)
				}
				if strings.Join(p.Proxy.TrustedProxies, ",") != strings.Join(tc.result, ",") || (tc.result == nil) != (p.Proxy.TrustedProxies == nil) {
					t.Fatalf("trusted proxies = %#v, want %#v", p.Proxy.TrustedProxies, tc.result)
				}
				return
			}
			var projectErr *Error
			if !errors.As(err, &projectErr) || !strings.Contains(projectErr.Message, tc.want) || projectErr.Path != tc.path {
				t.Fatalf("error = %v, want text %q at %s", err, tc.want, tc.path)
			}
		})
	}
}

func TestTrustedProxiesRequireManagedProxy(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"unmanaged", min + "proxy: {managed: false, trusted_proxies: [192.0.2.0/24]}\n"},
		{"no routes, so no managed proxy", base + "workloads: {worker: {image: app:1}}\nproxy: {trusted_proxies: [192.0.2.0/24]}\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadFixtureBytes([]byte(tc.body), "ob.yml")
			var projectErr *Error
			if !errors.As(err, &projectErr) || projectErr.Path != "spec.proxy.trustedProxies" || !strings.Contains(projectErr.Message, "requires a Onebox-managed proxy") {
				t.Fatalf("error = %v", err)
			}
		})
	}
	if _, err := loadFixtureBytes([]byte(base+"workloads: {worker: {image: app:1}}\nproxy: {managed: true, trusted_proxies: [192.0.2.0/24]}\n"), "ob.yml"); err != nil {
		t.Fatalf("an explicitly managed proxy accepts trusted proxies: %v", err)
	}
}

// The authored spelling is camelCase, like proxyProtocol and dnsChallenge.
func TestTrustedProxiesAuthoredSpelling(t *testing.T) {
	p, err := LoadBytes([]byte(`apiVersion: onebox.run/v1alpha2
kind: Application
metadata: {name: ledger}
spec:
  environments: {production: {server: root@192.0.2.10}}
  workloads:
    web:
      image: nginx:1.27
      routes: [{hostname: ledger.example.com, port: 8080}]
  proxy:
    trustedProxies: [198.51.100.0/24, 192.0.2.0/24]
`), "ob.yml")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(p.Proxy.TrustedProxies, ","); got != "192.0.2.0/24,198.51.100.0/24" {
		t.Fatalf("trusted proxies = %s", got)
	}
}
