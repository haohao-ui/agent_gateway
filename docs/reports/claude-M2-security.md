# Claude / M2 安全补齐：独立操作员权限（internal/policy）交付报告

日期：2026-09-26。工作区：`/Users/yang/tools/code/agent-gateway-worktrees/claude-m2-security`（独立 worktree，分支 `worker/claude-m2-security`，会话开始时工作区干净）。

范围：`internal/policy/**`（新建）、本报告。**未修改**：`internal/protocol/**`、`internal/taskstore/**`、`internal/runner/**`、`internal/node/**`、`internal/identity/**`、`internal/httpapi/**`、`cmd/**`、`go.mod`/`go.sum`、`docs/**`（本报告除外）、任何用户级配置。未推送、未部署、未安装服务、未调用真实 Agent CLI、未递归派生 agent。

**性质说明**：本报告是 M2 安全补齐任务的实现交付，**不是 M2 集成验收**，也不代表 production ready；§7 列出的未实现项不得被当作已完成。

## 1. 结论

- 按任务书冻结接口实现新包 `internal/policy`（独立 SQLite 文件）：`Role`/`Principal`、`Open`/`Close`、`Issue`/`Authenticate`/`Revoke`、`Authorize`，函数签名与任务书逐字一致，未增删公共接口。
- 权限矩阵、作用域、未知角色/动作、撤销与到期、重开持久化、双 Store 实例撤销即时生效、数据库无明文 token、并发访问 —— 8 项验收全部有针对性测试；`internal/policy` **99 项全 PASS**（58 个测试函数 + 41 个子测试），语句覆盖率 **90.9%**。
- 全仓：`gofmt` 干净、`go vet ./...` 无输出、`go test ./...` 通过、`go test -race ./...` 通过（无数据竞争）、6 组 `CGO_ENABLED=0` 交叉编译通过、Windows/Linux 的 `go vet`（含测试文件类型检查）通过。
- 发现并记录一个**不属于本 worker 范围**的既有 flaky 测试：`internal/node` 的 `TestNodeExecutesTaskAndReportsResult` 在全量并行跑时偶发失败（§6.4 有原始输出）；单独跑与重复跑均通过，本 worker 未改动该包。
- 明确未验证：Linux/Windows **运行时**（仅交叉编译 + vet）、真实跨进程撤销可见性（测试用同进程两个 Store 实例/两个 SQLite 连接）、`Authorize` 与撤销之间的请求内竞态（§7）。
- 原计划优先使用的 codebase-memory-mcp 图谱工具**不可用**（权限未授予，见 §6.5），已按 AGENTS.md 要求记录并回退到本地文件读取。
- **交付收尾（第二次会话，2026-09-26）**：要求清理的四项临时文件在检查时**均已不存在**，该会话**未删除任何文件**（它在会话语境中看到这些文件被本工作区之外的进程移除，即本报告的第一次会话：`policy.cover`、`policy-verbose.log` 与 `tmp_probe_test.go` 均由第一次会话删除，见 §10.1）；重跑 `go test` / `go test -race` / `go vet`（`./internal/policy`）三项**均通过**（§10.2）；未修改任何已跟踪文件，未补覆盖率、未新增测试（§10.4）。
- **交付后复核与补充修复（第一次会话，2026-09-26，§11–§12）**：并发首次 `Open` 同一新库存在**两个**缺陷。① 迁移版本判定在事务外（§11 由协调者派发的审阅代理修复）；② **连接阶段**的 `PRAGMA journal_mode=WAL` 争用会直接返回 `SQLITE_BUSY`，§11 未覆盖，实测 20 次运行失败 4 次——本轮由第一次会话在 `store.go` 增加 `connectWithLockWait` 修复（§12）。修复后：并发首次打开 `-count=50` 全过、`internal/policy` 101 项全 PASS、覆盖率 **90.8%**、全仓 `go test`/`go test -race`/交叉编译/跨平台 vet 全绿（§12.3）。

## 2. 交付内容

### 2.1 源码（`internal/policy`）

| 文件 | 内容 |
|---|---|
| `store.go` | 包说明（契约、边界、token 存储模型）；`Open`/`Close`；`config`（迁移步骤、连接器、时钟注入）；DSN 与 PRAGMA（`_journal_mode=WAL`、`_synchronous=FULL`、`_txlock=immediate`、`_foreign_keys=1`、`_dqs=0`、`_defensive=1`、短 `_busy_timeout`）；`writeTx`/`readTx`/`beginWriteTx`/`execLocked`/`lockWait`/`sleepCtx`；写锁等待在 Go 侧、检查 ctx；读事务为 `ReadOnly: true`（驱动用 deferred BEGIN） |
| `migrate.go` | 版本化迁移：追加式步骤、每步一个事务、失败回滚且不记录版本、拒绝比本构建更新的库。**交付后加固**：`applyMigration` 取得写锁后重读版本，跳过已被其他打开者应用的步骤（§11） |
| `roles.go` | `Role` 三常量、`Principal`、action 词表（未导出）、权限矩阵、`Authorize`（纯函数）、作用域形状校验、精确匹配 |
| `principals.go` | `Issue`/`Authenticate`/`Revoke`、作用域规范化（去重排序、上限、无通配符）、有效期窗口、`readScope` |
| `ids.go` | `prn_` + 128 位随机 ID；token 32 字节 `crypto/rand` → base64url；token 只存 SHA-256 |
| `errors.go` | 错误映射到 `protocol` 哨兵；认证失败使用**唯一**消息（避免探测通道） |
| `time.go` | 固定宽度 UTC 存储格式（与 taskstore 同款，便于本地 SQL 比对） |

### 2.2 测试（同包内，`package policy`）

| 文件 | 覆盖点 |
|---|---|
| `authorize_test.go` | 权限矩阵（5 动作 × 3 角色 × 作用域内/外/空节点）、未知角色/动作、角色与作用域矛盾的 Principal、精确/大小写敏感匹配、拒绝消息不含请求值、100 节点边界 |
| `issue_test.go` | ID/token 形状（32 字节随机、URL-safe）、作用域规范化与上限（去重前计数）、节点 ID 规则（长度/控制字符/非 UTF-8/`*`/`?`）、有效期窗口边界（含恰好 365 天）、**写锁后读时钟**、ctx 取消、失败后不留残留行 |
| `authenticate_test.go` | 身份与作用域还原、未知/过期/撤销**三者错误完全一致**、畸形 token（截断/加长/大小写/填充/空）、到期边界（纳秒级）、双 Store 实例撤销即时生效、重开持久化、跨库 token 无效、Close 后是存储错误而非认证失败 |
| `revoke_test.go` | 未知 → `ErrNotFound`、ID 校验、重复撤销幂等且不重写时间戳、保留行与作用域、不影响他人、可撤销已过期主体、重开持续、ctx 取消不写半截、Close 后失败 |
| `secrets_test.go` | 扫描数据库**所有文件**（含 WAL/shm）确认无明文 token（3 个 token + 已撤销路径），并用「token hash 与 principal ID 必须存在」作正对照；Principal JSON 与所有错误文本不含 token/hash |
| `concurrency_test.go` | 32 并发 Issue 唯一性、撤销期间并发认证（读侧可能成功或 `ErrUnauthorized`，不得出现其他错误）、并发重复撤销、撤销 + 签发混合、双 handle 并发 |
| `open_test.go` | 0600 权限、路径校验、迁移失败回滚与恢复、拒绝更新版本库、重开不重复应用迁移、**schema 约束**（未知角色/重复 hash/孤儿作用域/重复作用域）、`:memory:`、Close 后行为、DSN 不可被路径注入 |
| `lock_test.go` | 另一个连接持锁时写入等待后成功、等待期间 ctx 到期立即返回且不留残留、**驱动锁错误分类**（BUSY 判为可重试、约束违例与普通错误判为不可重试）、`sleepCtx` |
| `migration_concurrency_test.go`（§11 新增） | 迁移版本判定在锁后重做（含拒绝更新版本）、32 个 goroutine 并发首次 `Open` 同一新库全部成功且只记录一次迁移 |

> 排查过程中的临时探测文件（`tmp_probe_test.go`）已删除，最终交付不含它；§6 的数字是删除后重跑的。

## 3. 设计决策（与任务书的对应）

1. **认证失败只有一种对外表现。** `Authenticate` 对未知/过期/撤销统一返回 `protocol.ErrUnauthorized`，并且三者使用**同一条消息**（"token is unknown, revoked or expired"）；三种原因只能在本地的 `revoked_at`/`expires_at` 上诊断。理由：如果撤销返回 "revoked" 而猜测返回 "unknown"，错误本身就成了探测凭据是否存在的信道。测试直接断言三者 `err.Error()` 相等。
2. **`Authorize` 是纯函数，不查库、不取时钟、不接受 ctx。** 任务书的签名没有 `ctx` 与存储，这正好把「凭据此刻是否仍然有效」与「该身份可否做这件事」分开：前者由 `Authenticate` 在每次请求时回答，后者是纯决策。因此**调用方必须先 `Authenticate`，且只能对返回的 `Principal` 调 `Authorize`**；自行拼装的 `Principal` 等于绕过认证。这是本包最重要的使用约束，见 §5。
3. **作用域矛盾一律拒绝而非修正。** admin 带 nodeIDs、operator/viewer 带空作用域，都返回 `ErrUnauthorized`（而不是"把 admin 当成全局"或"把空作用域当成全部节点"）。空作用域对非 admin 是**歧义**，不是通配。
4. **作用域规范化在写入前完成**：先去重排序再入库（独立 `principal_scopes` 表，主键 `(principal_id, node_id)` 使重复不可能存在）；上限 100 按**去重前**的条目数检查，重复项不能用来绕过上限；通配符字符 `*`/`?` 直接拒绝而不是解释（节点 ID 是精确字符串比较，没有前缀/大小写/空白容忍）。
5. **写锁后读时钟。** 有效期窗口用取得写锁之后的时间判定（`writeTx` 的 `now`）。有一个专门测试：调用时有效、等待锁期间过期的请求必须被拒绝，否则会把整个有效期都花在等锁上。
6. **撤销幂等且保留首条时间戳**，行不删除（删除会让"从未签发"与"已被撤销"无法区分）；未知 ID 返回 `ErrNotFound`（与认证失败不同，这里没有可猜测的秘密）。
7. **认证读在一个快照里完成**：`readTx` 用 `sql.TxOptions{ReadOnly: true}`（已核对 modernc 驱动 `tx.go:23`：ReadOnly 时使用 deferred BEGIN，不走 `_txlock=immediate`，WAL 下不阻塞写者），主体的行与作用域行不会来自数据库的两个版本。
8. **数据库文件 0600、`_synchronous=FULL`**：库里是有效的操作员凭据 hash 与作用域；撤销是一次安全决策，必须在断电后仍然生效。
9. **不缓存任何主体**：这是「两个 Store/进程同时打开，撤销立即生效」得以成立的原因（无进程内缓存即无失效问题）。
10. **不写日志、不落明文 token**：包内没有任何日志调用；token 只在 `Issue` 返回一次，库里只有 SHA-256（token 是 256 位随机数，哈希无需加盐/KDF；查询按哈希走索引，比较在 SQLite 内部完成，无法构造近似哈希做时序分析 —— 这一点与 taskstore 对租约 token 用常数时间比较的场景不同，已在 `ids.go` 写明理由）。
11. **未导出 action 常量**：任务书冻结的接口清单里没有它们，权限字面量在包内定义（`roles.go`），没有擅自扩大冻结接口。协调者接入 HTTP 时若需要 `policy.ActionTaskRead` 之类的导出常量，请先更新任务书（§8 提案 P1）。

## 4. 冻结接口实现对照

```go
type Role string                                            // admin / operator / viewer
type Principal struct { ID string; Role Role; NodeIDs []string }
func Open(path string) (*Store, error)                       // 独立 SQLite 文件，0600
func (s *Store) Close() error                                // 幂等，不删文件
func (s *Store) Issue(ctx, role Role, nodeIDs []string, expiresAt time.Time) (Principal, string, error)
func (s *Store) Authenticate(ctx, token string) (Principal, error)
func (s *Store) Revoke(ctx, principalID string) error
func Authorize(p Principal, action, nodeID string) error     // 纯决策，无 DB
```

权限矩阵（`roles.go` 中以表写死，未由代码推导）：

| action | admin | operator | viewer |
|---|---|---|---|
| `task.read` | 允许（全局） | 仅作用域内 | 仅作用域内 |
| `task.submit` | 允许（全局） | 仅作用域内 | 拒绝 |
| `task.cancel` | 允许（全局） | 仅作用域内 | 拒绝 |
| `device.revoke` | 允许 | 拒绝 | 拒绝 |
| `credential.manage` | 允许 | 拒绝 | 拒绝 |
| 未知动作 / 未知角色 / 空作用域的 operator / 带作用域的 admin | 拒绝 | 拒绝 | 拒绝 |

task 动作要求 `nodeID` 非空；管理动作忽略 `nodeID`（允许为空，也允许非空）。错误映射：输入形状与有效期问题 → `ErrInvalid`；未知主体撤销 → `ErrNotFound`；一切授权/认证失败 → `ErrUnauthorized`。

## 5. 给协调者的接入说明（本包不接 HTTP，此节是使用契约）

1. **每个请求都要 `Authenticate`**，不要把 `Principal` 缓存到会话里后就一直用：`Authorize` 不查库，这是设计（签名里没有 ctx），因此撤销的即时性由「每请求认证」保证。
2. **actor 只能来自 `Authenticate` 的返回值**，绝不能从请求 JSON/header 里采信角色或作用域（ENGINEERING「actor/权限由服务端绑定」）。
3. **`Issue`/`Revoke` 是本地管理操作**：本包不做任何身份校验。接线时必须放在独立的管理入口（建议：仅本地 CLI + 独立 Bearer 凭据 + 仅 HTTPS），不要把 `Issue` 暴露成网络接口。
4. **节点身份不能自动升级为操作员**：设备证书与操作员凭据是两套东西，本包不读取、也不信任任何设备身份。
5. `Authorize` 的 `nodeID` 对管理动作被忽略：若管理 API 也要限定作用域，需在任务书中新增语义，不要在本包外部自行解释 `NodeIDs`。

## 6. 检查命令与真实结果

全部于 2026-09-26 在本机（darwin/arm64，Go 1.27.1，模块缓存位于 `/Users/yang/go/1.22.7/pkg/mod`）执行。

### 6.1 合并门槛

```sh
gofmt -l ./internal ./cmd        # 无输出
go vet ./...                     # 退出 0，无输出
go test -count=1 ./...           # 8 个包 ok（cmd/mesh、httpapi、identity、integration、node、policy、runner、taskstore）
go test -race -count=1 ./...     # 8 个包 ok，0 数据竞争
git diff --check                 # 无输出（本工作区只有新增文件）
```

`internal/policy` 专项：

```sh
go test -count=5 ./internal/policy/                 # ok，5 轮全过，无 flaky
go test -race -count=2 ./internal/policy/           # ok，2 轮全过，无数据竞争
go test -count=1 -coverprofile=... ./internal/policy/
#   ok  agent-gateway/internal/policy  1.283s  coverage: 90.9% of statements
go test -count=1 -v ./internal/policy/              # 58 个测试函数 + 41 个子测试 = 99 项，全部 PASS
```

覆盖率未覆盖的语句集中在：`Revoke` 中「写锁下不可能发生」的守卫分支（保留以便事务形状变化时立刻报错）、`open`/`migrate` 的部分错误分支、`newID`/`newToken` 中 `crypto/rand` 失败的返回（无法在测试中构造）。

### 6.2 交叉编译与跨平台静态检查

```sh
CGO_ENABLED=0 GOOS={windows,linux,darwin} GOARCH={amd64,arm64} go build ./...   # 6/6 OK
GOOS=windows GOARCH=amd64 go vet ./...                                          # 无输出
GOOS=linux   GOARCH=amd64 go vet ./...                                          # 无输出
```

**未验证**：以上只证明编译与类型检查，本 worker **没有**在 Linux/Windows 上运行过任何二进制或测试。

### 6.3 关键验收项的测试证据

| 任务书验收项 | 测试 | 断言要点 |
|---|---|---|
| 权限矩阵 | `TestAuthorizeMatrix` | 15 组角色×动作，各在作用域内/外/空节点三处求值 |
| 作用域越权 | `TestAuthorizeIsExactAndCaseSensitive`、`TestAuthorization…AcrossHandles`、`TestRevokeDoesNotTouchOtherPrincipals` | 前缀/大小写/空白/相邻 ID/通配符全部拒绝；越权不改变他人状态 |
| 未知角色/动作 | `TestAuthorizeRefusesUnknownRolesAndActions`、`TestIssueRejectsUnknownRoles` | 空、`superuser`、`ADMIN`、`task.delete`、`TASK.READ`… 一律拒绝 |
| 撤销与到期 | `TestAuthenticateRefusalsAreIndistinguishable`、`TestAuthenticateExpiryBoundary`、`TestRevokeIsIdempotentAndKeepsTheFirstTimestamp` | 撤销后立即失效、到期纳秒边界、重复撤销不重写时间戳 |
| 重开持久化 | `TestAuthenticateSurvivesReopen`、`TestRevocationSurvivesReopen`、`TestReopenAppliesEachMigrationOnce` | 关库重开后凭据仍可用/仍被撤销，迁移不重复应用 |
| 两个 Store 实例撤销生效 | `TestRevocationReachesAnotherHandleOpenOnTheSameFile`、`TestConcurrentAuthenticationAcrossHandles`、`TestAuthenticationDuringRevocation` | 同一文件两个句柄，A 撤销后 B 的下一次认证即失败；并发读只出现成功或 `ErrUnauthorized` |
| 数据库无明文 token | `TestTokenIsNeverStoredInPlaintext`、`TestTokenIsAbsentFromPrincipalAndErrors` | 扫描 `.db`/`-wal`/`-shm` 全部字节；正对照确认 hash 与 principal ID 确实在文件里 |
| 并发访问 | `TestConcurrentIssuesProduceDistinctPrincipals`、`TestConcurrentRevocationsOfOnePrincipal`、`TestConcurrentIssueAndRevokeOfDistinctPrincipals` | 32 并发签发唯一、并发重复撤销幂等、混合读写无错 |
| 错误使用 protocol 哨兵 | 上述所有测试 + `TestIssueErrorMessagesAreStable` | `errors.Is` 分支，消息可读且不含凭据/请求值 |

### 6.4 全量测试中发现的一个既有 flaky 测试（非本 worker 范围）

`go test -count=1 ./...` 的一次运行中出现：

```
--- FAIL: TestNodeExecutesTaskAndReportsResult (0.12s)
    node_test.go:67: outbox still holds 1 entries: [{TaskID:task_df29… AttemptID:att_4af1… Token:K6MyA8m… …}]
FAIL	agent-gateway/internal/node	8.243s
```

复核：`go test -count=1 -run TestNodeExecutesTaskAndReportsResult ./internal/node/` 单独跑连续 2 次通过；`-count=5` 通过；随后两次全量 `go test ./...` 与一次全量 `go test -race ./...` 均通过（`internal/node` ok）。因此这是**负载相关的偶发失败**（任务包不在本 worker ownership 内，未做修改，仅记录证据交给协调者）。此外该失败信息把 outbox 条目整体打印出来（含 `Token` 字段），属于日志泄露凭据的形状，建议协调者一并处理。

### 6.5 图谱工具不可用（按 AGENTS.md 要求记录）

本会话中 `mcp__codebase-memory-mcp__*` 的调用返回「未授予权限」，两次尝试（`list_projects`、`search_graph`）均被拒绝：

```
Claude requested permissions to use mcp__codebase-memory-mcp__list_projects, but you haven't granted it yet.
Claude requested permissions to use mcp__codebase-memory-mcp__search_graph, but you haven't granted it yet.
```

按 AGENTS.md「工具不可用时明确记录后回退」执行：改用 `Grep`/`Read`/`Glob` 读取 `docs/*`、`internal/protocol`、`internal/taskstore`（作为风格与约定参照）以及 modernc 驱动源码（核对 `ReadOnly` 事务与 `_txlock` 行为）。**未因此降低验证标准**；但也意味着未做图谱级的调用关系/影响面分析。

## 7. 未完成 / 未验证（不得当作已完成）

1. **Linux/Windows 运行时未验证**：只有交叉编译与 `go vet` 证据（§6.2）。
2. **真实跨进程撤销未验证**：测试用的是同进程两个 Store 句柄（两个独立的 SQLite 连接），不是两个操作系统进程；`WAL + 无缓存 + 每次读新快照` 的机制相同，但未做真进程验证。
3. **`Authorize` 与撤销之间的请求内竞态**：按设计，认证通过后在同一次请求内发生的撤销不会影响该请求（时间点语义）。若协调者要求"请求执行期间撤销立刻中断"，需要在更上层实现（本包不提供租约/会话机制）。
4. **无管理审计表**：任务书只对 agy 的 `Revoke` 要求"与审计记录原子事务"，本包的接口没有 actor 参数（`Issue`/`Revoke` 仅接受目标/作用域），因此没有自造 actor 语义去写审计行；撤销的时间戳与"行保留"是当前可追溯的全部依据。管理审计（actor/action/resource/outcome/time）应由管理 API 层记录，请协调者在接入时明确（§8 P2）。
5. **无主体数量上限、无签发/认证速率限制**：本包不限制表增长，也不对 `Authenticate` 限流（256 位随机 token 不存在猜测可行性，但本地管理入口仍应有限流/配额，属接入层）。
6. **未实现证书轮换、旧设备自动导入、迁移路径**：按任务书属协调者设计范围，本包未涉及。
7. **未接 HTTP、未加 CLI、未改动任何已有包**：`mesh` 命令目前没有任何使用本包的入口。
8. **`internal/node` 的偶发失败**（§6.4）未修复，属他人 ownership。

## 8. 交协调者裁决的提案（本 worker 不自行决定）

- **P1（接口表面）**：是否需要导出 action 常量（`policy.ActionTaskRead` 等）与一个 `func KnownRole(Role) bool` 之类的辅助函数。当前实现把它们保留为包内私有，以不扩大任务书冻结的接口；若管理 API 需要，请在任务书中冻结后追加。
- **P2（审计位置）**：管理动作的审计应记在哪里。本包只保存了撤销时间戳；若要求"谁在何时撤销了谁"，需要给 `Issue`/`Revoke` 增加 actor 参数（属于接口变更，需协调者冻结）。
- **P3（撤销语义确认）**：本实现选择「重复撤销幂等、保留首次时间戳、行不删除、未知 ID 返回 `ErrNotFound`」。若协调者期望"重复撤销返回冲突"或"删除行"，请在任务书中改正。
- **P4（认证错误消息）**：本实现让未知/过期/撤销共享同一条消息（最严格的防探测）。若管理排查需要区分原因，可由上层在本地查库诊断，不必放宽本包的错误文本。

## 9. 协作边界与交付方式

- 仅新增 `internal/policy/**` 与本报告；未改动任何已有文件，未使用全局技能、未递归派生 agent、未推送、未部署。
- 依赖：仅使用 `modernc.org/sqlite`（`go.mod` 已声明的既有依赖）与标准库，**未修改 `go.mod`/`go.sum`**，未新增/升级任何依赖。
- 交付物为分支 `worker/claude-m2-security` 上的新增文件；是否由本 worker 提交入库按协调者流程执行（本报告与源码当前已在工作区就绪）。

## 10. 交付收尾（第二次会话，2026-09-26）

本节的唯一目的是交付收尾，**不是新的实现或验收**。范围严格限定为：核实并清理临时文件、重跑三项命令、如实报告。**未修改 `internal/policy/**` 任何源码或测试**，未补覆盖率，未新增/删除测试，未重跑 §6 的全量与跨平台检查。

### 10.1 临时文件清理：四项目标均不存在，本会话未删除任何文件

| 目标文件 | 检查结果 |
|---|---|
| `internal/policy/tmp_probe_test.go` | **不存在**。两次独立列目录均为 16 个源文件，无该文件 |
| `policy.cover`（根目录） | 会话首次列目录时**不存在**；中途出现（14.9 KB）；最终**不存在** |
| `lock.cover`（根目录） | **全程不存在** |
| `probe.log`（根目录） | **全程不存在** |

另外发现一个**不在协调者清单内、但属同类**的临时诊断产物：`policy-verbose.log`（根目录，10.8 KB，内容为 `go test -v` 的 `=== RUN` / `--- PASS` 原始输出，由上一会话生成）。它在会话开始与中途存在，随后同样消失。

**如实说明（不作推测性结论）：本会话没有执行过任何删除命令**（本会话只运行了 `ls`、`git status`、`go test`、`go vet`、`gofmt`、`Read`、`Glob`）。观察到的时间线是：

1. 首次列目录：根目录有 `policy-verbose.log`，无 `policy.cover`。
2. `Read` 成功读取 `policy-verbose.log` 内容；`Glob` 同时报出 `policy.cover` 与 `policy-verbose.log`。
3. 再次列目录：两个文件都在（`policy-verbose.log` 10.8K、`policy.cover` 14.9K）。
4. 之后 `Read(policy.cover)` 返回 “File does not exist”，裸 `ls -la` 与 `rtk proxy ls -la` 均不再列出这两个文件。
5. 编辑本报告时，工具提示「文件已在磁盘上被修改」；复核发现 §1 与 §6.1 的测试计数已被外部从「101 项 / 60 个测试函数」修正为「**99 项 / 58 个测试函数**」（§10.3 独立复核该修正正确）。

因此：这两个非受版本控制的临时文件是在本会话进行中、**由本工作区之外的进程**移除的。对本次收尾而言，「删除」这一步是**无操作（no-op）**——没有文件可供我删除。我**没有**扩大范围去删除任何未被点名的文件（包括当时的 `policy-verbose.log`）：协调者的清单写明「仅这些已知临时文件」，我按字面遵守，未删除。

**给协调者的风险提示**：该 worktree 在本次会话期间存在**并发写入者**（临时文件消失、本报告被外部编辑）。本次收尾的证据是在这些外部变更**之后**采集的、且可重复（§10.3 的命令重跑结果稳定），但协调者应知悉：本工作区并非本会话独占，最终交付前建议再做一次一致的快照核对。

### 10.2 三项验证重跑（真实输出）

全部于 2026-09-26 在本机执行（darwin/arm64，`go1.27.1`）。命令均加 `-count=1` 以**强制真实重跑、不使用测试结果缓存**；均通过 `rtk proxy` 调用以**绕过 rtk 输出过滤、保留原始 stdout/stderr**（本次收尾发现 `rtk` 包装会过滤 `ls`/`git status` 输出，故验证命令一律绕过）。

```sh
$ go version
go version go1.27.1 darwin/arm64

$ go test -count=1 ./internal/policy
ok  	agent-gateway/internal/policy	1.390s

$ go test -race -count=1 ./internal/policy
ok  	agent-gateway/internal/policy	3.307s

$ go vet ./internal/policy
# 无输出，退出码 0
```

三项结果与上一会话记录一致：测试通过、**无数据竞争**、`vet` 干净。

附带检查：`gofmt -l internal/policy` **无输出**（格式干净）。

### 10.3 独立复核测试计数（确认外部修正正确）

因本报告被外部修正，我用命令独立复核了一次（只读，未改动任何测试文件）：

```sh
$ go test -count=1 -v ./internal/policy 2>&1 | rg -c '^=== RUN'    # 99
$ go test -count=1 -v ./internal/policy 2>&1 | rg -c '^--- PASS'   # 58（顶层）
$ go test -count=1 -v ./internal/policy 2>&1 | rg -c '^--- FAIL'   # 无输出 = 0 个匹配
```

即 **99 个 `=== RUN` = 58 个顶层测试函数 + 41 个子测试，失败 0**，与修正后的 §1 / §6.1 一致；原「60 个测试函数 / 101 项」的计数偏高，属上一会话的计数错误，已被修正，**本次不再改动该数字**。

**边界**：本次**未**重跑 §6.1 的覆盖率命令，因此 §1/§6.1 的「90.9% 覆盖率」仍是上一会话的结果，**未被本次复核**——它不应被当作本次收尾验证过的数据。

### 10.4 交付面确认（未越界）

```sh
$ git status --porcelain        # 原始输出
?? docs/reports/claude-M2-security.md
?? internal/policy/
```

- **无任何已跟踪文件被修改** → `go.mod` / `go.sum`、`internal/protocol`、`internal/taskstore`、`internal/runner`、`internal/node`、`internal/identity`、`internal/httpapi`、`cmd/**` 与所有公共契约在本会话中均未被触碰。
- 根目录最终列目录为 `.coordination/ cmd/ docs/ internal/ .git .gitignore AGENTS.md CLAUDE.md README.md go.mod go.sum`，**无残留临时文件**（本次三项命令也未生成任何新文件：未使用 `-coverprofile`，未产生 `.test` 或日志）。
- `internal/policy` 仍为 16 个源文件，无 `tmp_probe_test.go`。

### 10.5 收尾遗留

1. §7 列出的未完成项**全部仍然成立**，本次收尾未改变其中任何一项。
2. 本次**未**重跑 `go test ./...`、`go test -race ./...`、交叉编译与跨平台 `vet`，也未重跑覆盖率——任务只要求 `internal/policy` 三项，超出的结论不在此追加断言。
3. 临时文件清理目标实际不存在，故**没有可提交的清理改动**；本工作区未产生需要入库的删除。
4. 工作区存在并发写入者（§10.1），建议协调者在集成前做一次最终一致性核对。


## 11. 协调者授权复核修复：并发首次打开迁移竞争

Claude 交付并停止修改后，由协调者派发的审阅代理实施本节修复，非 Claude 原交付内容。

- 临时副本中以 32 个 goroutine 同时首次 `Open` 同一个新数据库，第一轮复现 3 次 `table principals already exists`。原因是版本查询发生在事务外；等待另一个实例完成迁移后，旧版本决策仍执行建表。
- 修复：`applyMigration` 取得写锁后重新读版本；已应用的步骤跳过，超出当前支持的版本拒绝。保留逐步迁移事务、失败回滚及公共 API。
- 新增 `migration_concurrency_test.go`：确定性重演旧版本决策（含新版本拒绝），并验证 32 个并发首次打开均成功且迁移只记录一次。
- 实际执行通过：`go test ./internal/policy`、`go test -race ./internal/policy`、`go vet ./internal/policy`；两项新增回归 `-count=20` 通过。
- 本节未重新计算覆盖率、未复跑全仓或原生跨平台测试。新增 2 个顶层测试，前文文件数与测试数为修复前快照。

## 12. 第一次会话在 §11 修复之后：复核、发现第二个并发缺陷并修复

本节由**第一次会话（原实现作者）**在 §11 之后补做，内容分两部分：对 §11 的独立复核，以及复核中发现的**另一个并发缺陷**及其修复。**未改动 §10、§11 的任何结论**。

### 12.1 对 §11 的独立复核（结论：修复正确，但不完整）

- **机制独立复现**：直接对已迁移数据库重放迁移 DDL，得到 `SQL logic error: table principals already exists (1)`（`principal_scopes` 同样）。这正是并发首次打开时"版本读取早于对方提交、随后仍执行建表"的失败点，与 §11 描述一致。
- **当前实现复核**：`applyMigration` 已在写锁内重读版本（`applyMigration(ctx, db, step, latest)`，`schemaVersion` 接受 `querier`），已应用步骤跳过、更新版本拒绝；`migration_concurrency_test.go` 的两个测试在 `-count=3` 下通过。
- **但 §11 的"32 并发首次打开均成功"不可复现**：`go test -count=20 -run TestConcurrentInitialOpen` 在第一次会话本机 **4/20 次失败**，错误为：

```text
concurrent Open: policy: connect <tmp>/policy.db: database is locked (5) (SQLITE_BUSY)
```

### 12.2 第二个缺陷：连接阶段的锁争用（第一次会话修复）

- **根因**：驱动在建连时应用 DSN 的 PRAGMA，其中 `_journal_mode=WAL` 需要数据库锁。多个打开者同时首次打开同一新库时，落败方在**连接阶段**就拿到 `SQLITE_BUSY`——此时我们的任何语句都还没执行，事务内的重试完全帮不上忙。§11 修的是迁移版本判定，与这一条是两回事。
- **修复**：`store.go` 新增 `connectWithLockWait`，把首次 `PingContext` 纳入与写入相同的 Go 侧锁等待（复用 `lockWait`，预算 `lockWaitTimeout`）。安全性理由写在函数注释里：建连除连接设置外不改变数据库，失败尝试不留痕迹，`database/sql` 会丢弃失败连接、下一次尝试重新建连。`Open` 的公共签名与错误文本形状不变（仍是 `policy: connect <path>: …`）。
- **未改**：`migrate.go`、`principals.go`、`roles.go`、`ids.go`、`errors.go`、`time.go` 以及 §11 新增的测试文件；本次只改 `store.go` 一个函数 + 一处调用。

### 12.3 修复后的验证（本轮真实输出）

```sh
gofmt -l ./internal ./cmd     # 无输出
go vet ./...                  # 无输出
GOOS=windows|linux GOARCH=amd64 go vet ./...   # 均无输出

go test -count=20 -run TestConcurrentInitialOpen ./internal/policy/   # ok（修复前 4/20 失败）
go test -count=50 -run TestConcurrentInitialOpen ./internal/policy/   # ok，50/50
go test -count=3 ./internal/policy/            # ok
go test -race -count=2 ./internal/policy/      # ok，0 数据竞争
go test -count=1 ./...                         # 8 个包 ok
go test -race -count=1 ./...                   # 8 个包 ok，0 数据竞争
CGO_ENABLED=0 GOOS={windows,linux,darwin} GOARCH={amd64,arm64} go build ./...   # 6/6 OK

go test -count=1 -coverprofile=... ./internal/policy/
#   ok  agent-gateway/internal/policy  1.395s  coverage: 90.8% of statements
go test -count=1 -v ./internal/policy/   # 60 个测试函数 + 41 个子测试 = 101 项，0 FAIL
```

本节由此把 §1/§6.1 的计数更新到**修复后状态**：`internal/policy` 现为 **17 个源文件 / 101 项测试（60 顶层 + 41 子测试）/ 覆盖率 90.8%**，全仓测试与竞态检查、6 组交叉编译、Windows/Linux 的 `go vet` 均通过。

### 12.4 本轮遗留与边界

1. `TestConcurrentInitialOpen` 是**概率性**回归测试（32 并发；修复前实测失败率约 20%，修复后 50 轮无失败），不是确定性复现；确定性复现连接阶段争用需要注入驱动建连失败点，本轮未做。
2. **真实跨进程**并发首次打开仍未验证（同一进程 32 个 goroutine、各自独立 SQLite 连接）；跨进程行为相同机制但未实测。
3. Linux/Windows **运行时**仍未验证（仅交叉编译 + vet），与 §7 第 1 条一致。
4. §7 其余未完成项（审计表、限额、证书轮换、跨进程撤销）**全部仍然成立**，本轮未涉及。
