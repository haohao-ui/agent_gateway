package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"agent-gateway/internal/node"
)

func defaultNodeDir() string {
	if d := os.Getenv("MESH_NODE_DIR"); d != "" {
		return d
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".agent-mesh-node")
	}
	return ".agent-mesh-node"
}

func runNode(ctx context.Context, args []string) error {
	cmd := newCommand("node", "Run the task loop: claim work, execute it locally and report the result.")
	defDir := defaultNodeDir()
	defConfig := envString("MESH_NODE_CONFIG", "")
	if defConfig == "" {
		defConfig = filepath.Join(defDir, "node.json")
	}
	configPath := cmd.flags.String("config", defConfig, "node configuration file (default: ~/.agent-mesh-node/node.json)")
	dir := cmd.flags.String("dir", envString("MESH_NODE_DIR", defDir), "node directory holding the journal and outbox (default: ~/.agent-mesh-node)")
	if err := cmd.flags.Parse(args); err != nil {
		return err
	}

	log, err := cmd.logger()
	if err != nil {
		return err
	}

	if _, err := os.Stat(*configPath); os.IsNotExist(err) {
		return fmt.Errorf("未找到节点配置文件：%s\n该机器尚未与网关配对。请在网关管理控制台复制一键命令，或手动执行配对：\n  mesh pair --server <网关地址> --token <邀请码>", *configPath)
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
