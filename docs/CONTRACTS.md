# 共享契约 v1 / M1 冻结范围

公共 Go 数据类型位于 internal/protocol/types.go，由协调者拥有。worker 不修改。模块路径 agent-gateway。M1 为本地库契约；HTTP wire 在 M2 冻结，不能把 M1 未认证服务直接暴露。

## 基础约束

- 所有 task ID/attempt/token 由服务端 crypto/rand 生成，不信任 caller 指定。时间使用 UTC。
- payload 为 JSON；capability 必须非空、版本 >= 1、input 合法 JSON，最多 64 KiB。timeout_seconds 1..3600。
- Task.State 使用 queued/leased/running/cancel_requested/succeeded/failed/cancelled/unknown。
- protocol.Task 不包含 lease token；Lease.Token 是一次领取返回的秘密。任务查询、事件、日志不能携带 token。
- Result.Text 至多 64 KiB UTF-8；输出保留按字节限制；错误和取消使用稳定类别。
- 所有 mutation 按 (task_id, attempt_id, lease_token) 验证；终态重传只允许匹配已记录 attempt/凭据 hash 和相同规范化结果。错误凭据即使终态也不得成功。

## Claude 提供 internal/taskstore

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

Submit 按 NodeID + IdempotencyKey 去重：同 payload 返回原任务，不同 payload ErrConflict；空 key 不去重。M2 按实际 actor 增加作用域，M1 不宣称多用户权限。

M1 输入相等定义为去除非字符串内空白后的 JSON 字节相等（lexical compaction），不承诺键顺序/转义写法的语义等价。终态相同结果重传仍需验证 attempt/token hash，但不重新要求租约未过期，支持 ACK 丢失后的恢复。租约判定与续期起点必须在取得写锁后读取当前时间，不能使用等待锁之前的旧时间。M1-A 报告中的标签长度限制与 ErrorCode 格式限制作为当前输入上限接受。

Claim 在原子事务领取 node 最早 queued 任务；无任务返回 nil,nil；每次生成 attempt 和秘密，存 hash。leaseFor >0 且 <=5m；Start/Renew/Complete 必须拒绝已过期租约。

Cancel：queued→cancelled；leased/running→cancel_requested；terminal/重复取消返回现态。cancel_requested 允许 Renew 使取消处理可报告；Complete 只接受 cancelled，拒绝 succeeded/failed（以服务器先到的取消为准，外部副作用仍可能已经发生）。leased 不允许成功完成；必须先 Start。Expire 将过期 leased/running/cancel_requested 转 unknown，不自动重排。unknown 不能 Complete；核对恢复留给 M2 明确设计。

错误通过 errors.Is 对 protocol.ErrInvalid/ErrNotFound/ErrConflict/ErrUnauthorized/ErrLeaseExpired 判断；内部细节可 wrap。Store 的后台清理由调用方触发，不在 Open 偷起 goroutine。M1 的 taskstore 是领域+存储的窄业务门面，内部允许分文件组织，禁止依赖 HTTP。

## agy 提供 internal/runner

```go
type Config struct {
    Executable string
    Args []string // 一个独立 {instruction}，或 Stdin=true 时禁止占位符
    Stdin bool
    WorkDir string
    Env map[string]string // 唯一允许继承的环境；不调用 os.Environ 全量复制
    MaxOutputBytes int // 默认 64 KiB，上限 64 KiB
    GracePeriod time.Duration // 默认 2s
}
func Run(ctx context.Context, cfg Config, instruction string) (protocol.Result, error)
```

验证绝对 executable、实际存在目录、模板合法和输出限制。禁止默认 shell；指令是 argv 单个参数或 stdin。收集 stdout/stderr 的合并有界尾部，保持有效 UTF-8，超过限制仍 drain，Result.Truncated 标记。

普通退出 0→succeeded，非零→failed；ctx deadline→failed + ErrorCode timeout；ctx cancelled→cancelled + ErrorCode cancelled。进程一旦启动，执行失败以 Result 返回且 error=nil；配置或进程启动失败返回 error。Result.ExitCode 在无法获得有效退出码时为 -1。

Unix 使用独立进程组取消并处理子进程遗留管道；Windows 使用 Job Objects，不能只杀父进程。若无法完整实现某平台，编译通过也必须在报告列未完成，不能假装支持。限制单个任务的日志，不写真实 Agent 配置。

## 下一阶段协议要求

wire envelope 含 protocol_version、capability/version、request_id；节点协商 min/max 版本。独立事件序号按任务单调递增，断线可补读；event 与状态同事务写入。Artifact 为引用，不内嵌字节。能力上报不等于授权。M2 的 HTTP/PKI/节点 journal 接口在派工前补齐，当前不得自行冻结或凭想象实现。
