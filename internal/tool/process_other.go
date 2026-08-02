//go:build !windows && !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package tool

import (
	"fmt"
	"os/exec"
)

func startLocalProcess(*exec.Cmd) (localProcessTree, error) {
	return nil, newRunnerFailure(
		RunnerFailureSetup,
		fmt.Errorf("process-tree ownership is unsupported on this platform"),
	)
}
