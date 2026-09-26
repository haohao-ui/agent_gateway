# Agent 技能安装、适配器配置与调用规范 (Agent Skill & Tool Calling Spec)

本规范规定了在大模型宿主（如豆包、Claude Desktop、Cursor）以及边缘工作机（Node）上接入和调用 Agent Gateway 的统一标准与最佳实践。

---

## 1. 核心概念与拓扑关系

- **控制面（Control Plane / Gateway）**：网关中心，负责任务排队调度、设备身份管理（CA 与 mTLS）、操作员权限、Web 控制台与远程 MCP 端点。
- **执行面（Worker / Node）**：部署在工作机器上的常驻守护进程，使用双向 mTLS 向网关长轮询领取任务，并在本地调用已注册的 Agent 适配器执行。
- **适配器（Adapter）**：Node 本地预先配置并验证过的非交互式可执行程序模板，是网关指令落地的唯一受控入口。
- **已发现软件（Installed Agents）**：Node 宿主机上自动检测到的桌面应用或 CLI 工具，供上层感知软件环境，仅供参考，未经适配器显式配置者不可直接派发。

---

## 2. 节点 Agent 安装与配置规范

### 2.1 配置文件规范（`node.json`）
Node 配置文件默认位于用户主目录 `~/.agent-gateway/node.json`，权限必须设为 `0600`。

```json
{
  "node_id": "macbook-pro-work",
  "gateway_url": "https://gateway.internal:8443",
  "workdir": "/Users/developer/projects",
  "file_roots": [
    "/Users/developer/projects",
    "/Users/developer/Downloads"
  ],
  "adapters": {
    "claude": {
      "name": "Claude Code",
      "executable": "/usr/local/bin/claude",
      "args": ["-p", "{instruction}"],
      "env": {
        "ANTHROPIC_API_KEY": "sk-ant-..."
      },
      "timeout_seconds": 600
    },
    "codex": {
      "name": "Codex Exec",
      "executable": "/usr/local/bin/codex",
      "args": ["exec", "--skip-git-repo-check", "--", "{instruction}"],
      "timeout_seconds": 300
    }
  }
}
```

### 2.2 适配器安全规则（强制执行）
1. **绝对可执行文件路径**：`executable` 必须解析为宿主机上真实存在的绝对路径；
2. **禁止 Shell 解释与字符串拼接**：
   - 参数列表必须为独立的字符串数组；
   - 有且仅能包含一个独立的 `"{instruction}"` 占位符；
   - 严禁使用 `sh -c`、`bash -c` 或 `cmd.exe /c` 执行任务指令，防止注入攻击；
3. **严格工作区限制**：任务进程的当前工作目录（cwd）必须在 `workdir` 内部，文件读写限制在 `file_roots` 声明的目录内；
4. **非交互模式保障**：配置的参数必须保证被调用的 Agent 以无人值守方式运行，超时未退出时由系统自动终止进程组。

### 2.3 常驻服务配置
配置完成后，在节点机器上运行：
```bash
agent-gateway service install --role node
```
将以用户身份注册为系统守护服务（macOS `launchd` / Linux `systemd`），开机自启、崩溃自动重启，并自动向网关保持长轮询连接。

---

## 3. 大模型调用规范（MCP 远程调用流程）

大模型客户端（如电脑端豆包、Claude、Cursor）通过配置网关提供的 MCP URL（如 `https://gateway.example:8443/mcp`，携带 Bearer Token）进行跨机器协作。

### 3.1 跨端协同标准时序（以豆包手机-电脑流转为例）

```text
手机豆包会话（用户下达指令）
   ↓ 同步至电脑端
电脑端豆包（获取会话上下文）
   ↓ 调用 MCP: list_devices
查看在线电脑列表与各电脑支持的 Agent 名称
   ↓ 调用 MCP: handoff_to_computer_agent
转交任务（包含上下文 context、指令 instruction、指定或自动选择电脑）
   ↓ 调用 MCP: wait_task_result
同步挂起等待最多 120 秒，直接获取执行结果
   ↓
电脑端豆包将执行结果写回原会话，同步回手机端
```

### 3.2 核心 MCP 工具与参数规范

#### A. 设备与软件查询：`list_devices`
- **目的**：查询当前已接入的所有设备、各设备在线状态、可执行的 Agent 适配器列表（`agents`）以及已发现的软件清单（`installed_agents`）；
- **参数**：无；
- **注意**：大模型只能派发给目标设备 `agents` 列表中明确声明的名称，不能派发给仅在 `installed_agents` 中出现但 `runnable: false` 的 GUI 软件。

#### B. 跨端交接任务：`handoff_to_computer_agent`
- **目的**：将用户在手机或主对话中的任务指派给特定电脑的 Agent 执行；
- **参数**：
  - `instruction`（string，必填）：具体的任务执行指令；
  - `agent`（string，必填）：目标 Agent 名称（如 `"claude"` 或 `"codex"`）；
  - `target_device`（string，选填）：目标设备 ID。**若省略，网关将根据 Agent 可用性与节点最新活跃状态自动选择最佳设备**；
  - `context`（string，选填）：前序对话或手机端会话的摘要背景；
- **返回值**：返回任务元数据，包含 `task_id`、`root_task_id`、`target_device` 与状态 `queued`。

#### C. 短任务同步等待：`wait_task_result`
- **目的**：短任务同步阻塞等待结果，避免大模型陷入反复查询的死循环；
- **参数**：
  - `task_id`（string，必填）：任务 ID；
  - `timeout_seconds`（int，选填）：等待超时秒数（默认 30，最大 120）；
- **返回值**：
  - 若在超时前任务结束，返回包含 `status: "succeeded"`、`result: { "text": "..." }` 的完整结果；
  - 若超时未完成，返回当前状态（`running`），大模型可稍后通过 `get_task_result` 继续查询。

#### D. 精确任务提交：`submit_task`
- **目的**：指定设备与 Agent 提交后台长任务；
- **参数**：
  - `target_device`（string，必填）；
  - `agent`（string，必填）；
  - `instruction`（string，必填）。

---

## 4. 任务执行回执标准格式

大模型在将远程执行结果反馈给最终用户时，推荐采用以下结构化 Markdown 呈现，保证清晰透明：

```markdown
### 🖥️ 远程任务执行完成
- **执行设备**：办公 MacBook Pro (`node-mac-office`)
- **执行引擎**：Claude Code (`claude`)
- **任务编号**：`task-8a9f2b1c`
- **执行状态**：✅ 成功 (`succeeded`)
- **耗时**：8.5 秒

#### 执行输出内容：
```
[这里呈现任务实际输出的文本或产物链接]
```
```
