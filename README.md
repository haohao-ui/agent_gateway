# Agent Gateway

Go 实现的轻量远程 Agent 执行网关：模块化单体、单二进制，以 mTLS 连接节点并调度可恢复任务。

## 核心能力

| 能力 | 实现位置 |
|---|---|
| 任务持久化与状态机：领取、续租、完成、unknown 对账、重排与人工裁决 | `internal/taskstore` |
| 本机任务执行与能力适配 | `internal/runner` |
| 私有 CA、配对邀请与证书签发 | `internal/identity` |
| 网关接口：mTLS 校验、长轮询与流式事件 | `internal/httpapi` |
| 事件广播：有界缓冲、慢客户端隔离 | `internal/events` |
| 节点出站客户端与任务循环 | `internal/node` |
| 设备注册、撤销与查询 | `internal/devicestore` |
| 操作员身份与节点级授权 | `internal/policy` |
| 内嵌 Web 管理控制台 | `internal/webui` |
| MCP 接入（stdio 与 HTTP） | `internal/mcp` |
| 产物沙箱与断点续传 | `internal/artifact` |
| 环境与连通性诊断 | `internal/doctor` |
| 用户级后台服务托管（macOS launchd / Linux systemd） | `internal/service` |

## 快速开始

构建：

```sh
go build -o bin/mesh ./cmd/mesh
```

项目检查：

```sh
go build ./...
go test ./...
go vet ./...
```

## 命令一览

```text
[Control Plane / Gateway]
  server                          运行网关：配对、mTLS 任务接口、WebUI 与 /mcp
  invite                          为运行中的网关签发新的配对邀请
  credential issue|revoke         本地管理操作员凭证
  device   list|revoke            查询或撤销已注册设备

[Worker Node]
  pair                            将本机注册到网关（节点侧）
  node                            运行本机任务循环
  node list                       查询已注册节点/设备状态

[Operator & Integrations]
  task     submit|get|cancel|requeue|resolve|list
  file     upload|download|list|delete
  mcp                             以 stdio 运行 MCP 服务
  doctor                          诊断节点或服务端环境与连通性
  service  install|uninstall|start|stop|status
```

具体参数用 `mesh <command> --help` 查看。

## 项目结构

```text
cmd/mesh/        统一 CLI 入口
internal/        能力模块，见「核心能力」
test/            MCP 联调程序（mcp_client、mcp_handoff）
docs/            架构、契约、规范、任务书与交付报告
go.mod, go.sum   模块声明与依赖锁定
```

## 项目文档

- [技术方案与决策](docs/ARCHITECTURE.md)
- [接口与协议契约](docs/CONTRACTS.md)
- [安全模型](docs/SECURITY.md)
- [开发规范](docs/ENGINEERING.md)
- [开发计划及验收](docs/DEVELOPMENT_PLAN.md)
- [协作任务与状态](docs/COORDINATION.md)
- [Agent 工具调用规范](docs/AGENT_SKILL_SPEC.md)
- [M2 安全设计](docs/M2-DESIGN.md)
- [M4 任务书](docs/tasks/M4-PLAN.md)
- 交付报告：[docs/reports/](docs/reports/)

## 运行要求

- Go 1.27.1（`go.mod` 声明）。Go 的自动工具链下载即可满足，无需改动系统默认 Go。
- 开发期可联网下载依赖。
- 交付程序为单一 Go 二进制，任务与策略状态存于本地 SQLite（`modernc.org/sqlite`，纯 Go），运行时不依赖外部语言运行时、Redis 或外置数据库。
- 目标 Agent CLI 及其认证由使用者自行安装配置。

## 当前状态

**开发阶段，尚未发布，不声明 production ready。**

在文档改动前的 `main`（commit `92d33b3`）干净检出下实测：

| 命令 | 结果 |
|---|---|
| `go build ./...` | exit 0 |
| `go test ./...` | exit 0，全部包通过 |
| `go vet ./...` | exit 0 |

以上只覆盖编译、单元与集成测试、静态检查，**不含**真机跨平台运行、真实 Agent 负载与长期稳定性验证。逐模块的验收范围、平台边界与未实测项，以 [协作任务与状态](docs/COORDINATION.md) 和各交付报告为准。
