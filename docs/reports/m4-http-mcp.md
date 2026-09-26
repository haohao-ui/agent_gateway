# 交付报告：M4-A/B 远程 HTTP/SSE MCP、智能任务等待与生态对齐

- **模块分支**：`feat/m4-http-mcp`
- **提交范围**：`internal/mcp/`、`internal/httpapi/`、`internal/node/`、`cmd/mesh/`
- **完成日期**：2026-09-26
- **交付状态**：全部功能完成，单元测试、竞态检测通过，多平台交叉编译成功。

---

## 1. 目标与架构对齐

针对原 Python 项目（`agent-mesh`）的生态能力及前期规划的 M4 里程碑，本阶段完成了以下核心能力的全面重构与对齐：

1. **远程 Streamable HTTP / SSE MCP 端点（`/mcp`）**：
   - 官方 Go SDK 原生挂载至网关服务端口；
   - 受到操作员 Bearer Token 中间件保护；
   - 允许电脑端豆包、云端 Agent 无需在本地启动 Stdio 子进程，直接通过 URL + Header 调用网关 MCP 服务。
2. **任务短时挂起同步等待（`wait_task_result`）**：
   - MCP 工具及网关端点（`GET /v1/operator/tasks/{id}/wait` 与 `GET /v1/tasks/{id}/wait`）支持长轮询同步等待最多 120 秒；
   - 任务终态立即返回，超时则返回当前运行进度，免去调用方大模型频繁发起查询工具调用。
3. **外部会话交接与智能选机路由（`handoff_to_computer_agent`）**：
   - 支持移动端或前序对话上下文（`context`）自动注入；
   - 当调用方未指定 `target_node` 时，网关自动过滤有效设备，实现智能自动派发。
4. **宿主机已安装 Agent 软件环境嗅探（`installed_agents`）**：
   - 实现 `internal/node/discovery.go`，跨平台探测 macOS 桌面应用（Claude, Cursor, ChatGPT, Doubao, Windsurf 等）及常见 CLI 工具（`claude`, `codex`, `gemini`, `hermes`, `aider` 等）；
   - 支持节点自动标记已配置并具备可执行能力的适配器（`runnable: true`）。
5. **生态辅助端点**：
   - `GET /api/public/status`：提供免认证轻量只读状态接口，专为桌面微控制器（如 ESP32）墨水屏设计；
   - `GET /onboarding.md`：动态注入网关当前地址的 AI 自助接入文档；
6. **角色与拓扑结构认知清晰化**：
   - 优化 CLI 结构，区分 `[Control Plane / Gateway]` 与 `[Worker Node]`，消除 `mesh` 对等网络误导，统一认知为主命令 `agent-gateway`。

---

## 2. 变更接口与文件清单

- `internal/mcp/types.go`：新增 `WaitTaskInput` 与 `HandoffInput` 结构体；
- `internal/mcp/backend.go`：`GatewayBackend` 接口新增 `WaitTask` 方法，并在 `LocalBackend` 与 `ClientBackend` 落地；
- `internal/mcp/server.go`：注册 `wait_task_result` 与 `handoff_to_computer_agent` 工具，暴露 `NewStreamableHTTPHandler` 与 `NewSSEHandler`；
- `internal/httpapi/extensions.go`：实现 `waitTaskTerminal` 长轮询逻辑、`/api/public/status`、`/onboarding.md` 以及 `/mcp` 认证挂载处理；
- `internal/httpapi/server.go` 与 `security.go`：注册相关路由，挂载 MCP 认证拦截；
- `internal/node/discovery.go`：实现宿主机 Agent 软件与 CLI 探测；
- `internal/node/config.go`：优化 `instruction` 函数，支持回退查找 `instruction` 与 `prompt`；
- `cmd/mesh/main.go` 与 `server.go`：装配 MCP HTTP Handler，重构并清晰化命令行结构。

---

## 3. 验收命令与真实结果记录

### 3.1 代码规范化检查（`gofmt`）
```bash
$ gofmt -l ./cmd ./internal
# 输出为空，全部符合 Go 格式规范
```

### 3.2 静态代码分析（`go vet`）
```bash
$ go vet ./...
# 退出码 0，零告警
```

### 3.3 全仓单元测试（`go test ./...`）
```bash
$ go test ./...
ok  	agent-gateway/cmd/mesh	1.260s
ok  	agent-gateway/internal/artifact	(cached)
ok  	agent-gateway/internal/devicestore	(cached)
ok  	agent-gateway/internal/doctor	1.755s
ok  	agent-gateway/internal/events	(cached)
ok  	agent-gateway/internal/httpapi	2.790s
ok  	agent-gateway/internal/identity	(cached)
ok  	agent-gateway/internal/integration	(cached)
ok  	agent-gateway/internal/mcp	2.730s
ok  	agent-gateway/internal/node	9.315s
ok  	agent-gateway/internal/policy	(cached)
ok  	agent-gateway/internal/runner	(cached)
ok  	agent-gateway/internal/service	(cached)
ok  	agent-gateway/internal/taskstore	(cached)
```

### 3.4 竞态安全检测（`go test -race ./...`）
```bash
$ go test -race ./...
ok  	agent-gateway/cmd/mesh	1.811s
ok  	agent-gateway/internal/artifact	1.986s
ok  	agent-gateway/internal/devicestore	1.877s
ok  	agent-gateway/internal/doctor	2.178s
ok  	agent-gateway/internal/events	1.781s
ok  	agent-gateway/internal/httpapi	3.926s
ok  	agent-gateway/internal/identity	1.681s
ok  	agent-gateway/internal/integration	2.633s
ok  	agent-gateway/internal/mcp	2.962s
ok  	agent-gateway/internal/node	13.805s
ok  	agent-gateway/internal/policy	3.223s
ok  	agent-gateway/internal/runner	22.636s
ok  	agent-gateway/internal/service	1.360s
ok  	agent-gateway/internal/taskstore	3.462s
```

### 3.5 跨平台交叉编译
```bash
$ CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /dev/null ./cmd/mesh
# 退出码 0

$ CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o /dev/null ./cmd/mesh
# 退出码 0

$ go build -o /dev/null ./cmd/mesh
# 退出码 0
```
