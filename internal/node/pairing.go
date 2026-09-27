package node

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agent-gateway/internal/identity"
	"agent-gateway/internal/protocol"
)

// File names written into a node directory by Pair.
const (
	NodeCertFile = "node.crt"
	NodeKeyFile  = "node.key"
	NodeCAFile   = "ca.crt"
	stateFile    = "state.json"
)

// PairConfig is one enrollment attempt.
type PairConfig struct {
	ServerURL       string
	InvitationToken string
	// CACertPEM is the gateway CA the node trusts. It must come from the
	// operator out of band (the gateway prints it at startup); the node never
	// learns its trust anchor from the network it is about to trust.
	CACertPEM []byte
	// Dir receives the key, certificate and CA copy. It must not already hold a
	// node key.
	Dir     string
	Timeout time.Duration
}

// PairState is the small record written next to the credentials.
type PairState struct {
	NodeID    string    `json:"node_id"`
	ServerURL string    `json:"server_url"`
	PairedAt  time.Time `json:"paired_at"`
}

// Pair generates a local key and CSR, exchanges them for a certificate over
// server-authenticated TLS, and stores the identity in Dir.
func Pair(ctx context.Context, cfg PairConfig) (PairState, error) {
	if cfg.ServerURL == "" || cfg.InvitationToken == "" {
		return PairState{}, fmt.Errorf("%w: server url and invitation token are required", protocol.ErrInvalid)
	}
	if len(cfg.CACertPEM) == 0 {
		return PairState{}, fmt.Errorf("%w: the gateway CA certificate is required", protocol.ErrInvalid)
	}
	if cfg.Dir == "" {
		return PairState{}, fmt.Errorf("%w: node directory is required", protocol.ErrInvalid)
	}
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		return PairState{}, fmt.Errorf("create node directory: %w", err)
	}

	// Re-pairing over an existing identity would silently break every task the
	// old certificate is holding; make the operator remove it first.
	keyPath := filepath.Join(cfg.Dir, NodeKeyFile)
	if _, err := os.Stat(keyPath); err == nil {
		return PairState{}, fmt.Errorf("%w: %s already exists, remove the node directory to pair again",
			protocol.ErrConflict, keyPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return PairState{}, fmt.Errorf("inspect node directory: %w", err)
	}

	trustedCA, err := parseCACertificate(cfg.CACertPEM)
	if err != nil {
		return PairState{}, err
	}

	keyPEM, csrPEM, err := identity.GenerateNodeKeyAndCSR()
	if err != nil {
		return PairState{}, err
	}

	tlsConfig, err := identity.BuildBootstrapTLSConfig(cfg.CACertPEM)
	if err != nil {
		return PairState{}, err
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	client := newTLSClient(tlsConfig, timeout)

	body, err := json.Marshal(protocol.PairRequest{
		InvitationToken: cfg.InvitationToken,
		CSRPEM:          string(csrPEM),
		MachineID:       protocol.GetMachineID(),
		Hostname:        protocol.GetDeviceName(),
	})
	if err != nil {
		return PairState{}, fmt.Errorf("encode pair request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(cfg.ServerURL, "/")+"/v1/pair", bytes.NewReader(body))
	if err != nil {
		return PairState{}, fmt.Errorf("build pair request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return PairState{}, fmt.Errorf("pair with gateway: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var payload protocol.ErrorResponse
		_ = json.NewDecoder(resp.Body).Decode(&payload)
		if resp.StatusCode == http.StatusUnauthorized {
			return PairState{}, fmt.Errorf("%w: the invitation was rejected: %s", ErrUnauthorized, payload.Message)
		}
		return PairState{}, fmt.Errorf("%w: pairing failed with status %d: %s", ErrRejected, resp.StatusCode, payload.Message)
	}

	var pairResp protocol.PairResponse
	if err := json.NewDecoder(resp.Body).Decode(&pairResp); err != nil {
		return PairState{}, fmt.Errorf("decode pair response: %w", err)
	}
	if pairResp.NodeID == "" || pairResp.CertPEM == "" {
		return PairState{}, fmt.Errorf("%w: gateway returned an incomplete pairing response", ErrRejected)
	}

	// The gateway must be operating the CA the operator decided to trust. A
	// response naming a different CA means the connection was not the gateway
	// the operator verified.
	issuedCA, err := parseCACertificate([]byte(pairResp.CACertPEM))
	if err != nil {
		return PairState{}, err
	}
	if !bytes.Equal(issuedCA.Raw, trustedCA.Raw) {
		return PairState{}, fmt.Errorf("%w: gateway answered with a different CA than the trusted one", ErrUnauthorized)
	}

	// Write the credentials before anything can use them.
	if err := writeFile(keyPath, keyPEM); err != nil {
		return PairState{}, err
	}
	if err := writeFile(filepath.Join(cfg.Dir, NodeCertFile), []byte(pairResp.CertPEM)); err != nil {
		return PairState{}, err
	}
	if err := writeFile(filepath.Join(cfg.Dir, NodeCAFile), cfg.CACertPEM); err != nil {
		return PairState{}, err
	}

	state := PairState{NodeID: pairResp.NodeID, ServerURL: cfg.ServerURL, PairedAt: time.Now().UTC()}
	encoded, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return PairState{}, fmt.Errorf("encode node state: %w", err)
	}
	if err := writeFile(filepath.Join(cfg.Dir, stateFile), append(encoded, '\n')); err != nil {
		return PairState{}, err
	}
	return state, nil
}

// LoadState reads the record written by Pair.
func LoadState(dir string) (PairState, error) {
	raw, err := os.ReadFile(filepath.Join(dir, stateFile))
	if err != nil {
		return PairState{}, fmt.Errorf("read node state: %w", err)
	}
	var state PairState
	if err := json.Unmarshal(raw, &state); err != nil {
		return PairState{}, fmt.Errorf("parse node state: %w", err)
	}
	return state, nil
}

func parseCACertificate(certPEM []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("%w: CA certificate is not valid PEM", protocol.ErrInvalid)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%w: parse CA certificate: %v", protocol.ErrInvalid, err)
	}
	return cert, nil
}

// writeFile publishes a private file atomically: the data is flushed to disk
// before the rename, so a crash never leaves a truncated credential or result
// under the final name.
func writeFile(path string, data []byte) error {
	tmp := path + ".tmp"
	file, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("create %s: %w", tmp, err)
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return fmt.Errorf("flush %s: %w", tmp, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("publish %s: %w", path, err)
	}
	return nil
}
