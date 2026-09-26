package identity

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"testing"
	"time"

	"agent-gateway/internal/protocol"
)

func TestCA_LifecycleAndCertSigning(t *testing.T) {
	dataDir := t.TempDir()

	ca, err := LoadOrGenerateCA(dataDir)
	if err != nil {
		t.Fatalf("create CA: %v", err)
	}

	// Verify fingerprint exists
	if ca.Fingerprint() == "" {
		t.Fatal("empty fingerprint")
	}

	// Reload CA from disk
	ca2, err := LoadOrGenerateCA(dataDir)
	if err != nil {
		t.Fatalf("reload CA: %v", err)
	}
	if ca.Fingerprint() != ca2.Fingerprint() {
		t.Fatal("fingerprint mismatch after reload")
	}

	// Server certificate generation
	serverCert, err := ca.GenerateServerCertificate([]string{"gateway.example.com", "10.0.0.1"})
	if err != nil {
		t.Fatalf("generate server cert: %v", err)
	}
	if len(serverCert.Certificate) == 0 {
		t.Fatal("missing certificate in tls.Certificate")
	}

	// Verify server cert with CA pool
	parsedServerCert, err := x509.ParseCertificate(serverCert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	opts := x509.VerifyOptions{
		Roots:   ca.CertPool(),
		DNSName: "gateway.example.com",
	}
	if _, err := parsedServerCert.Verify(opts); err != nil {
		t.Fatalf("server cert verification failed: %v", err)
	}

	// Invitation generation
	inv, err := ca.GenerateInvitation(10 * time.Minute)
	if err != nil {
		t.Fatalf("generate invitation: %v", err)
	}
	if inv.Token == "" {
		t.Fatal("empty invitation token")
	}

	// Generate node key & CSR
	keyPEM, csrPEM, err := GenerateNodeKeyAndCSR()
	if err != nil {
		t.Fatalf("generate node CSR: %v", err)
	}

	// Sign node CSR
	nodeID, certPEM, err := ca.SignNodeCSR(inv.Token, csrPEM)
	if err != nil {
		t.Fatalf("sign node CSR: %v", err)
	}
	if nodeID == "" {
		t.Fatal("empty node ID")
	}

	// Verify one-time use (second attempt must fail)
	_, _, err = ca.SignNodeCSR(inv.Token, csrPEM)
	if !errors.Is(err, protocol.ErrUnauthorized) {
		t.Fatalf("expected ErrUnauthorized for reused token, got %v", err)
	}

	// Verify client TLS config build
	clientTLS, err := BuildClientTLSConfig(ca.CACertPEM(), certPEM, keyPEM)
	if err != nil {
		t.Fatalf("build client TLS config: %v", err)
	}
	if len(clientTLS.Certificates) == 0 {
		t.Fatal("missing client certs")
	}

	// Parse client cert and test ExtractNodeID
	parsedClientCert, err := x509.ParseCertificate(clientTLS.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	extractedNodeID, err := ExtractNodeID([]*x509.Certificate{parsedClientCert})
	if err != nil {
		t.Fatalf("extract node ID: %v", err)
	}
	if extractedNodeID != nodeID {
		t.Fatalf("node ID mismatch: got %q, want %q", extractedNodeID, nodeID)
	}
}

func TestCA_RejectsUnsupportedCSRKeyTypes(t *testing.T) {
	ca, err := LoadOrGenerateCA(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		key  any
	}{
		{name: "rsa", key: mustRSAKey(t)},
		{name: "p384", key: mustECKey(t, elliptic.P384())},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inv, err := ca.GenerateInvitation(5 * time.Minute)
			if err != nil {
				t.Fatal(err)
			}

			csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, tc.key)
			if err != nil {
				t.Fatalf("create CSR: %v", err)
			}
			csrPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})

			if _, _, err := ca.SignNodeCSR(inv.Token, csrPEM); !errors.Is(err, protocol.ErrInvalid) {
				t.Fatalf("expected ErrInvalid for a %s CSR, got %v", tc.name, err)
			}

			// The invitation is consumed before the CSR is examined, so a
			// rejected CSR cannot be retried as an oracle for the token.
			if _, _, err := ca.SignNodeCSR(inv.Token, csrPEM); !errors.Is(err, protocol.ErrUnauthorized) {
				t.Fatalf("expected the invitation to stay consumed, got %v", err)
			}
		})
	}
}

func mustRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	return key
}

func mustECKey(t *testing.T, curve elliptic.Curve) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(curve, rand.Reader)
	if err != nil {
		t.Fatalf("generate EC key: %v", err)
	}
	return key
}

func TestCA_ExpiredInvitation(t *testing.T) {
	ca, err := LoadOrGenerateCA(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	inv, err := ca.GenerateInvitation(-1 * time.Second)
	if err != nil {
		t.Fatal(err)
	}

	_, csrPEM, err := GenerateNodeKeyAndCSR()
	if err != nil {
		t.Fatal(err)
	}

	_, _, err = ca.SignNodeCSR(inv.Token, csrPEM)
	if !errors.Is(err, protocol.ErrUnauthorized) {
		t.Fatalf("expected ErrUnauthorized for expired token, got %v", err)
	}
}
