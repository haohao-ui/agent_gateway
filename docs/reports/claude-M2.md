# Claude Code / M2 交付报告

日期：2026-09-26。工作区：`/Users/yang/tools/code/agent-gateway`（main 分支工作区，**未使用独立 worktree**，见 §11）。

范围：`internal/identity/**`、`internal/httpapi/**`、`internal/node/**`、`cmd/mesh/**`、本报告。

未修改：协调者冻结的 `internal/protocol/**`（含 `wire.go`）、`docs/CONTRACTS.md`、`docs/COORDINATION.md`、`go.mod`/`go.sum`、`internal/taskstore/**`、`internal/runner/**`、用户级配置或全局设置。未提交、未推送、未部署、未安装服务、未调用真实 Agent CLI、未递归派生 agent。

本报告是 M2 的实现交付说明，**不是 M2 集成验收，也不代表 production ready**；§9 列出的未实现项不得被当作已完成。

## 1. 结论

- 上一轮 antigravity 会话产出的 `internal/identity`、`internal/httpapi` 初版无法编译（未使用 import），且按冻结契约跑不通任务闭环。本轮修复并补齐，保留其设计意图（VerifyClientCertIfGiven + 路由级强制、SPIFFE URI SAN、邀请一次性）。
- 冻结契约的 5 条路由全部实现；补齐契约缺失但状态机必需的 `POST /v1/tasks/start`（§8 G1）。
- 新增 `internal/node`（节点循环、本地 journal、outbox、单实例锁）与 `cmd/mesh`（server/invite/pair/node/task 五组子命令），使项目首次具备**可运行的端到端闭环**：起网关 → 配对 → 任务入队 → 节点领取执行 → 结果回传 → 状态可查（§6.3 为实际 CLI 记录）。
- 邀请改为落盘（`<data-dir>/invitations/`，目录内每个邀请一个文件，只存 hash）并新增 `mesh invite`，**网关运行中也能给新机器签发邀请**，不需要任何新 HTTP 路由（§6.3 有实测）。
- 全量验证通过：`gofmt`、`go vet ./...`、`go test ./...`、`go test -race ./...` 全部干净；6 组 GOOS/GOARCH 交叉编译通过，Windows/Linux 的 `go vet`（含测试文件类型检查）通过。
- **两台真实 macOS 机器（192.168.3.237 网关 ↔ 192.168.3.81 节点，局域网）跑通完整闭环**：远端配对 → 长轮询领取 → 真实子进程执行 → 结果回传，见 §6.4。这是本项目首个跨机器证据。
- 已知契约偏差 10 项、未实现 7 项，全部列于 §8、§9 并附拟定处置，交由协调者裁决。

## 2. 改动清单

### 2.1 `internal/identity`（配对与 PKI）

| 文件 | 内容 |
|---|---|
| `ca.go` | `SignNodeCSR` 增加受支持密钥类型校验（仅 ECDSA P-256）；邀请不再放在内存 map，改为委托给落盘 store；`gofmt` 修正字段对齐 |
| `ca_test.go` | 新增 `TestCA_RejectsUnsupportedCSRKeyTypes`（RSA / P-384），并断言拒绝后邀请保持已消费 |
| `invitations.go` | 新增落盘邀请 store：`Issue`/`Consume`/`Pending`/`Used` 与离线签发 `IssueInvitation`；`Consume` 用原子 rename 保证一次性，无锁并发安全 |
| `invitations_test.go` | 新增 8 个测试：签发/消费/重放、过期、TTL 上限、跨进程存活、目录内不出现明文 token 且权限 0600、重启后旧邀请仍可用、离线签发被运行中的网关接受、非网关目录拒绝签发 |
| `client.go` | 未改动（原实现已满足 mTLS + h2 + TLS 1.3 客户端配置） |

### 2.2 `internal/httpapi`（网关 API）

| 文件 | 内容 |
|---|---|
| `server.go` | 包注释声明未实现项；全部任务路由纳入 `requireNodeAuth` 并强制 `VerifiedChains`；`authorizeTask` 节点作用域；`POST /v1/tasks/start` + 幂等确认；`readJSON` 请求体限长；`pairLimiter` 配对限流；`renew` 返回权威到期时间；`handleSubmit` 身份绑定；`handleEvents` 归属与 sequence 校验；导出 `StartRequest` 供节点复用 |
| `server_test.go` | 重写为 12 个测试：闭环、越权、重传幂等、断连、限流、限长、外部 CA、ALPN h2 + TLS 1.3 断言 |

### 2.3 `internal/node`（节点循环，新增）

| 文件 | 内容 |
|---|---|
| `config.go` | `node.json` 配置、默认值、严格校验（未知字段拒绝、续租间隔必须小于租约）、能力白名单、指令提取、work_dir 回退 |
| `client.go` | mTLS HTTP/2 客户端：Claim/Start/Renew/Complete/Cancel/Get/Submit；把网关拒绝分类为可分支的哨兵错误；错误信息不含 token |
| `pairing.go` | 节点侧配对：本地生成密钥 + CSR → `/v1/pair` → 校验响应 CA 与信任 CA 一致 → 原子写盘（0600）；拒绝覆盖已有身份 |
| `journal.go` | 本地 journal（JSONL + fsync），`claimed/starting/result_pending/acknowledged/lease_lost/aborted/rejected/needs_reconciliation`，容忍崩溃撕裂行 |
| `outbox.go` | 结果先落盘再上报；文件名安全校验；不可接受的结果移入 `rejected/` 保留证据 |
| `lock_unix.go` / `lock_windows.go` | 单实例锁（Unix `flock`；Windows `LockFileEx`），进程退出即释放 |
| `node.go` | 主循环：claim → journal → start（可重传）→ 能力校验 → 执行（任务自带期限 + 并行续租 + 取消探测）→ outbox → 重试上报；启动时 reconcile |

### 2.4 `cmd/mesh`（CLI，新增）

| 文件 | 内容 |
|---|---|
| `main.go` | 子命令分发、SIGINT/SIGTERM 优雅退出、slog 日志级别 |
| `server.go` | 起网关：SQLite + CA + HTTP/2 mTLS listener + 租约到期清扫 + 打印邀请与 CA 指纹；优雅关闭 |
| `pair.go` | 节点侧配对，并生成可直接使用的 `node.json` 骨架；`--token -` 支持从 stdin 读取邀请密钥，避免密钥进入 argv 与 shell 历史 |
| `invite.go` | `mesh invite`：为网关数据目录离线签发邀请，支持 `--ttl` / `--count` / `--list` |
| `server_test.go` | `certHostWarning`（绑通配地址却没给 `--hosts` 时告警）、`reachableAddr`（提示里不打印通配地址）、`resolveToken`（`--token -` 从 stdin 读） |
| `node.go` | 运行节点循环 |
| `task.go` | `task submit/get/cancel`，以节点证书身份操作任务 |
| `e2e_test.go` | 真实进程端到端测试 + 生成配置可被加载的回归测试 |

## 3. 路由与冻结契约的对应关系

| 路由 | 契约状态 | 认证 | 作用域 | 主要状态码 |
|---|---|---|---|---|
| `POST /v1/pair` | 已冻结 | 服务端 TLS + 邀请 + 限流 | 公开 | 200 / 400 / 401 / 429 |
| `POST /v1/tasks/claim` | 已冻结 | mTLS | 证书 `NodeID` | 200 / 204（25s 无任务）/ 400 / 401 |
| `POST /v1/tasks/renew` | 已冻结 | mTLS | 任务属主 | 200 / 400 / 401 / 404 / 409 / 413 |
| `POST /v1/tasks/complete` | 已冻结 | mTLS | 任务属主 | 200 / 400 / 401 / 404 / 409 / 413 |
| `POST /v1/tasks/events` | 已冻结 | mTLS | 任务属主 | 200（仅 ACK，§9）/ 400 / 404 |
| `POST /v1/tasks/start` | **未冻结（新增）** | mTLS | 任务属主 | 200 / 400 / 401 / 404 / 409 / 413 |
| `POST /v1/tasks/submit` | **未冻结** | mTLS | 必须等于证书身份 | 201 / 400 / 403 / 413 |
| `GET /v1/tasks/{id}` | **未冻结** | mTLS | 任务属主 | 200 / 404 |
| `POST /v1/tasks/{id}/cancel` | **未冻结** | mTLS | 任务属主 | 200 / 404 |

`protocol.*` 冻结类型直接复用，未新增共享类型；`httpapi.StartRequest` 定义在 API 包内并由节点客户端复用，避免两处重复定义线上结构。

## 4. 授权与传输设计

- **分层 TLS**：与 M2-DESIGN 一致。单 listener 用 `tls.VerifyClientCertIfGiven` 承载邀请与设备入口；设备路由额外强制 `r.TLS.VerifiedChains` 非空，身份取自已验证链的叶子证书，而不是客户端自报的 `PeerCertificates[0]`。
- **身份不可由输入决定**：所有 actor 取自证书 URI SAN（`spiffe://agent-gateway/nodes/<nodeID>`），请求体里的 `NodeID` 只作为一致性断言，不匹配即 403。
- **作用域**：`authorizeTask` 在调用 store 之前做属主判定，跨节点访问统一 404，避免探测任务 ID 是否存在；越权请求不改变属主任务状态。
- **信任锚只在带外**：`mesh pair` 必须提供 `--ca`，节点用它验证服务端，并额外校验配对响应里的 CA 与信任 CA **DER 完全一致**（§7 有测试），全程不使用 `InsecureSkipVerify`。
- **资源限制**：请求体解析前限长（凭据 8 KiB / 结果 256 KiB / 提交 128 KiB / 事件 256 KiB），超限 413；`/v1/pair` 按来源地址令牌桶限流（5/s、突发 10），桶数量有上限，不使用 `X-Forwarded-For`。
- **租约时间**：`renew` 返回 store 在写事务内记录的到期时间，不用本地时钟二次推算。

## 5. 节点执行模型与不确定性处理

设计依据：ADR-009（不确定执行进 unknown、不自动重跑）与 M2-DESIGN「任务操作可靠性」。

| 情形 | 节点行为 | 证据 |
|---|---|---|
| 正常完成 | 结果先写 outbox + journal(`result_pending`)，再上报，ACK 后**先**写 `acknowledged` 再删 outbox（顺序颠倒会让一次崩溃把已成功的任务误报成待核对） | `TestNodeExecutesTaskAndReportsResult` |
| 任务自带超时 | 把 `timeout_seconds` 作为执行硬期限，adapter 被切断，上报 `failed` + `error_code=timeout`（这是任务自身的期限，与「节点关闭」不同，不并入 unknown） | `TestNodeStopsTaskAtItsOwnDeadline` + §6.3 CLI 实测 |
| ACK 丢失 / 上报失败 | 指数退避 + 抖动重试（上限 6 次），失败则留在 outbox 等下次启动重投 | `TestReconcileReportsAStoredResult` |
| 续租成功 | 用网关返回的权威到期时间；同时读任务状态以发现取消请求 | `TestNodeKeepsLeaseAliveWhileRunning`（断言到期时间确实前移） |
| **租约丢失** | 立即停进程、**不上报任何结果**、journal 记 `lease_lost`；任务由网关 Expire 转 unknown | `TestNodeLeaseLossStopsTheTaskAndDoesNotReport`（断言只启动 1 次、outbox 为空、任务变 unknown 且无 result） |
| 取消请求 | 探测到 `cancel_requested` 后停进程并上报 `cancelled`，保留真实输出文本 | `TestNodeStopsOnCancellationRequest` |
| 节点关闭时任务未完成 | 不猜测结果，不提交终态，等租约到期转 unknown | 同上 lease-lost 路径 + 关闭分支 |
| 崩溃残留（claimed/starting 无结果） | 重启时**拒绝重跑**，journal 记 `needs_reconciliation` 并告警 | `TestReconcileRefusesToRerunAnInterruptedAttempt` |
| 双实例 | 目录级排他锁，第二个实例启动即失败 | `TestNodeRefusesToRunTwiceInOneDirectory` |
| 能力不在本地白名单 | 拒绝执行（先 Start 以便上报终态），返回 `unsupported_task` | `TestNodeRefusesCapabilityItDoesNotServe` |
| adapter 配置/启动失败 | 上报 `failed` + `error_code=adapter_failed`，文本为本地错误信息 | CLI 实测：work_dir 缺失时任务进入 failed 且错误可读（修复过程见 §6.3） |

## 6. 检查命令与真实结果

全部于 2026-09-26 在本机（darwin/arm64，Go 1.27.1）复跑。

### 6.1 合并门槛

```sh
gofmt -l ./internal ./cmd        # 无输出
go vet ./...                     # 退出 0，无输出
go test -count=1 ./...           # 全包 ok（cmd/mesh、httpapi、identity、integration、node、runner、taskstore）
go test -race -count=1 ./...     # 全包 ok，0 数据竞争
git diff --check                 # 仅 docs/CONTRACTS.md:102 报既有告警（协调者文件，见 §8 G8）

# 交叉编译（产品）
GOOS/GOARCH ∈ {windows,linux,darwin} × {amd64,arm64}，CGO_ENABLED=0 go build ./...  # 6/6 OK
# 交叉编译（含测试文件类型检查）
GOOS=windows|linux GOARCH=amd64 go vet ./...                                        # 2/2 OK
```

测试项计数（`go test -v` 逐条统计）：`internal/node` 36、`internal/httpapi` 12、`internal/identity` 13、`cmd/mesh` 14，合计 **75 项全部 PASS**。

### 6.2 端到端测试（真实进程）

`cmd/mesh/e2e_test.go:TestGatewayEndToEnd` 在同一进程内跑真实网关（真实 SQLite + 真实 CA + 真实 HTTP/2 mTLS listener）、真实配对、真实节点循环与真实子进程执行，断言任务到达 `succeeded` 且结果文本包含 prompt。

### 6.3 CLI 实测记录（本机真实命令行）

```sh
mesh server --addr 127.0.0.1:9443 --data-dir /tmp/mesh-smoke/gw --invitations 1 --invite-ttl 10m
# listening:   https://127.0.0.1:9443
# CA cert:     /tmp/mesh-smoke/gw/ca.crt
# CA SHA-256:  ef6fb892df60b6e98221df5b3c22e5d2052ea8ad3d3ca6f7e13067e4c7cac36b
# invitation:  54ac6888... (expires 2026-09-25T17:19:24Z)

mesh pair --server https://127.0.0.1:9443 --ca /tmp/mesh-smoke/gw/ca.crt --token <token> --dir /tmp/mesh-smoke/node
# paired as node-7354ed7edcf32227
# wrote:     /tmp/mesh-smoke/node/node.json

mesh node --config /tmp/mesh-smoke/node/node.json            # 后台
mesh task submit --node-dir ... --capability agent.run --input '{"prompt":"你好，执行这一条任务"}'
# task state: queued → succeeded
#   "text": "你好，执行这一条任务\n",  "exit_code": 0
# 节点日志：node started → executing task → task finished state=succeeded exit_code=0

mesh task cancel <task>     # 换用 /bin/sleep 适配器复测
# 取消前 state=running → cancel_requested → 节点日志 "cancellation requested; stopping the task"
# 8s 后 state=cancelled，error_code=cancelled，exit_code=-1
```

```sh
mesh task submit --timeout 2 ...        # 适配器为 /bin/sleep，本应运行 120s
# state=failed, error_code=timeout, exit_code=-1
# 节点日志：01:30:08.592 executing task → 01:30:10.594 task finished（2.002s）
```

```sh
# 网关以 0 邀请启动，然后在不重启的情况下给新机器签发
mesh server --addr 127.0.0.1:9446 --data-dir ./gw --invitations 0   # listening: https://127.0.0.1:9446
mesh invite --data-dir ./gw
# invitation:  1b98213244f6cc3225a25c095471b8a364a28aa051e52585 (expires 2026-09-25T18:02:22Z)
mesh pair --server https://127.0.0.1:9446 --ca ./gw/ca.crt --token <token> --dir ./node1
# paired as node-969bd57465367ba3
mesh pair ... --token <同一个token> --dir ./node2
# mesh: gateway rejected the node credentials: ... invitation has already been used   ← 一次性生效
mesh invite --data-dir ./gw --list
# pending: 1   used: 2
ls -l ./gw/invitations        # 全部 -rw-------，文件名即 hash，目录内无明文 token
```

冒烟进程已全部退出、临时目录已删除。过程中发现并修复的两个真实缺陷，均是 CLI 实测而非单元测试暴露的：

1. `mesh pair` 生成的 `node.json` 未设 `work_dir`，而 runner 要求"已存在的绝对路径"，导致任务必然 `adapter_failed`。已改为节点默认工作目录 `<node-dir>/work`（0700，与私钥目录分离），并有回归测试（`TestRunnerConfigFallsBackToTheNodeWorkDirectory`）。
2. **节点完全没有执行任务的 `timeout_seconds`**：`execute` 只包了 cancel 而没有 deadline，一个 `timeout_seconds=5` 的任务会一直跑到租约失效转 unknown。已改为把任务期限作为运行上下文的 deadline，并有测试与上面的 CLI 记录。两者都未被单元测试覆盖到，因为此前的测试都用了默认的 60s 超时。

### 6.4 跨机器验证（两台真实机器，局域网）

| 机器 | 角色 | 地址 |
|---|---|---|
| Mac（Apple Silicon，macOS 15 主机） | 网关 | `--addr 0.0.0.0:9443 --hosts 192.168.3.237` |
| Mac（Intel，macOS 12.7.6） | 节点 | 经 ssh 部署，`~/mesh-demo/node` |

步骤与实测结果：

```sh
# 1. 网关证书包含 LAN IP（--hosts 生效）
openssl s_client -connect 192.168.3.237:9443 | openssl x509 -noout -text
#   X509v3 Subject Alternative Name: DNS:localhost, IP Address:192.168.3.237, IP Address:127.0.0.1, IP Address:::1

# 2. 网关运行中离线签发邀请，token 经 stdin 传给远端（不进 argv / 历史）
mesh invite --data-dir ./gw --ttl 15m | awk '{print $2}' > /tmp/token
ssh work@192.168.3.81 '~/mesh-dist/mesh pair --server https://192.168.3.237:9443 \
  --ca ~/mesh-demo/gw/ca.crt --token - --dir ~/mesh-demo/node' < /tmp/token
#   paired as node-8eff79094130ebf0        （私钥在远端生成，未离开该机器）

# 3. 远端节点常驻（nohup，ssh 断开后仍运行）
#   node started  server=https://192.168.3.237:9443  capabilities=1

# 4. 远端以自己的身份提交任务并执行
mesh task submit --node-dir ~/mesh-demo/node --capability agent.run --input '{"prompt":"这条任务在 192.168.3.81 上执行"}'
#   state=succeeded  text="这条任务在 192.168.3.81 上执行\n"  exit_code=0
# 远端 journal：claimed(18:02:56.873) → starting → result_pending → acknowledged(…911)
```

这条证据把「mTLS + 配对 + 长轮询 + 结果回传」从"同机回环"提升到"真实局域网两台机器"。**仍未验证**：Linux/Windows 运行时、跨网段/NAT/VPN、多节点并发。

## 7. 设计要求 → 测试映射（仅列关键项）

| 来源 | 要求 | 测试 |
|---|---|---|
| CONTRACTS §1 | ALPN h2、优先 HTTP/2、TLS 1.3 | `TestHTTPAPI_PairAndUnauthenticatedAccess` |
| CONTRACTS §1 | `/v1/tasks/**` 强制 mTLS | `TestHTTPAPI_UnauthenticatedTaskRoutesAreRejected`（8 条路由） |
| CONTRACTS 基础约束 | 所有 mutation 认证/授权 | 同上 + `TestHTTPAPI_CrossNodeAccessIsRejected`（6 条路由跨节点全 404）+ `TestHTTPAPI_SubmitForAnotherNodeIsForbidden` |
| SECURITY | A 设备冒充 B | 跨节点测试 + `TestHTTPAPI_ForeignCACertificateIsRejected` + `TestPairRejectsAGatewayWithAnotherCA` |
| SECURITY | 缺失/错误凭据拒绝 | 401 用例组 + 错 token 401 |
| SECURITY | 限流 | `TestHTTPAPI_PairRateLimit`（失败尝试同样计数） |
| ENGINEERING | 解析前限制字节数 | `TestHTTPAPI_OversizedBodyIsRejected` |
| M2-DESIGN | 领取→写本地记录→确认开始→续租→执行→outbox→幂等上报 | §5 表格全部条目 |
| CONTRACTS | 任务 `timeout_seconds` 生效、架构要求"执行时间有限额" | `TestNodeStopsTaskAtItsOwnDeadline` |
| SECURITY | 中文大输出、有界尾缓冲、UTF-8 不被截断破坏（贯通 runner→节点→HTTP→store） | `TestNodeHandlesLargeUnicodeOutput`（240 KB 中文 → 保留 ≤64 KiB、`Truncated`、UTF-8 合法） |
| M2-DESIGN | 租约失效不得写新 attempt、不确定进 unknown | `TestNodeLeaseLossStopsTheTaskAndDoesNotReport` |
| M2-DESIGN | 崩溃处于 starting/running 先核对不重跑 | `TestReconcileRefusesToRerunAnInterruptedAttempt` |
| M2-DESIGN | 节点单实例锁 | `TestNodeRefusesToRunTwiceInOneDirectory` + `TestInstanceLockIsExclusiveAndReusable` |
| M2-DESIGN | 受支持密钥类型校验 | `TestCA_RejectsUnsupportedCSRKeyTypes` |
| M2-DESIGN | 邀请值只显示一次 | `TestCA_LifecycleAndCertSigning`、`TestCA_ExpiredInvitation` |
| M2-DESIGN | 邀请只存 hash、到期与消费状态；短期过期 | `TestInvitationStoreWritesNoSecret`（目录内无明文 token 且 0600）、`TestInvitationExpiry`、`TestInvitationTTLIsBounded` |
| M2-DESIGN | 网关重启或离线签发后邀请仍可用 | `TestCAAcceptsAnInvitationMintedBeforeItLoaded`、`TestInvitationSurvivesASeparateProcess`、`TestCAIssuesInvitationsWithoutThePrivateKey`、`TestInvitationMintedWhileTheGatewayRuns` |
| ENGINEERING 测试分层 4 | 通信层认证/越权/断连/重复消息 | 12 个 httpapi 测试覆盖四类 |

未覆盖（不宣称）：真实进程被 SIGKILL 后的恢复（用等价磁盘状态构造，未做真杀进程）、撤销、轮换、慢客户端/日志洪泛、多节点并发、Windows 运行时（仅交叉编译 + vet）。

## 8. 与冻结契约 / 分工的偏差，待协调者裁决

**G1（阻塞级）`/v1/tasks/start` 不在冻结路由表内，但闭环必需。** M1 `taskstore` 规定 leased 必须先 `Start` 才能成功 `Complete`，没有该路由则任何任务都无法完成。已实现并做重传幂等（同 attempt + running/cancel_requested + 租约未过期 → 200 确认）。请把该路由与语义补进 `docs/CONTRACTS.md`。

**G2（安全级，仍开放）三条非契约任务路由的 actor 模型。** `submit`/`GET {id}`/`{id}/cancel` 不在冻结契约内，原实现为**无认证公开路由**，直接违反 CONTRACTS「禁止无证书访问任务路由」与 ENGINEERING「所有 mutation 必须认证/授权」，并命中 SECURITY「查看他人任务」。现改为 mTLS + 节点作用域，但这与 M2-DESIGN「管理凭据与设备证书分离」冲突：`mesh task submit` 目前**以节点证书身份**提交。操作员/规划者 actor 需要 `internal/policy`，请裁决。

**G3（契约冲突，仍开放）`protocol.TaskEvent` 无 token 字段**，却落在「所有 mutation 按 (task_id, attempt_id, lease_token) 验证」的约束下。本轮只做归属 + sequence 校验，未自造凭据字段。另外**节点目前完全不发送事件**（runner 只在结束时返回有界输出），事件流式上报与持久化都未实现（§9）。

**G4（未实现，设计文档要求）撤销与证书轮换。** M2-DESIGN 要求「证书轮换、设备撤销、过期后重新邀请恢复必须有测试」，SECURITY 要求「连接成功仍检查撤销状态」。当前只有链验证，无 CRL/OCSP 来源。

**G5（已实现，待协调者追认存储位置）邀请持久化。** 原实现把邀请放在内存 map：网关重启即失效，运行中无法再签发。现改为 `<data-dir>/invitations/` 下每个邀请一个文件，内容只有 hash、创建/到期/消费时间（0600），并新增 `mesh invite` 做离线签发。选文件而非 taskstore 表的原因：(1) `internal/taskstore` 是 M1-A 冻结面，加表等于改他人契约；(2) 每个邀请一个文件让签发只创建新文件、消费只原子 rename 自己那一个文件，因此网关进程与离线签发进程不需要任何文件锁也不会互相覆盖；重放由 rename 的原子性保证。若协调者坚持放进 SQLite，`IssueInvitation`/`PendingInvitations`/`InvitationStore` 可原地替换实现，调用方不变。

**G6（存储约束，残余风险）`taskstore.Start` 非幂等。** HTTP 层用「attempt 匹配 + 状态 + 租约有效」确认重传，**该路径无法校验 token 本身**（store 只存 hash 且无只读校验 API）。调用方仍需持有效设备证书且知道服务端随机生成的 attempt ID。如需强校验，需要 taskstore 增加只读校验接口（属 M1-A 冻结面）。

**G7（细节，待确认）`claim` 请求体缺失或非法 JSON 返回 400。** 原实现静默回落到默认 60s 租约。严格化是为了不隐藏客户端 bug，但契约未定义该错误码。

**G8（协调者文件）`git diff --check` 在 `docs/CONTRACTS.md:102` 报 `new blank line at EOF`**，属协调者未提交改动，本 worker 未修改共享文档。

**G9（死代码）`internal/httpapi.Config` 未被使用**，保留给后续装配，请裁决删除或接线。

**G10（分工变更，需回写台账）本轮新增的 `internal/node` 与 `cmd/mesh` 原本属于 M2-B（agy）与 M2-C（协调者）。** 依据用户指示「直接做到能自动执行任务」实施，未与 agy/协调者并行写同一模块（这两处此前无任何实现）。**请协调者更新 `docs/COORDINATION.md` 的 ownership 与任务书**，避免后续重复实现或冲突。

## 9. 未完成 / 未实现

1. **事件未持久化、节点也不发事件**：`/v1/tasks/events` 仅 ACK，节点执行期间不产生事件，结果只在结束时上报一次有界输出。该 200 不是持久化承诺。
2. 撤销、证书轮换、重新邀请恢复（G4）。
3. 操作员 / MCP 角色凭据与作用域（G2，需要 `internal/policy`）。
4. **reconciliation 未实现**：只做到"不重跑 + 记录待核对"，operator 侧的核对与审计路径没有。
5. **节点并发为 1**，无同工作区串行策略、无跨任务调度。
6. 无文件/artifact 传输；结果为有界文本。
7. 能力协商（仅名称 + 版本白名单）、工作区策略、配额均未实现。

## 10. 风险与已知上限

1. **验收边界**：测试证据基于 macOS 本机回环与临时目录；另有一组**两台真实 macOS 机器的局域网验证**（§6.4，网关 192.168.3.237 ↔ 节点 192.168.3.81）。仍**无 Linux/Windows 运行时验证**（仅交叉编译 + vet，Windows 二进制从未在 Windows 上运行过），无跨网段/NAT/VPN 验证，无真实 Agent CLI 调用（adapter 用的是 `/bin/echo`）。曾尝试用 `golang:1.27` 容器补 Linux 运行时验证，但本机到镜像仓库的网络无法完成拉取（镜像层下载停滞），**该验证未完成，不作为通过项**；未使用本机已在运行的 postgres/redis 容器。
2. **事件缺失**（§9 第 1 条）是当前最可能造成"看不到执行过程"的点；M2-C 集成前需解决或明确降级为"事件不可靠"。
3. **越权判定依赖证书**：证书泄露只能等 90 天到期（G4）。
4. **限流是单进程内存态**，重启清零。
5. **长轮询每请求一个 goroutine + channel**，未设单节点并发挂起上限。
6. **节点崩溃恢复**只覆盖"检测 + 拒绝重跑"；进程被 SIGKILL 后遗留的子进程是否真的退出，依赖 runner 的进程组/Job Object 行为（M1-B 已验证该层，但本报告未做真杀进程的端到端验证）。
7. **单实例锁在 Windows 上只有编译与 vet 证据**，未在 Windows 实机运行，不宣称已验证。
8. 测试通过不等于 M2 验收通过；本报告不替代协调者的独立复核。

## 11. 交付方式与协作边界说明

1. **工作区**：本轮及上一轮均在 **main 工作区**直接推进（`internal/identity`、`internal/httpapi`、`internal/node`、`cmd/`、`internal/protocol/wire.go` 为未跟踪文件），未建独立 worker worktree，与 AGENTS.md「在各自独立 worktree 开发」不符。请协调者裁决是否迁移分支后再审阅。
2. **未提交**：按用户确认，本轮只交付源码 + 本报告；`git add`/`commit` 由协调者执行。
3. **未越界修改**：`docs/CONTRACTS.md`、`docs/COORDINATION.md`、`go.mod`/`go.sum`、`internal/protocol/**`、`internal/taskstore/**`、`internal/runner/**` 均未改动；未使用全局技能、未递归派生 agent、未改用户配置、未调用真实 Agent CLI。单实例锁使用了 `golang.org/x/sys/windows`，该依赖已由 M1-B 在执行器中引入并有 go.sum 记录，本轮未新增或升级任何依赖。

## 12. 交协调者的 M3-A 提案（草案）

**性质说明**：本节只是**提案**，不是任务书。公共接口的冻结与 ownership 变更由协调者决定；`docs/tasks/`、`docs/COORDINATION.md`、`docs/CONTRACTS.md` 本 worker 未修改。为便于交接，本节可整段复制为任务书草稿。

### 12.1 现状（先讲清楚"没有"）

- 仓库内**不存在任何管理页**：无 `html/template`、无 `go:embed`、无静态资源、无 `internal/mcp`。
- `internal/httpapi` 只有 9 条 JSON 路由，且任务路由全部以**设备证书**认证。
- 任务**没有任何列表查询**：`taskstore` 导出函数仅 Open/Close/Submit/Get/Claim/Start/Renew/Complete/Cancel/Expire；`httpapi` 无列表路由。现在看列表只能只读查 SQLite。

### 12.2 必须先裁决的前置（否则 M3-A 无法安全开工）

**P1（阻塞，即 §8 G2）操作员鉴权模型。** 浏览器无法出示设备证书，管理页需要独立操作员身份。SECURITY.md 已给出要求：HttpOnly/Secure/SameSite 会话、CSRF、Origin/Host 检查、CSP；凭据分角色、可撤销、可轮换；actor 由服务端绑定，不从输入 JSON 采信。需要冻结的最小集合：角色（建议 admin/viewer）、凭据存储（仅存 hash）、签发与撤销方式（建议本地 CLI，如 `mesh operator add/revoke`）。**在这些冻结前实现 UI，等于把"以节点证书身份提交任务"的权宜模型固化进界面。**

**P2（阻塞页面内容）只读列表接口，二选一或都给：**

- 方案 A（推荐，地基）：`taskstore` 增加只读查询，纯增量、不改现有签名
  ```go
  type TaskFilter struct {
      NodeID string
      State  protocol.State
      Limit  int          // 必须有上限
      Before time.Time    // 分页游标
  }
  func (s *Store) List(ctx context.Context, f TaskFilter) ([]protocol.Task, error)
  ```
  `protocol.Task` 本身不含 token，语义不变；实现沿用 `Get` 的只读路径（不开写事务）。

- 方案 B（暴露层）：`httpapi` 增加 `GET /v1/tasks?node_id=&state=&limit=`，返回 `[]protocol.Task`，200/400/401。**这是新增契约路由，需协调者冻结**；且必须走 P1 的操作员鉴权，不能复用设备证书。

**P3（阻塞 SSE）事件凭据与事件存储。** 即 §8 G3：`protocol.TaskEvent` 无 token 字段，与"所有 mutation 按 (task_id, attempt_id, lease_token) 验证"冲突；且无事件表，`/v1/tasks/events` 现在只 ACK 不落库、节点也不产生事件。没有事件存储，SSE 没有内容可推。

**P4（独立的更大缺口）没有节点注册表。** 节点身份目前只存在于证书 URI SAN 与任务的 `node_id` 字段，**没有"已注册设备列表"**。管理页要展示节点，需要新增 nodes 表与注册/撤销/轮换语义 —— 这正是 §8 G4（撤销）的前置。建议把「节点注册表 + 撤销 + 轮换」拆成独立子任务，不要塞进 UI 任务书一起做。

### 12.3 建议范围

- `internal/mcp`（官方 Go SDK，M3 再锁版本）+ 管理页（`go:embed` 静态资源，无 CDN，ADR-010）。
- 页面只读优先：任务列表/详情/结果。**写操作（取消任务）必须等 P1 落地**，否则就是一个无鉴权的 mutation 入口 —— 与 §8 G2 同一个错误。
- 节点视图等 P4。
- SSE 等 P3。

### 12.4 建议验收条件（须可测，不接受只能 curl health）

- 未鉴权访问管理页与列表 → 401；错误/过期会话 → 401；CSRF 缺失、Origin/Host 不匹配 → 拒绝（要有针对性测试）。
- 会话 cookie 为 HttpOnly + Secure + SameSite；无 CDN 外链；CSP 响应头存在（要有测试断言）。
- 列表分页有硬上限，慢客户端不拖住执行。
- 页面**不得渲染** lease token（`protocol.Task` 已保证）、不得把完整 prompt/结果写入日志。
- MCP：真正初始化、list tools、调用，不能只 curl health。

### 12.5 需要协调者裁决、本人不自行决定的三件事

1. 操作员凭据的签发方式与存储位置（P1）。
2. 是否新增节点注册表，以及它与撤销/轮换的关系（P4）。
3. **页面上是否展示任务输入（prompt）与结果全文** —— SECURITY.md 有"不记录完整 prompt"的约束，展示范围属于产品与安全决策。

### 12.6 MCP 工具面提案（草案）

**前置（三条缺一不可，均不由本 worker 决定）**

1. 协调者把官方 SDK `github.com/modelcontextprotocol/go-sdk` 加入 `go.mod`/`go.sum` 并锁版本（ADR-005「MCP 阶段再锁版本」+ ENGINEERING「依赖只由协调者增加/升级」，worker 不得自行 `go get`）。
2. 冻结工具面（工具名、入参、返回、错误语义）。工具一旦对客户端发布即为公共接口。
3. P1（操作员鉴权）落地；或明确把 MCP 限定为「本地 stdio + 复用节点证书身份」的临时模型（与当前 `mesh task` 一致），并在文档写清该限制。

**建议最小工具集（全部复用现有路由，不新增后端接口）**

| 工具 | 映射 | 入参 | 备注 |
|---|---|---|---|
| `submit_task` | `POST /v1/tasks/submit` | node_id, capability, capability_version, input(JSON), timeout_seconds, idempotency_key | 幂等键由调用方提供 |
| `get_task` | `GET /v1/tasks/{id}` | task_id | 返回 `protocol.Task`，本身不含 token |
| `cancel_task` | `POST /v1/tasks/{id}/cancel` | task_id | mutation，**必须等前置 2、3** |
| `list_nodes`（可选） | 无对应路由 | — | 依赖 P4 节点注册表 |
| `read_events`（可选） | 无对应路由 | — | 依赖 P3 事件存储 |

**明确不做**

- 不把 lease token、完整 prompt、结果全文写进 MCP 日志或资源（SECURITY「日志不记录完整 prompt」）。
- 不做任意能力透传：能力必须与本地节点配置的白名单一致（节点策略不可被远端扩权）。
- 不自行实现 MCP 协议或 JSON-RPC 分帧（ADR-005）。

**验收（对齐 ENGINEERING 测试分层第 5 条）**

真正初始化 MCP server、`list tools`、并用真实 mTLS 调用一次完整任务闭环（submit → claim → execute → complete → get），另覆盖未鉴权与越权拒绝；**不接受"只 curl health"**。

