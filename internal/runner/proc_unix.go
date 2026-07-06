//go:build unix

package runner

import (
	"os/exec"
	"syscall"
)

// setProcAttr puts the child in its own process group so the whole tree
// (Gradle daemons, forked test JVMs) can be signaled at once.
func setProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func terminate(cmd *exec.Cmd) { signalGroup(cmd, syscall.SIGTERM) }
func killHard(cmd *exec.Cmd)  { signalGroup(cmd, syscall.SIGKILL) }

func signalGroup(cmd *exec.Cmd, sig syscall.Signal) {
	if cmd.Process == nil {
		return
	}
	// With Setpgid the child's pid is its pgid; the negative pid signals
	// the entire group.
	_ = syscall.Kill(-cmd.Process.Pid, sig)
}

func platformEnv() []string { return nil }
