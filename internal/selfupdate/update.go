// Package selfupdate updates portable pi-go installations independently of
// Runtime, application settings, provider credentials and durable sessions.
package selfupdate

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/cat3399/pi-go/internal/product"
)

const repositoryURL = "https://github.com/cat3399/pi-go"
const defaultAPIURL = "https://api.github.com/repos/cat3399/pi-go"

type Options struct {
	Version string `json:"version"`
	Force   bool   `json:"force"`
}

// Plan is the same resolved operation shown by --check and applied by update.
// Private fields pin the archive checksum and the existing installation.
type Plan struct {
	CurrentVersion string `json:"current_version"`
	TargetVersion  string `json:"target_version"`
	Surface        string `json:"surface"`
	OS             string `json:"os"`
	Arch           string `json:"arch"`
	Distribution   string `json:"distribution"`
	Executable     string `json:"executable"`
	Action         string `json:"action"`
	Available      bool   `json:"update_available"`
	CanUpdate      bool   `json:"can_update"`
	Reason         string `json:"reason,omitempty"`
	ReleaseURL     string `json:"release_url"`
	Asset          string `json:"asset"`

	archiveURL string
	checksum   [32]byte
	original   os.FileInfo
	owner      *Updater
}

type Result struct {
	Plan
	Updated bool `json:"updated"`
}

// Updater is configured by the composition root. APIURL/Client are injectable
// for deterministic HTTP tests; user settings cannot redirect executable updates.
type Updater struct {
	Build      product.BuildInfo
	Executable string
	Client     *http.Client
	APIURL     string
}

func New(build product.BuildInfo) *Updater {
	return &Updater{Build: build, Client: &http.Client{Timeout: 5 * time.Minute}, APIURL: defaultAPIURL}
}

// Check is read-only: it fetches release metadata and checksums, never an archive.
func (u *Updater) Check(ctx context.Context, options Options) (Plan, error) {
	if err := ValidateOptions(options); err != nil {
		return Plan{}, err
	}
	if err := validateTarget(u.Build); err != nil {
		return Plan{}, err
	}
	path, original, err := u.installedExecutable()
	if err != nil {
		return Plan{}, err
	}
	target, err := u.resolve(ctx, options.Version)
	if err != nil {
		return Plan{}, err
	}
	p := Plan{
		CurrentVersion: u.Build.Version, TargetVersion: target.version,
		Surface: u.Build.Surface, OS: u.Build.OS, Arch: u.Build.Arch,
		Distribution: u.Build.Distribution, Executable: path, Action: "none",
		CanUpdate: true, ReleaseURL: repositoryURL + "/releases/tag/v" + target.version,
		Asset: target.asset.Name, archiveURL: target.asset.URL, checksum: target.checksum, original: original, owner: u,
	}
	cmp, known := compareCurrent(p.TargetVersion, p.CurrentVersion)
	switch {
	case !known && options.Version == "":
		p.CanUpdate = false
		p.Reason = "current version is unknown; select an official release with --version"
	case cmp > 0 || !known:
		p.Action = "upgrade"
	case cmp < 0 && options.Version != "":
		p.Action = "downgrade"
	case cmp == 0 && options.Force:
		p.Action = "reinstall"
	}
	p.Available = p.Action != "none"
	if u.Build.Surface == "mobile" {
		// Mobile only opens the matching APK download. It neither replaces an
		// installation nor needs permission to switch a locally built binary.
		p.CanUpdate, p.Reason = true, ""
		return p, nil
	}
	if u.Build.Distribution == "go-install" {
		p.CanUpdate = false
		p.Reason = "installed from a Go module; rebuild v" + p.TargetVersion + " using the original Go installation workflow and build options"
	} else if u.Build.Distribution != "archive" && options.Version == "" {
		p.CanUpdate = false
		p.Reason = "local build; rebuild from source, or select an official release with --version"
	}
	return p, nil
}

func (u *Updater) installedExecutable() (string, os.FileInfo, error) {
	if u.Build.Surface == "mobile" {
		return "", nil, nil // Android owns the installed APK; it is never replaced as a file.
	}
	path := u.Executable
	if path == "" {
		var err error
		path, err = os.Executable()
		if err != nil {
			return "", nil, fmt.Errorf("locate executable: %w", err)
		}
	}
	path, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", nil, fmt.Errorf("resolve executable: %w", err)
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return "", nil, err
	}
	original, err := os.Stat(path)
	if err != nil || !original.Mode().IsRegular() {
		return "", nil, fmt.Errorf("executable must be a regular file: %s", path)
	}
	return path, original, nil
}

func validateTarget(build product.BuildInfo) error {
	if build.Surface == "mobile" && build.OS == "android" && build.Arch == "arm64" {
		return nil
	}
	if build.Surface != "terminal" && build.Surface != "web" && build.Surface != "gui" {
		return fmt.Errorf("no release for %s/%s/%s", build.Surface, build.OS, build.Arch)
	}
	if build.Surface == "gui" && build.OS != "darwin" && build.Arch != "amd64" {
		return fmt.Errorf("no GUI release for %s/%s", build.OS, build.Arch)
	}
	if (build.OS != "linux" && build.OS != "darwin" && build.OS != "windows") ||
		(build.Arch != "amd64" && build.Arch != "arm64") {
		return fmt.Errorf("no portable release for %s/%s/%s", build.Surface, build.OS, build.Arch)
	}
	return nil
}

func assetName(build product.BuildInfo, version string) string {
	extension := ".tar.gz"
	if build.Surface == "mobile" {
		extension = ".apk"
	}
	if build.OS == "windows" {
		extension = ".zip"
	}
	return fmt.Sprintf("pi-go-%s_%s_%s_%s%s", build.Surface, version, build.OS, build.Arch, extension)
}

func binaryName(surface, goos string) string {
	name := "pi-go"
	if surface == "gui" {
		name = "pi-go-gui"
	}
	if goos == "windows" {
		name += ".exe"
	}
	return name
}
