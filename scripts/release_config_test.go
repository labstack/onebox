package scripts_test

import (
	"os"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type goreleaserConfig struct {
	Version     int    `yaml:"version"`
	ProjectName string `yaml:"project_name"`
	Builds      []struct {
		ID      string   `yaml:"id"`
		Main    string   `yaml:"main"`
		Binary  string   `yaml:"binary"`
		Env     []string `yaml:"env"`
		GOOS    []string `yaml:"goos"`
		GOARCH  []string `yaml:"goarch"`
		Flags   []string `yaml:"flags"`
		LDFlags []string `yaml:"ldflags"`
	} `yaml:"builds"`
	Archives []struct {
		ID              string   `yaml:"id"`
		IDs             []string `yaml:"ids"`
		NameTemplate    string   `yaml:"name_template"`
		Formats         []string `yaml:"formats"`
		FormatOverrides []struct {
			GOOS    string   `yaml:"goos"`
			Formats []string `yaml:"formats"`
		} `yaml:"format_overrides"`
	} `yaml:"archives"`
	NFPMS []struct {
		ID               string   `yaml:"id"`
		IDs              []string `yaml:"ids"`
		PackageName      string   `yaml:"package_name"`
		FileNameTemplate string   `yaml:"file_name_template"`
		Bindir           string   `yaml:"bindir"`
		Formats          []string `yaml:"formats"`
	} `yaml:"nfpms"`
	Checksum struct {
		NameTemplate string `yaml:"name_template"`
		Algorithm    string `yaml:"algorithm"`
	} `yaml:"checksum"`
	Notarize struct {
		MacOS []struct {
			Enabled string   `yaml:"enabled"`
			IDs     []string `yaml:"ids"`
			Sign    struct {
				Certificate string `yaml:"certificate"`
				Password    string `yaml:"password"`
			} `yaml:"sign"`
			Notary struct {
				IssuerID string `yaml:"issuer_id"`
				KeyID    string `yaml:"key_id"`
				Key      string `yaml:"key"`
				Wait     bool   `yaml:"wait"`
				Timeout  string `yaml:"timeout"`
			} `yaml:"notarize"`
		} `yaml:"macos"`
	} `yaml:"notarize"`
	HomebrewCasks []struct {
		Name      string   `yaml:"name"`
		IDs       []string `yaml:"ids"`
		Binaries  []string `yaml:"binaries"`
		Directory string   `yaml:"directory"`
		URL       struct {
			Template string `yaml:"template"`
			Verified string `yaml:"verified"`
		} `yaml:"url"`
		Repository releaseRepository `yaml:"repository"`
	} `yaml:"homebrew_casks"`
	Scoops []struct {
		Name        string            `yaml:"name"`
		IDs         []string          `yaml:"ids"`
		URLTemplate string            `yaml:"url_template"`
		Repository  releaseRepository `yaml:"repository"`
	} `yaml:"scoops"`
}

type releaseRepository struct {
	Owner  string `yaml:"owner"`
	Name   string `yaml:"name"`
	Branch string `yaml:"branch"`
	Token  string `yaml:"token"`
}

func TestGoReleaserArtifactContract(t *testing.T) {
	body, err := os.ReadFile("../.goreleaser.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var config goreleaserConfig
	if err := yaml.Unmarshal(body, &config); err != nil {
		t.Fatalf("parse .goreleaser.yaml: %v", err)
	}

	if config.Version != 2 || config.ProjectName != "onebox" {
		t.Fatalf("config identity = version %d, project %q", config.Version, config.ProjectName)
	}
	if len(config.Builds) != 1 {
		t.Fatalf("build count = %d, want 1", len(config.Builds))
	}
	build := config.Builds[0]
	if build.ID != "ob" || build.Main != "./cmd/ob" || build.Binary != "ob" {
		t.Fatalf("unexpected build identity: %+v", build)
	}
	assertStringSet(t, "build environment", build.Env, []string{"CGO_ENABLED=0"})
	assertStringSet(t, "operating systems", build.GOOS, []string{"linux", "darwin", "windows"})
	assertStringSet(t, "architectures", build.GOARCH, []string{"amd64", "arm64"})
	assertStringSet(t, "build flags", build.Flags, []string{"-trimpath"})
	ldflags := strings.Join(build.LDFlags, " ")
	for _, binding := range []string{
		"internal/buildinfo.release={{ .Tag }}",
		"internal/buildinfo.buildTime={{ .Date }}",
	} {
		if !strings.Contains(ldflags, binding) {
			t.Errorf("ldflags do not bind %q: %s", binding, ldflags)
		}
	}

	if len(config.Archives) != 1 {
		t.Fatalf("archive count = %d, want 1", len(config.Archives))
	}
	archive := config.Archives[0]
	if archive.ID != "ob" || !slices.Equal(archive.IDs, []string{"ob"}) || archive.NameTemplate != "onebox_{{ .Version }}_{{ .Os }}_{{ .Arch }}" {
		t.Fatalf("unexpected archive identity: %+v", archive)
	}
	assertStringSet(t, "archive formats", archive.Formats, []string{"tar.gz"})
	if len(archive.FormatOverrides) != 1 || archive.FormatOverrides[0].GOOS != "windows" || !slices.Equal(archive.FormatOverrides[0].Formats, []string{"zip"}) {
		t.Fatalf("Windows archive override = %+v, want zip", archive.FormatOverrides)
	}

	if len(config.NFPMS) != 1 {
		t.Fatalf("Linux package definition count = %d, want 1", len(config.NFPMS))
	}
	packaging := config.NFPMS[0]
	if packaging.ID != "ob" || !slices.Equal(packaging.IDs, []string{"ob"}) || packaging.PackageName != "onebox" || packaging.Bindir != "/usr/bin" {
		t.Fatalf("unexpected Linux package identity: %+v", packaging)
	}
	if packaging.FileNameTemplate != "onebox_{{ .Version }}_{{ .Os }}_{{ .Arch }}" {
		t.Fatalf("Linux package filename template = %q", packaging.FileNameTemplate)
	}
	assertStringSet(t, "Linux package formats", packaging.Formats, []string{"deb", "rpm"})

	if config.Checksum.NameTemplate != "onebox_{{ .Version }}_checksums.txt" || config.Checksum.Algorithm != "sha256" {
		t.Fatalf("unexpected checksum contract: %+v", config.Checksum)
	}

	if len(config.Notarize.MacOS) != 1 {
		t.Fatalf("macOS notarization definition count = %d, want 1", len(config.Notarize.MacOS))
	}
	notary := config.Notarize.MacOS[0]
	if notary.Enabled != `{{ isEnvSet "MACOS_SIGN_P12" }}` || !slices.Equal(notary.IDs, []string{"ob"}) {
		t.Fatalf("unexpected macOS notarization selector: %+v", notary)
	}
	if notary.Sign.Certificate != "{{ .Env.MACOS_SIGN_P12 }}" || notary.Sign.Password != "{{ .Env.MACOS_SIGN_PASSWORD }}" {
		t.Fatalf("unexpected macOS signing secret sources: %+v", notary.Sign)
	}
	if notary.Notary.IssuerID != "{{ .Env.MACOS_NOTARY_ISSUER_ID }}" || notary.Notary.KeyID != "{{ .Env.MACOS_NOTARY_KEY_ID }}" || notary.Notary.Key != "{{ .Env.MACOS_NOTARY_KEY }}" || !notary.Notary.Wait || notary.Notary.Timeout != "20m" {
		t.Fatalf("unexpected macOS notarization contract: %+v", notary.Notary)
	}

	if len(config.HomebrewCasks) != 1 {
		t.Fatalf("Homebrew Cask definition count = %d, want 1", len(config.HomebrewCasks))
	}
	cask := config.HomebrewCasks[0]
	if cask.Name != "onebox" || !slices.Equal(cask.IDs, []string{"ob"}) || !slices.Equal(cask.Binaries, []string{"ob"}) || cask.Directory != "Casks" {
		t.Fatalf("unexpected Homebrew Cask identity: %+v", cask)
	}
	if cask.URL.Template != "https://github.com/labstack/onebox/releases/download/{{ .Tag }}/{{ .ArtifactName }}" || cask.URL.Verified != "github.com/labstack/onebox/" {
		t.Fatalf("unexpected Homebrew Cask URL contract: %+v", cask.URL)
	}
	assertReleaseRepository(t, "Homebrew", cask.Repository, "homebrew-tap")

	if len(config.Scoops) != 1 {
		t.Fatalf("Scoop manifest definition count = %d, want 1", len(config.Scoops))
	}
	scoop := config.Scoops[0]
	if scoop.Name != "onebox" || !slices.Equal(scoop.IDs, []string{"ob"}) || scoop.URLTemplate != "https://github.com/labstack/onebox/releases/download/{{ .Tag }}/{{ .ArtifactName }}" {
		t.Fatalf("unexpected Scoop identity: %+v", scoop)
	}
	assertReleaseRepository(t, "Scoop", scoop.Repository, "scoop-bucket")
}

func assertReleaseRepository(t *testing.T, label string, got releaseRepository, name string) {
	t.Helper()
	if got.Owner != "labstack" || got.Name != name || got.Branch != "main" || got.Token != "{{ .Env.PACKAGE_REPOS_TOKEN }}" {
		t.Fatalf("unexpected %s repository: %+v", label, got)
	}
}

func assertStringSet(t *testing.T, label string, got, want []string) {
	t.Helper()
	got = append([]string(nil), got...)
	want = append([]string(nil), want...)
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("%s = %v, want %v", label, got, want)
	}
}
