# 诊断与自检命令（mesh doctor）交付与验收报告

日期：2026-09-26  
开发分支：`feat/doctor`（工作区：`/Users/yang/tools/code/agent-gateway-worktrees/doctor`）  
范围：`internal/doctor/**`、`internal/httpapi/server.go`、`cmd/mesh/doctor.go`、`cmd/mesh/doctor_test.go`、`cmd/mesh/main.go`、本报告。

---

## 1. 目标与背景

在去中心化或分布式 Mesh 环境中，节点端与网关服务端可能因证书过期、时钟漂移（导致租约误超时）、网络配置错误、TLS 握手拦截或 SQLite 数据损坏而发生静默失败。为保障系统的可用性与可运维性，M2 里程碑规划了 `doctor` 自检诊断机制。

本次交付完成了**一键环境与连通性自检命令 `mesh doctor`**：
1. **证书有效性与过期自检**：检查节点证书及网关 CA 证书的有效期，支持临期（如 14/30 天内）发出 `WARN` 预警；
2. **时钟漂移（Clock Skew）检测**：节点自检时向网关探针端点发起请求，通过 HTTP Date 响应头精确计算本地时间与服务端时间的绝对偏差，当漂移超过 5 秒安全阈值时触发警告，防止租约误判定；
3. **mTLS 双向网络握手探针**：网关新增 `GET /v1/tasks/probe` 探针端点，校验节点 mTLS 凭据合法性；
4. **SQLite 数据库完整性检查**：支持对 `tasks.sqlite`、`devices.sqlite`、`policy.sqlite`、`journal.sqlite`、`outbox.sqlite` 执行 `PRAGMA integrity_check` 完整性校验；
5. **双运行模式与结构化输出**：支持节点模式（默认 `--node-dir`）与服务端模式（`--server-mode --data-dir`），支持终端彩色/符号可视化输出与 `--json` 机器可读格式，在存在 `FAIL` 项时严格以非零状态码退出。

---

## 2. 接口与代码变动清单

| 文件路径 | 变动说明 |
|---|---|
| `internal/doctor/doctor.go` | 实现核心诊断函数：`CheckCertificate`、`CheckSQLiteIntegrity`、`CheckClockSkew`、`DiagnoseNode`、`DiagnoseServer` 及网关探针连通性检测 |
| `internal/doctor/doctor_test.go` | `internal/doctor` 针对性测试：证书有效/临期/过期/非法、SQLite 完好/损坏/缺失、时钟漂移、服务端及节点模式全流程诊断 |
| `internal/httpapi/server.go` | 增加探针路由 `GET /v1/tasks/probe` 及处理函数 `handleProbe`（经 `requireNodeAuth` 鉴权） |
| `cmd/mesh/doctor.go` | 实现 `mesh doctor` 命令行交互，支持 `--node-dir`、`--server-mode`、`--data-dir`、`--json` 及终端对齐格式化输出 |
| `cmd/mesh/doctor_test.go` | 针对 `runDoctor` 的命令行参数与模式分发进行端到端测试 |
| `cmd/mesh/main.go` | 在 `usage` 与主路由中注册 `doctor` 命令 |

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
ok  	agent-gateway/cmd/mesh	0.565s
ok  	agent-gateway/internal/devicestore	1.196s
ok  	agent-gateway/internal/doctor	1.136s
ok  	agent-gateway/internal/httpapi	1.723s
ok  	agent-gateway/internal/identity	1.433s
ok  	agent-gateway/internal/integration	1.097s
ok  	agent-gateway/internal/node	8.387s
ok  	agent-gateway/internal/policy	2.147s
?   	agent-gateway/internal/protocol	[no test files]
ok  	agent-gateway/internal/runner	4.012s
ok  	agent-gateway/internal/taskstore	1.270s

$ go test -race ./...
ok  	agent-gateway/cmd/mesh	1.895s
ok  	agent-gateway/internal/devicestore	1.884s
ok  	agent-gateway/internal/doctor	1.805s
ok  	agent-gateway/internal/httpapi	2.793s
ok  	agent-gateway/internal/identity	1.827s
ok  	agent-gateway/internal/integration	2.633s
ok  	agent-gateway/internal/node	13.910s
ok  	agent-gateway/internal/policy	3.551s
?   	agent-gateway/internal/protocol	[no test files]
ok  	agent-gateway/internal/runner	22.671s
ok  	agent-gateway/internal/taskstore	3.503s
```

### 3.3 跨平台交叉编译
```sh
$ CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ./...
# 退出码 0

$ CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build ./...
# 退出码 0
```

### 3.4 命令行输出效果演示
```sh
$ mesh doctor --server-mode --data-dir ./gateway-data
Agent Gateway Doctor (mode: server, target: ./gateway-data)
------------------------------------------------------------
[✓] Gateway CA            : valid (expires in 3649 days on 2036-09-23)
[✓] Tasks Database        : integrity check OK (PRAGMA integrity_check: ok)
[✓] Devices Database      : integrity check OK (PRAGMA integrity_check: ok)
[✓] Policy Database       : integrity check OK (PRAGMA integrity_check: ok)
------------------------------------------------------------
Result: HEALTHY
```

---

## 4. 结论与后续

本项自检能力彻底解除了排查节点和网关配置、证书及数据库损坏的运维黑盒问题，满足 M2 阶段生产可运维性门槛。
