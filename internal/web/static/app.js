// gopm2 Web 面板前端（原生 JS，无构建）。
"use strict";

const $ = (id) => document.getElementById(id);
let currentApp = null;
let followES = null;      // 日志 SSE
let eventsES = null;      // 事件 SSE
let refreshTimer = null;

// ---------- 工具 ----------
async function api(method, path, body) {
  const opts = { method, headers: {} };
  if (body !== undefined) {
    opts.headers["Content-Type"] = "application/json";
    opts.body = JSON.stringify(body);
  }
  const resp = await fetch(path, opts);
  if (resp.status === 401) { showLogin(); throw new Error("unauthorized"); }
  const data = await resp.json().catch(() => ({}));
  if (!resp.ok) throw new Error(data.error || resp.statusText);
  return data;
}

function fmtBytes(n) {
  if (!n || n <= 0) return "0";
  const u = ["B", "KB", "MB", "GB"];
  let f = n, i = 0;
  while (f >= 1024 && i < u.length - 1) { f /= 1024; i++; }
  return f.toFixed(1) + u[i];
}

function fmtUptime(iso) {
  if (!iso || iso.startsWith("0001-")) return "-";
  const ms = Date.now() - new Date(iso).getTime();
  if (ms < 0) return "0s";
  const s = Math.floor(ms / 1000);
  if (s < 60) return s + "s";
  const m = Math.floor(s / 60);
  if (m < 60) return m + "m" + (s % 60) + "s";
  const h = Math.floor(m / 60);
  if (h < 24) return h + "h" + (m % 60) + "m";
  return Math.floor(h / 24) + "d" + (h % 24) + "h";
}

function esc(s) {
  return String(s ?? "").replace(/[&<>"']/g, (c) => ({
    "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;",
  }[c]));
}

// ---------- 登录 ----------
function showLogin() {
  $("login-view").classList.remove("hidden");
  $("app-view").classList.add("hidden");
  stopEventStream();
}

async function showApp() {
  $("login-view").classList.add("hidden");
  $("app-view").classList.remove("hidden");
  startEventStream();
  await refresh();
}

$("login-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  $("login-error").textContent = "";
  const btn = $("login-btn");
  btn.disabled = true; btn.textContent = "登录中…";
  try {
    await api("POST", "/api/login", { password: $("login-password").value });
    $("login-password").value = ""; // 登录成功后清空密码框，避免残留
    await showApp();
  } catch (err) {
    $("login-error").textContent = err.message;
  } finally {
    btn.disabled = false; btn.textContent = "登录";
  }
});

$("btn-logout").addEventListener("click", async () => {
  try { await api("POST", "/api/logout", {}); } catch {}
  showLogin();
});

// ---------- 列表 ----------
async function refresh() {
  let apps;
  try {
    apps = await api("GET", "/api/apps");
    $("conn-dot").classList.add("on");
  } catch (err) {
    if (err.message !== "unauthorized") $("conn-dot").classList.remove("on");
    return;
  }
  const tbody = $("apps-tbody");
  tbody.innerHTML = "";
  $("empty-hint").classList.toggle("hidden", apps.length > 0);
  for (const a of apps) {
    const tr = document.createElement("tr");
    tr.dataset.name = a.spec.name;
    tr.innerHTML = `
      <td><b>${esc(a.spec.name)}</b></td>
      <td><span class="state ${esc(a.status.state)}">${esc(a.status.state)}</span></td>
      <td class="num">${a.status.pid || "-"}</td>
      <td class="num">${a.status.cpu_percent?.toFixed(1) ?? "0.0"}</td>
      <td class="num">${fmtBytes(a.status.rss_bytes)}</td>
      <td class="num">${a.status.restarts}</td>
      <td class="num">${fmtUptime(a.status.started_at)}</td>
      <td class="row-actions"></td>`;
    const act = tr.querySelector(".row-actions");
    const mkBtn = (label, cls, fn) => {
      const b = document.createElement("button");
      b.textContent = label; b.className = cls;
      b.addEventListener("click", (e) => { e.stopPropagation(); fn(); });
      return b;
    };
    if (a.status.state === "online") {
      act.append(
        mkBtn("停止", "warn", () => op("stop", a.spec.name)),
        mkBtn("重启", "ghost", () => op("restart", a.spec.name)));
    } else {
      act.append(mkBtn("启动", "ok", () => op("start", a.spec.name)));
    }
    act.append(mkBtn("删除", "danger", () => {
      if (confirm(`删除 ${a.spec.name}？`)) op("delete", a.spec.name);
    }));
    tr.addEventListener("click", () => openDetail(a.spec.name));
    tbody.appendChild(tr);
  }
}

async function op(action, name) {
  try {
    if (action === "delete") await api("DELETE", `/api/apps/${encodeURIComponent(name)}`);
    else await api("POST", `/api/apps/${encodeURIComponent(name)}/${action}`);
    await refresh();
    if (currentApp === name) {
      if (action === "delete") closeDetail();       // 删除后关闭抽屉，避免 404
      else await openDetail(name);                   // 其他操作刷新详情
    }
  } catch (err) { alert(`${action} ${name}: ${err.message}`); }
}

// closeDetail 关闭详情抽屉并停止日志跟随。
function closeDetail() {
  $("detail").classList.add("hidden");
  currentApp = null;
  stopFollow();
}

$("btn-save").addEventListener("click", async () => {
  try { const r = await api("POST", "/api/save"); alert(`已保存 ${r.saved} 个应用快照`); }
  catch (err) { alert("save: " + err.message); }
});
$("btn-resurrect").addEventListener("click", async () => {
  try {
    const r = await api("POST", "/api/resurrect");
    alert(`恢复: ${(r.started || []).join(", ") || "无"}` + (r.errors?.length ? `\n错误: ${r.errors.join("; ")}` : ""));
    await refresh();
  } catch (err) { alert("resurrect: " + err.message); }
});

// ---------- 详情 ----------
async function openDetail(name) {
  currentApp = name;
  $("detail").classList.remove("hidden");
  let a;
  try { a = await api("GET", `/api/apps/${encodeURIComponent(name)}`); }
  catch (err) { alert(err.message); return; }

  $("d-name").textContent = a.spec.name;
  const cmd = a.spec.args?.length ? [a.spec.command, ...a.spec.args].join(" ") : a.spec.command;
  const envRows = Object.entries(a.env_masked || {}).map(
    ([k, v]) => `<tr><td>${esc(k)}</td><td>${esc(v)}</td></tr>`).join("");
  const health = a.spec.health?.type
    ? `${a.spec.health.type}: ${esc(a.spec.health.target)}`
    : "未配置";
  $("d-info").innerHTML = `
    <h3>基本信息</h3>
    <div class="kv">
      <b>状态</b><span>${esc(a.status.state)}</span>
      <b>PID</b><span>${a.status.pid || "-"}</span>
      <b>命令</b><span>${esc(cmd)}</span>
      <b>cwd</b><span>${esc(a.spec.cwd || "-")}</span>
      <b>重启策略</b><span>${esc(a.spec.restart)}${a.spec.max_restarts ? ` (max ${a.spec.max_restarts})` : ""}</span>
      <b>重启次数</b><span>${a.status.restarts}</span>
      <b>最后退出码</b><span>${a.status.exit_code ?? "-"}</span>
      <b>启动时间</b><span>${a.status.started_at?.startsWith("0001") ? "-" : esc(a.status.started_at)}</span>
      <b>健康检查</b><span>${health}</span>
      ${a.status.last_error ? `<b>错误</b><span style="color:var(--err)">${esc(a.status.last_error)}</span>` : ""}
    </div>
    <h3>环境变量（已脱敏）</h3>
    ${envRows ? `<table class="env-table">${envRows}</table>` : "<p class='center'>无</p>"}`;

  $("d-start").onclick = () => op("start", name);
  $("d-stop").onclick = () => op("stop", name);
  $("d-restart").onclick = () => op("restart", name);
  $("d-delete").onclick = () => { if (confirm(`删除 ${name}？`)) op("delete", name); };

  await loadLogs(false);
}

$("d-close").addEventListener("click", closeDetail);
$("d-clear").addEventListener("click", () => { $("d-logs").textContent = ""; });

// ---------- 日志 ----------
function logStream() {
  return document.querySelector('input[name="logstream"]:checked').value;
}
document.querySelectorAll('input[name="logstream"]').forEach((r) =>
  r.addEventListener("change", () => loadLogs($("d-follow").checked)));
$("d-follow").addEventListener("change", (e) => loadLogs(e.target.checked));

async function loadLogs(follow) {
  if (!currentApp) return;
  stopFollow();
  const stream = logStream();
  const url = `/api/apps/${encodeURIComponent(currentApp)}/logs?stream=${stream}` +
    (follow ? "&follow=true" : "");
  try {
    if (!follow) {
      const resp = await fetch(url);
      $("d-logs").textContent = await resp.text();
      $("d-logs").scrollTop = $("d-logs").scrollHeight;
    } else {
      followES = new EventSource(url);
      followES.onmessage = (e) => {
        $("d-logs").textContent += e.data + "\n";
        $("d-logs").scrollTop = $("d-logs").scrollHeight;
      };
      followES.onerror = () => stopFollow();
    }
  } catch (err) { $("d-logs").textContent += `[日志错误: ${err.message}]\n`; }
}

function stopFollow() {
  if (followES) { followES.close(); followES = null; }
}

// ---------- 事件流（状态变化即时刷新） ----------
function startEventStream() {
  stopEventStream();
  eventsES = new EventSource("/api/events");
  eventsES.onopen = () => $("conn-dot").classList.add("on");
  eventsES.onerror = () => $("conn-dot").classList.remove("on");
  eventsES.addEventListener("state", () => refresh());
  // 定时兜底刷新（指标变化无事件）
  refreshTimer = setInterval(refresh, 5000);
}

function stopEventStream() {
  if (eventsES) { eventsES.close(); eventsES = null; }
  if (refreshTimer) { clearInterval(refreshTimer); refreshTimer = null; }
}

// ---------- 启动 ----------
(async function init() {
  try {
    await api("GET", "/api/apps");
    await showApp();
  } catch {
    showLogin();
  }
})();
