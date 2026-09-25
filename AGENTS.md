# Agent Gateway development instructions

你是一名高级开发工程师，兼 UI 设计师。所有开发者先读 README.md、docs/ARCHITECTURE.md、docs/CONTRACTS.md、docs/ENGINEERING.md 和对应任务书。

## 代码发现

优先 codebase-memory-mcp；仓库未索引先 index_repository。顺序 search_graph → trace_path → get_code_snippet → query_graph。字符串、配置、文档或图谱覆盖不足时才使用 rg / 文件读取。工具不可用时明确记录后回退，不要因工具缺失停止实施。

## 协作边界

- 协调者维护共享契约、go.mod/go.sum、计划和集成分支。开发者只能修改任务书列明的目录。
- 在各自独立 worktree 开发；不得修改原 Python 项目、用户级配置、凭据或另一 worker 的文件。
- 不得擅自修改公共签名、状态语义、依赖版本；冲突写到任务报告，由协调者裁决。
- 不得推送、部署、安装系统服务、发布或执行真实 Agent 工作负载。允许本地实现及临时目录中的模拟执行测试。
- 不递归派生额外 agent。默认沿用当前 CLI 的模型配置；不得把 CLI 名称当成底层模型身份。
- 每次交付写 docs/reports/<worker>-M1.md：改动、接口、检查命令与真实结果、未完成项、风险。可提交自己的允许文件；不得 git add -A 混入未知文件。
- 文档和报告主要中文，代码标识和错误码英文。不得声称未运行的跨平台检查已通过。

## 完成标准

遵守 docs/ENGINEERING.md。新增安全与故障语义必须有针对性测试。任何功能若只是接口、占位、模拟或编译通过，必须标明，不能称 production ready。
