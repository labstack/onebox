// Package mcp adapts the typed Onebox product service to MCP. It contains
// no deployment logic and currently exposes only read-only tools.
package mcp

import (
	"context"
	"errors"
	"fmt"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/labstack/onebox/internal/onebox"
)

type Service interface {
	Observe(context.Context, onebox.ObserveRequest) (onebox.Observation, error)
	Propose(context.Context, onebox.ProposeRequest) (onebox.DeploymentProposal, error)
	ReadMemory(context.Context, onebox.ReadMemoryRequest) (onebox.OperationalMemory, error)
	ProposeMemoryChange(context.Context, onebox.ProposeMemoryChangeRequest) (onebox.MemoryChangeProposal, error)
}

func New(service Service, version string) *mcpsdk.Server {
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "onebox", Version: version}, nil)

	mcpsdk.AddTool(server, &mcpsdk.Tool{
		Name:        "onebox_observe",
		Title:       "Observe Onebox target",
		Description: "Read the configured Onebox project and target as timestamped, structured, redaction-safe state. This tool never mutates production.",
		Annotations: &mcpsdk.ToolAnnotations{
			ReadOnlyHint:    true,
			DestructiveHint: boolPointer(false),
			OpenWorldHint:   boolPointer(true),
			Title:           "Observe Onebox target",
		},
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, input onebox.ObserveRequest) (*mcpsdk.CallToolResult, onebox.Observation, error) {
		output, err := service.Observe(ctx, input)
		if err != nil {
			return nil, onebox.Observation{}, publicToolError(ctx, "onebox_observe", "run `ob status` locally for diagnostic detail", err)
		}
		return nil, output, err
	})

	mcpsdk.AddTool(server, &mcpsdk.Tool{
		Name:        "onebox_propose_deploy",
		Title:       "Propose a Onebox deployment",
		Description: "Build a redacted, state-bound deployment proposal for the configured Onebox project and target. It may read the host and registry but never mutates production or executes the proposal.",
		Annotations: &mcpsdk.ToolAnnotations{
			ReadOnlyHint:    true,
			DestructiveHint: boolPointer(false),
			OpenWorldHint:   boolPointer(true),
			Title:           "Propose a Onebox deployment",
		},
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, input onebox.ProposeDeployRequest) (*mcpsdk.CallToolResult, onebox.DeploymentProposal, error) {
		output, err := service.Propose(ctx, onebox.ProposeRequest{Kind: onebox.KindDeploy})
		if err != nil {
			return nil, onebox.DeploymentProposal{}, publicToolError(ctx, "onebox_propose_deploy", "ask the operator to inspect project policy and run `ob plan` locally if appropriate", err)
		}
		return nil, output, err
	})

	mcpsdk.AddTool(server, &mcpsdk.Tool{
		Name:        "onebox_read_memory",
		Title:       "Read Onebox operational memory",
		Description: "Read the configured project's deterministic, redaction-safe operational model. This tool reads local declarations only and never writes project configuration.",
		Annotations: &mcpsdk.ToolAnnotations{
			ReadOnlyHint:    true,
			DestructiveHint: boolPointer(false),
			OpenWorldHint:   boolPointer(false),
			Title:           "Read Onebox operational memory",
		},
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, input onebox.ReadMemoryRequest) (*mcpsdk.CallToolResult, onebox.OperationalMemory, error) {
		output, err := service.ReadMemory(ctx, input)
		if err != nil {
			return nil, onebox.OperationalMemory{}, publicToolError(ctx, "onebox_read_memory", "ask the operator to inspect `ob.yml` locally", err)
		}
		return nil, output, nil
	})

	mcpsdk.AddTool(server, &mcpsdk.Tool{
		Name:        "onebox_propose_memory_change",
		Title:       "Propose a Onebox memory change",
		Description: "Create an immutable, revision-bound suggestion for operational-memory declarations. This tool returns a proposal only and never writes project configuration or production state.",
		Annotations: &mcpsdk.ToolAnnotations{
			ReadOnlyHint:    true,
			DestructiveHint: boolPointer(false),
			OpenWorldHint:   boolPointer(false),
			Title:           "Propose a Onebox memory change",
		},
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, input onebox.ProposeMemoryChangeRequest) (*mcpsdk.CallToolResult, onebox.MemoryChangeProposal, error) {
		output, err := service.ProposeMemoryChange(ctx, input)
		if err != nil {
			return nil, onebox.MemoryChangeProposal{}, publicToolError(ctx, "onebox_propose_memory_change", "read operational memory again and ask the operator to review the suggested declarations", err)
		}
		return nil, output, nil
	})

	return server
}

func boolPointer(value bool) *bool { return &value }

func publicToolError(ctx context.Context, code, guidance string, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("%s: operation could not complete; %s", code, guidance)
}

func Run(ctx context.Context, service Service, version string) error {
	return New(service, version).Run(ctx, &mcpsdk.StdioTransport{})
}
