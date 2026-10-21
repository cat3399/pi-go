package product

import (
	"runtime"
	"runtime/debug"
	"strings"
)

// Distribution is set to archive by the portable packaging script. A local
// build is not silently replaced with a release just because its version matches.
var Distribution = "source"

func GUIBuildInfo() BuildInfo {
	return BuildInfo{Version: Version, Surface: "gui", OS: runtime.GOOS, Arch: runtime.GOARCH, Distribution: Distribution}
}

// BuildInfo describes an installed executable, independently of Agent state.
type BuildInfo struct {
	Version      string `json:"version"`
	Surface      string `json:"surface"`
	OS           string `json:"os"`
	Arch         string `json:"arch"`
	Distribution string `json:"distribution"`
}

// CLIBuildInfo reports the capabilities actually linked into cmd/pi-go.
func CLIBuildInfo() BuildInfo {
	build, _ := debug.ReadBuildInfo()
	return cliBuildInfo(Version, Distribution, build)
}

func cliBuildInfo(version, distribution string, build *debug.BuildInfo) BuildInfo {
	info := BuildInfo{Version: version, Surface: "terminal", OS: runtime.GOOS, Arch: runtime.GOARCH, Distribution: distribution}
	if build != nil {
		info.Surface = CLISurface(build.Settings)
		// Recent Go versions also derive Main.Version for source checkouts.
		// A downloaded main module additionally has a module checksum.
		if distribution == "source" && build.Main.Sum != "" && build.Main.Version != "" && build.Main.Version != "(devel)" {
			info.Distribution = "go-install"
			info.Version = strings.TrimPrefix(build.Main.Version, "v")
		}
	}
	return info
}

// CLISurface also inspects a downloaded executable before it can be installed.
func CLISurface(settings []debug.BuildSetting) string {
	for _, setting := range settings {
		if setting.Key == "-tags" {
			for _, tag := range strings.FieldsFunc(setting.Value, func(r rune) bool { return r == ',' || r == ' ' }) {
				if tag == "pi_go_webui" {
					return "web"
				}
			}
		}
	}
	return "terminal"
}
