# M2 安全补齐任务（2026-09-26）

状态：派工；模块交付不代表 HTTP/CLI 已集成。协调者负责集成和验收。

共同要求：读 AGENTS.md、README、ARCHITECTURE、CONTRACTS、ENGINEERING、SECURITY；沿用现有 Go/SQLite 依赖，不修改 go.mod/go.sum 或公共 protocol，不递归派工，不部署，不使用真实 Agent。所有数据库操作支持 context；凭据只存 SHA256 hash，不记录明文；真实临时 SQLite 测试，执行 go test、race、vet。若图谱无覆盖，记录后读取文件。不要等待接口讨论，按下面接口实现；疑问写报告。

## agy：设备注册表

独占 internal/devicestore/** 和 docs/reports/agy-M2-security.md。新包 devicestore，独立 SQLite 文件，避免迁移其他模块数据库。提供：

- Open(path string) (*Store,error), Close() error
- Register(ctx context.Context, nodeID, fingerprint string, expiresAt time.Time) error
- Authorize(ctx context.Context, nodeID, fingerprint string) error
- Revoke(ctx context.Context, nodeID, actor, reason string) error

fingerprint 为证书 DER 的 SHA256 小写十六进制（64 字符）；nodeID 非空、最多128字节；actor 非空最多128字节；reason 非空最多1024字节；拒绝过期注册。Register 相同记录幂等，不得替换已有证书或复活已撤销设备。Authorize 每次查询持久记录，未知/撤销/过期/指纹不匹配都返回 protocol.ErrUnauthorized。Revoke 与审计记录原子事务，重复撤销幂等，未知返回 ErrNotFound。错误映射到现有 protocol 哨兵。不要实现证书轮换或自动导入旧设备，留协调者设计迁移；不修改 CA、HTTP 或 CLI。审计不记录任何秘密。

验收：重开数据库后授权/撤销持续有效；两个 Store 同时打开后撤销即时影响另一实例；并发注册冲突；过期/错误指纹/空参数；审计事务一致性；CGO_ENABLED=0 Windows/Linux 构建。报告清楚说明原生运行未验证。

## Claude：独立操作员权限

独占 internal/policy/** 和 docs/reports/claude-M2-security.md。新包 policy，独立 SQLite 文件。提供：

- type Role string; 常量 Admin Role="admin", Operator Role="operator", Viewer Role="viewer"
- type Principal struct { ID string; Role Role; NodeIDs []string }
- Open(path string) (*Store,error), Close() error
- Issue(ctx context.Context, role Role, nodeIDs []string, expiresAt time.Time) (Principal,string,error)
- Authenticate(ctx context.Context, token string) (Principal,error)
- Revoke(ctx context.Context, principalID string) error
- Authorize(p Principal, action, nodeID string) error

token 使用 crypto/rand 至少32随机字节，仅 Issue 返回一次，数据库只存 hash；过期/未知/撤销统一 ErrUnauthorized。Admin 必须空 nodeIDs 表示全局；Operator/Viewer 必须显式非空作用域，最多100个，不支持通配符，节点ID最多128字节；去重排序。有效期必须未来且最长365天。ID 服务端生成。action 仅 task.read/task.submit/task.cancel/device.revoke/credential.manage；Admin 可全部，Operator 仅作用域内前三项，Viewer 仅作用域内 task.read，未知角色/动作一律拒绝。管理动作 nodeID 允许空；task 动作必须非空。不接 HTTP，不添加 CLI，不记录明文 token。管理 Issue/Revoke 仅可信本地调用，网络授权由协调者接入。

验收：权限矩阵、作用域越权、未知角色/动作、撤销与到期、重开持久化、两个 Store 实例撤销生效、数据库无明文 token、并发访问。错误使用 protocol 哨兵。报告边界。

## 协调者后续集成

接入配对签发后的设备注册和每请求撤销校验；设计旧证书显式迁移，禁止隐式信任。管理 API 使用独立 Bearer 凭据，仅 HTTPS；node 身份不可自动升级操作员。补 CLI 安全凭据输入、真实 TLS 集成测试（含已建立连接撤销）。unknown 核对恢复作为下一批独立任务，不混入本批。当前事件接口只有 ACK，不宣称持久化或流式日志完成。
