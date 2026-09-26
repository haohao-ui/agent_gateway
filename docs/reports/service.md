# 交付报告：用户级跨平台系统服务托管（M3 里程碑完结）

- **模块分支**：`feat/service`
- **提交范围**：`internal/service/`、`cmd/mesh/service.go`、`cmd/mesh/main.go`、`cmd/mesh/service_test.go`
- **完成日期**：2026-09-26
- **交付状态**：已完成核心功能与跨平台架构，单元测试及竞态检查通过，零编译告警。

---

## 1. 目标与设计原则

根据架构设计规范（`docs/ARCHITECTURE.md` 第 7 行：*“一份程序、server/node 两个角色，另有 pair、doctor、service、task、file 子命令”*），本模块为用户提供免 root / 免管理员特权的用户级系统后台服务托管能力：

1. **零外部守护工具依赖**：无需安装或配置 supervisord、pm2、nssm 等第三方进程管理工具，单二进制直接利用操作系统原生守护机制；
2. **多平台原生适配**：
   - **macOS (Darwin)**：利用 `launchd` 用户代理机制（`~/Library/LaunchAgents/com.agent-gateway.<role>.plist`），支持开机自启、崩溃自动重启（`RunAtLoad` 与 `KeepAlive`）、日志独立重定向，并通过 `launchctl` 进行生命周期管理；
   - **Linux**：利用 `systemd` 用户服务机制（`~/.config/systemd/user/agent-gateway-<role>.service`），通过 `systemctl --user` 进行服务重载、启用与进程追踪；
   - **Windows 及其他系统**：Windows 普通用户运行服务需任务计划程序或管理员提权，模块提供明确的引导与提示信息，并保证零编译错误；
3. **安全与隔离**：
   - 服务定义在用户自身权限范围内，不向系统级目录写入文件；
   - 测试通过临时隔离目录注入与 Mock Runner，绝不污染宿主机的实际系统配置。

---

## 2. 变更内容与架构说明

### 2.1 核心模块（`internal/service`）
- `manager.go`：定义 `Config`、`Status` 结构体，`Manager` 核心接口（`Install`、`Uninstall`、`Start`、`Stop`、`Status`），提供 `NormalizeConfig` 与 `ValidateRole`；
- `exec.go`：通用命令调用封装，解耦平台与执行器；
- `template_launchd.go`：生成符合 Apple DTD 规范的 launchd XML plist 描述文件，包含命令参数、工作目录、环境变量及重定向日志路径；
- `template_systemd.go`：生成符合 systemd 规范的 `.service` unit 配置文件，包含 `Type=simple`、`ExecStart`、`Restart=always`、`RestartSec=3` 与 `StandardOutput/StandardError` 配置；
- `manager_darwin.go`：macOS launchd 驱动实现，负责创建目录、部署 plist、执行 `launchctl load/unload/start/stop/list` 并解析运行状态及 PID；
- `manager_linux.go`：Linux systemd 驱动实现，负责写入 service unit 文件并调用 `systemctl --user daemon-reload/enable/start/stop/show`；
- `manager_windows.go` / `manager_other.go`：非 Linux/macOS 平台桩实现与友好错误提示；
- `service_test.go` / `service_darwin_test.go` / `service_linux_test.go`：跨平台通用与平台特有单元测试，覆盖模板生成、参数合法性与完整生命周期流程。

### 2.2 命令行集成（`cmd/mesh`）
- `cmd/mesh/service.go`：提供统一的 `mesh service` 命令，包含以下动作：
  - `mesh service install [--role server|node] [--bin path] [--working-dir path] [--log-dir path] [--addr addr] [--data-dir path] [--config path] [--node-dir path] [--args "extra flags"]`
  - `mesh service uninstall [--role server|node]`
  - `mesh service start [--role server|node]`
  - `mesh service stop [--role server|node]`
  - `mesh service status [--role server|node] [--json]`
- `cmd/mesh/main.go`：将 `service` 注册进主命令分发及 usage 帮助信息；
- `cmd/mesh/service_test.go`：CLI 命令行参数及异常情况测试。

---

## 3. 验收命令与真实结果记录

### 3.1 代码规范化格式检查（`gofmt`）
```bash
$ gofmt -l ./cmd ./internal
# 输出为空，全部文件符合 Go 规范
```

### 3.2 静态代码分析（`go vet`）
```bash
$ go vet ./...
# 退出码 0，无任何告警
```

### 3.3 全仓单元测试（`go test ./...`）
```bash
$ go test ./...
ok  	agent-gateway/cmd/mesh	0.499s
ok  	agent-gateway/internal/artifact	0.892s
ok  	agent-gateway/internal/devicestore	1.056s
ok  	agent-gateway/internal/doctor	1.046s
ok  	agent-gateway/internal/events	0.997s
ok  	agent-gateway/internal/httpapi	1.741s
ok  	agent-gateway/internal/identity	0.429s
ok  	agent-gateway/internal/integration	0.586s
ok  	agent-gateway/internal/mcp	0.654s
ok  	agent-gateway/internal/node	8.293s
ok  	agent-gateway/internal/policy	1.654s
ok  	agent-gateway/internal/runner	3.312s
ok  	agent-gateway/internal/service	0.301s
ok  	agent-gateway/internal/taskstore	1.250s
```

### 3.4 竞态安全检测（`go test -race ./...`）
```bash
$ go test -race ./...
ok  	agent-gateway/cmd/mesh	1.865s
ok  	agent-gateway/internal/artifact	1.445s
ok  	agent-gateway/internal/devicestore	1.957s
ok  	agent-gateway/internal/doctor	1.760s
ok  	agent-gateway/internal/events	1.461s
ok  	agent-gateway/internal/httpapi	2.837s
ok  	agent-gateway/internal/identity	1.665s
ok  	agent-gateway/internal/integration	2.859s
ok  	agent-gateway/internal/mcp	1.601s
ok  	agent-gateway/internal/node	13.806s
ok  	agent-gateway/internal/policy	3.244s
ok  	agent-gateway/internal/runner	22.687s
ok  	agent-gateway/internal/service	1.340s
ok  	agent-gateway/internal/taskstore	3.426s
```

### 3.5 跨平台交叉编译检查
```bash
$ CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /dev/null ./cmd/mesh
# 退出码 0

$ CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o /dev/null ./cmd/mesh
# 退出码 0

$ go build -o /dev/null ./cmd/mesh
# 退出码 0
```

### 3.6 真实 CLI 命令验证输出
```bash
$ mesh service status --role server
Service Status (server):
  platform:     darwin (launchd)
  installed:    false
  running:      false
  service file: /Users/yang/Library/LaunchAgents/com.agent-gateway.server.plist
  log file:     /Users/yang/.agent-gateway/logs/server.log

$ mesh service status --role node --json
{
  "role": "node",
  "platform": "darwin (launchd)",
  "installed": false,
  "running": false,
  "service_file": "/Users/yang/Library/LaunchAgents/com.agent-gateway.node.plist",
  "log_file": "/Users/yang/.agent-gateway/logs/node.log"
}
```

---

## 4. 边界与未完成项提示

1. **Windows 平台**：Windows 下运行用户级免提权系统服务因其操作系统架构限制（传统 Windows Service 绑定 SCM 且要求管理员权限），目前返回指引提示；后续若需要无缝守护，可在未来里程碑基于 Windows Task Scheduler API 实现免提权注册；
2. **多角色同机多实例**：当前一个角色（`server` 或 `node`）注册一个系统单例服务（`com.agent-gateway.server` / `agent-gateway-server.service`），符合架构基线单机定位。
