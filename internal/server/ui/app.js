/*
 * Copyright 2026 MorphEdit (https://github.com/MorphEdit). All rights reserved.
 * Licensed under the PolyForm Strict License 1.0.0 - see LICENSE and NOTICE.
 * Required Notice: Copyright 2026 MorphEdit (https://github.com/MorphEdit)
 */
import { createScene } from './scene3d.js';

const $ = id => document.getElementById(id);
const esc = s => String(s ?? '').replace(/[&<>"]/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' })[c]);
const fmt = n => (n ?? 0).toLocaleString('th-TH');
const rel = iso => {
  if (!iso) return '—';
  const s = Math.max(0, (Date.now() - new Date(iso).getTime()) / 1000);
  if (s < 5) return 'เมื่อสักครู่';
  if (s < 60) return Math.round(s) + ' วินาทีที่แล้ว';
  if (s < 3600) return Math.round(s / 60) + ' นาทีที่แล้ว';
  return Math.round(s / 3600) + ' ชม.ที่แล้ว';
};
const bytes = b => b == null ? '—' : b >= 1e9 ? (b / 1e9).toFixed(1) + ' GB' : b >= 1e6 ? Math.round(b / 1e6) + ' MB' : Math.round(b / 1e3) + ' KB';
const stateText = { ok: 'ซิงก์แล้ว', busy: 'มีคิวรอส่ง', warn: 'กำลังลองใหม่', down: 'ติดต่อไม่ได้' };
const phaseText = { starting: 'กำลังเริ่ม', waiting_db: 'รอฐานข้อมูล', needs_config: 'รออนุญาตตั้งค่า Postgres', needs_restart: 'รอ restart Postgres', waiting_to_join: 'รอเข้าร่วม', joining: 'กำลังเข้าร่วม' };
const WAL_WARN = 1 << 30;

let mesh = null, selected = null, demo = false, everLive = false, adminPw = null;
let paused = window.matchMedia('(prefers-reduced-motion: reduce)').matches;

// ---------- model ----------
// Order = this site first, then peers as the server lists them; offsets are
// remembered so an unreachable site keeps its label.
const known = {};
function normalize(m) {
  const nodes = m.nodes.map((n, i) => {
    const st = n.status || {};
    if (st.sequences) known[n.id] = st.sequences;
    const sq = known[n.id];
    return { id: n.id, self: !!n.self, up: !!n.reachable, st, idx: i, offset: sq ? sq.offset : null, step: sq ? sq.step : null };
  });
  const byId = Object.fromEntries(nodes.map(n => [n.id, n]));
  const rank = { ok: 0, busy: 1, warn: 2, down: 3 };
  function dir(src, dst) {
    if (!src.up) return { state: 'down' };
    const p = (src.st.peers || []).find(x => x.id === dst.id);
    if (!p) return null;
    return { state: !dst.up ? 'down' : p.last_error ? 'warn' : p.backlog > 0 ? 'busy' : 'ok', peer: p };
  }
  const links = [];
  for (let i = 0; i < nodes.length; i++) for (let j = i + 1; j < nodes.length; j++) {
    const a = nodes[i], b = nodes[j], ab = dir(a, b), ba = dir(b, a);
    if (!ab && !ba) continue;
    const worst = [ab, ba].filter(Boolean).reduce((w, d) => rank[d.state] > rank[w] ? d.state : w, 'ok');
    links.push({ a: a.id, b: b.id, ab, ba, state: worst });
  }
  nodes.forEach(n => {
    n.phase = n.st.phase || 'running';
    const mine = links.filter(l => l.a === n.id || l.b === n.id).map(l => l.state);
    n.state = !n.up ? 'down' : n.phase !== 'running' ? 'warn' :
      mine.includes('down') || mine.includes('warn') ? 'warn' :
      mine.includes('busy') || (n.st.outbox && n.st.outbox.pending > 0) ? 'busy' : 'ok';
  });
  const requests = [];
  nodes.forEach(n => (n.up && n.st.join_requests || []).forEach(r => requests.push({ ...r, holder: n.id })));
  return { self: m.self, nodes, byId, links, requests };
}
const siteLabel = n => !n.up ? stateText.down : n.phase !== 'running' ? phaseText[n.phase] || n.phase : stateText[n.state];

// ---------- 3D ----------
const scene3d = createScene($('viewport'), { onSelect: id => { selected = id; render(); } });
if (!scene3d) $('no3d').hidden = false;
scene3d?.setPaused(paused);
// Keep the picture centred between the floating panels (desktop layout only).
function fitInsets() {
  if (!scene3d) return;
  const vw = $('viewport').clientWidth, floating = getComputedStyle($('viewport')).position === 'fixed';
  const left = floating ? $('cam').getBoundingClientRect().right : 0;
  const right = floating ? vw - $('side').getBoundingClientRect().left : 0;
  scene3d.setInsets(left, right);
}
new ResizeObserver(fitInsets).observe(document.body);
fitInsets();

// ---------- health (top bar + status bar) ----------
function setText(el, text) { if (el.textContent !== text) el.textContent = text; } // keeps aria-live quiet
function check(id, level, value) {
  const li = $(id);
  li.className = level;
  li.querySelector('span').textContent = value;
}

function renderHealth(m) {
  const up = m.nodes.filter(n => n.up), down = m.nodes.filter(n => !n.up);
  const queued = up.reduce((s, n) => s + (n.st.outbox ? n.st.outbox.pending : 0), 0);
  const conflicts = up.reduce((s, n) => s + (n.st.conflicts ? n.st.conflicts.total : 0), 0);
  const retrying = m.links.some(l => l.state === 'warn');
  const self = m.byId[m.self], phase = self ? self.phase : 'running';
  let level = 'ok', text = 'ทุกไซต์ซิงก์ปกติ';
  if (phase !== 'running') { level = 'busy'; text = 'ไซต์นี้: ' + (phaseText[phase] || phase); }
  else if (self && self.st.removed) { level = 'down'; text = 'ไซต์นี้ถูกถอดออกจากเครือข่ายแล้ว'; }
  else if (down.length) { level = 'down'; text = `ติดต่อไม่ได้ ${down.length} ไซต์: ${down.map(n => n.id.toUpperCase()).join(', ')}`; }
  else if (retrying) { level = 'busy'; text = 'การส่งข้อมูลบางเส้นกำลังลองใหม่'; }
  else if (queued > 0) { level = 'busy'; text = `กำลังส่งข้อมูลที่ค้าง ${fmt(queued)} รายการ`; }
  $('health').className = 'health ' + level;
  $('health-icon').textContent = level === 'ok' ? '✓' : level === 'busy' ? '!' : '×';
  setText($('health-text'), text);
  $('health-sub').textContent = (demo ? 'ข้อมูลตัวอย่าง · ' : '') +
    (down.length ? 'ไซต์ที่ติดต่อไม่ได้จะได้รับข้อมูลที่ค้างเมื่อกลับมา' : 'ลากเพื่อหมุน · scroll เพื่อซูม · คลิกไซต์เพื่อดูรายละเอียด');

  check('c-online', down.length ? 'bad' : '', `${up.length}/${m.nodes.length} ออนไลน์`);
  check('c-queue', queued > 0 ? 'warn' : '', `${fmt(queued)} tx`);
  check('c-conflicts', conflicts > 0 ? 'warn' : '', fmt(conflicts));
  const wal = self && self.st.wal;
  check('c-wal', wal && wal.retained_bytes > WAL_WARN ? 'bad' : '', wal ? bytes(wal.retained_bytes) : '—');
  $('c-updated').textContent = 'อัปเดต ' + new Date().toLocaleTimeString('th-TH');
}

// ---------- side panel ----------
function renderSites(m) {
  $('site-count').textContent = `· ${m.nodes.length}`;
  $('site-rows').innerHTML = m.nodes.map(n => {
    const st = n.st, state = !n.up ? 'down' : n.state;
    const q = n.up && st.outbox ? st.outbox.pending : null, c = n.up && st.conflicts ? st.conflicts.total : null;
    return `<li><button class="site-btn" type="button" data-select="${esc(n.id)}" aria-pressed="${n.id === selected}">
      <span class="lamp ${state}" aria-hidden="true"></span>
      <span><span class="name">${esc(n.id)}</span>${n.self ? '<span class="you">ไซต์นี้</span>' : ''}</span>
      <span class="state ${state}">${esc(siteLabel(n))}</span>
      <span class="nums">คิว <b class="${q > 0 ? 'warn' : ''}">${q == null ? '—' : fmt(q)}</b> · conflict <b class="${c > 0 ? 'warn' : ''}">${c == null ? '—' : fmt(c)}</b> · ชุด ID ${n.offset ? `${n.offset}/${n.step}` : '—'}</span>
    </button></li>`;
  }).join('');
}

function renderDetail(m) {
  const n = m.byId[selected];
  $('detail-title').textContent = n.id;
  $('detail-state').className = 'state ' + (n.up ? n.state : 'down');
  $('detail-state').textContent = (n.self ? 'ไซต์นี้ · ' : '') + siteLabel(n);
  const num = (v, unit) => v == null ? '—' : fmt(v) + (unit ? `<small>${unit}</small>` : '');
  $('m-outbox').innerHTML = num(n.up ? n.st.outbox?.pending : null, 'tx');
  $('m-captured').innerHTML = num(n.up ? n.st.capture?.captured_txs : null, 'tx');
  $('m-conflicts').innerHTML = num(n.up ? n.st.conflicts?.total : null);
  $('m-offset').innerHTML = n.offset ? `${n.offset}<small>/ ${n.step}</small>` : '—';

  const rows = m.links.filter(l => l.a === n.id || l.b === n.id).map(l => {
    const other = l.a === n.id ? l.b : l.a, out = l.a === n.id ? l.ab : l.ba;
    const inbox = (n.st.inbox || []).find(x => x.origin === other);
    const p = out && out.peer, state = out ? out.state : 'down';
    const meta = !n.up ? 'ไม่มีข้อมูล' :
      `ส่งแล้ว #${p ? p.acked_seq : 0} · ค้าง ${p ? fmt(p.backlog) : 0} · รับแล้ว #${inbox ? inbox.applied_seq : 0}<br>ส่งสำเร็จล่าสุด ${rel(p && p.last_ok)}`;
    const err = p && p.last_error ? `<span class="err" title="${esc(p.last_error)}">${esc(p.last_error)}</span>` : '';
    return `<li><span class="state ${state}" role="img" aria-label="${stateText[state]}"></span><span class="route">${esc(n.id)} ⇄ ${esc(other)}</span><span class="meta">${meta}${err}</span></li>`;
  });
  $('link-rows').innerHTML = rows.join('') || '<li class="empty">ยังไม่มีไซต์อื่น</li>';

  const cs = (n.up && n.st.conflicts && n.st.conflicts.recent) || [];
  $('conflict-rows').innerHTML = cs.slice(0, 6).map(c =>
    `<li class="conflict"><span class="kind">${esc(c.kind)}</span><span class="what" title="${esc(c.detail)}">${esc(c.table)} ${esc(c.pk)} <span class="muted">← ${esc(c.origin)}</span></span><span class="when">${rel(c.detected_at)}</span></li>`
  ).join('') || '<li class="empty">ไม่มี conflict</li>';
}

function renderCluster(m) {
  const self = m.byId[m.self], st = self ? self.st : {};
  const running = !st.phase || st.phase === 'running';
  const skipped = st.schema_skipped || 0;
  const msg = st.removed ? 'ไซต์นี้ถูกถอดออกจากเครือข่ายแล้ว — หยุดซิงก์' :
    st.warning || st.wal_warning || st.notice || (!running && st.error) ||
    (skipped ? `ข้ามข้อมูลไป ${skipped} รายการเพราะโครงสร้างตารางของไซต์นี้ไม่ตรงกับไซต์อื่น — แก้ตารางให้ตรงกันแล้วกดลองใหม่ (ข้อมูลไม่หาย)` : '');
  const banner = $('banner');
  banner.hidden = !msg || demo || st.phase === 'joining';
  $('banner-text').textContent = msg;
  banner.className = 'alert' + (st.removed || (!st.notice && !st.warning && !st.wal_warning && !skipped && st.error) ? ' error' : '');
  $('allow-pg').hidden = st.phase !== 'needs_config' || !st.admin_enabled;
  $('replay').hidden = !skipped || !st.admin_enabled || !!(st.warning || st.wal_warning || st.notice);

  const joining = st.phase === 'waiting_to_join' || st.phase === 'joining';
  $('join-panel').hidden = !joining;
  $('sites-block').hidden = joining;
  $('detail').hidden = joining;
  if (joining) {
    const p = st.pairing || {};
    $('pair-code').textContent = p.code || '— — —';
    $('show-code').hidden = !p.hidden;
    $('join-title').textContent = st.phase === 'joining' ? (st.notice || 'กำลังเข้าร่วม…') :
      p.status === 'requested' ? `ส่งคำขอไปที่ ${String(p.seed_id || '').toUpperCase()} แล้ว — รออนุมัติ` :
      p.status === 'rejected' ? 'คำขอถูกปฏิเสธ — จะลองใหม่ใน 5 นาที' :
      p.status === 'bad_proof' ? 'การอนุมัติใช้รหัสไม่ตรง — สร้างรหัสใหม่แล้ว กดอนุมัติอีกครั้งด้วยรหัสใหม่' : 'กำลังค้นหาไซต์อื่นในวง LAN…';
    $('paste-form').hidden = st.phase === 'joining';
  }

  $('add-site').hidden = !running || demo || !st.admin_enabled;
  const reqs = m.requests || [];
  $('requests').hidden = reqs.length === 0;
  $('request-rows').innerHTML = reqs.map(r => `<li>
    <span class="rq-name">${esc(r.node_id)}</span>${r.code ? `<span class="rq-code">${esc(r.code)}</span>` : ''}
    <span class="rq-actions">${demo ? '<span class="muted">ตัวอย่าง</span>' : r.holder === m.self
      ? `<button class="btn primary" type="button" data-approve="${esc(r.id)}" data-name="${esc(r.node_id)}">อนุมัติ</button><button class="btn danger" type="button" data-reject="${esc(r.id)}">ปฏิเสธ</button>`
      : `<span class="muted">อนุมัติที่ dashboard ของ ${esc(r.holder).toUpperCase()}</span>`}</span>
    <span class="rq-meta">${esc(r.url)} · ${rel(r.created_at)}</span></li>`).join('');
  $('attention').hidden = banner.hidden && $('requests').hidden && $('invite-box').hidden;

  const sel = m.byId[selected];
  $('remove-site').hidden = demo || !running || !st.admin_enabled || !sel || sel.id === m.self;
}

function render() {
  if (!mesh) return;
  const m = mesh;
  if (!selected || !m.byId[selected]) selected = m.self;
  $('self-node').textContent = m.self;
  const ver = m.byId[m.self] && m.byId[m.self].st.version;
  $('version').textContent = ver && ver !== 'dev' ? 'v' + ver : '';
  renderHealth(m);
  scene3d?.update(m, selected, n => !n.up ? stateText.down : n.phase !== 'running' ? siteLabel(n) :
    `${stateText[n.state]} · คิว ${fmt(n.st.outbox ? n.st.outbox.pending : 0)}${n.st.conflicts && n.st.conflicts.total ? ' · conflict ' + fmt(n.st.conflicts.total) : ''}`);
  renderSites(m);
  renderDetail(m);
  renderCluster(m);
  document.querySelector('.pause-symbol').textContent = paused ? '▷' : 'Ⅱ';
  $('motion-label').textContent = paused ? 'เล่นการเคลื่อนไหว' : 'หยุดเคลื่อนไหว';
  $('motion').setAttribute('aria-pressed', String(paused));
}

// ---------- actions ----------
document.addEventListener('click', e => {
  const s = e.target.closest('[data-select]');
  if (s) { selected = s.dataset.select; render(); }
  const c = e.target.closest('[data-cam]');
  if (c && scene3d) {
    const a = c.dataset.cam;
    ({ in: () => scene3d.zoom(0.8), out: () => scene3d.zoom(1.25), rotl: () => scene3d.rotate(-20), rotr: () => scene3d.rotate(20),
       up: () => scene3d.pan(0, 1), down: () => scene3d.pan(0, -1), left: () => scene3d.pan(-1, 0), right: () => scene3d.pan(1, 0), home: () => scene3d.home() })[a]?.();
  }
});
$('motion').addEventListener('click', () => { paused = !paused; scene3d?.setPaused(paused); render(); });

async function adminFetch(path, body) {
  for (let attempt = 0; attempt < 2; attempt++) {
    if (!adminPw) {
      adminPw = window.prompt('รหัส admin ของไซต์นี้ (CONDUIT_ADMIN_PASSWORD)');
      if (!adminPw) throw new Error('ยกเลิก');
    }
    const r = await fetch(path, { method: 'POST', headers: { 'X-Admin-Password': adminPw, 'Content-Type': 'application/json' }, body: body ? JSON.stringify(body) : undefined });
    if (r.status === 401) { adminPw = null; continue; }
    if (!r.ok) throw new Error((await r.text()).trim() || r.statusText);
    return r.status === 204 ? null : r.json();
  }
  throw new Error('รหัส admin ไม่ถูกต้อง');
}

document.addEventListener('click', async e => {
  const a = e.target.closest('[data-approve],[data-reject]');
  if (!a) return;
  const id = a.dataset.approve || a.dataset.reject;
  let body;
  if (a.dataset.approve) {
    // The code is shown only on the new site's own screen; typing it here
    // proves to the new site that this approval is genuine.
    const code = window.prompt(`พิมพ์รหัสจับคู่ 6 หลักที่แสดงบนหน้าจอของ ${a.dataset.name.toUpperCase()}`);
    if (code === null) return;
    body = { code };
  } else if (!confirm('ปฏิเสธคำขอนี้?')) return;
  try { await adminFetch(`v1/admin/requests/${id}/${a.dataset.approve ? 'approve' : 'reject'}`, body); tick(); }
  catch (err) { alert(err.message); }
});
$('show-code').addEventListener('click', async () => {
  const pw = window.prompt('รหัส admin ของไซต์นี้ (CONDUIT_ADMIN_PASSWORD)');
  if (!pw) return;
  const r = await fetch('v1/admin/check', { headers: { 'X-Admin-Password': pw } }).catch(() => null);
  if (!r || !r.ok) { alert('รหัส admin ไม่ถูกต้อง'); return; }
  adminPw = pw;
  tick();
});
$('allow-pg').addEventListener('click', async () => {
  if (!confirm('Conduit จะตั้งค่า wal_level=logical และ track_commit_timestamp=on ให้ Postgres ของไซต์นี้\nจากนั้นต้อง restart Postgres หนึ่งครั้ง — ดำเนินการต่อไหม?')) return;
  try { await adminFetch('v1/admin/configure-postgres'); tick(); } catch (err) { alert(err.message); }
});
$('replay').addEventListener('click', async () => {
  try {
    const r = await adminFetch('v1/admin/replay');
    alert(`ใส่สำเร็จ ${r.replayed} รายการ` + (r.still_failing ? ` · ยังไม่เข้า ${r.still_failing} รายการ (โครงสร้างยังไม่ตรง)` : ''));
    tick();
  } catch (err) { alert(err.message); }
});
$('add-site').addEventListener('click', async () => {
  try {
    const r = await adminFetch('v1/admin/invites');
    $('invite-code').textContent = r.code;
    $('invite-exp').textContent = new Date(r.expires_at).toLocaleString('th-TH');
    $('invite-box').hidden = false;
    $('attention').hidden = false;
    $('copy-invite').focus();
  } catch (err) { alert(err.message); }
});
$('copy-invite').addEventListener('click', () => {
  navigator.clipboard.writeText($('invite-code').textContent).then(() => {
    $('copy-invite').textContent = 'คัดลอกแล้ว';
    setTimeout(() => { $('copy-invite').textContent = 'คัดลอก'; }, 1500);
  });
});
$('close-invite').addEventListener('click', () => { $('invite-box').hidden = true; render(); });
$('remove-site').addEventListener('click', async () => {
  if (!confirm(`ถอด ${selected.toUpperCase()} ออกจากเครือข่าย? ไซต์อื่นจะหยุดส่งข้อมูลให้ไซต์นี้`)) return;
  try { await adminFetch(`v1/admin/members/${selected}/remove`); tick(); } catch (err) { alert(err.message); }
});
$('paste-form').addEventListener('submit', async e => {
  e.preventDefault();
  const code = $('paste-input').value.trim(), msg = $('paste-msg');
  if (!code.startsWith('cdt1_')) { msg.textContent = 'รหัสเชิญต้องขึ้นต้นด้วย cdt1_'; msg.className = 'form-msg err'; return; }
  try { await adminFetch('v1/admin/join', { invite: code }); msg.textContent = 'กำลังเข้าร่วม…'; msg.className = 'form-msg'; }
  catch (err) { msg.textContent = err.message; msg.className = 'form-msg err'; }
});
$('paste-input').addEventListener('input', () => { $('paste-msg').textContent = ''; });

function setMode(text, cls) { $('mode-text').textContent = text; $('mode').className = 'mode ' + cls; }

// ---------- demo data (?demo) ----------
const demoState = { t0: Date.now(), seq: { host: 120, local: 64, branch: 31, warehouse: 12 }, acked: {} };
function demoMesh() {
  const t = (Date.now() - demoState.t0) / 1000, cycle = t % 24;
  const localCut = cycle > 10 && cycle < 17;
  const ids = ['host', 'local', 'branch', 'warehouse'], off = { host: 1, local: 2, branch: 3, warehouse: 4 };
  ids.forEach(id => { if (Math.random() < 0.5) demoState.seq[id] += 1; });
  const now = new Date().toISOString();
  const status = id => {
    const peers = ids.filter(p => p !== id).map(p => {
      const k = id + '>' + p, cut = localCut && (id === 'local' || p === 'local');
      if (!cut) demoState.acked[k] = Math.min(demoState.seq[id], (demoState.acked[k] ?? demoState.seq[id]) + 3);
      const acked = demoState.acked[k] ?? demoState.seq[id];
      return { id: p, url: `https://conduit-${p}:7443`, acked_seq: acked, backlog: demoState.seq[id] - acked,
        last_ok: cut ? new Date(Date.now() - (cycle - 10) * 1000).toISOString() : now,
        last_error: cut ? `Post "https://conduit-${p}:7443/v1/apply": dial tcp: lookup conduit-${p}: no such host` : '' };
    });
    const backlog = Math.max(...peers.map(p => p.backlog));
    return { node_id: id, phase: 'running', version: '0.3.0', sequences: { offset: off[id], step: 10 },
      capture: { enabled: true, connected: true, captured_txs: demoState.seq[id] },
      outbox: { pending: backlog, max_seq: demoState.seq[id] }, peers, wal: { retained_bytes: 48e6 + demoState.seq[id] * 1e4 },
      inbox: ids.filter(p => p !== id).map(p => ({ origin: p, applied_seq: demoState.seq[p] - 2 })),
      join_requests: id === 'host' ? [{ id: 'demo', node_id: 'factory', url: 'https://factory:7443', created_at: new Date(demoState.t0).toISOString(), status: 'pending' }] : [],
      conflicts: id === 'local' ? { total: 2, recent: [
        { kind: 'update_update', table: 'public.customers', pk: '{"id":"231"}', origin: 'host', detected_at: new Date(Date.now() - 95e3).toISOString(), detail: 'local row was modified later' },
        { kind: 'unique_violation', table: 'public.customers', pk: '{"id":"261"}', origin: 'host', detected_at: new Date(Date.now() - 400e3).toISOString(), detail: 'duplicate key value violates unique constraint' }] }
        : { total: 0, recent: [] } };
  };
  return { self: 'host', nodes: ids.map(id => ({ id, self: id === 'host', reachable: !(localCut && id === 'local'), status: status(id) })) };
}

// ---------- polling ----------
// Sample data only when asked for (?demo) or opened as a file, never as a
// stand-in for a site that cannot be reached.
const forceDemo = new URLSearchParams(location.search).has('demo') || location.protocol === 'file:';
async function tick() {
  let raw = null;
  if (forceDemo) {
    demo = true; raw = demoMesh(); setMode('ข้อมูลตัวอย่าง', 'demo');
  } else {
    try {
      const r = await fetch('v1/mesh', { cache: 'no-store', headers: adminPw ? { 'X-Admin-Password': adminPw } : {} });
      if (r.ok) raw = await r.json();
    } catch (_) { /* fall through */ }
    if (!raw) {
      setMode(everLive ? 'ขาดการเชื่อมต่อ' : 'กำลังเชื่อมต่อ', 'offline');
      $('health').className = 'health down';
      $('health-icon').textContent = '×';
      setText($('health-text'), 'ติดต่อ Conduit ของไซต์นี้ไม่ได้');
      $('health-sub').textContent = 'กำลังลองใหม่ทุก 2 วินาที' + (everLive ? ' · ข้อมูลที่เห็นเป็นของล่าสุดที่ได้รับ' : '');
      return;
    }
    everLive = true; setMode('สด', 'live');
  }
  mesh = normalize(raw);
  render();
}

(async function loop() { await tick(); setTimeout(loop, 2000); })();
