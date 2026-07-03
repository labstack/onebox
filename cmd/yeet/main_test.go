package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRootHelpListsVerbs(t *testing.T) {
	cmd := newRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "yeet") {
		t.Fatalf("help output missing binary name: %s", out.String())
	}
}
