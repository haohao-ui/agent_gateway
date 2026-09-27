//go:build !windows

package main

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

func setSysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}
}

func isProcessAlive(pid int) bool {
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = process.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}

func killProcess(pid int) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return nil
	}
	return process.Signal(syscall.SIGTERM)
}

func forceKillProcess(pid int) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return nil
	}
	return process.Signal(syscall.SIGKILL)
}

func reloadProcess(pid int) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return process.Signal(syscall.SIGHUP)
}
