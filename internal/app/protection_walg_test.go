package app

import "testing"

// Retention has two floors and the larger one wins. A frequent schedule under a
// long window needs far more generations than the declared minimum, and keeping
// only the minimum silently shortens the window the policy promised.
func TestWalgRetainCountSatisfiesBothRetentionFloors(t *testing.T) {
	for _, tc := range []struct {
		name        string
		cron        string
		window      string
		minimum     int
		wantAtLeast int
	}{
		{"frequent schedule is bound by the window", "*/5 * * * *", "24h", 2, 288},
		{"daily schedule over a week", "0 2 * * *", "168h", 2, 7},
		{"sparse schedule falls back to the declared minimum", "0 2 * * *", "1h", 5, 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := WalgRetainCount(ProtectionPolicy{
				Schedule:  Schedule{Cron: tc.cron},
				Retention: ProtectionRetention{MinimumGenerations: tc.minimum, RecoveryWindow: tc.window},
			})
			if err != nil {
				t.Fatal(err)
			}
			if got < tc.wantAtLeast {
				t.Fatalf("retain count = %d, want at least %d to honour both floors", got, tc.wantAtLeast)
			}
			if got < tc.minimum {
				t.Fatalf("retain count %d is below the declared minimum %d", got, tc.minimum)
			}
		})
	}
}
