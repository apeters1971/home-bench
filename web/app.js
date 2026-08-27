const PHASE_LABELS = {
  software_unpack: "Software Unpack",
  software_cold: "Software Cold",
  software_warm: "Software Warm",
  git_clone: "Git Clone",
  untar: "Untar",
  create: "Create",
  delete: "Delete",
  write_bw: "Write BW",
  read_bw: "Read BW",
  read_write: "Read+Write",
  final_delete: "Final Delete",
};

// Phase band colors are distinct from the series colors (write=green, read=blue, delete=amber).
const PHASE_BANDS = {
  software_unpack: { fill: "rgba(88, 28, 135, 0.10)", stroke: "#6b21a8", label: "Unpack" },
  software_cold:   { fill: "rgba(14, 116, 144, 0.12)", stroke: "#0e7490", label: "Cold" },
  software_warm:   { fill: "rgba(190, 24, 93, 0.10)",  stroke: "#be185d", label: "Warm" },
  git_clone:       { fill: "rgba(22, 101, 52, 0.12)",  stroke: "#166534", label: "Git" },
  untar:           { fill: "rgba(120, 53, 15, 0.12)",  stroke: "#9a3412", label: "Untar" },
  create:       { fill: "rgba(15, 122, 95, 0.12)",  stroke: "#0f7a5f", label: "Create" },
  delete:       { fill: "rgba(180, 83, 9, 0.12)",   stroke: "#b45309", label: "Delete" },
  write_bw:     { fill: "rgba(71, 85, 105, 0.14)",  stroke: "#334155", label: "Write BW" },
  read_bw:      { fill: "rgba(15, 118, 110, 0.12)", stroke: "#0f766e", label: "Read BW" },
  read_write:   { fill: "rgba(202, 138, 4, 0.14)",  stroke: "#a16207", label: "R+W" },
  final_delete: { fill: "rgba(185, 28, 28, 0.12)",  stroke: "#b91c1c", label: "Final Del" },
};

const MiB = 1024 * 1024;

const state = {
  snapshot: null,
  phaseOrder: ["create", "delete", "write_bw", "read_bw", "read_write", "final_delete"],
  hover: null, // { canvasId, index }
  histHover: null, // { canvasId, index, x, y }
};

const SERIES_LABELS = {
  write_iops: "Write/Create",
  read_iops: "Read",
  delete_iops: "Delete",
  write_bps: "Write",
  read_bps: "Read",
  expected: "Expected",
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

const RAMP_PERCENTS = [10, 20, 30, 40, 50, 60, 70, 80, 90, 100];
const BW_FILE_SIZE = 64 * MiB;
const CREATE_FILE_SIZE = 4096;
const EXPECTED_COLOR = "#94a3b8";

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

function formatPhaseDuration(ms) {
  const s = Math.max(0, Math.round((ms || 0) / 1000));
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  const r = s % 60;
  if (m < 60) return r ? `${m}m${String(r).padStart(2, "0")}s` : `${m}m`;
  const h = Math.floor(m / 60);
  const rm = m % 60;
  return rm ? `${h}h${rm}m` : `${h}h`;
}

function softwareEnabled(cfg) {
  return !!(String(cfg?.package_url || "").trim() && String(cfg?.startup_command || "").trim());
}

function gitCloneEnabled(cfg) {
  return !!String(cfg?.git_clone_url || "").trim();
}

function untarEnabled(cfg) {
  return !!String(cfg?.untar_url || "").trim();
}

function spanDurationSec(span) {
  if (!span?.start || !span?.end) return null;
  return Math.max(0, (new Date(span.end).getTime() - new Date(span.start).getTime()) / 1000);
}

function addMeasuredOrApprox(spans, phase, fallbackSec, tips) {
  const sp = (spans || []).find((s) => s.phase === phase);
  const d = spanDurationSec(sp);
  if (d != null) {
    tips.push(`${PHASE_LABELS[phase] || phase} (measured): ${Math.round(d)}s`);
    return { sec: d, approx: false };
  }
  tips.push(`${PHASE_LABELS[phase] || phase}: variable`);
  return { sec: fallbackSec, approx: true };
}

// Planned wall time for a full run (ramps are fixed; software/git/untar vary).
function estimatedRuntime(cfg, spans) {
  const step = Math.max(1, Number(cfg?.phase_step_seconds) || 30);
  // create(10) + delete(10+1) + write(10) + read(10) + r+w(10) + final(10+1)
  let sec = (10 + 11 + 10 + 10 + 10 + 11) * step;
  let approx = false;
  const tips = [`IO ramps: ${62 * step}s (${step}s × 62 steps)`];

  if (softwareEnabled(cfg)) {
    const unpack = addMeasuredOrApprox(spans, "software_unpack", 0, tips);
    sec += unpack.sec;
    approx = approx || unpack.approx;
    for (const phase of ["software_cold", "software_warm"]) {
      const sp = (spans || []).find((s) => s.phase === phase);
      const d = spanDurationSec(sp);
      if (d != null) {
        sec += d;
        tips.push(`${PHASE_LABELS[phase] || phase} (measured): ${Math.round(d)}s`);
      } else {
        sec += 60;
        tips.push(`${PHASE_LABELS[phase] || phase}: ~60s`);
      }
    }
  }
  if (gitCloneEnabled(cfg)) {
    const g = addMeasuredOrApprox(spans, "git_clone", 0, tips);
    sec += g.sec;
    approx = approx || g.approx;
  }
  if (untarEnabled(cfg)) {
    const u = addMeasuredOrApprox(spans, "untar", 0, tips);
    sec += u.sec;
    approx = approx || u.approx;
  }

  return { sec, approx, title: tips.join(" · ") };
}

function updateEstimated(snap) {
  const el = $("estimated");
  if (!el) return;
  const { sec, approx, title } = estimatedRuntime(snap?.config || {}, snap?.phase_spans || []);
  el.textContent = (approx ? "~" : "") + formatElapsed(sec);
  el.title = title;
}

function formatRate(n) {
  if (n >= 1e9) return (n / 1e9).toFixed(2) + " GB/s";
  if (n >= 1e6) return (n / 1e6).toFixed(1) + " MB/s";
  if (n >= 1e3) return (n / 1e3).toFixed(1) + " KB/s";
  return Math.round(n) + " B/s";
}

// Compact Y-axis labels — short enough not to clip, decimal kept obvious.
function formatRateAxis(n) {
  if (n >= 1e9) return (n / 1e9).toFixed(1) + "G";
  if (n >= 1e6) return (n / 1e6).toFixed(0) + "M";
  if (n >= 1e3) return (n / 1e3).toFixed(0) + "k";
  return Math.round(n) + "B";
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
  form.package_url.value = cfg.package_url || "";
  form.startup_command.value = cfg.startup_command || "";
  form.git_clone_url.value = cfg.git_clone_url || "";
  form.untar_url.value = cfg.untar_url || "";
}

function render(snap) {
  const scrollX = window.scrollX;
  const scrollY = window.scrollY;
  const active = document.activeElement;
  const activeName = active && active.name ? active.name : null;

  state.snapshot = snap;
  if (snap.phase_order?.length) {
    const next = snap.phase_order.join("\0");
    if (state.phaseOrder.join("\0") !== next) {
      state.phaseOrder = snap.phase_order;
      buildPhaseRow();
    } else {
      state.phaseOrder = snap.phase_order;
    }
  }

  $("elapsed").textContent = formatElapsed(snap.elapsed_sec);
  updateEstimated(snap);
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
  drawLatencyHistograms(snap);

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

function formatLatencyUs(us) {
  if (us >= 1e6) return (us / 1e6).toFixed(us >= 1e7 ? 0 : 1) + "s";
  if (us >= 1e3) return (us / 1e3).toFixed(us >= 1e4 ? 0 : 1) + "ms";
  return Math.round(us) + "µs";
}

function formatLatencyAvg(hist) {
  if (!hist?.total) return "—";
  return formatLatencyUs(hist.sum_us / hist.total);
}

// Approximate percentile from fixed upper-bound buckets (returns bucket upper edge).
function histogramPercentile(hist, edges, pct) {
  const total = hist?.total || 0;
  if (!total || !edges?.length) return null;
  const counts = hist.counts || [];
  const target = (pct / 100) * total;
  let cum = 0;
  for (let i = 0; i < Math.max(counts.length, edges.length + 1); i++) {
    cum += Number(counts[i]) || 0;
    if (cum >= target) {
      if (i < edges.length) return { index: i, us: edges[i], overflow: false };
      return { index: i, us: edges[edges.length - 1], overflow: true };
    }
  }
  return { index: edges.length, us: edges[edges.length - 1], overflow: true };
}

function formatLatencyPercentile(hist, edges, pct) {
  const p = histogramPercentile(hist, edges, pct);
  if (!p) return "—";
  return p.overflow ? `> ${formatLatencyUs(p.us)}` : formatLatencyUs(p.us);
}

function formatLatencyMeta(hist, edges) {
  const n = hist?.total || 0;
  if (!n) return "n=0";
  return `n=${n} · avg ${formatLatencyAvg(hist)} · p95 ${formatLatencyPercentile(hist, edges, 95)} · p99 ${formatLatencyPercentile(hist, edges, 99)}`;
}

function formatLatencyBucket(edges, i) {
  if (!edges?.length) return `bucket ${i}`;
  if (i <= 0) return `≤ ${formatLatencyUs(edges[0])}`;
  if (i >= edges.length) return `> ${formatLatencyUs(edges[edges.length - 1])}`;
  return `${formatLatencyUs(edges[i - 1])} – ${formatLatencyUs(edges[i])}`;
}

function drawLatencyHistograms(snap) {
  const edges = snap.latency_edges_us || [];
  const lat = snap.latencies || {};
  const specs = [
    { id: "hist-create", meta: "hist-create-meta", hist: lat.create, color: "#0f7a5f", title: "Create" },
    { id: "hist-delete", meta: "hist-delete-meta", hist: lat.delete, color: "#b45309", title: "Delete" },
    { id: "hist-write", meta: "hist-write-meta", hist: lat.write, color: "#0f7a5f", title: "Write" },
    { id: "hist-read", meta: "hist-read-meta", hist: lat.read, color: "#1f5fbf", title: "Read" },
    { id: "hist-startup-cold", meta: "hist-startup-cold-meta", hist: lat.startup_cold, color: "#0e7490", title: "Startup Cold" },
    { id: "hist-startup-warm", meta: "hist-startup-warm-meta", hist: lat.startup_warm, color: "#be185d", title: "Startup Warm" },
    { id: "hist-git-clone", meta: "hist-git-clone-meta", hist: lat.git_clone, color: "#166534", title: "Git Clone" },
    { id: "hist-untar", meta: "hist-untar-meta", hist: lat.untar, color: "#9a3412", title: "Untar" },
  ];
  for (const s of specs) {
    drawHistogram($(s.id), edges, s.hist, s.color, s.title);
    $(s.meta).textContent = formatLatencyMeta(s.hist, edges);
  }
  if (state.histHover) {
    const c = $(state.histHover.canvasId);
    if (c) showHistTooltip(c, state.histHover.index, state.histHover.x, state.histHover.y);
  }
}

function bindHistHover(canvas) {
  if (!canvas || canvas.dataset.histHoverBound) return;
  canvas.dataset.histHoverBound = "1";

  canvas.addEventListener("mousemove", (e) => {
    const meta = canvas._hist;
    if (!meta?.nBuckets || !meta.total) {
      hideTooltip(canvas);
      return;
    }
    const rect = canvas.getBoundingClientRect();
    const x = e.clientX - rect.left;
    const y = e.clientY - rect.top;
    const { pad, plotW, plotH, nBuckets, gap, barW } = meta;
    if (x < pad.l || x > pad.l + plotW || y < pad.t || y > pad.t + plotH) {
      if (state.histHover?.canvasId === canvas.id) {
        state.histHover = null;
        hideTooltip(canvas);
        drawLatencyHistograms(state.snapshot || {});
      }
      return;
    }
    const idx = Math.min(
      nBuckets - 1,
      Math.max(0, Math.floor((x - pad.l) / (barW + gap)))
    );
    const prev = state.histHover;
    state.histHover = { canvasId: canvas.id, index: idx, x, y };
    showHistTooltip(canvas, idx, x, y);
    if (!prev || prev.canvasId !== canvas.id || prev.index !== idx) {
      drawLatencyHistograms(state.snapshot || {});
    }
  });

  canvas.addEventListener("mouseleave", () => {
    if (state.histHover?.canvasId === canvas.id) state.histHover = null;
    hideTooltip(canvas);
    drawLatencyHistograms(state.snapshot || {});
  });
}

function showHistTooltip(canvas, idx, localX, localY) {
  const meta = canvas._hist;
  const tip = ensureTooltip(canvas);
  if (!meta || !tip || idx < 0) return;
  const count = Number(meta.counts[idx]) || 0;
  const range = formatLatencyBucket(meta.edges, idx);
  const pct = meta.total ? ((count / meta.total) * 100).toFixed(1) : "0.0";
  tip.innerHTML =
    `<div class="tt-time">${meta.title}</div>` +
    `<div class="tt-row"><span class="tt-name">Latency</span><span class="tt-val">${range}</span></div>` +
    `<div class="tt-row"><span class="tt-name">Count</span><span class="tt-val">${count}</span></div>` +
    `<div class="tt-row"><span class="tt-name">Share</span><span class="tt-val">${pct}%</span></div>`;
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

function drawHistogram(canvas, edges, hist, color, title) {
  if (!canvas) return;
  bindHistHover(canvas);

  const dpr = window.devicePixelRatio || 1;
  const rect = canvas.getBoundingClientRect();
  const width = Math.max(240, Math.floor(rect.width));
  const height = 160;
  const bufW = Math.floor(width * dpr);
  const bufH = Math.floor(height * dpr);
  if (canvas.width !== bufW || canvas.height !== bufH) {
    canvas.width = bufW;
    canvas.height = bufH;
  }
  const ctx = canvas.getContext("2d");
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  ctx.clearRect(0, 0, width, height);

  const pad = { l: 36, r: 10, t: 10, b: 28 };
  const plotW = width - pad.l - pad.r;
  const plotH = height - pad.t - pad.b;
  const counts = hist?.counts || [];
  const nBuckets = Math.max(counts.length, edges.length + 1);
  const gap = 1;
  const barW = Math.max(1, (plotW - gap * (nBuckets - 1)) / nBuckets);
  const hoverIdx = state.histHover?.canvasId === canvas.id ? state.histHover.index : -1;

  canvas._hist = {
    edges,
    counts,
    total: hist?.total || 0,
    title: title || canvas.id,
    pad,
    plotW,
    plotH,
    nBuckets,
    gap,
    barW,
    color,
  };

  ctx.strokeStyle = "rgba(20,32,28,0.08)";
  ctx.lineWidth = 1;
  for (let i = 0; i <= 3; i++) {
    const y = pad.t + (plotH * i) / 3;
    ctx.beginPath();
    ctx.moveTo(pad.l, y);
    ctx.lineTo(pad.l + plotW, y);
    ctx.stroke();
  }

  if (!hist?.total) {
    ctx.fillStyle = "#5c6b64";
    ctx.font = "12px Sora, sans-serif";
    ctx.textAlign = "left";
    ctx.fillText("No samples yet", pad.l + 8, pad.t + 18);
    return;
  }

  let maxC = 1;
  for (let i = 0; i < nBuckets; i++) maxC = Math.max(maxC, Number(counts[i]) || 0);

  for (let i = 0; i < nBuckets; i++) {
    const c = Number(counts[i]) || 0;
    const h = (c / maxC) * plotH;
    const x = pad.l + i * (barW + gap);
    const y = pad.t + plotH - h;
    ctx.globalAlpha = i === hoverIdx ? 1 : 0.75;
    ctx.fillStyle = color;
    ctx.fillRect(x, y, barW, Math.max(h, c > 0 ? 1 : 0));
    if (i === hoverIdx) {
      ctx.globalAlpha = 0.18;
      ctx.fillRect(x, pad.t, barW, plotH);
    }
  }
  ctx.globalAlpha = 1;

  // p95 / p99 markers at the bucket that contains each percentile.
  const markers = [
    { pct: 95, stroke: "rgba(20,32,28,0.55)", label: "p95" },
    { pct: 99, stroke: "rgba(185,28,28,0.75)", label: "p99" },
  ];
  const markerByIdx = new Map();
  for (const m of markers) {
    const p = histogramPercentile(hist, edges, m.pct);
    if (!p || p.index < 0 || p.index >= nBuckets) continue;
    const list = markerByIdx.get(p.index) || [];
    list.push(m);
    markerByIdx.set(p.index, list);
  }
  ctx.font = "9px IBM Plex Mono, monospace";
  ctx.textAlign = "center";
  ctx.textBaseline = "top";
  for (const [index, list] of markerByIdx) {
    const x = pad.l + index * (barW + gap) + barW / 2;
    for (let i = 0; i < list.length; i++) {
      const m = list[i];
      ctx.strokeStyle = m.stroke;
      ctx.lineWidth = 1.25;
      ctx.setLineDash([3, 3]);
      ctx.beginPath();
      ctx.moveTo(x, pad.t);
      ctx.lineTo(x, pad.t + plotH);
      ctx.stroke();
      ctx.setLineDash([]);
      ctx.fillStyle = m.stroke;
      ctx.fillText(m.label, x, pad.t + 1 + i * 11);
    }
  }
  ctx.textBaseline = "alphabetic";

  ctx.fillStyle = "#5c6b64";
  ctx.font = "10px IBM Plex Mono, monospace";
  ctx.textAlign = "right";
  ctx.fillText(String(maxC), pad.l - 4, pad.t + 8);
  ctx.fillText("0", pad.l - 4, pad.t + plotH);

  // A few x labels along the edges.
  ctx.textAlign = "center";
  const labelIdx = [0, Math.floor((edges.length - 1) / 2), edges.length - 1];
  const seen = new Set();
  for (const i of labelIdx) {
    if (i < 0 || i >= edges.length || seen.has(i)) continue;
    seen.add(i);
    const x = pad.l + i * (barW + gap) + barW / 2;
    ctx.fillText(formatLatencyUs(edges[i]), x, height - 8);
  }
  // Overflow bucket marker
  ctx.fillText(">", pad.l + (nBuckets - 1) * (barW + gap) + barW / 2, height - 8);
}

function drawCharts(history) {
  const spans = state.snapshot?.phase_spans || [];
  const cfg = state.snapshot?.config || {};
  const rawHistory = state.snapshot?.history || history;
  const bwFiles = estimateBWFileCount(rawHistory, spans);
  const smoothed = smoothHistory(history);
  drawLineChart($("chart-iops"), smoothed, [
    { key: "write_iops", color: "#0f7a5f" },
    { key: "read_iops", color: "#1f5fbf" },
    { key: "delete_iops", color: "#b45309" },
  ], false, spans, cfg, bwFiles);
  drawLineChart($("chart-bw"), smoothed, [
    { key: "write_bps", color: "#0f7a5f" },
    { key: "read_bps", color: "#1f5fbf" },
  ], true, spans, cfg, bwFiles);
}

function sampleTime(pt) {
  return new Date(pt.timestamp).getTime();
}

function phaseIdAtTime(spans, t) {
  for (const span of spans || []) {
    const start = new Date(span.start).getTime();
    const end = span.end ? new Date(span.end).getTime() : Date.now();
    if (t >= start && t <= end) return span.phase;
  }
  return "";
}

// Bandwidth phases create one ledger file per completed write; final delete can
// only remove those (plus rare leftovers), not the full configured delete rate.
function estimateBWFileCount(history, spans) {
  let n = 0;
  for (const pt of history || []) {
    const phase = phaseIdAtTime(spans, sampleTime(pt));
    if (phase === "write_bw" || phase === "read_write") {
      n += Number(pt.write_iops) || 0;
    }
  }
  return n;
}

function phaseRampSteps(phase) {
  if (phase === "delete" || phase === "final_delete") {
    return RAMP_PERCENTS.concat(100); // extra 100% sweep
  }
  if (
    phase === "create" ||
    phase === "write_bw" ||
    phase === "read_bw" ||
    phase === "read_write"
  ) {
    return RAMP_PERCENTS.slice();
  }
  return [];
}

function expectedRateForPhase(phase, pct, cfg, isBytes) {
  const f = (Number(pct) || 0) / 100;
  if (f <= 0) return 0;
  const createRate = Number(cfg.file_creation_rate) || 0;
  const deleteRate = Number(cfg.file_deletion_rate) || 0;
  const writeBW = Number(cfg.file_write_bandwidth) || 0;
  const readBW = Number(cfg.file_read_bandwidth) || 0;

  if (isBytes) {
    switch (phase) {
      case "create":
        return createRate * f * CREATE_FILE_SIZE;
      case "write_bw":
        return writeBW * f;
      case "read_bw":
        return readBW * f;
      case "read_write":
        return (writeBW + readBW) * f;
      default:
        return 0;
    }
  }

  switch (phase) {
    case "create":
      return createRate * f;
    case "delete":
      return deleteRate * f;
    case "write_bw":
      return (writeBW * f) / BW_FILE_SIZE;
    case "read_bw":
      return (readBW * f) / BW_FILE_SIZE;
    case "read_write":
      return ((writeBW + readBW) * f) / BW_FILE_SIZE;
    default:
      return 0;
  }
}

// Scale the final-delete ramp so its integral equals the BW files available,
// never exceeding the configured delete rate at each step.
function finalDeleteExpectedOps(t, span, cfg, bwFiles) {
  const deleteRate = Number(cfg.file_deletion_rate) || 0;
  if (bwFiles <= 0 || deleteRate <= 0) return 0;
  const stepMs = Math.max(1, (Number(cfg.phase_step_seconds) || 30) * 1000);
  const steps = phaseRampSteps("final_delete");
  const start = new Date(span.start).getTime();
  const idx = Math.min(steps.length - 1, Math.max(0, Math.floor((t - start) / stepMs)));
  const weights = steps.map((p) => p / 100);
  const totalW = weights.reduce((a, b) => a + b, 0) || 1;
  const stepSec = stepMs / 1000;
  const configured = deleteRate * weights[idx];
  const share = (bwFiles * weights[idx]) / totalW / stepSec;
  return Math.min(configured, share);
}

function expectedAtTime(t, spans, cfg, isBytes, bwFiles) {
  const stepMs = Math.max(1, (Number(cfg.phase_step_seconds) || 30) * 1000);
  for (const span of spans || []) {
    const start = new Date(span.start).getTime();
    const end = span.end ? new Date(span.end).getTime() : Date.now();
    if (!(t >= start && t <= end)) continue;
    if (span.phase === "final_delete") {
      return isBytes ? 0 : finalDeleteExpectedOps(t, span, cfg, bwFiles || 0);
    }
    const steps = phaseRampSteps(span.phase);
    if (!steps.length) return 0;
    const idx = Math.min(steps.length - 1, Math.max(0, Math.floor((t - start) / stepMs)));
    return expectedRateForPhase(span.phase, steps[idx], cfg, isBytes);
  }
  return 0;
}

function actualForPhase(phase, pt, isBytes) {
  if (isBytes) {
    switch (phase) {
      case "create":
      case "write_bw":
        return Number(pt.write_bps) || 0;
      case "read_bw":
        return Number(pt.read_bps) || 0;
      case "read_write":
        return (Number(pt.write_bps) || 0) + (Number(pt.read_bps) || 0);
      default:
        return 0;
    }
  }
  switch (phase) {
    case "create":
      return Number(pt.write_iops) || 0;
    case "delete":
    case "final_delete":
      return Number(pt.delete_iops) || 0;
    case "write_bw":
      return Number(pt.write_iops) || 0;
    case "read_bw":
      return Number(pt.read_iops) || 0;
    case "read_write":
      return (Number(pt.write_iops) || 0) + (Number(pt.read_iops) || 0);
    default:
      return 0;
  }
}

function phaseAttainment(span, history, cfg, isBytes, bwFiles) {
  if (!span?.end || !history?.length) return null;
  if (!phaseRampSteps(span.phase).length) return null;
  const start = new Date(span.start).getTime();
  const end = new Date(span.end).getTime();
  let act = 0;
  let exp = 0;
  for (const pt of history) {
    const t = sampleTime(pt);
    if (t < start || t > end) continue;
    const e = expectedAtTime(t, [span], cfg, isBytes, bwFiles);
    if (e <= 0) continue;
    act += actualForPhase(span.phase, pt, isBytes);
    exp += e;
  }
  if (exp <= 0) return null;
  return (100 * act) / exp;
}

function drawPhaseBands(ctx, spans, tMin, tMax, pad, plotW, plotH, history, cfg, isBytes, bwFiles) {
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

    // When the phase has finished, show attainment % (IO phases) or duration (software).
    if (span.end && w >= 28) {
      const att = phaseAttainment(span, history, cfg, isBytes, bwFiles);
      let endLabel = null;
      if (att != null) {
        endLabel = `${Math.round(att)}%`;
      } else if (
        span.phase === "software_unpack" ||
        span.phase === "software_cold" ||
        span.phase === "software_warm" ||
        span.phase === "git_clone" ||
        span.phase === "untar"
      ) {
        endLabel = formatPhaseDuration(end - start);
      }
      if (endLabel) {
        ctx.fillStyle = EXPECTED_COLOR;
        ctx.font = "600 10px IBM Plex Mono, monospace";
        ctx.textAlign = "right";
        ctx.textBaseline = "alphabetic";
        ctx.fillText(endLabel, Math.min(x1 - 3, pad.l + plotW - 2), pad.t + plotH - 4);
      }
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
  const expected = meta.expected?.[idx] ?? 0;
  const expRow = expected > 0
    ? `<div class="tt-row"><span class="tt-name" style="color:${EXPECTED_COLOR}">Expected</span><span class="tt-val">${formatSeriesValue(expected, meta.isBytes)}</span></div>`
    : "";
  tip.innerHTML = `<div class="tt-time">${timeStr}${phase ? " · " + phase : ""}</div>${rows}${expRow}`;
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

function drawLineChart(canvas, history, series, isBytes, spans, cfg, bwFiles) {
  bindChartHover(canvas);
  cfg = cfg || {};
  bwFiles = bwFiles || 0;

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

  const pad = { l: isBytes ? 48 : 54, r: 12, t: 26, b: 24 };
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

  const expected = history.map((pt) => expectedAtTime(sampleTime(pt), spans, cfg, isBytes, bwFiles));

  let maxY = 1;
  for (const pt of history) {
    for (const s of series) maxY = Math.max(maxY, Number(pt[s.key]) || 0);
  }
  for (const e of expected) maxY = Math.max(maxY, e || 0);
  maxY *= 1.15;

  canvas._chart = { history, series, isBytes, spans, expected, pad, tMin, tMax, maxY, plotW, plotH };

  drawPhaseBands(ctx, spans, tMin, tMax, pad, plotW, plotH, history, cfg, isBytes, bwFiles);

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
  ctx.font = "12px IBM Plex Mono, monospace";
  ctx.textAlign = "right";
  for (let i = 0; i <= 4; i++) {
    const v = maxY * (1 - i / 4);
    const y = pad.t + (plotH * i) / 4 + 3;
    // Full "7.50 GB/s" was clipping so "7." vanished and looked like "75 GB/s".
    const label = isBytes ? formatRateAxis(v) : Math.round(v).toString();
    ctx.fillText(label, pad.l - 6, y);
  }

  const xAt = (t) => pad.l + ((t - tMin) / (tMax - tMin)) * plotW;
  const yAt = (v) => pad.t + plotH - (v / maxY) * plotH;

  // Expected target (staircase ramp) as a grey line.
  ctx.beginPath();
  ctx.strokeStyle = EXPECTED_COLOR;
  ctx.lineWidth = 1.75;
  ctx.setLineDash([5, 4]);
  let started = false;
  history.forEach((pt, i) => {
    const e = expected[i] || 0;
    const x = xAt(sampleTime(pt));
    const y = yAt(e);
    if (!started) {
      ctx.moveTo(x, y);
      started = true;
    } else {
      ctx.lineTo(x, y);
    }
  });
  ctx.stroke();
  ctx.setLineDash([]);

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
    const exp = expected[idx] || 0;
    if (exp > 0) {
      ctx.fillStyle = EXPECTED_COLOR;
      ctx.beginPath();
      ctx.arc(x, yAt(exp), 3, 0, Math.PI * 2);
      ctx.fill();
      ctx.strokeStyle = "#fff";
      ctx.lineWidth = 1.25;
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
    package_url: form.package_url.value.trim(),
    startup_command: form.startup_command.value.trim(),
    git_clone_url: form.git_clone_url.value.trim(),
    untar_url: form.untar_url.value.trim(),
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

function reportBasename() {
  const snap = state.snapshot || {};
  const name = (snap.config?.test_name || "homebench")
    .replace(/[^a-zA-Z0-9._-]+/g, "_")
    .replace(/^_+|_+$/g, "") || "homebench";
  const stamp = new Date().toISOString().replace(/[:.]/g, "-").slice(0, 19);
  return `homebench-${name}-${stamp}`;
}

function downloadBlob(filename, blob) {
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = filename;
  document.body.appendChild(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(url), 2000);
}

function buildExportPayload() {
  const snap = state.snapshot;
  if (!snap) return null;
  return {
    exported_at: new Date().toISOString(),
    homebench_report: 1,
    ...snap,
  };
}

function downloadJSON() {
  const payload = buildExportPayload();
  if (!payload) {
    alert("No run data to export yet.");
    return;
  }
  const text = JSON.stringify(payload, null, 2);
  downloadBlob(
    `${reportBasename()}.json`,
    new Blob([text], { type: "application/json;charset=utf-8" })
  );
}

function canvasPNG(id) {
  const c = $(id);
  if (!c) return "";
  try {
    return c.toDataURL("image/png");
  } catch (_) {
    return "";
  }
}

function escapeHTML(s) {
  return String(s ?? "")
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;");
}

function latencySummaryHTML(snap) {
  const edges = snap.latency_edges_us || [];
  const lat = snap.latencies || {};
  const specs = [
    ["Create", lat.create],
    ["Delete", lat.delete],
    ["Write", lat.write],
    ["Read", lat.read],
    ["Software Startup Cold", lat.startup_cold],
    ["Software Startup Warm", lat.startup_warm],
    ["Git Clone", lat.git_clone],
    ["Untar", lat.untar],
  ];
  return specs.map(([title, hist], i) => {
    const imgId = [
      "hist-create",
      "hist-delete",
      "hist-write",
      "hist-read",
      "hist-startup-cold",
      "hist-startup-warm",
      "hist-git-clone",
      "hist-untar",
    ][i];
    const src = canvasPNG(imgId);
    const meta = formatLatencyMeta(hist, edges);
    const img = src
      ? `<img src="${src}" alt="${escapeHTML(title)} latency histogram" />`
      : "<p class='muted'>No chart</p>";
    return `<div class="hist">
      <h3>${escapeHTML(title)}</h3>
      ${img}
      <p class="meta">${escapeHTML(meta)}</p>
    </div>`;
  }).join("");
}

function downloadReport() {
  const snap = state.snapshot;
  if (!snap) {
    alert("No run data to export yet.");
    return;
  }

  // Clear hover overlays so captured charts are clean.
  state.hover = null;
  state.histHover = null;
  drawCharts(snap.history || []);
  drawLatencyHistograms(snap);

  const cfg = snap.config || {};
  const clients = snap.clients || [];
  const started = snap.started_at ? new Date(snap.started_at).toLocaleString() : "—";
  const title = `Homebench · ${cfg.test_name || "report"}`;
  const filenameHint = reportBasename();

  const clientRows = clients.length
    ? clients.map((c) => `<tr>
        <td>${escapeHTML(c.hostname)}</td>
        <td class="mono">${escapeHTML(c.prefix)}</td>
        <td>${escapeHTML(c.status)}</td>
        <td>${escapeHTML(PHASE_LABELS[c.phase] || c.phase || "")}</td>
      </tr>`).join("")
    : `<tr><td colspan="4">No clients</td></tr>`;

  const prefixes = (cfg.prefixes || []).map((p) => `<li class="mono">${escapeHTML(p)}</li>`).join("") || "<li>—</li>";

  const html = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8" />
  <title>${escapeHTML(filenameHint)}</title>
  <style>
    :root { color-scheme: light; }
    * { box-sizing: border-box; }
    body {
      margin: 0;
      padding: 28px 32px 48px;
      font: 14px/1.45 -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
      color: #14201c;
      background: #fff;
    }
    h1 { font-size: 1.6rem; margin: 0 0 4px; }
    h2 { font-size: 1.05rem; margin: 22px 0 10px; border-bottom: 1px solid #d7e0d9; padding-bottom: 6px; }
    h3 { font-size: 0.95rem; margin: 0 0 8px; }
    .sub { color: #5c6b64; margin: 0 0 18px; }
    .grid { display: grid; grid-template-columns: 1fr 1fr; gap: 10px 24px; }
    .kv { margin: 0; }
    .kv span { display: block; color: #5c6b64; font-size: 0.72rem; text-transform: uppercase; letter-spacing: 0.06em; }
    .kv strong { font-size: 0.98rem; }
    .mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; font-size: 0.86em; }
    ul { margin: 6px 0 0; padding-left: 18px; }
    table { width: 100%; border-collapse: collapse; font-size: 0.9rem; }
    th, td { text-align: left; padding: 7px 6px; border-bottom: 1px solid #e3ebe5; vertical-align: top; }
    th { color: #5c6b64; font-weight: 600; font-size: 0.75rem; text-transform: uppercase; letter-spacing: 0.05em; }
    img { width: 100%; height: auto; border: 1px solid #d7e0d9; border-radius: 8px; background: #f7faf7; }
    .chart { margin: 8px 0 4px; }
    .legend { color: #5c6b64; font-size: 0.8rem; margin: 4px 0 14px; }
    .hists { display: grid; grid-template-columns: 1fr 1fr; gap: 14px; }
    .hist .meta { color: #5c6b64; font-family: ui-monospace, Menlo, monospace; font-size: 0.75rem; margin: 6px 0 0; }
    .muted { color: #5c6b64; }
    @media print {
      body { padding: 12px; }
      h2 { break-after: avoid; }
      .chart, .hist, table { break-inside: avoid; }
    }
  </style>
</head>
<body>
  <h1>${escapeHTML(title)}</h1>
  <p class="sub">Exported ${escapeHTML(new Date().toLocaleString())}</p>

  <h2>Run summary</h2>
  <div class="grid">
    <p class="kv"><span>Status</span><strong>${escapeHTML(snap.status_text || "—")}</strong></p>
    <p class="kv"><span>Elapsed</span><strong class="mono">${escapeHTML(formatElapsed(snap.elapsed_sec))}</strong></p>
    <p class="kv"><span>Started</span><strong>${escapeHTML(started)}</strong></p>
    <p class="kv"><span>Clients</span><strong class="mono">${escapeHTML(String(snap.client_count ?? clients.length))}</strong></p>
    <p class="kv"><span>Phase</span><strong>${escapeHTML(PHASE_LABELS[snap.phase] || snap.phase || "—")}${snap.percent ? ` · ${snap.percent}%` : ""}</strong></p>
    <p class="kv"><span>Running</span><strong>${snap.running ? "yes" : "no"}</strong></p>
  </div>

  <h2>Configuration</h2>
  <div class="grid">
    <p class="kv"><span>Test name</span><strong>${escapeHTML(cfg.test_name || "—")}</strong></p>
    <p class="kv"><span>Phase step</span><strong class="mono">${escapeHTML(String(cfg.phase_step_seconds ?? "—"))}s</strong></p>
    <p class="kv"><span>Create rate</span><strong class="mono">${escapeHTML(String(cfg.file_creation_rate ?? "—"))} files/s</strong></p>
    <p class="kv"><span>Delete rate</span><strong class="mono">${escapeHTML(String(cfg.file_deletion_rate ?? "—"))} files/s</strong></p>
    <p class="kv"><span>Write bandwidth</span><strong class="mono">${escapeHTML(((cfg.file_write_bandwidth || 0) / MiB).toFixed(1))} MiB/s</strong></p>
    <p class="kv"><span>Read bandwidth</span><strong class="mono">${escapeHTML(((cfg.file_read_bandwidth || 0) / MiB).toFixed(1))} MiB/s</strong></p>
    <p class="kv"><span>Package URL</span><strong class="mono">${escapeHTML(cfg.package_url || "—")}</strong></p>
    <p class="kv"><span>Startup command</span><strong class="mono">${escapeHTML(cfg.startup_command || "—")}</strong></p>
    <p class="kv"><span>GIT Clone</span><strong class="mono">${escapeHTML(cfg.git_clone_url || "—")}</strong></p>
    <p class="kv"><span>UNTAR</span><strong class="mono">${escapeHTML(cfg.untar_url || "—")}</strong></p>
  </div>
  <p class="kv" style="margin-top:12px"><span>Prefixes</span></p>
  <ul>${prefixes}</ul>

  <h2>Clients</h2>
  <table>
    <thead><tr><th>Hostname</th><th>Prefix</th><th>Status</th><th>Phase</th></tr></thead>
    <tbody>${clientRows}</tbody>
  </table>

  <h2>IOPS</h2>
  <div class="chart"><img src="${canvasPNG("chart-iops")}" alt="IOPS chart" /></div>
  <p class="legend">Write/Create · Read · Delete · 5s moving average</p>

  <h2>Bandwidth</h2>
  <div class="chart"><img src="${canvasPNG("chart-bw")}" alt="Bandwidth chart" /></div>
  <p class="legend">Write · Read · 5s moving average</p>

  <h2>Operation latency</h2>
  <div class="hists">${latencySummaryHTML(snap)}</div>
</body>
</html>`;

  const w = window.open("", "_blank");
  if (!w) {
    alert("Pop-up blocked. Allow pop-ups for this site to download the report.");
    return;
  }
  w.document.open();
  w.document.write(html);
  w.document.close();
  // Give images a tick to decode, then open the print dialog (Save as PDF).
  w.focus();
  setTimeout(() => {
    try {
      w.print();
    } catch (_) {}
  }, 250);
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
  $("btn-download-json").addEventListener("click", downloadJSON);
  $("btn-download-report").addEventListener("click", downloadReport);
  window.addEventListener("resize", () => {
    if (!state.snapshot) return;
    drawCharts(state.snapshot.history || []);
    drawLatencyHistograms(state.snapshot);
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
