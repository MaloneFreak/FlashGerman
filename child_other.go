//go:build !linux && !windows

package main

import "os/exec"

// No macOS não existe equivalente ao Pdeathsig; o app encerra o llama-server ao fechar
// e, se tiver sido fechado à força, limpa o processo antigo na próxima abertura (pid file).
func prepareChild(cmd *exec.Cmd) {}

func afterStart(cmd *exec.Cmd) {}
