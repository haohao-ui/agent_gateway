# Agent Gateway

Go 实现的轻量远程 Agent 执行网关。模块化单体、单二进制、跨平台节点、可恢复任务；旧版 Python 实现（agent-mesh）仅作只读参考，不在本仓库内。

当前阶段：M1 任务存储与执行器已集成；M2 TLS 配对、节点循环、设备注册撤销及独立操作员权限已完成主线集成。跨平台原生运行与持续日志流持久化尚在规划中，尚未发布。

## 项目文档

- [技术方案与决策](docs/ARCHITECTURE.md)
- [开发计划及验收](docs/DEVELOPMENT_PLAN.md)
- [接口与协议契约](docs/CONTRACTS.md)
- [开发规范](docs/ENGINEERING.md)
- [安全模型](docs/SECURITY.md)
- [协作任务与状态](docs/COORDINATION.md)
- [Claude 首批任务](docs/tasks/CLAUDE-M1.md)
- [agy 首批任务](docs/tasks/AGY-M1.md)

工具链固定 Go 1.27.1；Go 自动工具链下载可满足此版本，无需修改系统默认安装。开发期可联网下载依赖，交付程序不依赖 Python、Node、Redis 或外置数据库。目标 Agent CLI 及其认证仍需由用户安装配置。

```sh
go test ./...
go vet ./...
```

以上为项目检查命令，不表示当前所有功能已实现或验收通过。
