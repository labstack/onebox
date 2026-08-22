package app

import (
	"errors"
	"regexp"
	"sort"
	"strings"

	"github.com/labstack/onebox/internal/imageref"
)

type ServiceImageCandidate struct {
	Image               string
	PublicationVerified bool
	DigestAvailable     bool
	CacheVerified       bool
	ExactTransition     bool
}

// ServiceRuntimeState is observed durable lifecycle state, not authoring
// input. Rendering consumes it through WithServiceRuntimeStates so backup
// image behavior cannot be inferred from policy presence alone.
type ServiceRuntimeState struct {
	BackupState         string
	ServiceImage        string
	PublicationVerified bool
	DigestAvailable     bool
	CacheVerified       bool
	ManifestRootImages  []string
	TagObservedDigest   string
	RefreshCandidate    *ServiceImageCandidate
	LastEffective       *BackupEffectiveProjection
	// DatabaseSystemIdentifier is PostgreSQL's stable identity for the data
	// volume. BackupRepositoryGeneration is the generation segment bound at
	// enablement; it is empty only for repositories using the legacy layout.
	DatabaseSystemIdentifier   string
	BackupRepositoryGeneration string
}

type ServiceImageSelection struct {
	Image          string
	RetainedImages []string
	Origin         Origin
}

// ServiceRuntimeState returns the observed lifecycle binding for one service.
// Engine safety checks need the recorded database identity before they allow
// Compose to touch its volume.
func (r *Resolved) ServiceRuntimeState(service string) (ServiceRuntimeState, bool) {
	state, ok := r.serviceRuntime[service]
	return state, ok
}

func (r *Resolved) WithServiceRuntimeStates(states map[string]ServiceRuntimeState) (*Resolved, error) {
	if r == nil {
		return nil, errors.New("resolved project is nil")
	}
	copy := *r
	copy.serviceRuntime = make(map[string]ServiceRuntimeState, len(states))
	for service, state := range states {
		if _, ok := r.Services[service]; !ok {
			return nil, errf("project_invalid", "services."+service, "ob status --output json", "service runtime state names an undeclared service")
		}
		state.ManifestRootImages = append([]string(nil), state.ManifestRootImages...)
		if state.RefreshCandidate != nil {
			candidate := *state.RefreshCandidate
			state.RefreshCandidate = &candidate
		}
		if state.LastEffective != nil {
			last := *state.LastEffective
			state.LastEffective = &last
		}
		if err := state.validate(service); err != nil {
			return nil, err
		}
		copy.serviceRuntime[service] = state
	}
	return &copy, nil
}

func (state ServiceRuntimeState) validate(service string) error {
	if !contains([]string{"never-enabled", "enabled", "disable-pending", "disabled"}, state.BackupState) {
		return errf("project_invalid", "services."+service, "ob status --output json", "service runtime state has an invalid backup lifecycle state")
	}
	if state.ServiceImage != "" {
		if err := validatePinnedServiceImage(state.ServiceImage); err != nil {
			return errf("service_image_digest_unavailable", "services."+service, "ob service status --output json", "%v", err)
		}
	}
	if state.DatabaseSystemIdentifier != "" && !postgresSystemIdentifier.MatchString(state.DatabaseSystemIdentifier) {
		return errf("project_invalid", "services."+service, "ob backup status --output json", "service runtime state has an invalid PostgreSQL system identifier")
	}
	if state.BackupRepositoryGeneration != "" && state.BackupRepositoryGeneration != state.DatabaseSystemIdentifier {
		return errf("project_invalid", "services."+service, "ob backup status --output json", "backup repository generation does not match the PostgreSQL system identifier")
	}
	previous := ""
	for _, image := range state.ManifestRootImages {
		if err := validatePinnedServiceImage(image); err != nil {
			return errf("service_image_digest_unavailable", "services."+service, "ob backup inspect --output json", "manifest image root is invalid: %v", err)
		}
		if previous != "" && image <= previous {
			return errf("project_invalid", "services."+service, "ob backup inspect --output json", "manifest image roots must be unique and sorted")
		}
		previous = image
	}
	if state.RefreshCandidate != nil {
		if err := validatePinnedServiceImage(state.RefreshCandidate.Image); err != nil {
			return errf("service_image_digest_unavailable", "services."+service, "ob service status --output json", "candidate service image is invalid: %v", err)
		}
	}
	return nil
}

var postgresSystemIdentifier = regexp.MustCompile(`^[0-9]{1,20}$`)

func (r *Resolved) selectServiceImage(serviceName, tagImage string) (ServiceImageSelection, error) {
	state, observed := r.serviceRuntime[serviceName]
	if !observed || state.BackupState == "never-enabled" || state.BackupState == "disabled" {
		return ServiceImageSelection{Image: tagImage, Origin: OriginAuthored}, nil
	}
	if state.ServiceImage == "" {
		return ServiceImageSelection{}, errf("backup_image_revert_unsafe", "services."+serviceName+".version", "ob backup disable --output ndjson", "protected runtime state has no immutable service image; refusing tag reversion")
	}
	if !state.PublicationVerified {
		return ServiceImageSelection{}, errf("backup_service_image_unpublished", "services."+serviceName+".version", "ob service status --output json", "protected service image has no verified publication provenance")
	}
	if !state.DigestAvailable && !state.CacheVerified {
		return ServiceImageSelection{}, errf("service_image_digest_unavailable", "services."+serviceName+".version", "ob service status --output json", "protected service image is unavailable from the registry and exact local cache")
	}
	selected := state.ServiceImage
	retained := append([]string{state.ServiceImage}, state.ManifestRootImages...)
	if candidate := state.RefreshCandidate; candidate != nil {
		if state.BackupState == "disable-pending" {
			return ServiceImageSelection{}, errf("service_image_patch_disable_pending", "services."+serviceName+".version", "ob backup disable --output ndjson", "protected service image refresh is refused while disablement is pending")
		}
		if !candidate.PublicationVerified {
			return ServiceImageSelection{}, errf("backup_service_image_unpublished", "services."+serviceName+".version", "ob service status --output json", "candidate protected service image has no verified publication provenance")
		}
		if !candidate.DigestAvailable && !candidate.CacheVerified {
			return ServiceImageSelection{}, errf("service_image_digest_unavailable", "services."+serviceName+".version", "ob service status --output json", "candidate protected service image is unavailable from the registry and exact local cache")
		}
		if !candidate.ExactTransition {
			return ServiceImageSelection{}, errf("service_patch_unsupported", "services."+serviceName+".version", "ob service status --output json", "candidate image has no exact qualified protected transition")
		}
		selected = candidate.Image
		retained = append(retained, candidate.Image)
	}
	sort.Strings(retained)
	retained = compactStrings(retained)
	return ServiceImageSelection{Image: selected, RetainedImages: retained, Origin: OriginObserved}, nil
}

// DeclaredServiceImage is the reference the project authored — the driver's
// repository at the declared version — with no regard for what the host is
// running. It is deliberately not ServiceImageForRuntime: that one answers with
// the pinned digest while a service is protected and with the tag once it is
// not, so it cannot be used to recognise "the same image the operator asked
// for" across an enable/disable cycle. This can.
func (r *Resolved) DeclaredServiceImage(serviceName string) (string, error) {
	service, ok := r.Services[serviceName]
	if !ok {
		return "", errf("project_invalid", "services."+serviceName, "ob validate", "service is not declared")
	}
	driverName := service.Driver
	if driverName == "" {
		driverName = serviceName
	}
	driver, ok := drivers[driverName]
	if !ok {
		return "", errf("unknown_service_driver", "services."+serviceName, "ob validate", "no managed driver named %q", driverName)
	}
	return driver.image + ":" + versionString(service.Version), nil
}

// ServiceImageForRuntime exposes the same selection used by generation so
// planners, cache checks, and pruning all retain identical immutable roots.
func (r *Resolved) ServiceImageForRuntime(serviceName string) (ServiceImageSelection, error) {
	service, ok := r.Services[serviceName]
	if !ok {
		return ServiceImageSelection{}, errf("project_invalid", "services."+serviceName, "ob validate", "service is not declared")
	}
	driverName := service.Driver
	if driverName == "" {
		driverName = serviceName
	}
	driver, ok := drivers[driverName]
	if !ok {
		return ServiceImageSelection{}, errf("unknown_service_driver", "services."+serviceName, "ob validate", "no managed driver named %q", driverName)
	}
	return r.selectServiceImage(serviceName, driver.image+":"+versionString(service.Version))
}

// serviceImageDigest matches a full sha256 reference, which is what "pinned"
// means for a protected service image.
var serviceImageDigest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func validatePinnedServiceImage(image string) error {
	if err := imageref.Validate(image); err != nil {
		return err
	}
	marker := strings.LastIndex(image, "@sha256:")
	if marker < 1 || len(image)-marker != len("@sha256:")+64 || !serviceImageDigest.MatchString(image[marker+1:]) {
		return errors.New("service image must be pinned by lowercase sha256 digest")
	}
	return nil
}

func compactStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	out := values[:1]
	for _, value := range values[1:] {
		if value != out[len(out)-1] {
			out = append(out, value)
		}
	}
	return out
}
