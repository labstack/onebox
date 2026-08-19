package onebox

import (
	"errors"
	"sort"

	"github.com/labstack/onebox/internal/app"
)

// ProtectionSecretSlots resolves every credential required by one protected
// service to named entries in one target-side file. It never decrypts or
// accepts values and is therefore safe to bind into plans and output.
func ProtectionSecretSlots(cfg *app.Resolved, serviceName string) ([]SecretSlotReference, error) {
	if cfg == nil || cfg.Spec == nil {
		return nil, errors.New("protection credential config is required")
	}
	service, ok := cfg.Services[serviceName]
	if !ok || service.Backup == nil {
		return nil, errors.New("protected service is required")
	}
	target, ok := cfg.BackupTargets[service.Backup.Target]
	if !ok {
		return nil, errors.New("protection target is not declared")
	}
	driverName := service.Driver
	if driverName == "" {
		driverName = serviceName
	}
	driverSlots, ok := app.LifecycleCredentialSlots(driverName, cfg.DeclaredVersion(serviceName))
	if !ok {
		return nil, errors.New("service has no qualified lifecycle credential contract")
	}
	entries := append(driverSlots,
		target.Credentials.AccessKeyEntry,
		target.Credentials.SecretKeyEntry,
	)
	if target.Credentials.SessionTokenEntry != "" {
		entries = append(entries, target.Credentials.SessionTokenEntry)
	}
	sort.Strings(entries)
	entries = uniqueNonEmpty(entries)
	file := cfg.NamesFor(cfg.Env).ProtectionCredentialFile(serviceName, service.Backup.Target)
	slots := make([]SecretSlotReference, 0, len(entries))
	for _, entry := range entries {
		slots = append(slots, SecretSlotReference{Slot: "credential:" + entry, File: file, Entry: entry})
	}
	return slots, nil
}

func uniqueNonEmpty(values []string) []string {
	out := values[:0]
	for _, value := range values {
		if value == "" || len(out) > 0 && out[len(out)-1] == value {
			continue
		}
		out = append(out, value)
	}
	return out
}
