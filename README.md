# Agent Gateway

轻量级远程 Agent 执行网关。用一台网关统一管理分布在多台机器上的 Agent 工作节点：安全配对、任务调度、结果回传与产物收集。

- **单一二进制**：Go 实现，无外部语言运行时、无 Redis、无独立数据库，任务与策略状态存于本地 SQLite。
- **零信任连接**：私有 CA + mTLS 双向认证，节点证书可随时撤销；配对使用一次性邀请令牌。
- **任务可靠投递**：任务状态机含领取、续租、完成与失联对账；节点掉线后进入 `unknown`，可人工重排或裁决，不会静默丢失。
- **多机 Agent 调度**：自动探测各节点已安装的 Agent 工具链，把任务派发给最合适的机器。
- **开箱即用的控制台与集成**：内嵌 Web 控制台与实时事件流，提供 MCP 服务，可直接接入 Claude Desktop、Cursor、Cline 等宿主。

## 快速开始

**1. 安装网关**

源码构建，或按[签名安装流程](docs/INSTALL.md)使用独立可信的验证器和发行公钥安装 GitHub Release：

```sh
go build -o bin/mesh ./cmd/mesh
```

**2. 启动网关**

```sh
./bin/mesh server
```

终端会打印控制台 HTTPS 地址、凭据文件路径与 CA 指纹，不打印管理员密码或操作员令牌。首次启动自动生成 CA、证书与数据库；登录密码保存在私有的 `admin.password` 文件中。

**3. 接入一台工作节点**

先通过独立可信渠道准备 mesh 验证器、发行公钥与网关 CA，按[验证安装流程](docs/INSTALL.md)下载并验签，再用邀请码配对。未配置公钥、签名缺失或摘要不符时停止，不执行下载内容。源码构建也可直接用于配对。

## 核心能力

| 能力 | 实现位置 |
|---|---|
| 任务持久化与状态机：领取、续租、完成、失联对账、重排与人工裁决 | `internal/taskstore` |
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

## 文档

- [安装与部署](docs/INSTALL.md) —— 系统要求、一键安装、服务端部署、节点接入、后台常驻、安全注意事项
- [使用手册](docs/USAGE.md) —— 任务提交与跟踪、节点与设备管理、产物传输、Web 控制台、MCP 接入、诊断排障
- [许可证](LICENSE) —— Apache License 2.0

## 命令总览

```text
[Control Plane / Gateway]
  server                          运行网关：配对、mTLS 任务接口、WebUI 与 /mcp
  invite                          为运行中的网关签发新的配对邀请
  credential issue|revoke         本地管理操作员凭证
  release keygen|sign|verify|fetch 离线签名与可信下载
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

## 系统要求

- 服务端与节点支持 macOS、Linux、Windows
- 源码构建需要 Go 1.27.1（`go.mod` 声明）；终端用户使用已可信验证器安装时无需安装 Go
- 运行时不依赖外部语言运行时、Redis 或独立数据库

已实测通过 `darwin/arm64`、`linux/amd64`、`linux/arm64`、`windows/amd64` 四个目标的交叉编译；原生运行仅在 macOS 上验证过。

## 配置

所有参数都可用命令行标志、`MESH_*` 环境变量或工作目录下的 `.env` 文件提供，优先级为**命令行 > 环境变量 > `.env` > 默认值**。默认配置是保守的：只监听 `127.0.0.1:8443`，明文 HTTP 默认关闭。完整变量表见[安装与部署](docs/INSTALL.md)第 7 节。

## 状态

项目处于开发阶段，尚未发布，**不声明 production ready**。

在干净检出下实测：`go build ./...`、`go test ./...`、`go vet ./...` 均退出 0。这覆盖编译、单元与集成测试、静态检查，**不含**真机跨平台运行、真实 Agent 负载与长期稳定性验证。

## 许可证

[Apache License 2.0](LICENSE)
