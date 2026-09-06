// Package durable contains the on-demand host checkpoint helper. systemd and
// the existing schedule runner still own process lifetime and deployment locks.
package durable

import (
	_ "embed"
	"path"
)

// Script uses only the Python 3 standard library; it is installed only when a
// durable job is declared. Version 1 readers remain compatible with v1 records.
//
//go:embed runner.py
var Script string

func Helper(root string) string { return path.Join(root, "schedule", "execution-v1.py") }
func Store(root string) string  { return path.Join(root, "schedule", "executions") }
