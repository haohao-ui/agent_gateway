# 开发协作台账

协调者：Codex。新项目 /Users/yang/tools/code/agent-gateway；旧 Python 项目只读。

## 所有权

| 执行方 | 首批任务 | 允许写入 | 状态 |
|---|---|---|---|
| Codex | M0 + 集成验收 | 共享契约、依赖、计划、集成 | M0 已落盘，首批检查通过；集成待交付 |
| Claude Code | M1-A | internal/taskstore/**、对应报告 | 已通过 CLI 派发，执行中，尚未验收 |
| agy | M1-B | internal/runner/**、对应报告 | 阻塞：403 Verification Required，任务书已准备 |

Claude Code 当前使用已有配置的 deepseek-v4.1-flash[1m]；这是 Claude Code CLI 工作流，不声称底层为 Anthropic Claude。未修改模型设置。

Orca 不可用：`Unable to determine Orca.app path from symlink: /usr/local/bin/orca`。停止该路径，直接使用 CLI 与独立 Git worktree；不是 Orca 托管会话。agy 连通检查返回非重试错误 `PERMISSION_DENIED (code 403): Verify your account to continue.`，须用户在该产品内完成验证，不修改账号凭据。

## 调度规则

任务书落盘且共享类型编译后才启动。日志放 .coordination/（忽略提交），不含真实凭据。每个 worker 独立分支/worktree，只写 ownership 路径；集成者检查差异、测试、契约后导入。所有权不重叠，未交付不能标完成。

后续 M2/M3 的负责人已规划，但需要上一阶段验收及接口补齐后再派发；本台账不代表无人值守调度器。当前没有自动后台续派保证。

## 首批执行记录

- 基线提交：`3e8f895`，分支 main。
- Claude worktree：`/Users/yang/tools/code/agent-gateway-worktrees/claude-m1`，分支 `worker/claude-m1`。
- agy worktree：`/Users/yang/tools/code/agent-gateway-worktrees/agy-m1`，分支 `worker/agy-m1`。
- Claude 采用 print + acceptEdits 与受限的 Go/格式化/读取 Git shell 允许列表；没有启用 bypassPermissions。
- Claude 日志：`.coordination/claude-m1.jsonl`、`.coordination/claude-m1.stderr`；最终报告目标 `docs/reports/claude-M1.md`（开发 worktree 内）。
- agy 尚未提交开发调用：连通检查已明确返回账号验证错误，避免重复失败消耗。验证恢复后在 agy worktree 执行 `agy --mode accept-edits --print '读取 AGENTS.md 和 docs/tasks/AGY-M1.md，按契约完成 M1-B，实现并测试，只修改允许目录，完成后写报告。' --output-format json`。
- `.coordination/claude-prompt.txt`、`.coordination/agy-prompt.txt` 保存本机详细派工提示词，正式版本以 docs/tasks 为准。

## 状态与下一步

- 已验证 Go 1.27.1 可通过 GOTOOLCHAIN 按需运行，系统默认仍为 1.22.7。
- 初始共享协议 `go test ./...` 通过（尚无业务测试）；具体业务完成必须等 worker 交付后独立检查。
- agy 账号验证未完成前，不能宣称两个开发者都在编码。
