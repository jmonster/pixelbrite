//go:build unix

package adapter

import (
	"os"
	"os/exec"
	"syscall"
)

// ownGroup starts the adapter as the leader of a new process group, so the
// processes it starts can be stopped with it.
func ownGroup(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

// killGroup kills the adapter's process group: the adapter, if it still
// runs, and every process it started that stayed in the group.
func killGroup(p *os.Process) { _ = syscall.Kill(-p.Pid, syscall.SIGKILL) }
