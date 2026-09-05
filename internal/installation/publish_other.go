//go:build !darwin && !linux && !windows

package installation

import "errors"

func publishDirectory(string, string) error {
	return errors.New("exclusive installation publication is unavailable on this platform")
}
