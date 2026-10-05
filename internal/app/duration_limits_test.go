package app

import (
	"strings"
	"testing"
	"time"
)

func TestPositiveDurationRejectsOverflow(t *testing.T) {
	for _, value := range []string{"106752d", "213504d", "2147483647d", "9223372036854775807d"} {
		if got, err := PositiveDuration(value); err == nil || got != 0 {
			t.Errorf("PositiveDuration(%q) = %v, %v; want refusal", value, got, err)
		}
	}
	for value, want := range map[string]time.Duration{
		"1d":      24 * time.Hour,
		"106751d": 106751 * 24 * time.Hour,
		"15m":     15 * time.Minute,
	} {
		if got, err := PositiveDuration(value); err != nil || got != want {
			t.Errorf("PositiveDuration(%q) = %v, %v; want %v", value, got, err, want)
		}
	}
}

func TestPostgresDurationsRejectOverflowInEveryUnit(t *testing.T) {
	for _, value := range []string{
		"9223372037", "9223372037s", "9223372036855ms",
		"153722868min", "2562048h", "106752d", "213504d",
		"9223372036854775807s", "9223372036854775808ms",
	} {
		if got, ok := ParsePostgresDuration(value); ok || got != 0 {
			t.Errorf("ParsePostgresDuration(%q) = %v, %v; want refusal", value, got, ok)
		}
	}
	for value, want := range map[string]time.Duration{
		"9223372036":      9223372036 * time.Second,
		"9223372036s":     9223372036 * time.Second,
		"9223372036854ms": 9223372036854 * time.Millisecond,
		"153722867min":    153722867 * time.Minute,
		"2562047h":        2562047 * time.Hour,
		"106751d":         106751 * 24 * time.Hour,
		" 1 MIN ":         time.Minute,
		"0ms":             0,
	} {
		if got, ok := ParsePostgresDuration(value); !ok || got != want {
			t.Errorf("ParsePostgresDuration(%q) = %v, %v; want %v", value, got, ok, want)
		}
	}
}

func TestBackupPolicyRejectsOverflowBeforeItCanBecomeAShortWindow(t *testing.T) {
	for name, tc := range map[string]struct {
		policy string
		code   string
	}{
		"data loss": {"        maxDataLoss: 213504d\n", "project_invalid"},
		"retention": {"        maxDataLoss: 15m\n        retention: {window: 213504d}\n", "backup_retention_unsupported"},
		"drill age": {"        maxDataLoss: 15m\n        drill: {maxAge: 213504d}\n", "project_invalid"},
	} {
		t.Run(name, func(t *testing.T) {
			project := strings.Replace(validBackupProject, "        maxDataLoss: 15m\n", tc.policy, 1)
			_, err := loadFixtureBytes([]byte(project), "ob.yml")
			assertAppErrorCode(t, err, tc.code)
		})
	}
}
