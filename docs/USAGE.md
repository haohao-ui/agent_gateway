# 使用手册

本文说明 Agent Gateway 的日常使用方式：提交与跟踪任务、管理节点、传输产物、接入 MCP 与 Web 控制台、以及排障。

安装与部署见 [INSTALL.md](INSTALL.md)。

## 1. 命令总览

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

具体参数用 `mesh <command> --help` 查看；`mesh --version` 查看版本。

## 2. 两种调用身份

任务相关命令有两种身份，**不要混用**：

| 身份 | 认证方式 | 权限范围 |
|---|---|---|
| **节点身份** | 配对后保存在节点目录中的机器证书 | 只能看到**本机**的任务：提交给本机、也只有本机能读取或取消 |
| **操作员身份** | `credential issue` 签发的令牌文件 | 受角色与节点作用域限制，可跨节点查询、重排与裁决 |

节点身份用 `--node-dir <节点目录>`；操作员身份用 `--server <网关地址> --ca <CA 文件> --token-file <令牌文件>`，提交时还需 `--node-id <目标节点>`。

## 3. 任务

### 提交

```sh
# 节点身份：任务排给本机
./bin/mesh task submit --node-dir ./node \
  --capability agent.run \
  --input '{"prompt":"整理当前目录并生成摘要"}'

# 操作员身份：任务派给指定节点
./bin/mesh task submit \
  --server https://127.0.0.1:8443 --ca ./gateway-data/ca.crt --token-file operator.token \
  --node-id <node-id> \
  --capability agent.run \
  --input '{"prompt":"..."}'
```

| 参数 | 默认值 | 说明 |
|---|---|---|
| `--capability` | `agent.run` | 能力名称 |
| `--capability-version` | `1` | 能力版本 |
| `--input` | 空 | 能力输入，JSON 字符串 |
| `--timeout` | `300` | 任务超时（秒） |
| `--key` | 空 | 幂等键：相同键与相同输入重复提交会返回首个任务 |

### 查询与取消

```sh
./bin/mesh task get    --node-dir ./node <task-id>
./bin/mesh task cancel --node-dir ./node <task-id>
```

### 操作员：对账、重排与裁决

任务失联后会进入 `unknown` 状态，需要人工处理：

```sh
# 列出待核对的任务
./bin/mesh task list --server https://127.0.0.1:8443 \
  --ca ./gateway-data/ca.crt --token-file operator.token --state unknown

# 重排回 queued（旧节点的滞后写入会被拒绝）
./bin/mesh task requeue --server ... --ca ... --token-file ... <task-id>

# 强制裁决为终态
./bin/mesh task resolve --server ... --ca ... --token-file ... \
  --state failed --reason "人工判定失败" <task-id>
```

`--state` 用于 `resolve` 时可选 `failed` 或 `cancelled`；用于 `list` 时默认 `unknown`。

## 4. 节点与设备

```sh
./bin/mesh node list                     # 等价于 mesh device list
./bin/mesh device revoke <node-id> \
  --server https://127.0.0.1:8443 --ca ./gateway-data/ca.crt --token-file operator.token
```

`node list` 返回每个节点的 ID、证书有效期、是否已撤销，以及节点上探测到的可用 Agent 工具链。撤销后该节点的证书立即失效；已建立的连接在下一次校验时被拦截。

## 5. 产物传输

```sh
# 本机直连数据目录
./bin/mesh file upload   --data-dir ./gateway-data <task-id> ./report.md
./bin/mesh file list     --data-dir ./gateway-data <task-id>
./bin/mesh file download --data-dir ./gateway-data <task-id> report.md --output ./report.md
./bin/mesh file delete   --data-dir ./gateway-data <task-id> report.md

# 远程网关
./bin/mesh file upload --server https://127.0.0.1:8443 \
  --ca ./gateway-data/ca.crt --token operator.token <task-id> ./report.md
```

相关参数：`--name`（远端文件名，默认取本地文件名）、`--output`（下载保存路径）、`--json`（列表以 JSON 输出）。产物存放在受沙箱限制的目录中，路径穿越（`..`、绝对路径、符号链接逃逸）会被拒绝；大文件支持 HTTP Range 断点续传。

## 6. Web 控制台

网关启动时会在终端打印控制台地址与登录账号密码：

```text
web dashboard:   https://127.0.0.1:8443/ui/
web login:       username: <用户名> | password: <密码>
```

控制台提供任务列表与详情、节点列表、实时日志流、unknown 任务对账与系统诊断。

相关端点：

| 端点 | 说明 |
|---|---|
| `GET /ui/` | 管理控制台 |
| `GET /v1/events/stream` | SSE 实时事件流（带心跳保活） |
| `GET /v1/doctor` | 控制台用的自检数据 |
| `GET /api/public/status` | 免认证的轻量状态，便于外部探活 |
| `GET /onboarding.md` | 面向 AI 宿主的自助对接说明 |
| `GET /ca.crt` | 下载网关 CA 证书（用于带外核对指纹） |
| `GET /download/mesh`、`/download/mesh.sha256` | 下载节点二进制及其校验值 |
| `GET /download/install.sh`、`/download/install.ps1` | 一键安装脚本 |
| `GET /download/SKILL.md` | 下载内置技能说明 |

## 7. MCP 接入

MCP 服务提供两类入口，供 Claude Desktop、Cursor、Cline 等支持 MCP 的宿主调用。

### stdio（本地进程）

```sh
# 直连本地数据目录
./bin/mesh mcp --data-dir ./gateway-data

# 连接远程网关
./bin/mesh mcp --server https://127.0.0.1:8443 \
  --ca ./gateway-data/ca.crt --token <操作员令牌>
```

宿主配置示例：

```json
{
  "mcpServers": {
    "agent-gateway": {
      "command": "/path/to/mesh",
      "args": ["mcp", "--data-dir", "/path/to/gateway-data"]
    }
  }
}
```

### HTTP

网关直接挂载 `/mcp`，启动时会打印带令牌的完整地址：

```text
mcp (HTTPS):     https://127.0.0.1:8443/mcp?token=<操作员令牌>
```

该端点受操作员令牌鉴权保护。若同时开启了明文 HTTP（`-http-addr`），会额外提供一个 `http://` 地址，仅建议绑定本机使用。

### 工具清单

| 工具 | 别名 | 用途 |
|---|---|---|
| `task_submit` | `submit_task` | 提交任务 |
| `task_get` | `get_task_result` | 查询任务状态与结果 |
| `wait_task_result` | — | 同步等待任务完成（最长挂起 120 秒） |
| `task_cancel` | `cancel_task` | 取消任务 |
| `node_execute` | `execute_on_node` | 在指定节点执行 |
| `handoff_to_computer_agent` | — | 择机把任务派发给最合适的节点 |
| `device_list` | `list_devices` | 列出已配对节点及其状态与工具链 |
| `doctor_diagnose` | — | 环境与连通性诊断 |

## 8. 诊断

```sh
# 节点侧自检
./bin/mesh doctor --node-dir ./node

# 服务端自检
./bin/mesh doctor --server-mode --data-dir ./gateway-data

# 机器可读输出
./bin/mesh doctor --server-mode --data-dir ./gateway-data --json
```

检查项包括证书有效期与临期预警、时钟漂移、数据库完整性以及节点/服务端连通性。建议在配对失败、任务长期停滞或证书即将过期时先运行。

## 9. 常见问题

| 现象 | 排查方向 |
|---|---|
| 节点连接报 x509 证书名称不匹配 | 网关未声明该访问地址。用 `-hosts` 追加主机名或 IP 后重启 |
| `mesh pair` 报邀请无效 | 邀请令牌一次性且默认 15 分钟过期，用 `mesh invite` 重新签发 |
| 其他机器访问不到网关 | 网关默认只监听 `127.0.0.1:8443`，需显式设置 `-addr 0.0.0.0:8443` |
| 任务长期停在 `unknown` | 节点失联或崩溃。用 `task list --state unknown` 查看后用 `requeue` 或 `resolve` 处理 |
| 需要免证书访问控制台 | 仅在可信网络下用 `-http-addr 127.0.0.1:<port>`，不要绑到 `0.0.0.0` |
| Windows 上 `service install` 失败 | Windows 未支持用户级后台服务，请用任务计划程序 |

排障时优先使用 `mesh doctor`，它会把上述多数问题直接定位到具体配置项。
