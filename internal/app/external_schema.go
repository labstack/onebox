package app

import "strings"

func validateExternalService(external ExternalService, path string) error {
	if external.Driver == "" {
		return errf("project_invalid", path+".driver", "ob validate", "an external service must declare the connection driver it uses")
	}
	if _, ok := drivers[external.Driver]; !ok {
		return errf("unknown_service_driver", path+".driver", "ob validate", "no connection driver named %q; Onebox understands these shapes: %s", external.Driver, strings.Join(DriverNames(), ", "))
	}
	if err := gRepoPath.check(path+".connection.source.file", external.Connection.Source.File); err != nil {
		return err
	}
	if err := checkEnum(path+".connection.source.provider", external.Connection.Source.Provider, eSecretProvider); err != nil {
		return err
	}
	if len(external.Connection.Entries) == 0 {
		return errf("project_invalid", path+".connection.entries", "ob validate", "an external connection must map at least one trusted connection entry")
	}
	for _, part := range sortedKeys(external.Connection.Entries) {
		if err := checkEnum(path+".connection.entries."+part, part, eConnectionPart); err != nil {
			return err
		}
		if err := gEnvName.check(path+".connection.entries."+part, external.Connection.Entries[part]); err != nil {
			return err
		}
	}
	if _, hasURL := external.Connection.Entries["url"]; !hasURL {
		for _, part := range requiredExternalConnectionParts(external.Driver) {
			if external.Connection.Entries[part] == "" {
				return errf("project_invalid", path+".connection.entries", "ob validate", "external %s connection needs either url or the %s entry", external.Driver, part)
			}
		}
	}
	if err := gProtectionOwner.check(path+".protection_owner", external.ProtectionOwner); err != nil {
		return err
	}
	if external.Probe != nil {
		if err := checkEnum(path+".probe.kind", external.Probe.Kind, eExternalProbeKind); err != nil {
			return err
		}
		if _, err := positiveDuration(external.Probe.Timeout); err != nil {
			return errf("project_invalid", path+".probe.timeout", "ob validate", "probe timeout must be a positive duration: %v", err)
		}
		if _, err := positiveDuration(external.Probe.MaximumAge); err != nil {
			return errf("project_invalid", path+".probe.max_age", "ob validate", "probe max_age must be a positive duration: %v", err)
		}
	}
	return nil
}

func requiredExternalConnectionParts(driver string) []string {
	switch driver {
	case "postgres", "mysql", "mariadb", "mongodb", "clickhouse":
		return []string{"host", "port", "user", "password", "database"}
	case "redis", "valkey", "rabbitmq", "minio", "nats":
		return []string{"host", "port", "user", "password"}
	case "meilisearch":
		return []string{"host", "port", "password"}
	default:
		return nil
	}
}
