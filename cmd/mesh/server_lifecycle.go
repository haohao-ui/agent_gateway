package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"agent-gateway/internal/policy"
)

func defaultServerDataDir() string {
	if d := os.Getenv("MESH_DATA_DIR"); d != "" {
		return d
	}
	if _, err := os.Stat("./gateway-data"); err == nil {
		return "./gateway-data"
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".agent-mesh", "gateway-data")
	}
	return "./gateway-data"
}

func parseServerLifecycleFlags(args []string) (dataDir string, addr string, err error) {
	fs := flag.NewFlagSet("server-lifecycle", flag.ContinueOnError)
	defData := defaultServerDataDir()
	fData := fs.String("data-dir", envString("MESH_DATA_DIR", defData), "gateway data directory")
	fAddr := fs.String("addr", envOrDefault([]string{"MESH_SERVER_ADDR", "MESH_ADDR"}, defaultServerAddr), "listen address")
	if err := fs.Parse(args); err != nil {
		return "", "", err
	}
	return *fData, *fAddr, nil
}

func runServerStart(args []string) error {
	dataDir, addr, err := parseServerLifecycleFlags(args)
	if err != nil {
		return err
	}

	_ = os.MkdirAll(dataDir, 0o755)
	pidFile := filepath.Join(dataDir, "server.pid")
	logFile := filepath.Join(dataDir, "server.log")

	if pid, err := readPID(pidFile); err == nil && pid > 0 {
		if isProcessAlive(pid) {
			fmt.Printf("● 网关服务已在后台运行中 (PID %d)\n", pid)
			fmt.Printf("  运行日志: %s\n", logFile)
			fmt.Printf("  管理命令: mesh server status | mesh server stop | mesh server restart\n")
			return nil
		}
		_ = os.Remove(pidFile)
	}

	logHandle, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("打开网关日志文件失败: %w", err)
	}
	defer logHandle.Close()

	execPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("定位可执行文件路径失败: %w", err)
	}

	cmdArgs := []string{"server", "run", "--data-dir", dataDir, "--addr", addr}
	cmdArgs = append(cmdArgs, args...)

	cmd := exec.Command(execPath, cmdArgs...)
	cmd.Stdout = logHandle
	cmd.Stderr = logHandle
	setSysProcAttr(cmd)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动后台网关进程失败: %w", err)
	}

	pid := cmd.Process.Pid
	_ = os.WriteFile(pidFile, []byte(strconv.Itoa(pid)), 0o644)

	// 等待 500ms 检测进程是否启动成功
	time.Sleep(500 * time.Millisecond)
	if !isProcessAlive(pid) {
		_ = os.Remove(pidFile)
		tailLogs := readTailLines(logFile, 5)
		return fmt.Errorf("网关启动后异常退出，最新日志：\n%s", tailLogs)
	}

	fmt.Printf("✓ 网关服务已成功在后台启动 (PID %d)\n", pid)
	fmt.Printf("  数据目录: %s\n", dataDir)
	fmt.Printf("  监听地址: %s\n", addr)
	fmt.Printf("  运行日志: %s\n", logFile)
	fmt.Println("常用命令：")
	fmt.Println("  mesh server status    # 查看网关运行状态与最新日志")
	fmt.Println("  mesh server stop      # 停止后台网关服务")
	fmt.Println("  mesh server restart   # 重启后台网关服务")
	fmt.Println("  mesh server reload    # 重载网关服务配置")
	return nil
}

func runServerStop(args []string) error {
	dataDir, _, err := parseServerLifecycleFlags(args)
	if err != nil {
		return err
	}

	pidFile := filepath.Join(dataDir, "server.pid")
	pid, err := readPID(pidFile)
	if err != nil || pid <= 0 {
		fmt.Printf("○ 网关服务未在运行 (未找到 PID 文件: %s)\n", pidFile)
		return nil
	}

	if !isProcessAlive(pid) {
		_ = os.Remove(pidFile)
		fmt.Printf("○ 网关服务未在运行 (清理失效 PID %d)\n", pid)
		return nil
	}

	_ = killProcess(pid)

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
	fmt.Printf("✓ 网关服务进程 (PID %d) 已成功停止\n", pid)
	return nil
}

func runServerRestart(args []string) error {
	dataDir, _, _ := parseServerLifecycleFlags(args)
	pidFile := filepath.Join(dataDir, "server.pid")
	if pid, err := readPID(pidFile); err == nil && pid > 0 && isProcessAlive(pid) {
		fmt.Println("正在停止已有网关进程...")
		_ = runServerStop(args)
		time.Sleep(500 * time.Millisecond)
	}
	return runServerStart(args)
}

func runServerStatus(args []string) error {
	dataDir, addr, err := parseServerLifecycleFlags(args)
	if err != nil {
		return err
	}

	pidFile := filepath.Join(dataDir, "server.pid")
	logFile := filepath.Join(dataDir, "server.log")

	pid, err := readPID(pidFile)
	running := false
	if err == nil && pid > 0 {
		running = isProcessAlive(pid)
	}

	fmt.Println("==================================================")
	fmt.Println("             Agent Gateway 网关状态               ")
	fmt.Println("==================================================")

	if running {
		fmt.Printf("● 运行状态: [RUNNING] 正常运行中 (PID: %d)\n", pid)
	} else {
		fmt.Printf("○ 运行状态: [STOPPED] 未运行\n")
	}

	fmt.Printf("  数据目录: %s\n", dataDir)
	fmt.Printf("  监听地址: %s\n", addr)
	fmt.Printf("  运行日志: %s\n", logFile)

	passFile := filepath.Join(dataDir, "admin.password")
	if data, err := os.ReadFile(passFile); err == nil {
		fmt.Printf("  管理账号: admin | 密码文件: %s\n", passFile)
		_ = data
	}

	policyFile := filepath.Join(dataDir, "policy.sqlite")
	if st, err := policy.Open(policyFile); err == nil {
		defer st.Close()
		fmt.Printf("  策略存储: %s (正常)\n", policyFile)
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
		fmt.Println("提示: 运行 'mesh server start' 可在后台启动网关服务。")
	}
	return nil
}

func runServerReload(args []string) error {
	dataDir, _, err := parseServerLifecycleFlags(args)
	if err != nil {
		return err
	}

	pidFile := filepath.Join(dataDir, "server.pid")
	pid, err := readPID(pidFile)
	if err != nil || pid <= 0 || !isProcessAlive(pid) {
		return fmt.Errorf("网关服务未在运行，无法重载。请先运行 'mesh server start'。")
	}

	if err := reloadProcess(pid); err != nil {
		fmt.Println("当前平台不支持动态信号重载，正在执行平滑重启...")
		return runServerRestart(args)
	}

	fmt.Printf("✓ 网关服务 (PID %d) 配置重载信号已下发。\n", pid)
	return nil
}
