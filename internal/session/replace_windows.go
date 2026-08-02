//go:build windows

package session

import "os"

// Windows does not offer a portable directory fsync through os.File. Rename is
// still atomic on a volume; callers treat any returned error as commit-unknown.
func replaceTemporary(temporaryPath, targetPath string) (bool, error) {
	if err := os.Rename(temporaryPath, targetPath); err != nil {
		return false, err
	}
	return true, nil
}
