# Unknown 任务核对与恢复机制交付与验收报告

日期：2026-09-26  
开发分支：`feat/task-reconcile`（工作区：`/Users/yang/tools/code/agent-gateway-worktrees/task-reconcile`）  
范围：`internal/taskstore/**`、`internal/httpapi/**`、`cmd/mesh/**`、本报告。

---

## 1. 目标与背景

在前期架构设计中，任务租约过期后由网关 `Expire` 扫描置入 `unknown` 状态以防不可逆副作用被盲目重跑（ADR-009）。然而在 M1 基线中，`unknown` 状态处于死胡同（`Complete` 直接返回 `ErrConflict`），导致任务永久悬挂。

本次交付完整实现了**双通道核对与恢复机制（Reconciliation & Recovery）**：
1. **节点端对账补录（Node-driven Reconciliation）**：网络瞬断恢复后，持有当前合法 `attempt_id` 与租约 token 的节点仍可完成 `Complete` 结算，将执行成果转入终态（`succeeded` / `failed`）；
2. **操作员强制重排（Operator Requeue）**：对彻底失联的节点，操作员可通过管理端点将处于 `unknown` 的任务重置为 `queued`，递增并清除旧 attempt，供健康节点重新抢占；旧节点的迟到提交被 fencing 机制拦截；
3. **操作员终态裁决（Operator Resolve）**：操作员可将不可重跑的 `unknown` 任务明确标记为失败终态（`failed` / `cancelled`）并记录审计理由；
4. **管理端过滤查询（Operator List）**：支持按 `state=unknown` 筛选未决任务，严格受操作员节点作用域约束。

---

## 2. 接口与代码变动清单

| 文件路径 | 变动说明 |
|---|---|
| `internal/taskstore/lease.go` | 更新 `Complete`：针对 `unknown` 状态跳过活跃租约校验，允许携带有效凭据的合法 attempt 完成对账 |
| `internal/taskstore/reconcile.go` | 新增 `Requeue`、`Resolve`、`ListUnknown` 存储层核心事务方法 |
| `internal/taskstore/reconcile_test.go` | 新增对账单测：节点补录、Requeue 重新排队、Resolve 裁决、非 unknown 状态拒绝、并发 Complete 与 Requeue 竞争 |
| `internal/taskstore/complete_test.go` | 更新原有断言：验证凭据错误时被拒绝，合法凭据时对账成功 |
| `internal/taskstore/api_test.go` | 更新原有对账测试用例，断言对账成功转为 `succeeded` |
| `internal/httpapi/security.go` | 暴露操作员端点：`POST /v1/operator/tasks/{id}/requeue`、`POST /v1/operator/tasks/{id}/resolve`、`GET /v1/operator/tasks?state=unknown` |
| `internal/httpapi/security_test.go` | 新增端到端测试：涵盖未知任务列出、角色作用域越权拦截、Requeue 与 Resolve 完整流程 |
| `cmd/mesh/operator.go` | 扩展 `operatorTaskCommand`：支持 `requeue`、`resolve`、`list` 客户端请求 |
| `cmd/mesh/task.go` | 新增 `mesh task requeue`、`mesh task resolve`、`mesh task list` 子命令与帮助说明 |

---

## 3. 合并门槛与真实测试结果

执行环境：macOS (darwin/arm64)，Go 1.27.1。

### 3.1 格式与静态检查
```sh
$ gofmt -l ./cmd ./internal
# 无输出（代码完全符合规范）

$ go vet ./...
# 退出码 0，零告警
```

### 3.2 单元测试与数据竞争检测
```sh
$ go test ./...
ok  	agent-gateway/cmd/mesh	3.484s
ok  	agent-gateway/internal/devicestore	3.470s
ok  	agent-gateway/internal/httpapi	4.020s
ok  	agent-gateway/internal/identity	1.214s
ok  	agent-gateway/internal/integration	3.248s
ok  	agent-gateway/internal/node	9.127s
ok  	agent-gateway/internal/policy	4.263s
?   	agent-gateway/internal/protocol	[no test files]
ok  	agent-gateway/internal/runner	6.093s
ok  	agent-gateway/internal/taskstore	3.187s

$ go test -race ./...
ok  	agent-gateway/cmd/mesh	1.853s
ok  	agent-gateway/internal/devicestore	1.893s
ok  	agent-gateway/internal/httpapi	2.898s
ok  	agent-gateway/internal/identity	1.728s
ok  	agent-gateway/internal/integration	2.712s
ok  	agent-gateway/internal/node	14.072s
ok  	agent-gateway/internal/policy	3.706s
?   	agent-gateway/internal/protocol	[no test files]
ok  	agent-gateway/internal/runner	22.793s
ok  	agent-gateway/internal/taskstore	3.779s
```
全包通过，**0 数据竞争（0 race detected）**。

### 3.3 跨平台 CGO_ENABLED=0 构建检查
```sh
$ CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ./...
# 退出码 0

$ CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build ./...
# 退出码 0
```

---

## 4. 关键安全与并发保障

1. **时序与凭据隔离（Fencing）**：
   当操作员先触发 `Requeue` 后，任务回到 `queued` 且清除 attempt 指针。若旧节点此时上报 `Complete`，`checkAttemptCredentials` 检测到 `t.AttemptID != attemptID`，直接抛出 `ErrConflict`，旧结果无法污染新一轮执行。
2. **并发竞争原子性**：
   在 `TestConcurrent_ReconciliationVsRequeue` 中，高并发下节点提交 `Complete` 与操作员 `Requeue` 同时争夺写事务锁，精确保证**有且仅有一方胜出**，另一方安全收到 Conflict。
3. **操作员权限矩阵与作用域约束**：
   - `requeue` 端点要求 `task.submit` 权限（且校验操作员节点作用域）；
   - `resolve` 端点要求 `task.cancel` 权限；
   - `list` 查询仅返回操作员授权作用域内的节点任务，杜绝越权探测。
