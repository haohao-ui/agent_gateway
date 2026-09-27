package main

import (
	"agent-gateway/internal/policy"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestH7RestartDoesNotPrintSecrets(t *testing.T) {
	t.Setenv("MESH_ADMIN_PASSWORD", "")
	t.Setenv("GATEWAY_ADMIN_PASSWORD", "")
	dir := t.TempDir()
	for range 2 {
		output, err := os.CreateTemp(t.TempDir(), "startup-")
		if err != nil {
			t.Fatal(err)
		}
		stdout, stderr := os.Stdout, os.Stderr
		os.Stdout, os.Stderr = output, output
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		done := make(chan error, 1)
		go func() {
			done <- runServer(ctx, []string{"--addr", "127.0.0.1:0", "--http-addr", "127.0.0.1:0", "--data-dir", dir, "--invitations", "0"})
		}()
		ready := false
		for ctx.Err() == nil {
			data, _ := os.ReadFile(output.Name())
			if bytes.Contains(data, []byte("mcp (HTTPS)")) {
				ready = true
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
		err = <-done
		os.Stdout, os.Stderr = stdout, stderr
		output.Close()
		if err != nil || !ready {
			t.Fatalf("temporary startup failed: %v", err)
		}
		data, err := os.ReadFile(output.Name())
		if err != nil {
			t.Fatal(err)
		}
		for _, file := range []string{"admin.password", "admin.token"} {
			secret, err := os.ReadFile(filepath.Join(dir, file))
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(data, bytes.TrimSpace(secret)) {
				t.Fatalf("startup leaked %s", file)
			}
		}
		if bytes.Contains(data, []byte("?token=")) {
			t.Fatal("startup emitted query token")
		}
	}
}
func TestReleaseCLIKeygenSignVerify(t *testing.T) {
	dir := t.TempDir()
	ctx := t.Context()
	if err := runRelease(ctx, []string{"keygen", "--dir", dir}); err != nil {
		t.Fatal(err)
	}
	if err := runRelease(ctx, []string{"keygen", "--dir", dir}); err == nil {
		t.Fatal("overwrote signing key")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	target := "mesh-" + runtime.GOOS + "-" + runtime.GOARCH
	if runtime.GOOS == "windows" {
		target += ".exe"
	}
	binary := filepath.Join(dir, target)
	if err := os.WriteFile(binary, raw, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"LICENSE", "NOTICE", "THIRD_PARTY_NOTICES"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("test notice"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := runRelease(ctx, []string{"sign", "--dir", dir, "--key", filepath.Join(dir, "release.key"), "--version", "test-1", "--sequence", "1"}); err != nil {
		t.Fatal(err)
	}
	args := []string{"verify", "--dir", dir, "--public-key", filepath.Join(dir, "release.pub")}
	if err := runRelease(ctx, args); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binary, []byte(strings.Repeat("tampered", 100)), 0700); err != nil {
		t.Fatal(err)
	}
	if err := runRelease(ctx, args); err == nil {
		t.Fatal("tampered binary verified")
	}
}

func TestH7CredentialIssueRefusesOverwriteWithoutOrphan(t *testing.T) {
	dir := t.TempDir()
	st, err := policy.Open(filepath.Join(dir, "policy.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	out := filepath.Join(dir, "token")
	if err := os.WriteFile(out, []byte("existing"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := runCredential(t.Context(), []string{"issue", "--data-dir", dir, "--out", out}); err == nil {
		t.Fatal("overwrote token file")
	}
	items, err := st.Credentials(t.Context(), "", 100)
	if err != nil || len(items) != 0 {
		t.Fatal("failed issue left live credential")
	}
	if err := runCredential(t.Context(), []string{"issue", "--data-dir", dir, "--ttl", "8760h"}); err == nil {
		t.Fatal("long credential allowed")
	}
	if err := runCredential(t.Context(), []string{"revoke", "--data-dir", filepath.Join(dir, "missing"), "principal"}); err == nil {
		t.Fatal("unexpected credential store fallback")
	}
}
