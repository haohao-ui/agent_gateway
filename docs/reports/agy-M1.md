# M1-B (internal/runner) 交付报告

- 模块：`internal/runner`
- 分支：`worker/agy-m1`
- 工作区：`/Users/yang/tools/code/agent-gateway-worktrees/agy-m1`
- 基线提交：`3e8f895`（docs: establish architecture, contracts and development assignments）
- 交付状态：**未 commit**，保留在 worktree 内等待协调者审查
- 验证环境：`darwin/arm64`，`go1.27.1 darwin/arm64`，CGO_ENABLED=1（本地测试）/0（交叉编译）

## 0. 归属与接管说明（必读）

| 阶段 | 产出 | 说明 |
|---|---|---|
| agy 初始会话 | `internal/runner` 五个源文件初稿 | 完成脚手架后以 `Verification Required` 中止；协调者已终止该会话。其初稿存在下述 §2 列出的缺陷（忽略 `postStart` 错误、Unix 取消提前返回、无测试等）。 |
| Claude Code 接管会话 A（本会话） | `runner.go` / `config.go` / `buffer.go` / `process.go` / `process_unix.go` / `process_windows.go` 重写；`helpers_test.go`、`helpers_windows_test.go`、`config_test.go`、`buffer_test.go`、`runner_unix_test.go`、`runner_exec_test.go`；本报告 | 按审查项修复实现、建立 helper 进程测试体系、执行全部验证命令。**本报告的成功/失败/未验证结论以本会话实际执行的命令输出为准。** |
| 并发写入者（同一 worktree 的第二个会话） | `runner_test.go`（重写，使用本会话建立的 `newConfig`/helper 协议）、本报告早期版本 | 见 §7.1。该文件当前通过全部测试，但**不是本会话所写**，需要协调者确认归属。 |

底层模型身份：本报告只声明"Claude Code 接管会话"这一事实，不涉及任何 CLI 名称到模型身份的推断，也不声称任何未经运行的验证（尤其是 Windows/Linux 真实内核行为）。

## 1. 契约符合性

冻结签名与共享类型未被修改（`Run` / `Config` 与 `docs/CONTRACTS.md` 一致，`internal/protocol` 只读，`go.mod`/`go.sum` 未改）。

返回契约（已由测试逐条断言）：

| 场景 | State | ExitCode | ErrorCode | error |
|---|---|---|---|---|
| 配置校验失败（路径/模板/上限/env） | 零值 Result | — | — | 包装 `protocol.ErrInvalid` |
| ctx 已取消或已过期（启动前） | 零值 Result | — | — | 包装 `protocol.ErrInvalid`；**不启动子进程** |
| `cmd.Start()` 失败 | 零值 Result | — | — | 包装 `protocol.ErrInvalid` |
| 退出码 0 | `succeeded` | 0 | `""` | nil |
| 非零退出 | `failed` | N | `""` | nil |
| ctx deadline | `failed` | -1 | `timeout` | nil |
| ctx cancel | `cancelled` | -1 | `cancelled` | nil |
| confinement 失败（子进程已启动） | `failed` | -1 | `process_setup` | nil |

`Result.ExitCode` 在信号终止或 `Wait` 失败时为 `-1`。`Result.Text` 为合并 stdout/stderr 的有界 UTF-8 尾部，`Truncated` 标记是否丢弃过字节。

## 2. 关键审查项与修复

### 2.1 `postStart` 错误必须 fail closed（原实现静默忽略）
`runWith` 中 `controller.postStart` 返回错误时：先补一次 `cmd.Process.Kill()`（不依赖 controller 自己是否已杀），`<-waitErrCh` 等待直接子进程被回收，join stdin writer，`controller.cleanup`，`drainOutput`，最后返回 `Result{failed, process_setup}` 且 `error == nil`。**保证 `Run` 返回时该子进程已死**。

证据：`TestRunFailsClosedWhenConfinementFails` 用 sentinel 文件（子进程延迟写文件）证明子进程未存活；变异测试见 §5.4。

### 2.2 预取消 ctx 绝不启动子进程
`cfg.validate` 之后、`exec.Command` 之前显式 `if err := ctx.Err(); err != nil { return ..., ErrInvalid }`。
证据：`TestRunPreCancelledContext`（cancelled 与 pre-expired 两种）用 sentinel 文件证明无子进程；`TestRunLiveContextStartsChild` 为对照组，证明 sentinel 机制本身有效（否则该测试可能因哨兵失效而假通过）。

### 2.3 Unix 进程组：父进程退出后仍须杀掉忽略 TERM 的后代
- `prepareCmd` 设 `Setpgid: true`，子进程自成进程组（PGID == PID）。
- `terminate`：SIGTERM 全组 → 等待 grace 期**或直接子进程被回收** → 若组内仍有存活成员则 SIGKILL 全组 → `awaitGroupExit` 轮询（上限 2s）等待组内排空。
- 关键修复：`<-waitDone` 分支不再直接返回，而是先 `pgidAlive(pgid)` 判断；否则"父进程先退出、孙进程忽略 TERM"时孙进程会逃逸。
- `Run` 在返回前 join 取消监听 goroutine、`Wait` goroutine、stdin writer goroutine 与 drain goroutine，全部有界（grace + 2s 排空 + 500ms drain + 2s stdin join）。
- **避免 PGID 复用误杀**：`livePGID()` 只在"已启动且未被回收"时返回 PGID；`cleanup` 在 `reaped == true` 后不再对进程组发信号（内核可能已把该 PGID 分配给无关进程组）。

证据：`TestRunKillsDescendantsThatIgnoreTerm`（孙进程 `signal.Ignore(SIGTERM)`，父进程被 TERM 杀死后孙进程仍被 SIGKILL，`kill(pid,0)` 轮询确认消失）、`TestRunChildGetsOwnProcessGroup`、`TestRunDoesNotSignalGroupAfterNormalExit`（固定"正常退出后不清理后代"这一有意限制）、变异测试 §5.4。

### 2.4 Windows：挂起启动 + 加入 Job + 恢复（先约束后执行）
`os/exec` 在 `syscall.StartProcess` 中 `defer CloseHandle(pi.Thread)`，启动后无法再恢复挂起线程，因此：
1. `prepareCmd`：`CreateJobObject` + `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE`，并在 `cmd.SysProcAttr.CreationFlags` 加 `CREATE_SUSPENDED | CREATE_NEW_PROCESS_GROUP`；`SysProcAttr` 为 nil 时**直接报错拒绝启动**（无法请求挂起即无法关闭竞态，不能假装安全）。
2. `Start`：`CreateProcess` 返回，子进程已创建但**未执行任何用户代码**。
3. `postStart`：Toolhelp 快照按 PID 找到唯一主线程 TID → `OpenThread(THREAD_SUSPEND_RESUME)` → `OpenProcess` → `AssignProcessToJobObject` → 成功后 `ResumeThread`；任一步失败调用 `abortSuspended`（`TerminateProcess` + `TerminateJobObject`）并返回错误。
4. `terminate`：先 `GenerateConsoleCtrlEvent(CTRL_BREAK_EVENT)` 宽限，再 `TerminateJobObject`。
5. `cleanup`：`CloseHandle(job)`，`KILL_ON_JOB_CLOSE` 兜底销毁 Job 内所有进程（含后代）。

**未验证**：本文件仅交叉编译通过，从未在 Windows 内核上运行（见 §6）。

### 2.5 stdin writer 关闭与 join
写入 goroutine 写完即 `Close`；`join(timeout=2s)` 超时后强制关闭管道以解除阻塞再 join。
**实测发现（重要）**：`os/exec` 的 `Wait` 自身会关闭 `StdinPipe` 的父端，从而解除写阻塞，因此 join 在实际执行中几乎总是立即返回。`TestRunJoinsStdinWriterWhenDescendantHoldsStdin` 专门构造"直接子进程立刻退出、孙进程继承并持有 stdin 读端且从不读取"的场景，验证 `Run` 仍能及时返回且 goroutine 数回落基线。join 的价值在于使该保证不依赖 `os/exec` 的实现细节。

### 2.6 输出有界消费与遗留管道
单管道合并 stdout/stderr，`io.Copy` 持续消费到有界 `tailBuffer`（滑动窗口，默认/上限 64 KiB），子进程永不因管道满而阻塞；`drainOutput` 在直接子进程消失后最多再等 500ms，超时强制关闭读端（应对逃逸后代持有写端）。
证据：`TestRunLargeOutputBoundedAndDrained`（256 KiB → 保留 16 KiB）、`TestRunLeakedOutputPipeDoesNotHang`（孙进程持有 stdout 10s，`Run` 500ms 级返回）。

### 2.7 UTF-8 边界
`sanitizeTailUTF8` 先剪裁窗口前端被截断的残缺 rune，再剪裁尾部残缺 rune，最后逐字节剔除中间非法字节（保留合法文本，不用 `ToValidUTF8` 整体替换成 U+FFFD）。
证据：`TestRunExactUTF8TailBoundary`（4096 字节窗口 = 1365 个完整 `中` + 1 个孤立续字节 → 精确保留 4095 字节）、`TestRunSanitizesBinaryOutput` / `TestRunUTF8Sanitization`（0xFF 全部剔除）、`TestSanitizeTailUTF8*`。

### 2.8 配置校验与"非沙箱"边界
- `WorkDir` 必须绝对路径且为已存在目录；`Executable` 必须绝对路径、存在、且为**普通文件**（拒绝目录/FIFO/设备，避免 `Start` 阻塞或语义不明）。
- `Env` 拒绝空键、含 `=` 或 NUL 的键、含 NUL 的值；生成全新切片，绝不调用 `os.Environ()`。
- `Stdin=false` 时 argv 必须恰好含一个独立 `{instruction}`；`Stdin=true` 时禁止占位符；指令永远只占一个 argv 元素，不经过 shell。
- 未设置 `Stdin` 时子进程 stdin 接 `os.DevNull`，避免继承网关自身标准输入。
- 文档注释明确：`WorkDir` 只是启动目录，`Env` 只是白名单；**不限制文件系统、网络、凭据或内核权限**，可执行文件必须是受信本地配置。

## 3. 测试资产

无 Python/shell/真实 Agent 依赖：所有执行类测试都以 `go test` 二进制自身作为受控子进程（`TestMain` 按 argv[1] 的 `helper:` 前缀分发模式，模式走 argv 而非环境变量，以免被 `Config.Env` 白名单抹掉）。

| 文件 | 顶层测试数 | 内容 |
|---|---|---|
| `runner_test.go` | 15 | argv/stdin/workdir/env/退出码/stderr/大输出/UTF-8/预取消/取消/deadline/postStart/大 stdin/遗留管道/并发（**并发写入者重写，见 §7.1**） |
| `runner_exec_test.go` | 8 | stdin 额外参数、精确 UTF-8 边界、stdin join、sentinel 对照组、启动失败、confinement fail-closed、prepare 失败 |
| `runner_unix_test.go` | 4 | 进程树 SIGKILL 升级、正常退出不清理后代的特性固定、独立进程组、拒绝 FIFO 可执行文件 |
| `config_test.go` | 6 | executable/workdir/输出上限/宽限期/占位符规则/env 校验 |
| `buffer_test.go` | 6 | 滑动窗口、超大块、UTF-8 两端剪裁与非法字节剔除 |

合计 39 个顶层测试，`go test` 报告 68 个（含子测试）。

## 4. 检查命令与真实结果

全部在 worktree 根目录执行；`go` 命令经本地 CLI 代理（rtk）包装，其摘要行为已说明，未使用任何未运行的结论。

| 命令 | 观察到的结果 |
|---|---|
| `gofmt -l ./internal` | 无输出（已格式化） |
| `go vet ./...` | `No issues found` |
| `go build ./...` | `Success` |
| `go test -count=1 ./internal/runner` | `68 passed`（0 failed） |
| `go test -race -count=1 ./internal/runner` | `68 passed`（0 failed，无数据竞争报告） |
| `go test -count=3 -run '<5 个时序敏感用例>'` | `15 passed`（3 轮稳定） |
| `GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go test -c ./internal/runner` | 退出码 0 |
| `GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go test -c ./internal/runner` | 退出码 0 |
| `GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go test -c ./internal/runner` | 退出码 0 |
| `GOOS=darwin GOARCH=amd64 CGO_ENABLED=0 go test -c ./internal/runner` | 退出码 0 |
| `GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./...` | `Success` |
| `GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build ./...` | `Success` |

说明：本环境无法直接取回 `go test -v` 的逐行原始输出（CLI 代理会折叠为汇总行），因此上表只记录**实际观察到的汇总结果**；未在报告中编造 `-v` 逐行清单。交叉编译只证明编译，不证明运行。

## 5. 变异验证（测试能否发现真实故障）

为避免"只复刻实现"的测试，逐一注入真实缺陷并确认测试失败：

| 注入的缺陷 | 期望失败的测试 | 实测 |
|---|---|---|
| `postStart` 错误被忽略（恢复 agy 初稿行为） | `TestRunFailsClosedWhenConfinementFails` | **失败**：`the child survived a failed confinement handshake` |
| 同上 | `TestRunPostStartFailure`（仅断言 Result 字段） | **通过** → 证明"只断言字段"的测试无法发现子进程泄漏，需要 sentinel 版本 |
| Unix `terminate` 在 `waitDone` 后直接返回 | `TestRunKillsDescendantsThatIgnoreTerm` | **失败**：`process 39554 is still alive after 5s` |
| 移除预取消 ctx 检查 | `TestRunPreCancelledContext` | **失败**：`expected error for pre-cancelled context, got nil` |
| 移除 stdin join | `TestRunJoinsStdinWriterWhenDescendantHoldsStdin`（早期计时版） | 失败 → 进一步排查发现 `os/exec.Wait` 会关闭 StdinPipe，遂改为断言"及时返回 + goroutine 回落基线"，并在 §2.5 记录该发现 |

注入代码已全部还原（`grep -r MUTATION internal/runner` 无匹配），还原后全量测试与 race 仍为 68 passed。

## 6. 未完成项、未验证项与剩余风险

1. **Windows 运行时未验证**：`process_windows.go`（Job Object、CREATE_SUSPENDED、Toolhelp 主线程查找、`ResumeThread`）只做交叉编译，从未在 Windows 内核执行。Job 嵌套、`AssignProcessToJobObject` 在受限环境下的 `ERROR_ACCESS_DENIED`、CTRL_BREAK 宽限行为均未实测。失败路径按 fail-closed 设计（杀子进程 + `process_setup`），但**不能宣称 Windows 进程树管理已验证**。
2. **Linux/arm64、darwin/amd64 运行时未验证**：仅交叉编译通过；实际测试只在 `darwin/arm64` 执行。
3. **正常退出后不清理后代（有意限制）**：直接子进程正常退出并被 `Wait` 回收后，其 PGID 可能被内核复用，继续 `kill(-pgid)` 有误杀无关进程组的风险，因此不做清理；只有取消路径（子进程尚未回收）会清理全组。要覆盖该场景需要 pidfd（Linux）或 cgroup，属于后续工作。已由 `TestRunDoesNotSignalGroupAfterNormalExit` 固定为已知行为。
4. **无网关自身崩溃兜底**：未设置 Linux `Pdeathsig`，网关进程被 SIGKILL 时子进程可能残留。`Pdeathsig` 需要 `runtime.LockOSThread` 才能可靠工作，本阶段未引入。
5. **非沙箱**：无 namespace/cgroup/seccomp/AppContainer；`WorkDir` 只是启动目录。可执行文件与参数必须来自受信本地配置。
6. **`Result.Text` 语义**：合并流的有界**尾部**（非头部），截断只保证"有效 UTF-8"，不保证与原始字节一一对应。
7. **并发写入者事件（§7.1）未解决**：同一 worktree 存在第二个写入者，存在相互覆盖的风险。

## 7. 协作异常与需要协调者裁决的事项

### 7.1 `internal/runner/runner_test.go` 与本报早期版本由并发会话写入
事实时间线（本会话观察）：
1. 本会话创建 `runner_test.go` 并运行测试（当次 66 passed / 1 failed，失败点是本会话测试自身的期望值笔误）。
2. 该文件随后被替换为一份基于 `init()` + `GO_WANT_HELPER_PROCESS` + `--` 哨兵的版本（约 700 行，含 `helperConfig`），其中 `syscall.Kill` 导致 `GOOS=windows go vet` 报 `undefined: syscall.Kill`（Windows 测试二进制无法编译）。
3. 该文件再次被整体替换为当前版本（约 380 行）：改用本会话建立的 `newConfig`/helper 协议，Windows 编译问题随之消失；它包含 `runWith(...)`、`ErrorCodeProcessSetup`、`killPIDFromFile` 等**本会话在本次工作中新引入的标识符**。
4. `docs/reports/agy-M1.md` 在本会话写报告前已存在，其内容描述的是本会话创建的 `runner_exec_test.go` 等文件。

结论与请求：
- 当前 `runner_test.go` 通过全部测试（本会话已独立复核：`go test` 与 `go test -race` 均 68 passed），因此**本会话保留了它，未删除他人产出**。
- 但它并非本会话所写，且与 `runner_exec_test.go` 存在覆盖重叠（argv/stdin/workdir/env/退出码/stderr/大输出/预取消/取消/deadline/postStart/大 stdin/遗留管道/并发）。请协调者确认其归属，并决定是否合并为单一测试文件。
- 本会话对该文件做过的唯一改动：删除了其中"读取后代 pid 后无条件 SIGKILL、且不做任何断言"的 `TestRunDescendantCleanup`（无法发现真实故障，且使用 Windows 不存在的 `syscall.Kill`），其覆盖由 `runner_unix_test.go` 的两个强断言用例取代。
- 若协调者确认"每个目录只允许一个写入者"，建议核对并发会话是否应停止写入本 worktree；否则双方产出可能互相覆盖。

### 7.2 需要协调者确认的接口细节
1. 未启动子进程时（预取消 ctx、`Start` 失败）本实现返回**包装 `protocol.ErrInvalid` 的 error**。任务书要求"不启动子进程"，但未指定 error 的具体类别；若 M2 需要区分"取消"与"非法输入"，建议在契约中补充稳定错误码。
2. 新增稳定错误码常量 `ErrorCodeProcessSetup = "process_setup"`（冻结签名之外的新增导出标识符），用于 confinement 失败。请确认是否接受。
3. `Run` 对同一 `Config` 的并发调用是安全的（`validate` 只改本地副本），但 `Config.Args` 切片不会被修改；如后续要求"复制并冻结配置"，需更新契约。

## 8. 交接清单

- 变更路径：`internal/runner/**`（源文件与测试）、`docs/reports/agy-M1.md`。
- 未变更：`internal/protocol`、`go.mod`、`go.sum`、其他文档、全局配置、真实 Agent 凭据。
- 未执行：`git add`/`commit`/`push`、服务安装、真实 Agent 调用、递归派生 agent。
- 复现命令：`gofmt -l ./internal && go vet ./... && go test -count=1 ./internal/runner && go test -race -count=1 ./internal/runner`，交叉编译见 §4。
