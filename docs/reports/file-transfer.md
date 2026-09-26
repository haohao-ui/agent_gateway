# 安全文件传输与任务产物沙箱交付与验收报告

日期：2026-09-26  
开发分支：`feat/file-transfer`（工作区：`/Users/yang/tools/code/agent-gateway-worktrees/file-transfer`）  
范围：`internal/artifact/**`、`internal/httpapi/artifacts.go`、`internal/httpapi/artifacts_test.go`、`internal/httpapi/server.go`、`cmd/mesh/file.go`、`cmd/mesh/file_test.go`、`cmd/mesh/server.go`、`cmd/mesh/main.go`、本报告。

---

## 1. 目标与背景

根据 `docs/DEVELOPMENT_PLAN.md`（M3-B 阶段）与架构决策 ADR-008（结果、事件、产物分离）：
1. **大文件与数据库分离**：任务输入与执行产物字节不放入 SQLite 任务表；
2. **严密的沙箱与防路径遍历**：基于 Go 标准库 `os.Root` 机制对工作空间进行隔离，彻底在操作系统与 VFS 层面阻断 `..`、绝对路径与符号链接逃逸攻击；
3. **分块与断点续传（HTTP Range）**：大文件下载基于 `http.ServeContent`，支持 Range 请求，提高弱网下的可靠性；
4. **完整性校验与原子发布**：上传先写入临时文件，流式计算 SHA-256，校验一致后原子重命名（Atomic Rename）生效，杜绝写入损坏或并发脏读。

---

## 2. 接口与代码变动清单

| 文件路径 | 变动说明 |
|---|---|
| `internal/artifact/store.go` | 核心实现：基于 `os.Root` 沙箱的产物存储，实现 `Save`（SHA256 校验与原子替换）、`Open`、`List`、`Delete` 与路径合法性校验 |
| `internal/artifact/store_test.go` | 针对性单测：文件存取、SHA256 不匹配回滚清理、路径穿越恶意用例拦截、并发安全读写 |
| `internal/httpapi/artifacts.go` | HTTP 端点：`PUT /v1/artifacts/{id}/{filename}`、`GET /v1/artifacts/{id}/{filename}`（带 Range 支持）、`GET /v1/artifacts/{id}`、`DELETE /v1/artifacts/{id}/{filename}`；统一适配 mTLS 节点身份与操作员 Bearer 身份权限校验 |
| `internal/httpapi/artifacts_test.go` | HTTP 端到端集成测试：完整文件上传、完整下载、Partial Content 206 断点分块读取、列表、删除、校验和错误拦截 |
| `internal/httpapi/server.go` | 集成 `artifact.Store` 并挂载对应路由 |
| `cmd/mesh/file.go` | 实现 `mesh file upload/download/list/delete` 子命令，支持远程网关模式与本地嵌入式直接操作模式 |
| `cmd/mesh/file_test.go` | 针对 `mesh file` 命令行生命周期进行全链路测试 |
| `cmd/mesh/server.go` | 网关服务启动时自动开启 `artifacts` 沙箱子目录并注入服务 |
| `cmd/mesh/main.go` | 在 `usage` 与主分发中注册 `file` 命令 |

---

## 3. 合并门槛与真实测试结果

执行环境：macOS (darwin/arm64)，Go 1.27.1。

### 3.1 格式与静态检查
```sh
$ gofmt -l ./cmd ./internal
# 无输出（代码完全符合格式规范）

$ go vet ./...
# 退出码 0，零告警
```

### 3.2 单元测试与数据竞争检测
```sh
$ go test ./...
ok  	agent-gateway/cmd/mesh	1.094s
ok  	agent-gateway/internal/artifact	0.417s
ok  	agent-gateway/internal/devicestore	0.995s
ok  	agent-gateway/internal/doctor	0.458s
ok  	agent-gateway/internal/events	1.009s
ok  	agent-gateway/internal/httpapi	1.740s
ok  	agent-gateway/internal/identity	1.098s
ok  	agent-gateway/internal/integration	0.538s
ok  	agent-gateway/internal/mcp	0.745s
ok  	agent-gateway/internal/node	8.094s
ok  	agent-gateway/internal/policy	1.914s
?   	agent-gateway/internal/protocol	[no test files]
ok  	agent-gateway/internal/runner	3.360s
ok  	agent-gateway/internal/taskstore	1.204s

$ go test -race ./...
ok  	agent-gateway/cmd/mesh	2.047s
ok  	agent-gateway/internal/artifact	2.010s
ok  	agent-gateway/internal/devicestore	2.349s
ok  	agent-gateway/internal/doctor	2.203s
ok  	agent-gateway/internal/events	2.046s
ok  	agent-gateway/internal/httpapi	3.459s
ok  	agent-gateway/internal/identity	2.084s
ok  	agent-gateway/internal/integration	2.586s
ok  	agent-gateway/internal/mcp	1.565s
ok  	agent-gateway/internal/node	13.787s
ok  	agent-gateway/internal/policy	3.337s
?   	agent-gateway/internal/protocol	[no test files]
ok  	agent-gateway/internal/runner	22.592s
ok  	agent-gateway/internal/taskstore	3.431s
```

### 3.3 跨平台交叉编译
```sh
$ CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ./...
# 退出码 0

$ CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build ./...
# 退出码 0
```

---

## 4. 结论与下一步

安全文件传输与任务产物沙箱彻底解决了 Agent 产出物、输入依赖及日志大文件的隔离与存储可靠性问题。
下一步将推进 M3-B 的第二部分：**跨平台用户服务（`mesh service install/uninstall/status`，支持 macOS launchd 与 Linux systemd）**。
