package service

import (
	"bytes"
	"fmt"
	"strings"
)

// GenerateLinuxServiceUnit generates a systemd user service unit configuration.
func GenerateLinuxServiceUnit(cfg Config) ([]byte, error) {
	if err := ValidateRole(cfg.Role); err != nil {
		return nil, err
	}
	logFile := fmt.Sprintf("%s/%s.log", strings.TrimRight(cfg.LogDir, "/"), cfg.Role)

	var buf bytes.Buffer
	buf.WriteString("[Unit]\n")
	buf.WriteString(fmt.Sprintf("Description=Agent Gateway (%s)\n", cfg.Role))
	buf.WriteString("After=network.target\n\n")

	buf.WriteString("[Service]\n")
	buf.WriteString("Type=simple\n")

	// Escape args if necessary for ExecStart
	execArgs := []string{cfg.BinaryPath}
	execArgs = append(execArgs, cfg.Args...)
	buf.WriteString(fmt.Sprintf("ExecStart=%s\n", strings.Join(execArgs, " ")))

	if cfg.WorkingDir != "" {
		buf.WriteString(fmt.Sprintf("WorkingDirectory=%s\n", cfg.WorkingDir))
	}

	buf.WriteString(fmt.Sprintf("StandardOutput=append:%s\n", logFile))
	buf.WriteString(fmt.Sprintf("StandardError=append:%s\n", logFile))
	buf.WriteString("Restart=always\n")
	buf.WriteString("RestartSec=3\n")

	for k, v := range cfg.Env {
		buf.WriteString(fmt.Sprintf("Environment=\"%s=%s\"\n", k, v))
	}

	buf.WriteString("\n[Install]\n")
	buf.WriteString("WantedBy=default.target\n")

	return buf.Bytes(), nil
}
