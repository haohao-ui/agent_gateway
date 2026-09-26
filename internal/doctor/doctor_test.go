package doctor

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/pem"
	"math/big"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"agent-gateway/internal/devicestore"
	"agent-gateway/internal/httpapi"
	"agent-gateway/internal/identity"
	"agent-gateway/internal/node"
	"agent-gateway/internal/policy"
	"agent-gateway/internal/taskstore"
	_ "modernc.org/sqlite"
)

func generateTestCert(notBefore, notAfter time.Time) ([]byte, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			CommonName: "test-node",
		},
		NotBefore: notBefore,
		NotAfter:  notAfter,
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		return nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: der,
	})
	return certPEM, nil
}

func TestCheckCertificate(t *testing.T) {
	// 1. Valid certificate
	certValid, err := generateTestCert(time.Now().Add(-1*time.Hour), time.Now().Add(30*24*time.Hour))
	if err != nil {
		t.Fatalf("failed to generate test cert: %v", err)
	}
	res := CheckCertificate("valid-cert", certValid, 7*24*time.Hour)
	if res.Status != StatusOK {
		t.Errorf("expected OK, got %v: %s", res.Status, res.Message)
	}

	// 2. Expiring soon (warn)
	certExpiring, err := generateTestCert(time.Now().Add(-1*time.Hour), time.Now().Add(3*24*time.Hour))
	if err != nil {
		t.Fatalf("failed to generate test cert: %v", err)
	}
	res = CheckCertificate("expiring-cert", certExpiring, 7*24*time.Hour)
	if res.Status != StatusWarning {
		t.Errorf("expected WARN, got %v: %s", res.Status, res.Message)
	}

	// 3. Expired (fail)
	certExpired, err := generateTestCert(time.Now().Add(-48*time.Hour), time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("failed to generate test cert: %v", err)
	}
	res = CheckCertificate("expired-cert", certExpired, 7*24*time.Hour)
	if res.Status != StatusFail {
		t.Errorf("expected FAIL, got %v: %s", res.Status, res.Message)
	}

	// 4. Invalid PEM
	res = CheckCertificate("invalid-pem", []byte("invalid non-pem data"), 7*24*time.Hour)
	if res.Status != StatusFail {
		t.Errorf("expected FAIL, got %v: %s", res.Status, res.Message)
	}
}

func TestCheckSQLiteIntegrity(t *testing.T) {
	dir := t.TempDir()

	// 1. Missing file -> StatusWarning
	res := CheckSQLiteIntegrity("missing", filepath.Join(dir, "nonexistent.sqlite"))
	if res.Status != StatusWarning {
		t.Errorf("expected WARN for missing file, got %v", res.Status)
	}

	// 2. Valid SQLite database -> StatusOK
	validDB := filepath.Join(dir, "valid.sqlite")
	db, err := sql.Open("sqlite", validDB)
	if err != nil {
		t.Fatalf("failed to open sqlite: %v", err)
	}
	if _, err := db.Exec("CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT); INSERT INTO t VALUES (1, 'ok');"); err != nil {
		db.Close()
		t.Fatalf("failed to init db: %v", err)
	}
	db.Close()

	res = CheckSQLiteIntegrity("valid", validDB)
	if res.Status != StatusOK {
		t.Errorf("expected OK for valid db, got %v: %s", res.Status, res.Message)
	}

	// 3. Corrupt file -> StatusFail
	corruptDB := filepath.Join(dir, "corrupt.sqlite")
	if err := os.WriteFile(corruptDB, []byte("SQLite format 3\x00this is pure corrupted garbage content"), 0o600); err != nil {
		t.Fatalf("failed to write corrupt file: %v", err)
	}
	res = CheckSQLiteIntegrity("corrupt", corruptDB)
	if res.Status != StatusFail {
		t.Errorf("expected FAIL for corrupt db, got %v: %s", res.Status, res.Message)
	}
}

func TestCheckClockSkew(t *testing.T) {
	now := time.Now()

	// 1. In sync
	res := CheckClockSkew(now.Add(1*time.Second), 5*time.Second)
	if res.Status != StatusOK {
		t.Errorf("expected OK, got %v: %s", res.Status, res.Message)
	}

	// 2. Skewed
	res = CheckClockSkew(now.Add(-10*time.Second), 5*time.Second)
	if res.Status != StatusWarning {
		t.Errorf("expected WARN, got %v: %s", res.Status, res.Message)
	}
}

func TestDiagnoseServer(t *testing.T) {
	dataDir := t.TempDir()

	// Initial empty directory: CA not generated yet
	rep := DiagnoseServer(context.Background(), dataDir)
	if rep.Healthy {
		t.Errorf("expected empty server directory to report unhealthy")
	}

	// Initialize CA and databases
	ca, err := identity.LoadOrGenerateCA(dataDir)
	if err != nil {
		t.Fatalf("failed to generate CA: %v", err)
	}
	if ca == nil {
		t.Fatal("nil CA")
	}

	// Create tables in databases
	taskStore, err := taskstore.Open(filepath.Join(dataDir, "tasks.sqlite"))
	if err != nil {
		t.Fatalf("failed to create task store: %v", err)
	}
	taskStore.Close()

	devStore, err := devicestore.Open(filepath.Join(dataDir, "devices.sqlite"))
	if err != nil {
		t.Fatalf("failed to create dev store: %v", err)
	}
	devStore.Close()

	polStore, err := policy.Open(filepath.Join(dataDir, "policy.sqlite"))
	if err != nil {
		t.Fatalf("failed to create policy store: %v", err)
	}
	polStore.Close()

	// Diagnose again: should now be healthy
	rep2 := DiagnoseServer(context.Background(), dataDir)
	if !rep2.Healthy {
		for _, c := range rep2.Checks {
			if c.Status == StatusFail {
				t.Errorf("check %s failed: %s", c.Name, c.Message)
			}
		}
	}
}

func TestDiagnoseNode(t *testing.T) {
	serverDir := t.TempDir()
	ca, err := identity.LoadOrGenerateCA(serverDir)
	if err != nil {
		t.Fatalf("failed to generate CA: %v", err)
	}

	// Start test server with mTLS
	ts, err := taskstore.Open(filepath.Join(serverDir, "tasks.sqlite"))
	if err != nil {
		t.Fatalf("taskstore: %v", err)
	}
	defer ts.Close()

	srv := httpapi.NewServer(ts, ca)
	serverTLSConfig, err := srv.BuildTLSConfig([]string{"localhost", "127.0.0.1"})
	if err != nil {
		t.Fatalf("failed to build tls config: %v", err)
	}

	httptestServer := httptest.NewUnstartedServer(srv.Handler())
	httptestServer.TLS = serverTLSConfig
	httptestServer.EnableHTTP2 = true
	httptestServer.StartTLS()
	defer httptestServer.Close()

	// Pair node to generate valid state and certificates
	nodeDir := t.TempDir()
	invitation, err := ca.GenerateInvitation(5 * time.Minute)
	if err != nil {
		t.Fatalf("generate invitation: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := node.Pair(ctx, node.PairConfig{
		ServerURL:       httptestServer.URL,
		InvitationToken: invitation.Token,
		CACertPEM:       ca.CACertPEM(),
		Dir:             nodeDir,
	}); err != nil {
		t.Fatalf("pair node: %v", err)
	}

	// Run DiagnoseNode
	rep := DiagnoseNode(context.Background(), nodeDir)
	if !rep.Healthy {
		for _, c := range rep.Checks {
			if c.Status == StatusFail {
				t.Errorf("check %s failed: %s (remediation: %s)", c.Name, c.Message, c.Remediation)
			}
		}
	}
}
