package scripts_test

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Both live outside this package, so they are read rather than embedded.
func readRepositoryFile(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// Where a publisher writes is the one thing about it neither `goreleaser check`
// nor the artifact verifier can judge: a snapshot renders the cask and the
// manifest into dist/ no matter which repository they are destined for, and the
// release token can write to both, so swapping the tap and the bucket publishes
// cleanly and wrongly. The workflow's own preflight list is the statement of
// intent, so this binds the two together rather than restating either.
func TestPublisherDestinationsMatchTheWorkflowPreflight(t *testing.T) {
	var config struct {
		HomebrewCasks []publisher `yaml:"homebrew_casks"`
		Scoops        []publisher `yaml:"scoops"`
	}
	if err := yaml.Unmarshal([]byte(readRepositoryFile(t, "../.goreleaser.yaml")), &config); err != nil {
		t.Fatal(err)
	}
	if len(config.HomebrewCasks) != 1 || len(config.Scoops) != 1 {
		t.Fatalf("want exactly one cask and one bucket, got %d and %d", len(config.HomebrewCasks), len(config.Scoops))
	}

	destinations := map[string]string{
		"homebrew_casks": config.HomebrewCasks[0].destination(t, "homebrew_casks"),
		"scoops":         config.Scoops[0].destination(t, "scoops"),
	}
	if destinations["homebrew_casks"] == destinations["scoops"] {
		t.Fatalf("both publishers write to %s", destinations["homebrew_casks"])
	}

	preflighted := preflightedRepositories(t)
	published := make([]string, 0, len(destinations))
	for _, destination := range destinations {
		published = append(published, destination)
	}
	sort.Strings(published)
	if strings.Join(published, " ") != strings.Join(preflighted, " ") {
		t.Fatalf("publishers write to %v, but the release workflow preflights %v — a repository whose\n"+
			"write access is never checked fails after the release is already public", published, preflighted)
	}
}

func TestPublishedPackageLicensesMatchRepository(t *testing.T) {
	repositoryLicense := readRepositoryFile(t, "../LICENSE")
	if !strings.Contains(repositoryLicense, "Apache License") || !strings.Contains(repositoryLicense, "Version 2.0") {
		t.Fatal("repository license is no longer Apache License 2.0; update the expected package identifier")
	}

	var config struct {
		NFPMS  []packageMetadata `yaml:"nfpms"`
		Scoops []packageMetadata `yaml:"scoops"`
	}
	if err := yaml.Unmarshal([]byte(readRepositoryFile(t, "../.goreleaser.yaml")), &config); err != nil {
		t.Fatal(err)
	}
	if len(config.NFPMS) != 1 || len(config.Scoops) != 1 {
		t.Fatalf("want exactly one nFPM package and one Scoop manifest, got %d and %d", len(config.NFPMS), len(config.Scoops))
	}

	const want = "Apache-2.0"
	for target, license := range map[string]string{
		"nfpms":  config.NFPMS[0].License,
		"scoops": config.Scoops[0].License,
	} {
		if license != want {
			t.Errorf("%s publishes license %q, want repository SPDX identifier %q", target, license, want)
		}
	}
}

type packageMetadata struct {
	License string `yaml:"license"`
}

type publisher struct {
	Repository struct {
		Owner  string `yaml:"owner"`
		Name   string `yaml:"name"`
		Branch string `yaml:"branch"`
		Token  string `yaml:"token"`
	} `yaml:"repository"`
}

func (p publisher) destination(t *testing.T, field string) string {
	t.Helper()
	if p.Repository.Branch != "main" {
		t.Fatalf("%s publishes to branch %q, which the preflight does not check", field, p.Repository.Branch)
	}
	if p.Repository.Token != "{{ .Env.PACKAGE_REPOS_TOKEN }}" {
		t.Fatalf("%s uses token %q rather than the credential the workflow preflights", field, p.Repository.Token)
	}
	if p.Repository.Owner == "" || p.Repository.Name == "" {
		t.Fatalf("%s has no repository destination", field)
	}
	return fmt.Sprintf("%s/%s", p.Repository.Owner, p.Repository.Name)
}

// preflightedRepositories reads the list the release job actually loops over,
// so the binding breaks when either side is edited alone.
func preflightedRepositories(t *testing.T) []string {
	t.Helper()
	workflow := readRepositoryFile(t, "../.github/workflows/release.yml")
	loop := regexp.MustCompile(`for repository in ([^;\n]+); do`).FindStringSubmatch(workflow)
	if loop == nil {
		t.Fatal("release workflow no longer preflights publication targets in a recognizable loop")
	}
	repositories := strings.Fields(loop[1])
	sort.Strings(repositories)
	return repositories
}
