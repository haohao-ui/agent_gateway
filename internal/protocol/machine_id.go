package protocol

import (
	"crypto/sha256"
	"encoding/hex"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// GetMachineID returns a stable unique hardware identifier for the current physical or virtual machine.
func GetMachineID() string {
	raw := ""
	switch runtime.GOOS {
	case "darwin":
		out, err := exec.Command("ioreg", "-rd1", "-c", "IOPlatformExpertDevice").Output()
		if err == nil {
			for _, line := range strings.Split(string(out), "\n") {
				if strings.Contains(line, "IOPlatformUUID") {
					parts := strings.Split(line, "=")
					if len(parts) >= 2 {
						raw = strings.Trim(strings.TrimSpace(parts[1]), "\"")
						break
					}
				}
			}
		}
	case "linux":
		if data, err := os.ReadFile("/etc/machine-id"); err == nil && len(data) > 0 {
			raw = strings.TrimSpace(string(data))
		} else if data, err := os.ReadFile("/var/lib/dbus/machine-id"); err == nil && len(data) > 0 {
			raw = strings.TrimSpace(string(data))
		}
	case "windows":
		out, err := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", `(Get-CimInstance -ClassName Win32_ComputerSystemProduct).UUID`).Output()
		if err == nil && len(out) > 0 {
			raw = strings.TrimSpace(string(out))
		}
	}

	// Fallback: Combine MAC addresses and Hostname
	if raw == "" {
		var macs []string
		if ifaces, err := net.Interfaces(); err == nil {
			for _, iface := range ifaces {
				if iface.Flags&net.FlagUp != 0 && iface.Flags&net.FlagLoopback == 0 && len(iface.HardwareAddr) > 0 {
					macs = append(macs, iface.HardwareAddr.String())
				}
			}
		}
		host, _ := os.Hostname()
		raw = host + ":" + strings.Join(macs, ",")
	}

	h := sha256.Sum256([]byte(raw))
	return "mach-" + hex.EncodeToString(h[:8])
}

// GetDeviceName returns the human-readable machine hostname.
func GetDeviceName() string {
	name, err := os.Hostname()
	if err != nil || strings.TrimSpace(name) == "" {
		return "unknown-host"
	}
	return strings.TrimSpace(name)
}
