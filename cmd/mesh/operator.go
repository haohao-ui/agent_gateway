package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agent-gateway/internal/devicestore"
	"agent-gateway/internal/identity"
	"agent-gateway/internal/policy"
	"agent-gateway/internal/protocol"
)

func resolvePolicyStoreDir(specifiedDir string) (string, error) {
	candidates := []string{}
	if specifiedDir != "" {
		candidates = append(candidates, specifiedDir)
	}
	if def := defaultServerDataDir(); def != "" && def != specifiedDir {
		candidates = append(candidates, def)
	}
	candidates = append(candidates, "./gateway-data")
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		candidates = append(candidates, filepath.Join(home, ".agent-mesh", "gateway-data"))
		candidates = append(candidates, filepath.Join(home, ".agent-mesh-server"))
	}

	seen := make(map[string]bool)
	for _, dir := range candidates {
		clean := filepath.Clean(dir)
		if seen[clean] {
			continue
		}
		seen[clean] = true
		if _, err := os.Stat(filepath.Join(clean, "policy.sqlite")); err == nil {
			return clean, nil
		}
	}
	return specifiedDir, fmt.Errorf("open gateway credential store: policy.sqlite not found in %s (also checked %s). Please start 'mesh server' first or specify --data-dir", specifiedDir, strings.Join(candidates[1:], ", "))
}

func runCredential(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: mesh credential issue|revoke [--data-dir DIR] [flags]")
	}
	cmd := newCommand("credential "+args[0], "Trusted local credential administration. Requires access to the gateway data directory.")
	dir := cmd.flags.String("data-dir", envString("MESH_DATA_DIR", defaultServerDataDir()), "gateway data directory")
	role := cmd.flags.String("role", "admin", "admin, operator, or viewer")
	nodes := cmd.flags.String("nodes", "", "comma separated node scopes (empty for admin)")
	ttl := cmd.flags.Duration("ttl", 24*time.Hour, "credential lifetime (maximum 24 hours)")
	output := cmd.flags.String("out", "", "new token file (optional; prints to stdout if omitted)")
	force := cmd.flags.Bool("force", false, "overwrite token file if it already exists")
	if err := cmd.flags.Parse(args[1:]); err != nil {
		return err
	}
	if args[0] != "issue" && args[0] != "revoke" {
		return errors.New("unknown credential verb")
	}
	if args[0] == "issue" && cmd.flags.NArg() != 0 {
		return errors.New("issue takes no positional arguments")
	}
	if args[0] == "revoke" && cmd.flags.NArg() != 1 {
		return errors.New("revoke requires one principal ID")
	}

	dataDir, err := resolvePolicyStoreDir(*dir)
	if err != nil {
		return err
	}

	store, err := policy.Open(filepath.Join(dataDir, "policy.sqlite"))
	if err != nil {
		return err
	}
	defer store.Close()
	if args[0] == "revoke" {
		return store.Revoke(ctx, cmd.flags.Arg(0))
	}
	if *ttl <= 0 || *ttl > 24*time.Hour {
		return errors.New("credential lifetime must be within 24 hours")
	}

	r := strings.ToLower(strings.TrimSpace(*role))
	if (r == "operator" || r == "viewer") && strings.TrimSpace(*nodes) == "" {
		return fmt.Errorf("role %q requires at least one node scope via --nodes <node1,node2...> (or use --role admin for global cluster access)", *role)
	}

	expiresAt := time.Now().Add(*ttl)
	if *output == "" {
		p, token, err := store.Issue(ctx, policy.Role(*role), splitList(*nodes), expiresAt)
		if err != nil {
			return err
		}
		fmt.Printf("principal: %s\nrole:      %s\nexpires:   %s\ntoken:     %s\n", p.ID, p.Role, expiresAt.Format(time.RFC3339), token)
		return nil
	}

	outDir := filepath.Dir(*output)
	if outDir != "" && outDir != "." {
		if err := os.MkdirAll(outDir, 0o755); err != nil {
			return fmt.Errorf("create token directory: %w", err)
		}
	}

	openFlags := os.O_WRONLY | os.O_CREATE
	if *force {
		openFlags |= os.O_TRUNC
	} else {
		openFlags |= os.O_EXCL
	}

	file, err := os.OpenFile(*output, openFlags, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("token file %s already exists (use --force to overwrite)", *output)
		}
		return err
	}
	ok := false
	defer func() {
		file.Close()
		if !ok && !*force {
			os.Remove(*output)
		}
	}()
	p, token, err := store.Issue(ctx, policy.Role(*role), splitList(*nodes), expiresAt)
	if err != nil {
		return err
	}
	if _, err = file.WriteString(token + "\n"); err == nil {
		err = file.Sync()
	}
	if err == nil {
		err = file.Close()
	}
	if err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = store.Revoke(cleanupCtx, p.ID)
		return errors.New("token file write failed; credential revoked")
	}
	ok = true
	fmt.Printf("principal: %s\nrole:      %s\nexpires:   %s\ntoken file: %s\n", p.ID, p.Role, expiresAt.Format(time.RFC3339), *output)
	return nil
}

type operatorClient struct {
	client      *http.Client
	base, token string
}

func newOperatorClient(server, caFile, tokenFile string) (*operatorClient, error) {
	u, err := url.Parse(server)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("--server must be an HTTPS origin")
	}
	if caFile == "" || tokenFile == "" {
		return nil, errors.New("--ca and --token-file are required")
	}
	ca, err := os.ReadFile(caFile)
	if err != nil {
		return nil, err
	}
	config, err := identity.BuildBootstrapTLSConfig(ca)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(tokenFile)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("token file must be regular")
	}
	b, err := io.ReadAll(io.LimitReader(f, 1025))
	if err != nil {
		return nil, err
	}
	token := strings.TrimSpace(string(b))
	if len(b) > 1024 || token == "" || strings.ContainsAny(token, " \t\r\n") {
		return nil, errors.New("invalid token file")
	}
	return &operatorClient{client: &http.Client{Transport: &http.Transport{TLSClientConfig: config, ForceAttemptHTTP2: true}, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, base: strings.TrimRight(server, "/"), token: token}, nil
}
func (c *operatorClient) request(ctx context.Context, method, path string, in any, out any) error {
	var body bytes.Buffer
	if in != nil {
		if err := json.NewEncoder(&body).Encode(in); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, &body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	res, err := c.client.Do(req)
	if err != nil {
		return errors.New("operator request failed")
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("operator request rejected: HTTP %d", res.StatusCode)
	}
	if out != nil {
		return json.NewDecoder(io.LimitReader(res.Body, 512<<10)).Decode(out)
	}
	return nil
}
func operatorTaskCommand(ctx context.Context, verb, server, ca, token, nodeID, id string, in any) error {
	c, err := newOperatorClient(server, ca, token)
	if err != nil {
		return err
	}
	defer c.client.CloseIdleConnections()
	var task protocol.Task
	switch verb {
	case "submit":
		sub, ok := in.(protocol.SubmitRequest)
		if !ok || nodeID == "" {
			return errors.New("operator submit requires --node-id")
		}
		sub.NodeID = nodeID
		err = c.request(ctx, "POST", "/v1/operator/tasks/submit", sub, &task)
	case "get":
		err = c.request(ctx, "GET", "/v1/operator/tasks/"+url.PathEscape(id), nil, &task)
	case "cancel":
		err = c.request(ctx, "POST", "/v1/operator/tasks/"+url.PathEscape(id)+"/cancel", nil, &task)
	case "requeue":
		err = c.request(ctx, "POST", "/v1/operator/tasks/"+url.PathEscape(id)+"/requeue", nil, &task)
	case "resolve":
		err = c.request(ctx, "POST", "/v1/operator/tasks/"+url.PathEscape(id)+"/resolve", in, &task)
	case "list":
		var tasks []protocol.Task
		path := "/v1/operator/tasks"
		if id != "" {
			path += "?state=" + url.QueryEscape(id)
		}
		if err = c.request(ctx, "GET", path, nil, &tasks); err != nil {
			return err
		}
		encoded, err := json.MarshalIndent(tasks, "", "  ")
		if err != nil {
			return err
		}
		fmt.Printf("%s\n", encoded)
		return nil
	default:
		return fmt.Errorf("unknown operator task verb %q", verb)
	}
	if err != nil {
		return err
	}
	return printTask(task)
}
func runDevice(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: mesh device list|revoke [flags]")
	}
	switch args[0] {
	case "list":
		return runDeviceList(ctx, args[1:])
	case "revoke":
		return runDeviceRevoke(ctx, args[1:])
	default:
		return fmt.Errorf("unknown device verb %q (use list or revoke)", args[0])
	}
}

func runDeviceRevoke(ctx context.Context, args []string) error {
	cmd := newCommand("device revoke", "Revoke a node certificate using an administrator credential.")
	server := cmd.flags.String("server", envOrDefault([]string{"MESH_SERVER_URL", "MESH_SERVER"}, ""), "HTTPS gateway origin")
	ca := cmd.flags.String("ca", envOrDefault([]string{"MESH_CA_FILE", "MESH_CA"}, ""), "trusted CA file")
	token := cmd.flags.String("token-file", envString("MESH_TOKEN_FILE", ""), "operator token file")
	reason := cmd.flags.String("reason", "", "audit reason")
	if err := cmd.flags.Parse(args[1:]); err != nil {
		return err
	}
	if cmd.flags.NArg() != 1 || strings.TrimSpace(*reason) == "" {
		return errors.New("node ID and --reason required")
	}
	c, err := newOperatorClient(*server, *ca, *token)
	if err != nil {
		return err
	}
	defer c.client.CloseIdleConnections()
	return c.request(ctx, "POST", "/v1/operator/devices/"+url.PathEscape(cmd.flags.Arg(0))+"/revoke", struct {
		Reason string `json:"reason"`
	}{*reason}, nil)
}

func runDeviceList(ctx context.Context, args []string) error {
	cmd := newCommand("device list", "List registered devices (operator credential required).")
	server := cmd.flags.String("server", envOrDefault([]string{"MESH_SERVER_URL", "MESH_SERVER"}, ""), "HTTPS gateway origin")
	ca := cmd.flags.String("ca", envOrDefault([]string{"MESH_CA_FILE", "MESH_CA"}, ""), "trusted CA file")
	token := cmd.flags.String("token-file", envString("MESH_TOKEN_FILE", ""), "operator token file")
	if err := cmd.flags.Parse(args); err != nil {
		return err
	}
	c, err := newOperatorClient(*server, *ca, *token)
	if err != nil {
		return err
	}
	defer c.client.CloseIdleConnections()
	var devices []devicestore.Device
	if err := c.request(ctx, "GET", "/v1/operator/devices", nil, &devices); err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(devices, "", "  ")
	if err != nil {
		return err
	}
	fmt.Printf("%s\n", encoded)
	return nil
}
