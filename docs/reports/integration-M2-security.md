# M2 安全补齐集成与验收报告

日期：2026-09-26  
集成分支：`integration/m2-security`（工作区：`/Users/yang/tools/code/agent-gateway-worktrees/m2-security-integration`）  
输入源：
- `worker/agy-m2-security` (commit `2abbdbb`): `internal/devicestore/**`, `docs/reports/agy-M2-security.md`
- `worker/claude-m2-security`: `internal/policy/**`, `docs/reports/claude-M2-security.md`
- 协调者安全接入: `internal/httpapi/**`, `cmd/mesh/**`, 联动与安全边界加固测试

---

## 1. 集成结论

1. **核心安全链路全线打通**：
   - **设备注册与撤销**：`/v1/pair` 节点配对成功后立即自动注册证书指纹；所有 `/v1/tasks/**` mTLS 路由在每个请求（包括长轮询 `claim` 唤醒时刻）均穿透调用 `devicestore.Authorize` 强校验；
   - **持久连接撤销阻断**：即使底层 HTTP/2 或 TLS 连接已经建立并在复用状态，一旦设备被撤销，后续请求立即返回 `401 Unauthorized`；
   - **独立操作员与权限矩阵**：`/v1/operator/**` 接口全部要求 HTTPS 独立 Bearer 凭据认证，支持 `admin`、`operator`、`viewer` 角色及细粒度节点作用域隔离；设备证书被严格限制，无法提权访问操作员接口；
   - **本地 CLI 凭据管理**：新增 `mesh credential issue|revoke`（本地 0600 凭据文件管理）与 `mesh device revoke`（管理员远程审计撤销），`mesh task` 支持 `--token-file` 操作员执行通道。
2. **测试与质量门槛**：
   - 全仓 9 个 Go 模块包（`cmd/mesh`、`devicestore`、`httpapi`、`identity`、`integration`、`node`、`policy`、`runner`、`taskstore`）**全部通过测试**；
   - 全仓启用 `-race` **零数据竞争**；
   - `gofmt` 无格式偏差，`go vet ./...` 零告警；
   - `CGO_ENABLED=0` 交叉编译 Linux (amd64) 与 Windows (amd64) 均成功通过。

---

## 2. 交付与改动清单

| 模块 / 路径 | 变动类型 | 说明 |
|---|---|---|
| `internal/devicestore/**` | 新增 | 设备证书指纹绑定、常量时间比对、无缓存穿透、原子审计日志、并发安全 |
| `internal/policy/**` | 新增 | 角色与操作员主体模型、权限矩阵纯函数裁决、哈希凭据安全存储、作用域规范化 |
| `internal/httpapi/security.go` | 新增 | `NewSecureServer`、证书指纹提取、设备授权中间件、Bearer 操作员认证与路由 |
| `internal/httpapi/security_test.go` | 新增 | 真实 TLS 集成测试、连接复用下撤销测试、角色矩阵与提权拦截、明文 HTTP 拦截 |
| `internal/httpapi/server.go` | 增强 | `handlePair` 注册设备、`handleClaim` 唤醒重授权、错误响应防信息探测泄露 |
| `cmd/mesh/operator.go` | 新增 | `runCredential` 本地凭据管理、`newOperatorClient` HTTPS 客户端、`runDevice` 撤销 |
| `cmd/mesh/server.go` | 增强 | 网关服务集成 `devicestore` 与 `policy` 数据库初始化与安全装配 |
| `cmd/mesh/task.go` | 增强 | `task submit/get/cancel` 支持 `--token-file` / `--ca` 操作员身份与 `--node-dir` 互斥保护 |
| `cmd/mesh/main.go` | 增强 | 暴露 `credential` 与 `device` 命令行子命令 |
| `internal/node/node_test.go` | 修复 | 等待本地 ACK 清理完成，解决既有测试中因日志打印租约凭据或边界竞态导致的偶发问题 |
| `docs/reports/*` | 新增 | 包含各 worker 交付报告及本集成验收报告 |

---

## 3. 合并门槛与真实检查命令输出

执行环境：macOS (darwin/arm64)，Go 1.27.1。

### 3.1 格式与静态检查
```sh
$ gofmt -l ./cmd ./internal
# 无输出（代码完全符合 gofmt 规范）

$ go vet ./...
# 退出码 0，无任何告警
```

### 3.2 全量单元测试与数据竞争检测
```sh
$ go test ./...
ok  	agent-gateway/cmd/mesh	1.923s
ok  	agent-gateway/internal/devicestore	1.085s
ok  	agent-gateway/internal/httpapi	2.366s
ok  	agent-gateway/internal/identity	2.416s
ok  	agent-gateway/internal/integration	3.133s
ok  	agent-gateway/internal/node	9.510s
ok  	agent-gateway/internal/policy	2.958s
?   	agent-gateway/internal/protocol	[no test files]
ok  	agent-gateway/internal/runner	5.895s
ok  	agent-gateway/internal/taskstore	4.005s

$ go test -race ./...
ok  	agent-gateway/cmd/mesh	2.162s
ok  	agent-gateway/internal/devicestore	2.201s
ok  	agent-gateway/internal/httpapi	3.026s
ok  	agent-gateway/internal/identity	1.675s
ok  	agent-gateway/internal/integration	2.698s
ok  	agent-gateway/internal/node	14.434s
ok  	agent-gateway/internal/policy	4.174s
?   	agent-gateway/internal/protocol	[no test files]
ok  	agent-gateway/internal/runner	22.736s
ok  	agent-gateway/internal/taskstore	5.404s
```

### 3.3 跨平台 CGO_ENABLED=0 构建检查
```sh
$ CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ./...
# 退出码 0

$ CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build ./...
# 退出码 0
```

---

## 4. 关键安全语义验证

1. **已建立 TLS 连接撤销验证**：
   在 `TestSecureOperatorRolesAndRevocation` 中，节点先发起并复用一条底层 TLS 连接，管理员通过操作员接口撤销该节点后，该持久连接上的下一次请求（复用检验通过 `httptrace.GotConnInfo.Reused == true`）立即被拦截并返回 `401 Unauthorized`。
2. **越权与角色权限矩阵**：
   - `viewer` 角色提交或取消任务返回 `403 Forbidden`，查看其作用域外的任务返回 `404 Not Found`（防止探测存在的任务 ID）；
   - `operator` 角色在作用域内正常提交、取消，跨节点提交返回 `403 Forbidden`；
   - `admin` 具备全局提交、取消与设备撤销权限；
   - 节点设备 mTLS 客户端访问操作员端点直接返回 `401 Unauthorized`，无法提权；
   - 未知证书即使由本 CA 签发（但未记录在设备注册表），请求任务接口同样返回 `401 Unauthorized`；
   - 明文 HTTP 访问操作员接口直接拒绝（`401`）。

---

## 5. 边界说明与未完成项（不谎报 production ready）

1. **跨平台原生环境运行未验证**：Windows 与 Linux 仅通过了 `CGO_ENABLED=0` 交叉编译检查，尚未在目标系统的真实内核/文件系统权限（如 Windows ACL）下运行守护进程。
2. **日志事件持久化尚未实现**：正如 README 与 CONTRACTS 纠正所述，当前 `/v1/tasks/events` 仅对有效格式返回 ACK，尚未实现真正的持久事件流与断点恢复。
3. **旧设备证书迁移**：当前安全策略对未注册设备采取「拒绝并要求重新配对」的原则，后续版本如需平滑升级需引入显式迁移脚本。
