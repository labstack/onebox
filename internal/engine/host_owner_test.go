package engine

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/transport"
)

func TestForeignHostOwnerBlocksMutationsBeforeEffects(t *testing.T) {
	tests := []struct {
		name string
		run  func(context.Context, *Engine) error
	}{
		{"bootstrap", func(ctx context.Context, engine *Engine) error { return engine.Bootstrap(ctx, "R1", t.TempDir()) }},
		{"preflight", func(ctx context.Context, engine *Engine) error { return engine.Preflight(ctx) }},
		{"deploy", func(ctx context.Context, engine *Engine) error { return engine.Deploy(ctx, "R1", t.TempDir()) }},
		{"service apply", func(ctx context.Context, engine *Engine) error { return engine.ServiceApply(ctx, "R1", true) }},
		{"destroy", func(ctx context.Context, engine *Engine) error { return engine.Destroy(ctx, true, false) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := &transport.Fake{Dynamic: func(command string) (transport.Result, bool) {
				if strings.Contains(command, "_host/owner") {
					return transport.Result{Stdout: "another-app\n"}, true
				}
				return transport.Result{}, false
			}}
			engine := New(testConfig(), testProject(t), fake, Options{Out: &bytes.Buffer{}, Sleep: noSleep, ForceLock: true, Environment: "production"})
			err := test.run(context.Background(), engine)
			if err == nil || !strings.Contains(err.Error(), "another-app") {
				t.Fatalf("foreign owner was accepted: %v", err)
			}
			for _, command := range fake.Commands {
				if !strings.Contains(command, "_host/owner") {
					t.Fatalf("foreign-owner refusal performed another target operation: %s", command)
				}
			}
			if len(fake.Uploads) != 0 || len(fake.Inputs) != 0 {
				t.Fatalf("foreign-owner refusal wrote target input: uploads=%v inputs=%v", fake.Uploads, fake.Inputs)
			}
		})
	}
}
