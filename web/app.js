const PHASE_LABELS = {
  create: "Create",
  delete: "Delete",
  write_bw: "Write BW",
  read_bw: "Read BW",
  read_write: "Read+Write",
  final_delete: "Final Delete",
};

const MiB = 1024 * 1024;

const state = {
  snapshot: null,
  phaseOrder: ["create", "delete", "write_bw", "read_bw", "read_write", "final_delete"],
};

function $(id) {
  return document.getElementById(id);
}

function formatElapsed(sec) {
  const s = Math.max(0, Math.floor(sec || 0));
  const h = String(Math.floor(s / 3600)).padStart(2, "0");
  const m = String(Math.floor((s % 3600) / 60)).padStart(2, "0");
  const r = String(s % 60).padStart(2, "0");
  return `${h}:${m}:${r}`;
}

function formatRate(n) {
  if (n >= 1e9) return (n / 1e9).toFixed(2) + " GB/s";
  if (n >= 1e6) return (n / 1e6).toFixed(1) + " MB/s";
  if (n >= 1e3) return (n / 1e3).toFixed(1) + " KB/s";
  return n.toFixed(0) + " B/s";
}

function buildPhaseRow() {
  const row = $("phase-row");
  row.innerHTML = "";
  for (const p of state.phaseOrder) {
    const btn = document.createElement("button");
    btn.type = "button";
    btn.className = "phase-btn";
    btn.dataset.phase = p;
    btn.textContent = PHASE_LABELS[p] || p;
    btn.disabled = true;
    row.appendChild(btn);
  }
}

function applyConfigForm(cfg) {
  const form = $("config-form");
  form.test_name.value = cfg.test_name || "";
  form.prefixes.value = (cfg.prefixes || []).join("\n");
  form.file_creation_rate.value = cfg.file_creation_rate ?? "";
  form.file_deletion_rate.value = cfg.file_deletion_rate ?? "";
  form.file_write_bandwidth_mib.value = ((cfg.file_write_bandwidth || 0) / MiB).toFixed(1);
  form.file_read_bandwidth_mib.value = ((cfg.file_read_bandwidth || 0) / MiB).toFixed(1);
}

function render(snap) {
  state.snapshot = snap;
  if (snap.phase_order?.length) state.phaseOrder = snap.phase_order;

  $("elapsed").textContent = formatElapsed(snap.elapsed_sec);
  $("client-count").textContent = String(snap.client_count ?? snap.clients?.length ?? 0);
  $("status-text").textContent = snap.status_text || "Ready";
  $("percent").textContent = snap.running && snap.percent ? `${snap.percent}%` : "";

  $("btn-start").disabled = !!snap.running;
  $("btn-stop").disabled = !snap.running;

  document.querySelectorAll(".phase-btn").forEach((btn) => {
    btn.classList.toggle("active", snap.running && btn.dataset.phase === snap.phase);
  });

  const body = $("client-body");
  body.innerHTML = "";
  for (const c of snap.clients || []) {
    const tr = document.createElement("tr");
    tr.innerHTML = `
      <td>${escapeHtml(c.hostname)}</td>
      <td class="mono">${escapeHtml(c.prefix)}</td>
      <td>${escapeHtml(c.status)}</td>
      <td>${escapeHtml(PHASE_LABELS[c.phase] || c.phase || "")}</td>`;
    body.appendChild(tr);
  }

  drawCharts(snap.history || []);
}

function escapeHtml(s) {
  return String(s ?? "")
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;");
}

function drawCharts(history) {
  drawLineChart($("chart-iops"), history, [
    { key: "write_iops", color: "#0f7a5f" },
    { key: "read_iops", color: "#1f5fbf" },
    { key: "delete_iops", color: "#b45309" },
  ]);
  drawLineChart($("chart-bw"), history, [
    { key: "write_bps", color: "#0f7a5f" },
    { key: "read_bps", color: "#1f5fbf" },
  ], true);
}

function drawLineChart(canvas, history, series, isBytes) {
  const dpr = window.devicePixelRatio || 1;
  const rect = canvas.getBoundingClientRect();
  const width = Math.max(320, rect.width);
  const height = canvas.height;
  canvas.width = Math.floor(width * dpr);
  canvas.height = Math.floor(height * dpr);
  const ctx = canvas.getContext("2d");
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  ctx.clearRect(0, 0, width, height);

  const pad = { l: 54, r: 12, t: 12, b: 24 };
  const plotW = width - pad.l - pad.r;
  const plotH = height - pad.t - pad.b;

  ctx.strokeStyle = "rgba(20,32,28,0.08)";
  ctx.lineWidth = 1;
  for (let i = 0; i <= 4; i++) {
    const y = pad.t + (plotH * i) / 4;
    ctx.beginPath();
    ctx.moveTo(pad.l, y);
    ctx.lineTo(pad.l + plotW, y);
    ctx.stroke();
  }

  if (!history.length) {
    ctx.fillStyle = "#5c6b64";
    ctx.font = "12px Sora, sans-serif";
    ctx.fillText("Waiting for metrics…", pad.l + 8, pad.t + 20);
    return;
  }

  let maxY = 1;
  for (const pt of history) {
    for (const s of series) maxY = Math.max(maxY, Number(pt[s.key]) || 0);
  }
  maxY *= 1.15;

  ctx.fillStyle = "#5c6b64";
  ctx.font = "11px IBM Plex Mono, monospace";
  ctx.textAlign = "right";
  for (let i = 0; i <= 4; i++) {
    const v = maxY * (1 - i / 4);
    const y = pad.t + (plotH * i) / 4 + 3;
    const label = isBytes ? formatRate(v) : Math.round(v).toString();
    ctx.fillText(label, pad.l - 8, y);
  }

  const n = history.length;
  for (const s of series) {
    ctx.beginPath();
    ctx.strokeStyle = s.color;
    ctx.lineWidth = 2;
    history.forEach((pt, i) => {
      const x = pad.l + (plotW * i) / Math.max(1, n - 1);
      const y = pad.t + plotH - ((Number(pt[s.key]) || 0) / maxY) * plotH;
      if (i === 0) ctx.moveTo(x, y);
      else ctx.lineTo(x, y);
    });
    ctx.stroke();
  }

  // Phase window highlight using last point time span — subtle right edge glow when running.
  if (state.snapshot?.running) {
    const win = Math.min(n, 60);
    const x0 = pad.l + (plotW * (n - win)) / Math.max(1, n - 1);
    const grad = ctx.createLinearGradient(x0, 0, pad.l + plotW, 0);
    grad.addColorStop(0, "rgba(15,122,95,0)");
    grad.addColorStop(1, "rgba(15,122,95,0.08)");
    ctx.fillStyle = grad;
    ctx.fillRect(x0, pad.t, pad.l + plotW - x0, plotH);
  }
}

async function saveConfig(ev) {
  ev.preventDefault();
  const form = ev.target;
  const cfg = {
    test_name: form.test_name.value.trim(),
    prefixes: form.prefixes.value
      .split("\n")
      .map((s) => s.trim())
      .filter(Boolean),
    file_creation_rate: Number(form.file_creation_rate.value),
    file_deletion_rate: Number(form.file_deletion_rate.value),
    file_write_bandwidth: Number(form.file_write_bandwidth_mib.value) * MiB,
    file_read_bandwidth: Number(form.file_read_bandwidth_mib.value) * MiB,
  };
  const res = await fetch("/api/config", {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(cfg),
  });
  const msg = $("config-msg");
  if (!res.ok) {
    msg.textContent = await res.text();
    msg.style.color = "var(--danger)";
    return;
  }
  msg.textContent = "Configuration saved";
  msg.style.color = "var(--accent)";
}

async function startTest() {
  const res = await fetch("/api/start", { method: "POST" });
  if (!res.ok) alert(await res.text());
}

async function stopTest() {
  const res = await fetch("/api/stop", { method: "POST" });
  if (!res.ok) alert(await res.text());
}

function connectWS() {
  const proto = location.protocol === "https:" ? "wss" : "ws";
  const ws = new WebSocket(`${proto}://${location.host}/ws/ui`);
  ws.onmessage = (ev) => {
    try {
      render(JSON.parse(ev.data));
    } catch (_) {}
  };
  ws.onclose = () => setTimeout(connectWS, 1500);
}

async function bootstrap() {
  buildPhaseRow();
  $("config-form").addEventListener("submit", saveConfig);
  $("btn-start").addEventListener("click", startTest);
  $("btn-stop").addEventListener("click", stopTest);
  window.addEventListener("resize", () => {
    if (state.snapshot) drawCharts(state.snapshot.history || []);
  });

  const res = await fetch("/api/state");
  const snap = await res.json();
  applyConfigForm(snap.config || {});
  buildPhaseRow();
  render(snap);
  connectWS();

  // Keep elapsed ticking smoothly between WS pushes.
  setInterval(() => {
    if (!state.snapshot?.running || !state.snapshot.started_at) return;
    const started = new Date(state.snapshot.started_at).getTime();
    $("elapsed").textContent = formatElapsed((Date.now() - started) / 1000);
  }, 250);
}

bootstrap();
