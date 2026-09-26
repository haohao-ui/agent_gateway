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

  // Modals
  const taskModal = document.getElementById('task-modal');
  const reconcileModal = document.getElementById('reconcile-modal');
  const submitForm = document.getElementById('submit-task-form');
  const submitMessage = document.getElementById('submit-message');
  const runDoctorBtn = document.getElementById('run-doctor-btn');
  const doctorResults = document.getElementById('doctor-results');

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
      const res = await fetch('/v1/operator/tasks');
      if (res.ok) {
        tasks = await res.json();
        renderTasks();
        updateMetrics();
      } else {
        console.error('Failed to load tasks:', res.statusText);
      }
    } catch (err) {
      console.error('Error fetching tasks:', err);
    }
  }

  async function fetchNodes() {
    try {
      const res = await fetch('/v1/operator/devices');
      if (res.ok) {
        nodes = await res.json();
        renderNodes();
        updateMetrics();
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
      const statusBadge = isRevoked ? '<span class="badge badge-revoked">REVOKED</span>' : '<span class="badge badge-active">ACTIVE</span>';
      const fp = n.fingerprint ? n.fingerprint.slice(0, 16) + '...' : '-';
      const expiresAt = n.cert_expires_at ? new Date(n.cert_expires_at).toLocaleDateString() : '-';
      const registeredAt = n.registered_at ? new Date(n.registered_at).toLocaleString() : '-';

      let revokeBtn = isRevoked
        ? `<button class="btn btn-secondary btn-sm" disabled>已撤销</button>`
        : `<button class="btn btn-danger btn-sm revoke-node-btn" data-id="${n.node_id}">撤销凭证</button>`;

      return `
        <tr>
          <td><strong>${escapeHtml(n.node_id)}</strong></td>
          <td><code>${escapeHtml(fp)}</code></td>
          <td>${expiresAt}</td>
          <td>${statusBadge}</td>
          <td>${registeredAt}</td>
          <td>${revokeBtn}</td>
        </tr>
      `;
    }).join('');

    nodesTableBody.querySelectorAll('.revoke-node-btn').forEach(btn => {
      btn.addEventListener('click', () => revokeNode(btn.getAttribute('data-id')));
    });
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
        headers: { 'Content-Type': 'application/json' },
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
      const res = await fetch(`/v1/tasks/${taskId}/cancel`, { method: 'POST' });
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
      const res = await fetch(`/v1/operator/devices/${nodeId}/revoke`, { method: 'POST' });
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
      const res = await fetch('/v1/tasks/submit', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
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
      const res = await fetch('/v1/doctor');
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
  function connectSSE() {
    const sse = new EventSource('/v1/events/stream');

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

  // Event Listeners for Filters & Refresh
  taskStateFilter.addEventListener('change', renderTasks);
  taskSearch.addEventListener('input', renderTasks);
  refreshBtn.addEventListener('click', () => {
    fetchTasks();
    fetchNodes();
  });

  // Initialization
  fetchTasks();
  fetchNodes();
  connectSSE();

})();
