//go:build windows

package tool

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
	"unsafe"
)

const (
	createSuspended                     = 0x00000004
	createNewProcessGroup               = 0x00000200
	processTerminate                    = 0x0001
	processSetQuota                     = 0x0100
	threadSuspendResume                 = 0x0002
	jobObjectBasicAccountingInformation = 1
	jobObjectExtendedLimitInformation   = 9
	jobObjectLimitKillOnJobClose        = 0x00002000
	resumeThreadFailed                  = ^uint32(0)
	windowsTreeSettleLimit              = 5 * time.Second
)

var (
	kernel32                                 = syscall.NewLazyDLL("kernel32.dll")
	createJobObjectW                         = kernel32.NewProc("CreateJobObjectW")
	assignProcessToJobObject                 = kernel32.NewProc("AssignProcessToJobObject")
	setInformationJobObject                  = kernel32.NewProc("SetInformationJobObject")
	terminateJobObject                       = kernel32.NewProc("TerminateJobObject")
	queryInformationJobObject                = kernel32.NewProc("QueryInformationJobObject")
	thread32First                            = kernel32.NewProc("Thread32First")
	thread32Next                             = kernel32.NewProc("Thread32Next")
	openThread                               = kernel32.NewProc("OpenThread")
	resumeThread                             = kernel32.NewProc("ResumeThread")
	invalidWindowsHandle      syscall.Handle = ^syscall.Handle(0)
)

type windowsProcessTree struct {
	job        syscall.Handle
	process    *os.Process
	assigned   bool
	terminated bool
	released   bool
}

type threadEntry32 struct {
	Size           uint32
	Usage          uint32
	ThreadID       uint32
	OwnerProcessID uint32
	BasePriority   int32
	DeltaPriority  int32
	Flags          uint32
}

type jobObjectBasicAccounting struct {
	TotalUserTime             int64
	TotalKernelTime           int64
	ThisPeriodTotalUserTime   int64
	ThisPeriodTotalKernelTime int64
	TotalPageFaultCount       uint32
	TotalProcesses            uint32
	ActiveProcesses           uint32
	TotalTerminatedProcesses  uint32
}

type ioCounters struct {
	ReadOperationCount  uint64
	WriteOperationCount uint64
	OtherOperationCount uint64
	ReadTransferCount   uint64
	WriteTransferCount  uint64
	OtherTransferCount  uint64
}

type jobObjectExtendedLimit struct {
	BasicLimitInformation jobObjectBasicLimit
	IOInfo                ioCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

func startLocalProcess(command *exec.Cmd) (localProcessTree, error) {
	job, err := createWindowsJob()
	if err != nil {
		return nil, newRunnerFailure(RunnerFailureSetup, err)
	}
	tree := &windowsProcessTree{job: job}
	command.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: createSuspended | createNewProcessGroup,
		HideWindow:    true,
	}
	if err := command.Start(); err != nil {
		_ = tree.release()
		return nil, newRunnerFailure(RunnerFailureSpawn, err)
	}
	tree.process = command.Process
	if err := tree.assignAndResume(); err != nil {
		cleanupErr := tree.abortSuspended(command)
		return nil, errors.Join(newRunnerFailure(RunnerFailureSetup, err), cleanupErr)
	}
	return tree, nil
}

func createWindowsJob() (syscall.Handle, error) {
	handle, _, callErr := createJobObjectW.Call(0, 0)
	if handle == 0 {
		return 0, windowsCallError("CreateJobObjectW", callErr)
	}
	return syscall.Handle(handle), nil
}

func (tree *windowsProcessTree) assignAndResume() error {
	processHandle, err := syscall.OpenProcess(
		processTerminate|processSetQuota,
		false,
		uint32(tree.process.Pid),
	)
	if err != nil {
		return fmt.Errorf("open suspended shell process %d: %w", tree.process.Pid, err)
	}
	defer syscall.CloseHandle(processHandle)

	assigned, _, callErr := assignProcessToJobObject.Call(uintptr(tree.job), uintptr(processHandle))
	if assigned == 0 {
		return windowsCallError("AssignProcessToJobObject", callErr)
	}
	tree.assigned = true
	if err := resumeOnlyProcessThread(uint32(tree.process.Pid)); err != nil {
		return err
	}
	return nil
}

func resumeOnlyProcessThread(processID uint32) error {
	snapshot, err := syscall.CreateToolhelp32Snapshot(syscall.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return fmt.Errorf("snapshot suspended shell threads: %w", err)
	}
	if snapshot == invalidWindowsHandle {
		return errors.New("snapshot suspended shell threads returned an invalid handle")
	}
	defer syscall.CloseHandle(snapshot)

	entry := threadEntry32{Size: uint32(unsafe.Sizeof(threadEntry32{}))}
	ok, _, callErr := thread32First.Call(uintptr(snapshot), uintptr(unsafe.Pointer(&entry)))
	if ok == 0 {
		return windowsCallError("Thread32First", callErr)
	}
	for {
		if entry.OwnerProcessID == processID {
			threadHandle, _, openErr := openThread.Call(threadSuspendResume, 0, uintptr(entry.ThreadID))
			if threadHandle == 0 {
				return windowsCallError("OpenThread", openErr)
			}
			previous, _, resumeErr := resumeThread.Call(threadHandle)
			closeErr := syscall.CloseHandle(syscall.Handle(threadHandle))
			if uint32(previous) == resumeThreadFailed {
				return errors.Join(windowsCallError("ResumeThread", resumeErr), closeErr)
			}
			if previous != 1 {
				return errors.Join(
					fmt.Errorf("suspended shell thread had suspend count %d, want 1", previous),
					closeErr,
				)
			}
			return closeErr
		}

		entry.Size = uint32(unsafe.Sizeof(threadEntry32{}))
		next, _, nextErr := thread32Next.Call(uintptr(snapshot), uintptr(unsafe.Pointer(&entry)))
		if next == 0 {
			if errno, ok := nextErr.(syscall.Errno); ok && errno == syscall.ERROR_NO_MORE_FILES {
				return fmt.Errorf("suspended shell process %d has no enumerable main thread", processID)
			}
			return windowsCallError("Thread32Next", nextErr)
		}
	}
}

func (tree *windowsProcessTree) abortSuspended(command *exec.Cmd) error {
	var cleanup error
	if err := tree.terminate(); err != nil {
		cleanup = errors.Join(cleanup, err)
	}
	if (!tree.assigned || cleanup != nil) && tree.process != nil {
		if killErr := tree.process.Kill(); killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
			cleanup = errors.Join(cleanup, fmt.Errorf("kill suspended shell %d: %w", tree.process.Pid, killErr))
		}
	}
	if waitErr := command.Wait(); waitErr != nil {
		var exitError *exec.ExitError
		if !errors.As(waitErr, &exitError) {
			cleanup = errors.Join(cleanup, fmt.Errorf("reap suspended shell: %w", waitErr))
		}
	}
	if err := tree.settleTermination(); err != nil {
		cleanup = errors.Join(cleanup, err)
	}
	if err := tree.release(); err != nil {
		cleanup = errors.Join(cleanup, err)
	}
	return cleanup
}

func (tree *windowsProcessTree) terminate() error {
	if tree == nil || tree.job == 0 || tree.released {
		return nil
	}
	var result error
	if !tree.terminated {
		if err := setWindowsKillOnJobClose(tree.job); err != nil {
			result = errors.Join(result, err)
		}
		tree.terminated = true
	}
	terminated, _, callErr := terminateJobObject.Call(uintptr(tree.job), 1)
	if terminated == 0 {
		result = errors.Join(result, windowsCallError("TerminateJobObject", callErr))
		if tree.process != nil {
			if err := tree.process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
				result = errors.Join(result, fmt.Errorf("kill direct Windows shell %d: %w", tree.process.Pid, err))
			}
		}
	}
	return result
}

func setWindowsKillOnJobClose(job syscall.Handle) error {
	limits := jobObjectExtendedLimit{}
	limits.BasicLimitInformation.LimitFlags = jobObjectLimitKillOnJobClose
	ok, _, callErr := setInformationJobObject.Call(
		uintptr(job),
		jobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&limits)),
		unsafe.Sizeof(limits),
	)
	if ok == 0 {
		return windowsCallError("SetInformationJobObject", callErr)
	}
	return nil
}

func (tree *windowsProcessTree) settleTermination() error {
	if tree == nil || !tree.terminated || tree.job == 0 || tree.released {
		return nil
	}
	deadline := time.Now().Add(windowsTreeSettleLimit)
	for {
		active, err := windowsJobActiveProcesses(tree.job)
		if err != nil {
			return err
		}
		if active == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("Windows job still owns %d active process(es) after termination", active)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func windowsJobActiveProcesses(job syscall.Handle) (uint32, error) {
	accounting := jobObjectBasicAccounting{}
	ok, _, callErr := queryInformationJobObject.Call(
		uintptr(job),
		jobObjectBasicAccountingInformation,
		uintptr(unsafe.Pointer(&accounting)),
		unsafe.Sizeof(accounting),
		0,
	)
	if ok == 0 {
		return 0, windowsCallError("QueryInformationJobObject", callErr)
	}
	return accounting.ActiveProcesses, nil
}

func (tree *windowsProcessTree) release() error {
	if tree == nil || tree.job == 0 || tree.released {
		return nil
	}
	tree.released = true
	err := syscall.CloseHandle(tree.job)
	tree.job = 0
	if err != nil {
		return fmt.Errorf("close Windows process job: %w", err)
	}
	return nil
}

func windowsCallError(operation string, err error) error {
	if err == nil {
		return errors.New(operation + " failed")
	}
	if errno, ok := err.(syscall.Errno); ok && errno == 0 {
		return errors.New(operation + " failed")
	}
	return fmt.Errorf("%s: %w", operation, err)
}
