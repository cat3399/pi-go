package selfupdate

import (
	"bytes"
	"context"
	"debug/buildinfo"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/cat3399/pi-go/internal/filelock"
	"github.com/cat3399/pi-go/internal/product"
)

// Apply only installs the exact asset resolved by Check. The lock is scoped to
// the real executable path, so invocations through different symlinks serialize.
func (u *Updater) Apply(ctx context.Context, plan Plan) (Result, error) {
	result := Result{Plan: plan}
	if plan.Surface == "mobile" {
		return result, errors.New("mobile only supports APK downloads")
	}
	if !plan.CanUpdate {
		return result, errors.New(plan.Reason)
	}
	if !plan.Available {
		return result, nil
	}
	if plan.owner != u || plan.original == nil || plan.archiveURL == "" {
		return result, errors.New("update plan was not resolved by Check")
	}
	if plan.OS != runtime.GOOS || plan.Arch != runtime.GOARCH ||
		plan.Surface != u.Build.Surface || plan.CurrentVersion != u.Build.Version {
		return result, errors.New("update plan does not match this executable")
	}
	unlock, err := filelock.Acquire(ctx, plan.Executable+".update.lock")
	if err != nil {
		return result, fmt.Errorf("lock executable directory (update requires write access): %w", err)
	}
	defer unlock()
	if err := unchanged(plan); err != nil {
		return result, err
	}
	archive, err := u.download(ctx, plan)
	if err != nil {
		return result, err
	}
	stage, err := os.MkdirTemp(filepath.Dir(plan.Executable), ".pi-go-update-")
	if err != nil {
		return result, fmt.Errorf("stage update beside executable: %w", err)
	}
	filename := binaryName(plan.Surface, plan.OS)
	next := filepath.Join(stage, filename)
	if err := extractExecutable(ctx, archive, next, plan); err != nil {
		return result, fmt.Errorf("%w (staging directory: %s)", err, stage)
	}
	if err := verifyExecutable(ctx, next, plan); err != nil {
		return result, err
	}
	if err := os.Chmod(next, plan.original.Mode().Perm()); err != nil {
		return result, err
	}
	// Cancellation and concurrent replacements are checked immediately before
	// the short commit section. Cancellation cannot interrupt its rollback.
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if err := unchanged(plan); err != nil {
		return result, err
	}
	result.Backup = filepath.Join(stage, "previous-"+filename)
	if err := replaceExecutable(plan.Executable, next, result.Backup); err != nil {
		return result, err
	}
	result.Updated = true
	return result, nil
}

func unchanged(plan Plan) error {
	now, err := os.Stat(plan.Executable)
	if err != nil || !os.SameFile(plan.original, now) ||
		now.Size() != plan.original.Size() || now.Mode() != plan.original.Mode() || !now.ModTime().Equal(plan.original.ModTime()) {
		return errors.New("executable changed while checking for updates; run the command again")
	}
	return nil
}

func verifyExecutable(ctx context.Context, path string, plan Plan) error {
	info, err := buildinfo.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read downloaded Go build identity: %w", err)
	}
	expectedPath := "github.com/cat3399/pi-go/cmd/pi-go"
	if plan.Surface == "gui" {
		expectedPath = "github.com/cat3399/pi-go/surface/gui"
	}
	if info.Path != expectedPath {
		return fmt.Errorf("downloaded program does not match the %s package: %s", plan.Surface, info.Path)
	}
	settings := make(map[string]string)
	for _, setting := range info.Settings {
		settings[setting.Key] = setting.Value
	}
	if settings["GOOS"] != plan.OS || settings["GOARCH"] != plan.Arch || (plan.Surface != "gui" && product.CLISurface(info.Settings) != plan.Surface) {
		return fmt.Errorf("downloaded executable does not match %s/%s/%s", plan.Surface, plan.OS, plan.Arch)
	}
	if plan.Surface == "gui" {
		// Existing GUI releases are unstripped and have no --version entry point.
		// Their linker string can be read without starting a WebView. Future
		// stripped builds use the early --version path below instead.
		if version, err := guiLinkedVersion(path, plan.OS); err == nil {
			if version != plan.TargetVersion {
				return fmt.Errorf("downloaded GUI declares version %q, expected %s", version, plan.TargetVersion)
			}
			return nil
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, path, "--version")
	command.WaitDelay = time.Second
	var output boundedOutput
	command.Stdout, command.Stderr = &output, &output
	if err := command.Run(); err != nil {
		return fmt.Errorf("downloaded executable failed its version check: %w (%s)", err, strings.TrimSpace(output.String()))
	}
	if strings.TrimPrefix(strings.TrimSpace(output.String()), "v") != plan.TargetVersion {
		return fmt.Errorf("downloaded executable reports %q, expected %s", strings.TrimSpace(output.String()), plan.TargetVersion)
	}
	return nil
}

type boundedOutput struct{ buffer bytes.Buffer }

func (b *boundedOutput) String() string { return b.buffer.String() }

func (b *boundedOutput) Write(p []byte) (int, error) {
	if b.buffer.Len()+len(p) > 4096 {
		return 0, errors.New("version output exceeds limit")
	}
	return b.buffer.Write(p)
}
