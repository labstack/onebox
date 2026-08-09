package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"

	"github.com/spf13/cobra"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/engine"
	"github.com/labstack/onebox/internal/onebox"
)

const (
	cliOperationSchemaVersion = "onebox.run/cli-operation/v1alpha1"
	cliRecordSchemaVersion    = "onebox.run/cli-record/v1alpha1"
	cliStatusSchemaVersion    = "onebox.run/status/v1alpha1"

	// The read-only verbs. Each carries its own identity so a consumer can
	// tell what it is holding without inferring it from the shape, and so the
	// shape can grow without the consumer guessing which version it grew in.
	cliValidateSchemaVersion  = "onebox.run/cli-validate/v1alpha1"
	cliCanonicalSchemaVersion = "onebox.run/cli-canonical/v1alpha1"
	cliPreviewSchemaVersion   = "onebox.run/cli-preview/v1alpha1"
	cliEjectSchemaVersion     = "onebox.run/cli-eject/v1alpha1"
)

// cliValidateEnvelope is what `ob validate --output json` emits.
type cliValidateEnvelope struct {
	SchemaVersion string          `json:"schema_version"`
	App           string          `json:"app,omitempty"`
	Environment   string          `json:"environment,omitempty"`
	Workloads     []string        `json:"workloads,omitempty"`
	Jobs          []string        `json:"jobs,omitempty"`
	Services      []string        `json:"services,omitempty"`
	Error         *cliPublicError `json:"error,omitempty"`
}

// cliCanonicalEnvelope carries the normalised document and, separately, where
// each value came from. The human form marks origins in comments; a comment is
// not something a consumer can read, so the same fact is given as data.
type cliCanonicalEnvelope struct {
	SchemaVersion string            `json:"schema_version"`
	Environment   string            `json:"environment"`
	Document      string            `json:"document"`
	Redacted      bool              `json:"redacted"`
	Origins       map[string]string `json:"origins,omitempty"`
	Error         *cliPublicError   `json:"error,omitempty"`
}

// cliPreviewEnvelope carries the generated runtime and its digest.
//
// Redacted is always true: the structured stream is the one that gets piped
// into a file, a log or a CI artifact, and --raw is refused alongside it. The
// field is stated rather than implied so a consumer never has to assume.
type cliPreviewEnvelope struct {
	SchemaVersion string            `json:"schema_version"`
	Environment   string            `json:"environment"`
	Release       string            `json:"release"`
	Digest        string            `json:"digest"`
	Redacted      bool              `json:"redacted"`
	Runtime       string            `json:"runtime"`
	Services      map[string]string `json:"services,omitempty"`
	Error         *cliPublicError   `json:"error,omitempty"`
}

type cliEjectEnvelope struct {
	SchemaVersion string          `json:"schema_version"`
	Runtime       string          `json:"runtime"`
	Workloads     []string        `json:"workloads"`
	Error         *cliPublicError `json:"error,omitempty"`
}

type cliPublicError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Path    string `json:"path,omitempty"`
	Next    string `json:"next,omitempty"`
}

type cliOperationEnvelope struct {
	SchemaVersion string                  `json:"schema_version"`
	Events        []onebox.OperationEvent `json:"events"`
	Result        *onebox.OperationResult `json:"result,omitempty"`
	Error         *cliPublicError         `json:"error,omitempty"`
}

type cliOperationRecord struct {
	SchemaVersion string                  `json:"schema_version"`
	Type          string                  `json:"type"`
	Event         *onebox.OperationEvent  `json:"event,omitempty"`
	Result        *onebox.OperationResult `json:"result,omitempty"`
	Error         *cliPublicError         `json:"error,omitempty"`
}

type cliStatusEnvelope struct {
	SchemaVersion string                 `json:"schema_version"`
	Status        *engine.StatusSnapshot `json:"status,omitempty"`
	Error         *cliPublicError        `json:"error,omitempty"`
}

func isStructuredOutput(g *globalFlags) bool {
	return g != nil && (g.Output == "json" || g.Output == "ndjson")
}

var structuredOutputCommands = map[string]bool{
	"ob abort":                  true,
	"ob backup-evidence create": true,
	"ob bootstrap":              true,
	"ob canonical":              true,
	"ob deploy":                 true,
	"ob doctor":                 true,
	"ob eject":                  true,
	"ob plan":                   true,
	"ob preview":                true,
	"ob proxy apply":            true,
	"ob resume":                 true,
	"ob rollback":               true,
	"ob service apply":          true,
	"ob status":                 true,
	"ob validate":               true,
	"ob version":                true,
}

func validateOutputMode(cmd *cobra.Command, g *globalFlags) error {
	if g.Output == "human" {
		return nil
	}
	if g.Output != "json" && g.Output != "ndjson" {
		return fmt.Errorf("--output must be human, json, or ndjson")
	}
	if !structuredOutputCommands[cmd.CommandPath()] {
		return fmt.Errorf("--output %s is not supported by %s", g.Output, cmd.CommandPath())
	}
	return nil
}

func commandOutput(cmd *cobra.Command, g *globalFlags) io.Writer {
	if isStructuredOutput(g) {
		return io.Discard
	}
	return cmd.OutOrStdout()
}

func writeCLIJSON(out io.Writer, value any, pretty bool) error {
	encoder := json.NewEncoder(out)
	encoder.SetEscapeHTML(false)
	if pretty {
		encoder.SetIndent("", "  ")
	}
	return encoder.Encode(value)
}

func writeStructuredReadFailure(cmd *cobra.Command, g *globalFlags, schemaVersion string, commandErr error) error {
	explained := explain(commandErr)
	if !isStructuredOutput(g) {
		return explained
	}
	publicErr := &cliPublicError{
		Code:    "command_failed",
		Message: "command failed; inspect stderr for diagnostic detail",
	}
	var projectErr *app.Error
	if errors.As(commandErr, &projectErr) {
		publicErr.Code = projectErr.Code
		publicErr.Message = "project is invalid; inspect stderr for diagnostic detail"
		publicErr.Path = projectErr.Path
		publicErr.Next = projectErr.Next
	}
	if err := writeCLIJSON(cmd.OutOrStdout(), struct {
		SchemaVersion string          `json:"schema_version"`
		Error         *cliPublicError `json:"error"`
	}{SchemaVersion: schemaVersion, Error: publicErr}, g.Output == "json"); err != nil {
		return err
	}
	return explained
}

func safeOperationError() *cliPublicError {
	return &cliPublicError{
		Code:    "operation_failed",
		Message: "operation failed; inspect stderr and journal evidence",
	}
}

func writeStructuredCommandFailure(cmd *cobra.Command, g *globalFlags, code, message string, commandErr error) error {
	if !isStructuredOutput(g) {
		return commandErr
	}
	record := cliOperationRecord{
		SchemaVersion: cliRecordSchemaVersion,
		Type:          "error",
		Error:         &cliPublicError{Code: code, Message: message},
	}
	if err := writeCLIJSON(cmd.OutOrStdout(), record, g.Output == "json"); err != nil {
		return err
	}
	return commandErr
}

type cliOperationOutput struct {
	mu        sync.Mutex
	mode      string
	out       io.Writer
	events    []onebox.OperationEvent
	encodeErr error
}

func newCLIOperationOutput(cmd *cobra.Command, g *globalFlags) *cliOperationOutput {
	return &cliOperationOutput{
		mode:   g.Output,
		out:    cmd.OutOrStdout(),
		events: make([]onebox.OperationEvent, 0),
	}
}

func writeEarlyOperationFailure(cmd *cobra.Command, g *globalFlags, operationErr error) error {
	if !isStructuredOutput(g) {
		return operationErr
	}
	if err := newCLIOperationOutput(cmd, g).finish(nil, operationErr); err != nil {
		return err
	}
	return operationErr
}

func (o *cliOperationOutput) event(event onebox.OperationEvent) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.mode == "json" {
		o.events = append(o.events, event)
		return
	}
	if o.encodeErr != nil {
		return
	}
	record := cliOperationRecord{
		SchemaVersion: cliRecordSchemaVersion,
		Type:          "event",
		Event:         &event,
	}
	o.encodeErr = writeCLIJSON(o.out, record, false)
}

func (o *cliOperationOutput) finish(result *onebox.OperationResult, operationErr error) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	var publicErr *cliPublicError
	if operationErr != nil {
		publicErr = safeOperationError()
	}
	if o.mode == "json" {
		sort.SliceStable(o.events, func(i, j int) bool {
			return o.events[i].Sequence < o.events[j].Sequence
		})
		return writeCLIJSON(o.out, cliOperationEnvelope{
			SchemaVersion: cliOperationSchemaVersion,
			Events:        o.events,
			Result:        result,
			Error:         publicErr,
		}, true)
	}
	if o.encodeErr != nil {
		return o.encodeErr
	}
	recordType := "result"
	if result == nil {
		recordType = "error"
	}
	return writeCLIJSON(o.out, cliOperationRecord{
		SchemaVersion: cliRecordSchemaVersion,
		Type:          recordType,
		Result:        result,
		Error:         publicErr,
	}, false)
}

func safeStatusSnapshot(snapshot engine.StatusSnapshot) engine.StatusSnapshot {
	for i := range snapshot.Warnings {
		snapshot.Warnings[i].Message = "observation unavailable; inspect stderr for local diagnostics"
	}
	return snapshot
}
