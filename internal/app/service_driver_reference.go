package app

// ServiceDriverDocumentation is the intentionally public documentation view of
// one private service driver. It omits commands, environment variables, raw
// health probes, settings machinery, and lifecycle flags: publishing those
// implementation details would turn them into compatibility promises.
type ServiceDriverDocumentation struct {
	Name            string
	ImageRepository string
	Port            int
	DataPath        string
	URLScheme       string
	HealthAvailable bool
	ConnectionParts []string
}

// ServiceDriverReference returns a sorted, defensive projection of the driver
// catalogue for documentation generation. Every call owns its slice and every
// nested ConnectionParts slice, so a generator or test cannot mutate runtime
// driver state or affect a later caller.
func ServiceDriverReference() []ServiceDriverDocumentation {
	names := DriverNames()
	out := make([]ServiceDriverDocumentation, 0, len(names))
	for _, name := range names {
		d := drivers[name]
		out = append(out, ServiceDriverDocumentation{
			Name:            name,
			ImageRepository: d.image,
			Port:            d.port,
			DataPath:        d.dataPath,
			URLScheme:       d.scheme,
			HealthAvailable: len(d.health) > 0,
			ConnectionParts: documentedConnectionParts(d),
		})
	}
	return out
}

func documentedConnectionParts(d driver) []string {
	client := ClientEnv{User: d.urlUser, Database: databaseOf(d, "application")}
	present := map[string]bool{}
	for _, part := range client.canonicalNames() {
		present[part] = true
	}
	ordered := []string{"url", "host", "port", "user", "password", "database"}
	parts := make([]string, 0, len(present))
	for _, part := range ordered {
		if present[part] {
			parts = append(parts, part)
		}
	}
	return parts
}
