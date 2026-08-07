package main

import (
	"context"
	"reflect"
	"sort"
	"testing"
)

func TestScheduledRunnerCommandSurfaceIsRestricted(t *testing.T) {
	root := newRootCmd(func(context.Context, string) error { return nil })
	var names []string
	for _, command := range root.Commands() {
		names = append(names, command.Name())
	}
	sort.Strings(names)
	if !reflect.DeepEqual(names, []string{"run", "version"}) {
		t.Fatalf("runner commands = %#v", names)
	}
	for _, forbidden := range []string{"plan", "deploy", "approve", "bootstrap", "destroy", "serve", "listen"} {
		for _, name := range names {
			if name == forbidden {
				t.Fatalf("scheduled runner exposes forbidden command %q", forbidden)
			}
		}
	}
}

func TestScheduledRunnerRunsExactlyOneEnvelope(t *testing.T) {
	var calls []string
	root := newRootCmd(func(_ context.Context, path string) error {
		calls = append(calls, path)
		return nil
	})
	root.SetArgs([]string{"run", "/var/lib/onebox/example/protection/envelope.json"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(calls, []string{"/var/lib/onebox/example/protection/envelope.json"}) {
		t.Fatalf("runner calls = %#v", calls)
	}
	root = newRootCmd(func(context.Context, string) error { return nil })
	root.SetArgs([]string{"run", "one", "two"})
	if err := root.ExecuteContext(context.Background()); err == nil {
		t.Fatal("runner accepted more than one envelope")
	}
}
