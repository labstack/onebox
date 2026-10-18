package proxy

import "strings"

// Identity records the staged configuration and Compose payload independently.
type Identity struct{ Config, Compose string }

func (i Identity) String() string {
	return i.Config + " " + i.Compose
}

func ParseIdentity(s string) (Identity, bool) {
	fields := strings.Fields(s)
	if len(fields) != 2 {
		return Identity{}, false
	}
	for _, digest := range fields {
		if len(digest) != len("sha256:")+64 || !strings.HasPrefix(digest, "sha256:") {
			return Identity{}, false
		}
		for _, c := range digest[len("sha256:"):] {
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
				return Identity{}, false
			}
		}
	}
	return Identity{Config: fields[0], Compose: fields[1]}, true
}
