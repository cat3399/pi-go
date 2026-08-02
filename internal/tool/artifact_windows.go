//go:build windows

package tool

import (
	"fmt"
	"os"
)

func platformArtifactSupport() error {
	return fmt.Errorf(
		"%w: this build has no verified owner-only Windows ACL adapter",
		ErrArtifactSecurity,
	)
}

func validatePrivateArtifactDirectory(os.FileInfo) error {
	return platformArtifactSupport()
}

func validatePrivateArtifactFile(os.FileInfo) error {
	return platformArtifactSupport()
}
