//go:build !windows

package tool

import (
	"fmt"
	"os"
)

func platformCreatePrivateTempDirectory(parent, prefix string) (string, error) {
	root, err := os.MkdirTemp(parent, prefix)
	if err != nil {
		return "", err
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return "", err
	}
	return root, nil
}

func platformCreatePrivateDirectory(path string) error {
	if err := os.Mkdir(path, 0o700); err != nil {
		return err
	}
	return os.Chmod(path, 0o700)
}

func platformCreatePrivateTempFile(root, prefix, suffix string) (*os.File, error) {
	file, err := os.CreateTemp(root, prefix+"*"+suffix)
	if err != nil {
		return nil, err
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func validatePrivateArtifactDirectory(_ string, info os.FileInfo) error {
	if info.Mode().Perm() != 0o700 {
		return fmt.Errorf("directory mode is %04o, want 0700", info.Mode().Perm())
	}
	return nil
}

func validatePrivateArtifactFile(_ string, _ *os.File, info os.FileInfo) error {
	if !info.Mode().IsRegular() {
		return fmt.Errorf("artifact is not a regular file")
	}
	if info.Mode().Perm() != 0o600 {
		return fmt.Errorf("file mode is %04o, want 0600", info.Mode().Perm())
	}
	return nil
}
