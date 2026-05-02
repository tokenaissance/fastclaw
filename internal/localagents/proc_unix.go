//go:build !windows

package localagents

import (
	"os"
	"os/exec"
	"syscall"
)

func setSysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// signalPID delivers a signal. TERM and KILL go to the entire process
// group (negative PID) so children spawned by the gateway — sandbox
// runners, plugin subprocesses — get torn down too. CHECK uses signal 0
// to probe the leader only; that's enough for liveness.
func signalPID(pid int, sig string) error {
	switch sig {
	case "TERM":
		if err := syscall.Kill(-pid, syscall.SIGTERM); err == nil {
			return nil
		}
		// Fallback when the leader is not its own pgid (e.g. a process
		// started without Setsid that we picked up from a stale pidfile).
		proc, err := os.FindProcess(pid)
		if err != nil {
			return err
		}
		return proc.Signal(syscall.SIGTERM)
	case "KILL":
		if err := syscall.Kill(-pid, syscall.SIGKILL); err == nil {
			return nil
		}
		proc, err := os.FindProcess(pid)
		if err != nil {
			return err
		}
		return proc.Signal(syscall.SIGKILL)
	case "CHECK":
		proc, err := os.FindProcess(pid)
		if err != nil {
			return err
		}
		return proc.Signal(syscall.Signal(0))
	}
	return nil
}

func isProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return signalPID(pid, "CHECK") == nil
}
