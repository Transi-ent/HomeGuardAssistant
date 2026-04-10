const jsonHeaders = { "Content-Type": "application/json" };

async function cmd(deviceId, type) {
  const r = await fetch("/api/console/command", {
    method: "POST",
    headers: jsonHeaders,
    body: JSON.stringify({ device_id: deviceId, type })
  });
  if (!r.ok) alert(await r.text());
}

async function approveDevice(deviceId) {
  const r = await fetch("/api/console/device-requests/approve", {
    method: "POST",
    headers: jsonHeaders,
    body: JSON.stringify({ device_id: deviceId })
  });
  if (!r.ok) return alert(await r.text());
  location.reload();
}

async function rejectDevice(deviceId) {
  const r = await fetch("/api/console/device-requests/reject", {
    method: "POST",
    headers: jsonHeaders,
    body: JSON.stringify({ device_id: deviceId })
  });
  if (!r.ok) return alert(await r.text());
  location.reload();
}

async function toggleSchedule(id, enabled) {
  const r = await fetch("/api/console/schedules", {
    method: "PATCH",
    headers: jsonHeaders,
    body: JSON.stringify({ id, enabled: !enabled })
  });
  if (!r.ok) return alert(await r.text());
  location.reload();
}

async function deleteSchedule(id) {
  const r = await fetch(`/api/console/schedules?id=${encodeURIComponent(id)}`, { method: "DELETE" });
  if (!r.ok) return alert(await r.text());
  location.reload();
}

async function deleteMedia(name) {
  const r = await fetch(`/api/console/media?name=${encodeURIComponent(name)}`, { method: "DELETE" });
  if (!r.ok) return alert(await r.text());
  location.reload();
}

const schForm = document.getElementById("schForm");
if (schForm) {
  schForm.onsubmit = async (e) => {
    e.preventDefault();
    const fd = new FormData(e.target);
    const body = {
      device_id: fd.get("device_id"),
      type: fd.get("type"),
      interval_seconds: Number(fd.get("interval_seconds")),
      duration_seconds: Number(fd.get("duration_seconds"))
    };
    const r = await fetch("/api/console/schedules", { method: "POST", headers: jsonHeaders, body: JSON.stringify(body) });
    if (!r.ok) return alert(await r.text());
    location.reload();
  };
}

const pwdForm = document.getElementById("pwdForm");
if (pwdForm) {
  pwdForm.onsubmit = async (e) => {
    e.preventDefault();
    const fd = new FormData(e.target);
    const body = {
      username: fd.get("username"),
      old_password: fd.get("old_password"),
      new_password: fd.get("new_password")
    };
    const r = await fetch("/api/console/password", { method: "POST", headers: jsonHeaders, body: JSON.stringify(body) });
    if (!r.ok) return alert(await r.text());
    alert("密码已更新");
    e.target.reset();
  };
}

async function refreshAudit() {
  const r = await fetch("/api/console/audit?limit=50");
  if (!r.ok) return alert(await r.text());
  const list = await r.json();
  const container = document.getElementById("auditList");
  if (!container) return;
  if (!Array.isArray(list) || list.length === 0) {
    container.innerHTML = '<p class="muted">暂无审计记录</p>';
    return;
  }
  container.innerHTML = list.map(ev => `
    <div class="row">
      <div><b>${escapeHtml(ev.action || "")}</b> | actor=${escapeHtml(ev.actor || "")} | target=${escapeHtml(ev.target || "")} | ${escapeHtml(ev.detail || "")}</div>
      <div class="muted">${formatUnix(ev.created_at)}</div>
    </div>
  `).join("");
}

function formatUnix(ts) {
  if (!ts) return "-";
  return new Date(ts * 1000).toLocaleString();
}

function escapeHtml(s) {
  return String(s).replaceAll("&", "&amp;").replaceAll("<", "&lt;").replaceAll(">", "&gt;");
}
