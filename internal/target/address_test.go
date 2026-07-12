package target

import "testing"

func TestParse(t *testing.T) {
	tests := []struct {
		raw, user, host, port, destination string
	}{
		{"example.com", "", "example.com", "22", "deploy@example.com"},
		{"root@example.com", "root", "example.com", "22", "root@example.com"},
		{"example.com:2222", "", "example.com", "2222", "deploy@example.com:2222"},
		{"root@10.0.0.5:22", "root", "10.0.0.5", "22", "root@10.0.0.5:22"},
		{"[2001:db8::1]", "", "2001:db8::1", "22", "deploy@[2001:db8::1]"},
		{"root@[2001:db8::1]:2200", "root", "2001:db8::1", "2200", "root@[2001:db8::1]:2200"},
	}
	for _, test := range tests {
		t.Run(test.raw, func(t *testing.T) {
			address, err := Parse(test.raw)
			if err != nil {
				t.Fatal(err)
			}
			if address.User != test.user || address.Host != test.host || address.Port != test.port {
				t.Fatalf("parse = %#v", address)
			}
			if got := address.Destination("deploy"); got != test.destination {
				t.Fatalf("destination = %q, want %q", got, test.destination)
			}
		})
	}
}

func TestParseRejectsInvalidTargets(t *testing.T) {
	invalid := []string{
		"", " deploy@example.com", "deploy @example.com", "deploy@@example.com",
		"bad/user@example.com", "-deploy@example.com", "deploy@-example.com",
		"deploy@example_com", "deploy@example.com:0", "deploy@example.com:65536",
		"deploy@example.com:ssh", "deploy@2001:db8::1", "deploy@[example.com]",
		"deploy@example.com\nignore-this",
	}
	for _, raw := range invalid {
		t.Run(raw, func(t *testing.T) {
			if _, err := Parse(raw); err == nil {
				t.Fatalf("invalid target %q was accepted", raw)
			}
		})
	}
}
