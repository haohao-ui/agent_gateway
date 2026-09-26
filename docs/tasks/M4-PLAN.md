# M4 里程碑规划：远程生态接入、智能多 Agent 路由与角色清晰化

- **规划时间**：2026-09-26
- **里程碑目标**：
  1. 补齐与原 Python 版本的核心生态差距（HTTP/SSE 远程 MCP 网关、任务同步挂起等待 `wait_task_result`、外部会话交接 `handoff` 与自动择机）；
  2. 支持单节点多 Agent 适配器（Adapters）及跨平台宿主机 Agent 软件自动探测（`installed_agents`）；
  3. 彻底理顺系统命名与角色认知，消除 `mesh` 对等网络误导，统一为主命令 `agent-gateway`；
  4. 提供 AI 自助接入文档托管（`/onboarding.md`）与桌面微控制器硬件状态接口（`/api/public/status`）。

---

## 一、工作包分解

### M4-A：HTTP/SSE 远程 MCP 网关与同步等待工具
1. **网关挂载 `/mcp` HTTP 端点**：
   - 在 `internal/httpapi` 提供 HTTP/SSE/Streamable MCP 服务端入口；
   - 支持 Bearer Token 身份验证（与操作员鉴权打通）；
   - 使电脑端豆包、云端 Agent 无需启动本地子进程即可通过 URL 接入网关 MCP。
2. **短任务长轮询同步等待（`wait_task_result`）**：
   - 在网关和 MCP 工具中提供同步等待机制，最长挂起 120 秒；
   - 任务进入终态立即返回响应，超时则返回当前运行状态，免去大模型轮询开销；
   - HTTP API 对应提供 `GET /v1/tasks/{id}/wait`（兼容 `/api/tasks/{id}/wait`）。
3. **会话转交与智能选机（`handoff_to_computer_agent`）**：
   - 支持结构化上下文 `context` 组装（手机端会话/外部前序会话注入）；
   - 省略 `target_device` 时，网关根据节点在线状态与最新活跃时间自动智能选机。
4. **辅助端点支持**：
   - 提供 `GET /api/public/status` 免密轻量状态接口（供 ESP32 等微控制器小屏幕显示在线设备与任务数）；
   - 提供 `GET /onboarding.md` 动态渲染页面（自动注入当前网关公网/局域网 URL，供外部 AI 自助阅读对接）。

### M4-B：节点多 Agent 适配器与环境自动嗅探
1. **跨平台已安装 Agent 软件探测（`discover_agents`）**：
   - macOS：保守检测 `/Applications` 与 `~/Applications` 下的桌面端（ChatGPT, Claude, Cursor, Doubao, Windsurf 等）；
   - 跨平台：检测 `PATH` 及常见目录中的 CLI（`claude`, `codex`, `gemini`, `hermes`, `aider`, `opencode`, `cursor` 等）；
   - 形成 `installed_agents` 清单上报网关，并在 WebUI 与 MCP 中展示。
2. **单节点多 Agent 适配器映射（`adapters`）**：
   - 节点配置文件支持 `adapters` 字典，每项包含名称、绝对路径 `executable`、参数模板 `args`（含严格独立 `"{instruction}"`）；
   - 任务派发中携带 `agent` 标识，节点本地根据匹配的适配器启动相应子进程，未匹配时安全拒绝。

### M4-C：命名规范化与分发结构优化
1. **消除 `mesh` 命名混淆**：
   - 主二进制与命令规范为 `agent-gateway`（提供 `ag` / `mesh` 兼容别名或软链接）；
   - 帮助文档明确划分为 `[Control Plane / Gateway]` 与 `[Worker Node]` 区域。

---

## 二、准入与交付标准

按照 `docs/ENGINEERING.md` 规则执行：
1. 独立 worktree 分支开发；
2. 全流程零 CGO、单二进制无外部运行时依赖；
3. `gofmt` 零输出、`go vet ./...` 零告警；
4. `go test ./...` 与 `go test -race ./...` 100% 通过；
5. Linux (`GOOS=linux`) 与 Windows (`GOOS=windows`) 交叉编译无错误；
6. 提交交付验收报告 `docs/reports/m4-*.md` 并合入 `main`。
