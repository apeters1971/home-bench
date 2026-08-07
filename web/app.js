const PHASE_LABELS = {
  create: "Create",
  delete: "Delete",
  write_bw: "Write BW",
  read_bw: "Read BW",
  read_write: "Read+Write",
  final_delete: "Final Delete",
};

const PHASE_BANDS = {
  create:       { fill: "rgba(15, 122, 95, 0.13)",  stroke: "#0f7a5f", label: "Create" },
  delete:       { fill: "rgba(180, 83, 9, 0.13)",   stroke: "#b45309", label: "Delete" },
  write_bw:     { fill: "rgba(31, 95, 191, 0.13)",  stroke: "#1f5fbf", label: "Write BW" },
  read_bw:      { fill: "rgba(14, 116, 144, 0.13)", stroke: "#0e7490", label: "Read BW" },
  read_write:   { fill: "rgba(161, 98, 7, 0.15)",   stroke: "#a16207", label: "R+W" },
  final_delete: { fill: "rgba(185, 28, 28, 0.12)",  stroke: "#b91c1c", label: "Final Del" },
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
  form.phase_step_seconds.value = cfg.phase_step_seconds ?? 30;
}

function render(snap) {
  const scrollX = window.scrollX;
  const scrollY = window.scrollY;
  const active = document.activeElement;
  const activeName = active && active.name ? active.name : null;

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

  renderClients(snap.clients || []);
  drawCharts(snap.history || []);

  // Live WS redraws must not steal focus or jump the viewport (e.g. toward charts).
  window.scrollTo(scrollX, scrollY);
  if (activeName) {
    const field = document.querySelector(`[name="${activeName}"]`);
    if (field && document.activeElement !== field) field.focus({ preventScroll: true });
  } else if (active && active !== document.body && document.contains(active)) {
    active.focus({ preventScroll: true });
  }
}

function renderClients(clients) {
  const body = $("client-body");
  const meta = $("client-list-meta");
  const n = clients.length;
  meta.textContent = n <= 10 ? `${n} client${n === 1 ? "" : "s"}` : `${n} clients · scroll for more`;

  const next = clients.map((c) =>
    [c.hostname, c.prefix, c.status, PHASE_LABELS[c.phase] || c.phase || ""].join("\0")
  ).join("\n");
  if (body.dataset.sig === next) return;
  body.dataset.sig = next;

  // Preserve scroll position across live updates.
  const wrap = body.closest(".table-wrap");
  const scrollTop = wrap ? wrap.scrollTop : 0;

  body.innerHTML = "";
  for (const c of clients) {
    const tr = document.createElement("tr");
    tr.innerHTML = `
      <td>${escapeHtml(c.hostname)}</td>
      <td class="mono">${escapeHtml(c.prefix)}</td>
      <td>${escapeHtml(c.status)}</td>
      <td>${escapeHtml(PHASE_LABELS[c.phase] || c.phase || "")}</td>`;
    body.appendChild(tr);
  }
  if (wrap) wrap.scrollTop = scrollTop;
}

function escapeHtml(s) {
  return String(s ?? "")
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;");
}

function drawCharts(history) {
  const spans = state.snapshot?.phase_spans || [];
  drawLineChart($("chart-iops"), history, [
    { key: "write_iops", color: "#0f7a5f" },
    { key: "read_iops", color: "#1f5fbf" },
    { key: "delete_iops", color: "#b45309" },
  ], false, spans);
  drawLineChart($("chart-bw"), history, [
    { key: "write_bps", color: "#0f7a5f" },
    { key: "read_bps", color: "#1f5fbf" },
  ], true, spans);
}

function sampleTime(pt) {
  return new Date(pt.timestamp).getTime();
}

function drawPhaseBands(ctx, spans, tMin, tMax, pad, plotW, plotH) {
  if (!spans?.length || !(tMax > tMin)) return;
  const labelY = 12;
  const barTop = pad.t;
  const barH = plotH;

  for (const span of spans) {
    const style = PHASE_BANDS[span.phase];
    if (!style) continue;
    const start = new Date(span.start).getTime();
    const end = span.end ? new Date(span.end).getTime() : Date.now();
    const x0 = pad.l + ((Math.max(start, tMin) - tMin) / (tMax - tMin)) * plotW;
    const x1 = pad.l + ((Math.min(end, tMax) - tMin) / (tMax - tMin)) * plotW;
    const w = Math.max(1, x1 - x0);
    if (x1 < pad.l || x0 > pad.l + plotW) continue;

    ctx.fillStyle = style.fill;
    ctx.fillRect(x0, barTop, w, barH);

    ctx.strokeStyle = style.stroke;
    ctx.globalAlpha = 0.35;
    ctx.beginPath();
    ctx.moveTo(x0, barTop);
    ctx.lineTo(x0, barTop + barH);
    ctx.stroke();
    ctx.globalAlpha = 1;

    if (w >= 36) {
      ctx.fillStyle = style.stroke;
      ctx.font = "600 10px Sora, sans-serif";
      ctx.textAlign = "center";
      ctx.textBaseline = "middle";
      const lx = x0 + w / 2;
      ctx.fillText(style.label, lx, labelY);
    }
  }
  ctx.textBaseline = "alphabetic";
}

function drawLineChart(canvas, history, series, isBytes, spans) {
  const dpr = window.devicePixelRatio || 1;
  const rect = canvas.getBoundingClientRect();
  const width = Math.max(320, Math.floor(rect.width));
  const height = 170;
  const bufW = Math.floor(width * dpr);
  const bufH = Math.floor(height * dpr);
  // Only reset the bitmap when the display size changes — avoids layout jumps.
  if (canvas.width !== bufW || canvas.height !== bufH) {
    canvas.width = bufW;
    canvas.height = bufH;
  }
  const ctx = canvas.getContext("2d");
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  ctx.clearRect(0, 0, width, height);

  const pad = { l: 54, r: 12, t: 26, b: 24 };
  const plotW = width - pad.l - pad.r;
  const plotH = height - pad.t - pad.b;

  if (!history.length) {
    ctx.strokeStyle = "rgba(20,32,28,0.08)";
    ctx.lineWidth = 1;
    for (let i = 0; i <= 4; i++) {
      const y = pad.t + (plotH * i) / 4;
      ctx.beginPath();
      ctx.moveTo(pad.l, y);
      ctx.lineTo(pad.l + plotW, y);
      ctx.stroke();
    }
    ctx.fillStyle = "#5c6b64";
    ctx.font = "12px Sora, sans-serif";
    ctx.textAlign = "left";
    ctx.fillText("Waiting for metrics…", pad.l + 8, pad.t + 20);
    return;
  }

  let tMin = sampleTime(history[0]);
  let tMax = sampleTime(history[history.length - 1]);
  if (state.snapshot?.started_at) {
    tMin = Math.min(tMin, new Date(state.snapshot.started_at).getTime());
  }
  if (state.snapshot?.running) {
    tMax = Math.max(tMax, Date.now());
  }
  if (!(tMax > tMin)) tMax = tMin + 1;

  drawPhaseBands(ctx, spans, tMin, tMax, pad, plotW, plotH);

  ctx.strokeStyle = "rgba(20,32,28,0.08)";
  ctx.lineWidth = 1;
  for (let i = 0; i <= 4; i++) {
    const y = pad.t + (plotH * i) / 4;
    ctx.beginPath();
    ctx.moveTo(pad.l, y);
    ctx.lineTo(pad.l + plotW, y);
    ctx.stroke();
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

  const xAt = (t) => pad.l + ((t - tMin) / (tMax - tMin)) * plotW;

  for (const s of series) {
    ctx.beginPath();
    ctx.strokeStyle = s.color;
    ctx.lineWidth = 2;
    history.forEach((pt, i) => {
      const x = xAt(sampleTime(pt));
      const y = pad.t + plotH - ((Number(pt[s.key]) || 0) / maxY) * plotH;
      if (i === 0) ctx.moveTo(x, y);
      else ctx.lineTo(x, y);
    });
    ctx.stroke();
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
    phase_step_seconds: Number(form.phase_step_seconds.value),
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
