package app

import (
	"errors"
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
// input. Rendering consumes it through WithServiceRuntimeStates so protection
// image behavior cannot be inferred from policy presence alone.
type ServiceRuntimeState struct {
	ProtectionState     string
	ServiceImage        string
	PublicationVerified bool
	DigestAvailable     bool
	CacheVerified       bool
	ManifestRootImages  []string
	TagObservedDigest   string
	RefreshCandidate    *ServiceImageCandidate
	LastEffective       *ProtectionEffectiveProjection
}

type ServiceImageSelection struct {
	Image          string
	RetainedImages []string
	Origin         Origin
}

func (resolved *Resolved) WithServiceRuntimeStates(states map[string]ServiceRuntimeState) (*Resolved, error) {
	if resolved == nil {
		return nil, errors.New("resolved project is nil")
	}
	copy := *resolved
	copy.serviceRuntime = make(map[string]ServiceRuntimeState, len(states))
	for service, state := range states {
		if _, ok := resolved.Services[service]; !ok {
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
	if !contains([]string{"never-enabled", "enabled", "disable-pending", "disabled"}, state.ProtectionState) {
		return errf("project_invalid", "services."+service, "ob status --output json", "service runtime state has an invalid protection lifecycle state")
	}
	if state.ServiceImage != "" {
		if err := validatePinnedServiceImage(state.ServiceImage); err != nil {
			return errf("service_image_digest_unavailable", "services."+service, "ob service status --output json", "%v", err)
		}
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

func (resolved *Resolved) selectServiceImage(serviceName, tagImage string) (ServiceImageSelection, error) {
	state, observed := resolved.serviceRuntime[serviceName]
	if !observed || state.ProtectionState == "never-enabled" || state.ProtectionState == "disabled" {
		return ServiceImageSelection{Image: tagImage, Origin: OriginAuthored}, nil
	}
	if state.ServiceImage == "" {
		return ServiceImageSelection{}, errf("protection_image_revert_unsafe", "services."+serviceName+".version", "ob protection disable --output ndjson", "protected runtime state has no immutable service image; refusing tag reversion")
	}
	if !state.PublicationVerified {
		return ServiceImageSelection{}, errf("protection_service_image_unpublished", "services."+serviceName+".version", "ob service status --output json", "protected service image has no verified publication provenance")
	}
	if !state.DigestAvailable && !state.CacheVerified {
		return ServiceImageSelection{}, errf("service_image_digest_unavailable", "services."+serviceName+".version", "ob service status --output json", "protected service image is unavailable from the registry and exact local cache")
	}
	selected := state.ServiceImage
	retained := append([]string{state.ServiceImage}, state.ManifestRootImages...)
	if candidate := state.RefreshCandidate; candidate != nil {
		if state.ProtectionState == "disable-pending" {
			return ServiceImageSelection{}, errf("service_image_patch_disable_pending", "services."+serviceName+".version", "ob protection disable --output ndjson", "protected service image refresh is refused while disablement is pending")
		}
		if !candidate.PublicationVerified {
			return ServiceImageSelection{}, errf("protection_service_image_unpublished", "services."+serviceName+".version", "ob service status --output json", "candidate protected service image has no verified publication provenance")
		}
		if !candidate.DigestAvailable && !candidate.CacheVerified {
			return ServiceImageSelection{}, errf("service_image_digest_unavailable", "services."+serviceName+".version", "ob service status --output json", "candidate protected service image is unavailable from the registry and exact local cache")
		}
		if !candidate.ExactTransition {
			return ServiceImageSelection{}, errf("protected_service_patch_unsupported", "services."+serviceName+".version", "ob service status --output json", "candidate image has no exact qualified protected transition")
		}
		selected = candidate.Image
		retained = append(retained, candidate.Image)
	}
	sort.Strings(retained)
	retained = compactStrings(retained)
	return ServiceImageSelection{Image: selected, RetainedImages: retained, Origin: OriginObserved}, nil
}

// ServiceImageForRuntime exposes the same selection used by generation so
// planners, cache checks, and pruning all retain identical immutable roots.
func (resolved *Resolved) ServiceImageForRuntime(serviceName string) (ServiceImageSelection, error) {
	service, ok := resolved.Services[serviceName]
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
	return resolved.selectServiceImage(serviceName, driver.image+":"+versionString(service.Version))
}

func validatePinnedServiceImage(image string) error {
	if err := imageref.Validate(image); err != nil {
		return err
	}
	marker := strings.LastIndex(image, "@sha256:")
	if marker < 1 || len(image)-marker != len("@sha256:")+64 || !lifecycleDigest.MatchString(image[marker+1:]) {
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
