package target

import "testing"

func TestAddressString(t *testing.T) {
	tests := map[string]struct {
		address Address
		want    string
	}{
		"user and explicit port": {Address{User: "deploy", Host: "example.com", Port: "2222", ExplicitPort: true}, "deploy@example.com:2222"},
		"user without port":      {Address{User: "root", Host: "example.com", Port: "22"}, "root@example.com"},
		"port without user":      {Address{Host: "example.com", Port: "2222", ExplicitPort: true}, "example.com:2222"},
		"bare host":              {Address{Host: "example.com", Port: "22"}, "example.com"},
		"ipv6 with port":         {Address{User: "root", Host: "2a01:4ff::1", Port: "2222", ExplicitPort: true}, "root@[2a01:4ff::1]:2222"},
		"ipv6 without port":      {Address{User: "root", Host: "2a01:4ff::1", Port: "22"}, "root@2a01:4ff::1"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if got := test.address.String(); got != test.want {
				t.Fatalf("String() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestRouteStringNamesOnlyTheTargetWithoutAJump(t *testing.T) {
	route := Route{Target: Address{User: "root", Host: "10.20.0.10", Port: "22"}}
	if got := route.String(); got != "root@10.20.0.10" {
		t.Fatalf("String() = %q, want %q", got, "root@10.20.0.10")
	}
}

func TestRouteStringNamesTheJumpAfterTheTarget(t *testing.T) {
	route := Route{
		Target: Address{User: "root", Host: "10.20.0.10", Port: "22"},
		Jump:   &Address{User: "deploy", Host: "bastion.example.com", Port: "2222", ExplicitPort: true},
	}
	want := "root@10.20.0.10 via deploy@bastion.example.com:2222"
	if got := route.String(); got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}
