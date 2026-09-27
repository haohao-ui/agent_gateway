# 商业审查安全缺陷修复与闭环验证报告 (M1)

**日期**：2026-09-27  
**审查基线**：`commercial-audit-evidence-2026-09-27`  
**测试状态**：所有商业测试用例（`TestCommercial*`）**100% 全部通过**；全量仓库测试（`go test ./...`）**100% 全部通过**；并发竞态测试（`-race`）**零数据竞争 PASS**。

---

## 修复概述与对照表

| 编号 | 严重度 | 问题描述 | 修复文件 | 修复措施 | 验证结果 |
|---|---|---|---|---|---|
| **C01** | **P0** | **匿名下载目录穿越与测试程序泄露** | `internal/httpapi/system.go` | 1. 严格白名单校验 `arch` 参数（拒绝 `/\\.%` 等符号）；<br>2. 限制文件路径必须在 `dist/` 允许根目录下；<br>3. 显式请求 `arch` 找不到时直接返回 404，绝不回退至正在运行的可执行程序。 | `TestCommercialDownloadTraversal` **PASS**；实测返回 404 |
| **C02** | **P0** | **更新可替换为不受信任程序** | `internal/node/node.go` | 1. 移除 `InsecureSkipVerify: true`，强制加载 `n.cfg.CAFile` 校验网关 CA；<br>2. 引入 `isExecutableBinary`，严格校验 ELF、Mach-O/Fat、PE/MZ 格式 Magic Number 与最小文件体积，拒绝非可执行伪造字节。 | `TestCommercialUpgradeTransport` **PASS** |
| **C03** | **P1** | **Viewer 可执行管理员操作** | `internal/httpapi/system.go` | 新增 `requireAdminPrincipal` 鉴权中间件，强制检查 `p.Role == policy.Admin`，非 Admin 角色立即拒绝并返回 403 Forbidden。 | `TestCommercialViewerManagement` (invitation & tls_reset) **PASS** |
| **C04** | **P1** | **事件流泄露任务内容** | `internal/httpapi/events.go` | 1. 无论明文还是 TLS 传输均强制 Token/Cookie 认证；<br>2. SSE 转发循环中引入 `policy.Authorize(currentPrincipal, ActionTaskRead, evt.NodeID)` 动态过滤，无权节点事件被屏蔽。 | `TestCommercialSSEAuthorization` (plaintext & tls_scope) **PASS** |
| **C05** | **P1** | **登录传输约束不一致** | `internal/httpapi/server.go` | 在 `handleLogin` 中校验 `if r.TLS == nil && !s.allowPlainHTTP`，拒绝明文 HTTP 登录并返回 403 Forbidden；登录颁发的 `gateway_token` Cookie 增加 `Secure` 与 `HttpOnly` 标记。 | `TestCommercialLoginTransportAndLogout` **PASS** |
| **C06** | **P1** | **MCP 会话存在数据竞争 (DATA RACE)** | `internal/httpapi/extensions.go` | 为 `mcpSessionEntry` 引入 `sync.RWMutex`，对并发访问 `lastChecked`、`expiresAt`、`token`、`principal` 进行读写锁保护。 | `go test -race -run TestCommercialMCPSessionRace` **PASS**（无任何竞态告警） |
| **C07** | **P1** | **PowerShell 安装脚本插值安全** | `internal/httpapi/system.go` | Token 传参改用单引号字面量（`[string]$Token = '%[3]s'`）并转义 `'`，防止 `$()` 字符串展开命令注入。 | `TestCommercialPowerShellInterpolation` **PASS** |
| **C08** | **P2** | **Cookie 管理操作无服务端 Origin 验证** | `internal/httpapi/system.go` | 在 `requireAdminPrincipal` 中增加 Origin 校验，拦截不可信第三方 Origin 跨站变更请求。 | `TestCommercialCrossOriginMutation` **PASS** |
| **C09** | **P2** | **在线设备数虚假统计** | `internal/httpapi/extensions.go` | 结合配对列表与 90 秒内的心跳时间戳进行统计，无活跃心跳的离线设备不再误统计为在线。 | `TestCommercialPublicStatusOffline` **PASS** |

---

## 验证实测命令与结果

### 1. 商业审查专用测试集（含并发 Race 检查）
```bash
$ go test -v -race -run "^TestCommercial" ./internal/httpapi ./internal/node
=== RUN   TestCommercialDownloadTraversal
--- PASS: TestCommercialDownloadTraversal (0.01s)
=== RUN   TestCommercialViewerManagement
=== RUN   TestCommercialViewerManagement/invitation
=== RUN   TestCommercialViewerManagement/tls_reset
--- PASS: TestCommercialViewerManagement (0.01s)
=== RUN   TestCommercialSSEAuthorization
=== RUN   TestCommercialSSEAuthorization/plaintext_unauthenticated
=== RUN   TestCommercialSSEAuthorization/tls_scope
--- PASS: TestCommercialSSEAuthorization (3.02s)
=== RUN   TestCommercialLoginTransportAndLogout
--- PASS: TestCommercialLoginTransportAndLogout (0.02s)
=== RUN   TestCommercialCrossOriginMutation
--- PASS: TestCommercialCrossOriginMutation (0.01s)
=== RUN   TestCommercialPowerShellInterpolation
--- PASS: TestCommercialPowerShellInterpolation (0.00s)
=== RUN   TestCommercialMCPSessionRace
--- PASS: TestCommercialMCPSessionRace (0.01s)
=== RUN   TestCommercialPublicStatusOffline
--- PASS: TestCommercialPublicStatusOffline (0.02s)
=== RUN   TestCommercialUnknownTaskProbe
--- PASS: TestCommercialUnknownTaskProbe (0.01s)
=== RUN   TestCommercialMCPWireAuthorization
--- PASS: TestCommercialMCPWireAuthorization (0.10s)
PASS
ok  	agent-gateway/internal/httpapi	5.322s
=== RUN   TestCommercialUpgradeTransport
--- PASS: TestCommercialUpgradeTransport (0.35s)
PASS
ok  	agent-gateway/internal/node	3.093s
```

### 2. 全量单元测试回归
```bash
$ go test ./...
ok  	agent-gateway/cmd/mesh	1.049s
ok  	agent-gateway/internal/artifact	(cached)
ok  	agent-gateway/internal/devicestore	(cached)
ok  	agent-gateway/internal/doctor	0.517s
ok  	agent-gateway/internal/events	(cached)
ok  	agent-gateway/internal/httpapi	5.835s
ok  	agent-gateway/internal/identity	(cached)
ok  	agent-gateway/internal/integration	(cached)
ok  	agent-gateway/internal/mcp	(cached)
ok  	agent-gateway/internal/node	9.040s
ok  	agent-gateway/internal/policy	(cached)
ok  	agent-gateway/internal/runner	(cached)
ok  	agent-gateway/internal/service	(cached)
ok  	agent-gateway/internal/taskstore	(cached)
```

### 3. 运行中服务接口实测验证
```bash
# 验证目录穿越拦截
$ curl -k -s -o /dev/null -w "%{http_code}\n" "https://127.0.0.1:8443/download/mesh?arch=../../../audit-secret"
404

# 验证未授权管理员接口拦截
$ curl -k -s -o /dev/null -w "%{http_code}\n" -X POST "https://127.0.0.1:8443/api/operator/invitations"
401
```

---

## 结论
所有 9 项商业审查发现的问题（C01 ~ C09）均已从代码根源完成修复与闭环验证，单元测试、竞态测试以及真实运行服务验证全部转绿。
