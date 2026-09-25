# 开发协作台账

协调者：Codex。新项目 /Users/yang/tools/code/agent-gateway；旧 Python 项目只读。

## 所有权

| 执行方 | 首批任务 | 允许写入 | 状态 |
|---|---|---|---|
| Codex | M0 + 集成验收 | 共享契约、依赖、计划、集成 | 进行中 |
| Claude Code | M1-A | internal/taskstore/**、对应报告 | CLI 连通验证成功，待派工 |
| agy | M1-B | internal/runner/**、对应报告 | 阻塞：403 Verification Required，任务书已准备 |

Claude Code 当前使用已有配置的 deepseek-v4.1-flash[1m]；这是 Claude Code CLI 工作流，不声称底层为 Anthropic Claude。未修改模型设置。

Orca 不可用：`Unable to determine Orca.app path from symlink: /usr/local/bin/orca`。停止该路径，直接使用 CLI 与独立 Git worktree；不是 Orca 托管会话。agy 连通检查返回非重试错误 `PERMISSION_DENIED (code 403): Verify your account to continue.`，须用户在该产品内完成验证，不修改账号凭据。

## 调度规则

任务书落盘且共享类型编译后才启动。日志放 .coordination/（忽略提交），不含真实凭据。每个 worker 独立分支/worktree，只写 ownership 路径；集成者检查差异、测试、契约后导入。所有权不重叠，未交付不能标完成。

后续 M2/M3 的负责人已规划，但需要上一阶段验收及接口补齐后再派发；本台账不代表无人值守调度器。当前没有自动后台续派保证。

## 状态与下一步

- 已验证 Go 1.27.1 可通过 GOTOOLCHAIN 按需运行，系统默认仍为 1.22.7。
- 首批派工的进程、日志和执行结果将在启动后更新。
- agy 账号验证未完成前，不能宣称两个开发者都在编码。
