package shellquote

import "testing"

func TestQuote(t *testing.T) {
	tests := map[string]string{
		"":          "''",
		"plain":     "'plain'",
		"two words": "'two words'",
		"it's":      `'it'\''s'`,
		"a\nb":      "'a\nb'",
	}
	for input, want := range tests {
		if got := Quote(input); got != want {
			t.Errorf("Quote(%q) = %q, want %q", input, got, want)
		}
	}
}
