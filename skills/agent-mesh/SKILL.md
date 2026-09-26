---
name: agent-mesh-cluster
description: 调度和管理分布式 Agent Gateway 网关集群，支持跨机器执行任务、环境探测、节点维护与结果回传。
version: 1.0.0
---

# Agent Mesh 集群调度与执行专家技能规范 (SKILL.md)

当你拥有此技能时，你已具备**分布式多机 Agent 协作与集群调度能力**。你可以通过网关的 MCP 工具直接感知局域网/远程多台机器的在线状态、所安装的 Agent 工具链，并根据用户的需求将开发、测试、构建或运维任务精准派发到最佳目标机器上执行。

---

## 🛠️ 可用工具清单 (Tools Reference)

网关 MCP 服务提供以下 7 个核心工具：

1. **`device_list`**：
   - **作用**：查询集群中所有已配对注册的节点机器及其状态。
   - **返回数据**：各节点的 `node_id`、证书有效期、是否撤销，以及该节点当前在线状态与本地已安装的通用 Agent 工具（如 `claude`, `codex`, `python3`, `docker` 等）。
   - **最佳调用时机**：在用户未指定目标机器，或者需要了解哪台机器具备特定运行环境时**首先调用**。

2. **`handoff_to_computer_agent`**（极简智能派发，强烈推荐）：
   - **作用**：向指定节点或集群中最适合的节点派发一个执行任务。
   - **参数**：
     - `instruction` (必填，字符串)：具体的任务指令或需要运行的命令。
     - `target_node` (可选，字符串)：目标机器的 `node_id`。如果省略，网关调度器会自动匹配一台健康的在线机器。
     - `context` (可选，字符串)：任务的前置上下文、需求背景或需要参考的文件内容。
     - `agent` (可选，字符串)：能力标识（默认 `agent.run`）。
     - `timeout_seconds` (可选，整数)：最大超时秒数（默认 120 秒）。
   - **返回**：`task_id`, `node_id`, `state`（初始通常为 `queued`）。

3. **`wait_task_result`**（同步结果等待）：
   - **作用**：挂起等待指定任务执行完成并返回最终的标准输出、错误输出及退出码。
   - **参数**：
     - `task_id` (必填，字符串)：任务唯一标识。
     - `timeout_seconds` (可选，整数)：最长等待秒数（默认 30，最大 120）。
   - **返回**：`state` (`succeeded` / `failed` / `cancelled`), `output` (终端执行输出), `exit_code`。

4. **`task_submit`**：
   - **作用**：直接向指定 `node_id` 提交底层原始指令任务。
5. **`task_get`**：
   - **作用**：非阻塞主动轮询或查看某任务当前的执行快照。
6. **`task_cancel`**：
   - **作用**：取消正在排队或执行中的任务。
7. **`doctor_diagnose`**：
   - **作用**：一键自检网关与集群环境的健康状态（CA 证书、时钟同步、数据库完整性）。

---

## 🧭 标准执行工作流 (Standard Operating Procedure)

面对用户的多机协作或远程执行请求，请严格遵循**四步闭环工作流**：

```mermaid
flowchart TD
    A[用户提出远程执行/构建需求] --> B[步骤 1: 调用 device_list 查询可用节点及环境]
    B --> C{目标节点是否明确?}
    C -->|是| D[调用 handoff_to_computer_agent 指定 target_node]
    C -->|否| E[挑选在线且具备所需工具的节点，或直接让网关自动选机]
    E --> D
    D --> F[步骤 2: 获取 task_id 并调用 wait_task_result 同步等待完成]
    F --> G{任务是否终端完成?}
    G -->|成功 (succeeded)| H[步骤 3: 分析 output 终端输出，整理成果回复用户]
    G -->|超时/失败 (failed)| I[步骤 4: 检查 exit_code 与 error，给出排查建议或重试]
```

### 关键规范：
* **必须等待结果**：调用 `handoff_to_computer_agent` 成功创建任务后，**必须立即调用 `wait_task_result`** 获取实际执行输出。千万不要只提交了任务就告诉用户“已完成”！
* **退出码检查**：如果 `exit_code != 0`，代表远端命令执行出错，请将 output 中的关键报错摘录并向用户清晰说明。

---

## 💻 各主流客户端安装与使用说明

### 1. 豆包桌面版 (Doubao Desktop)
- **接入 MCP**：
  在设置 -> MCP 服务器 -> 添加 SSE 服务：
  - URL: `http://<网关IP>:8088/mcp?token=<操作员Token>` (推荐使用 HTTP 8088，免证书阻拦)
- **挂载技能提示词**：
  在豆包的【自定义指令 / 角色提示词】或对话开头粘贴：
  ```text
  你已接入 Agent Gateway 统一集群调度网关。当你需要执行本地无法运行的重型任务、Linux环境构建、自动化测试或远程巡检时，请优先使用 device_list 查询节点，并通过 handoff_to_computer_agent 与 wait_task_result 闭环完成执行，最后将机器输出汇报给我。
  ```

### 2. Cursor / Windsurf / VSCode
- **接入 MCP**：
  在 `cursor_settings.json` 或 `mcpServers` 中配置：
  ```json
  {
    "mcpServers": {
      "agent-gateway": {
        "url": "http://<网关IP>:8088/mcp?token=<操作员Token>"
      }
    }
  }
  ```
- **挂载技能**：
  在工作区根目录创建 `.cursorrules`，将本 `SKILL.md` 的内容粘贴进去即可。

### 3. Claude Desktop / Claude Code
- **配置文件**：`~/Library/Application Support/Claude/claude_desktop_config.json`
  ```json
  {
    "mcpServers": {
      "agent-gateway": {
        "command": "npx",
        "args": ["-y", "@modelcontextprotocol/server-sse", "http://<网关IP>:8088/mcp?token=<操作员Token>"]
      }
    }
  }
  ```

---

## 🎯 典型场景示例 (Real-World Examples)

### 场景一：在远端 Linux 服务器运行自动化测试
> **用户输入**：“请在 81 机器上帮我跑一下所有的回归测试。”
>
> **大模型标准操作链**：
> 1. 调用 `device_list` 确认 `node-bdb478eda935f4cc` (192.168.3.81) 在线；
> 2. 调用 `handoff_to_computer_agent(target_node="node-bdb478eda935f4cc", instruction="go test -v ./...")`；
> 3. 拿到返回的 `task_id`，立即调用 `wait_task_result(task_id="...", timeout_seconds=60)`；
> 4. 读取回传的测试报告，告知用户 PASS / FAIL 详情。

### 场景二：智能寻找具备 Docker 的机器并构建容器
> **用户输入**：“找一台装有 Docker 的机器，帮我构建这个镜像。”
>
> **大模型标准操作链**：
> 1. 调用 `device_list` 遍历各节点 `agents` 标签，找到具备 `docker` 的机器节点；
> 2. 将 Dockerfile 构建指令派发至该机器；
> 3. 等待构建完成并回传 Image ID。
