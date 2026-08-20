package app

// The catalogue. policyQualified says the project schema may accept a backup
// policy for this driver at all; postgres and minio are the two whose contracts
// are executable today, and every other driver refuses a policy rather than
// accepting one it cannot honour.
var lifecycleCapabilities = map[string]lifecycleCapability{
	"postgres": {
		driver: "postgres", policyQualified: true,
		recoveryKinds:     map[string]bool{"pitr": true},
		supportedVersions: []string{`^1[78]([.][0-9]+)*$`},
		credentialSlots:   []string{"POSTGRES_PASSWORD", WalgRepositoryKeyEntry},
	},
	"mysql": {
		driver:            "mysql",
		recoveryKinds:     map[string]bool{"pitr": true},
		supportedVersions: []string{`^8[.](0|4)([.][0-9]+)*$`},
		credentialSlots:   []string{"MYSQL_PASSWORD", "MYSQL_ROOT_PASSWORD", "RESTIC_PASSWORD"},
	},
	"mariadb": {
		driver:            "mariadb",
		recoveryKinds:     map[string]bool{"pitr": true},
		supportedVersions: []string{`^11[.][0-9]+([.][0-9]+)*$`},
		credentialSlots:   []string{"MARIADB_PASSWORD", "MARIADB_ROOT_PASSWORD", "RESTIC_PASSWORD"},
	},
	"mongodb": {
		driver:            "mongodb",
		recoveryKinds:     map[string]bool{"pitr": true},
		supportedVersions: []string{`^8[.]0([.][0-9]+)*$`},
		credentialSlots:   []string{"MONGO_INITDB_ROOT_PASSWORD", "PBM_STORAGE_CREDENTIAL"},
	},
	"clickhouse": {
		driver:            "clickhouse",
		recoveryKinds:     map[string]bool{"snapshot": true},
		supportedVersions: []string{`^25([.][0-9]+)*$`},
		credentialSlots:   []string{"CLICKHOUSE_PASSWORD", "CLICKHOUSE_BACKUP_CREDENTIAL"},
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
	"rabbitmq": {
		driver:            "rabbitmq",
		recoveryKinds:     map[string]bool{"cold": true},
		supportedVersions: []string{`^4([.][0-9]+)*$`},
		credentialSlots:   []string{"RABBITMQ_DEFAULT_PASS", "RABBITMQ_ERLANG_COOKIE", "RESTIC_PASSWORD"},
	},
	"minio": {
		driver: "minio", policyQualified: true,
		recoveryKinds:     map[string]bool{"cold": true},
		supportedVersions: []string{`^RELEASE[.][0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9-]+Z$`},
		credentialSlots:   []string{"MINIO_ROOT_PASSWORD", "RESTIC_PASSWORD"},
	},
	"meilisearch": {
		driver:            "meilisearch",
		recoveryKinds:     map[string]bool{"snapshot": true},
		supportedVersions: []string{`^1([.][0-9]+)*$`},
		credentialSlots:   []string{"MEILI_MASTER_KEY", "RESTIC_PASSWORD"},
	},
	"nats": {
		driver:            "nats",
		recoveryKinds:     map[string]bool{"snapshot": true},
		supportedVersions: []string{`^2([.][0-9]+)*$`},
		credentialSlots:   []string{"NATS_ACCOUNT_CREDENTIAL", "RESTIC_PASSWORD"},
	},
}
