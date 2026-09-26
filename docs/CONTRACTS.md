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

## M2 协议与 HTTP/2 路由规范

### 1. 传输与 TLS 配置
- 服务端启用标准库 `crypto/tls` + `net/http`，ALPN 宣告 `h2` 与 `http/1.1`，节点出站连接优先协商至 HTTP/2。
- 节点至网关单一 TCP 连接通过 HTTP/2 多路复用，承载任务拉取、心跳、流式事件及结果上报多条虚拟流。
- 认证分层：
  - `/v1/pair`：单向 TLS（仅服务端证书），要求有效的短期邀请凭证与合法 CSR，响应签发后的证书及 CA 根证书。
  - `/v1/tasks/**`：**强制 mTLS 双向认证**。校验客户端证书链有效性、吊销状态，并将证书 URI SAN 中的 `NodeID` 强制注入请求上下文；禁止任何未提供合法客户端证书的请求访问任务路由。

### 2. 核心路由与状态码约定

#### `POST /v1/pair`
- **认证**：公开入口（需服务端 TLS），限流保护。
- **请求体**：`protocol.PairRequest { invitation_token, csr_pem }`
- **响应体**：`protocol.PairResponse { node_id, cert_pem, ca_cert_pem, server_version }`
- **错误码**：`400 Bad Request`（无效 CSR 或参数）、`401 Unauthorized`（邀请码无效或已过期）。

#### `POST /v1/tasks/claim`
- **认证**：mTLS（提取 `NodeID`）。
- **语义**：长连接等待分配给该 `NodeID` 的最早任务。若当前无排队任务，请求挂起等待（默认 25s），有任务立即以 HTTP/2 帧返回；超时无任务返回 `204 No Content`，节点随后重试发起。
- **请求体**：`protocol.ClaimRequest { lease_duration_seconds }`
- **响应体**：`200 OK` + `protocol.Lease`（包含 Task 元数据与一次性 Lease Token）。

#### `POST /v1/tasks/renew`
- **认证**：mTLS。
- **语义**：在任务执行期间延长租约截止时间。
- **请求体**：`protocol.RenewRequest { task_id, attempt_id, token, lease_duration_seconds }`
- **响应体**：`200 OK` + `protocol.RenewResponse { lease_expires_at }`
- **错误码**：`409 Conflict`（租约已过期或 attempt 不匹配）。

#### `POST /v1/tasks/complete`
- **认证**：mTLS。
- **语义**：幂等提交任务终态结果。支持网络 ACK 丢失后的重传。
- **请求体**：`protocol.CompleteRequest { task_id, attempt_id, token, result }`
- **响应体**：`200 OK`。
- **错误码**：`409 Conflict`（相同 attempt 提交不同结果或 attempt 冲突）。

#### `POST /v1/tasks/events`
- **认证**：mTLS。
- **语义**：执行中事件与增量 stdout/stderr 输出流式上传。支持按单调自增序号（sequence）写入任务事件序列。
- **请求体**：`protocol.TaskEvent { task_id, attempt_id, sequence, timestamp, type, data }`
- **响应体**：`200 OK`。

