# 安全加固与合规审计修复交付报告 (Security Hardening M1)

## 1. 任务概述

本阶段针对安全审查报告中指出的 6 项安全隐患进行了全链路加固，消除了默认凭据、权限越权、裸命令行自动暴露、传输中间人篡改等核心风险。

---

## 2. 详细改动清单

### Issue 1: 默认管理员密码加固
- **改动文件**: `cmd/mesh/server.go`, `internal/webui/static/index.html`
- **实现机制**:
  - 移除硬编码密码 `admin`。
  - 未通过环境变量 `GATEWAY_ADMIN_PASSWORD` 指定时，首次启动由安全伪随机数生成器生成 16 位强随机密码。
  - 密码保存至私有文件 `dataDir/admin.password`，权限严格设定为 `0600`。
  - 启动控制台输出安全警示横幅与指引；Web 登录界面移除默认口令填充与提示。

### Issue 2 & Issue 5: MCP 鉴权增强、会话复验与设备范围控制
- **改动文件**: `internal/policy/roles.go`, `internal/httpapi/extensions.go`, `internal/mcp/server.go`, `internal/mcp/types.go`
- **实现机制**:
  - 在 `policy` 包导出 `ActionTaskRead`、`ActionTaskSubmit`、`ActionTaskCancel` 常量，增加 `WithPrincipal` / `PrincipalFromContext` 上下文注入机制。
  - MCP 会话缓存由原先静态 24 小时过期重构为 `mcpSessionEntry`，记录对应 Token、Principal 与最后验证时间。
  - 会话请求在后台定期/实时向底层 Policy Store 复验 Token 有效性，一旦 Token 吊销立即注销 Session 并阻断请求（返回 401）。
  - 在 MCP 工具函数（`task_submit`、`node_execute`、`handoff_to_computer_agent`、`task_get`、`task_cancel`、`device_list`、`doctor_diagnose`）中强制调用 `policy.Authorize`，严格限制非 Admin 操作员只能访问其授权范围内的设备。

### Issue 3: 节点 Shell/Python 能力受控
- **改动文件**: `internal/node/config.go`, `internal/node/discovery.go`
- **实现机制**:
  - 在 `Config` 中新增 `AllowShell bool` 字段，默认保持 `false`。
  - 能力自动发现时，默认**严禁**暴露系统的 `bash`、`sh`、`python3`、`python` 解释器。
  - 仅在节点配置文件中显式配置 `"allow_shell": true` 或声明环境变量 `MESH_ALLOW_SHELL=1` 时才开放原生命令行能力；默认仅保留专用 AI Agent CLI 工具链。

### Issue 4: 安装脚本与程序 SHA-256 完整性防篡改
- **改动文件**: `internal/httpapi/system.go`, `internal/httpapi/server.go`
- **实现机制**:
  - 新增 `GET /download/mesh.sha256?arch=<arch>` 哈希摘要查询端点。
  - Linux/macOS 的 `install.sh` 脚本增加步骤 4：通过 `sha256sum`/`shasum -a 256` 自动计算并与网关发布值进行强制比对，校验失败即刻中止并清理文件。
  - Windows 的 `install.ps1` 脚本通过 PowerShell `Get-FileHash` 进行哈希校验，防止中间人劫持与篡改。

### Issue 6: 凭据泄露防护与安全 Cookie
- **改动文件**: `internal/httpapi/server.go`, `internal/webui/static/app.js`
- **实现机制**:
  - 登录成功返回的 `Set-Cookie` 增加 `HttpOnly: true` 与 `SameSite: Lax`，阻止脚本与 XSS 攻击窃取 Token。
  - 登录接口响应体移除明文 `token` 字段。
  - Web UI 前端代码移除 `localStorage` 明文存储及默认弱口令静默探测。

---

## 3. 自动化测试与检查结果

### 3.1 单元测试与端到端测试
运行全量单元测试：
```bash
go test ./...
```
真实执行结果：
- `agent-gateway/cmd/mesh`: PASS
- `agent-gateway/internal/artifact`: PASS
- `agent-gateway/internal/devicestore`: PASS
- `agent-gateway/internal/doctor`: PASS
- `agent-gateway/internal/events`: PASS
- `agent-gateway/internal/httpapi`: PASS
  - `TestMCPSession_DynamicRevalidation`: PASS
  - `TestDownloadMeshSHA256`: PASS
- `agent-gateway/internal/mcp`: PASS
  - `TestMCPServer_RoleScopeAuthorization`: PASS
- `agent-gateway/internal/node`: PASS
  - `TestPopulateDefaultCapabilities_ShellRestriction`: PASS
- `agent-gateway/internal/policy`: PASS
- `agent-gateway/internal/runner`: PASS
- `agent-gateway/internal/service`: PASS
- `agent-gateway/internal/taskstore`: PASS

### 3.2 实测检查
1. **弱密码登录拦截**:
   - `POST /api/login {"username":"admin","password":"admin"}` -> 401 Unauthorized
2. **随机安全密码登录与 Cookie 验证**:
   - 使用 `admin.password` 中生成的密码登录 -> 200 OK，响应体无明文 token，`Set-Cookie` 包含 `HttpOnly; SameSite=Lax`。
3. **SHA-256 端点验证**:
   - `GET /download/mesh.sha256?arch=darwin-arm64` -> 返回准确十六进制哈希与文件名。

---

## 4. 风险与未完成项

- **未完成项**: 无。所有列明的 6 个安全漏洞已全部落地修复并通过验证。
- **运维风险提示**:
  - 对于升级到该版本的既有网关实例，如果此前依赖默认密码 `admin` 登录，升级后需从 `gateway-data/admin.password` 中查看随机新密码，或在启动前配置环境变量 `GATEWAY_ADMIN_PASSWORD`。
