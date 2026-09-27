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

### 一键安装网关（macOS / Linux，推荐）

```sh
curl -fsSL https://github.com/haohao-ui/agent_gateway/releases/latest/download/install.sh | sh
```

Windows：

```powershell
irm https://github.com/haohao-ui/agent_gateway/releases/latest/download/install.ps1 | iex
```

发布页附带的安装脚本由发版流水线生成，**内嵌该版本每个平台二进制的 SHA-256**（取自该版本的已签名清单）：

- 只依赖 `curl` 与 `sha256sum`/`shasum`（Windows 用内置 `Get-FileHash`），无需额外依赖；
- 校验不通过立即终止，**不落盘、不赋可执行权限**；
- 本机若存在可用验签后端（支持 Ed25519 的 `openssl`，或带 `cryptography` 的 `python3`），会额外用内嵌公钥验证清单签名，并**要求签名清单里的目标条目与下载到的二进制一致**，把校验等级提升为完整验签；
- 默认安装到 `~/.local/bin`，原程序备份为 `mesh.bak`，许可声明安装到 `agent-gateway-notices`；不修改 shell 配置、不注册后台服务；
- 安装结束后会打印本次达到的**校验等级**，不掩饰降级。

指定版本或目录：

```sh
curl -fsSL .../install.sh | sh -s -- --version v0.1.3
curl -fsSL .../install.sh | sh -s -- --dir "$HOME/bin"
```

### 强校验安装（不依赖下载服务器）

需要不依赖下载服务器的保证时，带外获取发行公钥（从已审核源码构建的 `mesh` 也可作为独立验证器）：

```sh
# 带外公钥 + 本机验签后端：验签后按签名清单安装
sh install.sh --public-key /trusted/release.pub

# 独立验证器：最强路径
sh install.sh --verifier /trusted/mesh --public-key /trusted/release.pub
```

```powershell
./install.ps1 -PublicKey C:\trusted\release.pub
./install.ps1 -Verifier C:\trusted\mesh.exe -PublicKey C:\trusted\release.pub
```

带外固定公钥指纹可防止脚本内嵌公钥被替换：

```sh
sh install.sh --fingerprint 88bdfc0623fa313e29567c48b1c64fc0cba6f0cb9afe07c890c808c26b631216
```

### 信任模型

| 路径 | 信任锚 | 是否做签名验证 |
|---|---|---|
| 一键安装（无参数） | HTTPS 传输 + 脚本内嵌 SHA-256（取自该版本的已签名清单） | 本机有可用后端时自动加上，并交叉核对签名清单 |
| `--public-key` | 带外公钥 | 是（必须成功，否则终止） |
| `--verifier` + `--public-key` | 带外验证器与公钥 | 是（最强，含平台条目校验） |

一键路径的信任锚在这份脚本内容与 HTTPS 上：能改脚本的人同样能改证书，所以它**不声称**替代带外验签。要把保证提升到不依赖下载服务器，请用 `--public-key` 或 `--verifier`。任何路径都**不提供跳过校验的选项**。

仓库根目录的 `install.sh` / `install.ps1` 未注入指纹（指纹只在发版时写入发布页附带的那份），直接运行仓库副本会明确报错并给出替代用法，不会静默降级。

### 方式一：验证发行包后安装节点

发行公钥与验证程序必须独立可信，不能从同一个待验证下载源取得后直接信任。可从已审核源码构建 mesh 验证器，或通过独立可信渠道交付。网关部署签名发行包到 `<data-dir>/dist/`。

macOS / Linux 先准备可信脚本（仓库 `internal/httpapi/install.sh`）、验证器、公钥、网关 CA，以及私有的 `invitation.txt` 文件：

```sh
export MESH_VERIFY_BIN=/trusted/mesh
export MESH_RELEASE_KEY=/trusted/release.pub
export MESH_TLS_CA=/trusted/gateway-ca.crt
bash ./install.sh https://gateway.example:8443 ./invitation.txt ./node
```

Windows 使用可信的 `internal/httpapi/install.ps1`：

```powershell
$env:MESH_VERIFY_BIN = 'C:\trusted\mesh.exe'
$env:MESH_RELEASE_KEY = 'C:\trusted\release.pub'
$env:MESH_TLS_CA = 'C:\trusted\gateway-ca.crt'
./install.ps1 -Server https://gateway.example:8443 -TokenFile ./invitation.txt -Dir ./node
```

脚本使用已可信的 mesh 验证器执行 HTTPS 下载、Ed25519 清单验签、文件大小/SHA-256/平台校验，并验证 LICENSE、NOTICE、THIRD_PARTY_NOTICES。缺失任何校验材料立即停止，不回退到同源 SHA-256。目标文件存在时拒绝覆盖。邀请码从文件经 stdin 传给配对命令，不进入 URL 或 argv。

配对后需检查 `node.json` 并手动启动节点；脚本不安装系统服务或自动执行 Agent。不要用未验证二进制自己验证自己，也不要把下载脚本通过 `curl | bash` / `irm | iex` 直接执行。

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
- 显式请求生成启动邀请码时，打印私有 startup-invitations.json 路径
- 操作员令牌文件 `admin.token` 的路径（默认有效期 24 小时）
- Web 控制台 HTTPS 地址与用户名；密码从私有 `admin.password` 文件读取，或由 `MESH_ADMIN_PASSWORD` 配置
- 节点配对命令模板

常用参数：

| 参数 | 默认值 | 说明 |
|---|---|---|
| `-addr` | `127.0.0.1:8443` | HTTPS/mTLS 监听地址 |
| `-data-dir` | `./gateway-data` | CA、数据库与节点记录目录 |
| `-http-addr` | 空（关闭） | 可选 HTTP 拒绝监听器；返回 426，不能登录、管理或接入 MCP |
| `-hosts` | 空 | 追加到服务端证书的额外主机名或 IP，逗号分隔 |
| `-invitations` | `0` | 启动时写入私有 startup-invitations.json 的邀请数量 |
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
| `-ttl` | `24h` | 有效期，上限 24 小时 |
| `-out` | 空 | 令牌输出文件；省略则打印到标准输出 |

撤销：`./bin/mesh credential revoke --data-dir ./gateway-data <principal-id>`。控制台“证书与设置”中也可查看并撤销凭据；API 为 `GET /v1/operator/credentials?after=<cursor>`（每页最多100条）及 `POST /v1/operator/credentials/{id}/revoke`，仅管理员可用。

登录创建独立的 8 小时可撤销会话，Secure / HttpOnly / SameSite=Strict Cookie 不复用管理员 API 令牌。退出会撤销当前会话。每来源登录突发上限5次、每30秒恢复1次，另有全局限流，超过返回429及 Retry-After。代理头不能改变限流来源。

首次升级启动会把旧凭据的总寿命收紧到签发后24小时，较早签发的旧令牌立即失效；需重新签发客户端凭据。已有撤销状态不会恢复。

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

- **管理接口仅支持 HTTPS**。即使配置 `--http-addr` 也返回426；不信任 X-Forwarded-Proto，TLS 终止代理需以 HTTPS 回源。默认只监听127.0.0.1:8443，对外开放须设置证书 hosts。
- **凭据不进入启动日志**。管理员密码和 API 令牌只显示文件路径；显式 `credential issue` 省略 `--out` 时仍会按请求输出新令牌。旧日志里的泄露凭据需撤销，不能仅靠更新程序。
- **URL token 已停用**。MCP 客户端须在每个请求发送 Authorization: Bearer，请勿把令牌拼进 URL。浏览器使用安全 Cookie。
- **SSE 撤销和过期生效**。每个主体最多4条事件/MCP GET流，总共64条；逐帧复核认证并每250ms检查外部CLI撤销，空闲连接也断开。已进入网络的字节不能追回；阻塞写有5秒截止时间。
- **CA 私钥位于数据目录**中，请按文件权限保护该目录并做好备份；丢失后所有节点需重新配对。
- **邀请令牌是一次性凭证**，默认 15 分钟过期，泄露后应立即让其过期并重新签发。

## 9. 卸载

```sh
./bin/mesh service stop    --role server
./bin/mesh service uninstall --role server
```

随后删除数据目录（默认 `./gateway-data`）与节点目录即可。节点侧同理：停止进程后删除安装目录（默认 `$HOME/.agent-mesh-node`）。

## 10. 签名发行与升级

离线保存发行私钥（不能部署到网关）。首次由发行负责人生成，公钥通过独立渠道分发：

```sh
mesh release keygen --dir /offline/keys
bash scripts/build-release.sh 0.1.2 1 /offline/keys/release.key /trusted/release.pub /new/release-dir
mesh release verify --dir /new/release-dir --public-key /trusted/release.pub
```

目录须预先存在以生成密钥；keygen 拒绝覆盖密钥。每次发行使用严格递增且不复用的 sequence。构建脚本生成六平台二进制、收集完整依赖许可证文本，签署含版本、到期时间、sequence、文件名、大小与SHA-256的清单。默认清单30天到期。将整个发行目录交付，保留三个许可声明文件。

节点自升级仅从配置的 HTTPS 网关源取签名发行文件，禁止明文和重定向。须先在节点目录带外放置 `release.pub`；没有公钥即拒绝升级。校验成功后保留 `.previous` 备份再替换，并保存 `release.sequence` 防止较旧序号回退；同序号允许失败重试，发行方不得复用序号。替换失败尝试恢复备份。备份恢复仍需运维人员按数据库兼容情况决定；没有宣称自动健康检查或跨平台升级回滚验收完成。

`/download/mesh.sha256` 仅用于非认证的完整性诊断，不是可信安装依据。下载证书、摘要或二进制的 HTTP 200 也不能证明其发行身份。

### GitHub Actions 发布配置

发布流水线先执行测试、race、vet、gofmt 和固定版本 govulncheck，再构建六个平台。`sign-and-publish` 使用受保护的 `release-signing` Environment；仓库维护者需自行配置审核人、环境 secret `RELEASE_SIGNING_KEY_PEM` 以及变量 `RELEASE_PUBLIC_KEY_PEM`。本次代码修改不创建或上传任何真实密钥。

签名前验证私钥/独立公钥匹配；缺少密钥即失败，没有无签名回退。私钥只临时写入运行器私有目录，不上传为产物。发布包保留 LICENSE、NOTICE、THIRD_PARTY_NOTICES、manifest.json 和 manifest.sig。现有 Release 不覆盖，需新 tag；序号按 workflow run number / attempt 递增。若变更 workflow 导致运行编号重置，应重新规划发行序号，不能降低节点已记录的序号。私钥也可完全离线使用 build-release.sh；不要求交给 CI。

首次发布的 verifier、公钥以及 installer 本身仍需独立可信交付。这是信任引导条件，不能由“同一下载服务器同时提供公钥和程序”代替。本机未执行 GitHub Actions，也未设置仓库或环境凭据。
