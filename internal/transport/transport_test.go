package transport

import (
	"context"
	"regexp"
	"testing"
)

func TestLocalRunCapturesExitAndOutput(t *testing.T) {
	tr := NewLocal()
	res, err := tr.Run(context.Background(), "echo hi; exit 3")
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 3 || res.Stdout != "hi\n" {
		t.Fatalf("got %+v", res)
	}
}

func TestFakeScriptsAndRecords(t *testing.T) {
	f := &Fake{Script: []Rule{
		{Match: regexp.MustCompile(`docker inspect`), Result: Result{Stdout: "healthy\n"}},
	}}
	res, _ := f.Run(context.Background(), "docker inspect -f x abc")
	if res.Stdout != "healthy\n" {
		t.Fatalf("scripted reply not used: %+v", res)
	}
	res, _ = f.Run(context.Background(), "docker stop abc")
	if res.ExitCode != 0 {
		t.Fatalf("default should be exit 0: %+v", res)
	}
	if len(f.Commands) != 2 || f.Commands[1] != "docker stop abc" {
		t.Fatalf("recording broken: %v", f.Commands)
	}
}
