/* 管理令牌只驻留内存；所有后端数据使用 textContent 渲染，避免日志/命令注入 HTML。 */
'use strict';
const $ = id => document.getElementById(id);
const state = {
  token: '', jobs: [], nodes: [], filter: 'all', view: 'overview', page: 0,
  auto: true, following: true, selected: null, loading: false, editing: false,
  samples: [], lastSample: 0, detailGeneration: 0, refreshGeneration: 0,
};
const labels = { pending: '排队中', running: '运行中', succeeded: '成功', failed: '失败', cancelled: '已取消', timeout: '超时', lost: '失联' };
const terminal = job => !['pending', 'running'].includes(job.state);
const date = value => value ? new Date(value).toLocaleString('zh-CN', { hour12: false }) : '—';
const duration = ns => {
  const seconds = Math.round(ns / 1e9);
  return seconds >= 3600 ? `${(seconds / 3600).toFixed(1)} 小时` : seconds >= 60 ? `${Math.round(seconds / 60)} 分钟` : `${seconds} 秒`;
};
function el(tag, className, text) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text !== undefined) node.textContent = text;
  return node;
}
function badge(kind, text) { return el('span', `badge ${kind}`, text || labels[kind] || kind); }
function setMessage(id, text) { $(id).textContent = text; $(id).hidden = !text; }
function notify(text) { setMessage('notice', text); }
async function request(path, options = {}) {
  const controller = new AbortController();
  const { timeoutMs = 10000, ...fetchOptions } = options;
  const timeout = setTimeout(() => controller.abort(), timeoutMs);
  try {
    const response = await fetch(path, {
      ...fetchOptions, signal: controller.signal, cache: 'no-store', credentials: 'omit',
      headers: { Authorization: `Bearer ${state.token}`, 'Content-Type': 'application/json', ...options.headers },
    });
    if (!response.ok) {
      const error = new Error((await response.text()).trim() || `HTTP ${response.status}`);
      error.status = response.status;
      throw error;
    }
    return await response.json();
  } finally { clearTimeout(timeout); }
}
function showConnection(online) {
  const node = $('connection');
  node.classList.toggle('offline', !online);
  node.replaceChildren(el('span', 'status-dot'), document.createTextNode(online ? ' 已连接' : ' 连接中断'));
}
function renderStats() {
  $('stat-running').textContent = state.jobs.filter(j => j.state === 'running').length;
  const pending = state.jobs.filter(j => j.state === 'pending').length;
  $('stat-pending').textContent = pending;
  $('nav-pending').textContent = pending;
  $('stat-completed').textContent = state.jobs.filter(j => j.state === 'succeeded').length;
  $('stat-gpus').textContent = state.nodes.filter(n => n.job_id).length;
  $('stat-gpus-total').textContent = ` / ${state.nodes.length}`;
  const external = state.nodes.filter(n => n.external).length;
  const available = state.nodes.filter(n => !n.job_id && !n.external).length;
  $('stat-gpu-caption').textContent = `${available} 张可用 · ${external} 张外部占用`;
}
function renderGPUs() {
  $('gpu-count').textContent = state.nodes.length;
  const cards = state.nodes.map(node => {
    const card = el('article', 'gpu-card');
    const head = el('div', 'gpu-card-top');
    const kind = node.job_id ? 'running' : node.external ? 'external' : 'available';
    const label = node.job_id ? `作业 #${node.job_id}` : node.external ? '外部占用' : '空闲可用';
    head.append(el('span', 'gpu-index', `DEVICE / ${String(node.index).padStart(2, '0')}`), badge(kind, label));
    const uuid = el('p', 'gpu-uuid', node.uuid); uuid.title = node.uuid;
    const metrics = el('div', 'gpu-metrics');
    metrics.append(el('span', '', '显存使用'), el('strong', '', `${node.used_mib.toLocaleString()} / ${node.total_mib ? node.total_mib.toLocaleString() : '—'} MiB`));
    const meter = el('progress', 'memory-meter');
    meter.max = node.total_mib || Math.max(node.used_mib, 1); meter.value = node.used_mib;
    meter.setAttribute('aria-label', `GPU ${node.index} 显存使用`);
    const foot = el('div', 'gpu-bottom');
    foot.append(el('span', '', `利用率 ${node.utilization === undefined ? '—' : node.utilization + '%'}`), el('span', '', node.job_id ? '内部作业分配' : node.external ? '等待外部资源释放' : '等待下一个实验'));
    card.append(head, el('h3', 'gpu-name', node.name || `NVIDIA GPU ${node.index}`), uuid, metrics, meter, foot);
    return card;
  });
  if (!cards.length) {
    const empty = el('div', 'empty-state'); empty.append(el('h3', '', '暂无 GPU 数据'), el('p', '', '设备探测失败时，作业列表与控制仍然可用。')); cards.push(empty);
  }
  $('gpu-grid').replaceChildren(...cards);
}
function renderChart(data) {
  if (data) {
    const values = state.nodes.map(n => n.utilization).filter(n => typeof n === 'number');
    const now = Date.now();
    if (values.length && now - state.lastSample >= 2000) {
      state.samples.push({ time: now, value: values.reduce((a, b) => a + b, 0) / values.length });
      if (state.samples.length > 60) state.samples.shift();
      state.lastSample = now;
    }
  }
  const latest = state.samples[state.samples.length - 1];
  $('utilization-current').textContent = latest ? `${latest.value.toFixed(0)}%` : '—';
  $('chart-caption').textContent = latest ? `${state.samples.length} 次真实采样 · 最近采样 ${new Date(latest.time).toLocaleTimeString('zh-CN')}` : '等待有效利用率采样';
  const canvas = $('utilization-chart');
  const width = canvas.getBoundingClientRect().width;
  if (!width) return;
  const ratio = window.devicePixelRatio || 1;
  canvas.width = Math.round(width * ratio); canvas.height = Math.round(90 * ratio);
  const ctx = canvas.getContext('2d');
  ctx.scale(ratio, ratio); ctx.clearRect(0, 0, width, 90);
  ctx.strokeStyle = '#e7ebde'; ctx.lineWidth = 1;
  for (const y of [12, 44, 76]) { ctx.beginPath(); ctx.moveTo(0, y); ctx.lineTo(width, y); ctx.stroke(); }
  if (!state.samples.length) return;
  const points = state.samples.map((sample, i) => [6 + (width - 12) * i / Math.max(state.samples.length - 1, 1), 76 - sample.value * .64]);
  ctx.beginPath(); points.forEach(([x, y], i) => i ? ctx.lineTo(x, y) : ctx.moveTo(x, y));
  ctx.strokeStyle = '#8b9e69'; ctx.lineWidth = 2; ctx.stroke();
  const last = points[points.length - 1]; ctx.beginPath(); ctx.arc(last[0], last[1], 3, 0, 2 * Math.PI); ctx.fillStyle = '#8b9e69'; ctx.fill();
}
function renderOwners() {
  const selected = $('owner-filter').value;
  const first = el('option', '', '所有用户'); first.value = '';
  const options = [...new Set(state.jobs.map(j => j.user))].sort().map(user => { const option = el('option', '', user); option.value = user; return option; });
  $('owner-filter').replaceChildren(first, ...options);
  $('owner-filter').value = options.some(o => o.value === selected) ? selected : '';
}
function renderJobs() {
  const search = $('search').value.trim().toLowerCase();
  const owner = $('owner-filter').value;
  let jobs = state.jobs.filter(j => (state.filter === 'all' || (state.filter === 'history' ? terminal(j) : j.state === state.filter)) && (!owner || j.user === owner) && (!search || `${j.id} ${j.user} ${j.container} ${j.cmd.join(' ')}`.toLowerCase().includes(search)));
  // 活跃队列遵循后端调度顺序，历史记录按结束时间倒序便于检查最近结果。
  jobs = [...jobs].sort((a, b) => {
    if (terminal(a) !== terminal(b)) return terminal(a) ? 1 : -1;
    return terminal(a) ? new Date(b.end_at || b.submit_at) - new Date(a.end_at || a.submit_at) : 0;
  });
  const pageSize = 12;
  const pages = Math.max(1, Math.ceil(jobs.length / pageSize));
  state.page = Math.min(state.page, pages - 1);
  const visible = jobs.slice(state.page * pageSize, (state.page + 1) * pageSize);
  const rows = visible.map(job => {
    const row = el('tr');
    const commandCell = el('td');
    commandCell.append(el('span', 'job-id', `#${String(job.id).padStart(4, '0')}`), el('span', '', job.cmd[0] || '作业'));
    const command = el('code', 'job-command', job.cmd.join(' ')); command.title = job.cmd.join(' '); commandCell.append(command);
    const ownerCell = el('td'); const ownerName = el('span', 'owner'); ownerName.append(el('span', 'owner-initial', job.user.slice(0, 1).toUpperCase()), document.createTextNode(job.user)); ownerCell.append(ownerName);
    const statusCell = el('td'); statusCell.append(badge(job.state));
    const priority = el('td', `priority-value ${job.priority > 0 ? 'priority-high' : ''}`, `${job.priority > 0 ? '+' : ''}${job.priority || 0}`);
    const gpu = el('td', '', `${job.ngpus} 张`);
    const submitted = el('td', 'job-date', date(job.submit_at));
    const actionCell = el('td'); const button = el('button', 'detail-button', '详情 ↗'); button.setAttribute('aria-label', `查看作业 ${job.id} 详情`); button.addEventListener('click', () => openJob(job.id)); actionCell.append(button);
    row.append(commandCell, ownerCell, statusCell, priority, gpu, submitted, actionCell);
    return row;
  });
  $('jobs-body').replaceChildren(...rows); $('jobs-empty').hidden = !!jobs.length;
  $('jobs-count').textContent = state.jobs.length;
  $('pagination-info').textContent = jobs.length ? `显示 ${state.page * pageSize + 1}–${state.page * pageSize + visible.length} / 共 ${jobs.length} 个作业` : '0 个作业';
  $('previous-page').disabled = state.page === 0; $('next-page').disabled = state.page >= pages - 1;
}
async function refresh() {
  if (!state.token || state.loading) return;
  state.loading = true; $('refresh').disabled = true;
  const generation = state.refreshGeneration;
  try {
    const data = await request('/api/overview');
    if (generation !== state.refreshGeneration) return;
    state.jobs = data.jobs; state.nodes = data.nodes || [];
    showConnection(true); setMessage('error-banner', ''); setMessage('gpu-error', data.gpu_error ? `GPU 探测暂不可用：${data.gpu_error}` : '');
    $('updated-at').textContent = `同步于 ${new Date(data.updated_at).toLocaleTimeString('zh-CN')}`;
    renderStats(); renderGPUs(); renderOwners(); renderJobs(); renderChart(true);
    await refreshContainers();
    if (data.gpu_error) { $('stat-gpus').textContent = '—'; $('stat-gpus-total').textContent = ' / —'; $('stat-gpu-caption').textContent = '设备探测暂不可用'; }
    if ($('job-dialog').open && state.selected !== null) await refreshDetail();
  } catch (error) {
    if (generation !== state.refreshGeneration) return;
    showConnection(false);
    if (error.status === 401) { logout(); $('login-error').textContent = error.message; }
    else setMessage('error-banner', `同步失败，显示最后一次成功采样：${error.message}`);
  } finally { state.loading = false; $('refresh').disabled = false; }
}
function field(label, value) { const wrap = el('div', 'detail-field'); wrap.append(el('dt', '', label), el('dd', '', value)); return wrap; }
function renderDetail(job) {
  $('detail-title').textContent = `作业 #${job.id}`;
  const grid = el('dl', 'detail-grid');
  grid.append(field('状态', labels[job.state] || job.state), field('提交用户', job.user), field('GPU 申请', `${job.ngpus} 张`), field('提交时间', date(job.submit_at)), field('开始时间', date(job.start_at)), field('结束时间', date(job.end_at)), field('运行时限', duration(job.time_limit_ns)), field('退出码', job.exit_code ?? '—'), field('UID', job.uid), field('工作目录', job.cwd), field('容器 ID', job.container), field('GPU UUID', (job.gpus || []).join(', ') || '尚未分配'));
  const command = el('pre', 'detail-command', JSON.stringify(job.cmd));
  const reason = el('p', 'detail-reason', job.reason || ''); reason.hidden = !job.reason;
  $('detail-content').replaceChildren(command, grid, reason);
  // 轮询不覆盖管理员正在输入的新优先级。
  if (!state.editing) $('priority-input').value = job.priority || 0;
  $('priority-input').disabled = job.state !== 'pending'; $('save-priority').disabled = job.state !== 'pending';
  $('cancel-job').disabled = terminal(job);
}
async function refreshDetail() {
  const id = state.selected; const generation = state.detailGeneration;
  if (id === null) return;
  try {
    const [job, log] = await Promise.all([request(`/api/jobs/${id}`), request(`/api/jobs/${id}/logs`)]);
    if (generation !== state.detailGeneration || !$('job-dialog').open) return;
    renderDetail(job);
    if (state.following || $('log-output').textContent === '等待日志…') {
      $('log-output').textContent = log.text || (job.state === 'pending' ? '作业正在排队，启动后将显示日志。' : '暂无日志输出。');
      $('log-note').textContent = log.truncated ? '日志较长，仅显示末尾 64 KiB' : '显示末尾 64 KiB · 每 3 秒刷新';
      if (state.following) $('log-output').scrollTop = $('log-output').scrollHeight;
    }
    setMessage('detail-error', '');
  } catch (error) { if (generation === state.detailGeneration) setMessage('detail-error', error.message); }
}
async function openJob(id) {
  state.selected = id; state.detailGeneration++; state.editing = false;
  $('detail-content').replaceChildren(); $('log-output').textContent = '等待日志…'; $('detail-title').textContent = `作业 #${id}`;
  $('priority-input').disabled = true; $('save-priority').disabled = true; $('cancel-job').disabled = true;
  setMessage('detail-error', ''); $('job-dialog').showModal(); await refreshDetail();
}
function setView(view) {
  if (!['overview', 'jobs', 'gpus'].includes(view)) view = 'overview';
  state.view = view;
  const titles = { overview: ['总览', '每一份算力，都有安排。', '从等待到完成，在这里掌握实验的每一步。'], jobs: ['作业队列', '让每个实验，有序向前。', '查看全局队列，调整优先级，追踪每次运行。'], gpus: ['GPU 资源', '算力状态，一目了然。', '查看设备分配、显存与本次会话的利用率趋势。'] };
  $('breadcrumb-view').textContent = titles[view][0]; $('view-title').textContent = titles[view][1]; $('view-description').textContent = titles[view][2];
  $('containers-section').hidden = view === 'jobs';
  $('stats-section').hidden = view !== 'overview'; $('gpu-section').hidden = view === 'jobs'; $('jobs-section').hidden = view === 'gpus';
  document.querySelectorAll('[data-view]').forEach(button => { const active = button.dataset.view === view; button.classList.toggle('active', active); if (active) button.setAttribute('aria-current', 'page'); else button.removeAttribute('aria-current'); });
  renderChart(false);
}
function logout() {
  state.token = ''; state.jobs = []; state.nodes = []; state.samples = []; state.lastSample = 0;
  state.selected = null; state.detailGeneration++; state.refreshGeneration++;
  $('containers-body').replaceChildren(); if ($('restart-dialog').open) $('restart-dialog').close();
  $('application').hidden = true; $('login').hidden = false; $('token').value = '';
  if ($('confirm-dialog').open) $('confirm-dialog').close();
  if ($('job-dialog').open) $('job-dialog').close();
  $('jobs-body').replaceChildren(); $('detail-content').replaceChildren(); $('log-output').textContent = '';
  setMessage('notice', ''); setMessage('error-banner', ''); $('token').focus();
}
$('login-form').addEventListener('submit', async event => {
  event.preventDefault(); $('login-submit').disabled = true; $('login-error').textContent = '';
  state.token = $('token').value.trim();
  try {
    await request('/api/overview');
    $('login').hidden = true; $('application').hidden = false; $('token').value = '';
    setView(location.hash.slice(1)); await refresh();
  } catch (error) { state.token = ''; $('login-error').textContent = `无法登录：${error.message}`; }
  finally { $('login-submit').disabled = false; }
});
$('logout').addEventListener('click', logout);
$('refresh').addEventListener('click', refresh);
$('auto-refresh').addEventListener('click', () => { state.auto = !state.auto; $('auto-refresh').textContent = `自动刷新 · ${state.auto ? '开' : '关'}`; $('auto-refresh').setAttribute('aria-pressed', String(state.auto)); if (state.auto) refresh(); });
document.querySelectorAll('[data-view]').forEach(button => button.addEventListener('click', () => { location.hash = button.dataset.view; setView(button.dataset.view); }));
window.addEventListener('hashchange', () => setView(location.hash.slice(1)));
document.querySelectorAll('[data-filter]').forEach(button => button.addEventListener('click', () => {
  state.filter = button.dataset.filter; state.page = 0;
  document.querySelectorAll('[data-filter]').forEach(tab => { const active = tab === button; tab.classList.toggle('active', active); tab.setAttribute('aria-pressed', String(active)); }); renderJobs();
}));
$('search').addEventListener('input', () => { state.page = 0; renderJobs(); });
$('owner-filter').addEventListener('change', () => { state.page = 0; renderJobs(); });
$('previous-page').addEventListener('click', () => { state.page = Math.max(0, state.page - 1); renderJobs(); });
$('next-page').addEventListener('click', () => { state.page++; renderJobs(); });
$('close-detail').addEventListener('click', () => $('job-dialog').close());
$('job-dialog').addEventListener('close', () => { state.selected = null; state.detailGeneration++; });
$('priority-input').addEventListener('input', () => { state.editing = true; });
$('save-priority').addEventListener('click', async () => {
  const priority = Number($('priority-input').value);
  if ($('priority-input').value === '' || !Number.isInteger(priority) || priority < -100 || priority > 100) { setMessage('detail-error', '优先级必须是 -100 到 100 之间的整数。'); return; }
  const id = state.selected; $('save-priority').disabled = true;
  try { await request(`/api/jobs/${id}/priority`, { method: 'PATCH', body: JSON.stringify({ priority }) }); state.editing = false; notify(`作业 #${id} 的优先级已更新为 ${priority}。`); await refresh(); }
  catch (error) { setMessage('detail-error', error.message); }
  finally { if (id === state.selected) $('save-priority').disabled = state.jobs.find(j => j.id === id)?.state !== 'pending'; }
});
$('follow-logs').addEventListener('click', () => { state.following = !state.following; $('follow-logs').textContent = `自动跟随 · ${state.following ? '开' : '关'}`; $('follow-logs').setAttribute('aria-pressed', String(state.following)); if (state.following) refreshDetail(); });
$('cancel-job').addEventListener('click', () => { $('confirm-description').textContent = `作业 #${state.selected} 将被取消。运行中的任务会收到终止信号，已经产生的输出会保留。`; $('confirm-dialog').showModal(); });
$('confirm-back').addEventListener('click', () => $('confirm-dialog').close());
$('confirm-cancel').addEventListener('click', async () => {
  const id = state.selected; $('confirm-cancel').disabled = true;
  try { await request(`/api/jobs/${id}`, { method: 'DELETE' }); $('confirm-dialog').close(); notify(`已发送作业 #${id} 的取消请求；运行作业将在确认退出后更新状态。`); await refresh(); }
  catch (error) { $('confirm-dialog').close(); setMessage('detail-error', error.message); }
  finally { $('confirm-cancel').disabled = false; }
});
window.addEventListener('resize', () => renderChart(false));
document.addEventListener('visibilitychange', () => { if (!document.hidden && state.auto) refresh(); });
setInterval(() => { if (!document.hidden && state.auto) refresh(); }, 3000);

// 容器操作使用完整 ID，名称只用于展示；独立错误不影响 GPU 和队列刷新。
let restartTarget = null;
const restartingContainers = new Set();
async function refreshContainers() {
  const generation = state.refreshGeneration;
  try {
    const rows = await request('/api/containers');
    if (generation !== state.refreshGeneration) return;
    $('containers-body').replaceChildren(...rows.map(container => {
      const tr = el('tr'); const name = el('td');
      name.append(el('strong', '', container.name), el('code', 'job-command', container.image));
      const status = el('td', '', container.maintenance === 'restarting' ? '重启中' : container.maintenance === 'restart_error' ? '重启失败 · 调度暂停，请重试' : container.status);
      const jobs = el('td', '', String(container.running_jobs)); const actions = el('td');
      for (const [mode, label] of [['graceful', '优雅重启'], ['force', '强制重启']]) {
        const button = el('button', mode === 'force' ? 'button small danger' : 'button small', label);
        button.disabled = container.running_jobs > 0 || container.maintenance === 'restarting' || restartingContainers.has(container.id);
        button.addEventListener('click', () => {
          restartTarget = { id: container.id, name: container.name, mode };
          $('restart-title').textContent = `${label} ${container.name}？`;
          $('restart-description').textContent = mode === 'force' ? '立即终止容器进程并重启，未保存的数据可能丢失。' : '先请求容器退出，最多等待 10 秒，仍未退出则强制终止并重启。';
          $('restart-dialog').showModal();
        }); actions.append(button, document.createTextNode(' '));
      }
      tr.append(name, status, jobs, actions); return tr;
    }));
    $('containers-empty').hidden = rows.length > 0; $('containers-empty').textContent = '暂无容器';
    setMessage('containers-error', '');
  } catch (error) { if (generation === state.refreshGeneration) setMessage('containers-error', `容器信息获取失败：${error.message}`); }
}
$('restart-back').addEventListener('click', () => $('restart-dialog').close());
$('restart-confirm').addEventListener('click', async () => {
  if (!restartTarget) return;
  const target = restartTarget; const generation = state.refreshGeneration;
  restartingContainers.add(target.id); $('restart-dialog').close();
  notify(`正在重启 ${target.name}…`);
  try {
    await request(`/api/containers/${target.id}/restart`, { method: 'POST', body: JSON.stringify({ mode: target.mode }), timeoutMs: 70000 });
    if (generation === state.refreshGeneration) notify(`容器 ${target.name} 已重启。`);
  } catch (error) {
    if (generation === state.refreshGeneration) { setMessage('notice', ''); setMessage('error-banner', `容器重启未确认：${error.message}。请刷新容器状态后再操作。`); }
  } finally { restartingContainers.delete(target.id); if (generation === state.refreshGeneration) await refreshContainers(); }
});
