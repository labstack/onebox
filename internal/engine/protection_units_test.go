package engine

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/app"
)

type memoryProtectionSystemd struct {
	units   map[string]ObservedProtectionUnit
	writes  int
	enables int
	removes int
	reloads int
}

func (systemd *memoryProtectionSystemd) ListProtectionUnits(_ context.Context, prefix string) ([]ObservedProtectionUnit, error) {
	var units []ObservedProtectionUnit
	for name, unit := range systemd.units {
		if strings.HasPrefix(name, prefix) {
			units = append(units, unit)
		}
	}
	sort.Slice(units, func(i, j int) bool { return units[i].Name < units[j].Name })
	return units, nil
}

func (systemd *memoryProtectionSystemd) InstallProtectionUnit(_ context.Context, unit GeneratedProtectionUnit) error {
	if systemd.units == nil {
		systemd.units = map[string]ObservedProtectionUnit{}
	}
	current := systemd.units[unit.Name]
	systemd.units[unit.Name] = ObservedProtectionUnit{
		Name: unit.Name, Application: unit.Application, Environment: unit.Environment, Service: unit.Service,
		Mode: unit.Mode, Digest: unit.Digest, Enabled: current.Enabled, Active: current.Active,
	}
	systemd.writes++
	return nil
}

func (systemd *memoryProtectionSystemd) EnableProtectionTimer(_ context.Context, name string) error {
	unit := systemd.units[name]
	unit.Enabled, unit.Active = true, true
	systemd.units[name] = unit
	systemd.enables++
	return nil
}

func (systemd *memoryProtectionSystemd) DisableAndRemoveProtectionUnit(_ context.Context, name string) error {
	delete(systemd.units, name)
	systemd.removes++
	return nil
}

func (systemd *memoryProtectionSystemd) ReloadProtectionUnits(context.Context) error {
	systemd.reloads++
	return nil
}

func (systemd *memoryProtectionSystemd) reboot() {
	for name, unit := range systemd.units {
		unit.Active = unit.Enabled
		systemd.units[name] = unit
	}
}

func protectionUnitInput() ProtectionUnitInput {
	names := app.Names{App: "example", BasePath: "/var/lib/onebox"}
	runner := names.ProtectionRunnerPath("sha256:" + strings.Repeat("a", 64))
	schedule := app.Schedule{Cron: "17 */6 * * *", Timezone: "UTC"}
	drill := app.Schedule{Cron: "23 4 * * 1,4", Timezone: "UTC"}
	makeSchedule := func(kind string, timing app.Schedule) ProtectionUnitSchedule {
		return ProtectionUnitSchedule{
			Kind: kind, Schedule: timing, Active: true, RunnerPath: runner,
			EnvelopePath: names.ProtectionEnvelopePath("database", kind),
		}
	}
	return ProtectionUnitInput{
		Names: names, Application: "example", Environment: "production", Service: "database", User: "onebox", Group: "onebox",
		Schedules: []ProtectionUnitSchedule{
			makeSchedule("backup-create", schedule), makeSchedule("backup-prune", schedule),
			makeSchedule("replay-archive", schedule), makeSchedule("restore-drill", drill),
		},
	}
}

func TestGenerateProtectionUnitsUsesExactScheduleAndRestrictedRunner(t *testing.T) {
	set, err := GenerateProtectionUnits(protectionUnitInput())
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Units) != 8 {
		t.Fatalf("generated units = %d, want 8", len(set.Units))
	}
	var service, timer string
	for _, unit := range set.Units {
		if unit.Mode != 0o644 || !strings.HasPrefix(unit.Name, "ob-example-production-database-") {
			t.Fatalf("unit metadata = %#v", unit)
		}
		if unit.Kind == "backup-create" && strings.HasSuffix(unit.Name, ".service") {
			service = string(unit.Content)
		}
		if unit.Kind == "backup-create" && strings.HasSuffix(unit.Name, ".timer") {
			timer = string(unit.Content)
		}
	}
	for _, required := range []string{
		"User=onebox", "Group=onebox", "NoNewPrivileges=true", "ProtectSystem=strict",
		`ReadWritePaths=-"/var/lib/onebox/example/journal" -"/var/lib/onebox/example/protection"`,
		`ob-scheduled-runner" run "`, "X-Onebox-Environment=production",
	} {
		if !strings.Contains(service, required) {
			t.Errorf("service unit missing %q:\n%s", required, service)
		}
	}
	if !strings.Contains(timer, "OnCalendar=*-*-* 00/6:17:00 UTC") || !strings.Contains(timer, "Persistent=true") {
		t.Fatalf("timer did not preserve exact cron semantics:\n%s", timer)
	}
}

func TestProtectionUnitConvergenceIsIdempotentAndRebootPersistent(t *testing.T) {
	desired, err := GenerateProtectionUnits(protectionUnitInput())
	if err != nil {
		t.Fatal(err)
	}
	systemd := &memoryProtectionSystemd{}
	first, err := ReconcileProtectionUnits(context.Background(), systemd, desired, "example", "production", "database")
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Installed) != 8 || len(first.Enabled) != 4 || systemd.reloads != 1 {
		t.Fatalf("first convergence = %#v reloads=%d", first, systemd.reloads)
	}
	writes, enables, reloads := systemd.writes, systemd.enables, systemd.reloads
	second, err := ReconcileProtectionUnits(context.Background(), systemd, desired, "example", "production", "database")
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Installed)+len(second.Enabled)+len(second.Removed) != 0 || systemd.writes != writes || systemd.enables != enables || systemd.reloads != reloads {
		t.Fatalf("idempotent convergence = %#v counters=%d/%d/%d", second, systemd.writes, systemd.enables, systemd.reloads)
	}
	systemd.reboot()
	for name, unit := range systemd.units {
		if strings.HasSuffix(name, ".timer") && (!unit.Enabled || !unit.Active) {
			t.Fatalf("timer did not persist across reboot: %#v", unit)
		}
	}
}

func TestDisablePendingKeepsBackupArchivePruneAndStopsDrill(t *testing.T) {
	input := protectionUnitInput()
	enabled, err := GenerateProtectionUnits(input)
	if err != nil {
		t.Fatal(err)
	}
	systemd := &memoryProtectionSystemd{}
	if _, err := ReconcileProtectionUnits(context.Background(), systemd, enabled, "example", "production", "database"); err != nil {
		t.Fatal(err)
	}
	for index := range input.Schedules {
		if input.Schedules[index].Kind == "restore-drill" {
			input.Schedules[index].Active = false
		}
	}
	pending, err := GenerateProtectionUnits(input)
	if err != nil {
		t.Fatal(err)
	}
	result, err := ReconcileProtectionUnits(context.Background(), systemd, pending, "example", "production", "database")
	if err != nil {
		t.Fatal(err)
	}
	wantRemoved := []string{
		"ob-example-production-database-restore-drill.service",
		"ob-example-production-database-restore-drill.timer",
	}
	if !reflect.DeepEqual(result.Removed, wantRemoved) {
		t.Fatalf("pending removal = %#v, want %#v", result.Removed, wantRemoved)
	}
	for _, kind := range []string{"backup-create", "backup-prune", "replay-archive"} {
		if _, ok := systemd.units["ob-example-production-database-"+kind+".timer"]; !ok {
			t.Fatalf("pending disablement removed required %s timer", kind)
		}
	}
}

func TestProtectionUnitInspectionPreservesForeignAndRemovalIsScoped(t *testing.T) {
	desired, err := GenerateProtectionUnits(protectionUnitInput())
	if err != nil {
		t.Fatal(err)
	}
	systemd := &memoryProtectionSystemd{units: map[string]ObservedProtectionUnit{
		"ob-example-production-database-foreign.timer": {
			Name: "ob-example-production-database-foreign.timer", Application: "other", Environment: "production", Service: "database", Enabled: true, Active: true,
		},
	}}
	result, err := ReconcileProtectionUnits(context.Background(), systemd, desired, "example", "production", "database")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result.Preserved, []string{"ob-example-production-database-foreign.timer"}) {
		t.Fatalf("foreign units = %#v", result.Preserved)
	}
	empty := ProtectionUnitSet{Prefix: desired.Prefix}
	if _, err := ReconcileProtectionUnits(context.Background(), systemd, empty, "example", "production", "database"); err != nil {
		t.Fatal(err)
	}
	if _, ok := systemd.units["ob-example-production-database-foreign.timer"]; !ok {
		t.Fatal("scoped protection removal deleted a foreign unit")
	}
}

func TestProtectionUnitConvergenceRefusesExactForeignCollisionBeforeMutation(t *testing.T) {
	desired, err := GenerateProtectionUnits(protectionUnitInput())
	if err != nil {
		t.Fatal(err)
	}
	collision := desired.Units[0].Name
	systemd := &memoryProtectionSystemd{units: map[string]ObservedProtectionUnit{
		collision: {Name: collision, Application: "other", Environment: "production", Service: "database"},
	}}
	result, err := ReconcileProtectionUnits(context.Background(), systemd, desired, "example", "production", "database")
	if err == nil || !strings.Contains(err.Error(), "foreign-owned") {
		t.Fatalf("collision error = %v", err)
	}
	if !reflect.DeepEqual(result.Preserved, []string{collision}) {
		t.Fatalf("preserved = %#v", result.Preserved)
	}
	if systemd.writes != 0 || systemd.enables != 0 || systemd.removes != 0 || systemd.reloads != 0 {
		t.Fatalf("collision mutated systemd: %#v", systemd)
	}
}

func TestGenerateHostHousekeepingAndAssuranceUnits(t *testing.T) {
	input := protectionUnitInput()
	input.Service = "host"
	input.Schedules = nil
	for _, kind := range []string{"hygiene-run", "assurance-check"} {
		input.Schedules = append(input.Schedules, ProtectionUnitSchedule{
			Kind: kind, Active: true, Schedule: app.Schedule{Cron: "0 3 * * *", Timezone: "UTC"},
			RunnerPath:   input.Names.ProtectionRunnerPath("sha256:" + strings.Repeat("a", 64)),
			EnvelopePath: input.Names.ProtectionEnvelopePath("host", kind),
		})
	}
	set, err := GenerateProtectionUnits(input)
	if err != nil || len(set.Units) != 4 {
		t.Fatalf("host units = %d, %v", len(set.Units), err)
	}
}
