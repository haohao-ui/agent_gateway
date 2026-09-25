# 开发协作台账

协调者：Codex。新项目 /Users/yang/tools/code/agent-gateway；旧 Python 项目只读。

## 所有权

| 执行方 | 首批任务 | 允许写入 | 状态 |
|---|---|---|---|
| Codex | M0 + 集成验收 | 共享契约、依赖、计划、集成 | M1 两个子模块与集成测试全量验收通过，已合并至主分支 |
| Claude Code | M1-A | internal/taskstore/**、对应报告 | 交付完成并通过复核，测试与报告已导入 |
| agy / 接管 | M1-B | internal/runner/**、对应报告 | 由接管会话串行完成修复、跨平台编译与全量测试，测试与报告已导入 |

Claude Code 当前使用已有配置的 deepseek-v4.1-flash[1m]；这是 Claude Code CLI 工作流，不声称底层为 Anthropic Claude。未修改模型设置。

Orca 不可用：`Unable to determine Orca.app path from symlink: /usr/local/bin/orca`。停止该路径，直接使用 CLI 与独立 Git worktree；不是 Orca 托管会话。agy 首次连通检查曾返回 `PERMISSION_DENIED (code 403): Verify your account to continue.`。用户随后确认已恢复可运行，协调者已重派实际 M1-B 任务；新的调用不修改账号凭据。

## 调度规则

任务书落盘且共享类型编译后才启动。日志放 .coordination/（忽略提交），不含真实凭据。每个 worker 独立分支/worktree，只写 ownership 路径；集成者检查差异、测试、契约后导入。所有权不重叠，未交付不能标完成。

后续 M2/M3 的负责人已规划，但需要上一阶段验收及接口补齐后再派发；本台账不代表无人值守调度器。当前没有自动后台续派保证。

## 首批执行记录

- 基线提交：`3e8f895`，分支 main。
- Claude worktree：`/Users/yang/tools/code/agent-gateway-worktrees/claude-m1`，分支 `worker/claude-m1`。
- agy worktree：`/Users/yang/tools/code/agent-gateway-worktrees/agy-m1`，分支 `worker/agy-m1`。
- Claude 采用 print + acceptEdits 与受限的 Go/格式化/读取 Git shell 允许列表；没有启用 bypassPermissions。
- Claude 日志：`.coordination/claude-m1.jsonl`、`.coordination/claude-m1.stderr`；最终报告目标 `docs/reports/claude-M1.md`（开发 worktree 内）。
- agy 已在对应 worktree 使用 `--mode accept-edits --print ... --output-format json` 提交详细 M1-B 提示词；日志为 `.coordination/agy-m1.jsonl` 和 `.coordination/agy-m1.stderr`。未启用 skip-permissions。
- `.coordination/claude-prompt.txt`、`.coordination/agy-prompt.txt` 保存本机详细派工提示词，正式版本以 docs/tasks 为准。

## 状态与下一步

- 已验证 Go 1.27.1 可通过 GOTOOLCHAIN 按需运行，系统默认仍为 1.22.7。
- 初始共享协议 `go test ./...` 通过（尚无业务测试）；具体业务完成必须等 worker 交付后独立检查。
- agy 无交互重派因 MCP 权限被自动拒绝（非账号错误）；已改用交互式会话，只在当前会话批准 list_projects，只信任独立工作区，未写全局允许规则。已实际开始读取 AGENTS.md。
- Claude 初次报告的检查被 rtk 包装器权限拦截。协调者实际运行 `go test ./internal/taskstore`，发现 `TestClaimFailsFastWhileWriteLockHeld` 约 5.05 秒后 SQLITE_BUSY，未遵守 caller context。已通过原会话派回修复并增加仅 Go/rtk Go 检查的命令允许范围。
- CLI 会话标识（仅本次协调运行）：agy interactive exec session 45989；Claude 修复 exec session 74810；日志 .coordination/claude-m1-fix.jsonl。不得把测试数量当作验收通过。

## 最新进度核查 2026-09-25T23:22:50+08:00

- Claude：实际复跑 `go test ./...`（taskstore 1.725s）、`go test -race ./...`（3.234s）、`go vet ./...` 全部退出 0。测试通过不是代码审阅或 M1 集成完成。
- agy：核查时 internal/runner 与交付报告尚不存在；会话停在 `GOOS=windows GOARCH=amd64 go vet ./internal/protocol` 权限提示。协调者已仅在本会话允许该命令，继续执行。
- 更正：会话存活不等于持续编码。后续状态按实际改动与工具输出更新；交互式 worker 仍可能被新命令授权暂停。
- M2/M3/M4 尚未开始，main 仍为设计/协议基线。

- 本轮解除暂停后的新增证据：agy 已创建 `internal/runner/config.go`（4162 字节），工作区出现 `?? internal/runner/`。目前仅开始产出，不代表完整执行器或测试完成。

## 本轮继续开发

- agy 生成 6 个 runner 源文件后再次收到 `Verification Required`（error id: 3b1cc8c9-1efd-4696-a8f3-278d782b3f03-43），尚无测试。会话已 /exit 且确认进程退出；conversation id `5c22e27e-e3ea-4a54-9b7c-84d55ffe6559`。
- M1-B 当前唯一写入者改为 Claude Code 接续会话，仍在 agy-m1 worktree，仅写 runner 与对应报告；保留 agy 来源记录。日志 `.coordination/runner-takeover.jsonl`。不自动反复尝试账号验证。
- M1-A 原 Claude 会话复核修复中：事务拿锁后取时间、有效 PRAGMA 多连接检查、输入 lexical compaction 语义澄清；日志 `.coordination/claude-m1-review.jsonl`。

- M1-B 接续会话 `0c3a6085-26bb-47f2-bae0-9362c6ee611d` 在文件修改前收到 429 Requests are too frequent，已结束。为控制请求频率，等待 M1-A 会话结束后串行恢复，不并发重试。
- 协调者新增 internal/integration/m1_test.go，覆盖真实 SQLite + Go 辅助执行进程、成功/失败结果、重启后 ACK 重传、失联领取 unknown 不重排；等待两模块验收导入后运行。M2 设计准备见 docs/M2-DESIGN.md，尚未派工。

## M1 阶段集成验收完成 2026-09-26T00:02:00+08:00

- M1-A（taskstore）与 M1-B（runner）全部交付并通过审阅，已同步导入主仓库。
- 交付报告归档至 docs/reports/claude-M1.md 与 docs/reports/agy-M1.md。
- 全量检查实测通过：
  - `go test -v ./...`（退出码 0，taskstore 40 项 + runner 33 项 + integration 2 项全 PASS）
  - `go test -race ./...`（退出码 0，全包通过，0 数据竞争）
  - `go vet ./...`（退出码 0，无任何静态告警）
  - 交叉编译：`GOOS=windows GOARCH=amd64`、`GOOS=linux GOARCH=amd64`、`GOOS=linux GOARCH=arm64`、`GOOS=darwin GOARCH=amd64` 全部通过。
- M1 目标已达成。M2 规划见 docs/M2-DESIGN.md，等待协调者审定后派工。
