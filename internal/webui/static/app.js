// Agent Gateway Web Dashboard Logic
(function() {
  'use strict';

  // State
  let tasks = [];
  let nodes = [];
  let currentTask = null;
  let activeTab = 'tab-tasks';

  // DOM Elements
  const statusIndicator = document.getElementById('connection-status');
  const refreshBtn = document.getElementById('refresh-btn');
  const taskStateFilter = document.getElementById('task-state-filter');
  const taskSearch = document.getElementById('task-search');
  const tasksTableBody = document.getElementById('tasks-table-body');
  const nodesTableBody = document.getElementById('nodes-table-body');

  // Metrics
  const metricNodes = document.getElementById('metric-nodes');
  const metricQueued = document.getElementById('metric-queued');
  const metricRunning = document.getElementById('metric-running');
  const metricSucceeded = document.getElementById('metric-succeeded');
  const metricFailed = document.getElementById('metric-failed');
  const metricUnknown = document.getElementById('metric-unknown');

  // Modals & Views
  const taskModal = document.getElementById('task-modal');
  const reconcileModal = document.getElementById('reconcile-modal');
  const loginView = document.getElementById('login-view');
  const dashboardView = document.getElementById('dashboard-view');
  const loginForm = document.getElementById('login-form');
  const loginUsername = document.getElementById('login-username');
  const loginPassword = document.getElementById('login-password');
  const loginError = document.getElementById('login-error');
  const logoutBtn = document.getElementById('logout-btn');
  const userBadge = document.getElementById('user-badge');

  const submitForm = document.getElementById('submit-task-form');
  const submitMessage = document.getElementById('submit-message');
  const runDoctorBtn = document.getElementById('run-doctor-btn');
  const doctorResults = document.getElementById('doctor-results');

  // Token & Auth management
  const urlParams = new URLSearchParams(window.location.search);
  if (urlParams.has('token')) {
    localStorage.setItem('agent_gateway_token', urlParams.get('token'));
    window.history.replaceState({}, document.title, window.location.pathname);
  }
  let operatorToken = localStorage.getItem('agent_gateway_token') || '';

  function authHeaders(extra = {}) {
    const headers = { 'Content-Type': 'application/json', ...extra };
    if (operatorToken) {
      headers['Authorization'] = `Bearer ${operatorToken}`;
    }
    return headers;
  }

  function showLogin(errMsg) {
    if (loginView) {
      loginView.classList.remove('hidden');
      loginView.style.display = 'flex';
    }
    if (dashboardView) {
      dashboardView.classList.add('hidden');
      dashboardView.style.display = 'none';
    }
    if (errMsg && loginError) {
      loginError.textContent = errMsg;
      loginError.classList.remove('hidden');
      loginError.style.display = 'block';
    } else if (loginError) {
      loginError.classList.add('hidden');
      loginError.style.display = 'none';
    }
  }

  function showDashboard() {
    if (loginView) {
      loginView.classList.add('hidden');
      loginView.style.display = 'none';
    }
    if (dashboardView) {
      dashboardView.classList.remove('hidden');
      dashboardView.style.display = 'block';
    }
    if (userBadge) userBadge.textContent = '👤 admin';
    updateMCPDocs();
  }

  if (loginForm) {
    loginForm.addEventListener('submit', async (e) => {
      e.preventDefault();
      loginError.classList.add('hidden');
      const u = loginUsername.value.trim();
      const p = loginPassword.value;
      try {
        const res = await fetch('/api/login', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ username: u, password: p }),
          credentials: 'same-origin'
        });
        if (res.ok) {
          const data = await res.json();
          operatorToken = data.token || '';
          if (operatorToken) {
            localStorage.setItem('agent_gateway_token', operatorToken);
          }
          showDashboard();
          await fetchTasks();
          await fetchNodes();
          connectSSE();
        } else {
          loginError.textContent = '账号或密码错误（默认账号: admin / 密码: admin）';
          loginError.classList.remove('hidden');
        }
      } catch (err) {
        loginError.textContent = '登录请求失败: ' + err;
        loginError.classList.remove('hidden');
      }
    });
  }

  if (logoutBtn) {
    logoutBtn.addEventListener('click', async () => {
      try {
        await fetch('/api/logout', { method: 'POST', credentials: 'same-origin' });
      } catch (e) {}
      operatorToken = '';
      localStorage.removeItem('agent_gateway_token');
      if (activeSSE) {
        try { activeSSE.close(); } catch (e) {}
        activeSSE = null;
      }
      showLogin();
    });
  }

  // 1. Navigation Tabs
  document.querySelectorAll('.tab-btn').forEach(btn => {
    btn.addEventListener('click', () => {
      document.querySelectorAll('.tab-btn').forEach(b => b.classList.remove('active'));
      document.querySelectorAll('.tab-pane').forEach(p => p.classList.remove('active'));

      btn.classList.add('active');
      const targetId = btn.getAttribute('data-tab');
      const targetPane = document.getElementById(targetId);
      if (targetPane) {
        targetPane.classList.add('active');
        activeTab = targetId;
      }
    });
  });

  // 2. Fetch Tasks & Nodes
  async function fetchTasks() {
    try {
      const res = await fetch('/v1/operator/tasks', { headers: authHeaders(), credentials: 'same-origin' });
      if (res.ok) {
        tasks = await res.json();
        renderTasks();
        updateMetrics();
      } else if (res.status === 401) {
        console.warn('Operator authentication required');
        showLogin('凭据已失效，请重新登录');
      } else {
        console.error('Failed to load tasks:', res.statusText);
      }
    } catch (err) {
      console.error('Error fetching tasks:', err);
    }
  }

  async function fetchNodes() {
    try {
      const res = await fetch('/v1/operator/devices', { headers: authHeaders(), credentials: 'same-origin' });
      if (res.ok) {
        nodes = await res.json();
        renderNodes();
        updateMetrics();
      } else if (res.status === 401) {
        console.warn('Operator authentication required');
        showLogin('凭据已失效，请重新登录');
      } else {
        console.error('Failed to load nodes:', res.statusText);
      }
    } catch (err) {
      console.error('Error fetching nodes:', err);
    }
  }

  function updateMetrics() {
    metricNodes.textContent = nodes.filter(n => !n.revoked).length;

    let queued = 0, running = 0, succeeded = 0, failed = 0, unknown = 0;
    tasks.forEach(t => {
      switch (t.state) {
        case 'queued': queued++; break;
        case 'leased':
        case 'running': running++; break;
        case 'succeeded': succeeded++; break;
        case 'failed': failed++; break;
        case 'unknown': unknown++; break;
      }
    });

    metricQueued.textContent = queued;
    metricRunning.textContent = running;
    metricSucceeded.textContent = succeeded;
    metricFailed.textContent = failed;
    metricUnknown.textContent = unknown;
  }

  // 3. Render Tables
  function renderTasks() {
    const filter = taskStateFilter.value.toLowerCase();
    const query = taskSearch.value.trim().toLowerCase();

    const filtered = tasks.filter(t => {
      if (filter && t.state.toLowerCase() !== filter) return false;
      if (query) {
        const idMatch = t.id && t.id.toLowerCase().includes(query);
        const nodeMatch = t.node_id && t.node_id.toLowerCase().includes(query);
        if (!idMatch && !nodeMatch) return false;
      }
      return true;
    });

    if (filtered.length === 0) {
      tasksTableBody.innerHTML = '<tr><td colspan="7" class="empty-state">没有符合条件的数据</td></tr>';
      return;
    }

    tasksTableBody.innerHTML = filtered.map(t => {
      let badgeClass = `badge-${t.state}`;
      let instructionSummary = '-';
      if (t.input) {
        try {
          const parsed = typeof t.input === 'string' ? JSON.parse(t.input) : t.input;
          instructionSummary = parsed.instruction || JSON.stringify(parsed);
        } catch {
          instructionSummary = String(t.input);
        }
      }
      if (instructionSummary.length > 45) {
        instructionSummary = instructionSummary.slice(0, 42) + '...';
      }

      const updated = t.updated_at ? new Date(t.updated_at).toLocaleTimeString() : '-';

      let actionButtons = `<button class="btn btn-secondary btn-sm view-btn" data-id="${t.id}">详情</button>`;
      if (t.state === 'unknown') {
        actionButtons += ` <button class="btn btn-primary btn-sm reconcile-btn" data-id="${t.id}">对账</button>`;
      } else if (t.state === 'queued' || t.state === 'leased' || t.state === 'running') {
        actionButtons += ` <button class="btn btn-danger btn-sm cancel-btn" data-id="${t.id}">取消</button>`;
      }

      return `
        <tr>
          <td><code>${t.id}</code></td>
          <td><span class="badge ${badgeClass}">${t.state}</span></td>
          <td>${t.node_id || '-'}</td>
          <td>${t.capability || 'agent.run'}</td>
          <td title="${escapeHtml(String(instructionSummary))}">${escapeHtml(instructionSummary)}</td>
          <td>${updated}</td>
          <td>${actionButtons}</td>
        </tr>
      `;
    }).join('');

    // Attach row events
    tasksTableBody.querySelectorAll('.view-btn').forEach(btn => {
      btn.addEventListener('click', () => showTaskDetail(btn.getAttribute('data-id')));
    });
    tasksTableBody.querySelectorAll('.reconcile-btn').forEach(btn => {
      btn.addEventListener('click', () => openReconcileModal(btn.getAttribute('data-id')));
    });
    tasksTableBody.querySelectorAll('.cancel-btn').forEach(btn => {
      btn.addEventListener('click', () => cancelTask(btn.getAttribute('data-id')));
    });
  }

  function renderNodes() {
    if (nodes.length === 0) {
      nodesTableBody.innerHTML = '<tr><td colspan="6" class="empty-state">当前暂无已登记节点</td></tr>';
      return;
    }

    nodesTableBody.innerHTML = nodes.map(n => {
      const isRevoked = n.revoked;
      const isOnline = n.online;
      let statusBadge = '<span class="badge" style="background:#4b5563;color:#9ca3af;">OFFLINE</span>';
      if (isRevoked) {
        statusBadge = '<span class="badge badge-revoked">REVOKED</span>';
      } else if (isOnline) {
        statusBadge = '<span class="badge badge-active" style="background:rgba(16,185,129,0.2);color:#34d399;">ONLINE</span>';
      }

      const versionStr = n.version ? `<code>v${escapeHtml(n.version)}</code>` : '<span style="color:var(--text-muted)">-</span>';
      const sysStr = (n.os && n.arch) ? `<span style="font-size:11px;color:var(--text-muted);">${escapeHtml(n.os)}/${escapeHtml(n.arch)}</span>` : '';

      let agentsHtml = '<span style="font-size:11px;color:var(--text-muted);">未上报</span>';
      if (Array.isArray(n.agents) && n.agents.length > 0) {
        agentsHtml = n.agents.map(a => {
          return `<span class="badge" style="font-size:10px;margin:2px;background:rgba(56,189,248,0.15);color:var(--primary);border:1px solid rgba(56,189,248,0.3);">${escapeHtml(a.id)}</span>`;
        }).join('');
      }

      const expiresAt = n.cert_expires_at ? new Date(n.cert_expires_at).toLocaleDateString() : '-';

      let actions = '';
      if (!isRevoked && isOnline) {
        actions += `<button class="btn btn-secondary btn-sm restart-node-btn" data-id="${n.node_id}" style="margin-right:4px;">🔄 重启</button>`;
        actions += `<button class="btn btn-primary btn-sm upgrade-node-btn" data-id="${n.node_id}" style="margin-right:4px;">🚀 更新</button>`;
      }
      if (!isRevoked) {
        actions += `<button class="btn btn-warning btn-sm revoke-node-btn" data-id="${n.node_id}" style="margin-right:4px;">撤销</button>`;
      } else {
        actions += `<button class="btn btn-secondary btn-sm" disabled style="margin-right:4px;">已撤销</button>`;
      }
      actions += `<button class="btn btn-danger btn-sm delete-node-btn" data-id="${n.node_id}" title="从网关中彻底删除该节点">🗑️ 删除</button>`;

      return `
        <tr>
          <td><strong>${escapeHtml(n.node_id)}</strong></td>
          <td>${statusBadge}</td>
          <td><div>${versionStr}</div><div>${sysStr}</div></td>
          <td style="max-width:240px;">${agentsHtml}</td>
          <td>${expiresAt}</td>
          <td>${actions}</td>
        </tr>
      `;
    }).join('');

    nodesTableBody.querySelectorAll('.revoke-node-btn').forEach(btn => {
      btn.addEventListener('click', () => revokeNode(btn.getAttribute('data-id')));
    });
    nodesTableBody.querySelectorAll('.restart-node-btn').forEach(btn => {
      btn.addEventListener('click', () => restartNode(btn.getAttribute('data-id')));
    });
    nodesTableBody.querySelectorAll('.upgrade-node-btn').forEach(btn => {
      btn.addEventListener('click', () => upgradeNode(btn.getAttribute('data-id')));
    });
    nodesTableBody.querySelectorAll('.delete-node-btn').forEach(btn => {
      btn.addEventListener('click', () => deleteNode(btn.getAttribute('data-id')));
    });
  }

  async function deleteNode(nodeId) {
    if (!confirm(`确定要从网关中彻底删除节点 [${nodeId}] 吗？\n删除后该节点将从数据库注销。如果节点仍需要使用，需重新运行配对。`)) return;
    try {
      const res = await fetch(`/v1/operator/devices/${nodeId}`, {
        method: 'DELETE',
        headers: authHeaders(),
        credentials: 'same-origin'
      });
      const data = await res.json().catch(() => ({}));
      if (res.ok) {
        alert(`节点 [${nodeId}] 已成功删除！`);
        fetchNodes();
      } else {
        alert(`删除失败: ${data.message || res.statusText}`);
      }
    } catch (e) {
      alert(`请求异常: ${e.message}`);
    }
  }

  async function restartNode(nodeId) {
    if (!confirm(`确定要远程重启节点 [${nodeId}] 吗？`)) return;
    try {
      const res = await fetch(`/v1/operator/devices/${nodeId}/restart`, {
        method: 'POST',
        headers: authHeaders(),
        credentials: 'same-origin'
      });
      const data = await res.json();
      if (res.ok) {
        alert(`已下发重启命令 (Task ID: ${data.task_id})`);
        fetchTasks();
      } else {
        alert(`重启失败: ${data.message || res.statusText}`);
      }
    } catch (e) {
      alert(`请求异常: ${e.message}`);
    }
  }

  async function upgradeNode(nodeId) {
    if (!confirm(`确定让节点 [${nodeId}] 从网关自动下载最新可执行程序并升级重启吗？`)) return;
    try {
      const res = await fetch(`/v1/operator/devices/${nodeId}/upgrade`, {
        method: 'POST',
        headers: authHeaders(),
        credentials: 'same-origin'
      });
      const data = await res.json();
      if (res.ok) {
        alert(`已下发自动更新命令 (Task ID: ${data.task_id})`);
        fetchTasks();
      } else {
        alert(`更新失败: ${data.message || res.statusText}`);
      }
    } catch (e) {
      alert(`请求异常: ${e.message}`);
    }
  }

  // 4. Modals & Actions
  function showTaskDetail(taskId) {
    const t = tasks.find(x => x.id === taskId);
    if (!t) return;
    currentTask = t;

    document.getElementById('modal-task-title').textContent = `任务详情: ${t.id}`;
    document.getElementById('modal-task-id').textContent = t.id;
    document.getElementById('modal-task-node').textContent = t.node_id || '-';
    document.getElementById('modal-task-attempt').textContent = t.attempt_id || '-';
    document.getElementById('modal-task-lease').textContent = t.lease_expires_at ? new Date(t.lease_expires_at).toLocaleTimeString() : '无';

    const statusBadge = document.getElementById('modal-task-status');
    statusBadge.textContent = t.state;
    statusBadge.className = `badge badge-${t.state}`;

    let inputStr = '';
    try {
      inputStr = typeof t.input === 'string' ? JSON.stringify(JSON.parse(t.input), null, 2) : JSON.stringify(t.input, null, 2);
    } catch {
      inputStr = String(t.input);
    }
    document.getElementById('modal-task-input').textContent = inputStr || '-';

    const outputEl = document.getElementById('modal-task-output');
    if (t.result && t.result.output) {
      outputEl.textContent = t.result.output;
    } else {
      outputEl.textContent = t.state === 'running' ? '正在执行中，等待输出流...\n' : '暂无输出日志\n';
    }

    taskModal.classList.remove('hidden');
  }

  function openReconcileModal(taskId) {
    document.getElementById('reconcile-task-id').textContent = taskId;
    reconcileModal.classList.remove('hidden');
  }

  document.querySelectorAll('.modal-close-btn, .modal-backdrop').forEach(el => {
    el.addEventListener('click', () => {
      taskModal.classList.add('hidden');
      reconcileModal.classList.add('hidden');
      currentTask = null;
    });
  });

  document.getElementById('reconcile-submit-btn').addEventListener('click', async () => {
    const taskId = document.getElementById('reconcile-task-id').textContent;
    const action = document.querySelector('input[name="reconcile-action"]:checked').value;

    try {
      let url = '';
      let body = {};
      if (action === 'requeue') {
        url = `/v1/operator/tasks/${taskId}/requeue`;
      } else {
        url = `/v1/operator/tasks/${taskId}/resolve`;
        body = { resolution: action };
      }

      const res = await fetch(url, {
        method: 'POST',
        headers: authHeaders(),
        body: JSON.stringify(body),
      });

      if (res.ok) {
        reconcileModal.classList.add('hidden');
        await fetchTasks();
      } else {
        const err = await res.text();
        alert('裁决失败: ' + err);
      }
    } catch (err) {
      alert('网络错误: ' + err);
    }
  });

  async function cancelTask(taskId) {
    if (!confirm(`确认取消任务 ${taskId} 吗？`)) return;
    try {
      const res = await fetch(`/v1/operator/tasks/${taskId}/cancel`, {
        method: 'POST',
        headers: authHeaders()
      });
      if (res.ok) {
        await fetchTasks();
      } else {
        const err = await res.text();
        alert('取消失败: ' + err);
      }
    } catch (err) {
      alert('请求错误: ' + err);
    }
  }

  async function revokeNode(nodeId) {
    if (!confirm(`确定要撤销节点 ${nodeId} 的访问凭证吗？此操作立即生效。`)) return;
    try {
      const res = await fetch(`/v1/operator/devices/${nodeId}/revoke`, {
        method: 'POST',
        headers: authHeaders()
      });
      if (res.ok) {
        await fetchNodes();
      } else {
        const err = await res.text();
        alert('撤销失败: ' + err);
      }
    } catch (err) {
      alert('网络错误: ' + err);
    }
  }

  // 5. Submit Task Form
  submitForm.addEventListener('submit', async (e) => {
    e.preventDefault();
    submitMessage.className = 'alert hidden';

    const nodeId = document.getElementById('submit-node-id').value.trim();
    const capability = document.getElementById('submit-capability').value.trim();
    const rawInstruction = document.getElementById('submit-instruction').value.trim();
    const timeout = parseInt(document.getElementById('submit-timeout').value, 10) || 120;

    let inputData = rawInstruction;
    try {
      inputData = JSON.parse(rawInstruction);
    } catch {
      inputData = { instruction: rawInstruction };
    }

    try {
      const res = await fetch('/v1/operator/tasks/submit', {
        method: 'POST',
        headers: authHeaders(),
        body: JSON.stringify({
          node_id: nodeId,
          capability: capability,
          capability_version: 1,
          input: inputData,
          timeout_seconds: timeout
        })
      });

      if (res.ok) {
        const data = await res.json();
        submitMessage.textContent = `任务提交成功！Task ID: ${data.id}`;
        submitMessage.className = 'alert alert-success';
        submitForm.reset();
        await fetchTasks();
      } else {
        const err = await res.text();
        submitMessage.textContent = `提交失败: ${err}`;
        submitMessage.className = 'alert alert-danger';
      }
    } catch (err) {
      submitMessage.textContent = `网络错误: ${err}`;
      submitMessage.className = 'alert alert-danger';
    }
  });

  // 6. Doctor Check
  runDoctorBtn.addEventListener('click', async () => {
    doctorResults.innerHTML = '<div class="empty-state">正在进行系统环境与组件自检...</div>';
    try {
      const res = await fetch('/v1/doctor', { headers: authHeaders() });
      if (res.ok) {
        const rep = await res.json();
        renderDoctorResults(rep);
      } else {
        doctorResults.innerHTML = `<div class="empty-state text-danger">诊断接口请求失败 (${res.status})</div>`;
      }
    } catch (err) {
      doctorResults.innerHTML = `<div class="empty-state text-danger">诊断失败: ${err}</div>`;
    }
  });

  function renderDoctorResults(rep) {
    if (!rep.checks || rep.checks.length === 0) {
      doctorResults.innerHTML = '<div class="empty-state">未获得检测结果</div>';
      return;
    }

    doctorResults.innerHTML = rep.checks.map(c => {
      let statusClass = 'doctor-ok';
      let icon = '✓';
      if (c.status === 'WARN') {
        statusClass = 'doctor-warn';
        icon = '!';
      } else if (c.status === 'FAIL') {
        statusClass = 'doctor-fail';
        icon = '✗';
      }

      let remHtml = c.remediation ? `<div class="doctor-remediation">建议: ${escapeHtml(c.remediation)}</div>` : '';

      return `
        <div class="doctor-item ${statusClass}">
          <div class="doctor-icon">${icon}</div>
          <div class="doctor-info">
            <span class="doctor-name">${escapeHtml(c.name)}</span>
            <span class="doctor-msg">${escapeHtml(c.message)}</span>
            ${remHtml}
          </div>
        </div>
      `;
    }).join('');
  }

  // 7. Server-Sent Events (SSE) Stream
  let activeSSE = null;
  function connectSSE() {
    if (activeSSE) {
      try { activeSSE.close(); } catch (e) {}
      activeSSE = null;
    }

    const sseUrl = operatorToken
      ? `/v1/events/stream?token=${encodeURIComponent(operatorToken)}`
      : '/v1/events/stream';
    const sse = new EventSource(sseUrl);
    activeSSE = sse;

    sse.onopen = () => {
      statusIndicator.className = 'status-indicator online';
      statusIndicator.querySelector('.text').textContent = '● LIVE';
    };

    sse.onmessage = (event) => {
      try {
        const evt = JSON.parse(event.data);
        handleStreamEvent(evt);
      } catch (e) {
        // Ping or non-json keepalive
      }
    };

    sse.onerror = () => {
      statusIndicator.className = 'status-indicator offline';
      statusIndicator.querySelector('.text').textContent = '○ Reconnecting...';
    };
  }

  function handleStreamEvent(evt) {
    if (!evt || !evt.type) return;

    // Output chunk stream
    if (evt.type === 'task.output' || evt.type === 'stdout' || evt.type === 'stderr') {
      if (currentTask && currentTask.id === evt.task_id) {
        const outputEl = document.getElementById('modal-task-output');
        outputEl.textContent += (evt.data || '');
        outputEl.scrollTop = outputEl.scrollHeight;
      }
      return;
    }

    // Lifecycle status updates
    if (evt.task_id) {
      const existing = tasks.find(t => t.id === evt.task_id);
      if (existing) {
        if (evt.state) existing.state = evt.state;
        existing.updated_at = evt.timestamp || new Date().toISOString();
        if (currentTask && currentTask.id === evt.task_id) {
          showTaskDetail(evt.task_id);
        }
      } else {
        // New task submitted, pull tasks
        fetchTasks();
      }
      renderTasks();
      updateMetrics();
    }
  }

  // Utility
  function escapeHtml(str) {
    return String(str)
      .replace(/&/g, '&amp;')
      .replace(/</g, '&lt;')
      .replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;');
  }

  // MCP Configuration & Collaboration Docs
  function updateMCPDocs() {
    const origin = window.location.origin;
    const mcpUrl = `${origin}/mcp`;
    const token = operatorToken || localStorage.getItem('agent_gateway_token') || '';
    const fullUrl = token ? `${mcpUrl}?token=${token}` : mcpUrl;

    const elFullUrl = document.getElementById('mcp-full-url');
    const elEndpoint = document.getElementById('mcp-endpoint-url');
    const elToken = document.getElementById('mcp-token-val');
    const elDoubaoUrl = document.getElementById('mcp-doubao-url');
    const elJsonCfg = document.getElementById('mcp-json-cfg');
    const elPrompt = document.getElementById('mcp-prompt-text');

    if (elFullUrl) elFullUrl.textContent = fullUrl;
    if (elEndpoint) elEndpoint.textContent = mcpUrl;
    if (elToken) elToken.textContent = token || '(请先登录获取 Token)';
    if (elDoubaoUrl) elDoubaoUrl.textContent = fullUrl;

    const mcpJson = {
      mcpServers: {
        "agent-gateway": {
          url: fullUrl
        }
      }
    };
    if (elJsonCfg) elJsonCfg.textContent = JSON.stringify(mcpJson, null, 2);

    const promptText = `【Agent 网关集群协作接入指南】
你好！请将我们的统一 Agent 网关接入你的 AI 编程助手（豆包桌面版 / Codex / Cursor / Claude Desktop），接入后即可直接在本地调度远端集群节点执行编码与构建：

1. 豆包桌面版 & 通用代理工具接入（极简推荐）
直接复制这一个完整 URL，粘贴到客户端的【服务 URL / Server URL】即可，无需配置任何 Headers：
${fullUrl}

2. Codex / Cursor / Claude Desktop 接入
在 mcpServers 配置中添加以下配置（一行 URL 自动带鉴权）：
${JSON.stringify(mcpJson, null, 2)}

3. 网关基本信息
• 控制台看板：${origin}/ui/ (账号 admin / 密码 admin)
• 原始 MCP 端点：${mcpUrl}
• 操作员 Token：${token}

4. 接入后拥有的大模型集群调度能力
• handoff_to_computer_agent：智能选机或向指定远端机器下发任务
• wait_task_result：同步等待任务产物回传与终端输出
• list_devices：查询集群中可用的工作节点与运行环境`;

    if (elPrompt) elPrompt.textContent = promptText;
  }

  // Copy functionality
  document.addEventListener('click', (e) => {
    const btn = e.target.closest('.copy-btn');
    if (!btn) return;
    const targetId = btn.getAttribute('data-target');
    const targetEl = document.getElementById(targetId);
    if (!targetEl) return;
    const textToCopy = targetEl.value || targetEl.textContent || '';
    navigator.clipboard.writeText(textToCopy).then(() => {
      const origText = btn.textContent;
      btn.textContent = '✓ 已复制';
      setTimeout(() => { btn.textContent = origText; }, 2000);
    });
  });

  const copyPromptBtn = document.getElementById('copy-prompt-btn');
  if (copyPromptBtn) {
    copyPromptBtn.addEventListener('click', () => {
      const promptEl = document.getElementById('mcp-prompt-text');
      if (!promptEl) return;
      navigator.clipboard.writeText(promptEl.textContent).then(() => {
        const origText = copyPromptBtn.textContent;
        copyPromptBtn.textContent = '✓ 协作指南已复制到剪贴板！';
        setTimeout(() => { copyPromptBtn.textContent = origText; }, 2500);
      });
    });
  }

  // Event Listeners for Filters & Refresh
  taskStateFilter.addEventListener('change', renderTasks);
  taskSearch.addEventListener('input', renderTasks);
  refreshBtn.addEventListener('click', () => {
    fetchTasks();
    fetchNodes();
    updateMCPDocs();
    loadTLSStatus();
  });

  // 5. System Settings & TLS
  async function loadTLSStatus() {
    try {
      const res = await fetch('/api/system/tls', { headers: authHeaders(), credentials: 'same-origin' });
      if (!res.ok) return;
      const data = await res.json();
      const badge = document.getElementById('tls-type-badge');
      const details = document.getElementById('tls-cert-details');
      if (badge && details) {
        if (data.custom_enabled) {
          badge.textContent = '自定义受信证书 (Custom TLS)';
          badge.style.background = 'rgba(16, 185, 129, 0.2)';
          badge.style.color = '#34d399';
          details.innerHTML = `域名(CN): <strong>${escapeHtml(data.subject || '-')}</strong> | 颁发者: ${escapeHtml(data.issuer || '-')} | 到期时间: ${data.expires_at || '-'}`;
        } else {
          badge.textContent = '内置自签 CA 证书 (Self-Signed)';
          badge.style.background = 'rgba(56, 189, 248, 0.2)';
          badge.style.color = 'var(--primary)';
          details.textContent = '当前正在使用网关启动时自动生成的内置 CA 根证书。';
        }
      }
    } catch (e) {
      console.error('Failed to load TLS status:', e);
    }
  }

  const uploadTlsForm = document.getElementById('upload-tls-form');
  if (uploadTlsForm) {
    uploadTlsForm.addEventListener('submit', async (e) => {
      e.preventDefault();
      const certFile = document.getElementById('tls-cert-file').files[0];
      const keyFile = document.getElementById('tls-key-file').files[0];
      if (!certFile || !keyFile) return;

      const fd = new FormData();
      fd.append('cert', certFile);
      fd.append('key', keyFile);

      const msgEl = document.getElementById('upload-tls-msg');
      msgEl.className = 'alert';
      msgEl.textContent = '正在校验并上传证书...';
      msgEl.classList.remove('hidden');

      try {
        const res = await fetch('/api/system/tls/upload', {
          method: 'POST',
          headers: authHeaders(),
          body: fd,
          credentials: 'same-origin'
        });
        const data = await res.json();
        if (res.ok) {
          msgEl.className = 'alert alert-success';
          msgEl.textContent = data.message || '证书上传成功！请点击下方按钮重启网关生效。';
          loadTLSStatus();
        } else {
          msgEl.className = 'alert alert-danger';
          msgEl.textContent = '上传失败: ' + (data.message || res.statusText);
        }
      } catch (err) {
        msgEl.className = 'alert alert-danger';
        msgEl.textContent = '网络错误: ' + err.message;
      }
    });
  }

  const resetTlsBtn = document.getElementById('reset-tls-btn');
  if (resetTlsBtn) {
    resetTlsBtn.addEventListener('click', async () => {
      if (!confirm('确定要清除自定义证书并恢复为内置 CA 吗？')) return;
      try {
        const res = await fetch('/api/system/tls/reset', {
          method: 'POST',
          headers: authHeaders(),
          credentials: 'same-origin'
        });
        const data = await res.json();
        alert(data.message || '已恢复默认证书，重启网关后生效。');
        loadTLSStatus();
      } catch (e) {
        alert('重置异常: ' + e.message);
      }
    });
  }

  const restartGatewayBtn = document.getElementById('restart-gateway-btn');
  if (restartGatewayBtn) {
    restartGatewayBtn.addEventListener('click', async () => {
      if (!confirm('⚠️ 确定要立即重启 Agent Gateway 网关进程吗？\n重启期间网络会短暂中断 1~2 秒。')) return;
      const msgEl = document.getElementById('restart-gateway-msg');
      msgEl.className = 'alert alert-warning';
      msgEl.textContent = '正在重启网关服务，将在 3 秒后自动重新连接...';
      msgEl.classList.remove('hidden');

      try {
        await fetch('/api/system/restart', {
          method: 'POST',
          headers: authHeaders(),
          credentials: 'same-origin'
        });
      } catch (e) {
        // network will disconnect immediately on restart, which is expected
      }

      let count = 3;
      const interval = setInterval(() => {
        count--;
        if (count > 0) {
          msgEl.textContent = `网关重启中，倒计时 ${count} 秒后自动刷新...`;
        } else {
          clearInterval(interval);
          window.location.reload();
        }
      }, 1000);
    });
  }

  // Update quick install command with current origin
  function updateQuickInstall() {
    const origin = window.location.origin;
    const quickInstallEl = document.getElementById('node-quick-install-cmd');
    if (quickInstallEl) {
      quickInstallEl.textContent = `curl -fsSL ${origin}/download/install.sh | bash -s -- <配对邀请码>`;
    }
  }

  // Initialization
  async function init() {
    try {
      const res = await fetch('/v1/operator/devices', {
        headers: authHeaders(),
        credentials: 'same-origin'
      });
      if (res.ok) {
        nodes = await res.json();
        showDashboard();
        renderNodes();
        updateMetrics();
        await fetchTasks();
        connectSSE();
        loadTLSStatus();
        updateQuickInstall();
        return;
      }
    } catch (e) {}

    // Show login page if unauthenticated
    showLogin();
  }

  init();

})();
