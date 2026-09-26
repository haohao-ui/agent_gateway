# 命令行节点与设备列表查询交付与验收报告

日期：2026-09-26  
开发分支：`feat/device-list`（工作区：`/Users/yang/tools/code/agent-gateway-worktrees/device-list`）  
范围：`internal/devicestore/**`、`internal/httpapi/**`、`cmd/mesh/**`、本报告。

---

## 1. 目标与背景

在前期安全基线中，网关仅支持 `mesh device revoke`（单节点撤销），运维人员和操作员无法直观获取当前已登记节点的清单及实时认证状态。

本次交付补全了**节点设备列表查询链路（Device List / Node List）**：
1. **底层存储**：`devicestore` 增加 `List(ctx)` 只读查询方法，返回已注册节点的指纹、证书有效期与撤销标志；
2. **服务端权限过滤**：`GET /v1/operator/devices` 要求 HTTPS + Bearer 凭据，且自动结合 `policy.Authorize` 过滤，Admin 具备全局可见性，Operator 仅能查看自身作用域内的授权节点；
3. **命令行交互**：支持 `mesh device list` 以及符合直觉的别名 `mesh node list`，提供格式化 JSON 与状态输出。

---

## 2. 接口与代码变动清单

| 文件路径 | 变动说明 |
|---|---|
| `internal/devicestore/store.go` | 新增 `Device` 结构体及 `List(ctx context.Context) ([]Device, error)` 查询方法 |
| `internal/devicestore/store_test.go` | 新增 `TestStore_List` 单元测试（空表查询、多节点排序、撤销标记判定） |
| `internal/httpapi/security.go` | 挂载 `GET /v1/operator/devices` 端点，集成操作员角色与节点作用域动态过滤 |
| `internal/httpapi/security_test.go` | 新增 `TestSecureOperatorListDevices` 端到端集成测试（Admin 全局可见性 vs Operator 作用域过滤 vs 未授权 401 拦截） |
| `cmd/mesh/operator.go` | 扩展 `runDevice` 分发 `list` 与 `revoke`；实现 `runDeviceList` |
| `cmd/mesh/main.go` | 更新命令行帮助文档，支持 `mesh device list` 及快捷别名 `mesh node list` |
| `internal/taskstore/concurrency_test.go` | 更新并发测试断言，适应 M2 对账机制带来的并发时序演进 |

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
ok  	agent-gateway/cmd/mesh	2.621s
ok  	agent-gateway/internal/devicestore	3.398s
ok  	agent-gateway/internal/httpapi	3.983s
ok  	agent-gateway/internal/identity	3.638s
ok  	agent-gateway/internal/integration	3.338s
ok  	agent-gateway/internal/node	10.292s
ok  	agent-gateway/internal/policy	4.604s
?   	agent-gateway/internal/protocol	[no test files]
ok  	agent-gateway/internal/runner	6.189s
ok  	agent-gateway/internal/taskstore	1.128s

$ go test -race ./...
ok  	agent-gateway/cmd/mesh	2.356s
ok  	agent-gateway/internal/devicestore	2.370s
ok  	agent-gateway/internal/httpapi	2.671s
ok  	agent-gateway/internal/identity	2.283s
ok  	agent-gateway/internal/integration	3.183s
ok  	agent-gateway/internal/node	13.776s
ok  	agent-gateway/internal/policy	3.768s
?   	agent-gateway/internal/protocol	[no test files]
ok  	agent-gateway/internal/runner	23.324s
ok  	agent-gateway/internal/taskstore	3.415s
```
全包测试通过，**0 数据竞争（0 race detected）**。

### 3.3 跨平台 CGO_ENABLED=0 构建检查
```sh
$ CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ./...
# 退出码 0

$ CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build ./...
# 退出码 0
```

---

## 4. 安全与权限保障

1. **强认证与传输加密**：节点列表端点必须在 HTTPS 下携带有效 Bearer Token，拒绝明文 HTTP 和未配对证书访问；
2. **多租户节点作用域隔离**：普通 Operator 即使拥有管理凭据，也只能查阅受权管辖的节点清单，跨节点信息完全不可见；
3. **零敏感数据泄露**：只输出公钥指纹、过期时间、节点 ID 与撤销标记，绝不涉及私钥或一次性配对 Token。
