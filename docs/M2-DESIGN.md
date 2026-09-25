# M2 设计准备（未派工、未实现）

前置：M1 集成验收通过后，再冻结 M2 的共享类型与任务书。本文件用于明确下一阶段边界，不代表以下接口已经实现。

## 首个可运行闭环

server 初始化本地 CA/服务器证书与管理身份 → 创建短期邀请 → node 本地产生密钥并提交 CSR 配对 → mTLS 出站长轮询 → 领取、写本地记录、确认开始 → 独立续租 → runner 执行 → outbox 落盘 → 幂等结果上报。

首个网络闭环仅支持明确 target node 与已配置 adapter/workspace；暂不自动选择机器、不做文件传输、不做 DAG。长轮询等待默认 25s，lease 默认 60s，续租 15s；这些是配置默认值，不是不可修改的协议常量。

## 身份与 TLS

- 首次信任使用显式 CA 文件/独立核验指纹，禁止 InsecureSkipVerify。密钥本地生成；邀请值只显示一次，数据库仅保存 hash、到期与消费状态。
- CSR 签名校验、受支持密钥类型校验；node ID 由服务端产生，证书 URI SAN 绑定该 ID，不能信任节点任意填写的 subject。
- 配对入口无需已有客户端证书，但必须验证服务端 TLS、邀请及限流。设备入口必须有验证成功的客户端链和匹配身份；不可用“TLS 连接成功”替代设备授权。
- 若同一个 listener 同时承载邀请与设备入口，TLS 层采用 VerifyClientCertIfGiven，设备路由额外强制 VerifiedChains 与撤销状态。客户端证书缺失必须在设备路由拒绝。
- 私有 CA、叶子证书用途分别约束；证书轮换、设备撤销、过期后重新邀请恢复必须有测试。Windows 私钥 ACL 未验证时不得宣称已具备平台发行条件。
- 管理/MCP 角色凭据与设备证书分离。actor/权限由服务端绑定，不能从输入 JSON 采信。初始管理员 bootstrap 只用于建角色和邀请，不传给节点。

## 任务操作可靠性

- Claim 响应丢失：节点从未收到凭据，不执行；服务端最终 unknown，后续通过受控 reconciliation 处理，不重新排队。
- Start 必须设计幂等重传：同 attempt/凭据、运行状态明确且租约有效时返回已启动确认；不应由 Start 的网络 ACK 决定进程是否重复启动。
- 节点本地 journal 记录 claimed/starting/running/result_pending/acknowledged，写入后才进行下一个副作用。本机重启处于 starting/running 时先核对，不猜测未执行。
- 外部进程创建与本地 journal 无法组成同一原子事务：进程启动后、PID 持久化前崩溃仍可能存在孤儿。需进程组/job 管理、节点单实例锁及恢复状态 unknown，禁止用 PID 单独识别进程（有复用风险）。
- outbox 先写完整结果再上报；同结果 ACK 丢失可重试。鉴权失败停止取新任务并保留 outbox；网络重试退避并有抖动。
- 取消与完成冲突：保留真实执行结果与取消确认，不能将已成功产生副作用的任务伪造成 cancelled。M1 的取消优先规则需要 M2 reconciliation 补齐，不直接用覆盖结果的方式消除冲突。
- expired→unknown 后节点拿到结果不得自动新执行。reconciliation 需要当前有效设备身份、匹配旧 attempt、本地证据以及审计；不能简单放开任意过期 Complete。

## 下一批所有权草案

| 模块 | 拟负责人 | 内容 |
|---|---|---|
| internal/identity、policy、httpapi | Claude 网关会话 | 配对、PKI、角色范围、设备路由、长轮询 |
| internal/node、journal | 执行器会话 | 节点循环、续租、单实例、outbox 和恢复 |
| internal/protocol、cmd/mesh、集成测试 | Codex 协调者 | 冻结网络接口、CLI 装配、真实 TLS 联合测试 |

只有具名任务书冻结公共接口并更新 ownership 后才启动下一批；没有在后台自动执行 M2。
