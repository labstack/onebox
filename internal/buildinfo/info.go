// Package buildinfo reports the provenance embedded in the running binary.
package buildinfo

import (
	gobuildinfo "debug/buildinfo"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
)

const developmentVersion = "0.0.1-m0"

// release and buildTime are set by the build recipe with -ldflags. Keeping
// them private makes Read the single source of truth for CLI provenance.
var (
	release   string
	buildTime string
)

// Info is the build and source provenance for the running executable.
type Info struct {
	Version     string `json:"version"`
	VCSRevision string `json:"vcs_revision"`
	Dirty       bool   `json:"dirty"`
	VCSTime     string `json:"vcs_time"`
	BuildTime   string `json:"build_time"`
	GoVersion   string `json:"go_version"`
}

// Runner is the complete provenance contract embedded in executable plans and
// execution evidence. The supported schema list makes compatibility explicit
// instead of asking operators to infer it from a release number.
type Runner struct {
	Info
	SupportedExecutablePlanSchemas []string `json:"supported_executable_plan_schemas"`
}

// CurrentRunner returns provenance for this process with a defensive copy of
// the executable-plan schemas its caller can execute.
func CurrentRunner(schemas ...string) Runner {
	return Runner{
		Info:                           Read(),
		SupportedExecutablePlanSchemas: append([]string(nil), schemas...),
	}
}

// Read returns provenance for the running executable. Go's embedded module
// version is used when no release was supplied by the linker; checkout builds
// retain the development version used by the existing CLI.
func Read() Info {
	info, ok := debug.ReadBuildInfo()
	return read(info, ok, release, buildTime)
}

// ReadFile reads the Go provenance embedded in another executable without
// running it. Linker-only fields such as Onebox's release and build time are
// unavailable unless they are also present in the standard Go build metadata.
func ReadFile(path string) (Info, error) {
	info, err := gobuildinfo.ReadFile(path)
	if err != nil {
		return Info{}, err
	}
	return read(info, true, "", ""), nil
}

func read(info *debug.BuildInfo, ok bool, linkedRelease, linkedBuildTime string) Info {
	result := Info{
		Version:   strings.TrimSpace(linkedRelease),
		BuildTime: strings.TrimSpace(linkedBuildTime),
		GoVersion: runtime.Version(),
	}
	if ok && info != nil {
		if result.Version == "" && info.Main.Version != "" && info.Main.Version != "(devel)" {
			result.Version = info.Main.Version
		}
		if info.GoVersion != "" {
			result.GoVersion = info.GoVersion
		}
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				result.VCSRevision = setting.Value
			case "vcs.modified":
				result.Dirty, _ = strconv.ParseBool(setting.Value)
			case "vcs.time":
				result.VCSTime = setting.Value
			}
		}
	}
	if result.Version == "" {
		result.Version = developmentVersion
	}
	return result
}
