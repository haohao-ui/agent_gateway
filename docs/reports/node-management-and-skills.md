# 节点全生命周期管理、证书配置与 Agent 技能体系交付报告

## 1. 任务概述

针对 Agent Gateway 在生产运维场景中的痛点，本阶段完成了 5 项关键能力演进：
1. **自定义 TLS 证书管理与系统平滑热重启**：支持外部合规证书（`server.crt` / `server.key`）上传与安全校验，支持 Web/API 触发网关平滑热重启；
2. **节点安装所需文件一键分发中心**：网关内置开箱即用的分发端点（`GET /download/mesh` 多架构自动分发、`GET /download/ca.crt`、`GET /download/install.sh`）；
3. **节点远程升级与自愈能力**：节点内核支持自升级指令（`MESH_SYS:UPGRADE <url>`）及远程安全重启指令（`MESH_SYS:RESTART`）；
4. **节点元数据与通用 Agent 工具自动探测上报**：节点启动自动探测环境通用开发工具（`python3`, `node`, `docker`, `git`, `bash`, `go`）与 AI 运行环境，心跳实时上报网关，Web 控制台可视化呈现并支持直接下发管理指令；
5. **官方配套 Agent 技能体系 (Agent Mesh Skill)**：发布官方标准 `skills/agent-mesh/SKILL.md`，并在 Web 控制台上线【💡 技能中心】，提供豆包、Cursor、Claude Desktop 一键配置及 System Prompt。

---

## 2. 接口与契约变动

### 2.1 网关核心接口扩充
- `GET /download/mesh?arch={arch}`：二进制下载中心，根据 User-Agent 或参数自动匹配 `linux-amd64` / `darwin-amd64` / `arm64` 架构；
- `GET /download/ca.crt`：网关 CA 根证书下载；
- `GET /download/install.sh`：一键自动安装、配对并以守护进程拉起节点的 Shell 脚本；
- `GET /skills/agent-mesh/SKILL.md`：官方技能包直链；
- `GET /api/system/tls`：查询当前 TLS 证书来源（自定义证书还是内置 CA 生成）；
- `POST /api/system/tls/upload`：上传自定义证书与私钥（包含 X.509 预校验）；
- `POST /api/system/tls/reset`：清除自定义证书，回退至内置 CA；
- `POST /api/system/restart`：平滑热重启网关进程；
- `POST /v1/operator/devices/{id}/restart`：向指定工作节点下发远程重启指令；
- `POST /v1/operator/devices/{id}/upgrade`：向指定工作节点下发远程自升级指令。

### 2.2 协议扩充与元数据 (`internal/protocol/wire.go`)
- `ClaimRequest` 扩充字段：
  - `NodeVersion`: 节点客户端版本（如 `"0.1.0"`）；
  - `OS`: 操作系统类型（如 `"darwin"`, `"linux"`）；
  - `Arch`: 系统架构（如 `"amd64"`, `"arm64"`）；
  - `Agents`: 探测到的本机工具列表（如 `[{"id":"python3","name":"Python 3","runnable":true}, ...]`）。

---

## 3. 真实环境验证记录

### 3.1 远程真实节点一键安装与配对测试
在远程机器 `work@192.168.3.81` 执行一键安装脚本：
```bash
curl -fsSL http://192.168.3.237:8088/download/install.sh | bash -s -- _oakRb6P_B23u6MDJM8wuUTGjYntgyF_wceACo9pRkw
```
**真实输出**:
```text
=== Agent Mesh Node Installer ===
[+] Target Gateway:  https://192.168.3.237:8443
[+] Download URL:    http://192.168.3.237:8088
[+] Detected Platform: darwin-amd64
[+] Downloading mesh executable for darwin-amd64...
[+] Downloading CA certificate...
[+] Pairing node with gateway...
paired node node-1035d230a700082b
[+] Launching node background daemon...
[✓] Agent Mesh Node successfully installed and running! Node ID: node-1035d230a700082b
```

### 3.2 节点状态与工具链探测验证
网关 API `GET /v1/operator/devices` 真实返回：
```json
[
  {
    "node_id": "node-1035d230a700082b",
    "fingerprint": "1a2bbc8565ce541458f6751d185928fea2556f8506b86d6c424680009fce11f9",
    "cert_expires_at": "2026-12-25T07:04:44Z",
    "revoked": false,
    "version": "0.1.0",
    "os": "darwin",
    "arch": "amd64",
    "agents": [
      {"id": "agent.run", "name": "agent.run", "kind": "cli", "runnable": true},
      {"id": "bash", "name": "Bash Shell", "kind": "cli", "runnable": true},
      {"id": "chatgpt", "name": "ChatGPT", "kind": "gui", "runnable": false},
      {"id": "docker", "name": "Docker", "kind": "cli", "runnable": true},
      {"id": "git", "name": "Git", "kind": "cli", "runnable": true},
      {"id": "go", "name": "Go Runtime", "kind": "cli", "runnable": true},
      {"id": "hermes", "name": "Hermes", "kind": "cli", "runnable": true},
      {"id": "python3", "name": "Python 3", "kind": "cli", "runnable": true}
    ],
    "online": true,
    "last_seen": "2026-09-26T07:10:58.972617Z"
  }
]
```

### 3.3 远程重启与自愈指令下发
调用网关指令：
```bash
curl -fsS -X POST http://192.168.3.237:8088/v1/operator/devices/node-1035d230a700082b/restart \
  -H "Authorization: Bearer _oakRb6P_B23u6MDJM8wuUTGjYntgyF_wceACo9pRkw"
```
**远端节点日志回显 (`/Users/work/node-auto/node.log`)**:
```text
time=2026-09-26T15:08:23.877+08:00 level=INFO msg="received system restart command" task=task_29af9cd80f6e4b7911bf51a0d56bf3fb attempt=att_b7d0717ab80761398c6f160fe520bef7 capability=agent.run
time=2026-09-26T15:08:25.011+08:00 level=INFO msg="node started" server=https://192.168.3.237:8443 work_dir=/Users/work/node-auto/work capabilities=1 lease_seconds=60 renew_seconds=15
```
节点成功响应重启指令，平滑退出并重新拉起新实例，心跳保持在线。

---

## 4. 全套单元测试结果
```bash
go test ./internal/...
```
**结果**: 100% 全部通过 (PASS)。
