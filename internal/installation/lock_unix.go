//go:build darwin || linux || freebsd || openbsd || netbsd || dragonfly

package installation

import "os"

func syncDirectory(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}
