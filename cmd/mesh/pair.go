package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"agent-gateway/internal/node"
)

// runPair enrols this machine with a gateway.
//
// The gateway is trusted through the CA certificate the operator passes with
// --ca, never through the connection itself: that file is what the operator
// takes from the gateway machine, so it is the only trust anchor the node has.
func runPair(ctx context.Context, args []string) error {
	cmd := newCommand("pair", "Enrol this machine with a gateway. The gateway CA must be passed out of band with --ca.")
	server := cmd.flags.String("server", envOrDefault([]string{"MESH_SERVER_URL", "MESH_SERVER"}, ""), "gateway base URL, for example https://127.0.0.1:8443")
	token := cmd.flags.String("token", envOrDefault([]string{"MESH_PAIR_TOKEN", "MESH_INVITATION_TOKEN", "MESH_TOKEN"}, ""), "one-time invitation token printed by 'mesh server' or 'mesh invite'; use - to read it from stdin")
	caFile := cmd.flags.String("ca", envOrDefault([]string{"MESH_CA_FILE", "MESH_CA"}, ""), "path to the gateway CA certificate to trust")
	dir := cmd.flags.String("dir", envString("MESH_NODE_DIR", "./node"), "directory that will hold this machine's identity")
	writeConfig := cmd.flags.Bool("write-config", true, "write a starter node.json if the directory has none")
	if err := cmd.flags.Parse(args); err != nil {
		return err
	}

	if *server == "" || *token == "" || *caFile == "" {
		return errors.New("--server, --token and --ca are required")
	}
	invitationToken, err := resolveToken(*token, os.Stdin)
	if err != nil {
		return err
	}
	if _, err := cmd.logger(); err != nil {
		return err
	}

	caPEM, err := os.ReadFile(*caFile)
	if err != nil {
		return fmt.Errorf("read the gateway CA certificate: %w", err)
	}

	state, err := node.Pair(ctx, node.PairConfig{
		ServerURL:       *server,
		InvitationToken: invitationToken,
		CACertPEM:       caPEM,
		Dir:             *dir,
	})
	if err != nil {
		return err
	}

	fmt.Printf("paired as %s\n", state.NodeID)
	fmt.Printf("identity:  %s\n", *dir)

	if *writeConfig {
		configPath := filepath.Join(*dir, "node.json")
		if _, err := os.Stat(configPath); errors.Is(err, os.ErrNotExist) {
			if err := writeStarterConfig(configPath, *server); err != nil {
				return err
			}
			fmt.Printf("wrote:     %s\n", configPath)
			fmt.Println("\nEdit the adapter executable in that file before running 'mesh node':")
			fmt.Println("  the configured command is what executes a task's prompt, and it is never")
			fmt.Println("  taken from the network.")
		}
	}
	return nil
}

// resolveToken accepts the invitation either as a flag value or on standard
// input. Standard input is the better path when pairing over ssh or a script:
// an argument shows up in the process list and in shell history.
func resolveToken(flagValue string, stdin io.Reader) (string, error) {
	if flagValue != "-" {
		return flagValue, nil
	}
	raw, err := io.ReadAll(io.LimitReader(stdin, 4096))
	if err != nil {
		return "", fmt.Errorf("read the invitation token from stdin: %w", err)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", errors.New("no invitation token on stdin")
	}
	return token, nil
}

// starterConfig is a valid, minimal node configuration. The adapter points at
// /bin/echo so that the file is syntactically complete, but the operator has to
// replace it with the real agent command before any task can be served.
type starterConfig struct {
	ServerURL    string `json:"server_url"`
	CertFile     string `json:"cert_file"`
	KeyFile      string `json:"key_file"`
	CAFile       string `json:"ca_file"`
	LeaseSeconds int    `json:"lease_seconds"`
	RenewSeconds int    `json:"renew_seconds"`
	Capabilities []struct {
		Name    string `json:"name"`
		Version int    `json:"version"`
		Adapter struct {
			Executable string   `json:"executable"`
			Args       []string `json:"args"`
		} `json:"adapter"`
	} `json:"capabilities"`
}

func writeStarterConfig(path, serverURL string) error {
	cfg := starterConfig{
		ServerURL:    serverURL,
		CertFile:     node.NodeCertFile,
		KeyFile:      node.NodeKeyFile,
		CAFile:       node.NodeCAFile,
		LeaseSeconds: node.DefaultLeaseSeconds,
		RenewSeconds: node.DefaultRenewSeconds,
	}
	capability := struct {
		Name    string `json:"name"`
		Version int    `json:"version"`
		Adapter struct {
			Executable string   `json:"executable"`
			Args       []string `json:"args"`
		} `json:"adapter"`
	}{Name: "agent.run", Version: 1}
	capability.Adapter.Executable = "/bin/echo"
	capability.Adapter.Args = []string{"{instruction}"}
	cfg.Capabilities = append(cfg.Capabilities, capability)

	encoded, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("encode the starter configuration: %w", err)
	}
	if err := os.WriteFile(path, append(encoded, '\n'), 0o600); err != nil {
		return fmt.Errorf("write the starter configuration: %w", err)
	}
	return nil
}
