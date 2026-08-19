package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
	"ob backup create": {Class: cliClassFiniteStream, JSON: true, NDJSON: true},
	"ob backup enable": {Class: cliClassFiniteStream, JSON: true, NDJSON: true},
	"ob backup prune":  {Class: cliClassFiniteStream, JSON: true, NDJSON: true},
	"ob backup verify": {Class: cliClassFiniteStream, JSON: true, NDJSON: true},
	"ob backup status": {Class: cliClassFiniteEnvelope, JSON: true},
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
	var existing *cliExitError
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
	// An unparseable mode is still a request for machine output. Answering it
	// with an empty stream would make the one failure an agent is most likely to
	// hit on its first call the only failure it cannot read; JSON is the
	// fallback because no stream framing was successfully selected.
	if g.Output != "json" && g.Output != "ndjson" {
		modeErr := fmt.Errorf("output_mode_incompatible: --output %s is not a supported output mode", g.Output)
		publicErr := &cliPublicError{
			Code:              "output_mode_incompatible",
			SafeMessage:       safeMessageForCode("output_mode_incompatible", "the requested output mode is incompatible with this command"),
			DiagnosticCommand: helpCommandFor(cmd),
		}
		if err := writeCLIJSON(cmd.OutOrStdout(), cliEnvelope{
			SchemaVersion: cliSchemaVersion, Command: commandName(cmd),
			Outcome: cliOutcomeError, Error: publicErr,
		}, true); err != nil {
			return errors.Join(withExitCode(modeErr, 1), fmt.Errorf("write structured failure: %w", err))
		}
		return withExitCode(modeErr, 1)
	}
	class, ok := cliOutputMatrix[cmd.CommandPath()]
	if !ok || (g.Output == "json" && !class.JSON) || (g.Output == "ndjson" && !class.NDJSON) {
		modeErr := fmt.Errorf("output_mode_incompatible: --output %s is not supported by %s", g.Output, cmd.CommandPath())
		publicErr := &cliPublicError{
			Code:              "output_mode_incompatible",
			SafeMessage:       safeMessageForCode("output_mode_incompatible", "the requested output mode is incompatible with this command"),
			DiagnosticCommand: helpCommandFor(cmd),
		}
		// A command group and the root carry no matrix row because they execute
		// nothing. They still answer in the requested framing: an agent probing
		// `ob job --output json` to discover the surface must get an envelope,
		// not zero bytes on a stream it is already parsing.
		if g.Output == "json" {
			if err := writeFiniteOutcome(cmd, g, cliOutcomeError, nil, publicErr); err != nil {
				return errors.Join(withExitCode(modeErr, 1), fmt.Errorf("write structured failure: %w", err))
			}
		} else {
			stream := newCLIRecordStream(cmd.OutOrStdout(), commandName(cmd))
			if err := stream.terminal(cliOutcomeError, nil, publicErr); err != nil {
				return errors.Join(withExitCode(modeErr, 1), fmt.Errorf("write structured failure: %w", err))
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
			return errors.Join(withExitCode(explained, 1), fmt.Errorf("write structured failure: %w", err))
		}
	} else if err := writeFiniteOutcome(cmd, g, cliOutcomeError, nil, publicErr); err != nil {
		return errors.Join(withExitCode(explained, 1), fmt.Errorf("write structured failure: %w", err))
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
			return errors.Join(withExitCode(commandErr, 1), fmt.Errorf("write structured failure: %w", err))
		}
	} else if err := writeFiniteOutcome(cmd, g, cliOutcomeError, nil, publicErr); err != nil {
		return errors.Join(withExitCode(commandErr, 1), fmt.Errorf("write structured failure: %w", err))
	}
	return withExitCode(commandErr, 1)
}

func writeEarlyOperationFailure(cmd *cobra.Command, g *globalFlags, operationErr error) error {
	if !isStructuredOutput(g) {
		return operationErr
	}
	if err := newCLIOperationOutput(cmd, g).finish(nil, operationErr); err != nil {
		exitCode := cancellationExitCode(operationErr, "operation_failed")
		return errors.Join(withExitCode(operationErr, exitCode), fmt.Errorf("write structured failure: %w", err))
	}
	return withExitCode(operationErr, cancellationExitCode(operationErr, "operation_failed"))
}

func writeCancelled(cmd *cobra.Command, g *globalFlags, message string) error {
	if message == "" {
		message = "operation cancelled"
	}
	publicErr := &cliPublicError{Code: "cancelled", SafeMessage: message}
	cancelled := withExitCode(errors.New(message), 2)
	if isStructuredOutput(g) {
		if g.Output == "ndjson" {
			stream := newCLIRecordStream(cmd.OutOrStdout(), commandName(cmd))
			if err := stream.terminal(cliOutcomeCancelled, nil, publicErr); err != nil {
				return errors.Join(cancelled, fmt.Errorf("write structured cancellation: %w", err))
			}
		} else if err := writeFiniteOutcome(cmd, g, cliOutcomeCancelled, nil, publicErr); err != nil {
			return errors.Join(cancelled, fmt.Errorf("write structured cancellation: %w", err))
		}
	}
	return cancelled
}

// publicError builds the operator-facing error. fallbackMessage is used only
// when the resolved code is in no published table; enumerated codes carry their
// published sentence, whichever family owns them.
func publicError(commandErr error, fallbackCode, fallbackMessage string) *cliPublicError {
	result := &cliPublicError{Code: fallbackCode, SafeMessage: fallbackMessage}
	if commandErr == nil {
		return result
	}

	var projectErr *app.Error
	if errors.As(commandErr, &projectErr) {
		result.Code = projectErr.Code
		// The loader publishes a sentence per code; collapsing all of them to one
		// generic string discarded the only diagnostic a structured caller gets.
		result.SafeMessage = safeMessageForCode(projectErr.Code, "project configuration is invalid")
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
	}
	// Only when nothing more specific resolved. A typed failure can carry a
	// cancelled cause — an interrupt during a post-commit secret sweep — and
	// calling that "cancelled" tells the operator nothing was applied when the
	// new generation is committed and live.
	if result.Code == fallbackCode && errors.Is(commandErr, context.Canceled) {
		result.Code = "cancelled"
	}

	// Resolution happens here for every path that reaches it. When only the typed
	// branch consulted the registry, a call site passing a literal code kept
	// whatever sentence it had authored and no guidance was attached. The
	// lifecycle branch above returns with its own typed fields.
	result.SafeMessage = safeMessageForCode(result.Code, fallbackMessage)
	command := guidanceCommandForCode(result.Code)
	// An error that knows a more specific safe command than its code's
	// published default wins: image_unresolved can name the exact workload to
	// pin. A specific command that is not publishable falls back to the
	// registry default rather than leaving the failure with no guidance.
	var specific interface{ GuidanceCommand() string }
	if errors.As(commandErr, &specific) && onebox.SafeGuidanceCommand(specific.GuidanceCommand()) {
		command = specific.GuidanceCommand()
	}
	setCommandGuidance(result, command)
	return result
}

// safeMessageForCode and guidanceCommandForCode both read the published
// operation-failure registry rather than a private table. The registry is what
// the error reference is generated from, so a code cannot carry one sentence in
// the envelope and a different one in the documentation, and a code that
// reaches an operator without being enumerated fails a test.
func safeMessageForCode(code, fallback string) string {
	if message, ok := onebox.OperationFailureMeaning(code); ok && message != "" {
		return message
	}
	// A few codes are raised by both the loader and the engine — image_unresolved
	// is the same condition found at two moments — and the validation table owns
	// the sentence for those.
	if message, ok := app.ErrorCodeMeaning(code); ok && message != "" {
		return message
	}
	return fallback
}

func guidanceCommandForCode(code string) string {
	return onebox.OperationFailureCommand(code)
}

// setCommandGuidance assigns one semantic role. Inspection commands diagnose,
// placeholders describe a workflow step an operator must complete, and only a
// concrete mutation is called resolving. This prevents agents from looping on
// status/validate while believing they changed the reported condition.
func setCommandGuidance(result *cliPublicError, command string) {
	if result == nil || command == "" {
		return
	}
	// The guidance fields are documented as safe Onebox commands, and an agent
	// may run what it finds there. Anything that is not one — a YAML fragment
	// authored as prose guidance, say — is dropped rather than published: the
	// message already carries it, and a field that sometimes holds a command is
	// worse than one that is absent.
	if !onebox.SafeGuidanceCommand(command) {
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

// typedFailure reports whether err carries its own operation code. Such an
// error names a condition the cancelled cause does not describe.
//
// It has to recognise every shape publicError resolves a code from, or the
// envelope contradicts itself: outcome "cancelled" beside a specific
// error.code. Two of the three carry Code as a struct field with no method,
// and errors.Join puts them in chains that also satisfy errors.Is(Canceled).
func typedFailure(err error, fallbackCode string) bool {
	var projectErr *app.Error
	if errors.As(err, &projectErr) && projectErr.Code != "" {
		return true
	}
	var lifecycleErr onebox.LifecycleFailure
	if errors.As(err, &lifecycleErr) && lifecycleErr.Code != "" {
		return true
	}
	// A code equal to the fallback is what publicError itself rewrites to
	// "cancelled", so treating it as typed would put outcome "error" beside
	// error.code "cancelled" — the same contradiction, mirrored.
	var coded interface{ Code() string }
	return errors.As(err, &coded) && coded.Code() != "" && coded.Code() != fallbackCode
}

// cancellationExitCode reports the exit status for a finished operation. It
// mirrors typedFailure because the published contract says the exit code
// matches the terminal outcome in the envelope: exit 2 means cancelled and
// nothing was changed, so a typed post-commit failure must not claim it.
func cancellationExitCode(err error, fallbackCode string) int {
	if errors.Is(err, context.Canceled) && !typedFailure(err, fallbackCode) {
		return 2
	}
	return 1
}

func (o *cliOperationOutput) finish(result *onebox.OperationResult, operationErr error) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	outcome := cliOutcomeSuccess
	// Only an interrupt that carries no more specific code is a cancellation.
	// publicError makes the same distinction for error.code; outcome is the
	// top-level field an agent branches on, so letting a post-commit failure
	// ship as "cancelled" tells it nothing was applied while the new
	// generation is committed and live.
	if errors.Is(operationErr, context.Canceled) && !typedFailure(operationErr, "operation_failed") {
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

// cliCodedError attaches a published operation-failure code to a refusal the
// CLI raises before any service call. Without one, an argument refusal — the
// first thing a new agent operator hits — arrives as the generic
// operation_failed envelope with the actual instruction stranded on stderr.
type cliCodedError struct {
	err  error
	code string
}

func (e *cliCodedError) Error() string { return e.err.Error() }
func (e *cliCodedError) Unwrap() error { return e.err }
func (e *cliCodedError) Code() string  { return e.code }

func codedError(code string, format string, args ...any) error {
	return &cliCodedError{err: fmt.Errorf(format, args...), code: code}
}

// helpCommandFor names the help page for a command, and plain `ob help` for the
// root, so the guidance never renders with a doubled space or a repeated "ob".
func helpCommandFor(cmd *cobra.Command) string {
	return strings.TrimSpace("ob help " + strings.TrimSpace(strings.TrimPrefix(cmd.CommandPath(), "ob")))
}

// stagedArtifact writes a file next to its destination and moves it into place
// only once every other artifact in the set has also been written. Ordering
// alone is not enough: whichever file is written first is left orphaned when a
// later one fails, and the caller cannot tell an orphan from a complete set.
// A rename is the last step for each, so a failure before the renames leaves
// the caller's previous files exactly as they were.
type stagedArtifact struct {
	staged, final string
	// restored on rollback: a rename replaces whatever was at final, and
	// without keeping it the "all or nothing" below can only mean "nothing",
	// which deletes a file the failed run was never asked to touch.
	backup   string
	replaced bool
	// orphan records that a backup was already sitting there when staging
	// began, with no destination beside it — a previous run killed between
	// commit()'s two renames. It is the caller's only copy, so discard() must
	// leave it alone even though THIS artifact replaced nothing.
	orphan bool
}

func stageArtifact(final, suffix string, write func(string) error) (stagedArtifact, error) {
	// The suffix distinguishes artifacts staged in the same directory. Two
	// artifacts sharing a destination would otherwise stage to one path, and
	// the second write would silently become the content of the first.
	// A unique staged name, not a derived one. Two concurrent runs against the
	// same --out would otherwise write to one temp path: the second write
	// wins, the first run commits it and reports success, and the second is
	// told it failed while the destination holds ITS plan. Reserving the name
	// here also means the write below renames onto a path nothing else owns.
	// The writers used to create this directory on their way to the file.
	// Reserving a temp inside it happens first now, so the MkdirAll has to
	// move here too — otherwise `--out artifacts/plan.json` into a directory
	// that does not exist yet fails where it used to work.
	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		return stagedArtifact{}, err
	}
	reserved, err := os.CreateTemp(filepath.Dir(final), filepath.Base(final)+".ob-tmp"+suffix+"-")
	if err != nil {
		return stagedArtifact{}, err
	}
	staged := reserved.Name()
	if err := reserved.Close(); err != nil {
		_ = os.Remove(staged)
		return stagedArtifact{}, err
	}
	// A previous run killed between commit and settle leaves a backup here.
	// Clearing it bounds the leak to one crashed run — but ONLY when the
	// destination itself survives. A backup with no destination beside it is
	// the other kill window, between the two renames inside commit(), and
	// there it is the caller's only remaining copy: deleting it would destroy
	// the data this machinery exists to protect.
	backup := final + ".ob-bak" + suffix
	orphan := false
	switch {
	case fileExists(final):
		_ = os.Remove(backup)
	case fileExists(backup):
		orphan = true
	}
	if err := write(staged); err != nil {
		_ = os.Remove(staged)
		return stagedArtifact{}, err
	}
	return stagedArtifact{staged: staged, final: final, backup: backup, orphan: orphan}, nil
}

func (a *stagedArtifact) commit() error {
	if a.staged == "" {
		return nil
	}
	switch info, err := os.Stat(a.final); {
	case err == nil && !info.Mode().IsRegular():
		// Never move a directory or device aside: discard would then delete
		// the backup, so "replace the artifact" would quietly destroy
		// something this command has no business touching.
		return fmt.Errorf("refusing to replace %s: not a regular file", a.final)
	case err == nil:
		if err := os.Rename(a.final, a.backup); err != nil {
			return err
		}
		a.replaced = true
	case !errors.Is(err, os.ErrNotExist):
		return err
	}
	if err := os.Rename(a.staged, a.final); err != nil {
		a.restore()
		return err
	}
	return nil
}

// rollback undoes a commit: the new file goes away and whatever it replaced
// comes back. Best effort — the failure being reported is the one worth
// surfacing — but without it a partial set leaves one destination holding a
// previous run's content and another deleted outright.
func (a *stagedArtifact) rollback() {
	if a.staged == "" {
		return
	}
	_ = os.Remove(a.final)
	a.restore()
}

func (a *stagedArtifact) restore() {
	if !a.replaced {
		return
	}
	if err := os.Rename(a.backup, a.final); err != nil {
		// The caller's previous file is still in the backup and is now the
		// only copy: rollback has already removed final. Keeping the flag set
		// stops discard from deleting it, which would leave neither the new
		// artifact nor the one it replaced.
		return
	}
	a.replaced = false
}

// settle releases the backup once the WHOLE set has landed. It cannot happen
// inside commit(): a later artifact's failure rolls this one back, and that
// restore needs the backup. Only when every commit and the directory sync have
// succeeded is the previous file safe to drop.
func (a *stagedArtifact) settle() {
	if a.staged == "" {
		return
	}
	// Both the file this run replaced and any orphan from a crashed earlier
	// run are redundant now: the destination exists and holds the new
	// artifact, so neither is anybody's last copy.
	_ = os.Remove(a.backup)
	a.replaced, a.orphan = false, false
}

func (a stagedArtifact) discard() {
	if a.staged == "" {
		return
	}
	_ = os.Remove(a.staged)
	// Only when nothing is depending on it. A still-set replaced flag means a
	// restore failed and this backup holds the caller's only copy; orphan
	// means it was already the only copy before this run began. Either way
	// the run is ending without a destination in place, so the backup stays.
	if !a.replaced && !a.orphan {
		_ = os.Remove(a.backup)
	}
}

// commitArtifactSet moves a whole set into place, or puts the tree back as it
// was. Every staged and backup file is removed on the way out whatever happens,
// and a failure part-way restores what already landed: a caller told the run
// failed must find neither a fresh artifact nor a missing one.
func commitArtifactSet(artifacts ...stagedArtifact) error {
	defer func() {
		for _, artifact := range artifacts {
			artifact.discard()
		}
	}()
	rollback := func(upto int) {
		for i := upto - 1; i >= 0; i-- {
			artifacts[i].rollback()
		}
	}
	for i := range artifacts {
		if err := artifacts[i].commit(); err != nil {
			rollback(i)
			return err
		}
	}
	// The writers fsync the file and then the directory, but that sync made
	// the STAGED name durable — the renames above are further directory
	// changes with nothing behind them. Without this, a command can report
	// success and a power loss can leave the destination absent while the
	// .ob-tmp name survives.
	//
	// A sync failure rolls the set back like any other: returning it with the
	// renames standing would report failure with a fresh, complete, approvable
	// artifact in place.
	if err := syncArtifactDirectories(artifacts); err != nil {
		rollback(len(artifacts))
		return err
	}
	// Nothing can roll back past this point, so the replaced files are no
	// longer anybody's only copy. Without this they outlive every successful
	// run: a second `ob plan` over the same path would leave the previous
	// run's complete, approvable artifact beside the new one forever.
	for i := range artifacts {
		artifacts[i].settle()
	}
	return nil
}

func syncArtifactDirectories(artifacts []stagedArtifact) error {
	synced := map[string]bool{}
	for _, artifact := range artifacts {
		// Zero-value artifacts were never staged. filepath.Dir("") is ".",
		// which would fsync the process's working directory on every run that
		// passed no --backup-report-out, and fail the whole set if that
		// directory has since been removed.
		if artifact.staged == "" {
			continue
		}
		dir := filepath.Dir(artifact.final)
		if synced[dir] {
			continue
		}
		synced[dir] = true
		directory, err := os.Open(dir)
		if err != nil {
			return err
		}
		if err := directory.Sync(); err != nil {
			_ = directory.Close()
			return err
		}
		if err := directory.Close(); err != nil {
			return err
		}
	}
	return nil
}

// sameArtifactPath reports whether two destinations are the same file. A raw
// string comparison lets `plan.json` and `./plan.json` (or `dir/../plan.json`)
// through, and the two would then commit onto one path with the second
// silently winning.
func sameArtifactPath(left, right string) bool {
	// Resolve the PARENT, not the file: the artifacts do not exist yet, so
	// EvalSymlinks on the full path would fail. A symlinked output directory
	// otherwise defeats the guard entirely, and a lexical Clean over `..`
	// through a symlink gets it wrong in the other direction too.
	clean := func(path string) string {
		// Split BEFORE any cleaning: filepath.Abs runs a lexical Clean, which
		// collapses `link/..` to nothing and loses the symlink entirely. The
		// directory half is resolved on its own, where EvalSymlinks walks the
		// real chain, and only then is the base joined back on.
		dir, base := filepath.Split(path)
		if dir == "" {
			dir = "."
		}
		resolved, err := filepath.EvalSymlinks(dir)
		if err != nil {
			if absolute, absErr := filepath.Abs(path); absErr == nil {
				return filepath.Clean(absolute)
			}
			return filepath.Clean(path)
		}
		absolute, err := filepath.Abs(filepath.Join(resolved, base))
		if err != nil {
			return filepath.Join(resolved, base)
		}
		return absolute
	}
	return clean(left) == clean(right)
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}
