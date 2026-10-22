//go:build darwin || linux

package selfupdate

import (
	"fmt"
	"os"
)

func replaceExecutable(target, next string) error {
	// The staged file is on the same filesystem. Rename atomically replaces
	// the directory entry while running processes keep their original image.
	if err := os.Rename(next, target); err != nil {
		return fmt.Errorf("replace executable: %w", err)
	}
	return nil
}
