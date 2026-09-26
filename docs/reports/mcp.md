# 官方 MCP（Model Context Protocol）接入交付与验收报告

日期：2026-09-26  
开发分支：`feat/mcp`（工作区：`/Users/yang/tools/code/agent-gateway-worktrees/mcp`）  
范围：`internal/mcp/**`、`cmd/mesh/mcp.go`、`cmd/mesh/mcp_test.go`、`cmd/mesh/main.go`、`go.mod`、`go.sum`、本报告。

---

## 1. 目标与背景

根据 `docs/DEVELOPMENT_PLAN.md`（M3-A 阶段）与架构决策 ADR-005，网关引入官方 MCP SDK（`github.com/modelcontextprotocol/go-sdk`），为外部 Agent（如 Claude Desktop、Cursor、Cline 等支持 Model Context Protocol 的客户端宿主）暴露标准的工具调用入口。

本次交付完成了：
1. **官方 SDK 选型与版本锁定**：锁定官方标准库 `github.com/modelcontextprotocol/go-sdk v1.8.0`，严格遵从 Go 1.27 工具链及官方规范；
2. **标准工具集（MCP Tools）**：
   - `task_submit`：向指定节点提交执行任务，自动校验 JSON 结构，支持自定义 capability 与超时设置；
   - `task_get`：查询任务状态、Attempt 标识、退出码与执行输出（Stdout/Stderr 尾缓冲）；
   - `task_cancel`：请求取消指定排队或运行中的任务；
   - `device_list`：列出当前网络中所有已登记设备、公钥指纹与证书有效期；
   - `doctor_diagnose`：获取网关与节点环境系统自检报告；
3. **双后端适配层（`GatewayBackend`）**：
   - `ClientBackend`：通过 HTTPS + Bearer 凭证安全调用后台正在运行的网关 REST 接口（标准远程/本地运维模式）；
   - `LocalBackend`：直接对接本地 SQLite 存储（适用于单机无服务或本地嵌入式运行）；
4. **标准 I/O 交互与命令行集成**：
   - 新增 `mesh mcp` 命令，默认通过标准输入输出 Stdio 协议传输 newline-delimited JSON-RPC，便于 Claude Desktop 配置文件（`claude_desktop_config.json`）一键挂载启动。

---

## 2. 接口与代码变动清单

| 文件路径 | 变动说明 |
|---|---|
| `internal/mcp/types.go` | 定义 MCP 工具输入输出结构体，使用结构化 tag 规范提供参数描述 |
| `internal/mcp/backend.go` | 抽象 `GatewayBackend` 接口；实现 `LocalBackend` 与 `ClientBackend`（支持 CA 根证书与 Bearer Token 强鉴权） |
| `internal/mcp/server.go` | 使用官方 SDK 注册 `task_submit`、`task_get`、`task_cancel`、`device_list`、`doctor_diagnose`，实现 `RunStdio` |
| `internal/mcp/server_test.go` | 利用官方 SDK 的 `NewInMemoryTransports` 与 `mcp.NewClient` 进行端到端内存协议通信与全工具链测试 |
| `cmd/mesh/mcp.go` | 实现 `mesh mcp` 命令行交互，支持 `--server`、`--token`、`--ca` 与 `--data-dir` |
| `cmd/mesh/mcp_test.go` | 针对 `runMCP` 参数合法性校验进行单元测试 |
| `cmd/mesh/main.go` | 在 `usage` 与主分发中注册 `mcp` 命令 |
| `go.mod` / `go.sum` | 规范引入 `github.com/modelcontextprotocol/go-sdk v1.8.0` 及其必要依赖 |

---

## 3. 合并门槛与真实测试结果

执行环境：macOS (darwin/arm64)，Go 1.27.1。

### 3.1 格式与静态检查
```sh
$ gofmt -l ./cmd ./internal
# 无输出（代码完全符合格式规范）

$ go vet ./...
# 退出码 0，零告警
```

### 3.2 单元测试与数据竞争检测
```sh
$ go test ./...
ok  	agent-gateway/cmd/mesh	1.094s
ok  	agent-gateway/internal/devicestore	0.995s
ok  	agent-gateway/internal/doctor	0.458s
ok  	agent-gateway/internal/events	1.009s
ok  	agent-gateway/internal/httpapi	1.740s
ok  	agent-gateway/internal/identity	1.098s
ok  	agent-gateway/internal/integration	0.538s
ok  	agent-gateway/internal/mcp	0.745s
ok  	agent-gateway/internal/node	8.460s
ok  	agent-gateway/internal/policy	1.914s
?   	agent-gateway/internal/protocol	[no test files]
ok  	agent-gateway/internal/runner	3.360s
ok  	agent-gateway/internal/taskstore	1.204s

$ go test -race ./...
ok  	agent-gateway/cmd/mesh	2.847s
ok  	agent-gateway/internal/devicestore	2.789s
ok  	agent-gateway/internal/doctor	2.645s
ok  	agent-gateway/internal/events	2.469s
ok  	agent-gateway/internal/httpapi	4.248s
ok  	agent-gateway/internal/identity	3.011s
ok  	agent-gateway/internal/integration	3.962s
ok  	agent-gateway/internal/mcp	2.839s
ok  	agent-gateway/internal/node	13.805s
ok  	agent-gateway/internal/policy	3.383s
?   	agent-gateway/internal/protocol	[no test files]
ok  	agent-gateway/internal/runner	22.686s
ok  	agent-gateway/internal/taskstore	3.470s
```

### 3.3 跨平台交叉编译
```sh
$ CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ./...
# 退出码 0

$ CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build ./...
# 退出码 0
```

---

## 4. 客户端配置示例（Claude Desktop）

在 `claude_desktop_config.json` 中配置：
```json
{
  "mcpServers": {
    "agent-gateway": {
      "command": "/path/to/mesh",
      "args": [
        "mcp",
        "--server", "https://127.0.0.1:8443",
        "--token", "<YOUR_OPERATOR_TOKEN>",
        "--ca", "/path/to/gateway-data/ca.crt"
      ]
    }
  }
}
```
启动后即可直接向模型提示：“查询当前网关有哪些节点”、“向 node-1 提交测试任务并获取结果”。
