package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	"github.com/spf13/cobra"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/engine"
	"github.com/labstack/onebox/internal/onebox"
)

const cliSchemaVersion = "onebox.run/cli/v1alpha1"

const (
	cliOutcomeSuccess   = "success"
	cliOutcomeNoOp      = "no_op"
	cliOutcomeCancelled = "cancelled"
	cliOutcomeError     = "error"
)

type cliEnvelope struct {
	SchemaVersion string          `json:"schema_version"`
	Command       string          `json:"command"`
	Outcome       string          `json:"outcome"`
	Data          any             `json:"data,omitempty"`
	Error         *cliPublicError `json:"error,omitempty"`
}

type cliPublicError struct {
	Code              string `json:"code"`
	SafeMessage       string `json:"safe_message"`
	DiagnosticCommand string `json:"diagnostic_command,omitempty"`
	NextCommand       string `json:"next_command,omitempty"`
	ResolvingCommand  string `json:"resolving_command,omitempty"`
	Path              string `json:"path,omitempty"`
	Details           any    `json:"details,omitempty"`
}

type cliRecord struct {
	SchemaVersion string          `json:"schema_version"`
	Command       string          `json:"command"`
	Sequence      uint64          `json:"sequence"`
	Kind          string          `json:"kind"`
	Event         any             `json:"event,omitempty"`
	Channel       string          `json:"channel,omitempty"`
	Chunk         string          `json:"chunk,omitempty"`
	Outcome       string          `json:"outcome,omitempty"`
	Data          any             `json:"data,omitempty"`
	Error         *cliPublicError `json:"error,omitempty"`
}

type cliOutputClass struct {
	Class  string
	JSON   bool
	NDJSON bool
}

const (
	cliClassFiniteEnvelope      = "finite_envelope"
	cliClassFiniteStream        = "finite_stream"
	cliClassOperatorPassthrough = "operator_passthrough"
	cliClassTrustedEditor       = "trusted_editor"
)

// Every invocable leaf is classified here. Groups, help, and completion use
// Cobra's native human output. Keeping this list closed makes adding a command
// an explicit CLI-contract decision instead of silently inheriting behavior.
var cliOutputMatrix = map[string]cliOutputClass{
	"ob abort":         {Class: cliClassFiniteStream, JSON: true, NDJSON: true},
	"ob approve":       {Class: cliClassFiniteEnvelope, JSON: true},
	"ob audit":         {Class: cliClassFiniteEnvelope, JSON: true},
	"ob bootstrap":     {Class: cliClassFiniteStream, JSON: true, NDJSON: true},
	"ob canonical":     {Class: cliClassFiniteEnvelope, JSON: true},
	"ob deploy":        {Class: cliClassFiniteStream, JSON: true, NDJSON: true},
	"ob destroy":       {Class: cliClassFiniteStream, JSON: true, NDJSON: true},
	"ob doctor":        {Class: cliClassFiniteEnvelope, JSON: true},
	"ob eject":         {Class: cliClassFiniteEnvelope, JSON: true},
	"ob exec":          {Class: cliClassOperatorPassthrough, NDJSON: true},
	"ob init":          {Class: cliClassFiniteEnvelope, JSON: true},
	"ob job plan":      {Class: cliClassFiniteEnvelope, JSON: true},
	"ob job run":       {Class: cliClassFiniteStream, JSON: true, NDJSON: true},
	"ob logs":          {Class: cliClassOperatorPassthrough, JSON: true, NDJSON: true},
	"ob plan":          {Class: cliClassFiniteEnvelope, JSON: true},
	"ob preflight":     {Class: cliClassFiniteEnvelope, JSON: true},
	"ob preview":       {Class: cliClassFiniteEnvelope, JSON: true},
	"ob proxy apply":   {Class: cliClassFiniteStream, JSON: true, NDJSON: true},
	"ob resume":        {Class: cliClassFiniteStream, JSON: true, NDJSON: true},
	"ob rollback":      {Class: cliClassFiniteStream, JSON: true, NDJSON: true},
	"ob schema":        {Class: cliClassFiniteEnvelope, JSON: true},
	"ob secrets edit":  {Class: cliClassTrustedEditor, JSON: true},
	"ob secrets list":  {Class: cliClassFiniteEnvelope, JSON: true},
	"ob secrets push":  {Class: cliClassFiniteStream, JSON: true, NDJSON: true},
	"ob service apply": {Class: cliClassFiniteStream, JSON: true, NDJSON: true},
	"ob status":        {Class: cliClassFiniteEnvelope, JSON: true},
	"ob validate":      {Class: cliClassFiniteEnvelope, JSON: true},
	"ob version":       {Class: cliClassFiniteEnvelope, JSON: true},
}

type cliExitError struct {
	err  error
	code int
}

func (e *cliExitError) Error() string { return e.err.Error() }
func (e *cliExitError) Unwrap() error { return e.err }
func (e *cliExitError) ExitCode() int { return e.code }

func withExitCode(err error, code int) error {
	if err == nil {
		return nil
	}
	var existing interface{ ExitCode() int }
	if errors.As(err, &existing) {
		return err
	}
	return &cliExitError{err: err, code: code}
}

func isStructuredOutput(g *globalFlags) bool {
	return g != nil && (g.Output == "json" || g.Output == "ndjson")
}

func validateOutputMode(cmd *cobra.Command, g *globalFlags) error {
	if g.Output == "human" {
		return nil
	}
	if g.Output != "json" && g.Output != "ndjson" {
		return fmt.Errorf("--output must be human, json, or ndjson")
	}
	class, ok := cliOutputMatrix[cmd.CommandPath()]
	if !ok || (g.Output == "json" && !class.JSON) || (g.Output == "ndjson" && !class.NDJSON) {
		modeErr := fmt.Errorf("output_mode_incompatible: --output %s is not supported by %s", g.Output, cmd.CommandPath())
		if ok {
			publicErr := &cliPublicError{
				Code: "output_mode_incompatible", SafeMessage: "the requested output mode is incompatible with this command",
				DiagnosticCommand: "ob help " + strings.TrimPrefix(cmd.CommandPath(), "ob "),
			}
			if g.Output == "json" {
				if err := writeFiniteOutcome(cmd, g, cliOutcomeError, nil, publicErr); err != nil {
					return err
				}
			} else {
				stream := newCLIRecordStream(cmd.OutOrStdout(), commandName(cmd))
				if err := stream.terminal(cliOutcomeError, nil, publicErr); err != nil {
					return err
				}
			}
		}
		return withExitCode(modeErr, 1)
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

func commandName(cmd *cobra.Command) string {
	if cmd == nil {
		return "ob"
	}
	name := cmd.CommandPath()
	if name == "" {
		return "ob"
	}
	return name
}

func writeFiniteSuccess(cmd *cobra.Command, g *globalFlags, data any) error {
	return writeFiniteOutcome(cmd, g, cliOutcomeSuccess, data, nil)
}

func writeFiniteNoOp(cmd *cobra.Command, g *globalFlags, data any) error {
	return writeFiniteOutcome(cmd, g, cliOutcomeNoOp, data, nil)
}

func writeFiniteOutcome(cmd *cobra.Command, g *globalFlags, outcome string, data any, publicErr *cliPublicError) error {
	if g == nil || g.Output == "human" {
		return nil
	}
	if g.Output != "json" {
		return fmt.Errorf("finite output requires --output json")
	}
	if data == nil && publicErr == nil {
		data = map[string]any{}
	}
	return writeCLIJSON(cmd.OutOrStdout(), cliEnvelope{
		SchemaVersion: cliSchemaVersion,
		Command:       commandName(cmd),
		Outcome:       outcome,
		Data:          data,
		Error:         publicErr,
	}, true)
}

func writeStructuredReadFailure(cmd *cobra.Command, g *globalFlags, commandErr error) error {
	explained := explain(commandErr)
	if !isStructuredOutput(g) {
		return explained
	}
	publicErr := publicError(commandErr, "command_failed", "command failed; inspect stderr for diagnostic detail")
	if g.Output == "ndjson" {
		stream := newCLIRecordStream(cmd.OutOrStdout(), commandName(cmd))
		if err := stream.terminal(cliOutcomeError, nil, publicErr); err != nil {
			return err
		}
	} else if err := writeFiniteOutcome(cmd, g, cliOutcomeError, nil, publicErr); err != nil {
		return err
	}
	return withExitCode(explained, 1)
}

func writeStructuredCommandFailure(cmd *cobra.Command, g *globalFlags, code, message string, commandErr error) error {
	if !isStructuredOutput(g) {
		return commandErr
	}
	publicErr := publicError(commandErr, code, message)
	if g.Output == "ndjson" {
		stream := newCLIRecordStream(cmd.OutOrStdout(), commandName(cmd))
		if err := stream.terminal(cliOutcomeError, nil, publicErr); err != nil {
			return err
		}
	} else if err := writeFiniteOutcome(cmd, g, cliOutcomeError, nil, publicErr); err != nil {
		return err
	}
	return withExitCode(commandErr, 1)
}

func writeEarlyOperationFailure(cmd *cobra.Command, g *globalFlags, operationErr error) error {
	if !isStructuredOutput(g) {
		return operationErr
	}
	if err := newCLIOperationOutput(cmd, g).finish(nil, operationErr); err != nil {
		return err
	}
	if errors.Is(operationErr, context.Canceled) {
		return withExitCode(operationErr, 2)
	}
	return withExitCode(operationErr, 1)
}

func writeCancelled(cmd *cobra.Command, g *globalFlags, message string) error {
	if message == "" {
		message = "operation cancelled"
	}
	publicErr := &cliPublicError{Code: "cancelled", SafeMessage: message}
	if isStructuredOutput(g) {
		if g.Output == "ndjson" {
			stream := newCLIRecordStream(cmd.OutOrStdout(), commandName(cmd))
			if err := stream.terminal(cliOutcomeCancelled, nil, publicErr); err != nil {
				return err
			}
		} else if err := writeFiniteOutcome(cmd, g, cliOutcomeCancelled, nil, publicErr); err != nil {
			return err
		}
	}
	return withExitCode(errors.New(message), 2)
}

func publicError(commandErr error, fallbackCode, fallbackMessage string) *cliPublicError {
	result := &cliPublicError{Code: fallbackCode, SafeMessage: fallbackMessage}
	if commandErr == nil {
		return result
	}

	var projectErr *app.Error
	if errors.As(commandErr, &projectErr) {
		result.Code = projectErr.Code
		result.SafeMessage = "project configuration is invalid"
		result.Path = projectErr.Path
		setCommandGuidance(result, projectErr.Next)
		return result
	}

	var lifecycleErr onebox.LifecycleFailure
	if errors.As(commandErr, &lifecycleErr) {
		result.Code = lifecycleErr.Code
		result.SafeMessage = lifecycleErr.Message
		result.DiagnosticCommand = lifecycleErr.DiagnosticCommand
		result.NextCommand = lifecycleErr.NextCommand
		result.ResolvingCommand = lifecycleErr.ResolvingCommand
		return result
	}

	var coded interface{ Code() string }
	if errors.As(commandErr, &coded) && coded.Code() != "" {
		result.Code = coded.Code()
		result.SafeMessage = safeMessageForCode(coded.Code(), fallbackMessage)
		setCommandGuidance(result, guidanceCommandForCode(coded.Code()))
	}
	if errors.Is(commandErr, context.Canceled) {
		result.Code = "cancelled"
		result.SafeMessage = "operation cancelled"
	}
	return result
}

func safeMessageForCode(code, fallback string) string {
	messages := map[string]string{
		"recovery_incomplete":             "recovery did not reach its verified terminal state",
		"secret_recovery_incomplete":      "secret recovery did not reach its verified terminal state",
		"unknown_runtime_target":          "the requested runtime target is not declared",
		"secret_declaration_not_deployed": "the deployed release does not declare this secret graph",
		"secret_generation_not_deployed":  "the deployed release does not reference the active secret generation",
		"output_mode_incompatible":        "the requested output mode is incompatible with this command",
	}
	if message := messages[code]; message != "" {
		return message
	}
	return fallback
}

func guidanceCommandForCode(code string) string {
	commands := map[string]string{
		"recovery_incomplete":             "ob resume --output ndjson",
		"secret_recovery_incomplete":      "ob secrets push --output ndjson",
		"secret_declaration_not_deployed": "ob deploy --output ndjson",
		"secret_generation_not_deployed":  "ob deploy --output ndjson",
		"unknown_runtime_target":          "ob status --output json",
		"output_mode_incompatible":        "ob help",
	}
	return commands[code]
}

// setCommandGuidance assigns one semantic role. Inspection commands diagnose,
// placeholders describe a workflow step an operator must complete, and only a
// concrete mutation is called resolving. This prevents agents from looping on
// status/validate while believing they changed the reported condition.
func setCommandGuidance(result *cliPublicError, command string) {
	if result == nil || command == "" {
		return
	}
	switch onebox.GuidanceRoleForCommand(command) {
	case "diagnostic":
		result.DiagnosticCommand = command
	case "next":
		result.NextCommand = command
	default:
		result.ResolvingCommand = command
	}
}

type cliRecordStream struct {
	mu        sync.Mutex
	out       io.Writer
	command   string
	sequence  uint64
	terminald bool
	encodeErr error
}

type cliChunkWriter struct {
	stream  *cliRecordStream
	channel string
}

func (w cliChunkWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	err := w.stream.write("output", func(record *cliRecord) {
		record.Channel = w.channel
		record.Chunk = string(p)
	})
	if err != nil {
		return 0, err
	}
	return len(p), nil
}

func (s *cliRecordStream) channelWriter(channel string) io.Writer {
	return cliChunkWriter{stream: s, channel: channel}
}

func newCLIRecordStream(out io.Writer, command string) *cliRecordStream {
	return &cliRecordStream{out: out, command: command}
}

func (s *cliRecordStream) write(kind string, mutate func(*cliRecord)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.encodeErr != nil {
		return s.encodeErr
	}
	if s.terminald {
		return errors.New("NDJSON stream already has a terminal record")
	}
	s.sequence++
	record := cliRecord{SchemaVersion: cliSchemaVersion, Command: s.command, Sequence: s.sequence, Kind: kind}
	mutate(&record)
	s.encodeErr = writeCLIJSON(s.out, record, false)
	return s.encodeErr
}

func (s *cliRecordStream) terminal(outcome string, data any, publicErr *cliPublicError) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.encodeErr != nil {
		return s.encodeErr
	}
	if s.terminald {
		return errors.New("NDJSON stream already has a terminal record")
	}
	s.sequence++
	record := cliRecord{
		SchemaVersion: cliSchemaVersion, Command: s.command, Sequence: s.sequence, Kind: "terminal",
		Outcome: outcome, Data: data, Error: publicErr,
	}
	s.encodeErr = writeCLIJSON(s.out, record, false)
	if s.encodeErr == nil {
		s.terminald = true
	}
	return s.encodeErr
}

type cliOperationOutput struct {
	mu      sync.Mutex
	mode    string
	out     io.Writer
	command string
	events  []onebox.OperationEvent
	stream  *cliRecordStream
}

func newCLIOperationOutput(cmd *cobra.Command, g *globalFlags) *cliOperationOutput {
	command := commandName(cmd)
	return &cliOperationOutput{
		mode: g.Output, out: cmd.OutOrStdout(), command: command,
		events: make([]onebox.OperationEvent, 0),
		stream: newCLIRecordStream(cmd.OutOrStdout(), command),
	}
}

func (o *cliOperationOutput) event(event onebox.OperationEvent) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.mode == "json" {
		o.events = append(o.events, event)
		return
	}
	_ = o.stream.write("event", func(record *cliRecord) { record.Event = event })
}

func (o *cliOperationOutput) finish(result *onebox.OperationResult, operationErr error) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	outcome := cliOutcomeSuccess
	if errors.Is(operationErr, context.Canceled) {
		outcome = cliOutcomeCancelled
	} else if operationErr != nil {
		outcome = cliOutcomeError
	} else if result != nil && result.NoOp {
		outcome = cliOutcomeNoOp
	}

	var data any
	var publicErr *cliPublicError
	if operationErr != nil {
		publicErr = publicError(operationErr, "operation_failed", "operation failed; inspect stderr and journal evidence")
		if result != nil {
			publicErr.Details = map[string]any{"result": result}
		}
	} else {
		data = map[string]any{"result": result}
	}

	if o.mode == "json" {
		sort.SliceStable(o.events, func(i, j int) bool { return o.events[i].Sequence < o.events[j].Sequence })
		if operationErr == nil {
			data = map[string]any{"events": o.events, "result": result}
		} else if publicErr != nil {
			publicErr.Details = map[string]any{"events": o.events, "result": result}
		}
		return writeCLIJSON(o.out, cliEnvelope{
			SchemaVersion: cliSchemaVersion, Command: o.command, Outcome: outcome,
			Data: data, Error: publicErr,
		}, true)
	}
	return o.stream.terminal(outcome, data, publicErr)
}

func safeStatusSnapshot(snapshot engine.StatusSnapshot) engine.StatusSnapshot {
	for i := range snapshot.Warnings {
		snapshot.Warnings[i].Message = "observation unavailable; inspect stderr for local diagnostics"
	}
	return snapshot
}
