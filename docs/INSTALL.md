# 安装与部署

本文说明如何部署 Agent Gateway 网关，以及如何把机器接入为工作节点。

## 1. 系统要求

| 项目 | 要求 |
|---|---|
| 服务端 / 节点 | macOS、Linux、Windows |
| 源码构建 | Go 1.27.1（`go.mod` 声明）；Go 的自动工具链下载即可满足 |
| 运行时依赖 | 无。单一 Go 二进制，任务与策略状态存于本地 SQLite，不需要外部语言运行时、Redis 或独立数据库 |
| 网络 | 网关与节点之间使用 mTLS，节点侧需要能访问网关的 HTTPS 端口；首次配对需要带外传递网关 CA 证书 |

本项目实测通过以下交叉编译目标（`CGO_ENABLED=0`）：

| 目标 | 结果 |
|---|---|
| `darwin/arm64` | 通过 |
| `linux/amd64` | 通过 |
| `linux/arm64` | 通过 |
| `windows/amd64` | 通过 |

仅在 macOS 上做过本机运行验证；Linux 与 Windows 的原生运行、开机自启行为未实测。

## 2. 获取程序

### 方式一：从网关一键安装（节点推荐，无需 Go）

网关启动后自带安装脚本与二进制分发端点。在目标机器上执行：

```sh
curl -fsSL https://<网关地址>:8443/download/install.sh | bash -s -- <邀请令牌> [安装目录]
```

脚本会依次完成：

1. 探测主机操作系统与架构；
2. 下载网关 CA 证书到安装目录；
3. 下载对应架构的 `mesh` 二进制并 `chmod +x`；
4. 用网关提供的 SHA-256 校验二进制完整性，**校验不通过立即删除并终止**；
5. 执行 `mesh pair` 完成证书配对；
6. 后台启动 `mesh node`，日志写入安装目录下的 `node.log`。

默认安装目录为 `$HOME/.agent-mesh-node`，可用第二个参数覆盖。邀请令牌为**一次性**凭证，默认 15 分钟过期，由网关启动时打印或 `mesh invite` 签发。

Windows 节点使用 PowerShell 脚本：

```powershell
irm https://<网关地址>:8443/download/install.ps1 | iex
```

### 方式二：从源码构建（服务端）

```sh
git clone https://github.com/haohao-ui/agent_gateway.git
cd agent_gateway
go build -o bin/mesh ./cmd/mesh
```

验证：

```sh
./bin/mesh --version
./bin/mesh --help
```

按需交叉编译：

```sh
GOOS=darwin  GOARCH=arm64 CGO_ENABLED=0 go build -o dist/mesh-darwin-arm64  ./cmd/mesh
GOOS=linux   GOARCH=amd64 CGO_ENABLED=0 go build -o dist/mesh-linux-amd64   ./cmd/mesh
GOOS=linux   GOARCH=arm64 CGO_ENABLED=0 go build -o dist/mesh-linux-arm64   ./cmd/mesh
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o dist/mesh-windows-amd64.exe ./cmd/mesh
```

## 3. 部署网关

```sh
./bin/mesh server
```

首次启动会自动生成私有 CA、服务端证书与 SQLite 数据库，并在终端打印：

- TLS 监听地址与数据目录
- CA 证书路径与 CA SHA-256 指纹（用于带外核对）
- 配对邀请令牌（含过期时间）
- 操作员令牌
- Web 控制台地址与登录账号密码
- 节点配对命令模板

常用参数：

| 参数 | 默认值 | 说明 |
|---|---|---|
| `-addr` | `127.0.0.1:8443` | HTTPS/mTLS 监听地址 |
| `-data-dir` | `./gateway-data` | CA、数据库与节点记录目录 |
| `-http-addr` | 空（关闭） | 可选的**明文** HTTP 监听地址，用于免证书访问 WebUI 与 MCP |
| `-hosts` | 空 | 追加到服务端证书的额外主机名或 IP，逗号分隔 |
| `-invitations` | `1` | 启动时打印的邀请数量 |
| `-invite-ttl` | `15m` | 邀请有效期 |
| `-expire-sweep` | `5s` | 失联租约转入 unknown 的扫描间隔 |

让局域网内其他机器接入时，需显式放开监听并声明证书名称：

```sh
./bin/mesh server -addr 0.0.0.0:8443 -hosts 192.168.1.10,gateway.local
```

若绑定通配地址却未声明 `-hosts`，其他机器会因证书名称不匹配而报 x509 错误；网关启动时会就此给出告警。

追加邀请（网关运行中亦可）：

```sh
./bin/mesh invite -data-dir ./gateway-data            # 新签一个
./bin/mesh invite -data-dir ./gateway-data -list     # 查看还有几个可用
```

## 4. 接入工作节点

在目标机器上配对：

```sh
./bin/mesh pair \
  --server https://<网关地址>:8443 \
  --ca /path/to/ca.crt \
  --token <邀请令牌> \
  --dir ./node
```

CA 证书必须带外传递（从网关终端输出的路径复制，或从 `/ca.crt` 端点下载并用指纹核对）。`--dir` 默认为 `./node`，`--write-config` 默认开启，会生成起步用的 `node.json`。

启动节点任务循环：

```sh
./bin/mesh node --config ./node/node.json
```

## 5. 签发操作员凭证

```sh
./bin/mesh credential issue --role admin --out operator.token
```

| 参数 | 默认值 | 说明 |
|---|---|---|
| `-data-dir` | `./gateway-data` | 网关数据目录 |
| `-role` | `admin` | `admin`、`operator` 或 `viewer` |
| `-nodes` | 空 | 逗号分隔的节点作用域；`admin` 留空 |
| `-ttl` | `365d` | 有效期，上限 365 天 |
| `-out` | 空 | 令牌输出文件；省略则打印到标准输出 |

撤销：`./bin/mesh credential revoke <principal-id>`

## 6. 后台常驻（可选）

```sh
./bin/mesh service install --role server
./bin/mesh service start   --role server
./bin/mesh service status  --role server --json
```

| 动作 | 说明 |
|---|---|
| `install` | 注册后台服务 |
| `uninstall` | 注销后台服务 |
| `start` / `stop` | 启动 / 停止 |
| `status` | 查看状态与进程信息，`--json` 输出机器可读格式 |

安装参数：`--bin`（可执行文件路径，默认当前二进制）、`--working-dir`、`--log-dir`、`--addr`、`--data-dir`、`--config`、`--node-dir`、`--args`；`--role` 可选 `server` 或 `node`。

平台支持：

- **macOS**：`launchd` 用户级 LaunchAgent
- **Linux**：`systemd` 用户级 unit
- **Windows**：**未支持**。`mesh service` 会返回 `system service is not supported on this platform`，请改用 Windows 任务计划程序自行配置

## 7. 配置方式与环境变量

配置优先级：**命令行参数 > 环境变量 > `.env` 文件 > 内置默认值**。

CLI 启动时会从当前工作目录读取 `.env`（可用 `MESH_ENV_FILE` 指定其他文件），支持 `export ` 前缀、`#` 注释与成对引号。

常用变量：

| 变量 | 对应参数 |
|---|---|
| `MESH_SERVER_ADDR` / `MESH_ADDR` | 网关 HTTPS 监听地址 |
| `MESH_HTTP_ADDR` | 明文 HTTP 监听地址 |
| `MESH_DATA_DIR` | 网关数据目录 |
| `MESH_HOSTS` | 服务端证书附加主机名 |
| `MESH_NODE_DIR` / `MESH_NODE_CONFIG` | 节点目录 / 节点配置文件 |
| `MESH_SERVER_URL` / `MESH_SERVER` | 网关地址 |
| `MESH_CA_FILE` / `MESH_CA` | 网关 CA 证书路径 |
| `MESH_OPERATOR_TOKEN` / `MESH_TOKEN` | 操作员令牌 |
| `MESH_PAIR_TOKEN` / `MESH_INVITATION_TOKEN` | 配对邀请令牌 |
| `MESH_TOKEN_FILE` | 操作员令牌文件 |
| `MESH_NODE_ID` | 目标节点 ID |
| `MESH_ENV_FILE` | 指定 `.env` 文件路径 |

`.env` 含令牌与私钥路径，**不要提交到版本库**。

## 8. 安全注意事项

- **明文 HTTP 默认关闭**。`-http-addr` 为空表示不监听明文端口。仅当需要免证书访问 WebUI 或 MCP 时才开启，并建议只绑定本机：
  ```sh
  ./bin/mesh server -http-addr 127.0.0.1:18080
  ```
  设为 `0.0.0.0:<port>` 会把**明文**管理控制台与 MCP 端点暴露到所有网卡，只应在完全可信的网络中进行。
- **默认只监听 `127.0.0.1:8443`**。对局域网开放需同时设置 `-addr` 与 `-hosts`，并确认证书名称与访问地址一致。
- **启动日志包含敏感信息**：管理员账号密码与操作员令牌会打印到标准输出。重定向日志时请限制文件权限，不要把日志贴到公开位置。
- **CA 私钥位于数据目录**中，请按文件权限保护该目录并做好备份；丢失后所有节点需重新配对。
- **邀请令牌是一次性凭证**，默认 15 分钟过期，泄露后应立即让其过期并重新签发。

## 9. 卸载

```sh
./bin/mesh service stop    --role server
./bin/mesh service uninstall --role server
```

随后删除数据目录（默认 `./gateway-data`）与节点目录即可。节点侧同理：停止进程后删除安装目录（默认 `$HOME/.agent-mesh-node`）。
