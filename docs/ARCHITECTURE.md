# 技术方案 v0.1

状态：2026-09-25 确定的开发基线。范围为个人/小团队，单网关，多台 macOS/Linux/Windows 电脑。网络默认局域网或已有 VPN；公网控制通道支持出站 HTTPS，任意 NAT 间文件直传不承诺。

## 产品与交付

一份程序、server/node 两个角色，另有 pair、doctor、service、task、file 子命令。单二进制不等于零构建依赖；用户侧无需 Go、Python、Node 或外置数据库。Agent CLI、自身运行时和账号是外部依赖。第一版不接管消费版聊天会话、不实现任意 GUI 唤醒。

## 已确定决策

| 编号 | 决策 | 代价与约束 |
|---|---|---|
| ADR-001 | Go 1.27.1，模块化单体 | 固定工具链；不修改全局 Go。发行前复核安全补丁 |
| ADR-002 | 标准库 net/http、crypto/tls、encoding/json、slog、embed | 不引入 Web 框架、ORM 或前端运行时 |
| ADR-003 | SQLite + modernc.org/sqlite；WAL、显式迁移、短事务 | 单网关；限量写并发；锁等待有超时。驱动版本由协调者锁定 |
| ADR-004 | 节点 HTTPS 长轮询，独立心跳；浏览器 SSE | 节点不需要入站任务端口；代理超时需配置；事件仍以持久记录为准 |
| ADR-005 | 官方 MCP Go SDK | MCP 仅入口；不自行实现协议。MCP 阶段再锁版本 |
| ADR-006 | 配对后设备 mTLS；管理/MCP 使用分角色、可撤销凭据 | 实现证书续期、撤销与首次信任；不得关闭验证 |
| ADR-007 | 内置执行器先行，外部进程扩展后续 | 不使用 Go 动态 plugin；出现第二种外部需求再冻结扩展协议 |
| ADR-008 | 结果、事件、产物分离 | 有存储及保留策略，文件字节不放任务 JSON/数据库 |
| ADR-009 | 不确定执行进入 unknown，不默认自动重跑 | 不能保证任意副作用恰好一次；安全优先于表面成功率 |
| ADR-010 | 原生静态管理页内嵌 | 无 CDN；后续可替换 UI 而不改变领域服务 |

## 模块及依赖方向

M0 已锁定 `modernc.org/sqlite v1.59.0`、`golang.org/x/sys v0.48.0`，传递依赖记录于 go.sum。MCP 在 M3 再引入；禁止各 worker 各自执行 go get 升级。

入口 cmd/mesh、internal/httpapi、internal/mcp → 应用服务 → internal/task、identity、policy、event、artifact → store / executor / platform 适配。

internal/protocol 仅稳定数据类型；不得导入数据库、HTTP 或进程实现。执行器返回事件和结果，不直接写任务状态。权限在入口和节点本地分别执行，不能依赖提示词约束。

能力记录 name/version/input schema/output schema；设备上报能力、适配器健康、容量和协议范围。能力存在不等于已授权。第一种能力 agent.run，文件是独立能力，不要求安装 Agent。

## 任务与恢复

queued → leased → running → succeeded/failed；queued 可直接 cancelled；leased/running → cancel_requested → cancelled。租约过期转 unknown，不自动重排；unknown 必须核对执行记录或由有权限的用户明确创建新 attempt。取消请求不等于副作用已回滚。

每次执行持有 task_id、attempt_id、lease_token。节点持久化领取记录后才启动，心跳续租独立于执行；结果先落本地 outbox，再幂等上报。凭据按 hash 存储；相同 attempt 相同结果重传成功，不同结果冲突。租约失效的执行不得写入新 attempt；发现仍运行的旧进程须协调停止/标未知，不仅靠数据库 fencing 宣称已隔离副作用。

## 资源与性能

默认节点并发 1，同工作区串行。输出持续消费、有界尾缓冲、日志磁盘配额；慢订阅者不拖住执行。HTTP 请求体、日志批次、队列、文件并发、执行时间均有限额。连接复用、指数退避及抖动。SQLite 连接池有限且 busy timeout 受 context 约束。

性能承诺在基准完成后确定；先以模拟执行器测网关延迟/吞吐/RSS，再单独测真实 CLI。Go 重写不承诺模型推理提速。

## 安全与平台

详见 SECURITY.md。执行器默认普通用户、最小环境变量，命令路径仅本地配置。cwd 不是沙箱。强隔离是平台能力/专用账号/可选容器，不作跨平台标准库承诺。Unix 进程组、Windows Job Objects；Job Object 不是文件权限沙箱。

文件以 root_id + 相对路径访问，使用 os.Root 约束打开；接收端临时文件、完整性校验后原子发布。断点续传使用有期限、接收方绑定的传输会话；禁用跨身份重定向。支持配置的 VPN 地址，不以 is_private 代替身份校验。直连不可达明确报错；中继后续单独设计。

## 扩展路线

工作流、定时任务、Webhook、浏览器、本地模型通过公共任务服务接入。保留 parent_task_id/workflow_id 和结构化产物引用；DAG 状态放独立表，不扩张普通任务状态。数据库接口按业务事务设计，未来 PostgreSQL/多实例必须重新验证领取、通知和幂等语义。第一版不做多租户 SaaS、集群、NAT 打洞或插件市场。

## 官方参考（2026-09-25 已核验）

- https://go.dev/dl/?mode=json — 工具链发行；本机已实际运行 Go 1.27.1。
- https://go.dev/blog/osroot — 目录约束文件 API 及边界。
- https://github.com/modelcontextprotocol/go-sdk — 官方 MCP SDK。
- https://pkg.go.dev/modernc.org/sqlite — CGo-free SQLite 驱动。
- https://pkg.go.dev/os/exec — CommandContext 不自动保证整棵进程树终止。
- https://learn.microsoft.com/en-us/windows/win32/procthread/job-objects — Windows 子进程管理。
