package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"agent-gateway/internal/node"
)

func parseNodeFlags(args []string) (dir string, configPath string, err error) {
	fs := flag.NewFlagSet("node-lifecycle", flag.ContinueOnError)
	defDir := defaultNodeDir()
	defConfig := envString("MESH_NODE_CONFIG", "")
	if defConfig == "" {
		defConfig = filepath.Join(defDir, "node.json")
	}
	fDir := fs.String("dir", envString("MESH_NODE_DIR", defDir), "node directory holding credentials and outbox")
	fConfig := fs.String("config", defConfig, "node configuration file")
	if err := fs.Parse(args); err != nil {
		return "", "", err
	}
	nodeDir := *fDir
	cfgPath := *fConfig
	if cfgPath == "" {
		cfgPath = filepath.Join(nodeDir, "node.json")
	}
	return nodeDir, cfgPath, nil
}

func readPID(pidFile string) (int, error) {
	data, err := os.ReadFile(pidFile)
	if err != nil {
		return 0, err
	}
	pidStr := strings.TrimSpace(string(data))
	return strconv.Atoi(pidStr)
}

func runNodeStart(args []string) error {
	nodeDir, configPath, err := parseNodeFlags(args)
	if err != nil {
		return err
	}

	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		return fmt.Errorf("未找到节点配置文件：%s\n该机器尚未与网关配对。请在网关管理控制台复制一键命令，或手动执行配对：\n  mesh pair --server <网关地址> --token <邀请码>", configPath)
	}

	pidFile := filepath.Join(nodeDir, "node.pid")
	logFile := filepath.Join(nodeDir, "node.log")

	if pid, err := readPID(pidFile); err == nil && pid > 0 {
		if isProcessAlive(pid) {
			fmt.Printf("● 节点已在后台运行中 (PID %d)\n", pid)
			fmt.Printf("  运行日志: %s\n", logFile)
			fmt.Printf("  管理命令: mesh node status | mesh node stop | mesh node restart\n")
			return nil
		}
		_ = os.Remove(pidFile)
	}

	_ = os.MkdirAll(nodeDir, 0o755)

	logHandle, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("打开日志文件失败: %w", err)
	}
	defer logHandle.Close()

	execPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("定位可执行文件路径失败: %w", err)
	}

	cmd := exec.Command(execPath, "node", "run", "--dir", nodeDir, "--config", configPath)
	cmd.Stdout = logHandle
	cmd.Stderr = logHandle
	setSysProcAttr(cmd)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动后台节点进程失败: %w", err)
	}

	pid := cmd.Process.Pid
	_ = os.WriteFile(pidFile, []byte(strconv.Itoa(pid)), 0o644)

	// 等待 300ms 检测进程是否异常闪退
	time.Sleep(300 * time.Millisecond)
	if !isProcessAlive(pid) {
		_ = os.Remove(pidFile)
		// 读取最后几行日志告知用户原因
		tailLogs := readTailLines(logFile, 5)
		return fmt.Errorf("节点启动后异常退出，最新日志：\n%s", tailLogs)
	}

	fmt.Printf("✓ 节点已成功在后台启动 (PID %d)\n", pid)
	fmt.Printf("  节点目录: %s\n", nodeDir)
	fmt.Printf("  配置文件: %s\n", configPath)
	fmt.Printf("  运行日志: %s\n", logFile)
	fmt.Println("常用命令：")
	fmt.Println("  mesh node status    # 查看节点状态与最新日志")
	fmt.Println("  mesh node stop      # 停止后台节点")
	fmt.Println("  mesh node restart   # 重启后台节点")
	fmt.Println("  mesh node reload    # 重载节点配置")
	return nil
}

func runNodeStop(args []string) error {
	nodeDir, _, err := parseNodeFlags(args)
	if err != nil {
		return err
	}

	pidFile := filepath.Join(nodeDir, "node.pid")
	pid, err := readPID(pidFile)
	if err != nil || pid <= 0 {
		fmt.Printf("○ 节点未在运行 (未找到 PID 文件: %s)\n", pidFile)
		return nil
	}

	if !isProcessAlive(pid) {
		_ = os.Remove(pidFile)
		fmt.Printf("○ 节点未在运行 (清理失效 PID %d)\n", pid)
		return nil
	}

	_ = killProcess(pid)

	// 最多等待 5 秒等待优雅退出
	stopped := false
	for i := 0; i < 25; i++ {
		time.Sleep(200 * time.Millisecond)
		if !isProcessAlive(pid) {
			stopped = true
			break
		}
	}

	if !stopped {
		_ = forceKillProcess(pid)
	}

	_ = os.Remove(pidFile)
	fmt.Printf("✓ 节点已成功停止 (PID %d)\n", pid)
	return nil
}

func runNodeRestart(args []string) error {
	nodeDir, _, _ := parseNodeFlags(args)
	pidFile := filepath.Join(nodeDir, "node.pid")
	if pid, err := readPID(pidFile); err == nil && pid > 0 && isProcessAlive(pid) {
		fmt.Println("正在停止已有节点进程...")
		_ = runNodeStop(args)
		time.Sleep(500 * time.Millisecond)
	}
	return runNodeStart(args)
}

func runNodeStatus(args []string) error {
	nodeDir, configPath, err := parseNodeFlags(args)
	if err != nil {
		return err
	}

	pidFile := filepath.Join(nodeDir, "node.pid")
	logFile := filepath.Join(nodeDir, "node.log")

	pid, err := readPID(pidFile)
	running := false
	if err == nil && pid > 0 {
		running = isProcessAlive(pid)
	}

	fmt.Println("==================================================")
	fmt.Println("             Agent Gateway 节点状态               ")
	fmt.Println("==================================================")

	if running {
		fmt.Printf("● 运行状态: [RUNNING] 正常运行中 (PID: %d)\n", pid)
	} else {
		fmt.Printf("○ 运行状态: [STOPPED] 未运行\n")
	}

	fmt.Printf("  节点目录: %s\n", nodeDir)
	fmt.Printf("  配置文件: %s\n", configPath)
	fmt.Printf("  运行日志: %s\n", logFile)

	if cfg, err := node.LoadConfig(configPath); err == nil {
		fmt.Printf("  网关地址: %s\n", cfg.ServerURL)
		var caps []string
		for _, c := range cfg.Capabilities {
			caps = append(caps, c.Name)
		}
		if len(caps) > 0 {
			fmt.Printf("  承接能力: %s\n", strings.Join(caps, ", "))
		}
	} else {
		fmt.Printf("  配置校验: 未配对或配置损坏 (%v)\n", err)
	}

	if _, err := os.Stat(logFile); err == nil {
		fmt.Println("\n--- 最新日志摘要 (最后 5 行) ---")
		tail := readTailLines(logFile, 5)
		if tail != "" {
			fmt.Print(tail)
		} else {
			fmt.Println("(日志为空)")
		}
	}
	fmt.Println("==================================================")
	if !running {
		fmt.Println("提示: 运行 'mesh node start' 可启动后台节点服务。")
	}
	return nil
}

func runNodeReload(args []string) error {
	nodeDir, configPath, err := parseNodeFlags(args)
	if err != nil {
		return err
	}

	// 先预先校验配置文件是否正确，防止错误配置中断运行
	if _, err := node.LoadConfig(configPath); err != nil {
		return fmt.Errorf("重载失败：配置文件校验错误: %w", err)
	}

	pidFile := filepath.Join(nodeDir, "node.pid")
	pid, err := readPID(pidFile)
	if err != nil || pid <= 0 || !isProcessAlive(pid) {
		return fmt.Errorf("节点未在运行，无法重载配置。请使用 'mesh node start' 启动节点。")
	}

	// 尝试发送信号
	if err := reloadProcess(pid); err != nil {
		// 若平台不支持 SIGHUP 信号（如 Windows），进行平滑重启
		fmt.Println("当前平台不支持动态信号重载，正在执行平滑重启...")
		return runNodeRestart(args)
	}

	fmt.Printf("✓ 节点 (PID %d) 配置重载信号已下发。\n", pid)
	return nil
}

func readTailLines(path string, count int) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	var lines []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
		if len(lines) > count*2 {
			lines = lines[len(lines)-count:]
		}
	}
	if len(lines) > count {
		lines = lines[len(lines)-count:]
	}
	if len(lines) == 0 {
		return ""
	}
	var sb strings.Builder
	for _, l := range lines {
		sb.WriteString("  ")
		sb.WriteString(l)
		sb.WriteString("\n")
	}
	return sb.String()
}
