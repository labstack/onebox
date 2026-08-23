package app

import (
	"strings"
	"testing"
)

func projectWithJump(jump string) string {
	return "api_version: onebox.run/v1\napp: ledger\n" +
		"environments: {production: {server: root@10.20.0.10, jump: " + jump + "}}\n" +
		"image: nginx\ndomain: ledger.example.com\nport: 8080\n"
}

func TestScalarJumpExpandsToUserHostAndPort(t *testing.T) {
	resolved, err := LoadBytes([]byte(projectWithJump("deploy@bastion.example.com:2222")), "ob.yml")
	if err != nil {
		t.Fatal(err)
	}
	jump := resolved.Environments["production"].Jump
	if jump == nil {
		t.Fatal("jump = nil, want the declared bastion")
	}
	if jump.User != "deploy" || jump.Host != "bastion.example.com" || jump.Port != 2222 {
		t.Fatalf("jump = %#v", jump)
	}
}

func TestScalarJumpWithoutUserOrPortKeepsThoseImplicit(t *testing.T) {
	resolved, err := LoadBytes([]byte(projectWithJump("bastion.example.com")), "ob.yml")
	if err != nil {
		t.Fatal(err)
	}
	jump := resolved.Environments["production"].Jump
	if jump == nil || jump.User != "" || jump.Host != "bastion.example.com" || jump.Port != 0 {
		t.Fatalf("jump = %#v", jump)
	}
}

func TestObjectJumpDecodes(t *testing.T) {
	resolved, err := LoadBytes([]byte(projectWithJump("{host: bastion.example.com, user: deploy, port: 2222}")), "ob.yml")
	if err != nil {
		t.Fatal(err)
	}
	jump := resolved.Environments["production"].Jump
	if jump == nil || jump.User != "deploy" || jump.Host != "bastion.example.com" || jump.Port != 2222 {
		t.Fatalf("jump = %#v", jump)
	}
}

func TestAbsentJumpLeavesTheEnvironmentDirect(t *testing.T) {
	resolved, err := LoadBytes([]byte(min), "ob.yml")
	if err != nil {
		t.Fatal(err)
	}
	if jump := resolved.Environments["production"].Jump; jump != nil {
		t.Fatalf("jump = %#v, want nil", jump)
	}
}

// A jump that only fails at dial time is a jump that fails after the operator
// has already been told the plan is sound, so every malformed form is rejected
// while the project is still being read.
func TestInvalidJumpIsRejectedAtLoad(t *testing.T) {
	invalid := map[string]string{
		"port out of range":  "deploy@bastion.example.com:99999",
		"port not numeric":   "deploy@bastion.example.com:ssh",
		"object port high":   "{host: bastion.example.com, port: 70000}",
		"missing host":       "{user: deploy}",
		"empty scalar":       "\"\"",
		"unbracketed ipv6":   "deploy@2001:db8::1",
		"two at signs":       "deploy@bastion@example.com",
		"bad user character": "\"bad/user@bastion.example.com\"",
		"port inside host":   "{host: \"bastion.example.com:2222\"}",
		"user inside host":   "{host: \"deploy@bastion.example.com\"}",
		"bad user object":    "{host: bastion.example.com, user: \"bad/user\"}",
	}
	for name, jump := range invalid {
		t.Run(name, func(t *testing.T) {
			_, err := LoadBytes([]byte(projectWithJump(jump)), "ob.yml")
			if err == nil {
				t.Fatalf("jump %q was accepted", jump)
			}
			if !strings.Contains(err.Error(), "jump") {
				t.Fatalf("error does not name the jump field: %v", err)
			}
		})
	}
}

// An IPv6 bastion must be expressible: bracketed in the scalar form, where the
// grammar needs the brackets to find the port, and bare in the object form,
// where each part is already its own field.
func TestIPv6JumpIsAcceptedInBothForms(t *testing.T) {
	forms := map[string]string{
		"scalar bracketed":      `"deploy@[2001:db8::1]"`,
		"scalar bracketed port": `"deploy@[2001:db8::1]:2222"`,
		"object bare":           `{host: "2001:db8::1", user: deploy}`,
		// Brackets carried over from the scalar spelling are stripped rather
		// than refused: the author meant the address, not a hostname with
		// punctuation in it.
		"object bracketed": `{host: "[2001:db8::1]", user: deploy}`,
	}
	for name, form := range forms {
		t.Run(name, func(t *testing.T) {
			resolved, err := LoadBytes([]byte(projectWithJump(form)), "ob.yml")
			if err != nil {
				t.Fatal(err)
			}
			if host := resolved.Environments["production"].Jump.Host; host != "2001:db8::1" {
				t.Fatalf("jump host = %q, want the bare literal", host)
			}
		})
	}
}

func TestIPv6JumpRoutesWithBracketsOnlyWhenAPortIsWritten(t *testing.T) {
	route := func(form string) string {
		t.Helper()
		resolved, err := LoadBytes([]byte(projectWithJump(form)), "ob.yml")
		if err != nil {
			t.Fatal(err)
		}
		return resolved.Environments["production"].Route().String()
	}
	if got, want := route(`"deploy@[2001:db8::1]"`), "root@10.20.0.10 via deploy@2001:db8::1"; got != want {
		t.Fatalf("route = %q, want %q", got, want)
	}
	if got, want := route(`"deploy@[2001:db8::1]:2222"`), "root@10.20.0.10 via deploy@[2001:db8::1]:2222"; got != want {
		t.Fatalf("route = %q, want %q", got, want)
	}
}
