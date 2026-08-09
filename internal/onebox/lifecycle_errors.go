package onebox

import (
	"fmt"
	"sort"
	"strings"
)

// LifecycleFailure is the public, secret-free failure contract shared by
// lifecycle plans, event streams, terminal results, status, and doctor.
// Diagnostic detail remains in restricted local evidence; this record carries
// only a stable branchable code and a safe resolving command.
type LifecycleFailure struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Next    string `json:"next"`
}

type lifecycleFailureDefinition struct {
	message string
	next    string
}

var lifecycleFailureDefinitions = map[string]lifecycleFailureDefinition{
	"assurance_stale":                              {"continuous assurance evidence is no longer current", "ob status --output json"},
	"backup_conflict":                              {"another protected-service operation holds the serialization boundary", "ob status --output json"},
	"backup_driver_unsupported":                    {"the service driver has no qualified executable protection contract", "ob validate --output json"},
	"backup_encryption_unverified":                 {"the selected protection destination cannot prove its required encryption mode", "ob validate --output json"},
	"backup_interruption_not_authorized":           {"the recovery contract requires a recurring stopped-service window the author did not permit", "ob validate --output json"},
	"backup_retention_unsupported":                 {"the declared recovery history cannot map to qualified native retention semantics", "ob validate --output json"},
	"backup_stale":                                 {"the latest recoverable point is older than policy permits", "ob plan --output json"},
	"backup_target_not_independent":                {"the backup target shares the protected failure domain", "ob validate --output json"},
	"backup_target_unauthorized":                   {"the backup target credentials are unavailable, unsafe, or unauthorized", "ob plan --output json"},
	"backup_target_unknown":                        {"the protection policy selects no declared backup target", "ob validate --output json"},
	"backup_target_unreachable":                    {"the selected backup target cannot be reached", "ob plan --output json"},
	"disk_pressure_critical":                       {"a relevant filesystem lacks safe headroom for a space-increasing mutation", "ob status --output json"},
	"drill_deferred_capacity":                      {"a restore drill was deferred before materialization because aggregate staging headroom is insufficient", "ob status --output json"},
	"external_service_not_owned":                   {"the requested lifecycle mutation targets a dependency Onebox does not own", "ob status --output json"},
	"external_service_state_stale":                 {"an external-service observation changed after planning", "ob plan --output json"},
	"protected_service_identity_changed":           {"a protected service name would orphan durable recovery identity", "ob validate --output json"},
	"protected_service_patch_incompatible":         {"the candidate protected service or helper cannot prove repository and runtime compatibility", "ob status --output json"},
	"protected_service_patch_unsupported":          {"no exact qualified protected current-to-candidate transition exists", "ob status --output json"},
	"protection_disable_pending":                   {"protection removal is waiting for an authorized safe prerequisite reversal", "ob status --output json"},
	"protection_disablement_not_authorized":        {"protection disablement requires a fresh strong approval bound to current state", "ob status --output json"},
	"protection_disablement_overdue":               {"protection disablement remains pending beyond its action deadline", "ob status --output json"},
	"protection_enablement_restart_not_authorized": {"a restart-bound protection prerequisite lacks fresh strong approval", "ob validate --output json"},
	"protection_image_revert_unsafe":               {"the requested image reversion would strand an effective protection prerequisite", "ob status --output json"},
	"protection_image_update_overdue":              {"a qualified protected service image publication missed its maintenance target", "ob status --output json"},
	"protection_prerequisite_drifted":              {"a live prerequisite no longer matches the verified protection configuration", "ob validate --output json"},
	"protection_service_image_unpublished":         {"no qualified immutable protection image is published for the observed service base", "ob status --output json"},
	"protection_service_patch_available":           {"a qualified exact protected service image transition is available", "ob service apply --output ndjson"},
	"protection_service_patch_required":            {"protection enablement requires a separate qualified same-major service patch first", "ob service apply --output ndjson"},
	"recovery_objective_unsupported":               {"the selected driver, version, or target cannot execute the declared recovery kind", "ob validate --output json"},
	"replay_continuity_broken":                     {"the native replay sequence has a gap inside the required recovery window", "ob plan --output json"},
	"restore_drill_schedule_too_sparse":            {"the restore-drill cadence cannot keep restore proof current", "ob validate --output json"},
	"restore_state_stale":                          {"live service, volume, or repository state changed after restore planning", "ob status --output json"},
	"service_image_digest_unavailable":             {"the exact immutable service image required by recovery is unavailable", "ob status --output json"},
	"service_image_patch_disable_pending":          {"service image refresh is refused while safe protection disablement is pending", "ob status --output json"},
	"service_major_upgrade_unsupported":            {"the requested service image transition crosses an unsupported major version", "ob status --output json"},
}

func NewLifecycleFailure(code string) (LifecycleFailure, error) {
	definition, ok := lifecycleFailureDefinitions[code]
	if !ok {
		return LifecycleFailure{}, fmt.Errorf("unknown lifecycle failure code %q", code)
	}
	failure := LifecycleFailure{Code: code, Message: definition.message, Next: definition.next}
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
	if failure.Message != definition.message || failure.Next != definition.next {
		return fmt.Errorf("lifecycle failure %q does not match its public definition", failure.Code)
	}
	if !safeResolvingCommand(failure.Next) {
		return fmt.Errorf("lifecycle failure %q has an unsafe resolving command", failure.Code)
	}
	return nil
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

func safeResolvingCommand(command string) bool {
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

// reservedLifecycleFailures are codes the contract enumerates that no path
// raises today. The operations behind them exist in the model — protection
// enable and disable among them — but are wired to no CLI verb, so an operator
// cannot reach them. They are kept so the code set is stable when those land,
// and named here so the reference can mark them rather than presenting a
// failure that cannot occur.
var reservedLifecycleFailures = map[string]struct{}{
	"assurance_stale":                              {},
	"backup_stale":                                 {},
	"disk_pressure_critical":                       {},
	"drill_deferred_capacity":                      {},
	"external_service_not_owned":                   {},
	"external_service_state_stale":                 {},
	"protected_service_patch_incompatible":         {},
	"protection_enablement_restart_not_authorized": {},
	"protection_image_update_overdue":              {},
	"protection_prerequisite_drifted":              {},
	"protection_service_patch_available":           {},
	"protection_service_patch_required":            {},
	"replay_continuity_broken":                     {},
	"restore_state_stale":                          {},
	"service_major_upgrade_unsupported":            {},
}

// LifecycleFailureReserved reports whether a code is enumerated but not yet
// reachable, so the reference can mark it rather than presenting a failure an
// operator cannot cause.
func LifecycleFailureReserved(code string) bool {
	_, reserved := reservedLifecycleFailures[code]
	return reserved
}
