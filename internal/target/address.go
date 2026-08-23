// Package target parses the stable Onebox SSH target grammar.
package target

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

// Address is a validated [user@]host[:port] target. IPv6 hosts must be
// bracketed, with or without an explicit port.
type Address struct {
	User         string
	Host         string
	Port         string
	ExplicitPort bool
}

// Parse validates the target grammar shared by config validation and the SSH
// transport. Keeping one parser prevents a v1 config from validating one way
// and connecting another way.
func Parse(raw string) (Address, error) {
	if raw == "" || strings.TrimSpace(raw) != raw || strings.ContainsAny(raw, " \t\r\n") {
		return Address{}, fmt.Errorf("must be [user@]host[:port] without whitespace")
	}
	if strings.Count(raw, "@") > 1 {
		return Address{}, fmt.Errorf("must contain at most one @")
	}

	address := Address{Port: "22"}
	hostPort := raw
	if before, after, ok := strings.Cut(raw, "@"); ok {
		if !validUser(before) {
			return Address{}, fmt.Errorf("user %q must start with a letter, digit, or underscore and contain only letters, digits, dot, underscore, or hyphen", before)
		}
		address.User, hostPort = before, after
	}
	if hostPort == "" {
		return Address{}, fmt.Errorf("host is required")
	}

	switch {
	case strings.HasPrefix(hostPort, "["):
		closeIndex := strings.IndexByte(hostPort, ']')
		if closeIndex < 0 {
			return Address{}, fmt.Errorf("bracketed IPv6 host is missing ]")
		}
		if closeIndex == len(hostPort)-1 {
			address.Host = hostPort[1:closeIndex]
		} else {
			host, port, err := net.SplitHostPort(hostPort)
			if err != nil {
				return Address{}, fmt.Errorf("invalid bracketed host and port: %w", err)
			}
			address.Host, address.Port, address.ExplicitPort = host, port, true
		}
		if net.ParseIP(address.Host) == nil || !strings.Contains(address.Host, ":") {
			return Address{}, fmt.Errorf("brackets are only valid around an IPv6 address")
		}
	case strings.Contains(hostPort, ":"):
		if strings.Count(hostPort, ":") != 1 {
			return Address{}, fmt.Errorf("IPv6 hosts must be bracketed")
		}
		host, port, err := net.SplitHostPort(hostPort)
		if err != nil {
			return Address{}, fmt.Errorf("invalid host and port: %w", err)
		}
		address.Host, address.Port, address.ExplicitPort = host, port, true
	default:
		address.Host = hostPort
	}

	if !validHost(address.Host) {
		return Address{}, fmt.Errorf("host %q must be a DNS name, IPv4 address, or bracketed IPv6 address", address.Host)
	}
	port, err := strconv.Atoi(address.Port)
	if err != nil || port < 1 || port > 65535 {
		return Address{}, fmt.Errorf("port %q must be numeric and between 1 and 65535", address.Port)
	}
	return address, nil
}

// ValidUser reports whether user is a legal SSH user in this grammar. It is
// exported so config validation can hold an authored field to the same rule the
// transport dials by, without recomposing a string to parse.
func ValidUser(user string) bool { return validUser(user) }

// ValidHost reports whether host is a legal host: a DNS name, an IPv4 address,
// or an *unbracketed* IPv6 literal. Brackets belong to the scalar grammar,
// where they separate the address from the port; a host field has no port to
// separate.
func ValidHost(host string) bool { return validHost(host) }

func validUser(user string) bool {
	if user == "" || !isUserStart(user[0]) {
		return false
	}
	for i := 1; i < len(user); i++ {
		c := user[i]
		if !isASCIIAlphaNumeric(c) && c != '.' && c != '_' && c != '-' {
			return false
		}
	}
	return true
}

func isUserStart(c byte) bool { return isASCIIAlphaNumeric(c) || c == '_' }

func validHost(host string) bool {
	if ip := net.ParseIP(host); ip != nil {
		return true
	}
	name := strings.TrimSuffix(host, ".")
	if name == "" || len(name) > 253 {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if label == "" || len(label) > 63 || !isASCIIAlphaNumeric(label[0]) || !isASCIIAlphaNumeric(label[len(label)-1]) {
			return false
		}
		for i := 1; i < len(label)-1; i++ {
			if !isASCIIAlphaNumeric(label[i]) && label[i] != '-' {
				return false
			}
		}
	}
	return true
}

func isASCIIAlphaNumeric(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// Destination returns a normalized OpenSSH destination. resolvedUser is used
// when the author omitted a user. The port is deliberately excluded because
// OpenSSH needs it as -p. IPv6 stays unbracketed: OpenSSH treats brackets as
// literal hostname characters.
func (a Address) Destination(resolvedUser string) string {
	user := a.User
	if user == "" {
		user = resolvedUser
	}
	host := a.Host
	if user == "" {
		return host
	}
	return user + "@" + host
}
