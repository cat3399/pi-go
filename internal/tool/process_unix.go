//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package tool

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

type unixProcessTree struct {
	process *os.Process
}

func startLocalProcess(command *exec.Cmd) (localProcessTree, error) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		return nil, newRunnerFailure(RunnerFailureSpawn, err)
	}
	return &unixProcessTree{process: command.Process}, nil
}

func (tree *unixProcessTree) terminate() error {
	process := tree.process
	if process == nil {
		return nil
	}
	groupErr := syscall.Kill(-process.Pid, syscall.SIGKILL)
	if groupErr == nil || errors.Is(groupErr, syscall.ESRCH) {
		return nil
	}
	directErr := process.Kill()
	if directErr == nil || errors.Is(directErr, os.ErrProcessDone) {
		return fmt.Errorf("kill process group %d: %w", process.Pid, groupErr)
	}
	return errors.Join(
		fmt.Errorf("kill process group %d: %w", process.Pid, groupErr),
		fmt.Errorf("kill direct process %d: %w", process.Pid, directErr),
	)
}

func (*unixProcessTree) release() error {
	return nil
}

func (*unixProcessTree) settleTermination() error {
	return nil
}
