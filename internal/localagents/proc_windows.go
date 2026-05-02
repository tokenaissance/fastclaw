//go:build windows

package localagents

import (
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// setSysProcAttr detaches the gateway from the launching console so it
// survives the parent shell closing and can receive a graceful CTRL_BREAK
// from Stop(). DETACHED_PROCESS prevents the child from inheriting the
// console; CREATE_NEW_PROCESS_GROUP gives it its own group id so we can
// signal it later.
func setSysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP,
	}
}

// signalPID maps the platform-neutral signal names onto Windows
// primitives. TERM sends CTRL_BREAK_EVENT (handled by Go runtimes the
// same as SIGTERM); KILL terminates immediately; CHECK probes liveness.
func signalPID(pid int, sig string) error {
	switch sig {
	case "TERM":
		if err := windows.GenerateConsoleCtrlEvent(syscall.CTRL_BREAK_EVENT, uint32(pid)); err == nil {
			return nil
		}
		proc, err := os.FindProcess(pid)
		if err != nil {
			return err
		}
		return proc.Kill()
	case "KILL":
		proc, err := os.FindProcess(pid)
		if err != nil {
			return err
		}
		return proc.Kill()
	case "CHECK":
		if isProcessAlive(pid) {
			return nil
		}
		return os.ErrProcessDone
	}
	return nil
}

func isProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	const stillActive = 259
	return code == stillActive
}
