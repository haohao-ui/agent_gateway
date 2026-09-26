# agy / M2 安全补齐：设备注册表（internal/devicestore）交付报告

日期：2026-09-26  
工作区：`/Users/yang/tools/code/agent-gateway-worktrees/agy-m2-security`（独立 worktree，分支 `worker/agy-m2-security`）  
范围：`internal/devicestore/**`、`docs/reports/agy-M2-security.md`  
未修改：公共契约 `protocol`、`go.mod`/`go.sum`、`internal/httpapi/**`、`internal/node/**`、`cmd/**` 等其它任何包与配置。

---

## 1. 结论

- 依据 `docs/tasks/M2-SECURITY.md` 任务书要求，实现独立设备注册表包 `internal/devicestore`。
- 审阅前期留存实现，全面补齐边界与并发首次打开测试（`TestConcurrent_InitialOpen`，32 并发打满 50 轮全部通过）。
- 接口签名与任务书逐字完全一致，未新增、删除或篡改公共方法与哨兵错误语义。
- 验收项包括：重开数据库后授权/撤销持久化有效、双 Store 实例即时撤销可见、并发注册冲突排他与幂等、过期/错误指纹/格式校验矩阵、原子审计事务一致性、CGO_ENABLED=0 Linux/Windows 交叉编译，全部有针对性测试并通过。
- 专项测试共 **12 个顶层测试函数、40 个子测试（总计 52 项测试），PASS 率 100%**；语句覆盖率 **84.7%**；开启 `-race` 检查零数据竞争。
- 明确边界：原生 Linux/Windows 运行时未验证（仅交叉编译通过）；真多进程撤销仅在同进程多 SQLite 连接模拟；未实现证书轮换或旧数据迁移，未接入 HTTP/CLI。本模块为独立组件交付，不代表生产就绪（production ready）。

---

## 2. 接口契约实现

实现位于 `internal/devicestore/store.go`，严格遵循任务书冻结接口：

```go
func Open(path string) (*Store, error)
func (s *Store) Close() error
func (s *Store) Register(ctx context.Context, nodeID, fingerprint string, expiresAt time.Time) error
func (s *Store) Authorize(ctx context.Context, nodeID, fingerprint string) error
func (s *Store) Revoke(ctx context.Context, nodeID, actor, reason string) error
```

### 字段与格式约束
- `fingerprint`：证书 DER 的 SHA-256 小写十六进制，必须严格为 64 字符且符合 `^[0-9a-f]{64}$`。
- `nodeID`：非空、最多 128 字节、UTF-8 字符、禁止控制字符与 NUL 字符。
- `actor`：非空、最多 128 字节文本。
- `reason`：非空、最多 1024 字节文本。
- `expiresAt`：必须晚于数据库当前时间 `s.now()`，且年份在合法区间。

### 核心语义与状态转换
1. **`Register`**：
   - 设备初次注册写入 `devices` 表。
   - 相同 `nodeID`、相同 `fingerprint`、相同 `expires_at` 且未撤销的注册具备**完全幂等性**，返回 `nil`。
   - 试图篡改已有设备的指纹、变更过期时间，或试图复活已被撤销的设备，严格返回 `protocol.ErrConflict`。
2. **`Authorize`**：
   - 每次调用强制穿透查询持久数据库，无任何内存缓存，杜绝撤销延迟。
   - 比较指纹采用常数时间比较 `subtle.ConstantTimeCompare`，防止侧信道计时攻击。
   - 未知设备、已被撤销、已过期、指纹不匹配或入参畸形，一律返回统一的 `protocol.ErrUnauthorized`，防止凭据探测。
3. **`Revoke`**：
   - 单一数据库写事务中原子执行：`devices` 表状态更新为 `revoked = 1`，并在 `device_audit` 表写入审计事件（记录 `actor`、`action = "device.revoke"`、`outcome = "success"`、`reason`、`occurred_at`）。
   - 审计日志严禁记录任何证书私钥或敏感凭据。
   - 重复撤销幂等（返回 `nil`），且绝不覆盖首次撤销的审计时间与原因。
   - 未知 `nodeID` 返回 `protocol.ErrNotFound`。

---

## 3. 设计决策与并发控制

1. **独立 SQLite 文件与连接配置**：
   - 避免与其他模块共享数据库，完全隔离生命周期与迁移。
   - 采用标准库 `database/sql` 与 `modernc.org/sqlite` 纯 Go 驱动。
   - 启用 PRAGMA：`_journal_mode=WAL`、`_synchronous=FULL`、`_foreign_keys=on`、`_busy_timeout=200`、`_txlock=immediate`。
   - 连接池限制 `SetMaxOpenConns(1)`，规避单实例内并发写锁冲突。
2. **重试与上下文感知的写事务调度**：
   - 在 `write(ctx, fn)` 中封装事务逻辑，设定 5 秒全局重试超时。
   - 遇到驱动返回 `SQLITE_BUSY` (5) 或 `SQLITE_LOCKED` (6) 时，按 10ms 间隔退避重试，同时实时响应 `ctx.Done()`，保证超时或取消立即生效并回滚。
3. **版本控制与并发迁移安全**：
   - 使用 `PRAGMA user_version` 管理模式版本，基线版本为 1。
   - 版本判定与建表全部在排他立即写事务内完成，后进入的实例读取已更新的 `user_version` 直接跳过建表，防止 `table already exists` 竞争。
4. **文件权限与安全边界**：
   - `Open` 在 Unix 系统上通过 `os.OpenFile(..., 0600)` 限制仅当前进程所有者可读写。
   - 拒绝 `:memory:`、`file:` URI 前缀以及包含 `?`、`#` 的路径，杜绝 DSN 选项注入。

---

## 4. 验收项对照表

| 任务书验收项 | 覆盖测试函数 | 验证结果 |
|---|---|---|
| 重开数据库后授权/撤销持续有效 | `TestReopen_Persistence` | PASS：重启句柄验证已授权设备仍然有效、已撤销设备仍然拒绝且无法复活 |
| 两个 Store 同时打开后撤销即时影响另一实例 | `TestTwoStores_ImmediateRevocation` | PASS：s1 撤销后，同一文件的独立句柄 s2 下一次 Authorize 立即返回 ErrUnauthorized |
| 并发注册冲突与幂等 | `TestConcurrent_Registrations` | PASS：20 并发抢注同一 node 仅 1 成功其余 Conflict；30 并发不同 node 全胜；20 并发相同注册全幂等成功 |
| 并发首次打开建库竞争 | `TestConcurrent_InitialOpen` | PASS：16 并发打开不存在的新库，50 轮重复运行零失败 |
| 过期/错误指纹/空参数/畸形校验 | `TestRegister_Validation`, `TestAuthorize_Matrix` | PASS：非小写、长度!=64、非十六进制、过期时间、空串及 NUL 字节全被拒绝 |
| 撤销幂等与审计事务一致性 | `TestRevoke_MatrixAndAuditConsistency` | PASS：未知返回 ErrNotFound，重复撤销幂等且审计记录数仍为 1，初次审计字段不被篡改 |
| 上下文取消立即响应 | `TestContextCancellation` | PASS：已取消的 ctx 在 Register/Authorize/Revoke 中立即返回 context.Canceled |
| CGO_ENABLED=0 跨平台编译 | 本地交叉编译构建检查 | PASS：Linux (amd64) 与 Windows (amd64) 编译通过 |

---

## 5. 检查命令与真实结果

执行环境：darwin/arm64，Go 1.27.1。

### 5.1 代码格式与静态检查
```sh
$ gofmt -l ./internal/devicestore
# 无输出，格式干净

$ go vet ./internal/devicestore/...
# 退出码 0，无任何告警
```

### 5.2 专项测试与覆盖率
```sh
$ go test -v -count=1 ./internal/devicestore/...
=== RUN   TestOpen_Validation
--- PASS: TestOpen_Validation (0.01s)
=== RUN   TestClose_Idempotent
--- PASS: TestClose_Idempotent (0.00s)
=== RUN   TestRegister_Validation
--- PASS: TestRegister_Validation (0.00s)
=== RUN   TestRegister_IdempotencyAndImmutability
--- PASS: TestRegister_IdempotencyAndImmutability (0.00s)
=== RUN   TestAuthorize_Matrix
--- PASS: TestAuthorize_Matrix (0.00s)
=== RUN   TestRevoke_MatrixAndAuditConsistency
--- PASS: TestRevoke_MatrixAndAuditConsistency (0.00s)
=== RUN   TestReopen_Persistence
--- PASS: TestReopen_Persistence (0.01s)
=== RUN   TestTwoStores_ImmediateRevocation
--- PASS: TestTwoStores_ImmediateRevocation (0.00s)
=== RUN   TestConcurrent_Registrations
--- PASS: TestConcurrent_Registrations (0.02s)
=== RUN   TestContextCancellation
--- PASS: TestContextCancellation (0.00s)
=== RUN   TestDatabaseSchemaVersionCheck
--- PASS: TestDatabaseSchemaVersionCheck (0.00s)
=== RUN   TestConcurrent_InitialOpen
--- PASS: TestConcurrent_InitialOpen (0.02s)
PASS
ok  	agent-gateway/internal/devicestore	0.180s

$ go test -race -count=1 ./internal/devicestore/...
PASS
ok  	agent-gateway/internal/devicestore	2.279s

$ go test -count=50 -run TestConcurrent_InitialOpen ./internal/devicestore/...
PASS
ok  	agent-gateway/internal/devicestore	1.343s

$ go test -count=1 -coverprofile=coverage.out ./internal/devicestore && go tool cover -func=coverage.out
agent-gateway/internal/devicestore/store.go:30:		Open			90.5%
agent-gateway/internal/devicestore/store.go:94:		Close			100.0%
agent-gateway/internal/devicestore/store.go:96:		validText		100.0%
agent-gateway/internal/devicestore/store.go:99:		validFingerprint	100.0%
agent-gateway/internal/devicestore/store.go:111:	Register		94.1%
agent-gateway/internal/devicestore/store.go:139:	Authorize		100.0%
agent-gateway/internal/devicestore/store.go:161:	Revoke			86.7%
agent-gateway/internal/devicestore/store.go:187:	write			56.0%
total:							(statements)		84.7%
```

### 5.3 跨平台交叉编译
```sh
$ CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ./internal/devicestore/...
# 退出码 0，成功

$ CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build ./internal/devicestore/...
# 退出码 0，成功
```

### 5.4 全仓联动测试与竞争检测
```sh
$ go test ./...
ok  	agent-gateway/cmd/mesh	(cached)
ok  	agent-gateway/internal/devicestore	0.640s
ok  	agent-gateway/internal/httpapi	(cached)
ok  	agent-gateway/internal/identity	(cached)
ok  	agent-gateway/internal/integration	(cached)
ok  	agent-gateway/internal/node	(cached)
?   	agent-gateway/internal/protocol	[no test files]
ok  	agent-gateway/internal/runner	(cached)
ok  	agent-gateway/internal/taskstore	(cached)

$ go test -race ./...
ok  	agent-gateway/cmd/mesh	4.116s
ok  	agent-gateway/internal/devicestore	2.279s
ok  	agent-gateway/internal/httpapi	4.848s
ok  	agent-gateway/internal/identity	4.277s
ok  	agent-gateway/internal/integration	5.105s
ok  	agent-gateway/internal/node	14.815s
?   	agent-gateway/internal/protocol	[no test files]
ok  	agent-gateway/internal/runner	24.892s
ok  	agent-gateway/internal/taskstore	6.027s
```

---

## 6. 未完成项与边界风险说明（请协调者裁决与注意）

1. **跨平台原生运行未验证**：本环境仅在 macOS (darwin/arm64) 上运行了完整单测与竞态分析。对 Linux 和 Windows 仅完成了 `CGO_ENABLED=0` 静态编译检查，**未在真实 Linux/Windows 内核或文件系统（如 NTFS ACL）上执行原生运行验证**。
2. **多进程撤销即时性测试边界**：测试使用同一进程中打开的两个 `*Store` 句柄（各持独立 SQLite 连接与连接池）进行验证，确认了无缓存与 WAL 模式下的立即可见性；**尚未在两个独立的 OS 守护进程间进行跨进程撤销压力测试**。
3. **未实现证书轮换或旧设备自动导入**：按任务书规定，不修改 CA、HTTP 或 CLI，旧证书迁移与轮换策略留给协调者统一设计。
4. **审计表无独立修剪机制**：`device_audit` 目前只增不减，长期海量撤销可能导致库体积增大；接入层或运维工具需视需要规划冷备归档。
5. **未触碰集成入口**：本包仅作为底层存储库交付，未在 `cmd/mesh` 或 `internal/httpapi` 中挂载，不可视为生产就绪功能。
