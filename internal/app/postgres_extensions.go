package app

import (
	"fmt"
	"sort"
	"strings"
)

// Extensions that need a server library loaded before CREATE EXTENSION. The
// image contains many ordinary extensions, which require no catalogue entry;
// this table exists only for behavior PostgreSQL cannot discover before start.
var postgresExtensionPreloads = map[string]string{
	"pg_cron":            "pg_cron",
	"pgaudit":            "pgaudit",
	"pg_stat_statements": "pg_stat_statements",
}

// Extension dependencies are part of the selected image contract. Users ask
// for the capability they need; Onebox establishes its exact PostgreSQL
// prerequisites without requiring duplicate declarations in the project file.
var postgresExtensionDependencies = map[string][]string{
	"vectorscale": {"vector"},
}

func postgresFeatureExtensions(service Service) []string {
	roots := make([]string, 0, len(service.Features.Extensions))
	for extension := range service.Features.Extensions {
		roots = append(roots, extension)
	}
	sort.Strings(roots)

	seen := make(map[string]bool, len(roots)+1)
	out := make([]string, 0, len(roots)+1)
	var add func(string)
	add = func(extension string) {
		if seen[extension] {
			return
		}
		seen[extension] = true
		dependencies := append([]string(nil), postgresExtensionDependencies[extension]...)
		sort.Strings(dependencies)
		for _, dependency := range dependencies {
			add(dependency)
		}
		out = append(out, extension)
	}
	for _, extension := range roots {
		add(extension)
	}
	return out
}

// PostgresPreloadExtensions lists declarations whose installed database object
// cannot safely outlive its feature declaration. Engine preflight uses this to
// refuse a restart that would silently unload an installed extension.
func PostgresPreloadExtensions() []string {
	out := make([]string, 0, len(postgresExtensionPreloads))
	for extension := range postgresExtensionPreloads {
		out = append(out, extension)
	}
	sort.Strings(out)
	return out
}

func postgresFeatureSettings(appName string, service Service) map[string]any {
	effective := make(map[string]any, len(service.Settings)+3)
	for key, value := range service.Settings {
		effective[key] = value
	}
	if service.Features == nil {
		return effective
	}

	preloads := splitPostgresList(fmt.Sprint(effective["shared_preload_libraries"]))
	for extension := range service.Features.Extensions {
		if library := postgresExtensionPreloads[extension]; library != "" {
			preloads[library] = true
		}
	}
	if len(preloads) > 0 {
		libraries := make([]string, 0, len(preloads))
		for library := range preloads {
			libraries = append(libraries, library)
		}
		sort.Strings(libraries)
		effective["shared_preload_libraries"] = strings.Join(libraries, ",")
	}

	// pg_cron otherwise looks for its metadata in the default `postgres`
	// database and opens new password-authenticated local connections. A managed
	// service has one application database, and background workers let scheduled
	// jobs run there without inventing a second credential path.
	if _, requested := service.Features.Extensions["pg_cron"]; requested {
		if _, authored := effective["cron.database_name"]; !authored {
			effective["cron.database_name"] = appName
		}
		if _, authored := effective["cron.use_background_workers"]; !authored {
			effective["cron.use_background_workers"] = "on"
		}
	}
	return effective
}

// PostgresServiceSettings returns the effective server settings for recovery
// and other runtimes that do not use the generated Compose command.
func (p *Spec) PostgresServiceSettings(name string) map[string]any {
	service, ok := p.Services[name]
	if !ok {
		return nil
	}
	return postgresFeatureSettings(p.Name, service)
}

func splitPostgresList(value string) map[string]bool {
	out := map[string]bool{}
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" && item != "<nil>" {
			out[item] = true
		}
	}
	return out
}
