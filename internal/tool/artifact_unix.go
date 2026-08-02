//go:build !windows

package tool

import (
	"fmt"
	"os"
)

func platformArtifactSupport() error {
	return nil
}

func validatePrivateArtifactDirectory(info os.FileInfo) error {
	if info.Mode().Perm() != 0o700 {
		return fmt.Errorf("directory mode is %04o, want 0700", info.Mode().Perm())
	}
	return nil
}

func validatePrivateArtifactFile(info os.FileInfo) error {
	if !info.Mode().IsRegular() {
		return fmt.Errorf("artifact is not a regular file")
	}
	if info.Mode().Perm() != 0o600 {
		return fmt.Errorf("file mode is %04o, want 0600", info.Mode().Perm())
	}
	return nil
}
