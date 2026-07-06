//go:build windows

package runner

import (
	"os"
	"os/exec"
)

// Windows builds exist only so the module compiles and unit-tests on the
// dev box; production is always linux/amd64 inside the builder container.
// There is no process group here — only the direct child is killed, and
// SIGTERM grace degrades to an immediate kill.
func setProcAttr(*exec.Cmd) {}

func terminate(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

func killHard(cmd *exec.Cmd) { terminate(cmd) }

// platformEnv adds the minimum a Windows child process needs on top of the
// allowlist so dev-box tests can execute at all.
func platformEnv() []string {
	return []string{
		"SYSTEMROOT=" + os.Getenv("SYSTEMROOT"),
		"TEMP=" + os.Getenv("TEMP"),
		"TMP=" + os.Getenv("TMP"),
	}
}
