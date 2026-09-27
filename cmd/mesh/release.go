package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"agent-gateway/internal/release"
)

func writeExclusive(path string, b []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err = f.Write(b); err != nil {
		return err
	}
	return f.Sync()
}
func runRelease(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: mesh release keygen|sign|verify|fetch")
	}
	cmd := newCommand("release "+args[0], "Offline signed releases. Public key/verifier must be provisioned independently of the download server.")
	dir := cmd.flags.String("dir", ".", "release directory (keygen/sign/verify)")
	keyPath := cmd.flags.String("key", "", "offline private key PEM (sign only)")
	pubPath := cmd.flags.String("public-key", "", "independently trusted Ed25519 public key PEM")
	version := cmd.flags.String("version", "", "release version")
	sequence := cmd.flags.Uint64("sequence", 0, "monotonically increasing release sequence")
	ttl := cmd.flags.Duration("ttl", 30*24*time.Hour, "manifest validity")
	server := cmd.flags.String("server", "", "HTTPS download origin")
	ca := cmd.flags.String("ca", "", "trusted TLS CA PEM (optional)")
	target := cmd.flags.String("target", "", "target platform (fetch defaults to host; verify defaults to all files)")
	out := cmd.flags.String("out", "", "new destination file; refuses overwrite")
	if err := cmd.flags.Parse(args[1:]); err != nil {
		return err
	}
	switch args[0] {
	case "keygen":
		for _, name := range []string{"release.key", "release.pub"} {
			if _, err := os.Lstat(filepath.Join(*dir, name)); !os.IsNotExist(err) {
				return errors.New("key destination exists or is inaccessible")
			}
		}
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return err
		}
		private, err := x509.MarshalPKCS8PrivateKey(priv)
		if err != nil {
			return err
		}
		public, err := x509.MarshalPKIXPublicKey(pub)
		if err != nil {
			return err
		}
		if err := writeExclusive(filepath.Join(*dir, "release.key"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private}), 0600); err != nil {
			return err
		}
		return writeExclusive(filepath.Join(*dir, "release.pub"), pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: public}), 0644)
	case "sign":
		if *version == "" || *sequence == 0 || *ttl <= 0 || *ttl > 90*24*time.Hour {
			return errors.New("version, positive sequence and ttl <= 90 days required")
		}
		raw, err := os.ReadFile(*keyPath)
		if err != nil {
			return err
		}
		b, _ := pem.Decode(raw)
		if b == nil {
			return errors.New("invalid private key PEM")
		}
		parsed, err := x509.ParsePKCS8PrivateKey(b.Bytes)
		if err != nil {
			return err
		}
		key, ok := parsed.(ed25519.PrivateKey)
		if !ok {
			return errors.New("Ed25519 key required")
		}
		m := release.Manifest{Schema: 1, Version: *version, Sequence: *sequence, ExpiresAt: time.Now().UTC().Add(*ttl), Files: make(map[string]release.File)}
		for target, name := range release.Targets {
			path := filepath.Join(*dir, name)
			if _, err := os.Stat(path); os.IsNotExist(err) {
				continue
			}
			if err := release.CheckPlatform(path, target); err != nil {
				return err
			}
			f, err := release.Digest(path)
			if err != nil {
				return err
			}
			m.Files[target] = f
		}
		for _, name := range release.NoticeFiles {
			f, err := release.Digest(filepath.Join(*dir, name))
			if err != nil {
				return err
			}
			m.Files[name] = f
		}
		raw, err = json.MarshalIndent(m, "", "  ")
		if err != nil {
			return err
		}
		sig := ed25519.Sign(key, raw)
		if _, err := release.VerifyManifest(raw, sig, key.Public().(ed25519.PublicKey), time.Now(), 0); err != nil {
			return err
		}
		if err := writeExclusive(filepath.Join(*dir, "manifest.json"), raw, 0644); err != nil {
			return err
		}
		return writeExclusive(filepath.Join(*dir, "manifest.sig"), sig, 0644)
	case "verify":
		pub, err := release.ReadPublicKey(*pubPath)
		if err != nil {
			return err
		}
		raw, err := os.ReadFile(filepath.Join(*dir, "manifest.json"))
		if err != nil {
			return err
		}
		sig, err := os.ReadFile(filepath.Join(*dir, "manifest.sig"))
		if err != nil {
			return err
		}
		m, err := release.VerifyManifest(raw, sig, pub, time.Now(), *sequence)
		if err != nil {
			return err
		}
		if *target != "" {
			if _, ok := release.Targets[*target]; !ok {
				return errors.New("invalid target")
			}
			if _, ok := m.Files[*target]; !ok {
				return errors.New("target absent from manifest")
			}
		}
		for entryTarget, f := range m.Files {
			if _, binary := release.Targets[entryTarget]; binary && *target != "" && entryTarget != *target {
				continue
			}
			path := filepath.Join(*dir, f.Name)
			if err := release.VerifyFile(path, f); err != nil {
				return err
			}
			if _, ok := release.Targets[entryTarget]; ok {
				if err := release.CheckPlatform(path, entryTarget); err != nil {
					return err
				}
			}
		}
		fmt.Printf("verified release %s (sequence %d)\n", m.Version, m.Sequence)
		return nil
	case "fetch":
		if *target == "" {
			*target = runtime.GOOS + "-" + runtime.GOARCH
		}
		if *out == "" {
			return errors.New("--out required")
		}
		if _, err := os.Lstat(*out); !os.IsNotExist(err) {
			return errors.New("destination exists or is inaccessible")
		}
		pub, err := release.ReadPublicKey(*pubPath)
		if err != nil {
			return err
		}
		tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
		if *ca != "" {
			raw, err := os.ReadFile(*ca)
			if err != nil {
				return err
			}
			roots := x509.NewCertPool()
			if !roots.AppendCertsFromPEM(raw) {
				return errors.New("invalid TLS CA")
			}
			tlsConfig.RootCAs = roots
		}
		transport := &http.Transport{TLSClientConfig: tlsConfig}
		defer transport.CloseIdleConnections()
		path, m, err := release.Fetch(ctx, &http.Client{Transport: transport}, *server, pub, *target, *sequence, *out)
		if err != nil {
			return err
		}
		defer os.Remove(path)
		if err := os.Link(path, *out); err != nil {
			return err
		}
		fmt.Printf("verified download %s (sequence %d)\n", m.Version, m.Sequence)
		return nil
	default:
		return errors.New("unknown release command")
	}
}
