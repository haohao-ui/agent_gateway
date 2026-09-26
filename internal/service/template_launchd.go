package service

import (
	"bytes"
	"fmt"
	"html"
	"strings"
)

// GenerateDarwinPlist generates an XML Property List for macOS launchd.
func GenerateDarwinPlist(cfg Config) ([]byte, error) {
	if err := ValidateRole(cfg.Role); err != nil {
		return nil, err
	}
	label := fmt.Sprintf("com.agent-gateway.%s", cfg.Role)
	logFile := fmt.Sprintf("%s/%s.log", strings.TrimRight(cfg.LogDir, "/"), cfg.Role)

	var buf bytes.Buffer
	buf.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n")
	buf.WriteString("<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n")
	buf.WriteString("<plist version=\"1.0\">\n<dict>\n")

	// Label
	buf.WriteString(fmt.Sprintf("\t<key>Label</key>\n\t<string>%s</string>\n", html.EscapeString(label)))

	// ProgramArguments
	buf.WriteString("\t<key>ProgramArguments</key>\n\t<array>\n")
	buf.WriteString(fmt.Sprintf("\t\t<string>%s</string>\n", html.EscapeString(cfg.BinaryPath)))
	for _, arg := range cfg.Args {
		buf.WriteString(fmt.Sprintf("\t\t<string>%s</string>\n", html.EscapeString(arg)))
	}
	buf.WriteString("\t</array>\n")

	// WorkingDirectory
	if cfg.WorkingDir != "" {
		buf.WriteString(fmt.Sprintf("\t<key>WorkingDirectory</key>\n\t<string>%s</string>\n", html.EscapeString(cfg.WorkingDir)))
	}

	// Logging
	buf.WriteString(fmt.Sprintf("\t<key>StandardOutPath</key>\n\t<string>%s</string>\n", html.EscapeString(logFile)))
	buf.WriteString(fmt.Sprintf("\t<key>StandardErrorPath</key>\n\t<string>%s</string>\n", html.EscapeString(logFile)))

	// Process management
	buf.WriteString("\t<key>RunAtLoad</key>\n\t<true/>\n")
	buf.WriteString("\t<key>KeepAlive</key>\n\t<true/>\n")

	// Environment variables
	if len(cfg.Env) > 0 {
		buf.WriteString("\t<key>EnvironmentVariables</key>\n\t<dict>\n")
		for k, v := range cfg.Env {
			buf.WriteString(fmt.Sprintf("\t\t<key>%s</key>\n\t\t<string>%s</string>\n", html.EscapeString(k), html.EscapeString(v)))
		}
		buf.WriteString("\t</dict>\n")
	}

	buf.WriteString("</dict>\n</plist>\n")
	return buf.Bytes(), nil
}
