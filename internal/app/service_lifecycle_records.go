package app

import "strings"

var lifecycleCapabilities = buildLifecycleCapabilities()

func buildLifecycleCapabilities() map[string]lifecycleCapability {
	return map[string]lifecycleCapability{
		"postgres": lifecycleRecord(
			"postgres", true, "pitr", deliveryDerivedImage, repositoryNativeDirect, "archive-password", "pgbackrest", "5m",
			"^17([.][0-9]+)*$", artifact("ghcr.io/labstack/onebox-postgres-pgbackrest", "postgres", '1'), nil,
			[]lifecyclePrecondition{{Code: "archive-mode", Consistency: "physical-base-wal", Topology: "single-primary", RestartRequired: true}},
			[]string{"POSTGRES_PASSWORD", "OB_REPOSITORY_PASSPHRASE"}, []string{"data-volume", "wal-stream"},
			lifecycleOperations{Backup: "pgbackrest-backup", Restore: "pgbackrest-restore", Verify: "pgbackrest-check"}),
		"mysql": lifecycleRecord(
			"mysql", false, "pitr", deliveryExternalHelper, repositoryArtifact, "client-side", "artifact", "5m",
			"^8[.](0|4)([.][0-9]+)*$", artifact("mysql", "mysql", '2'), helperArtifact("percona/percona-xtrabackup", "xtrabackup", '3'),
			[]lifecyclePrecondition{{Code: "binary-log", Consistency: "physical-base-binlog", Topology: "single-primary"}},
			[]string{"MYSQL_PASSWORD", "MYSQL_ROOT_PASSWORD", "RESTIC_PASSWORD"}, []string{"data-volume", "binary-log"},
			lifecycleOperations{Backup: "xtrabackup-create", Restore: "xtrabackup-restore", Verify: "mysql-verify"}),
		"mariadb": lifecycleRecord(
			"mariadb", false, "pitr", deliveryExternalHelper, repositoryArtifact, "client-side", "artifact", "5m",
			"^11[.][0-9]+([.][0-9]+)*$", artifact("mariadb", "mariadb", '4'), helperArtifact("mariadb", "mariadb-backup", '5'),
			[]lifecyclePrecondition{{Code: "binary-log", Consistency: "physical-base-binlog", Topology: "single-primary", RestartRequired: true}},
			[]string{"MARIADB_PASSWORD", "MARIADB_ROOT_PASSWORD", "RESTIC_PASSWORD"}, []string{"data-volume", "binary-log"},
			lifecycleOperations{Backup: "mariadb-backup", Restore: "mariadb-restore", Verify: "mariadb-verify"}),
		"mongodb": lifecycleRecord(
			"mongodb", false, "pitr", deliveryExternalHelper, repositoryNativeDirect, "server-side-sse", "pbm", "5m",
			"^8[.]0([.][0-9]+)*$", artifact("mongo", "mongodb", '6'), helperArtifact("percona/percona-backup-mongodb", "pbm", '7'),
			[]lifecyclePrecondition{{Code: "replica-set", Consistency: "pbm-oplog", Topology: "single-node-replica-set"}},
			[]string{"MONGO_INITDB_ROOT_PASSWORD", "PBM_STORAGE_CREDENTIAL"}, []string{"data-volume", "oplog", "replica-set-identity"},
			lifecycleOperations{Backup: "pbm-backup", Restore: "pbm-restore", Verify: "mongodb-verify"}),
		"clickhouse": lifecycleRecord(
			"clickhouse", false, "snapshot", deliveryUpstreamDigest, repositoryNativeDirect, "server-side-sse", "clickhouse-chain", "30m",
			"^25([.][0-9]+)*$", artifact("clickhouse/clickhouse-server", "clickhouse", '8'), nil,
			[]lifecyclePrecondition{{Code: "named-collection", Consistency: "native-backup", Topology: "single-server", RestartRequired: true}},
			[]string{"CLICKHOUSE_PASSWORD", "CLICKHOUSE_BACKUP_CREDENTIAL"}, []string{"data-volume", "named-collection"},
			lifecycleOperations{Backup: "clickhouse-backup", Restore: "clickhouse-restore", Verify: "clickhouse-verify"}),
		"redis": lifecycleRecord(
			"redis", false, "snapshot", deliveryExternalHelper, repositoryArtifact, "client-side", "snapshot", "1h",
			"^8([.][0-9]+)*$", artifact("redis", "redis", '9'), helperArtifact("restic/restic", "restic", 'a'),
			[]lifecyclePrecondition{{Code: "persistence-mode", Consistency: "sealed-set-or-rdb", Topology: "single-server"}},
			[]string{"REDIS_PASSWORD", "RESTIC_PASSWORD"}, []string{"data-volume", "rdb-or-sealed-set"},
			lifecycleOperations{Backup: "redis-snapshot", Restore: "redis-restore", Verify: "redis-verify"}),
		"valkey": lifecycleRecord(
			"valkey", false, "snapshot", deliveryExternalHelper, repositoryArtifact, "client-side", "snapshot", "1h",
			"^8([.][0-9]+)*$", artifact("valkey/valkey", "valkey", 'b'), helperArtifact("restic/restic", "restic", 'c'),
			[]lifecyclePrecondition{{Code: "persistence-mode", Consistency: "immutable-rdb", Topology: "single-server"}},
			[]string{"REDIS_PASSWORD", "RESTIC_PASSWORD"}, []string{"data-volume", "rdb"},
			lifecycleOperations{Backup: "valkey-snapshot", Restore: "valkey-restore", Verify: "valkey-verify"}),
		"rabbitmq": lifecycleRecord(
			"rabbitmq", false, "cold", deliveryExternalHelper, repositoryArtifact, "client-side", "artifact", "24h",
			"^4([.][0-9]+)*$", artifact("rabbitmq", "rabbitmq", 'd'), helperArtifact("restic/restic", "restic", 'e'),
			[]lifecyclePrecondition{{Code: "stopped-node", Consistency: "cold-node-store", Topology: "single-node"}},
			[]string{"RABBITMQ_DEFAULT_PASS", "RABBITMQ_ERLANG_COOKIE", "RESTIC_PASSWORD"}, []string{"data-volume", "node-name", "erlang-cookie"},
			lifecycleOperations{Backup: "rabbitmq-cold", Restore: "rabbitmq-restore", Verify: "rabbitmq-verify"}),
		"minio": lifecycleRecord(
			"minio", true, "cold", deliveryExternalHelper, repositoryArtifact, "client-side", "artifact", "24h",
			"^RELEASE[.][0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9-]+Z$", artifact("minio/minio", "minio", 'f'), helperArtifact("restic/restic", "restic", '0'),
			[]lifecyclePrecondition{{Code: "stopped-service", Consistency: "cold-data-store", Topology: "single-node"}},
			[]string{"MINIO_ROOT_PASSWORD", "RESTIC_PASSWORD"}, []string{"data-volume", "minio-configuration"},
			lifecycleOperations{Backup: "minio-cold", Restore: "minio-restore", Verify: "minio-verify"}),
		"meilisearch": lifecycleRecord(
			"meilisearch", false, "snapshot", deliveryExternalHelper, repositoryArtifact, "client-side", "snapshot", "24h",
			"^1([.][0-9]+)*$", artifact("getmeili/meilisearch", "meilisearch", '0'), helperArtifact("restic/restic", "restic", '1'),
			[]lifecyclePrecondition{{Code: "snapshot-task", Consistency: "native-snapshot", Topology: "single-node"}},
			[]string{"MEILI_MASTER_KEY", "RESTIC_PASSWORD"}, []string{"data-volume", "snapshot"},
			lifecycleOperations{Backup: "meilisearch-snapshot", Restore: "meilisearch-restore", Verify: "meilisearch-verify"}),
		"nats": lifecycleRecord(
			"nats", false, "snapshot", deliveryExternalHelper, repositoryArtifact, "client-side", "artifact", "1h",
			"^2([.][0-9]+)*$", artifact("nats", "nats", '2'), helperArtifact("natsio/nats-box", "nats-cli", '3'),
			[]lifecyclePrecondition{{Code: "file-streams", Consistency: "account-snapshot", Topology: "single-server"}},
			[]string{"NATS_ACCOUNT_CREDENTIAL", "RESTIC_PASSWORD"}, []string{"data-volume", "streams", "consumers"},
			lifecycleOperations{Backup: "nats-account-backup", Restore: "nats-account-restore", Verify: "nats-verify"}),
	}
}

func lifecycleRecord(
	driver string,
	policyQualified bool,
	recoveryKind string,
	delivery serviceDeliveryClass,
	repository repositoryOwnership,
	encryption string,
	retention string,
	rpo string,
	versionPattern string,
	service lifecycleArtifactProvenance,
	helper *lifecycleArtifactProvenance,
	preconditions []lifecyclePrecondition,
	credentials []string,
	resources []string,
	operations lifecycleOperations,
) lifecycleCapabilityRecord {
	return lifecycleCapabilityRecord{
		driver: driver, policyQualified: policyQualified, graduated: false,
		recoveryKinds: map[string]bool{recoveryKind: true}, delivery: delivery,
		serviceArtifact: service, helperArtifact: helper,
		supportedVersions: []lifecycleVersionRange{{Pattern: versionPattern}},
		patchTransitions:  []protectedPatchTransition{}, repository: repository,
		encryptionByKind: map[string]string{recoveryKind: encryption}, retentionMapping: retention,
		preconditions: preconditions, achievableRPO: rpo,
		credentialSlots: credentials, protectedResources: resources, operations: operations,
		graduationEvidence: []string{"runtime-health", "recoverable-point", "retention-current", "restore-proof"},
	}
}

func artifact(repository, name string, seed byte) lifecycleArtifactProvenance {
	digest := seededDigest(seed)
	return lifecycleArtifactProvenance{
		Repository: repository, Digest: digest, UpstreamDigest: digest,
		SBOMDigest: seededDigest(nextHex(seed)), ProvenanceID: "onebox/catalog/" + name + "/v1",
	}
}

func helperArtifact(repository, name string, seed byte) *lifecycleArtifactProvenance {
	value := artifact(repository, name, seed)
	return &value
}

func seededDigest(seed byte) string {
	if !strings.ContainsRune("0123456789abcdef", rune(seed)) {
		seed = '0'
	}
	return "sha256:" + strings.Repeat(string(seed), 64)
}

func nextHex(seed byte) byte {
	const digits = "0123456789abcdef"
	index := strings.IndexByte(digits, seed)
	if index < 0 {
		return '0'
	}
	return digits[(index+1)%len(digits)]
}
