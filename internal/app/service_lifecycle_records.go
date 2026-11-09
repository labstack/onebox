package app

// The catalogue. policyQualified says the project schema may accept a backup
// policy for this driver at all, and it means one thing: `ob backup enable` can
// actually establish it. Today that is postgres alone.
//
// A driver earns this flag when its contract runs, not when its contract is
// described.
var lifecycleCapabilities = map[string]lifecycleCapability{
	"postgres": {
		driver: "postgres", policyQualified: true,
		recoveryKinds:     map[string]bool{"pitr": true},
		supportedVersions: []string{`^1[78]([.][0-9]+)*$`},
		credentialSlots:   []string{"POSTGRES_PASSWORD", WalgRepositoryKeyEntry},
	},
	"redis": {
		driver:            "redis",
		recoveryKinds:     map[string]bool{"snapshot": true},
		supportedVersions: []string{`^8([.][0-9]+)*$`},
		credentialSlots:   []string{"REDIS_PASSWORD", "RESTIC_PASSWORD"},
	},
	"valkey": {
		driver:            "valkey",
		recoveryKinds:     map[string]bool{"snapshot": true},
		supportedVersions: []string{`^8([.][0-9]+)*$`},
		credentialSlots:   []string{"REDIS_PASSWORD", "RESTIC_PASSWORD"},
	},
}
