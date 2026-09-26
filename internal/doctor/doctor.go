// Package doctor provides environmental and diagnostic health checks for nodes and gateway servers.
package doctor

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/pem"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agent-gateway/internal/identity"
	"agent-gateway/internal/node"
	_ "modernc.org/sqlite"
)

// Status represents the health status of a diagnostic check.
type Status string

const (
	StatusOK      Status = "OK"
	StatusWarning Status = "WARN"
	StatusFail    Status = "FAIL"
)

// CheckResult records the outcome of a single diagnostic check.
type CheckResult struct {
	Name        string `json:"name"`
	Status      Status `json:"status"`
	Message     string `json:"message"`
	Remediation string `json:"remediation,omitempty"`
}

// Report holds the aggregated findings of a diagnostic run.
type Report struct {
	Mode    string        `json:"mode"`
	Target  string        `json:"target"`
	Healthy bool          `json:"healthy"`
	Checks  []CheckResult `json:"checks"`
}

// CheckCertificate verifies that a PEM-encoded X.509 certificate is valid and not expired.
func CheckCertificate(name string, certPEM []byte, warnWithin time.Duration) CheckResult {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return CheckResult{
			Name:        name,
			Status:      StatusFail,
			Message:     "not a valid PEM certificate block",
			Remediation: "verify the certificate file content or re-enroll",
		}
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return CheckResult{
			Name:        name,
			Status:      StatusFail,
			Message:     fmt.Sprintf("failed to parse certificate: %v", err),
			Remediation: "re-generate or re-enroll the certificate",
		}
	}
	now := time.Now()
	if now.Before(cert.NotBefore) {
		return CheckResult{
			Name:        name,
			Status:      StatusFail,
			Message:     fmt.Sprintf("certificate is not yet valid (valid from %s)", cert.NotBefore.UTC().Format(time.RFC3339)),
			Remediation: "check system clock synchronization",
		}
	}
	if now.After(cert.NotAfter) {
		return CheckResult{
			Name:        name,
			Status:      StatusFail,
			Message:     fmt.Sprintf("certificate expired on %s", cert.NotAfter.UTC().Format(time.RFC3339)),
			Remediation: "re-pair device to renew certificate",
		}
	}
	remaining := cert.NotAfter.Sub(now)
	if remaining < warnWithin {
		days := int(remaining.Hours() / 24)
		return CheckResult{
			Name:        name,
			Status:      StatusWarning,
			Message:     fmt.Sprintf("certificate expires in %d days (%s)", days, cert.NotAfter.UTC().Format(time.RFC3339)),
			Remediation: "plan to renew or re-pair before expiration",
		}
	}
	days := int(remaining.Hours() / 24)
	return CheckResult{
		Name:    name,
		Status:  StatusOK,
		Message: fmt.Sprintf("valid (expires in %d days on %s)", days, cert.NotAfter.UTC().Format("2006-01-02")),
	}
}

// CheckSQLiteIntegrity opens a SQLite database file and executes PRAGMA integrity_check.
func CheckSQLiteIntegrity(name, dbPath string) CheckResult {
	if _, err := os.Stat(dbPath); err != nil {
		if os.IsNotExist(err) {
			return CheckResult{
				Name:    name,
				Status:  StatusWarning,
				Message: fmt.Sprintf("database file %s does not exist yet", filepath.Base(dbPath)),
			}
		}
		return CheckResult{
			Name:        name,
			Status:      StatusFail,
			Message:     fmt.Sprintf("cannot access database file: %v", err),
			Remediation: "verify file path and OS permissions",
		}
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return CheckResult{
			Name:        name,
			Status:      StatusFail,
			Message:     fmt.Sprintf("failed to open database: %v", err),
			Remediation: "check database lock or permissions",
		}
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var result string
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check(1);").Scan(&result); err != nil {
		return CheckResult{
			Name:        name,
			Status:      StatusFail,
			Message:     fmt.Sprintf("integrity check failed: %v", err),
			Remediation: "database file may be corrupt; restore from backup",
		}
	}
	if strings.ToLower(result) != "ok" {
		return CheckResult{
			Name:        name,
			Status:      StatusFail,
			Message:     fmt.Sprintf("database integrity error: %s", result),
			Remediation: "database file is damaged; restore from backup",
		}
	}
	return CheckResult{
		Name:    name,
		Status:  StatusOK,
		Message: "integrity check OK (PRAGMA integrity_check: ok)",
	}
}

// CheckClockSkew compares local time with an external reference timestamp.
func CheckClockSkew(serverTime time.Time, maxSkew time.Duration) CheckResult {
	now := time.Now()
	skew := now.Sub(serverTime)
	absSkew := time.Duration(math.Abs(float64(skew)))

	if absSkew > maxSkew {
		return CheckResult{
			Name:        "Clock Skew",
			Status:      StatusWarning,
			Message:     fmt.Sprintf("clock drift of %v exceeds safe threshold of %v (server: %s, local: %s)", skew.Round(time.Millisecond), maxSkew, serverTime.UTC().Format(time.RFC3339), now.UTC().Format(time.RFC3339)),
			Remediation: "synchronize system clock with an NTP server",
		}
	}
	return CheckResult{
		Name:    "Clock Skew",
		Status:  StatusOK,
		Message: fmt.Sprintf("in sync (drift %v within safe threshold %v)", skew.Round(time.Millisecond), maxSkew),
	}
}

// DiagnoseNode performs environmental checks for a paired node directory.
func DiagnoseNode(ctx context.Context, nodeDir string) Report {
	report := Report{
		Mode:    "node",
		Target:  nodeDir,
		Healthy: true,
	}

	state, err := node.LoadState(nodeDir)
	if err != nil {
		report.Checks = append(report.Checks, CheckResult{
			Name:        "Node State",
			Status:      StatusFail,
			Message:     fmt.Sprintf("failed to load state.json: %v", err),
			Remediation: "run 'mesh pair' to enroll this machine first",
		})
		report.Healthy = false
		return report
	}
	report.Checks = append(report.Checks, CheckResult{
		Name:    "Node State",
		Status:  StatusOK,
		Message: fmt.Sprintf("loaded state.json for NodeID=%s (server: %s)", state.NodeID, state.ServerURL),
	})

	certBytes, err := os.ReadFile(filepath.Join(nodeDir, node.NodeCertFile))
	if err != nil {
		report.Checks = append(report.Checks, CheckResult{
			Name:        "Node Certificate",
			Status:      StatusFail,
			Message:     fmt.Sprintf("cannot read certificate (%s): %v", node.NodeCertFile, err),
			Remediation: "re-pair the node to re-issue credentials",
		})
		report.Healthy = false
	} else {
		certCheck := CheckCertificate("Node Certificate", certBytes, 14*24*time.Hour)
		if certCheck.Status == StatusFail {
			report.Healthy = false
		}
		report.Checks = append(report.Checks, certCheck)
	}

	caBytes, err := os.ReadFile(filepath.Join(nodeDir, node.NodeCAFile))
	if err != nil {
		report.Checks = append(report.Checks, CheckResult{
			Name:        "Gateway CA Root",
			Status:      StatusFail,
			Message:     fmt.Sprintf("cannot read CA certificate (%s): %v", node.NodeCAFile, err),
			Remediation: "re-pair the node or copy the gateway CA cert",
		})
		report.Healthy = false
	} else {
		caCheck := CheckCertificate("Gateway CA Root", caBytes, 30*24*time.Hour)
		if caCheck.Status == StatusFail {
			report.Healthy = false
		}
		report.Checks = append(report.Checks, caCheck)
	}

	keyBytes, err := os.ReadFile(filepath.Join(nodeDir, node.NodeKeyFile))
	if err != nil {
		report.Checks = append(report.Checks, CheckResult{
			Name:        "Node Key",
			Status:      StatusFail,
			Message:     fmt.Sprintf("cannot read private key (%s): %v", node.NodeKeyFile, err),
			Remediation: "verify permissions or re-pair the node",
		})
		report.Healthy = false
	}

	// Check Gateway Connectivity & Clock Skew
	u, err := url.Parse(state.ServerURL)
	if err != nil {
		report.Checks = append(report.Checks, CheckResult{
			Name:        "Gateway Connection",
			Status:      StatusFail,
			Message:     fmt.Sprintf("invalid gateway URL %q: %v", state.ServerURL, err),
			Remediation: "correct the server URL in node.json",
		})
		report.Healthy = false
	} else if len(caBytes) > 0 && len(certBytes) > 0 && len(keyBytes) > 0 {
		tlsConfig, err := identity.BuildClientTLSConfig(caBytes, certBytes, keyBytes)
		if err != nil {
			report.Checks = append(report.Checks, CheckResult{
				Name:        "mTLS Configuration",
				Status:      StatusFail,
				Message:     fmt.Sprintf("cannot build client TLS config: %v", err),
				Remediation: "check key and certificate validity",
			})
			report.Healthy = false
		} else {
			report.Checks = append(report.Checks, checkGatewayProbe(ctx, u.String(), tlsConfig))
		}
	}

	// Check Local Journal / Outbox DB files
	journalDB := filepath.Join(nodeDir, "journal.sqlite")
	if _, err := os.Stat(journalDB); err == nil {
		report.Checks = append(report.Checks, CheckSQLiteIntegrity("Journal DB", journalDB))
	}
	outboxDB := filepath.Join(nodeDir, "outbox.sqlite")
	if _, err := os.Stat(outboxDB); err == nil {
		report.Checks = append(report.Checks, CheckSQLiteIntegrity("Outbox DB", outboxDB))
	}

	for _, c := range report.Checks {
		if c.Status == StatusFail {
			report.Healthy = false
			break
		}
	}
	return report
}

// DiagnoseServer performs diagnostic checks for a gateway server data directory.
func DiagnoseServer(ctx context.Context, dataDir string) Report {
	report := Report{
		Mode:    "server",
		Target:  dataDir,
		Healthy: true,
	}

	// Check CA existence
	caCertPath := filepath.Join(dataDir, "ca.crt")
	caBytes, err := os.ReadFile(caCertPath)
	if err != nil {
		report.Checks = append(report.Checks, CheckResult{
			Name:        "Gateway CA",
			Status:      StatusFail,
			Message:     fmt.Sprintf("cannot read CA certificate from %s: %v", caCertPath, err),
			Remediation: "ensure gateway server has run at least once to initialize CA",
		})
		report.Healthy = false
	} else {
		caCheck := CheckCertificate("Gateway CA", caBytes, 30*24*time.Hour)
		if caCheck.Status == StatusFail {
			report.Healthy = false
		}
		report.Checks = append(report.Checks, caCheck)
	}

	// Check Core Databases Integrity
	dbs := []struct {
		name string
		file string
	}{
		{"Tasks Database", "tasks.sqlite"},
		{"Devices Database", "devices.sqlite"},
		{"Policy Database", "policy.sqlite"},
	}
	for _, dbInfo := range dbs {
		dbPath := filepath.Join(dataDir, dbInfo.file)
		check := CheckSQLiteIntegrity(dbInfo.name, dbPath)
		if check.Status == StatusFail {
			report.Healthy = false
		}
		report.Checks = append(report.Checks, check)
	}

	for _, c := range report.Checks {
		if c.Status == StatusFail {
			report.Healthy = false
			break
		}
	}
	return report
}

func checkGatewayProbe(ctx context.Context, serverURL string, tlsConfig *tls.Config) CheckResult {
	probeClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig:   tlsConfig,
			ForceAttemptHTTP2: true,
		},
		Timeout: 5 * time.Second,
	}
	defer probeClient.CloseIdleConnections()

	req, err := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(serverURL, "/")+"/v1/tasks/probe", nil)
	if err != nil {
		return CheckResult{
			Name:    "Gateway Connectivity",
			Status:  StatusFail,
			Message: fmt.Sprintf("invalid probe request: %v", err),
		}
	}
	resp, err := probeClient.Do(req)
	if err != nil {
		return CheckResult{
			Name:        "Gateway Connectivity",
			Status:      StatusFail,
			Message:     fmt.Sprintf("cannot reach gateway: %v", err),
			Remediation: "check gateway process status and network firewall",
		}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return CheckResult{
			Name:        "Gateway Connectivity",
			Status:      StatusFail,
			Message:     fmt.Sprintf("gateway probe rejected with HTTP %d", resp.StatusCode),
			Remediation: "verify node certificate enrollment and authorization",
		}
	}

	var notes []string
	proto := resp.Proto
	if proto != "" {
		notes = append(notes, "protocol "+proto)
	}

	// Inspect Date header for Clock Skew
	dateHeader := resp.Header.Get("Date")
	if dateHeader != "" {
		if serverTime, err := http.ParseTime(dateHeader); err == nil {
			skew := time.Since(serverTime)
			if math.Abs(float64(skew)) > float64(5*time.Second) {
				return CheckResult{
					Name:        "Gateway Connectivity",
					Status:      StatusWarning,
					Message:     fmt.Sprintf("connected (%s), but clock drift is %v (> 5s)", strings.Join(notes, ", "), skew.Round(time.Millisecond)),
					Remediation: "synchronize system clock with network time (NTP)",
				}
			}
			notes = append(notes, fmt.Sprintf("clock drift %v", skew.Round(time.Millisecond)))
		}
	}

	return CheckResult{
		Name:    "Gateway Connectivity",
		Status:  StatusOK,
		Message: fmt.Sprintf("mTLS handshake succeeded (%s)", strings.Join(notes, ", ")),
	}
}
