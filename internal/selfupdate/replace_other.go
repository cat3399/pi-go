//go:build !darwin && !linux && !windows

package selfupdate

import "errors"

func replaceExecutable(string, string, string) error {
	return errors.New("portable self-update is unavailable on this platform")
}
