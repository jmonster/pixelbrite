//go:build windows

package adapter

import (
	"os"
	"os/exec"
)

func ownGroup(*exec.Cmd) {}

// killGroup kills the adapter. Windows has no process group to take its
// helpers with it; the adapter stops them when its stdin closes.
func killGroup(p *os.Process) { _ = p.Kill() }
