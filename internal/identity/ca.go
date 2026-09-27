package identity

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"agent-gateway/internal/protocol"
)

const (
	caCertFilename = "ca.crt"
	caKeyFilename  = "ca.key"

	defaultCATTL    = 10 * 365 * 24 * time.Hour // 10 years
	serverCertTTL   = 365 * 24 * time.Hour      // 1 year
	nodeCertTTL     = 90 * 24 * time.Hour       // 90 days
	spiffeURIPrefix = "spiffe://agent-gateway/nodes/"
)

// CA manages root certificate authority, server certificates, and node enrollment.
type CA struct {
	mu          sync.Mutex
	caCert      *x509.Certificate
	caCertPEM   []byte
	caKey       *ecdsa.PrivateKey
	certPool    *x509.CertPool
	fingerprint string

	// Invitations are stored on disk so a gateway can hand out a new one
	// without being restarted.
	invitations *InvitationStore
}

// LoadOrGenerateCA loads the root CA from dir or generates a new one.
func LoadOrGenerateCA(dataDir string) (*CA, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create ca data dir: %w", err)
	}

	certPath := filepath.Join(dataDir, caCertFilename)
	keyPath := filepath.Join(dataDir, caKeyFilename)

	certBytes, certErr := os.ReadFile(certPath)
	keyBytes, keyErr := os.ReadFile(keyPath)

	invitations, err := OpenInvitationStore(InvitationDir(dataDir))
	if err != nil {
		return nil, err
	}

	if certErr == nil && keyErr == nil {
		return loadCA(certBytes, keyBytes, invitations)
	}

	return generateCA(certPath, keyPath, invitations)
}

func loadCA(certPEM, keyPEM []byte, invitations *InvitationStore) (*CA, error) {
	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil {
		return nil, errors.New("invalid CA certificate PEM")
	}
	caCert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse CA certificate: %w", err)
	}

	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, errors.New("invalid CA private key PEM")
	}
	privKey, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse CA private key: %w", err)
	}

	pool := x509.NewCertPool()
	pool.AddCert(caCert)

	return &CA{
		caCert:      caCert,
		caCertPEM:   certPEM,
		caKey:       privKey,
		certPool:    pool,
		fingerprint: fingerprintOf(caCert),
		invitations: invitations,
	}, nil
}

func generateCA(certPath, keyPath string, invitations *InvitationStore) (*CA, error) {
	privKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate CA key: %w", err)
	}

	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("generate serial number: %w", err)
	}

	template := &x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{"Agent Gateway Root CA"},
			CommonName:   "Agent Gateway CA",
		},
		NotBefore:             time.Now().Add(-1 * time.Minute),
		NotAfter:              time.Now().Add(defaultCATTL),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, template, template, &privKey.PublicKey, privKey)
	if err != nil {
		return nil, fmt.Errorf("create CA certificate: %w", err)
	}

	caCert, err := x509.ParseCertificate(derBytes)
	if err != nil {
		return nil, fmt.Errorf("parse generated CA cert: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes})
	keyBytes, err := x509.MarshalECPrivateKey(privKey)
	if err != nil {
		return nil, fmt.Errorf("marshal CA private key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes})

	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		return nil, fmt.Errorf("save CA certificate: %w", err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return nil, fmt.Errorf("save CA private key: %w", err)
	}

	pool := x509.NewCertPool()
	pool.AddCert(caCert)

	return &CA{
		caCert:      caCert,
		caCertPEM:   certPEM,
		caKey:       privKey,
		certPool:    pool,
		fingerprint: fingerprintOf(caCert),
		invitations: invitations,
	}, nil
}

// CACertPEM returns the PEM representation of the root CA certificate.
func (c *CA) CACertPEM() []byte {
	return c.caCertPEM
}

// Fingerprint returns the hex SHA-256 fingerprint of the CA certificate.
func (c *CA) Fingerprint() string {
	return c.fingerprint
}

// CertPool returns a pool containing only this CA.
func (c *CA) CertPool() *x509.CertPool {
	return c.certPool
}

// GenerateServerCertificate creates a TLS server certificate signed by this CA.
func (c *CA) GenerateServerCertificate(hosts []string) (tls.Certificate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	privKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generate server key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, err
	}

	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			Organization: []string{"Agent Gateway"},
			CommonName:   "gateway.local",
		},
		NotBefore:   time.Now().Add(-1 * time.Minute),
		NotAfter:    time.Now().Add(serverCertTTL),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}

	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			template.IPAddresses = append(template.IPAddresses, ip)
		} else {
			template.DNSNames = append(template.DNSNames, h)
		}
	}
	// Always include localhost loopbacks
	template.IPAddresses = append(template.IPAddresses, net.IPv4(127, 0, 0, 1), net.IPv6loopback)
	template.DNSNames = append(template.DNSNames, "localhost")

	derBytes, err := x509.CreateCertificate(rand.Reader, template, c.caCert, &privKey.PublicKey, c.caKey)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("sign server certificate: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes})
	keyBytes, err := x509.MarshalECPrivateKey(privKey)
	if err != nil {
		return tls.Certificate{}, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes})

	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("build server tls certificate: %w", err)
	}
	return tlsCert, nil
}

// GenerateInvitation creates a one-time invitation token valid for ttl. The
// record is written to disk, so the invitation outlives this process and an
// offline command can mint one for a gateway that is already running.
func (c *CA) GenerateInvitation(ttl time.Duration) (protocol.PairInvitation, error) {
	return c.invitations.Issue(ttl, c.fingerprint)
}

// GenerateInvitationWithUses creates an invitation token with custom usage limits.
func (c *CA) GenerateInvitationWithUses(ttl time.Duration, maxUses int) (protocol.PairInvitation, error) {
	return c.invitations.IssueWithUses(ttl, maxUses, c.fingerprint)
}

// PendingInvitations counts invitations that can still be used.
func (c *CA) PendingInvitations() (int, error) {
	return c.invitations.Pending()
}

// SignNodeCSR validates the invitation and signs the node's CSR, returning the PEM cert and NodeID.
func (c *CA) SignNodeCSR(invitationToken string, csrPEM []byte) (nodeID string, certPEM []byte, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// The invitation is consumed before the CSR is examined, so a rejected CSR
	// cannot be retried as an oracle for the token.
	if err := c.invitations.Consume(invitationToken); err != nil {
		return "", nil, err
	}

	block, _ := pem.Decode(csrPEM)
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return "", nil, fmt.Errorf("%w: invalid CSR PEM", protocol.ErrInvalid)
	}

	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return "", nil, fmt.Errorf("%w: parse CSR: %v", protocol.ErrInvalid, err)
	}

	if err := csr.CheckSignature(); err != nil {
		return "", nil, fmt.Errorf("%w: CSR signature verification failed: %v", protocol.ErrInvalid, err)
	}

	// The key type is part of what the gateway agrees to issue. Only the curve
	// the node side generates (ECDSA P-256) is accepted, so an unsupported or
	// weaker key cannot be smuggled in through the CSR's public key.
	if pub, ok := csr.PublicKey.(*ecdsa.PublicKey); !ok || pub.Curve != elliptic.P256() {
		return "", nil, fmt.Errorf("%w: unsupported CSR public key, want ECDSA P-256", protocol.ErrInvalid)
	}

	// Server allocates node ID
	rawID := make([]byte, 8)
	if _, err := rand.Read(rawID); err != nil {
		return "", nil, err
	}
	nodeID = "node-" + hex.EncodeToString(rawID)

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return "", nil, err
	}

	uri, _ := url.Parse(spiffeURIPrefix + nodeID)
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			Organization: []string{"Agent Gateway Nodes"},
			CommonName:   nodeID,
		},
		NotBefore:             time.Now().Add(-1 * time.Minute),
		NotAfter:              time.Now().Add(nodeCertTTL),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		URIs:                  []*url.URL{uri},
		BasicConstraintsValid: true,
		IsCA:                  false,
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, template, c.caCert, csr.PublicKey, c.caKey)
	if err != nil {
		return "", nil, fmt.Errorf("create node certificate: %w", err)
	}

	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes})
	return nodeID, certPEM, nil
}

// ExtractNodeID extracts and verifies the Node ID from verified client certificate URI SAN.
func ExtractNodeID(certs []*x509.Certificate) (string, error) {
	if len(certs) == 0 {
		return "", fmt.Errorf("%w: missing client certificate", protocol.ErrUnauthorized)
	}
	cert := certs[0]
	for _, u := range cert.URIs {
		if strings.HasPrefix(u.String(), spiffeURIPrefix) {
			id := strings.TrimPrefix(u.String(), spiffeURIPrefix)
			if id != "" {
				return id, nil
			}
		}
	}
	// Fall back to CommonName if URI SAN is missing
	if cert.Subject.CommonName != "" {
		return cert.Subject.CommonName, nil
	}
	return "", fmt.Errorf("%w: client certificate missing NodeID identity", protocol.ErrUnauthorized)
}

func hashToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}
