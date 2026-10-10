//go:build linux

package main

import (
	"os/exec"
	"syscall"
)

// No Linux o kernel encerra o llama-server automaticamente se o app morrer.
func prepareChild(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
}

func afterStart(cmd *exec.Cmd) {}
