package main

import "testing"

func TestParseScheduleInputsFlags(t *testing.T) {
	got, err := parseScheduleInputs([]string{"SOURCE=prices", "SINCE=2026-09-01=ish"})
	if err != nil || got["SOURCE"] != "prices" || got["SINCE"] != "2026-09-01=ish" {
		t.Fatalf("got %#v, %v", got, err)
	}
	if got, err := parseScheduleInputs(nil); err != nil || len(got) != 0 {
		t.Fatalf("no flags should mean no overrides: %#v, %v", got, err)
	}
	for _, bad := range [][]string{{"SOURCE"}, {"=x"}, {"SOURCE=a", "SOURCE=b"}} {
		if _, err := parseScheduleInputs(bad); err == nil {
			t.Errorf("%v was accepted", bad)
		}
	}
}
