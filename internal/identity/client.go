package identity

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
)

// GenerateNodeKeyAndCSR generates an ECDSA keypair and CSR for the node.
func GenerateNodeKeyAndCSR() (keyPEM []byte, csrPEM []byte, err error) {
	privKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate node key: %w", err)
	}

	template := &x509.CertificateRequest{
		Subject: pkix.Name{
			Organization: []string{"Agent Gateway Node Candidate"},
		},
	}

	csrDER, err := x509.CreateCertificateRequest(rand.Reader, template, privKey)
	if err != nil {
		return nil, nil, fmt.Errorf("create CSR: %w", err)
	}

	keyDER, err := x509.MarshalECPrivateKey(privKey)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal node key: %w", err)
	}

	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	csrPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})
	return keyPEM, csrPEM, nil
}

// BuildClientTLSConfig constructs an mTLS tls.Config with HTTP/2 enabled (NextProtos: []string{"h2", "http/1.1"}).
func BuildClientTLSConfig(caCertPEM, certPEM, keyPEM []byte) (*tls.Config, error) {
	caCertPool := x509.NewCertPool()
	if ok := caCertPool.AppendCertsFromPEM(caCertPEM); !ok {
		return nil, errors.New("failed to append CA certificate to pool")
	}

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("parse client cert/key pair: %w", err)
	}

	return &tls.Config{
		RootCAs:      caCertPool,
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{"h2", "http/1.1"},
		MinVersion:   tls.VersionTLS13,
	}, nil
}

// BuildBootstrapTLSConfig constructs a single-direction TLS config verifying only the CA.
func BuildBootstrapTLSConfig(caCertPEM []byte) (*tls.Config, error) {
	caCertPool := x509.NewCertPool()
	if ok := caCertPool.AppendCertsFromPEM(caCertPEM); !ok {
		return nil, errors.New("failed to append CA certificate to pool")
	}

	return &tls.Config{
		RootCAs:    caCertPool,
		NextProtos: []string{"h2", "http/1.1"},
		MinVersion: tls.VersionTLS13,
	}, nil
}
