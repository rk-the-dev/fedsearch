"use strict";
// FedSearch console. Every value from telemetry is attacker-controllable, so
// all dynamic text goes through esc() or textContent — never raw innerHTML.

const $ = (s, el = document) => el.querySelector(s);
const esc = (v) => String(v ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
const state = { catalog: null, plan: null, job: null, es: null, ctxMode: "as_of" };

async function api(method, path, body) {
  const res = await fetch(path, { method, headers: body ? { "Content-Type": "application/json" } : {}, body: body ? JSON.stringify(body) : undefined });
  const data = await res.json().catch(() => ({}));
  if (!res.ok) { const e = new Error(data.error || res.statusText); e.data = data; e.status = res.status; throw e; }
  return data;
}

// ---------- formatting ----------
const fmtBytes = (b) => {
  if (b == null) return "–";
  const u = ["B", "KB", "MB", "GB", "TB"]; let i = 0; let n = Number(b);
  while (n >= 1000 && i < u.length - 1) { n /= 1000; i++; }
  return `${n >= 100 || i === 0 ? n.toFixed(0) : n.toFixed(1)} ${u[i]}`;
};
const fmtUSD = (v) => !v ? "$0" : v < 0.00001 ? "<$0.00001" : `$${v.toFixed(v < 0.01 ? 5 : 3)}`;
const fmtInt = (n) => Number(n).toLocaleString("en-US");
const fmtMs = (ns) => ns == null ? "–" : ns / 1e6 < 1000 ? `${Math.round(ns / 1e6)} ms` : `${(ns / 1e9).toFixed(2)} s`;
const day = (t) => new Date(t).toISOString().slice(0, 10);
const when = (t) => new Date(t).toISOString().replace("T", " ").slice(0, 19);
const tierOf = (loc) => (loc || "").startsWith("hot") ? "hot" : "cold";
const days = (r) => Math.round((new Date(r.to) - new Date(r.from)) / 864e5);

// ---------- tabs ----------
document.querySelectorAll(".tabs button").forEach((b) => b.addEventListener("click", () => show(b.dataset.view)));
function show(view) {
  document.querySelectorAll(".tabs button").forEach((b) => b.setAttribute("aria-selected", String(b.dataset.view === view)));
  document.querySelectorAll(".view").forEach((v) => (v.hidden = v.id !== `view-${view}`));
  if (view === "audit") loadAudit();
  history.replaceState(null, "", `#${view}`);
}

// ---------- tier strip (SVG) ----------
// lanes: [{label, sub, segments:[{from,to,cls}], ticks:[{at,cls}]}], bands:[{from,to}]
function strip({ from, to, lanes, bands = [], now }) {
  const t0 = new Date(from).getTime(), t1 = new Date(to).getTime();
  const x = (t) => Math.max(0, Math.min(1000, ((new Date(t).getTime() - t0) / (t1 - t0)) * 1000));
  const defs = `<defs><pattern id="hatch" patternUnits="userSpaceOnUse" width="7" height="7" patternTransform="rotate(45)"><rect width="2.2" height="7" fill="currentColor" opacity=".45"/></pattern></defs>`;
  let html = `<div class="strip">`;
  for (const lane of lanes) {
    let svg = `<svg viewBox="0 0 1000 30" preserveAspectRatio="none" role="img" aria-label="${esc(lane.label)} coverage">${defs}<rect class="lane-bg" x="0" y="4" width="1000" height="22" rx="3"/>`;
    for (const s of lane.segments) svg += `<rect class="${s.cls}" x="${x(s.from)}" y="4" width="${Math.max(1.5, x(s.to) - x(s.from))}" height="22" rx="2"><title>${esc(s.title || "")}</title></rect>`;
    for (const t of lane.ticks || []) svg += `<line class="${t.cls}" x1="${x(t.at)}" x2="${x(t.at)}" y1="6" y2="24"/>`;
    for (const b of bands) svg += `<rect class="overlap-band" style="color:var(--ink)" x="${x(b.from)}" y="4" width="${x(b.to) - x(b.from)}" height="22"/>`;
    if (now) svg += `<line class="now-line" x1="${x(now)}" x2="${x(now)}" y1="0" y2="30"/>`;
    svg += `</svg>`;
    html += `<div class="lane-label">${esc(lane.label)}<small>${esc(lane.sub || "")}</small></div><div>${svg}</div>`;
  }
  // axis: monthly ticks
  let axis = `<svg viewBox="0 0 1000 22" preserveAspectRatio="none">`;
  const d = new Date(t0); d.setUTCDate(1); d.setUTCHours(0, 0, 0, 0); d.setUTCMonth(d.getUTCMonth() + 1);
  const spanDays = (t1 - t0) / 864e5;
  const stepMonths = spanDays > 200 ? 2 : 1;
  if (spanDays < 45) {
    const s = new Date(t0); s.setUTCHours(0, 0, 0, 0); s.setUTCDate(s.getUTCDate() + 1);
    const step = spanDays > 14 ? 7 : spanDays > 4 ? 2 : 1;
    for (; s.getTime() < t1; s.setUTCDate(s.getUTCDate() + step)) axis += `<text x="${x(s)}" y="15" text-anchor="middle">${s.toISOString().slice(5, 10)}</text>`;
  } else {
    for (; d.getTime() < t1; d.setUTCMonth(d.getUTCMonth() + stepMonths)) axis += `<text x="${x(d)}" y="15" text-anchor="middle">${d.toLocaleString("en-US", { month: "short", timeZone: "UTC" })} ${d.getUTCFullYear() % 100}</text>`;
  }
  axis += `</svg>`;
  html += `<div></div><div class="axis">${axis}</div></div>`;
  return html;
}

// ---------- status bar ----------
function renderStatus() {
  const c = state.catalog; if (!c) return;
  $("#status").innerHTML = `<span class="pill">${esc(c.mode)} mode</span><span>Catalog refreshed ${esc(when(c.refreshed_at))} UTC</span><span class="pill">${c.llm_available ? "LLM connected" : "LLM offline: golden answers"}</span>`;
  $("#mode-agent").disabled = !c.llm_available;
}

// ---------- catalog view ----------
async function loadCatalog() {
  state.catalog = await api("GET", "/api/catalog");
  renderStatus();
  renderCatalog();
}

function renderCatalog() {
  const c = state.catalog;
  const names = Object.keys(c.datasets).sort();
  let html = "";
  for (const name of names) {
    const ds = c.datasets[name];
    const locs = c.locations.filter((l) => l.dataset === name).sort((a, b) => a.preference - b.preference);
    if (!locs.length) continue;
    const from = locs.reduce((m, l) => (l.coverage.from < m ? l.coverage.from : m), locs[0].coverage.from);
    const to = c.now;
    const overlaps = c.overlaps[name] || [];
    const lanes = locs.map((l) => {
      const eff = c.effective[l.id];
      const tier = l.tier;
      return {
        label: `${tier === "hot" ? "Hot" : "Cold"} tier`,
        sub: `${l.capabilities.engine}, ${l.partitions.length} partitions`,
        segments: [{ from: eff.from, to: eff.to, cls: `seg-${tier}`, title: `${l.id}: ${day(eff.from)} to ${day(eff.to)}` }],
        ticks: l.partitions.map((p) => ({ at: p.span.from, cls: "tick" })),
      };
    });
    const ovText = overlaps.map((o) => `${days(o.range)} days (${day(o.range.from)} to ${day(o.range.to)}) are stored in both tiers. The planner gives that window to the hot tier only, so nothing is counted twice.`).join(" ");
    html += `<article class="dataset">
      <div class="dataset-head"><h2>${esc(ds.class_name)}</h2><span class="meta">OCSF class ${ds.class_uid}, dataset <code>${esc(name)}</code></span></div>
      ${strip({ from, to, lanes, bands: overlaps.map((o) => o.range), now: c.now })}
      <div class="legend"><span><i class="l-cold"></i>Cold: object storage, Parquet</span><span><i class="l-hot"></i>Hot: SIEM tier</span><span><i class="l-overlap"></i>Held by both tiers</span></div>
      ${overlaps.length ? `<p class="note">${esc(ovText)}</p>` : ""}
      ${locationsTable(locs)}
      ${fieldsTable(ds, locs)}
    </article>`;
  }
  $("#catalog").innerHTML = html;
}

function locationsTable(locs) {
  const rows = locs.map((l) => `<tr>
    <td class="tier-${esc(l.tier)}"><b>${esc(l.id)}</b><br><small class="mono">${esc(l.layout.pattern)}</small></td>
    <td>${esc(l.capabilities.engine)}<br><small>${esc(l.capabilities.dialect)}</small></td>
    <td>${esc(day(l.coverage.from))} to ${l.coverage.live ? "now (live)" : esc(day(l.coverage.to))}</td>
    <td class="num">${fmtInt(l.partitions.length)}</td>
    <td class="num">${fmtInt(l.rows)}</td>
    <td class="num">${fmtBytes(l.bytes)}</td>
    <td>${esc(l.cost.kind.replaceAll("_", " "))}${l.cost.rate ? ` at $${l.cost.rate}` : ""}</td>
    <td>${(l.capabilities.unsupported_op || []).length ? `cannot evaluate: ${esc(l.capabilities.unsupported_op.join(", "))}` : "all operators"}</td>
  </tr>`).join("");
  return `<div class="tablewrap"><table><thead><tr><th>Location</th><th>Engine</th><th>Coverage</th><th class="num">Partitions</th><th class="num">Rows</th><th class="num">Size</th><th>Billing</th><th>Pushdown</th></tr></thead><tbody>${rows}</tbody></table></div>`;
}

function fieldsTable(ds, locs) {
  const rows = ds.fields.map((f) => {
    const phys = locs.map((l) => {
      const b = l.bindings[f.path];
      if (!b) return `<td class="withheld">not stored</td>`;
      const enumNote = b.enum ? ` <small>(captions mapped to ids)</small>` : "";
      const cap = b.searchable ? "" : ` <small>not searchable</small>`;
      return `<td class="mono">${esc(b.physical)}${enumNote}${cap}</td>`;
    }).join("");
    let vals = "";
    if (f.enum) vals = Object.entries(f.enum).map(([k, v]) => `${k} ${v}`).join(", ");
    else if (f.stats?.top_values?.length) vals = f.stats.top_values.join(", ");
    else if (f.stats?.examples?.length) vals = f.stats.examples.map((e) => e.suspicious ? "[withheld]" : e.value).join(", ");
    return `<tr><td class="mono">${esc(f.path)}</td><td>${esc(f.type)}</td>${phys}<td>${esc(vals)}${f.stats ? ` <small>(${fmtInt(f.stats.distinct)} distinct in sample)</small>` : ""}</td></tr>`;
  }).join("");
  const heads = locs.map((l) => `<th>${esc(l.tier)} column</th>`).join("");
  return `<details><summary>${ds.fields.length} OCSF fields and their physical columns</summary><div class="tablewrap"><table><thead><tr><th>OCSF path</th><th>Type</th>${heads}<th>Values</th></tr></thead><tbody>${rows}</tbody></table></div></details>`;
}

$("#refresh").addEventListener("click", async (e) => {
  e.target.disabled = true; e.target.textContent = "Refreshing…";
  try {
    const rep = await api("POST", "/api/catalog/refresh");
    const lines = rep.locations.map((l) => {
      const pct = l.file_bytes ? ((100 * l.metadata_bytes) / l.file_bytes).toFixed(2) : null;
      return `<li><b>${esc(l.location)}</b>: ${l.partitions_read} of ${l.partitions} partitions read${l.files_read ? `, ${fmtBytes(l.metadata_bytes)} of metadata from ${fmtBytes(l.file_bytes)} of files (${pct}%)` : " (all unchanged, served from cache)"}</li>`;
    }).join("");
    $("#refresh-report").innerHTML = `<div class="note ok">Refreshed in ${fmtMs(rep.duration_ns)}.<ul>${lines}</ul></div>`;
    await loadCatalog();
  } catch (err) {
    $("#refresh-report").innerHTML = `<div class="note crit">Refresh failed: ${esc(err.message)}</div>`;
  } finally { e.target.disabled = false; e.target.textContent = "Refresh catalog"; }
});

// ---------- query view ----------
async function loadQuestions() {
  const qs = await api("GET", "/api/questions");
  const demo = qs.filter((q) => q.demo), rest = qs.filter((q) => !q.demo);
  $("#suggestions").innerHTML = [...demo, ...rest.slice(0, 4)].map((q) => `<button class="chip ${q.demo ? "demo" : ""}" data-q="${esc(q.question)}">${esc(q.question)}</button>`).join("");
  document.querySelectorAll("#suggestions .chip").forEach((b) => b.addEventListener("click", () => { $("#question").value = b.dataset.q; translate(); }));
}

async function translate() {
  const question = $("#question").value.trim();
  if (!question) return;
  const btn = $("#translate"); btn.disabled = true; btn.textContent = "Translating…";
  $("#ir-errors").innerHTML = ""; $("#nl-attempts").innerHTML = "";
  try {
    const tr = await api("POST", "/api/nl", { question });
    $("#ir").value = JSON.stringify(irForEditing(tr.ir), null, 2);
    const src = $("#nl-source"); src.hidden = false;
    src.textContent = tr.source === "llm" ? `Translated by the LLM in ${tr.latency_ms} ms` : "Answered from the golden set";
    src.className = `badge ${tr.source === "llm" ? "llm" : ""}`;
    $("#nl-explain").hidden = false; $("#nl-explain").textContent = tr.explanation;
    renderAttempts(tr.attempts);
    await plan();
  } catch (err) {
    $("#ir-errors").innerHTML = `<div class="note crit">${esc(err.message)}</div>`;
    renderAttempts(err.data?.attempts);
  } finally { btn.disabled = false; btn.textContent = "Translate"; }
}

function renderAttempts(attempts) {
  const repaired = (attempts || []).filter((a) => a.problems?.length);
  if (!repaired.length) return;
  $("#nl-attempts").innerHTML = `<div class="note warn">The first draft failed validation and was repaired:<ul>${repaired.flatMap((a) => a.problems).map((p) => `<li>${esc(p)}</li>`).join("")}</ul></div>`;
}

// Show relative times as written; the server resolves them.
function irForEditing(q) {
  const out = { ...q };
  delete out.v;
  return out;
}

$("#translate").addEventListener("click", translate);
$("#question").addEventListener("keydown", (e) => { if (e.key === "Enter") translate(); });
$("#plan").addEventListener("click", plan);
$("#naive").addEventListener("change", plan);

function readIR() {
  try { return JSON.parse($("#ir").value); } catch (e) { throw new Error(`The query is not valid JSON: ${e.message}`); }
}

async function plan() {
  $("#ir-errors").innerHTML = "";
  let query;
  try { query = readIR(); } catch (e) { $("#ir-errors").innerHTML = `<div class="note crit">${esc(e.message)}</div>`; return; }
  try {
    const res = await api("POST", "/api/plan", { query, naive: $("#naive").checked });
    state.plan = res;
    renderPlan(res);
  } catch (err) {
    const probs = err.data?.problems;
    $("#ir-errors").innerHTML = `<div class="note crit">${esc(err.message)}${probs ? `<ul>${probs.map((p) => `<li>${esc(p)}</li>`).join("")}</ul>` : ""}</div>`;
    $("#plan-out").innerHTML = `<p class="empty">Fix the query to see a plan.</p>`;
  }
}

function renderPlan({ plan, decision }) {
  const q = plan.query;
  const lanes = [{
    label: "Query range", sub: `${days(q.time)} days`,
    segments: [
      ...plan.slices.map((s) => ({ from: s.time.from, to: s.time.to, cls: `seg-${s.tier}`, title: `${s.id} ${s.location}` })),
      ...(plan.gaps || []).map((g) => ({ from: g.from, to: g.to, cls: "seg-gap", title: "no location covers this window" })),
    ],
  }];
  for (const s of plan.slices) {
    lanes.push({
      label: `${s.id}: ${s.tier} tier`, sub: `${s.partitions.length} of ${s.partitions_total} partitions read`,
      segments: [{ from: s.time.from, to: s.time.to, cls: `seg-${s.tier}-soft` }],
      ticks: s.partitions.map((p) => ({ at: p.span.from, cls: "tick-pruned" })),
    });
  }
  const slices = plan.slices.map((s) => `
    <div class="slice ${esc(s.tier)}">
      <h3>${esc(s.id)} <span>${esc(s.location)}</span></h3>
      <span class="num">${fmtUSD(s.estimate.usd)}</span>
      <div class="when">${esc(when(s.time.from))} to ${esc(when(s.time.to))} UTC</div>
      <div class="facts">
        <span><b>${s.partitions.length}</b> of ${s.partitions_total} partitions</span>
        <span>est. <b>${fmtBytes(s.estimate.bytes)}</b> scanned, ${fmtInt(s.estimate.rows)} rows</span>
        <span>${s.residual ? `<b>filtered after fetch</b> (engine cannot evaluate part of the filter)` : "all filters pushed down"}</span>
      </div>
      <details><summary>${s.dialect === "duckdb_sql" ? "SQL sent to DuckDB" : "Query DSL sent to OpenSearch"}</summary><pre class="native">${esc(s.native)}</pre>
      <p class="explain">${esc(s.estimate.method || "")}</p></details>
    </div>`).join("");
  const warns = (plan.warnings || []).length ? `<div class="note warn"><ul>${plan.warnings.map((w) => `<li>${esc(w)}</li>`).join("")}</ul></div>` : "";
  const verdict = !decision.allowed ? `<div class="note crit">${esc(decision.reason)}</div>`
    : decision.needs_confirm ? `<div class="note warn">${esc(decision.reason)}. You will be asked to confirm.</div>` : "";
  const bands = [];
  for (let i = 0; i < plan.slices.length; i++) for (let k = i + 1; k < plan.slices.length; k++) {
    const a = plan.slices[i].time, b = plan.slices[k].time;
    const f = a.from > b.from ? a.from : b.from, t = a.to < b.to ? a.to : b.to;
    if (f < t) bands.push({ from: f, to: t });
  }
  $("#plan-out").innerHTML = `
    ${strip({ from: q.time.from, to: q.time.to, lanes, bands })}
    ${bands.length ? `<div class="note crit">Slices overlap in time: events in the hatched window will be read twice. Rows are deduplicated by event identity; aggregate counts cannot be.</div>` : ""}
    <div class="legend"><span><i class="l-cold"></i>Cold slice</span><span><i class="l-hot"></i>Hot slice</span>${plan.gaps?.length ? `<span><i class="l-gap"></i>Not covered</span>` : ""}</div>
    <div class="slices">${slices || `<p class="empty">No tier covers this time range.</p>`}</div>
    ${warns}${verdict}
    <div class="cost-total"><div><span class="big">${fmtUSD(plan.total.usd)}</span> <span class="explain">estimated, ${fmtBytes(plan.total.bytes)} scanned</span></div>
    <button class="run" id="run" ${decision.allowed ? "" : "disabled"}>Run query</button></div>`;
  $("#run").addEventListener("click", run);
}

// ---------- execution + SSE ----------
async function run() {
  let query;
  try { query = readIR(); } catch (e) { return; }
  if (state.es) state.es.close();
  $("#results").innerHTML = `<p class="spinner">Starting…</p>`;
  try {
    const job = await api("POST", "/api/jobs", { query, naive: $("#naive").checked });
    state.job = { id: job.id, plan: job.plan, slices: {}, rows: 0, done: false, t0: performance.now() };
    renderResultsShell();
    const es = new EventSource(`/api/jobs/${encodeURIComponent(job.id)}/events`);
    state.es = es;
    es.addEventListener("state", (e) => onState(JSON.parse(e.data)));
    es.addEventListener("slice", (e) => onSlice(JSON.parse(e.data)));
    es.addEventListener("rows", (e) => onRows(JSON.parse(e.data)));
    es.addEventListener("result", (e) => onResult(JSON.parse(e.data)));
    es.addEventListener("done", () => es.close());
  } catch (err) {
    $("#results").innerHTML = `<div class="note crit">${esc(err.message)}</div>`;
  }
}

function renderResultsShell() {
  const j = state.job;
  $("#results").innerHTML = `<section class="panel results">
    <div class="panel-head"><h2>Results</h2><span class="badge" id="job-state">running</span></div>
    <div id="confirm"></div>
    <div class="live" id="live">${j.plan.slices.map((s) => `<div class="sstat ${esc(s.tier)}" id="ss-${esc(s.id)}"><span class="dot"></span>${esc(s.id)} ${esc(s.tier)}: queued</div>`).join("")}</div>
    <div id="result-body"><p class="spinner">Waiting for the first slice…</p></div>
  </section>`;
}

function onState(s) {
  $("#job-state").textContent = s.state.replaceAll("_", " ");
  if (s.state === "awaiting_confirmation") {
    $("#confirm").innerHTML = `<div class="note warn">${esc(s.reason)}. <button id="ok">Run anyway</button> <button class="danger" id="no">Cancel</button></div>`;
    $("#ok").onclick = () => api("POST", `/api/jobs/${state.job.id}/confirm`).then(() => ($("#confirm").innerHTML = ""));
    $("#no").onclick = () => api("POST", `/api/jobs/${state.job.id}/confirm?reject=1`).then(() => ($("#confirm").innerHTML = ""));
  }
}

function onSlice(s) {
  const el = $(`#ss-${CSS.escape(s.id)}`); if (!el) return;
  const extra = s.state === "done" ? `${fmtInt(s.rows)} rows, ${fmtBytes(s.bytes)}, ${fmtMs(s.latency_ns)}`
    : s.state === "running" ? (s.rows ? `${fmtInt(s.rows)} rows so far` : "running") : `${s.state}${s.error ? `: ${s.error}` : ""}`;
  el.innerHTML = `<span class="dot ${esc(s.state)}"></span>${esc(s.id)} ${esc(s.tier)}: ${esc(extra)}`;
}

function onRows(r) {
  const body = $("#result-body");
  if (body.dataset.final) return;
  state.job.rows += r.rows.length;
  body.innerHTML = `<p class="spinner">${fmtInt(state.job.rows)} rows received so far; merging when every slice finishes…</p>`;
}

function onResult(res) {
  state.job.result = res;
  $("#job-state").textContent = res.status;
  const body = $("#result-body"); body.dataset.final = "1";
  const q = state.job.plan.query;
  const kpis = [
    `<div class="kpi"><span class="v">${fmtInt(res.rows ? res.rows.length : (res.groups || []).length)}</span><span class="k">${res.groups ? "groups" : "rows"}</span></div>`,
    `<div class="kpi"><span class="v">${fmtBytes(res.bytes_scanned)}</span><span class="k">scanned</span></div>`,
    `<div class="kpi"><span class="v">${fmtUSD(res.usd)}</span><span class="k">actual cost</span></div>`,
    `<div class="kpi"><span class="v">${fmtMs(res.elapsed_ns)}</span><span class="k">elapsed</span></div>`,
  ];
  if (!res.groups) kpis.push(`<div class="kpi ${res.merge.duplicates ? "alert" : ""}"><span class="v">${fmtInt(res.merge.duplicates)}</span><span class="k">duplicates removed</span></div>`);
  let html = `<div class="kpis">${kpis.join("")}</div>`;
  if (res.status !== "completed") html += `<div class="note crit">${esc(res.status)}: ${esc(res.reason)}</div>`;
  if (res.groups && (state.job.plan.warnings || []).some((w) => w.startsWith("naive mode"))) html += `<div class="note crit">Naive mode: both tiers counted the overlap window, so these numbers are inflated. Turn naive mode off to get the correct answer.</div>`;
  if (res.merge?.duplicates) html += `<div class="note warn">${fmtInt(res.merge.duplicates)} events arrived from both tiers and were dropped by event identity. Row results survive overlap; aggregate counts would not, which is why the planner normally gives the overlap to one tier.</div>`;
  if (res.flagged_values) html += `<div class="note crit">${res.flagged_values} value(s) contain text addressed to an AI analyst (prompt injection). Shown to you here; withheld from agents.</div>`;
  if (res.may_be_inexact) html += `<div class="note warn">A slice hit its group cap; the top groups may be inexact.</div>`;
  html += res.groups ? groupsTable(q, res.groups) : rowsTable(q, res);
  body.innerHTML = html;
  const t = $("#ctx-toggle");
  if (t) t.querySelectorAll("button").forEach((b) => b.addEventListener("click", () => { state.ctxMode = b.dataset.mode; onResult(state.job.result); }));
}

// Enum ids render with their OCSF caption, e.g. "2 Failure".
function enumCaption(path, v) {
  const ds = state.job?.plan?.query?.dataset;
  const f = ds && state.catalog?.datasets[ds]?.fields.find((x) => x.path === path);
  return f?.enum && f.enum[v] ? `${v} ${f.enum[v]}` : null;
}

function cell(v, path) {
  if (v == null) return "";
  const cap = path && enumCaption(path, v);
  if (cap) return esc(cap);
  if (typeof v === "object") return esc(JSON.stringify(v));
  return esc(v);
}

function rowsTable(q, res) {
  if (!res.rows?.length) return `<p class="empty">No events match.</p>`;
  const cols = (q.select || []).filter((c) => c !== "time" && c !== "metadata.uid");
  const enr = res.enriched_fields || [];
  const changed = res.rows.filter((r) => enr.some((f) => r.context?.[f]?.changed)).length;
  const toggle = enr.length ? `<div class="row"><div class="toggle" id="ctx-toggle" role="group" aria-label="Device attribution">
      <button data-mode="as_of" aria-pressed="${state.ctxMode === "as_of"}">Device at event time</button>
      <button data-mode="current" aria-pressed="${state.ctxMode === "current"}">Current owner (naive)</button></div>
      ${changed ? `<span class="explain">${changed} row(s) where the IP has changed hands since the event.</span>` : ""}</div>` : "";
  const head = `<th>Time (UTC)</th>${cols.map((c) => `<th>${esc(c)}</th>`).join("")}${enr.map((f) => `<th>Device for ${esc(f)}</th>`).join("")}<th>Source</th>`;
  const body = res.rows.slice(0, 1000).map((r) => {
    const flagged = (r.flags || []).length > 0;
    const isChanged = enr.some((f) => r.context?.[f]?.changed);
    const tds = cols.map((c) => {
      const v = r.fields[c];
      const isFlag = (r.flags || []).includes(`suspicious:${c}`);
      return `<td class="${isFlag ? "withheld" : ""}">${cell(v, c)}</td>`;
    }).join("");
    const ctx = enr.map((f) => {
      const c = r.context?.[f];
      if (!c) return `<td><small class="explain">external</small></td>`;
      const h = state.ctxMode === "as_of" ? c.as_of : c.current;
      return h ? `<td><div class="ctx"><b>${esc(h.hostname)}</b><small>${esc(h.owner)}, ${esc(h.department)}</small></div></td>` : `<td>–</td>`;
    }).join("");
    return `<tr class="${flagged ? "flagged" : isChanged ? "changed" : ""}"><td class="tier-${tierOf(r.location)} time" title="${esc(r.location)} / ${esc(r.id)}">${esc(when(r.time))}</td>${tds}${ctx}<td><small class="mono" title="event ${esc(r.id)}">${esc(r.location)}</small></td></tr>`;
  }).join("");
  return `${toggle}<div class="tablewrap"><table><thead><tr>${head}</tr></thead><tbody>${body}</tbody></table></div>`;
}

function groupsTable(q, groups) {
  if (!groups.length) return `<p class="empty">No groups match.</p>`;
  const gcols = q.group_by || [];
  const aliases = q.aggs.map((a) => a.as);
  const first = aliases[0];
  const max = Math.max(...groups.map((g) => Number(g.values[first]) || 0), 1);
  const head = `${gcols.map((c) => `<th>${esc(c)}</th>`).join("")}${aliases.map((a) => `<th class="num">${esc(a)}</th>`).join("")}<th></th>`;
  const body = groups.map((g) => `<tr>${gcols.map((c) => `<td>${cell(g.values[c])}</td>`).join("")}${aliases.map((a) => {
    const v = g.values[a];
    return `<td class="num">${typeof v === "number" ? (a.includes("byte") ? fmtBytes(v) : fmtInt(Math.round(v * 100) / 100)) : cell(v)}${g.approx ? " ≈" : ""}</td>`;
  }).join("")}<td style="width:30%"><div class="bar" style="width:${(100 * (Number(g.values[first]) || 0)) / max}%"></div></td></tr>`).join("");
  return `<div class="tablewrap"><table><thead><tr>${head}</tr></thead><tbody>${body}</tbody></table></div>`;
}

// ---------- investigation ----------
$("#inv-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const mode = document.querySelector('input[name="mode"]:checked').value;
  const out = $("#inv-out");
  out.innerHTML = `<p class="spinner">Investigating ${esc($("#ioc").value)}${mode === "agent" ? " with the LLM agent (this takes a minute)" : ""}…</p>`;
  try {
    const rep = await api("POST", "/api/investigate", { indicator: $("#ioc").value.trim(), window: $("#window").value, mode });
    out.innerHTML = renderReport(rep);
  } catch (err) {
    out.innerHTML = `<div class="note crit">${esc(err.message)}</div>`;
  }
});

function renderReport(rep) {
  const findings = (rep.findings || []).map((f) => `<div class="note ${/injection|Attribution trap/.test(f) ? "warn" : "crit"}">${esc(f)}</div>`).join("");
  const tl = (rep.timeline || []).map((e) => `<li class="${esc(e.severity)}">
      <span class="t">${esc(when(e.start))}</span><span class="rail"></span>
      <div class="body"><span class="stage">${esc(e.stage)}</span><span>${esc(e.summary)}</span>
      <div class="cites">${(e.citations || []).map((c) => `<span class="cite" title="job ${esc(c.job)}">${esc(c.location)}/${esc(c.event_id)}</span>`).join("")}</div></div></li>`).join("");
  const steps = (rep.steps || []).map((s, i) => `<tr><td class="num">${i + 1}</td><td>${esc(s.title)}</td><td>${esc((s.tiers || []).join(" + "))}</td><td class="num">${fmtInt(s.rows || 0)}</td><td class="num">${fmtUSD(s.usd || 0)}</td><td class="num">${fmtMs(s.duration_ns)}</td><td class="mono">${esc(s.job || "")}</td><td>${esc(s.summary || "")}</td></tr>`).join("");
  return `
    <div class="kpis">
      <div class="kpi"><span class="v">${(rep.timeline || []).length || "–"}</span><span class="k">timeline entries</span></div>
      <div class="kpi"><span class="v">${(rep.steps || []).length}</span><span class="k">governed queries</span></div>
      <div class="kpi"><span class="v">${fmtUSD(rep.usd || 0)}</span><span class="k">total cost</span></div>
      <div class="kpi"><span class="v">${fmtMs(rep.elapsed_ns)}</span><span class="k">elapsed</span></div>
    </div>
    ${findings ? `<section class="findings"><h2>Findings</h2>${findings}</section>` : ""}
    ${tl ? `<section class="panel"><h2>Attack timeline</h2><ol class="timeline">${tl}</ol></section>` : ""}
    ${rep.narrative ? `<section class="panel"><h2>Agent report</h2><div class="narrative">${esc(rep.narrative)}</div></section>` : ""}
    <details><summary>Every step, as audited</summary><div class="tablewrap"><table><thead><tr><th class="num">#</th><th>Step</th><th>Tiers</th><th class="num">Rows</th><th class="num">Cost</th><th class="num">Time</th><th>Job</th><th>Result</th></tr></thead><tbody>${steps}</tbody></table></div></details>`;
}

// ---------- audit ----------
async function loadAudit() {
  const recs = await api("GET", "/api/audit?n=300");
  if (!recs.length) { $("#audit").innerHTML = `<p class="empty">Nothing yet. Run a query or an investigation.</p>`; return; }
  const rows = recs.map((r) => `<tr><td class="num">${esc(when(r.at))}</td><td>${esc(r.principal)}</td><td>${esc(r.surface || "")}</td><td>${esc(r.action)}${r.tool ? `: ${esc(r.tool)}` : ""}</td><td class="mono">${esc(r.job_id || r.query_hash || "")}</td><td>${esc(r.dataset || "")}</td><td class="num">${r.bytes ? fmtBytes(r.bytes) : ""}</td><td class="num">${r.usd ? fmtUSD(r.usd) : ""}</td><td>${esc(r.outcome || "")}</td><td>${esc((r.detail || "").slice(0, 140))}</td></tr>`).join("");
  $("#audit").innerHTML = `<div class="tablewrap"><table><thead><tr><th class="num">Time (UTC)</th><th>Who</th><th>Surface</th><th>Action</th><th>Job / query</th><th>Dataset</th><th class="num">Bytes</th><th class="num">Cost</th><th>Outcome</th><th>Detail</th></tr></thead><tbody>${rows}</tbody></table></div>`;
}
$("#audit-reload").addEventListener("click", loadAudit);

// ---------- boot ----------
(async () => {
  try {
    await loadCatalog();
    await loadQuestions();
  } catch (err) {
    $("#catalog").innerHTML = `<div class="note crit">Could not load the catalog: ${esc(err.message)}</div>`;
  }
  const v = location.hash.slice(1);
  if (["catalog", "query", "investigate", "audit"].includes(v)) show(v);
})();
