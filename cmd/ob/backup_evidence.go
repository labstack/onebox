package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/labstack/onebox/internal/journal"
	"github.com/labstack/onebox/internal/onebox"
)

const backupFactsSchemaVersion = "onebox.run/migration-backup-facts/v1alpha1"

// backupFactsManifest contains attestations and identifiers only. Deliberately
// absent are artifact bytes, key values, commands, and provider credentials.
type backupFactsManifest struct {
	SchemaVersion string                                      `json:"schema_version"`
	Resources     []onebox.MigrationBackupResourceEvidence    `json:"resources"`
	KeyMaterial   []onebox.MigrationBackupKeyMaterialEvidence `json:"key_material,omitempty"`
}

func addBackupEvidenceCommand(root *cobra.Command, g *globalFlags) {
	group := &cobra.Command{
		Use:   "backup-evidence",
		Short: "create plan-bound attestations about externally validated backups",
		Long:  "Seal externally validated facts about a backup into a receipt bound to one\nplan.\n\nFor environments whose policy requires backup evidence before a migration.\nThe receipt records artifact, integrity, restore-test and key-usability\nfacts — never backup bytes and never secrets. Onebox does not take the\nbackup; it records that something else did, and refuses to be the thing that\nclaims one exists.",
		Args:  cobra.NoArgs,
		RunE:  showCommandHelp,
	}
	var planPath, manifestPath, outPath string
	create := &cobra.Command{
		Use:   "create",
		Short: "seal validation facts into a migration backup evidence receipt",
		Long:  "Seal a facts manifest into a receipt bound to one exact plan.\n\nThe manifest records artifact, integrity, restore-test and key-usability\nfacts about a backup something else took — never backup bytes and never\nsecrets. Onebox does not take the backup and will not claim one exists.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runBackupEvidenceCreate(cmd, g, planPath, manifestPath, outPath)
		},
	}
	create.Flags().StringVar(&planPath, "plan", "", "executable plan containing the backup requirement")
	create.Flags().StringVar(&manifestPath, "manifest", "", "JSON facts manifest from external backup validation")
	create.Flags().StringVarP(&outPath, "out", "o", "ob-backup-evidence.json", "receipt output path")
	_ = create.MarkFlagRequired("plan")
	_ = create.MarkFlagRequired("manifest")
	group.AddCommand(create)
	root.AddCommand(group)
}

func runBackupEvidenceCreate(cmd *cobra.Command, g *globalFlags, planPath, manifestPath, outPath string) error {
	plan, err := onebox.LoadDeployPlan(planPath)
	if err != nil {
		return writeStructuredCommandFailure(cmd, g, "backup_evidence_failed", "backup evidence creation failed; inspect stderr for local diagnostics", err)
	}
	manifest, err := loadBackupFactsManifest(manifestPath)
	if err != nil {
		return writeStructuredCommandFailure(cmd, g, "backup_evidence_failed", "backup evidence creation failed; inspect stderr for local diagnostics", err)
	}
	receipt, err := onebox.NewBackupEvidenceReceipt(
		plan, journal.DefaultOperator(), time.Now(), manifest.Resources, manifest.KeyMaterial,
	)
	if err != nil {
		return writeStructuredCommandFailure(cmd, g, "backup_evidence_failed", "backup evidence creation failed; inspect stderr for local diagnostics", err)
	}
	if err := receipt.Save(outPath); err != nil {
		return writeStructuredCommandFailure(cmd, g, "backup_evidence_failed", "backup evidence creation failed; inspect stderr for local diagnostics", err)
	}
	if isStructuredOutput(g) {
		return writeCLIJSON(cmd.OutOrStdout(), receipt, g.Output == "json")
	}
	fmt.Fprintf(cmd.OutOrStdout(), "backup evidence written to %s (%s)\n", outPath, receipt.EvidenceDigest)
	return nil
}

func loadBackupFactsManifest(path string) (backupFactsManifest, error) {
	file, err := os.Open(path)
	if err != nil {
		return backupFactsManifest{}, fmt.Errorf("open backup facts manifest: %w", err)
	}
	defer file.Close()
	const maxManifestBytes = 1 << 20
	encoded, err := io.ReadAll(io.LimitReader(file, maxManifestBytes+1))
	if err != nil {
		return backupFactsManifest{}, fmt.Errorf("read backup facts manifest: %w", err)
	}
	if len(encoded) > maxManifestBytes {
		return backupFactsManifest{}, fmt.Errorf("backup facts manifest exceeds %d bytes", maxManifestBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var manifest backupFactsManifest
	if err := decoder.Decode(&manifest); err != nil {
		return backupFactsManifest{}, fmt.Errorf("decode backup facts manifest: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return backupFactsManifest{}, errors.New("decode backup facts manifest: multiple JSON values")
		}
		return backupFactsManifest{}, fmt.Errorf("decode backup facts manifest: %w", err)
	}
	if manifest.SchemaVersion != backupFactsSchemaVersion {
		return backupFactsManifest{}, fmt.Errorf("unsupported backup facts schema %q; want %s", manifest.SchemaVersion, backupFactsSchemaVersion)
	}
	return manifest, nil
}
