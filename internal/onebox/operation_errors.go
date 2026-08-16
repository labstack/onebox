package onebox

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// OperationFailure is the public definition of a failure the CLI and engine
// raise while running a command, as distinct from a project-file validation
// code (which the loader owns) and a lifecycle failure code (which the
// protection contract owns). Those two families were already enumerated and
// published; these were not, so an operator or agent branching on a code the
// binary actually emits had nothing to read.
//
// A definition carries at most one safe command, classified into the same
// diagnostic/next/resolving roles the lifecycle contract uses. Some failures
// have no honest guidance — a local write failure is not resolved by running
// another Onebox command — and those carry none rather than an invented one.
type OperationFailure struct {
	Code    string
	Message string
	Command string
}

// GuidanceRole reports which semantic role this definition's command occupies,
// or "" when the failure carries no command.
func (failure OperationFailure) GuidanceRole() string {
	if failure.Command == "" {
		return ""
	}
	return GuidanceRoleForCommand(failure.Command)
}

var operationFailureDefinitions = map[string]OperationFailure{
	"activation_refused": {
		Message: "the release cannot be activated from its recorded manifest state",
		Command: "ob abort --output ndjson",
	},
	"approval_expired": {
		Message: "the local confirmation is older than its validity window",
		Command: "ob approve --plan <path>",
	},
	"approval_failed": {
		Message: "the approval artifact is missing, unreadable, or not bound to this plan",
		Command: "ob approve --plan <path>",
	},
	"artifact_write_failed": {
		Message: "a requested local artifact could not be written",
	},
	"cancelled": {
		// Covers both a declined confirmation and an interrupted operation. It
		// must not claim nothing changed: an interrupt can land after a deploy
		// has already mutated the host, and the journal is what says how far it
		// got.
		Message: "the operation was cancelled or interrupted; consult the journal for how far it got",
		Command: "ob audit --output json",
	},
	"command_failed": {
		Message: "the command failed; diagnostic detail is on stderr",
	},
	"compose_invalid": {
		Message: "the Compose project could not be loaded",
		Command: "ob validate --output json",
	},
	"compose_not_found": {
		Message: "no Compose file was found to adopt",
		Command: "ob init --output json",
	},
	"config_exists": {
		Message: "a project file already exists and init refuses to overwrite it",
		Command: "ob validate --output json",
	},
	"config_write_failed": {
		Message: "the project file could not be written",
	},
	"confirmation_failed": {
		Message: "the typed confirmation did not match the release identifier",
		Command: "ob approve --plan <path>",
	},
	"divergence_detected": {
		Message: "the live release does not match the recorded release state",
		// Not `ob status`: this code is raised BY ob status, so publishing it
		// tells a caller to re-run the command that just failed.
		Command: "ob audit --output json",
	},
	"doctor_failed": {
		Message: "a local readiness check failed",
		// No guidance on purpose. This is raised BY ob doctor, so naming it
		// tells a caller to re-run the command that just failed; and no other
		// command re-checks local readiness — `ob validate` inspects the
		// project file, which is a different question. The failing checks are
		// already in the envelope's details.
	},
	"exec_failed": {
		Message: "the audited exec could not be completed",
		Command: "ob status --output json",
	},
	"finalize_refused": {
		Message: "the release cannot be finalized because the recorded activation evidence disagrees with the live host",
		Command: "ob status --output json",
	},
	"host_owner_mismatch": {
		Message: "this host is owned by a different Onebox application, and one host has one owner",
		Command: "ob preflight --output json",
	},
	"job_plan_failed": {
		Message: "the one-shot job plan could not be produced",
		Command: "ob job plan <job> --output json",
	},
	"logs_failed": {
		Message: "log retrieval failed",
		Command: "ob status --output json",
	},
	"manifest_invalid": {
		Message: "a release manifest is not valid closed JSON for its schema",
		Command: "ob status --output json",
	},
	"manifest_missing": {
		Message: "a release directory carries no manifest, so its lifecycle state is unknown",
		Command: "ob status --output json",
	},
	"manifest_mode_unsafe": {
		Message: "a release manifest is not mode 0600 on the host",
		Command: "ob doctor --output json",
	},
	"manifest_read_failed": {
		Message: "a release manifest could not be read from the host",
		Command: "ob status --output json",
	},
	"manifest_schema_unknown": {
		Message: "a release manifest declares a schema this binary does not support",
		Command: "ob version --output json",
	},
	"manifest_write_failed": {
		Message: "a release manifest could not be written to the host",
		Command: "ob status --output json",
	},
	"migration_backup_required": {
		Message: "a migration-effect step requires a plan-bound backup report or an audited override",
		Command: "ob plan --backup-report-out <path>",
	},
	"migration_gate_closed": {
		// Command-neutral: both `ob resume` and `ob abort` raise this code,
		// and a sentence naming recovery told an operator who asked to roll
		// back that something else was refused. The command that was refused
		// is in the error's own message; the guidance below is the default,
		// which the typed error overrides per command.
		Message: "the interrupted release ran rollback-unknown data effects, so the requested recovery action is refused",
		Command: "ob resume --output ndjson",
	},
	"post_activation_failed": {
		Message: "the release is serving, but the work after activation did not finish",
		Command: "ob resume --output ndjson",
	},
	"operation_failed": {
		Message: "the operation failed; inspect stderr and journal evidence",
		Command: "ob audit --output json",
	},
	"output_mode_incompatible": {
		Message: "the requested output mode is incompatible with this command",
		Command: "ob help",
	},
	"plan_expired": {
		Message: "the sealed plan is older than its validity window",
		// A job plan re-plans with `ob job plan <job>`; PlanExpiredError carries
		// the kind and overrides this default through GuidanceCommand.
		Command: "ob plan --output json",
	},
	"plan_failed": {
		Message: "the plan could not be produced; inspect stderr for local diagnostics",
		Command: "ob validate --output json",
	},
	"plan_required": {
		Message: "this command requires a sealed plan when run with structured output",
		Command: "ob plan --output json",
	},
	"preflight_failed": {
		Message: "a target readiness check failed before any mutation",
		// No guidance, same reasoning as doctor_failed. Every preflight check
		// is a fact about the target host — base path, owner record, name
		// collisions — and `ob doctor` only ever looks at the local machine,
		// so it would pass cleanly and explain nothing.
	},
	"recovery_incomplete": {
		Message: "recovery did not reach its verified terminal state",
		Command: "ob resume --output ndjson",
	},
	"rollback_target_missing": {
		Message: "no previously serving release is recorded as a rollback target",
		// Not `ob deploy --output ndjson`: structured deploy refuses without a
		// plan, so that guidance would send an agent straight into
		// plan_required. Deploying forward starts with a plan.
		Command: "ob plan --output json",
	},
	"secret_cleanup_pending": {
		Message: "the rotation is applied and verified, but removing the retired generation did not finish",
		Command: "ob secrets push --output ndjson",
	},
	"secret_declaration_not_deployed": {
		Message: "the deployed release does not declare this secret graph",
		// Deploying forward starts with a plan; `ob deploy --output ndjson`
		// refuses without one.
		Command: "ob plan --output json",
	},
	"secret_entry_not_selected": {
		Message: "more than one editable secret source exists, so an entry identifier is required",
		Command: "ob secrets list --output json",
	},
	"secret_generation_not_deployed": {
		Message: "the deployed release predates opaque secret generations",
		Command: "ob plan --output json",
	},
	"secret_recovery_incomplete": {
		Message: "secret recovery did not reach its verified terminal state",
		Command: "ob secrets push --output ndjson",
	},
	"secret_rotation_rolled_back": {
		Message: "an interrupted rotation was restored to its prior generation and the requested payload was not applied",
		Command: "ob secrets push --output ndjson",
	},
	"sops_failed": {
		Message: "the SOPS editor exited with a failure",
	},
	"sops_source_missing": {
		Message: "a declared encrypted source file does not exist",
		Command: "ob validate --output json",
	},
	"status_failed": {
		Message: "the status snapshot could not be read",
		Command: "ob doctor --output json",
	},
	"unknown_runtime_target": {
		Message: "the requested runtime target is not declared",
		Command: "ob status --output json",
	},
}

// OperationFailureCodes lists every code in this registry, sorted. Codes owned
// by the loader or lifecycle families are deliberately absent — image_unresolved
// is raised by the engine but documented by the loader.
func OperationFailureCodes() []string {
	codes := make([]string, 0, len(operationFailureDefinitions))
	for code := range operationFailureDefinitions {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	return codes
}

// OperationFailureFor returns the published definition for a code.
func OperationFailureFor(code string) (OperationFailure, bool) {
	definition, ok := operationFailureDefinitions[code]
	if !ok {
		return OperationFailure{}, false
	}
	definition.Code = code
	return definition, true
}

// OperationFailureMeaning returns the published sentence for a code, and
// whether the code is enumerated at all.
func OperationFailureMeaning(code string) (string, bool) {
	definition, ok := operationFailureDefinitions[code]
	return definition.Message, ok
}

// OperationFailureCommand returns the published guidance command for a code, or
// "" when the failure has no honest command to offer.
func OperationFailureCommand(code string) string {
	return operationFailureDefinitions[code].Command
}

// safeOperationCommand is safeGuidanceCommand with angle brackets permitted.
// A lifecycle failure names a command an operator can run verbatim, so a
// placeholder there is a defect. An operation failure often cannot: the plan
// path and the image reference are the operator's to choose, and
// GuidanceRoleForCommand already treats a bracketed command as the "next"
// role — a step to complete rather than a command to execute blindly.
func safeOperationCommand(command string) bool {
	// Placeholders are allowed; redirects are not. Bracket pairs that look like
	// a named hole (<path>, <workload>=<reference>) are removed before the
	// safety scan, and anything else containing < or > is refused — stripping
	// them unconditionally would have let `ob audit > /tmp/x` through the very
	// check that exists to stop it.
	stripped := placeholderHole.ReplaceAllString(command, "")
	if strings.ContainsAny(stripped, "<>") {
		return false
	}
	return safeGuidanceCommand(stripped)
}

var placeholderHole = regexp.MustCompile(`<[a-z][a-z0-9-]*>`)

func init() {
	// A definition with no meaning, or one naming an unsafe command, would
	// publish guidance the contract forbids. Fail at load rather than ship it.
	for code, definition := range operationFailureDefinitions {
		if definition.Message == "" {
			panic(fmt.Sprintf("operation failure %q has no meaning", code))
		}
		if definition.Command != "" && !safeOperationCommand(definition.Command) {
			panic(fmt.Sprintf("operation failure %q names an unsafe guidance command", code))
		}
	}
}

// SafeGuidanceCommand reports whether a string may be published in a guidance
// field. The lifecycle contract already enforces this on its own definitions;
// exporting it lets the CLI apply the same rule to guidance that arrives from
// the loader, where a "next" value is authored per error and is not guaranteed
// to be a command at all.
func SafeGuidanceCommand(command string) bool { return safeOperationCommand(command) }
