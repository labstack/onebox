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
	var templatePlan string
	template := &cobra.Command{
		Use:   "template",
		Short: "print a facts manifest skeleton for a plan's resources",
		Long: "Print a manifest skeleton with the plan's resources already filled in.\n\n" +
			"The manifest is the only artifact here a person has to author — the plan,\n" +
			"the grant and the receipt are all produced by ob — so authoring it blind\n" +
			"meant discovering its shape one refusal at a time. Fill in the digests and\n" +
			"timestamps your backup tooling produced, then pass it to `create`.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			plan, err := onebox.LoadDeployPlan(templatePlan)
			if err != nil {
				return err
			}
			if plan.MigrationBackup == nil {
				return errors.New("this plan carries no migration backup requirement, so no manifest is needed")
			}
			manifest := backupFactsManifest{SchemaVersion: backupFactsSchemaVersion}
			for _, resource := range plan.MigrationBackup.Resources {
				manifest.Resources = append(manifest.Resources, onebox.MigrationBackupResourceEvidence{
					Resource: resource, BackupID: "REPLACE-with-your-backup-id", CreatedAt: "REPLACE-with-RFC3339-time",
					Integrity:   onebox.BackupIntegrityEvidence{ArtifactDigest: "sha256:REPLACE", Method: "sha256", ValidatedAt: "REPLACE-with-RFC3339-time"},
					RestoreTest: restoreTestSkeleton(plan.MigrationBackup.RequireRestoreTest),
				})
			}
			for _, name := range plan.MigrationBackup.RequiredKeyMaterial {
				manifest.KeyMaterial = append(manifest.KeyMaterial, onebox.MigrationBackupKeyMaterialEvidence{
					Name: name, BackupID: "REPLACE-with-your-backup-id", CreatedAt: "REPLACE-with-RFC3339-time",
					Integrity: onebox.BackupIntegrityEvidence{ArtifactDigest: "sha256:REPLACE", Method: "sha256", ValidatedAt: "REPLACE-with-RFC3339-time"},
					Usability: onebox.BackupKeyMaterialUsabilityEvidence{Method: "REPLACE-how-you-proved-the-key-opens-it", ValidatedAt: "REPLACE-with-RFC3339-time", ValidationDigest: "sha256:REPLACE"},
				})
			}
			encoder := json.NewEncoder(cmd.OutOrStdout())
			encoder.SetIndent("", "  ")
			return encoder.Encode(manifest)
		},
	}
	template.Flags().StringVar(&templatePlan, "plan", "", "executable plan containing the backup requirement")
	_ = template.MarkFlagRequired("plan")
	group.AddCommand(create)
	group.AddCommand(template)
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

// restoreTestSkeleton matches what the plan will accept. A policy requiring a
// passed restore test refuses a "not_tested" receipt, and `passed` needs method,
// tested_at and validation_digest — so emitting the wrong one reinstates the
// discover-by-refusal loop this command exists to end.
func restoreTestSkeleton(required bool) onebox.BackupRestoreTestEvidence {
	if !required {
		return onebox.BackupRestoreTestEvidence{State: "not_tested"}
	}
	return onebox.BackupRestoreTestEvidence{
		State: "passed", Method: "REPLACE-how-you-restored-and-checked-it",
		TestedAt: "REPLACE-with-RFC3339-time", ValidationDigest: "sha256:REPLACE",
	}
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
