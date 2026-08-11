package buildinfo

import "testing"

func TestParseReleaseVersion(t *testing.T) {
	valid := []string{
		"v10.1.1",
		"v26.8.42",
		"v99.12.18446744073709551615",
	}
	for _, value := range valid {
		if _, err := ParseReleaseVersion(value); err != nil {
			t.Errorf("ParseReleaseVersion(%q): %v", value, err)
		}
	}

	invalid := []string{
		"",
		"26.8.1",
		"v2026.8.1",
		"v09.8.1",
		"v100.8.1",
		"v26.08.1",
		"v26.0.1",
		"v26.13.1",
		"v26.8.0",
		"v26.8.01",
		"v26.8.1-rc1",
		"v26.8.18446744073709551616",
		"dev",
	}
	for _, value := range invalid {
		if _, err := ParseReleaseVersion(value); err == nil {
			t.Errorf("ParseReleaseVersion(%q) succeeded", value)
		}
	}
}

func TestParseReleaseVersionMapsSupportedEpoch(t *testing.T) {
	got, err := ParseReleaseVersion("v26.8.1")
	if err != nil {
		t.Fatal(err)
	}
	if got.year != 2026 || got.month != 8 || got.sequence != 1 {
		t.Fatalf("parsed release = %+v, want year 2026, month 8, sequence 1", got)
	}
}

func TestCompareReleaseVersions(t *testing.T) {
	tests := []struct {
		actual  string
		minimum string
		want    int
	}{
		{actual: "v26.8.4", minimum: "v26.8.3", want: 1},
		{actual: "v26.8.4", minimum: "v26.8.4", want: 0},
		{actual: "v26.8.4", minimum: "v26.8.5", want: -1},
		{actual: "v26.7.20", minimum: "v26.8.1", want: -1},
		{actual: "v27.1.1", minimum: "v26.12.99", want: 1},
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
