//go:build windows

package main

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

func setSysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: 0x00000008 | 0x00000200, // DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP
	}
}

const processQueryLimitedInformation = 0x1000

func isProcessAlive(pid int) bool {
	handle, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		// Fallback to query information
		handle, err = syscall.OpenProcess(0x0400, false, uint32(pid))
		if err != nil {
			return false
		}
	}
	defer syscall.CloseHandle(handle)
	var exitCode uint32
	if err := syscall.GetExitCodeProcess(handle, &exitCode); err != nil {
		return false
	}
	const stillActive = 259
	return exitCode == stillActive
}

func killProcess(pid int) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return nil
	}
	return process.Kill()
}

func forceKillProcess(pid int) error {
	return killProcess(pid)
}

func reloadProcess(pid int) error {
	return errors.New("signal reload not supported on windows")
}
