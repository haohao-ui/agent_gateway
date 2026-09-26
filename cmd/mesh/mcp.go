package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"agent-gateway/internal/mcp"
)

func runMCP(ctx context.Context, args []string) error {
	cmd := newCommand("mcp", "Run Model Context Protocol (MCP) server over standard I/O for Claude Desktop, Cursor and Cline.")
	serverURL := cmd.flags.String("server", "", "gateway base URL, e.g. https://127.0.0.1:8443 (remote mode)")
	token := cmd.flags.String("token", "", "operator token for authenticating to gateway server (remote mode)")
	caFile := cmd.flags.String("ca", "", "path to gateway CA certificate to trust (remote mode)")
	dataDir := cmd.flags.String("data-dir", "./gateway-data", "path to gateway data directory (when running in direct local mode)")

	if err := cmd.flags.Parse(args); err != nil {
		return err
	}

	var backend mcp.GatewayBackend

	if *serverURL != "" {
		if *token == "" {
			return errors.New("--token is required when --server is specified")
		}
		var caPEM []byte
		if *caFile != "" {
			var err error
			caPEM, err = os.ReadFile(*caFile)
			if err != nil {
				return fmt.Errorf("read CA certificate: %w", err)
			}
		}
		client, err := mcp.NewClientBackend(*serverURL, *token, caPEM)
		if err != nil {
			return fmt.Errorf("initialize MCP client backend: %w", err)
		}
		backend = client
	} else {
		local, cleanup, err := mcp.OpenLocalBackend(*dataDir)
		if err != nil {
			return fmt.Errorf("initialize MCP local backend from %s: %w", *dataDir, err)
		}
		defer cleanup()
		backend = local
	}

	return mcp.RunStdio(ctx, backend)
}
