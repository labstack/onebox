package onebox

import (
	"fmt"
	"sort"
	"strings"
)

// LifecycleFailure is the public, secret-free failure contract shared by
// lifecycle plans, event streams, terminal results, status, and doctor.
// Diagnostic detail remains in restricted local evidence; this record carries
// only a stable branchable code and one semantically classified safe command.
type LifecycleFailure struct {
	Code              string `json:"code"`
	Message           string `json:"message"`
	DiagnosticCommand string `json:"diagnostic_command,omitempty"`
	NextCommand       string `json:"next_command,omitempty"`
	ResolvingCommand  string `json:"resolving_command,omitempty"`
}

type lifecycleFailureDefinition struct {
	message string
	command string
}

var lifecycleFailureDefinitions = map[string]lifecycleFailureDefinition{
	"backup_conflict":                     {"another protected-service operation holds the serialization boundary", "ob status --output json"},
	"backup_driver_unsupported":           {"the service driver has no qualified executable backup contract", "ob validate --output json"},
	"backup_encryption_unverified":        {"the selected backup destination cannot prove its required encryption mode", "ob validate --output json"},
	"backup_interruption_not_authorized":  {"the recovery contract requires a recurring stopped-service window the author did not permit", "ob validate --output json"},
	"backup_retention_unsupported":        {"the declared recovery history cannot map to qualified native retention semantics", "ob validate --output json"},
	"backup_target_not_independent":       {"the backup target shares the protected failure domain", "ob validate --output json"},
	"backup_target_unknown":               {"the backup policy selects no declared backup target", "ob validate --output json"},
	"service_patch_unsupported":           {"no exact qualified protected current-to-candidate transition exists", "ob status --output json"},
	"backup_disable_pending":              {"backup removal is waiting for an authorized safe prerequisite reversal", "ob status --output json"},
	"backup_disablement_overdue":          {"backup disablement remains pending beyond its action deadline", "ob status --output json"},
	"backup_image_revert_unsafe":          {"the requested image reversion would strand an effective backup prerequisite", "ob status --output json"},
	"backup_service_image_unpublished":    {"no qualified immutable backup image is published for the observed service base", "ob status --output json"},
	"recovery_objective_unsupported":      {"the selected driver, version, or target cannot execute the declared recovery kind", "ob validate --output json"},
	"drill_schedule_too_sparse":           {"the declared drill cadence is too sparse to keep restore proof within its maximum age", "ob validate --output json"},
	"service_image_digest_unavailable":    {"the exact immutable service image required by recovery is unavailable", "ob status --output json"},
	"service_image_patch_disable_pending": {"service image refresh is refused while safe backup disablement is pending", "ob status --output json"},
}

func NewLifecycleFailure(code string) (LifecycleFailure, error) {
	definition, ok := lifecycleFailureDefinitions[code]
	if !ok {
		return LifecycleFailure{}, fmt.Errorf("unknown lifecycle failure code %q", code)
	}
	failure := LifecycleFailure{Code: code, Message: definition.message}
	setLifecycleGuidance(&failure, definition.command)
	if err := failure.Validate(); err != nil {
		return LifecycleFailure{}, err
	}
	return failure, nil
}

func (failure LifecycleFailure) Error() string { return failure.Code + ": " + failure.Message }

func (failure LifecycleFailure) Validate() error {
	definition, ok := lifecycleFailureDefinitions[failure.Code]
	if !ok {
		return fmt.Errorf("unknown lifecycle failure code %q", failure.Code)
	}
	if failure.Message != definition.message || failure.GuidanceCommand() != definition.command {
		return fmt.Errorf("lifecycle failure %q does not match its public definition", failure.Code)
	}
	set := 0
	for _, command := range []string{failure.DiagnosticCommand, failure.NextCommand, failure.ResolvingCommand} {
		if command != "" {
			set++
		}
	}
	if set != 1 || !safeGuidanceCommand(failure.GuidanceCommand()) {
		return fmt.Errorf("lifecycle failure %q has invalid command guidance", failure.Code)
	}
	return nil
}

func (failure LifecycleFailure) GuidanceCommand() string {
	for _, command := range []string{failure.DiagnosticCommand, failure.NextCommand, failure.ResolvingCommand} {
		if command != "" {
			return command
		}
	}
	return ""
}

func (failure LifecycleFailure) GuidanceRole() string {
	switch {
	case failure.DiagnosticCommand != "":
		return "diagnostic"
	case failure.NextCommand != "":
		return "next"
	case failure.ResolvingCommand != "":
		return "resolving"
	default:
		return ""
	}
}

func setLifecycleGuidance(failure *LifecycleFailure, command string) {
	switch GuidanceRoleForCommand(command) {
	case "next":
		failure.NextCommand = command
	case "diagnostic":
		failure.DiagnosticCommand = command
	default:
		failure.ResolvingCommand = command
	}
}

// GuidanceRoleForCommand classifies a safe Onebox command by what executing it
// can honestly accomplish for the reported condition.
func GuidanceRoleForCommand(command string) string {
	if strings.ContainsAny(command, "<>") {
		return "next"
	}
	for _, prefix := range []string{"ob plan", "ob job plan", "ob approve"} {
		if command == prefix || strings.HasPrefix(command, prefix+" ") {
			return "next"
		}
	}
	for _, prefix := range []string{
		"ob status", "ob validate", "ob doctor", "ob audit", "ob help",
		"ob canonical", "ob preflight", "ob preview", "ob schema", "ob version", "ob logs",
		"ob secrets list", "ob service status", "ob backup status", "ob assurance inspect",
		"ob backup target inspect", "ob backup list", "ob backup inspect", "ob restore inspect",
		"ob housekeeping status",
	} {
		if command == prefix || strings.HasPrefix(command, prefix+" ") {
			return "diagnostic"
		}
	}
	return "resolving"
}

func LifecycleFailureCodes() []string {
	codes := make([]string, 0, len(lifecycleFailureDefinitions))
	for code := range lifecycleFailureDefinitions {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	return codes
}

func LifecycleFailureMeaning(code string) (string, bool) {
	definition, ok := lifecycleFailureDefinitions[code]
	return definition.message, ok
}

func safeGuidanceCommand(command string) bool {
	if !strings.HasPrefix(command, "ob ") || len(command) > 256 || strings.ContainsAny(command, "\r\n\x00;|&`$<>") {
		return false
	}
	lower := strings.ToLower(command)
	for _, unsafe := range []string{"password=", "secret=", "token=", "credential="} {
		if strings.Contains(lower, unsafe) {
			return false
		}
	}
	return true
}
