package app

// The error codes are enumerated so an agent can branch on a failure without
// reading the sentence, and so nobody can add one by accident.
//
// A code is a promise: it names a kind of failure that will keep meaning the
// same thing across releases. Adding one is a deliberate act — the test in this
// package refuses any code the loader emits that is not listed here, and any
// code listed here that nothing emits, so the enumeration cannot drift in
// either direction.
//
// The message beside each is what the code means, not what any one instance of
// it says.
var errorCodes = map[string]string{
	// The document could not be read at all.
	"project_unreadable":          "the project file could not be read",
	"project_unparsable":          "the project file is not valid YAML, or is not a mapping",
	"schema_identity_missing":     "the project declares no api_version",
	"schema_identity_unsupported": "the project declares an api_version this binary does not speak",

	// The document is not what the contract defines.
	"unknown_field":   "a field the contract does not define",
	"project_invalid": "a value that does not satisfy the contract",

	// Rules across more than one field.
	"app_required":                 "the shorthand form needs an application name to attach the workload to",
	"no_environment":               "a project must declare at least one environment",
	"no_workload":                  "a project must declare at least one workload",
	"workload_malformed":           "a workload is not a mapping",
	"workload_source":              "a workload declares other than exactly one of build, image or compose",
	"stateful_replicas":            "a workload keeping durable state asks for more than one replica",
	"strategy_ungated":             "a rolling release is asked for by a workload with no health check to gate it",
	"shorthand_and_workloads":      "top-level shorthand cannot be combined with a workloads block",
	"routing_exclusive":            "the domain shorthand and the routes list say the same thing twice",
	"routing_incomplete":           "domain and port are declared together or not at all",
	"route_collision":              "two workloads claim the same address",
	"route_without_proxy":          "a route is declared with nothing to route it",
	"identifier_collision":         "a name is used by both a workload and a service",
	"derived_name_too_long":        "a name Onebox derives exceeds the runtime's limit",
	"unknown_prerequisite":         "a prerequisite names something the project does not declare",
	"prerequisite_has_no_health":   "a wait for health names something with no health check",
	"unknown_service_driver":       "a service names a driver Onebox has no implementation for",
	"service_settings_unsupported": "a setting was declared for a driver with no way to apply it",
	"schedule_untranslatable":      "a cron expression whose meaning the host's scheduler cannot preserve",
	"unknown_environment":          "an environment the project does not declare",

	// Repository paths.
	"path_absolute":           "a repository path may not be absolute",
	"path_escapes_repository": "a path resolves outside the project directory",
	"path_unresolvable":       "a path could not be resolved",

	// Environment overrides.
	"override_not_permitted":    "a field that may not vary per environment",
	"override_unknown_workload": "an override names a workload the project does not declare",
	"override_unknown_service":  "an override names a service the project does not declare",
	"override_invalid":          "an override produced a value the contract does not accept",

	// Referenced Compose.
	"compose_ref_malformed":    "a Compose reference is not of the form path#service",
	"compose_file_unreadable":  "a referenced Compose file could not be read",
	"compose_file_unparsable":  "a referenced Compose file is not valid YAML",
	"compose_service_missing":  "a referenced Compose file has no such service",
	"compose_extends":          "a referenced service uses extends, which hides what runs",
	"compose_container_name":   "a referenced service fixes its container name, which Onebox owns",
	"compose_network_mode":     "a referenced service sets network_mode, which conflicts with the network it needs",
	"compose_ob_label":         "a referenced service carries a label in a namespace Onebox generates into",
	"compose_traefik_label":    "a referenced service carries routing labels while also declaring a route",
	"compose_ingress_attached": "a referenced service already attaches the ingress network",

	// Generation and the target.
	"env_file_unreadable":         "an environment file the project declares cannot be read",
	"health_port_unknown":         "an http health check has no port to probe and none can be derived",
	"connection_variable_claimed": "an authored value claims a name a managed-service connection supplies",
	"secrets_withdrawn":           "the withdrawn secrets block; environment files carry encrypted entries now",
	"image_unresolved":            "a build-sourced workload has no resolved image for this release",
	"render_failed":               "the runtime could not be rendered",
	"target_unreachable":          "the target could not be reached",
	"preflight_env_incomplete":    "an environment file is missing keys the project requires",

	// Ejection.
	"eject_destination_exists": "the ejection destination already exists",
	"eject_nothing_to_do":      "every workload already references a Compose file",
	"eject_failed":             "the runtime could not be handed over",

	// Onebox's own faults, which are never the author's to fix.
	"internal_copy_failed":   "a project could not be copied internally",
	"internal_decode_failed": "a normalised project could not be decoded",
}

// ErrorCodes lists every code the loader may emit, sorted.
func ErrorCodes() []string { return sortedKeys(errorCodes) }

// ErrorCodeMeaning describes a code, for a caller rendering a failure.
func ErrorCodeMeaning(code string) (string, bool) {
	meaning, ok := errorCodes[code]
	return meaning, ok
}
