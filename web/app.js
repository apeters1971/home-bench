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
  hover: null, // { canvasId, index }
};

const SERIES_LABELS = {
  write_iops: "Write/Create",
  read_iops: "Read",
  delete_iops: "Delete",
  write_bps: "Write",
  read_bps: "Read",
};

// Trailing window (seconds/samples) used to damp multi-client plot noise.
const CHART_SMOOTH_WINDOW = 5;
const SMOOTH_KEYS = [
  "read_iops",
  "write_iops",
  "delete_iops",
  "create_iops",
  "read_bps",
  "write_bps",
];

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

function smoothHistory(history, windowSize = CHART_SMOOTH_WINDOW) {
  if (!history?.length || windowSize <= 1) return history || [];
  return history.map((pt, i) => {
    const from = Math.max(0, i - windowSize + 1);
    const slice = history.slice(from, i + 1);
    const n = slice.length;
    const out = { ...pt };
    for (const key of SMOOTH_KEYS) {
      let sum = 0;
      for (const s of slice) sum += Number(s[key]) || 0;
      out[key] = sum / n;
    }
    return out;
  });
}

function drawCharts(history) {
  const spans = state.snapshot?.phase_spans || [];
  const smoothed = smoothHistory(history);
  drawLineChart($("chart-iops"), smoothed, [
    { key: "write_iops", color: "#0f7a5f" },
    { key: "read_iops", color: "#1f5fbf" },
    { key: "delete_iops", color: "#b45309" },
  ], false, spans);
  drawLineChart($("chart-bw"), smoothed, [
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

function ensureTooltip(canvas) {
  const frame = canvas.parentElement;
  if (!frame) return null;
  let tip = frame.querySelector(".chart-tooltip");
  if (!tip) {
    tip = document.createElement("div");
    tip.className = "chart-tooltip";
    frame.appendChild(tip);
  }
  return tip;
}

function formatSeriesValue(v, isBytes) {
  if (isBytes) return formatRate(v);
  if (v >= 100) return Math.round(v).toString();
  return v.toFixed(1);
}

function phaseAtTime(spans, t) {
  for (const span of spans || []) {
    const start = new Date(span.start).getTime();
    const end = span.end ? new Date(span.end).getTime() : Date.now();
    if (t >= start && t <= end) return PHASE_LABELS[span.phase] || span.phase;
  }
  return "";
}

function nearestSampleIndex(history, tMin, tMax, plotW, padL, localX) {
  if (!history.length || !(tMax > tMin)) return -1;
  const t = tMin + ((localX - padL) / plotW) * (tMax - tMin);
  let best = 0;
  let bestDist = Infinity;
  for (let i = 0; i < history.length; i++) {
    const d = Math.abs(sampleTime(history[i]) - t);
    if (d < bestDist) {
      bestDist = d;
      best = i;
    }
  }
  return best;
}

function bindChartHover(canvas) {
  if (canvas.dataset.hoverBound) return;
  canvas.dataset.hoverBound = "1";

  canvas.addEventListener("mousemove", (e) => {
    const meta = canvas._chart;
    if (!meta?.history?.length) {
      hideTooltip(canvas);
      return;
    }
    const rect = canvas.getBoundingClientRect();
    const x = e.clientX - rect.left;
    const y = e.clientY - rect.top;
    const { pad, plotW, plotH, tMin, tMax, history } = meta;
    if (x < pad.l || x > pad.l + plotW || y < pad.t || y > pad.t + plotH) {
      if (state.hover?.canvasId === canvas.id) {
        state.hover = null;
        hideTooltip(canvas);
        drawCharts(state.snapshot?.history || []);
      }
      return;
    }
    const idx = nearestSampleIndex(history, tMin, tMax, plotW, pad.l, x);
    const prev = state.hover;
    state.hover = { canvasId: canvas.id, index: idx, x, y };
    showTooltip(canvas, idx, x, y);
    if (!prev || prev.canvasId !== canvas.id || prev.index !== idx) {
      drawCharts(state.snapshot?.history || []);
    }
  });

  canvas.addEventListener("mouseleave", () => {
    if (state.hover?.canvasId === canvas.id) state.hover = null;
    hideTooltip(canvas);
    drawCharts(state.snapshot?.history || []);
  });
}

function hideTooltip(canvas) {
  const tip = canvas.parentElement?.querySelector(".chart-tooltip");
  if (tip) tip.classList.remove("visible");
}

function showTooltip(canvas, idx, localX, localY) {
  const meta = canvas._chart;
  const tip = ensureTooltip(canvas);
  if (!meta || !tip || idx < 0) return;
  const pt = meta.history[idx];
  const t = sampleTime(pt);
  const phase = phaseAtTime(meta.spans, t);
  const timeStr = new Date(t).toLocaleTimeString();
  const rows = meta.series.map((s) => {
    const v = Number(pt[s.key]) || 0;
    return `<div class="tt-row"><span class="tt-name" style="color:${s.color}">${SERIES_LABELS[s.key] || s.key}</span><span class="tt-val">${formatSeriesValue(v, meta.isBytes)}</span></div>`;
  }).join("");
  tip.innerHTML = `<div class="tt-time">${timeStr}${phase ? " · " + phase : ""}</div>${rows}`;
  tip.classList.add("visible");

  const frame = canvas.parentElement.getBoundingClientRect();
  const tipW = tip.offsetWidth || 160;
  const tipH = tip.offsetHeight || 80;
  let left = localX + 14;
  let top = localY - tipH - 10;
  if (left + tipW > frame.width - 4) left = localX - tipW - 14;
  if (top < 4) top = localY + 14;
  tip.style.left = `${Math.max(4, left)}px`;
  tip.style.top = `${Math.max(4, top)}px`;
}

function drawLineChart(canvas, history, series, isBytes, spans) {
  bindChartHover(canvas);

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
    canvas._chart = null;
    hideTooltip(canvas);
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

  let maxY = 1;
  for (const pt of history) {
    for (const s of series) maxY = Math.max(maxY, Number(pt[s.key]) || 0);
  }
  maxY *= 1.15;

  canvas._chart = { history, series, isBytes, spans, pad, tMin, tMax, maxY, plotW, plotH };

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
  const yAt = (v) => pad.t + plotH - (v / maxY) * plotH;

  for (const s of series) {
    ctx.beginPath();
    ctx.strokeStyle = s.color;
    ctx.lineWidth = 2;
    history.forEach((pt, i) => {
      const x = xAt(sampleTime(pt));
      const y = yAt(Number(pt[s.key]) || 0);
      if (i === 0) ctx.moveTo(x, y);
      else ctx.lineTo(x, y);
    });
    ctx.stroke();
  }

  // Hover crosshair + sample markers
  if (state.hover?.canvasId === canvas.id && state.hover.index >= 0) {
    const idx = Math.min(state.hover.index, history.length - 1);
    state.hover.index = idx;
    const pt = history[idx];
    const x = xAt(sampleTime(pt));
    ctx.strokeStyle = "rgba(20,32,28,0.35)";
    ctx.lineWidth = 1;
    ctx.setLineDash([4, 3]);
    ctx.beginPath();
    ctx.moveTo(x, pad.t);
    ctx.lineTo(x, pad.t + plotH);
    ctx.stroke();
    ctx.setLineDash([]);
    for (const s of series) {
      const y = yAt(Number(pt[s.key]) || 0);
      ctx.fillStyle = s.color;
      ctx.beginPath();
      ctx.arc(x, y, 3.5, 0, Math.PI * 2);
      ctx.fill();
      ctx.strokeStyle = "#fff";
      ctx.lineWidth = 1.5;
      ctx.stroke();
    }
    if (state.hover.x != null) {
      showTooltip(canvas, idx, state.hover.x, state.hover.y);
    }
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
