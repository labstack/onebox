package onebox

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"

	"github.com/labstack/onebox/internal/app"
)

const ProtectedServiceIdentitySchemaVersion = "onebox.run/protected-service-identity/v1alpha1"

type ProtectedServiceIdentity struct {
	SchemaVersion    string   `json:"schema_version"`
	Application      string   `json:"application"`
	Environment      string   `json:"environment"`
	Service          string   `json:"service"`
	Driver           string   `json:"driver"`
	LogicalVolume    string   `json:"logical_volume"`
	ServiceProject   string   `json:"service_project"`
	ServiceContainer string   `json:"service_container"`
	RestoreProject   string   `json:"restore_project"`
	RestoreContainer string   `json:"restore_container"`
	RestoreNetwork   string   `json:"restore_network"`
	RestoreVolume    string   `json:"restore_volume"`
	StatePath        string   `json:"state_path"`
	Timers           []string `json:"timers"`
	ManifestBound    bool     `json:"manifest_bound"`
	IdentityDigest   string   `json:"identity_digest"`
}

func NewProtectedServiceIdentity(cfg *app.Resolved, serviceName string, manifestBound bool) (ProtectedServiceIdentity, error) {
	if cfg == nil || cfg.Spec == nil {
		return ProtectedServiceIdentity{}, errors.New("protected service identity requires a resolved project")
	}
	service, ok := cfg.Services[serviceName]
	if !ok {
		return ProtectedServiceIdentity{}, protectedIdentityFailure()
	}
	if service.Backup == nil && !manifestBound {
		return ProtectedServiceIdentity{}, errors.New("service identity is not yet protection-bound")
	}
	driver := service.Driver
	if driver == "" {
		driver = serviceName
	}
	names := cfg.NamesFor(cfg.Env)
	var timers []string
	// Bind the complete service schedule namespace, including replay archival
	// when the current policy does not use it. The identity must remain stable
	// after policy removal so it can still inspect and remove units created by
	// the last effective policy.
	for _, kind := range []string{"backup-create", "backup-prune", "replay-archive", "restore-drill"} {
		timers = append(timers, names.ProtectionTimerForEnvironment(cfg.Env, serviceName, kind))
	}
	sort.Strings(timers)
	record := ProtectedServiceIdentity{
		SchemaVersion: ProtectedServiceIdentitySchemaVersion,
		Application:   names.App, Environment: cfg.Env, Service: serviceName, Driver: driver, LogicalVolume: firstServiceVolume(service),
		ServiceProject: names.ServiceProject(serviceName), ServiceContainer: names.ServiceContainer(serviceName),
		RestoreProject: names.ProtectionRestoreProject(serviceName), RestoreContainer: names.ProtectionRestoreContainer(serviceName),
		RestoreNetwork: names.ProtectionRestoreNetwork(serviceName), RestoreVolume: names.ProtectionRestoreVolume(serviceName),
		StatePath: names.ActiveVolumeFile(serviceName), Timers: timers, ManifestBound: manifestBound,
	}
	if err := record.Seal(); err != nil {
		return ProtectedServiceIdentity{}, err
	}
	return record, nil
}

func (record ProtectedServiceIdentity) canonicalJSON() ([]byte, error) {
	copy := record
	copy.IdentityDigest = ""
	return json.Marshal(copy)
}

func (record ProtectedServiceIdentity) ComputeDigest() (string, error) {
	encoded, err := record.canonicalJSON()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func (record ProtectedServiceIdentity) validateContent() error {
	if record.SchemaVersion != ProtectedServiceIdentitySchemaVersion {
		return fmt.Errorf("unsupported protected service identity schema %q", record.SchemaVersion)
	}
	for name, value := range map[string]string{
		"application": record.Application, "environment": record.Environment, "service": record.Service,
		"driver": record.Driver, "logical_volume": record.LogicalVolume,
		"service_project": record.ServiceProject, "service_container": record.ServiceContainer,
		"restore_project": record.RestoreProject, "restore_container": record.RestoreContainer,
		"restore_network": record.RestoreNetwork, "restore_volume": record.RestoreVolume,
	} {
		if !safeLifecycleMetadata(value) {
			return fmt.Errorf("protected service identity %s is invalid", name)
		}
	}
	if record.StatePath == "" || record.StatePath[0] != '/' {
		return errors.New("protected service identity state path must be absolute")
	}
	if !sort.StringsAreSorted(record.Timers) {
		return errors.New("protected service identity timers must be sorted")
	}
	for index, timer := range record.Timers {
		if !safeLifecycleMetadata(timer) || index > 0 && timer == record.Timers[index-1] {
			return errors.New("protected service identity timer is invalid or repeated")
		}
	}
	return nil
}

func (record *ProtectedServiceIdentity) Seal() error {
	if record == nil {
		return errors.New("protected service identity is nil")
	}
	if err := record.validateContent(); err != nil {
		return err
	}
	digest, err := record.ComputeDigest()
	if err != nil {
		return err
	}
	record.IdentityDigest = digest
	return nil
}

func (record ProtectedServiceIdentity) Validate() error {
	if err := record.validateContent(); err != nil {
		return err
	}
	if !lifecycleGraphDigest.MatchString(record.IdentityDigest) {
		return errors.New("protected service identity digest is missing or invalid")
	}
	expected, err := record.ComputeDigest()
	if err != nil {
		return err
	}
	if record.IdentityDigest != expected {
		return errors.New("protected service identity digest mismatch")
	}
	return nil
}

func ValidateProtectedServiceIdentity(cfg *app.Resolved, record ProtectedServiceIdentity) error {
	if err := record.Validate(); err != nil {
		return err
	}
	current, err := NewProtectedServiceIdentity(cfg, record.Service, record.ManifestBound)
	if err != nil {
		return protectedIdentityFailure()
	}
	current.IdentityDigest = record.IdentityDigest
	if !reflect.DeepEqual(current, record) {
		return protectedIdentityFailure()
	}
	return nil
}

func firstServiceVolume(service app.Service) string {
	if len(service.Volumes) > 0 {
		return service.Volumes[0]
	}
	return "data"
}

func protectedIdentityFailure() error {
	failure, _ := NewLifecycleFailure("protected_service_identity_changed")
	return failure
}
