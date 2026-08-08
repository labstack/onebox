package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/labstack/onebox/internal/app"
)

type ProtectionUnitSchedule struct {
	Kind         string
	Schedule     app.Schedule
	Active       bool
	EnvelopePath string
	RunnerPath   string
}

type ProtectionUnitInput struct {
	Names       app.Names
	Application string
	Environment string
	Service     string
	User        string
	Group       string
	Schedules   []ProtectionUnitSchedule
}

type GeneratedProtectionUnit struct {
	Name        string
	Kind        string
	Application string
	Environment string
	Service     string
	Mode        uint32
	Digest      string
	Content     []byte
}

type ProtectionUnitSet struct {
	Prefix string
	Units  []GeneratedProtectionUnit
}

type ObservedProtectionUnit struct {
	Name        string
	Application string
	Environment string
	Service     string
	Mode        uint32
	Digest      string
	Enabled     bool
	Active      bool
}

type ProtectionUnitTarget interface {
	ListProtectionUnits(context.Context, string) ([]ObservedProtectionUnit, error)
	InstallProtectionUnit(context.Context, GeneratedProtectionUnit) error
	EnableProtectionTimer(context.Context, string) error
	DisableAndRemoveProtectionUnit(context.Context, string) error
	ReloadProtectionUnits(context.Context) error
}

type ProtectionUnitConvergence struct {
	Installed []string
	Enabled   []string
	Removed   []string
	Preserved []string
}

var protectionUnitKinds = map[string]bool{
	"backup-create": true, "backup-prune": true, "replay-archive": true,
	"restore-drill": true, "hygiene-run": true, "assurance-check": true,
}

func GenerateProtectionUnits(input ProtectionUnitInput) (ProtectionUnitSet, error) {
	if !unitMetadata.MatchString(input.Application) || !unitMetadata.MatchString(input.Environment) || !unitMetadata.MatchString(input.Service) ||
		!unitMetadata.MatchString(input.User) || !unitMetadata.MatchString(input.Group) || input.Names.App != input.Application {
		return ProtectionUnitSet{}, errors.New("protection unit ownership metadata is invalid")
	}
	prefix := "ob-" + input.Application + "-" + input.Environment + "-" + input.Service + "-"
	units := make([]GeneratedProtectionUnit, 0, len(input.Schedules)*2)
	seen := map[string]bool{}
	for _, schedule := range input.Schedules {
		if !protectionUnitKinds[schedule.Kind] {
			return ProtectionUnitSet{}, fmt.Errorf("unsupported protection unit kind %q", schedule.Kind)
		}
		if seen[schedule.Kind] {
			return ProtectionUnitSet{}, fmt.Errorf("duplicate protection unit kind %q", schedule.Kind)
		}
		seen[schedule.Kind] = true
		if !schedule.Active {
			continue
		}
		if schedule.Schedule.Cron == "" || schedule.Schedule.Timezone == "" ||
			strings.ContainsAny(schedule.Schedule.Cron+schedule.Schedule.Timezone, "\r\n\x00") ||
			!filepath.IsAbs(schedule.EnvelopePath) || filepath.Clean(schedule.EnvelopePath) != schedule.EnvelopePath ||
			!filepath.IsAbs(schedule.RunnerPath) || filepath.Clean(schedule.RunnerPath) != schedule.RunnerPath ||
			strings.ContainsAny(schedule.EnvelopePath+schedule.RunnerPath, "\r\n\x00 ") {
			return ProtectionUnitSet{}, fmt.Errorf("protection unit %q has invalid schedule or artifact paths", schedule.Kind)
		}
		calendar, err := app.CronToCalendar(schedule.Schedule.Cron)
		if err != nil {
			return ProtectionUnitSet{}, fmt.Errorf("protection unit %q schedule: %w", schedule.Kind, err)
		}
		timerName := input.Names.ProtectionTimerForEnvironment(input.Environment, input.Service, schedule.Kind)
		serviceName := strings.TrimSuffix(timerName, ".timer") + ".service"
		serviceBody := renderProtectionServiceUnit(input, schedule)
		timerBody := renderProtectionTimerUnit(input, schedule, calendar)
		units = append(units,
			newGeneratedProtectionUnit(serviceName, schedule.Kind, input, serviceBody),
			newGeneratedProtectionUnit(timerName, schedule.Kind, input, timerBody),
		)
	}
	sort.Slice(units, func(i, j int) bool { return units[i].Name < units[j].Name })
	return ProtectionUnitSet{Prefix: prefix, Units: units}, nil
}

func ReconcileProtectionUnits(ctx context.Context, target ProtectionUnitTarget, desired ProtectionUnitSet, application, environment, service string) (ProtectionUnitConvergence, error) {
	if target == nil {
		return ProtectionUnitConvergence{}, errors.New("protection unit target is nil")
	}
	if desired.Prefix != "ob-"+application+"-"+environment+"-"+service+"-" {
		return ProtectionUnitConvergence{}, errors.New("protection unit desired prefix does not match ownership")
	}
	observed, err := target.ListProtectionUnits(ctx, desired.Prefix)
	if err != nil {
		return ProtectionUnitConvergence{}, err
	}
	observedByName := make(map[string]ObservedProtectionUnit, len(observed))
	foreignByName := make(map[string]ObservedProtectionUnit)
	result := ProtectionUnitConvergence{}
	for _, unit := range observed {
		if unit.Application != application || unit.Environment != environment || unit.Service != service {
			result.Preserved = append(result.Preserved, unit.Name)
			foreignByName[unit.Name] = unit
			continue
		}
		observedByName[unit.Name] = unit
	}
	// Ownership is checked before the first mutation. A unit whose filename is
	// ours but whose embedded ownership metadata is not must never be replaced:
	// systemd filenames are host-global, and installing here would overwrite a
	// foreign operator's unit before convergence had a chance to report it.
	for _, unit := range desired.Units {
		if _, collision := foreignByName[unit.Name]; collision {
			sort.Strings(result.Preserved)
			return result, fmt.Errorf("protection unit %q collides with a foreign-owned unit", unit.Name)
		}
	}
	wanted := make(map[string]GeneratedProtectionUnit, len(desired.Units))
	changed := false
	for _, unit := range desired.Units {
		wanted[unit.Name] = unit
		current, exists := observedByName[unit.Name]
		if !exists || current.Digest != unit.Digest || current.Mode != unit.Mode {
			if err := target.InstallProtectionUnit(ctx, unit); err != nil {
				return result, err
			}
			result.Installed = append(result.Installed, unit.Name)
			changed = true
		}
	}
	for name := range observedByName {
		if _, keep := wanted[name]; keep {
			continue
		}
		if err := target.DisableAndRemoveProtectionUnit(ctx, name); err != nil {
			return result, err
		}
		result.Removed = append(result.Removed, name)
		changed = true
	}
	if changed {
		if err := target.ReloadProtectionUnits(ctx); err != nil {
			return result, err
		}
	}
	for _, unit := range desired.Units {
		if !strings.HasSuffix(unit.Name, ".timer") {
			continue
		}
		current, exists := observedByName[unit.Name]
		if !exists || !current.Enabled || !current.Active || changed {
			if err := target.EnableProtectionTimer(ctx, unit.Name); err != nil {
				return result, err
			}
			result.Enabled = append(result.Enabled, unit.Name)
		}
	}
	sort.Strings(result.Installed)
	sort.Strings(result.Enabled)
	sort.Strings(result.Removed)
	sort.Strings(result.Preserved)
	return result, nil
}

func InspectProtectionUnits(ctx context.Context, target ProtectionUnitTarget, prefix string) ([]ObservedProtectionUnit, error) {
	if target == nil || !unitName.MatchString(prefix) || !strings.HasPrefix(prefix, "ob-") {
		return nil, errors.New("protection unit inspection prefix is invalid")
	}
	units, err := target.ListProtectionUnits(ctx, prefix)
	if err != nil {
		return nil, err
	}
	sort.Slice(units, func(i, j int) bool { return units[i].Name < units[j].Name })
	return units, nil
}

func newGeneratedProtectionUnit(name, kind string, input ProtectionUnitInput, content []byte) GeneratedProtectionUnit {
	sum := sha256.Sum256(content)
	return GeneratedProtectionUnit{
		Name: name, Kind: kind, Application: input.Application, Environment: input.Environment, Service: input.Service,
		Mode: 0o644, Digest: "sha256:" + hex.EncodeToString(sum[:]), Content: content,
	}
}

func renderProtectionServiceUnit(input ProtectionUnitInput, schedule ProtectionUnitSchedule) []byte {
	writePaths := []string{
		"-" + systemdQuote(input.Names.AppDir()+"/journal"),
		"-" + systemdQuote(input.Names.AppDir()+"/protection"),
	}
	return []byte(strings.Join([]string{
		"[Unit]",
		"Description=Onebox " + schedule.Kind + " for " + input.Application + "/" + input.Environment + "/" + input.Service,
		"X-Onebox-Application=" + input.Application,
		"X-Onebox-Environment=" + input.Environment,
		"X-Onebox-Service=" + input.Service,
		"After=network-online.target",
		"Wants=network-online.target",
		"",
		"[Service]",
		"Type=oneshot",
		"User=" + input.User,
		"Group=" + input.Group,
		"NoNewPrivileges=true",
		"PrivateTmp=true",
		"ProtectSystem=strict",
		"ProtectHome=true",
		"ReadWritePaths=" + strings.Join(writePaths, " "),
		"ExecStart=" + systemdQuote(schedule.RunnerPath) + " run " + systemdQuote(schedule.EnvelopePath),
		"",
	}, "\n"))
}

func systemdQuote(value string) string {
	return `"` + strings.ReplaceAll(value, "%", "%%") + `"`
}

func renderProtectionTimerUnit(input ProtectionUnitInput, schedule ProtectionUnitSchedule, calendar string) []byte {
	return []byte(strings.Join([]string{
		"[Unit]",
		"Description=Onebox schedule for " + schedule.Kind + " (" + input.Application + "/" + input.Environment + "/" + input.Service + ")",
		"X-Onebox-Application=" + input.Application,
		"X-Onebox-Environment=" + input.Environment,
		"X-Onebox-Service=" + input.Service,
		"",
		"[Timer]",
		"OnCalendar=" + calendar + " " + schedule.Schedule.Timezone,
		"Persistent=true",
		"Unit=" + strings.TrimSuffix(input.Names.ProtectionTimerForEnvironment(input.Environment, input.Service, schedule.Kind), ".timer") + ".service",
		"",
		"[Install]",
		"WantedBy=timers.target",
		"",
	}, "\n"))
}

var unitMetadata = unitName
