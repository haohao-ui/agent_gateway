# Claude Code / M1-A 交付报告

日期：2026-09-25（含协调者复核与第二轮修复）。Worktree：`/Users/yang/tools/code/agent-gateway-worktrees/claude-m1`，分支 `worker/claude-m1`。
范围：`internal/taskstore/**`、本报告。未修改共享契约、`go.mod`/`go.sum`、其他 worker 文件、用户配置或全局设置；未提交、未推送、未部署、未调用真实 Agent、未递归派生 agent。

## 1. 结论

- 实现 `docs/CONTRACTS.md` 冻结的 10 个导出函数，签名逐字一致（§3）。
- 72 个测试项（69 个顶层测试函数 + 1 个含 3 个子测试），`go test`、`go test -race`、`go vet` 全部通过（§5）。
- 协调者复核的 4 项发现已全部处理（§6）：时间戳取自写锁获取之后、DSN 选项逐连接实测、payload 身份改为显式词法语义、被接受的决策已归档（§9）。
- 未实现项见 §8；已知上限与残余风险见 §9 第 14 条与 §10。

## 2. 改动清单

| 文件 | 内容 |
|---|---|
| `internal/taskstore/store.go` | 包文档、`Open`/`Close`、DSN 与池参数、锁等待（`lockWait`/`beginWriteTx`/`execLocked`）、`writeTx`（含权威时间）、`querier` |
| `internal/taskstore/migrate.go` | 版本化迁移、事务化步骤、失败回滚、版本回退拒绝 |
| `internal/taskstore/tasks.go` | `Submit`/`Get`/`Cancel`、行映射 |
| `internal/taskstore/lease.go` | `Claim`/`Start`/`Renew`/`Complete`/`Expire`、凭据与租约校验 |
| `internal/taskstore/validate.go` | 输入/结果校验、`compactValidatedJSON`（词法 payload 身份）、payload hash |
| `internal/taskstore/ids.go` | crypto/rand ID、租约 token、SHA-256 hash、常数时间比较 |
| `internal/taskstore/errors.go` | 错误包装（errors.Is 语义、不泄露 token/SQL） |
| `internal/taskstore/time.go` | 固定宽度 UTC 时间存储格式 |
| 12 个测试文件（`helpers/open/submit/lease/complete/cancel_expire/concurrency/secrets/time/timing/dsn/api_test.go`） | 69 个测试函数 + 3 个子测试；`api_test.go` 为 `package taskstore_test`，只用契约导出 API |

无新增依赖：`go list -deps` 显示依赖全部在本地 module cache 解析，`go.mod`/`go.sum` 未改动。

## 3. 接口（与契约一致）

```go
func Open(path string) (*Store, error)
func (s *Store) Close() error
func (s *Store) Submit(ctx context.Context, in protocol.SubmitRequest) (protocol.Task, error)
func (s *Store) Get(ctx context.Context, taskID string) (protocol.Task, error)
func (s *Store) Claim(ctx context.Context, nodeID string, leaseFor time.Duration) (*protocol.Lease, error)
func (s *Store) Start(ctx context.Context, taskID, attemptID, token string) error
func (s *Store) Renew(ctx context.Context, taskID, attemptID, token string, leaseFor time.Duration) error
func (s *Store) Complete(ctx context.Context, taskID, attemptID, token string, result protocol.Result) error
func (s *Store) Cancel(ctx context.Context, taskID string) (protocol.Task, error)
func (s *Store) Expire(ctx context.Context, now time.Time) (int64, error)
```

`protocol.Task` 无 token 字段；token 只在 `Claim` 返回值里出现一次。时钟与迁移列表通过包内 `config` 注入（测试用），不影响上述签名。

## 4. 存储与并发设计

- 表：`tasks`（`seq INTEGER PRIMARY KEY AUTOINCREMENT` 决定领取顺序、`state` CHECK 约束）、`attempts`（token hash 只存于此）、`idempotency`（PK `(node_id, idem_key)` + payload hash）、`schema_migrations`。外键开启。
- DSN：`_txlock=immediate`、`_journal_mode=WAL`、`_synchronous=FULL`、`_busy_timeout=200`、`_foreign_keys=1`、`_dqs=0`、`_defensive=1`。**逐连接实测值见 §6.2**。
- 池：文件库 `maxOpenConns=4`，`:memory:` 固定 1（SQLite 每个连接一个独立内存库）。
- **权威时间**：`writeTx` 在拿到写锁之后读一次时钟，并把该瞬时作为回调参数传给 mutation；任何 mutation 都不得在等锁前取时间（§6.1）。
- **锁等待**：`lockWait` 在 Go 内重试 `BEGIN IMMEDIATE`，每次尝试前检查 context，等待用 context 感知的定时器；总预算 `lockWaitTimeout=5s`，重试间隔 10ms。SQLite 忙处理器只保留 200ms 作为事务内碰撞（如 checkpoint）的兜底。事务体永不重试。`Expire` 例外地使用调用方传入的瞬时（契约参数，语义是"以 T 为界的清扫"，见 §9 第 14 条）。
- 时间以固定宽度 UTC 文本 `2006-01-02T15:04:05.000000000Z07:00` 存储；租约过期在 SQL 内按文本比较，只有定宽 UTC 才能保证文本序 = 时间序（有子秒边界测试）。
- payload 身份是词法的：`compactValidatedJSON` 只做空白压缩，键序与转义写法都算差异（§6.3）。
- `Open` 失败路径统一关闭连接池（含迁移失败），有计数 connector 的测试证明连接确实被释放。

## 5. 检查命令与真实结果

| 命令 | 结果 |
|---|---|
| `gofmt -l ./internal` | 无输出（全部已格式化）✅ |
| `go vet ./...` | `Go vet: No issues found` ✅ |
| `go test ./internal/taskstore` | `Go test: 72 passed in 1 packages` ✅ |
| `go test ./...` | `Go test: 72 passed in 2 packages` ✅ |
| `go test -race ./internal/taskstore` | `Go test: 72 passed in 1 packages` ✅ |
| `go test -race ./...` | `Go test: 72 passed in 2 packages` ✅ |
| `go test -race ./internal/taskstore -count=3` | `Go test: 216 passed in 1 packages`（72×3）✅ |
| `go test ./internal/taskstore -run TestClaimFailsFastWhileWriteLockHeld -count=5` | 5 passed ✅ |
| `grep -c '^func Test' internal/taskstore/*_test.go` | 11 个文件共 69 处；运行 72 项（多出的 3 项是子测试）✅ 交叉核对一致 |
| `git diff --check` | 无输出（本模块文件尚未跟踪，该命令只覆盖已跟踪文件）✅ |
| `git status --short` | `?? docs/reports/`、`?? internal/taskstore/`（仅本任务 ownership 路径）✅ |

说明：本会话 `go test` 输出被 rtk 包装为单行摘要，失败时会打印失败用例与断言消息（§6 引用的失败输出即来自该格式）；`-v` 的逐项输出被 rtk 过滤，未能保留。协调者直接运行 `go test` 可看到原始输出。未运行的检查：跨平台编译、性能基准、`govulncheck`（不在 M1-A 任务书内）。

## 6. 复核发现与修复

### 6.1 时间戳在等锁前取（协调者发现 1）

**问题**：`Claim/Start/Renew/Complete` 原先在进入 `writeTx` 之前调用 `s.now()`。写锁等待最长 5s，因此该时间可能已过期：`Claim` 会发出"到手即已消耗一部分"的租约，`Start/Renew/Complete` 会用过期前的时间判定一个已经失效的租约。

**修复**：`writeTx` 改为 `fn func(q querier, now time.Time) error`，在 `beginWriteTx` 返回（写锁已持有）之后读时钟，mutation 只能使用这个瞬时。`Submit`/`Cancel` 的时间戳同样改到事务内（复核要求一并审计）。`Expire` 保持使用调用方传入的 `now`（契约参数），并在注释中说明为何不跟随 store 时钟。

**证据（故障注入）**：临时把 `writeTx` 改回"等锁前取时间"，新增的时间测试立即失败 6 项，说明这些测试确实能发现该故障：

```
[FAIL] TestClaimLeaseIsMeasuredFromLockAcquisition
   timing_test.go:42: lease expires at 15:42:07.72, want 15:44:07.72 (acquisition + lease duration)
[FAIL] TestMutationsRejectedWhenLeaseExpiresDuringLockWait/Start
   timing_test.go:106: Start = <nil>, want protocol.ErrLeaseExpired
[FAIL] TestMutationsRejectedWhenLeaseExpiresDuringLockWait/Renew
   timing_test.go:106: Renew = <nil>, want protocol.ErrLeaseExpired
[FAIL] TestMutationsRejectedWhenLeaseExpiresDuringLockWait/Complete
   timing_test.go:106: Complete = <nil>, want protocol.ErrLeaseExpired
[FAIL] TestSubmitAndCancelTimestampsFollowLockAcquisition
   timing_test.go:167: Submit stamped 15:41:07.74 / 15:41:07.74, want 15:43:07.74
```

恢复后同一组测试 7 项全部通过。新增/加强的测试：

- `TestClaimLeaseIsMeasuredFromLockAcquisition`：等锁期间时钟前进，租约到期时刻必须等于"获取时刻 + leaseFor"。
- `TestMutationsRejectedWhenLeaseExpiresDuringLockWait`（Start/Renew/Complete 三个子测试）：等锁期间租约到期，三种 mutation 都必须返回 `ErrLeaseExpired`，且任务状态不变。
- `TestCompleteRetransmissionSurvivesExpiryDuringLockWait`：终态相同结果重传在"等锁跨越过期"后仍然成功；不同结果仍是 `ErrConflict`（保护复核要求保留的重传语义）。
- `TestSubmitAndCancelTimestampsFollowLockAcquisition`：`Submit` 的 created_at/updated_at、`Cancel` 的 updated_at 都取自获取时刻。

这些测试是确定性的：`runWhileLockHeld` 用 `db.Stats().InUse` 等到 mutation 真的拿到连接（不再靠 sleep 猜），再让时钟前进并释放锁，因此"等锁前读"与"等锁后读"必然取到不同值。

### 6.2 DSN 选项是否真的生效（协调者发现 2）

**做法**：不假设 mattn 风格别名可用，而是新增 `TestConnectionSettingsTakeEffect`，为池中**每个物理连接**（占满 `maxOpenConns=4`，迫使 4 个不同连接）执行真实 PRAGMA 读回，并做两项行为检查。

**实测结果（全部通过，即观测值等于期望值）**：

| 检查 | 期望 | 观测 |
|---|---|---|
| `PRAGMA journal_mode` | `wal` | 4/4 连接 `wal` |
| `PRAGMA foreign_keys` | `1` | 4/4 连接 `1` |
| `PRAGMA synchronous` | `2`（FULL） | 4/4 连接 `2` |
| `PRAGMA busy_timeout` | `200` | 4/4 连接 `200` |
| `SELECT "not_a_column"`（`_dqs=0`） | 报错而非字符串字面量 | 4/4 连接报错 |
| `PRAGMA writable_schema=ON` 后读回（`_defensive=1`） | 仍为 `0` | 4/4 连接 `0` |
| 第二条连接 `BEGIN IMMEDIATE`（`_txlock=immediate`） | 锁错误（首条事务已持写锁） | 得到锁错误 |

**测试有效性证据（故障注入）**：把 DSN 里 `_journal_mode=WAL` 临时改成 `_journal_mode=MEMORY`，测试在 4 个连接上全部报出实际值：

```
[FAIL] TestConnectionSettingsTakeEffect
   dsn_test.go:45: connection 0: PRAGMA journal_mode = "memory", want "wal"
   ... connection 1..3 同上
```

即：该断言读的是连接上的真实取值，且驱动确实按 DSN 应用了这些 shorthand（改 DSN 观测值随之改变）。恢复 WAL 后测试通过。结论：**无需修改 DSN 格式**，pinned 的 modernc v1.59.0 支持当前写法。

### 6.3 payload 身份（协调者发现 3）

- 内部函数 `canonicalJSON` 更名为 `compactValidatedJSON`，文档明确：唯一的规范化是去空白，payload 身份是**词法的**；键序与转义写法都算差异，语义等价的两种写法会被判为不同 payload 并返回 `ErrConflict`，M1 不做语义归一化。
- `Submit` 的导出文档同步改写（不再称 canonical）。
- 新增 `TestSubmitPayloadIdentityIsLexical`：`{"path":"/tmp/中","n":2}` 之后，用键序调换、`中` 转义、`\/` 转义三种"同值不同字节"的写法重传，都必须 `ErrConflict` 且只存 1 个任务。
- `TestSubmitStoresCanonicalTask` 更名为 `TestSubmitStoresCompactedInput`。

### 6.4 已接受决策归档（协调者发现 4）

复核接受：原报告决策 1–6、8–11 与 `_synchronous=FULL`（现第 13 条）。M1 的调用方是受信内部调用者，不存在对外暴露的未认证 `Cancel`，因此决策 10 在 M1 范围内成立。§9 已按此标注。

## 7. 测试覆盖（任务书条目 → 测试）

| 任务书要求 | 测试 |
|---|---|
| 重复 Submit 同 payload | `TestSubmitDeduplicatesSamePayload`、`TestConcurrentSubmitWithSameKey` |
| 重复 Submit 异 payload | `TestSubmitConflictsOnDifferentPayload`、`TestConcurrentSubmitWithDifferentPayloads`、`TestSubmitPayloadIdentityIsLexical` |
| 并发 Claim 单领取 | `TestConcurrentClaimHandsOutOneTaskOnce`、`TestConcurrentClaimDistributesDistinctTasks` |
| Start/Renew 凭据与过期 | `TestStartRejectsBadCredentials`、`TestStartRejectsExpiredLease`、`TestRenewRejectedAfterExpiry`、`TestRenewRejectsTerminalTask`、`TestRenewValidatesLeaseDuration`、`TestStartRejectsSupersededAttempt` |
| 等锁跨越过期（复核新增） | `TestMutationsRejectedWhenLeaseExpiresDuringLockWait`、`TestClaimLeaseIsMeasuredFromLockAcquisition`、`TestSubmitAndCancelTimestampsFollowLockAcquisition` |
| 租约到期 unknown | `TestExpireMovesLapsedLeasesToUnknown`、`TestExpireKeepsLiveLeases`、`TestExpireOrdersSubSecondLeases`、`TestPublicAPIExpiresLapsedLeaseIntoUnknown` |
| 取消竞争 | `TestConcurrentCancelAndComplete`、`TestCancelQueuedTaskIsImmediate`、`TestCancelRequestsCancellationOfLiveTask` |
| 相同结果重传 | `TestCompleteRetransmissionIsIdempotent`、`TestCompleteRetransmissionSurvivesExpiryDuringLockWait`、`TestReopenPreservesTaskAndResult`、`TestPublicAPILifecycle` |
| 不同结果冲突 | `TestCompleteRetransmissionIsIdempotent`、`TestCompleteRetransmissionSurvivesExpiryDuringLockWait` |
| 跨任务 token 拒绝 | `TestStartRejectsBadCredentials`、`TestLeaseTokenIsAbsentFromTaskViewsAndErrors` |
| 重开数据库恢复 | `TestReopenPreservesTaskAndResult`、`TestReopenKeepsLeaseUsable` |
| 有效 UTF-8 / 结果字节上限 | `TestCompleteValidatesResultLimits`、`TestCompleteRunningTaskRecordsResult`（中文往返） |
| 无 token 明文持久/错误泄露 | `TestLeaseTokenIsNeverStoredInPlaintext`（扫描 db 与 WAL 字节）、`TestClaimStoresOnlyTokenHash`、`TestLeaseTokenIsAbsentFromTaskViewsAndErrors` |
| 迁移失败回滚/失败关闭连接 | `TestMigrationRollsBackFailedStep`、`TestOpenReleasesPoolWhenMigrationFails`、`TestMigrateRejectsInvalidLists`、`TestOpenRejectsNewerSchemaVersion` |
| 忙等受 ctx 约束 | `TestClaimFailsFastWhileWriteLockHeld`、`TestClaimWaitsForWriteLockAndSucceeds` |
| DSN 选项逐连接生效（复核新增） | `TestConnectionSettingsTakeEffect` |
| 文件权限 | `TestOpenCreatesFileWithPrivateMode` |
| 时间格式定宽 UTC | `TestFormatTimeIsFixedWidthUTC` |
| 其他失败语义 | `TestOpenRejectsUnusablePaths`、`TestOpenFailsWhenDirectoryIsMissing`、`TestSubmitValidatesRequest`、`TestSubmitRejectsCancelledContext`、`TestGetUnknownTaskIsNotFound`、`TestCancelUnknownTaskIsNotFound`、`TestExpireRejectsZeroTime`、`TestCompleteRejectsExpiredLease`、`TestCompleteRequiresStart`、`TestCompleteRejectsNonTerminalResultState`、`TestOperationsAfterCloseFail`、`TestMemoryDatabaseSupportsTaskLifecycle`、`TestConcurrentExpireAndComplete` |

## 8. 未完成 / 未验证

1. 未实现 events 表/事件流。契约把"事件与状态同事务、按任务单调序号、断线补读"放在"下一阶段协议要求"（M2），M1 不预先制造。
2. 未实现 unknown 的核对恢复流程（契约明确留给 M2）；`Complete` 对 unknown 一律返回 `ErrConflict`。
3. 未做跨平台编译检查、性能基准、`govulncheck`（不在 M1-A 任务书内）。
4. 未提交代码（任务书要求交协调者审阅后提交）。

## 9. 决策与实现上限（复核状态）

| # | 决策 | 状态 |
|---|---|---|
| 1 | `cancel_requested` 上 `Start` 返回 `ErrConflict`（只允许 `Renew`/`Complete(cancelled)`） | 已接受 |
| 2 | `running` + `Complete(cancelled)` → `cancelled`（节点本地放弃） | 已接受 |
| 3 | 终态重传不检查租约过期，只比对已记录凭据 hash 与完全相同的结果 | 已接受 |
| 4 | `queued → cancelled` 不写 `Result`，`attempt_id` 保持为空 | 已接受 |
| 5 | `Expire` 清 `lease_expires_at`、保留 `attempt_id` | 已接受 |
| 6 | 实现上限：`node_id`/`capability` ≤128B，`idempotency_key` ≤256B，`task_id`/`attempt_id` ≤128B，`ErrorCode` ≤64B 且限 `[a-z0-9_.-]`，`input`/`Result.Text` ≤64 KiB，超限 `ErrInvalid` 不截断 | 已接受 |
| 7 | payload 身份为**词法**（去空白后字节比较，键序/转义差异算不同 payload） | 已按复核意见改为显式文档化，函数更名 `compactValidatedJSON`，并有测试固定该语义 |
| 8 | `Open` 只接受普通路径或 `":memory:"`，拒绝 `file:` URI 与含 `?`/`#` 的路径 | 已接受 |
| 9 | 新建库文件 0600（受 umask 影响），不改既有文件权限，不创建父目录 | 已接受 |
| 10 | `Cancel` 在 M1 无凭据（契约签名只有 `taskID`）；M1 调用方为受信内部调用者，未对外暴露未认证入口；M2 必须加 actor 授权 | 已接受（含 M1 范围说明） |
| 11 | fencing 分支（凭据正确但非当前 attempt → `ErrConflict`）M1 不可达，作为 M2 防护保留并有构造测试 | 已接受 |
| 12 | 锁等待参数：`busyTimeoutMS=200`、`lockWaitTimeout=5s`、`lockRetryInterval=10ms`；总预算与旧 5s 相同，但由 context 界定 | 已实现并有测试（§6.2 的 busy_timeout 实测值 200） |
| 13 | `_synchronous=FULL`（持久性优先于写吞吐） | 已接受 |
| 14 | `Expire` 使用调用方传入的瞬时而非 store 时钟 | 新增说明：契约参数语义为"以 T 为界的清扫"，与第 1 条的时间规则不冲突 |

## 10. 风险与已知上限

- **SQLite 忙处理器残余超时**：忙处理器不可中断，因此一次已经在跑的 `BEGIN IMMEDIATE` 尝试最多会超出调用方 deadline `busyTimeoutMS`（200ms），再加上 Go 侧重试间隔 10ms。即 context 的实际响应上界约为 deadline + 210ms。`TestClaimFailsFastWhileWriteLockHeld` 用 300ms deadline 断言 1s 内返回 `context.DeadlineExceeded`，覆盖该上界；要彻底消除这一残余，只能把 `busyTimeoutMS` 设为 0，代价是事务内 checkpoint 碰撞会直接报错。
- **词法 payload 身份**：节点若以不同转义写法重发同一 payload，会收到 `ErrConflict` 而不是复用任务。这是 M1 的明确取舍（不合并"看起来相同"的请求）；若 M2 需要语义比较，必须由协调者先冻结规则。
- 读路径（`Get`、`Claim` 的探测查询）不再享有 5s 忙等待：WAL 下读不被写者阻塞，只有 checkpoint 独占等罕见情形可能返回 `SQLITE_BUSY`；若 M2 出现该场景，可给读路径加同样的 `lockWait`。
- 单机 SQLite + WAL 支持多进程共享同一文件，但 M1 不宣称多实例/HA。
- 尚无 events 表，M2 落地事件时需要 schema v2 迁移（当前迁移框架已支持）。
- 测试通过不等于三平台验收：本报告只覆盖 darwin/arm64 上的运行，未做交叉编译或他平台运行。
