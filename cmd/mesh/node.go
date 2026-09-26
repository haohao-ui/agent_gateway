package main

import (
	"context"
	"fmt"
	"path/filepath"

	"agent-gateway/internal/node"
)

func runNode(ctx context.Context, args []string) error {
	cmd := newCommand("node", "Run the task loop: claim work, execute it locally and report the result.")
	configPath := cmd.flags.String("config", "./node/node.json", "node configuration file written by 'mesh pair'")
	dir := cmd.flags.String("dir", "", "node directory holding the journal and outbox (default: the configuration file's directory)")
	if err := cmd.flags.Parse(args); err != nil {
		return err
	}

	log, err := cmd.logger()
	if err != nil {
		return err
	}

	config, err := node.LoadConfig(*configPath)
	if err != nil {
		return err
	}

	nodeDir := *dir
	if nodeDir == "" {
		absolute, err := filepath.Abs(*configPath)
		if err != nil {
			return fmt.Errorf("resolve the configuration path: %w", err)
		}
		nodeDir = filepath.Dir(absolute)
	}

	worker, err := node.New(config, nodeDir, log)
	if err != nil {
		return err
	}
	defer worker.Close()

	return worker.Run(ctx)
}
