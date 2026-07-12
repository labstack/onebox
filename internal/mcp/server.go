// Package mcp adapts the typed Onebox product service to MCP. It contains
// no deployment logic and exposes only read-only tools in this milestone.
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
	ProposeDeploy(context.Context, onebox.ProposeDeployRequest) (onebox.DeploymentProposal, error)
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
		output, err := service.ProposeDeploy(ctx, input)
		if err != nil {
			return nil, onebox.DeploymentProposal{}, publicToolError(ctx, "onebox_propose_deploy", "ask the operator to inspect project policy and run `ob plan` locally if appropriate", err)
		}
		return nil, output, err
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
