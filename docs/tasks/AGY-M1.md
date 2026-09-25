# agy / M1-B 任务书

目标：完成 docs/CONTRACTS.md 的 internal/runner，实现跨平台受控 CLI 执行及模拟进程测试。不做节点网络、HTTP、CLI 主入口或 UI。

允许修改：internal/runner/**、docs/reports/agy-M1.md。共享协议/go.mod/go.sum/其他文档只读。平台依赖 golang.org/x/sys 由协调者预置。

必须覆盖：独立 argv 注入不变形、stdin、非法模板、未知 executable、空/超大限制、环境白名单、非零退出、大输出持续消费、UTF-8 截断、ctx cancel/deadline、子进程清理、遗留管道、并发 Run race。使用 Go 测试辅助进程，不依赖真实 Agent/Python/shell。

Unix/Windows 使用 build tags，Windows Job Objects 的启动/加入/退出竞态要解释；不能仅父进程 Kill 就宣称进程树管理完成。未能验证的平台明确标记。cwd 只是启动目录，不宣传 sandbox。

交付：源码、测试、gofmt、go test ./internal/runner、go test -race ./internal/runner、go vet ./internal/runner；Windows/Linux 交叉编译；docs/reports/agy-M1.md 记录真实证据和剩余风险。不要提交代码，交由协调者审阅后提交/集成。不得改全局配置、安装服务、递归委派或使用真实凭据。
