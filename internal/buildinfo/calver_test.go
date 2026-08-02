package buildinfo

import "testing"

func TestParseReleaseVersion(t *testing.T) {
	valid := []string{
		"v2026.01.1",
		"v2026.08.42",
		"v9999.12.18446744073709551615",
	}
	for _, value := range valid {
		if _, err := ParseReleaseVersion(value); err != nil {
			t.Errorf("ParseReleaseVersion(%q): %v", value, err)
		}
	}

	invalid := []string{
		"",
		"2026.08.1",
		"v2026.8.1",
		"v2026.00.1",
		"v2026.13.1",
		"v2026.08.0",
		"v2026.08.01",
		"v2026.08.1-rc1",
		"v2026.08.18446744073709551616",
		"dev",
	}
	for _, value := range invalid {
		if _, err := ParseReleaseVersion(value); err == nil {
			t.Errorf("ParseReleaseVersion(%q) succeeded", value)
		}
	}
}

func TestCompareReleaseVersions(t *testing.T) {
	tests := []struct {
		actual  string
		minimum string
		want    int
	}{
		{actual: "v2026.08.4", minimum: "v2026.08.3", want: 1},
		{actual: "v2026.08.4", minimum: "v2026.08.4", want: 0},
		{actual: "v2026.08.4", minimum: "v2026.08.5", want: -1},
		{actual: "v2026.07.20", minimum: "v2026.08.1", want: -1},
		{actual: "v2027.01.1", minimum: "v2026.12.99", want: 1},
	}
	for _, test := range tests {
		actual, err := ParseReleaseVersion(test.actual)
		if err != nil {
			t.Fatal(err)
		}
		minimum, err := ParseReleaseVersion(test.minimum)
		if err != nil {
			t.Fatal(err)
		}
		if got := CompareReleaseVersions(actual, minimum); got != test.want {
			t.Errorf("CompareReleaseVersions(%q, %q) = %d, want %d", test.actual, test.minimum, got, test.want)
		}
	}
}
