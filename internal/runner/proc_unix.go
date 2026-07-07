// Copyright 2026 Jeffrey B. Stewart
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

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
