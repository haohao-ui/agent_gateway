package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agent-gateway/internal/artifact"
)

func runFile(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return errors.New("file command requires an action: upload, download, list, or delete")
	}

	action := args[0]
	subArgs := args[1:]

	switch action {
	case "upload":
		return runFileUpload(ctx, subArgs)
	case "download":
		return runFileDownload(ctx, subArgs)
	case "list":
		return runFileList(ctx, subArgs)
	case "delete":
		return runFileDelete(ctx, subArgs)
	default:
		return fmt.Errorf("unknown file action %q; valid actions are: upload, download, list, delete", action)
	}
}

func fileHTTPClient(caFile string) (*http.Client, error) {
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS13}
	if caFile != "" {
		caPEM, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("read CA certificate: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			return nil, errors.New("failed to parse gateway CA certificate")
		}
		tlsConfig.RootCAs = pool
	}
	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig:   tlsConfig,
			ForceAttemptHTTP2: true,
		},
		Timeout: 60 * time.Second,
	}, nil
}

func runFileUpload(ctx context.Context, args []string) error {
	cmd := newCommand("file upload", "Upload a local file as an artifact associated with a task.")
	serverURL := cmd.flags.String("server", "", "gateway base URL, e.g. https://127.0.0.1:8443")
	token := cmd.flags.String("token", "", "operator token for authenticating to the gateway")
	caFile := cmd.flags.String("ca", "", "path to gateway CA certificate")
	remoteName := cmd.flags.String("name", "", "remote filename (defaults to local file base name)")
	dataDir := cmd.flags.String("data-dir", "./gateway-data", "path to gateway data directory for direct local mode")

	if err := cmd.flags.Parse(args); err != nil {
		return err
	}
	tail := cmd.flags.Args()
	if len(tail) < 2 {
		return errors.New("usage: mesh file upload <task-id> <local-path> [flags]")
	}
	taskID := tail[0]
	localPath := tail[1]

	filename := *remoteName
	if filename == "" {
		filename = filepath.Base(localPath)
	}

	content, err := os.ReadFile(localPath)
	if err != nil {
		return fmt.Errorf("read local file: %w", err)
	}

	hasher := sha256.New()
	hasher.Write(content)
	checksum := hex.EncodeToString(hasher.Sum(nil))

	if *serverURL != "" {
		client, err := fileHTTPClient(*caFile)
		if err != nil {
			return err
		}
		url := fmt.Sprintf("%s/v1/artifacts/%s/%s", strings.TrimRight(*serverURL, "/"), taskID, filename)
		req, err := http.NewRequestWithContext(ctx, "PUT", url, bytes.NewReader(content))
		if err != nil {
			return fmt.Errorf("create upload request: %w", err)
		}
		if *token != "" {
			req.Header.Set("Authorization", "Bearer "+*token)
		}
		req.Header.Set("X-Checksum-SHA256", checksum)

		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("upload: %w", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusCreated {
			body, _ := io.ReadAll(resp.Body)
			return fmt.Errorf("upload rejected (HTTP %d): %s", resp.StatusCode, string(body))
		}
		fmt.Printf("Artifact %s uploaded successfully (SHA-256: %s, %d bytes)\n", filename, checksum, len(content))
		return nil
	}

	// Local mode
	store, err := artifact.Open(filepath.Join(*dataDir, "artifacts"))
	if err != nil {
		return fmt.Errorf("open local artifact store: %w", err)
	}
	defer store.Close()

	info, err := store.Save(ctx, taskID, filename, bytes.NewReader(content), checksum)
	if err != nil {
		return fmt.Errorf("save local artifact: %w", err)
	}
	fmt.Printf("Artifact %s stored locally (SHA-256: %s, %d bytes)\n", info.Name, info.SHA256, info.Size)
	return nil
}

func runFileDownload(ctx context.Context, args []string) error {
	cmd := newCommand("file download", "Download a task artifact to local filesystem.")
	serverURL := cmd.flags.String("server", "", "gateway base URL, e.g. https://127.0.0.1:8443")
	token := cmd.flags.String("token", "", "operator token for authenticating to the gateway")
	caFile := cmd.flags.String("ca", "", "path to gateway CA certificate")
	outputPath := cmd.flags.String("output", "", "output file path (defaults to remote filename in current directory)")
	dataDir := cmd.flags.String("data-dir", "./gateway-data", "path to gateway data directory for direct local mode")

	if err := cmd.flags.Parse(args); err != nil {
		return err
	}
	tail := cmd.flags.Args()
	if len(tail) < 2 {
		return errors.New("usage: mesh file download <task-id> <remote-filename> [flags]")
	}
	taskID := tail[0]
	filename := tail[1]

	destPath := *outputPath
	if destPath == "" {
		destPath = filename
	}

	if *serverURL != "" {
		client, err := fileHTTPClient(*caFile)
		if err != nil {
			return err
		}
		url := fmt.Sprintf("%s/v1/artifacts/%s/%s", strings.TrimRight(*serverURL, "/"), taskID, filename)
		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			return fmt.Errorf("create download request: %w", err)
		}
		if *token != "" {
			req.Header.Set("Authorization", "Bearer "+*token)
		}

		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("download: %w", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			return fmt.Errorf("download failed (HTTP %d): %s", resp.StatusCode, string(body))
		}

		out, err := os.Create(destPath)
		if err != nil {
			return fmt.Errorf("create output file: %w", err)
		}
		defer out.Close()

		written, err := io.Copy(out, resp.Body)
		if err != nil {
			return fmt.Errorf("write output file: %w", err)
		}
		fmt.Printf("Downloaded %s to %s (%d bytes)\n", filename, destPath, written)
		return nil
	}

	// Local mode
	store, err := artifact.Open(filepath.Join(*dataDir, "artifacts"))
	if err != nil {
		return fmt.Errorf("open local artifact store: %w", err)
	}
	defer store.Close()

	file, info, err := store.Open(ctx, taskID, filename)
	if err != nil {
		return fmt.Errorf("open local artifact: %w", err)
	}
	defer file.Close()

	out, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("create destination file: %w", err)
	}
	defer out.Close()

	written, err := io.Copy(out, file)
	if err != nil {
		return fmt.Errorf("copy artifact content: %w", err)
	}
	fmt.Printf("Copied %s to %s (%d bytes, updated %s)\n", info.Name, destPath, written, info.UpdatedAt.Format(time.RFC3339))
	return nil
}

func runFileList(ctx context.Context, args []string) error {
	cmd := newCommand("file list", "List all artifacts associated with a task.")
	serverURL := cmd.flags.String("server", "", "gateway base URL, e.g. https://127.0.0.1:8443")
	token := cmd.flags.String("token", "", "operator token for authenticating to the gateway")
	caFile := cmd.flags.String("ca", "", "path to gateway CA certificate")
	jsonOutput := cmd.flags.Bool("json", false, "output results in JSON format")
	dataDir := cmd.flags.String("data-dir", "./gateway-data", "path to gateway data directory for direct local mode")

	if err := cmd.flags.Parse(args); err != nil {
		return err
	}
	tail := cmd.flags.Args()
	if len(tail) < 1 {
		return errors.New("usage: mesh file list <task-id> [flags]")
	}
	taskID := tail[0]

	var items []artifact.FileInfo

	if *serverURL != "" {
		client, err := fileHTTPClient(*caFile)
		if err != nil {
			return err
		}
		url := fmt.Sprintf("%s/v1/artifacts/%s", strings.TrimRight(*serverURL, "/"), taskID)
		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			return fmt.Errorf("create list request: %w", err)
		}
		if *token != "" {
			req.Header.Set("Authorization", "Bearer "+*token)
		}

		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("list: %w", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			return fmt.Errorf("list failed (HTTP %d): %s", resp.StatusCode, string(body))
		}
		if err := json.NewDecoder(resp.Body).Decode(&items); err != nil {
			return fmt.Errorf("decode list: %w", err)
		}
	} else {
		store, err := artifact.Open(filepath.Join(*dataDir, "artifacts"))
		if err != nil {
			return fmt.Errorf("open local artifact store: %w", err)
		}
		defer store.Close()

		items, err = store.List(ctx, taskID)
		if err != nil {
			return fmt.Errorf("list local artifacts: %w", err)
		}
	}

	if *jsonOutput {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(items)
	}

	fmt.Printf("Artifacts for Task %s (%d total):\n", taskID, len(items))
	if len(items) == 0 {
		fmt.Println("  (no artifacts found)")
		return nil
	}
	fmt.Printf("%-24s %-12s %s\n", "FILENAME", "SIZE", "UPDATED AT")
	for _, it := range items {
		fmt.Printf("%-24s %-12d %s\n", it.Name, it.Size, it.UpdatedAt.Format(time.RFC3339))
	}
	return nil
}

func runFileDelete(ctx context.Context, args []string) error {
	cmd := newCommand("file delete", "Delete a task artifact.")
	serverURL := cmd.flags.String("server", "", "gateway base URL, e.g. https://127.0.0.1:8443")
	token := cmd.flags.String("token", "", "operator token for authenticating to the gateway")
	caFile := cmd.flags.String("ca", "", "path to gateway CA certificate")
	dataDir := cmd.flags.String("data-dir", "./gateway-data", "path to gateway data directory for direct local mode")

	if err := cmd.flags.Parse(args); err != nil {
		return err
	}
	tail := cmd.flags.Args()
	if len(tail) < 2 {
		return errors.New("usage: mesh file delete <task-id> <filename> [flags]")
	}
	taskID := tail[0]
	filename := tail[1]

	if *serverURL != "" {
		client, err := fileHTTPClient(*caFile)
		if err != nil {
			return err
		}
		url := fmt.Sprintf("%s/v1/artifacts/%s/%s", strings.TrimRight(*serverURL, "/"), taskID, filename)
		req, err := http.NewRequestWithContext(ctx, "DELETE", url, nil)
		if err != nil {
			return fmt.Errorf("create delete request: %w", err)
		}
		if *token != "" {
			req.Header.Set("Authorization", "Bearer "+*token)
		}

		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("delete: %w", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			return fmt.Errorf("delete failed (HTTP %d): %s", resp.StatusCode, string(body))
		}
		fmt.Printf("Artifact %s deleted successfully\n", filename)
		return nil
	}

	store, err := artifact.Open(filepath.Join(*dataDir, "artifacts"))
	if err != nil {
		return fmt.Errorf("open local artifact store: %w", err)
	}
	defer store.Close()

	if err := store.Delete(ctx, taskID, filename); err != nil {
		return fmt.Errorf("delete local artifact: %w", err)
	}
	fmt.Printf("Local artifact %s deleted successfully\n", filename)
	return nil
}
