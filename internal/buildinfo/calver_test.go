package buildinfo

import "testing"

func TestParseReleaseVersion(t *testing.T) {
	valid := []string{
		"v2010.1.0",
		"v2026.8.42",
		"v9999.12.18446744073709551615",
	}
	for _, value := range valid {
		if _, err := ParseReleaseVersion(value); err != nil {
			t.Errorf("ParseReleaseVersion(%q): %v", value, err)
		}
	}

	invalid := []string{
		"",
		"2026.8.0",
		"v26.8.0",
		"v026.8.0",
		"v02026.8.0",
		"v2026.08.0",
		"v2026.0.0",
		"v2026.13.0",
		"v2026.8.00",
		"v2026.8.01",
		"v2026.8.0-rc1",
		"v2026.8.18446744073709551616",
		"dev",
	}
	for _, value := range invalid {
		if _, err := ParseReleaseVersion(value); err == nil {
			t.Errorf("ParseReleaseVersion(%q) succeeded", value)
		}
	}
}

func TestParseReleaseVersionFields(t *testing.T) {
	got, err := ParseReleaseVersion("v2026.8.0")
	if err != nil {
		t.Fatal(err)
	}
	if got.year != 2026 || got.month != 8 || got.revision != 0 {
		t.Fatalf("parsed release = %+v, want year 2026, month 8, revision 0", got)
	}
}

func TestCompareReleaseVersions(t *testing.T) {
	tests := []struct {
		actual  string
		minimum string
		want    int
	}{
		{actual: "v2026.8.4", minimum: "v2026.8.3", want: 1},
		{actual: "v2026.8.4", minimum: "v2026.8.4", want: 0},
		{actual: "v2026.8.4", minimum: "v2026.8.5", want: -1},
		{actual: "v2026.7.20", minimum: "v2026.8.0", want: -1},
		{actual: "v2027.1.0", minimum: "v2026.12.99", want: 1},
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
