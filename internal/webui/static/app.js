// Agent Gateway Web Dashboard Logic
(function() {
  'use strict';

  // State
  let tasks = [];
  let nodes = [];
  let currentTask = null;
  let activeTab = 'tab-tasks';
  const expandedToolsNodes = new Set();

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
  // Remove legacy browser-stored tokens; web authentication uses HttpOnly cookies.
  localStorage.removeItem('agent_gateway_token');
  if (new URLSearchParams(window.location.search).has('token')) {
    window.history.replaceState({}, document.title, window.location.pathname);
  }
  function authHeaders(extra = {}) {
    return { 'Content-Type': 'application/json', ...extra };
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

  let globalPollTimer = null;
  let uptimeTicker = null;

  function startBackgroundSync() {
    if (globalPollTimer) clearInterval(globalPollTimer);
    globalPollTimer = setInterval(() => {
      if (dashboardView && !dashboardView.classList.contains('hidden')) {
        fetchNodes();
        fetchTasks();
      }
    }, 4000);

    if (uptimeTicker) clearInterval(uptimeTicker);
    uptimeTicker = setInterval(() => {
      if (activeTab === 'tab-nodes') {
        renderNodes();
      }
    }, 1000);
  }

  async function updateGatewayVersion() {
    try {
      const res = await fetch('/api/public/status');
      if (res.ok) {
        const data = await res.json();
        const badge = document.getElementById('gateway-version-badge');
        if (badge && data.version) {
          badge.textContent = `🌐 网关 v${data.version}`;
          badge.title = `中心网关版本: v${data.version}\nGit Commit: ${data.git_commit || '-'}\n构建时间: ${data.build_time || '-'}`;
        }
      }
    } catch (e) {}
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
    updateGatewayVersion();
    updateMCPDocs();
    startBackgroundSync();
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
          showDashboard();
          await fetchTasks();
          await fetchNodes();
          connectSSE();
          loadTLSStatus();
          updateQuickInstall();
        } else {
          loginError.textContent = res.status === 429 ? '登录尝试过于频繁，请稍后重试。' : '账号或密码错误（初始密码见服务器 admin.password 文件）';
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
      localStorage.removeItem('agent_gateway_token');
      if (globalPollTimer) { clearInterval(globalPollTimer); globalPollTimer = null; }
      if (uptimeTicker) { clearInterval(uptimeTicker); uptimeTicker = null; }
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
        if (targetId === 'tab-submit') {
          fetchNodes();
          updateNodeSelector();
        } else if (targetId === 'tab-nodes') {
          fetchNodes();
        } else if (targetId === 'tab-tasks') {
          fetchTasks();
        }
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
        updateNodeSelector();
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

  function updateNodeSelector() {
    const select = document.getElementById('submit-node-select');
    if (!select) return;
    const currentVal = select.value;
    select.innerHTML = '<option value="">-- 从在线节点中快捷选择 --</option>';

    const validNodes = Array.isArray(nodes) ? nodes.filter(n => !n.revoked) : [];
    if (validNodes.length === 0) {
      const opt = document.createElement('option');
      opt.value = "";
      opt.textContent = "当前暂无可用节点";
      opt.disabled = true;
      select.appendChild(opt);
      return;
    }

    validNodes.forEach(n => {
      const opt = document.createElement('option');
      opt.value = n.node_id;
      const count = Array.isArray(n.agents) ? n.agents.length : 0;
      const statusText = n.online ? '🟢 在线' : '⚪ 离线';
      const hostLabel = n.hostname ? `🖥️ ${n.hostname} ` : '';
      opt.textContent = `${statusText} ${hostLabel}(${n.node_id}) [${n.os || '未知'}/${n.arch || '未知'}, ${count}个工具]`;
      select.appendChild(opt);
    });

    const nodeInput = document.getElementById('submit-node-id');
    if (currentVal && validNodes.some(x => x.node_id === currentVal)) {
      select.value = currentVal;
    } else if (nodeInput && nodeInput.value) {
      select.value = nodeInput.value;
      renderNodeAgentTools(nodeInput.value);
    } else {
      // 找到第一个在线节点并自动填入
      const onlineNode = validNodes.find(x => x.online) || validNodes[0];
      if (onlineNode) {
        select.value = onlineNode.node_id;
        if (nodeInput && !nodeInput.value) {
          nodeInput.value = onlineNode.node_id;
          renderNodeAgentTools(onlineNode.node_id);
        }
      }
    }
  }

  function renderNodeAgentTools(nodeId) {
    const container = document.getElementById('node-agent-tools-container');
    if (!container) return;
    container.innerHTML = '';
    const n = nodes.find(x => x.node_id === nodeId);
    if (!n || !Array.isArray(n.agents) || n.agents.length === 0) {
      container.innerHTML = '<span style="font-size:11px;color:var(--text-muted);">该节点尚未上报专属 Agent 工具，可使用下方通用工具</span>';
      return;
    }
    const label = document.createElement('span');
    label.style.fontSize = '12px';
    label.style.color = '#38bdf8';
    label.style.fontWeight = '600';
    label.style.marginRight = '6px';
    label.textContent = '🚀 该节点专属 Agent 工具 (点击直接选用): ';
    container.appendChild(label);

    n.agents.forEach(a => {
      const btn = document.createElement('button');
      btn.type = 'button';
      btn.className = 'btn btn-secondary btn-sm';
      btn.style.fontSize = '11px';
      btn.style.padding = '2px 8px';
      btn.style.margin = '2px 4px 2px 0';
      btn.style.border = '1px solid rgba(56,189,248,0.4)';
      btn.style.background = 'rgba(56,189,248,0.12)';
      btn.style.color = '#38bdf8';
      btn.textContent = `⚡ ${a.id}${a.version ? ' (' + a.version + ')' : ''}`;
      btn.title = a.path ? `路径: ${a.path}` : a.id;
      btn.addEventListener('click', () => {
        selectToolCapability(a.id);
      });
      container.appendChild(btn);
    });
  }

  window.selectToolCapability = function(cap) {
    const capInput = document.getElementById('submit-capability');
    const instInput = document.getElementById('submit-instruction');
    if (capInput) capInput.value = cap;
    if (instInput && (!instInput.value || instInput.value.includes('"instruction": "echo Hello') || instInput.value.includes('"prompt"'))) {
      if (cap === 'bash' || cap === 'sh' || cap === 'zsh') {
        instInput.value = JSON.stringify({ prompt: "uname -a && whoami" }, null, 2);
      } else if (cap === 'python3' || cap === 'python') {
        instInput.value = JSON.stringify({ prompt: "import sys; print(f'Python: {sys.version}')" }, null, 2);
      } else if (cap === 'docker') {
        instInput.value = JSON.stringify({ prompt: "docker ps -a" }, null, 2);
      } else if (cap === 'git') {
        instInput.value = JSON.stringify({ prompt: "git status" }, null, 2);
      } else {
        instInput.value = JSON.stringify({ prompt: `echo "Running ${cap} tool..."` }, null, 2);
      }
    }
  };

  function dispatchTaskToNode(nodeId) {
    document.querySelectorAll('.tab-btn').forEach(b => b.classList.remove('active'));
    document.querySelectorAll('.tab-pane').forEach(p => p.classList.remove('active'));
    const btn = document.querySelector('[data-tab="tab-submit"]');
    if (btn) btn.classList.add('active');
    const pane = document.getElementById('tab-submit');
    if (pane) pane.classList.add('active');

    const input = document.getElementById('submit-node-id');
    const select = document.getElementById('submit-node-select');
    if (input) input.value = nodeId;
    if (select) select.value = nodeId;
    renderNodeAgentTools(nodeId);
    if (input) input.focus();
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

  function formatUptime(startedAtSec) {
    if (!startedAtSec) return '';
    const elapsed = Math.max(0, Math.floor(Date.now() / 1000 - startedAtSec));
    if (elapsed < 60) return `运行 ${elapsed}秒`;
    if (elapsed < 3600) return `运行 ${Math.floor(elapsed / 60)}分钟`;
    return `运行 ${Math.floor(elapsed / 3600)}小时${Math.floor((elapsed % 3600) / 60)}分`;
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
      } else if (n.status === 'restarting') {
        statusBadge = '<span class="badge" style="background:rgba(245,158,11,0.25);color:#fbbf24;border:1px solid rgba(245,158,11,0.5);">🔄 重启中 (RESTARTING)</span>';
      } else if (n.status === 'upgrading') {
        statusBadge = '<span class="badge" style="background:rgba(234,179,8,0.25);color:#facc15;border:1px solid rgba(234,179,8,0.5);">🚀 更新中 (UPGRADING)</span>';
      } else if (isOnline) {
        statusBadge = '<span class="badge badge-active" style="background:rgba(16,185,129,0.2);color:#34d399;">ONLINE</span>';
      }

      const versionStr = n.version ? `<code>v${escapeHtml(n.version)}</code>` : '<span style="color:var(--text-muted)">-</span>';
      const sysStr = (n.os && n.arch) ? `<span style="font-size:11px;color:var(--text-muted);">${escapeHtml(n.os)}/${escapeHtml(n.arch)}</span>` : '';
      const uptimeStr = (isOnline && n.started_at && n.status !== 'restarting' && n.status !== 'upgrading')
        ? `<div style="font-size:11px;color:#10b981;font-weight:500;margin-top:2px;">⏱️ ${formatUptime(n.started_at)}</div>`
        : '';

      const hostTitle = n.hostname ? `🖥️ ${escapeHtml(n.hostname)}` : '🖥️ 未命名主机';
      const nodeColHtml = `
        <div>
          <div style="font-weight:600;font-size:13px;color:#f8fafc;margin-bottom:2px;">${hostTitle}</div>
          <code style="font-size:11px;color:var(--text-muted);">${escapeHtml(n.node_id)}</code>
        </div>
      `;

      let agentsHtml = '<span style="font-size:11px;color:var(--text-muted);">未上报</span>';
      if (Array.isArray(n.agents) && n.agents.length > 0) {
        const total = n.agents.length;
        const isExpanded = expandedToolsNodes.has(n.node_id);
        const displayLimit = 3;
        const visible = isExpanded ? n.agents : n.agents.slice(0, displayLimit);

        const tags = visible.map(a => {
          return `<span class="badge agent-tool-tag" data-node="${escapeHtml(n.node_id)}" style="font-size:10px;margin:2px;cursor:pointer;background:rgba(56,189,248,0.15);color:var(--primary);border:1px solid rgba(56,189,248,0.3);" title="点击查看工具物理绝对路径与详情">⚡ ${escapeHtml(a.id)}</span>`;
        }).join('');

        let controlBtns = '';
        if (total > displayLimit) {
          if (isExpanded) {
            controlBtns = `
              <button type="button" class="btn btn-secondary btn-sm toggle-tools-btn" data-node="${escapeHtml(n.node_id)}" style="font-size:10px;padding:2px 8px;margin:2px;background:rgba(250,204,21,0.12);border:1px solid rgba(250,204,21,0.3);color:#facc15;" title="收起剩余工具标签">🔼 收起</button>
              <button type="button" class="btn btn-secondary btn-sm view-tools-modal-btn" data-node="${escapeHtml(n.node_id)}" style="font-size:10px;padding:2px 8px;margin:2px;background:rgba(56,189,248,0.12);border:1px solid rgba(56,189,248,0.4);color:#38bdf8;" title="弹窗查看所有工具绝对物理路径">📋 探测路径清单</button>
            `;
          } else {
            controlBtns = `
              <button type="button" class="btn btn-secondary btn-sm toggle-tools-btn" data-node="${escapeHtml(n.node_id)}" style="font-size:10px;padding:2px 8px;margin:2px;background:rgba(56,189,248,0.15);border:1px solid rgba(56,189,248,0.4);color:#38bdf8;font-weight:600;" title="点击就地展开该节点全部 ${total} 个工具名称">+${total - displayLimit} 全部 (${total}个)</button>
              <button type="button" class="btn btn-secondary btn-sm view-tools-modal-btn" data-node="${escapeHtml(n.node_id)}" style="font-size:10px;padding:2px 6px;margin:2px;background:rgba(255,255,255,0.06);border:1px solid rgba(255,255,255,0.15);color:var(--text-muted);" title="弹窗查看所有工具绝对物理路径">📋 路径清单</button>
            `;
          }
        } else {
          controlBtns = `<button type="button" class="btn btn-secondary btn-sm view-tools-modal-btn" data-node="${escapeHtml(n.node_id)}" style="font-size:10px;padding:2px 6px;margin:2px;background:rgba(255,255,255,0.06);border:1px solid rgba(255,255,255,0.15);color:var(--text-muted);" title="查看工具详情">📋 详情</button>`;
        }
        agentsHtml = `<div style="display:flex;flex-wrap:wrap;align-items:center;">${tags}${controlBtns}</div>`;
      }

      const expiresAt = n.cert_expires_at ? new Date(n.cert_expires_at).toLocaleDateString() : '-';

      let actions = '';
      if (!isRevoked && isOnline) {
        actions += `<button class="btn btn-primary btn-sm dispatch-task-btn" data-id="${n.node_id}" style="margin-right:4px;background:#0284c7;">⚡ 调试派工</button>`;
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
          <td>${nodeColHtml}</td>
          <td>${statusBadge}</td>
          <td><div>${versionStr}</div><div>${sysStr}</div>${uptimeStr}</td>
          <td style="max-width:280px;">${agentsHtml}</td>
          <td>${expiresAt}</td>
          <td>${actions}</td>
        </tr>
      `;
    }).join('');

    // 使用事件委托确保频繁重绘下事件永久生效
    if (nodesTableBody && !nodesTableBody._hasBoundEvents) {
      nodesTableBody._hasBoundEvents = true;
      nodesTableBody.addEventListener('click', (e) => {
        const toggleBtn = e.target.closest('.toggle-tools-btn');
        if (toggleBtn) {
          const nid = toggleBtn.getAttribute('data-node');
          if (expandedToolsNodes.has(nid)) {
            expandedToolsNodes.delete(nid);
          } else {
            expandedToolsNodes.add(nid);
          }
          renderNodes();
          return;
        }

        const modalBtn = e.target.closest('.view-tools-modal-btn, .agent-tool-tag, .show-more-tools-btn');
        if (modalBtn) {
          const nid = modalBtn.getAttribute('data-node');
          if (nid) openAgentToolsModal(nid);
          return;
        }

        const dispatchBtn = e.target.closest('.dispatch-task-btn');
        if (dispatchBtn) {
          dispatchTaskToNode(dispatchBtn.getAttribute('data-id'));
          return;
        }

        const restartBtn = e.target.closest('.restart-node-btn');
        if (restartBtn) {
          restartNode(restartBtn.getAttribute('data-id'));
          return;
        }

        const upgradeBtn = e.target.closest('.upgrade-node-btn');
        if (upgradeBtn) {
          upgradeNode(upgradeBtn.getAttribute('data-id'));
          return;
        }

        const revokeBtn = e.target.closest('.revoke-node-btn');
        if (revokeBtn) {
          revokeNode(revokeBtn.getAttribute('data-id'));
          return;
        }

        const deleteBtn = e.target.closest('.delete-node-btn');
        if (deleteBtn) {
          deleteNode(deleteBtn.getAttribute('data-id'));
          return;
        }
      });
    }
  }

  function openAgentToolsModal(nodeId) {
    const n = nodes.find(x => x.node_id === nodeId);
    if (!n) {
      console.warn('Node not found:', nodeId);
      return;
    }
    const modal = document.getElementById('agent-tools-modal');
    const titleEl = document.getElementById('agent-tools-modal-title');
    const subtitleEl = document.getElementById('agent-tools-modal-subtitle');
    const tbody = document.getElementById('agent-tools-modal-tbody');
    if (!modal || !tbody) return;

    const hostname = n.hostname || n.node_id;
    titleEl.textContent = `🖥️ ${hostname} 工具链清单 (共 ${n.agents ? n.agents.length : 0} 个)`;
    subtitleEl.textContent = `该节点通过本地系统 PATH (LookPath) 及应用目录真实探测到的可用工具及二进制安装路径：`;

    if (!Array.isArray(n.agents) || n.agents.length === 0) {
      tbody.innerHTML = '<tr><td colspan="5" class="empty-state">该节点尚未探测到任何已安装的 Agent 工具</td></tr>';
    } else {
      tbody.innerHTML = n.agents.map(a => {
        const pathStr = a.path
          ? `<code style="font-size:11px;color:#38bdf8;word-break:break-all;">${escapeHtml(a.path)}</code>`
          : '<span style="font-size:11px;color:var(--text-muted);">(系统 PATH 自动发现)</span>';
        const kindBadge = a.kind === 'gui'
          ? '<span class="badge" style="background:rgba(168,85,247,0.2);color:#c084fc;font-size:10px;">GUI 应用</span>'
          : '<span class="badge" style="background:rgba(56,189,248,0.2);color:#38bdf8;font-size:10px;">CLI 工具</span>';
        return `
          <tr>
            <td><strong>⚡ ${escapeHtml(a.id)}</strong></td>
            <td>${escapeHtml(a.name || a.id)}</td>
            <td>${kindBadge}</td>
            <td>${pathStr}</td>
            <td><button class="btn btn-primary btn-sm modal-dispatch-tool-btn" data-node="${escapeHtml(n.node_id)}" data-cap="${escapeHtml(a.id)}" style="font-size:10px;padding:2px 8px;">调试</button></td>
          </tr>
        `;
      }).join('');

      tbody.querySelectorAll('.modal-dispatch-tool-btn').forEach(btn => {
        btn.addEventListener('click', () => {
          modal.classList.add('hidden');
          modal.style.display = 'none';
          dispatchTaskToNode(btn.getAttribute('data-node'));
          selectToolCapability(btn.getAttribute('data-cap'));
        });
      });
    }

    modal.classList.remove('hidden');
    modal.style.display = 'flex';
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
    const targetNode = nodes.find(n => n.node_id === nodeId);
    if (targetNode) {
      targetNode.status = 'restarting';
      renderNodes();
    }
    try {
      const res = await fetch(`/v1/operator/devices/${nodeId}/restart`, {
        method: 'POST',
        headers: authHeaders(),
        credentials: 'same-origin'
      });
      const data = await res.json();
      if (res.ok && data.task_id) {
        await fetchTasks();
        showTaskDetail(data.task_id);
        setTimeout(fetchNodes, 2000);
      } else {
        alert(`重启下发失败: ${data.message || res.statusText}`);
        fetchNodes();
      }
    } catch (e) {
      alert(`请求异常: ${e.message}`);
      fetchNodes();
    }
  }

  async function upgradeNode(nodeId) {
    if (!confirm(`确定让节点 [${nodeId}] 从网关自动下载最新可执行程序并升级重启吗？`)) return;
    const targetNode = nodes.find(n => n.node_id === nodeId);
    if (targetNode) {
      targetNode.status = 'upgrading';
      renderNodes();
    }
    try {
      const res = await fetch(`/v1/operator/devices/${nodeId}/upgrade`, {
        method: 'POST',
        headers: authHeaders(),
        credentials: 'same-origin'
      });
      const data = await res.json();
      if (res.ok && data.task_id) {
        await fetchTasks();
        showTaskDetail(data.task_id);
        setTimeout(fetchNodes, 2500);
      } else {
        alert(`更新下发失败: ${data.message || res.statusText}`);
        fetchNodes();
      }
    } catch (e) {
      alert(`请求异常: ${e.message}`);
      fetchNodes();
    }
  }

  // 4. Modals & Actions
  let detailPollTimer = null;

  async function showTaskDetail(taskId) {
    if (detailPollTimer) {
      clearInterval(detailPollTimer);
      detailPollTimer = null;
    }

    let t = tasks.find(x => x.id === taskId);
    currentTask = t || { id: taskId, state: 'loading' };

    renderTaskModalData(currentTask);
    taskModal.classList.remove('hidden');

    // 立即向后端拉取最新的单任务精确状态与完整输出
    await fetchSingleTaskDetail(taskId);

    // 如果任务仍处于排队或执行中，开启定时轮询直到执行完成
    if (currentTask && (currentTask.state === 'running' || currentTask.state === 'leased' || currentTask.state === 'queued')) {
      detailPollTimer = setInterval(async () => {
        if (!currentTask || currentTask.id !== taskId || taskModal.classList.contains('hidden')) {
          clearInterval(detailPollTimer);
          detailPollTimer = null;
          return;
        }
        await fetchSingleTaskDetail(taskId);
        if (currentTask && currentTask.state !== 'running' && currentTask.state !== 'leased' && currentTask.state !== 'queued') {
          clearInterval(detailPollTimer);
          detailPollTimer = null;
          fetchTasks();
        }
      }, 1000);
    }
  }

  async function fetchSingleTaskDetail(taskId) {
    try {
      const res = await fetch(`/v1/operator/tasks/${taskId}`, {
        headers: authHeaders(),
        credentials: 'same-origin'
      });
      if (res.ok) {
        const data = await res.json();
        currentTask = data;
        const idx = tasks.findIndex(x => x.id === taskId);
        if (idx !== -1) {
          tasks[idx] = data;
          renderTasks();
          updateMetrics();
        }
        renderTaskModalData(data);
      }
    } catch (e) {
      console.error('Fetch task detail failed:', e);
    }
  }

  function renderTaskModalData(t) {
    if (!t) return;
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
      inputStr = String(t.input || '');
    }
    document.getElementById('modal-task-input').textContent = inputStr || '-';

    const outputEl = document.getElementById('modal-task-output');
    if (t.result) {
      let content = t.result.text || t.result.output || '';
      let meta = [];
      if (typeof t.result.exit_code === 'number') {
        meta.push(`[进程退出码: ${t.result.exit_code}]`);
      }
      if (t.result.error_code) {
        meta.push(`[错误码: ${t.result.error_code}]`);
      }
      if (t.result.truncated) {
        meta.push(`[输出过长已截断]`);
      }
      if (meta.length > 0) {
        outputEl.textContent = meta.join(' ') + '\n\n' + (content || '(无标准输出)');
      } else {
        outputEl.textContent = content || '(无标准输出)';
      }
    } else {
      outputEl.textContent = (t.state === 'running' || t.state === 'leased') ? '⏳ 正在执行中，等待节点回报输出...\n' : '暂无输出日志\n';
    }
  }

  function openReconcileModal(taskId) {
    document.getElementById('reconcile-task-id').textContent = taskId;
    reconcileModal.classList.remove('hidden');
  }

  const agentToolsModal = document.getElementById('agent-tools-modal');

  function closeAllModals() {
    if (taskModal) { taskModal.classList.add('hidden'); taskModal.style.display = 'none'; }
    if (reconcileModal) { reconcileModal.classList.add('hidden'); reconcileModal.style.display = 'none'; }
    if (agentToolsModal) { agentToolsModal.classList.add('hidden'); agentToolsModal.style.display = 'none'; }
    if (detailPollTimer) {
      clearInterval(detailPollTimer);
      detailPollTimer = null;
    }
    currentTask = null;
  }

  document.body.addEventListener('click', (e) => {
    if (e.target.closest('.modal-close-btn') || e.target.classList.contains('modal-backdrop')) {
      closeAllModals();
    }
  });

  window.addEventListener('keydown', (e) => {
    if (e.key === 'Escape') {
      closeAllModals();
    }
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

  // 5. Submit Task Form & Node Selector Linkage
  const submitNodeSelect = document.getElementById('submit-node-select');
  if (submitNodeSelect) {
    submitNodeSelect.addEventListener('change', (e) => {
      const val = e.target.value;
      if (val) {
        document.getElementById('submit-node-id').value = val;
        renderNodeAgentTools(val);
      }
    });
  }
  const submitNodeInput = document.getElementById('submit-node-id');
  if (submitNodeInput) {
    submitNodeInput.addEventListener('input', (e) => {
      const val = e.target.value.trim();
      renderNodeAgentTools(val);
    });
  }

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
      inputData = { instruction: rawInstruction, prompt: rawInstruction };
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
        submitMessage.textContent = `任务提交成功！Task ID: ${data.id}，已自动打开实时控制台追踪输出。`;
        submitMessage.className = 'alert alert-success';
        await fetchTasks();
        showTaskDetail(data.id);
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

    const sseUrl = '/v1/events/stream';
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

    // Node & Device lifecycle updates
    if (evt.type === 'node.heartbeat' || evt.type === 'device.registered' || evt.type === 'node.registered') {
      fetchNodes();
      return;
    }

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
    const fullUrl = mcpUrl;

    const elFullUrl = document.getElementById('mcp-full-url');
    const elEndpoint = document.getElementById('mcp-endpoint-url');
    const elToken = document.getElementById('mcp-token-val');
    const elDoubaoUrl = document.getElementById('mcp-doubao-url');
    const elJsonCfg = document.getElementById('mcp-json-cfg');
    const elPrompt = document.getElementById('mcp-prompt-text');

    if (elFullUrl) elFullUrl.textContent = fullUrl;
    if (elEndpoint) elEndpoint.textContent = mcpUrl;
    if (elToken) elToken.textContent = '使用 mesh credential issue --out 签发独立凭据';
    if (elDoubaoUrl) elDoubaoUrl.textContent = fullUrl;

    const mcpJson = {
      mcpServers: {
        "agent-gateway": {
          url: fullUrl,
          headers: { Authorization: 'Bearer <独立签发的操作员令牌>' }
        }
      }
    };
    if (elJsonCfg) elJsonCfg.textContent = JSON.stringify(mcpJson, null, 2);

    const promptText = `【Agent 网关集群协作接入指南】
你好！请将我们的统一 Agent 网关接入你的 AI 编程助手（豆包桌面版 / Codex / Cursor / Claude Desktop），接入后即可直接在本地调度远端集群节点执行编码与构建：

1. 豆包桌面版 & 通用代理工具接入（极简推荐）
将 URL 填入客户端，并单独配置 Authorization: Bearer 请求头：
${fullUrl}

2. Codex / Cursor / Claude Desktop 接入
在 mcpServers 配置中添加以下配置（将占位符替换为独立签发的短期凭据）：
${JSON.stringify(mcpJson, null, 2)}

3. 接入后拥有的大模型集群调度能力
• handoff_to_computer_agent：智能选机或向指定远端机器下发任务
• wait_task_result：同步等待任务产物回传与终端输出
• list_devices：查询集群中可用的工作节点与运行环境`;

    if (elPrompt) elPrompt.textContent = promptText;
  }


  let credentialCursor = '';
  async function loadCredentials(append = false) {
    const tbody = document.getElementById('credentials-body');
    const message = document.getElementById('credentials-message');
    try {
      const res = await fetch('/v1/operator/credentials' + (append && credentialCursor ? '?after=' + encodeURIComponent(credentialCursor) : ''), { headers: authHeaders() });
      if (res.status === 401) { showLogin('会话已过期或被撤销，请重新登录。'); return; }
      if (!res.ok) throw new Error(res.status === 403 ? '只有管理员可以管理凭据。' : '无法加载凭据');
      const data = await res.json();
      if (!append) tbody.replaceChildren();
      for (const item of data.credentials) {
        const row = document.createElement('tr');
        const active = !item.revoked_at && Date.parse(item.expires_at) > Date.now();
        const values = [item.id + (item.id === data.current_principal_id ? '（当前会话）' : ''), item.role, new Date(item.expires_at).toLocaleString(), item.revoked_at ? '已撤销' : active ? '有效' : '已过期'];
        for (const value of values) { const cell = document.createElement('td'); cell.textContent = value; row.appendChild(cell); }
        const actions = document.createElement('td');
        const button = document.createElement('button');
        button.className = 'btn btn-secondary btn-sm'; button.textContent = '撤销'; button.disabled = !active;
        button.addEventListener('click', async () => {
          button.disabled = true;
          try {
            const result = await fetch('/v1/operator/credentials/' + encodeURIComponent(item.id) + '/revoke', {method:'POST',headers:authHeaders()});
            if (!result.ok) throw new Error('撤销失败，请刷新后重试。');
            if (item.id === data.current_principal_id) { if (activeSSE) activeSSE.close(); showLogin('当前会话已撤销。'); }
            else await loadCredentials();
          } catch (err) { message.textContent = err.message; button.disabled = false; }
        });
        actions.appendChild(button); row.appendChild(actions); tbody.appendChild(row);
      }
      credentialCursor = data.next_cursor;
      document.getElementById('credentials-more').hidden = !credentialCursor;
      message.textContent = '撤销后新请求立即被拒绝，已建立的事件流也会断开。';
    } catch (err) { message.textContent = err.message; }
  }
  document.getElementById('credentials-refresh').addEventListener('click', () => loadCredentials());
  document.getElementById('credentials-more').addEventListener('click', () => loadCredentials(true));
  document.querySelector('[data-tab="tab-settings"]').addEventListener('click', () => loadCredentials());

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

  // Update quick install command and pairing token management
  async function generatePairInvitation(maxUses = 1) {
    const quickInstallEl = document.getElementById('node-quick-install-cmd');
    const badgeEl = document.getElementById('invite-status-badge');
    const btn = document.getElementById('btn-generate-invite');
    if (btn) btn.disabled = true;

    try {
      const res = await fetch('/api/operator/invitations', {
        method: 'POST',
        headers: authHeaders(),
        credentials: 'same-origin',
        body: JSON.stringify({
          ttl_minutes: 120,
          max_uses: parseInt(maxUses, 10) || 1
        })
      });
      if (res.ok) {
        const data = await res.json();
        const origin = window.location.origin;
        const httpsNote = origin.indexOf('http://') === 0
          ? '\n注意：当前控制台走明文端口，请把命令里的 http 换成网关的 https 地址（默认 8443 端口）。'
          : '';
        if (quickInstallEl) {
          quickInstallEl.textContent =
            `① 下载 CA 证书到目标机器当前目录（保存为 ca.crt）\n` +
            `② 在目标机器执行：\n` +
            `curl --cacert ./ca.crt -fsSL ${origin}/download/install.sh | MESH_TOKEN='${data.token}' bash -s -- ${origin}` +
            httpsNote;
        }
        const quickInstallPs1 = document.getElementById('node-quick-install-cmd-ps1');
        if (quickInstallPs1) {
          quickInstallPs1.textContent =
            `① 下载 CA 证书到目标机器当前目录（保存为 ca.crt）\n` +
            `② 在目标机器执行：\n` +
            `curl.exe --cacert .\\ca.crt -fsSL ${origin}/download/install.ps1 -o install.ps1\n` +
            `$env:MESH_TOKEN='${data.token}'; ./install.ps1 -Server ${origin} -Ca .\\ca.crt` +
            httpsNote;
        }
        if (badgeEl) {
          const expTime = new Date(data.expires_at).toLocaleTimeString();
          let typeDesc = '🔒 单台专用 (限1次)';
          if (data.max_uses > 1) {
            typeDesc = `🌐 一码多用 (限 ${data.max_uses} 台)`;
          } else if (data.max_uses === -1) {
            typeDesc = `⚡ 一码多用 (不限台数)`;
          }
          badgeEl.textContent = `${typeDesc} | 有效期至 ${expTime}`;
          badgeEl.style.display = 'inline-block';
        }
      } else {
        const err = await res.text();
        console.error('Failed to generate invitation:', err);
      }
    } catch (e) {
      console.error('Network error generating invitation:', e);
    } finally {
      if (btn) btn.disabled = false;
    }
  }

  function setupInvitationControls() {
    const btn = document.getElementById('btn-generate-invite');
    const select = document.getElementById('invite-mode-select');
    if (btn && select) {
      btn.addEventListener('click', () => {
        generatePairInvitation(select.value);
      });
    }

    const tabBash = document.getElementById('tab-install-bash');
    const tabPs1 = document.getElementById('tab-install-ps1');
    const boxBash = document.getElementById('box-install-bash');
    const boxPs1 = document.getElementById('box-install-ps1');
    const tipBash = document.getElementById('tip-install-bash');
    const tipPs1 = document.getElementById('tip-install-ps1');

    if (tabBash && tabPs1) {
      tabBash.addEventListener('click', () => {
        tabBash.className = 'btn btn-primary btn-sm';
        tabPs1.className = 'btn btn-secondary btn-sm';
        if (boxBash) boxBash.style.display = 'flex';
        if (tipBash) tipBash.style.display = 'block';
        if (boxPs1) boxPs1.style.display = 'none';
        if (tipPs1) tipPs1.style.display = 'none';
      });

      tabPs1.addEventListener('click', () => {
        tabPs1.className = 'btn btn-primary btn-sm';
        tabBash.className = 'btn btn-secondary btn-sm';
        if (boxPs1) boxPs1.style.display = 'flex';
        if (tipPs1) tipPs1.style.display = 'block';
        if (boxBash) boxBash.style.display = 'none';
        if (tipBash) tipBash.style.display = 'none';
      });
    }
  }

  function updateQuickInstall() {
    setupInvitationControls();
    generatePairInvitation(1);
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
        updateNodeSelector();
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
