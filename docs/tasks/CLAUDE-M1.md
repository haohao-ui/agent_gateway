# Claude Code / M1-A 任务书

目标：完成 docs/CONTRACTS.md 的 internal/taskstore，实现真实 SQLite 持久化任务状态机及针对性测试。不是 mock 存储，不做 HTTP/UI/MCP。

允许修改：internal/taskstore/**、docs/reports/claude-M1.md。共享协议/go.mod/go.sum/其他文档只读。SQLite 依赖由协调者预置，如缺失先报告。

必须覆盖：重复 Submit 同/异 payload；并发 Claim 单领取；Start/Renew 凭据和过期验证；租约到期 unknown；取消竞争；相同结果重传；不同结果冲突；跨任务 token 拒绝；重开数据库恢复；有效 UTF-8 和结果字节上限；无敏感 token 持久明文/错误泄露。

实现步骤：读契约→列出少量潜在矛盾→不阻塞部分先实现→测试→自查→报告。协议不清楚写报告而不越权修改。Open/迁移失败须关闭连接，创建文件权限合理，WAL/忙等/池大小明确。避免 sleep 驱动长测试；可使用包内时钟注入或短租约加可控 Expire。

交付：源码、测试、gofmt、go test ./internal/taskstore、go test -race ./internal/taskstore、go vet ./internal/taskstore 的实际结果，未完成项写明。不要提交代码，交由协调者审阅后提交/集成。不得部署、调用真实 Agent、递归委派或更改用户配置。
