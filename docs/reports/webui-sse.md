# 内嵌 Web 控制台与实时 SSE 事件流交付与验收报告

日期：2026-09-26  
开发分支：`feat/webui-sse`（工作区：`/Users/yang/tools/code/agent-gateway-worktrees/webui-sse`）  
范围：`internal/events/**`、`internal/webui/**`、`internal/httpapi/events.go`、`internal/httpapi/events_test.go`、`internal/httpapi/server.go`、`cmd/mesh/server.go`、本报告。

---

## 1. 目标与背景

根据 `docs/DEVELOPMENT_PLAN.md`（M3-A 阶段）与架构决策 ADR-010，网关需要提供原生的内嵌 Web 管理页面与实时流式事件推送机制：
1. **单二进制内嵌（零前端外部依赖）**：使用 Go 标准库 `embed.FS` 将现代化单页仪表盘打包进单一二进制产物，用户无需 Node.js、npm 或外置 CDN 即可在浏览器中开箱即用；
2. **实时事件流（Server-Sent Events）**：通过 `GET /v1/events/stream` 路由长连接分发任务生命周期事件（提交、领取、状态更新、完成、对账）及节点增量输出日志；
3. **运维可视化**：提供节点大盘、任务列表与状态统计、指令与终端日志实时抽屉、Unknown 任务对账裁决模态框及一键网关环境诊断（Doctor）。

---

## 2. 接口与代码变动清单

| 文件路径 | 变动说明 |
|---|---|
| `internal/events/hub.go` | 实现事件发布-订阅广播中心 `Hub`，支持非阻塞缓冲发布与慢客户端隔离（避免拖慢核心 HTTP/2 调度） |
| `internal/events/hub_test.go` | `internal/events` 针对性测试：订阅消费、上下文超时注销、满缓冲丢弃、并发订阅者压力验证 |
| `internal/webui/webui.go` | 利用 `//go:embed static/*` 嵌入前端静态资源，提供静态文件 HTTP Handler |
| `internal/webui/static/index.html` | 现代化暗黑极客风格控制台单页应用（仪表盘大盘、任务看板、节点列表、任务提交与诊断模态框） |
| `internal/webui/static/style.css` | 响应式 CSS 样式，包含状态指示灯呼吸灯、暗黑科技调色板、代码/终端模拟容器 |
| `internal/webui/static/app.js` | 原生 JavaScript 控制器：连接 SSE 实时通道、动态维护任务/节点状态、增量渲染与操作交互 |
| `internal/httpapi/events.go` | 实现 `GET /v1/events/stream`（带 15s 保活 Ping）以及 `GET /v1/doctor` 接口 |
| `internal/httpapi/events_test.go` | 针对静态资源服务、SSE 长连接实时分发及 Doctor 端点的端到端集成测试 |
| `internal/httpapi/server.go` | 集成 `events.Hub`，在任务提交、领取、启动、续租、完成、取消、事件流各生命周期处发布事件 |
| `internal/httpapi/security.go` | 在操作员提交、取消、重排、裁决端点处发布对应事件 |
| `cmd/mesh/server.go` | 网关服务启动时为 HTTP API 注入数据目录与系统级自检函数 |

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
ok  	agent-gateway/cmd/mesh	0.486s
ok  	agent-gateway/internal/devicestore	0.349s
ok  	agent-gateway/internal/doctor	0.315s
ok  	agent-gateway/internal/events	0.197s
ok  	agent-gateway/internal/httpapi	1.043s
ok  	agent-gateway/internal/identity	0.328s
ok  	agent-gateway/internal/integration	0.275s
ok  	agent-gateway/internal/node	7.909s
ok  	agent-gateway/internal/policy	1.160s
?   	agent-gateway/internal/protocol	[no test files]
ok  	agent-gateway/internal/runner	3.070s
ok  	agent-gateway/internal/taskstore	0.981s

$ go test -race ./...
ok  	agent-gateway/cmd/mesh	1.997s
ok  	agent-gateway/internal/devicestore	2.423s
ok  	agent-gateway/internal/doctor	1.600s
ok  	agent-gateway/internal/events	2.067s
ok  	agent-gateway/internal/httpapi	3.440s
ok  	agent-gateway/internal/identity	2.316s
ok  	agent-gateway/internal/integration	3.269s
ok  	agent-gateway/internal/node	14.361s
ok  	agent-gateway/internal/policy	6.457s
?   	agent-gateway/internal/protocol	[no test files]
ok  	agent-gateway/internal/runner	25.477s
ok  	agent-gateway/internal/taskstore	6.603s
```

### 3.3 跨平台交叉编译
```sh
$ CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ./...
# 退出码 0

$ CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build ./...
# 退出码 0
```

---

## 4. 结论与下一步

内嵌 Web 控制台与实时 SSE 流彻底解决了网关缺少可视化看板与必须依赖命令行轮询的技术痛点。
下一步将基于 M3 规划继续推进 **官方 MCP（Model Context Protocol）SDK 接入**。
