/*
 * Copyright 2026 MorphEdit (https://github.com/MorphEdit). All rights reserved.
 * Licensed under the PolyForm Strict License 1.0.0 - see LICENSE and NOTICE.
 * Required Notice: Copyright 2026 MorphEdit (https://github.com/MorphEdit)
 */
(() => {
  'use strict';

  // ---------- isometric helpers ----------
  const COS = Math.cos(Math.PI / 6);
  const P = (o, x, y, z = 0) => [o[0] + (x - y) * COS, o[1] + (x + y) * 0.5 - z];
  const pts = a => a.map(p => p[0].toFixed(1) + ',' + p[1].toFixed(1)).join(' ');
  const SLAB = 120, SLAB_H = 18;                       // half-size and thickness of a site platform
  const RACK = { x: 12, y: -96, w: 70, d: 46, h: 132 };  // Conduit server, in slab coordinates
  const DB = { x: -52, y: 38, r: 38, h: 74 };            // Postgres cylinder

  function box(o, x, y, z, w, d, h, cls) {
    const top = [P(o, x, y, z + h), P(o, x + w, y, z + h), P(o, x + w, y + d, z + h), P(o, x, y + d, z + h)];
    const left = [P(o, x, y + d, z + h), P(o, x + w, y + d, z + h), P(o, x + w, y + d, z), P(o, x, y + d, z)];
    const right = [P(o, x + w, y, z + h), P(o, x + w, y + d, z + h), P(o, x + w, y + d, z), P(o, x + w, y, z)];
    return `<g class="${cls}"><polygon class="face-left" points="${pts(left)}"/><polygon class="face-right" points="${pts(right)}"/><polygon class="face-top" points="${pts(top)}"/></g>`;
  }

  function cylinder(c, r, h) {
    const ry = r * 0.5, [x, y] = c;
    const ring = t => `<path class="db-ring" d="M${x - r} ${y - t} A${r} ${ry} 0 0 0 ${x + r} ${y - t}"/>`;
    return `<path class="db-body" d="M${x - r} ${y} A${r} ${ry} 0 0 0 ${x + r} ${y} L${x + r} ${y - h} A${r} ${ry} 0 0 1 ${x - r} ${y - h} Z"/>` +
      ring(h * 0.33) + ring(h * 0.66) + `<ellipse class="db-top" cx="${x}" cy="${y - h}" rx="${r}" ry="${ry}"/>`;
  }

  function leds(o) {
    const { x, y, w, d, h } = RACK, z = SLAB_H;
    let s = '';
    for (let k = 0; k < 6; k++) {                     // rows on the front-left face
      const zz = z + h - 20 - k * 18;
      const a = P(o, x + 8, y + d, zz), b = P(o, x + w - 26, y + d, zz);
      s += `<polyline class="led idle" points="${pts([a, b])}" stroke-width="2" stroke="currentColor"/>`;
      const dot = P(o, x + w - 14, y + d, zz);
      s += `<circle class="led act blink" cx="${dot[0].toFixed(1)}" cy="${dot[1].toFixed(1)}" r="2.6"/>`;
    }
    for (let k = 0; k < 6; k++) {                     // status column on the front-right face
      const dot = P(o, x + w, y + 10, z + h - 20 - k * 18);
      s += `<circle class="led blink" cx="${dot[0].toFixed(1)}" cy="${dot[1].toFixed(1)}" r="2"/>`;
    }
    return s;
  }

  // ---------- layout ----------
  const W = 1536, CX = 768, CY = 490, RX = 390, RY = 270;
  function layout(ids) {
    const n = ids.length, pos = {};
    ids.forEach((id, i) => {
      if (n === 1) { pos[id] = [CX, CY]; return; }
      const a = -Math.PI / 2 + (i * 2 * Math.PI) / n;
      pos[id] = [CX + RX * Math.cos(a), CY + RY * Math.sin(a) + (n === 2 ? 60 : 0)];
    });
    return pos;
  }
  const anchor = o => P(o, RACK.x + RACK.w / 2, RACK.y + RACK.d / 2, SLAB_H + RACK.h);

  // ---------- state ----------
  const $ = id => document.getElementById(id);
  const stage = $('stage');
  let mesh = null, topo = '', pos = {}, selected = null, demo = false, everLive = false;
  let paused = window.matchMedia('(prefers-reduced-motion: reduce)').matches;

  // Order = this site, then peers as listed in its config: stable even
  // while a site is unreachable. Offsets are remembered for the labels.
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
      const state = !dst.up ? 'down' : p.last_error ? 'warn' : p.backlog > 0 ? 'busy' : 'ok';
      return { state, peer: p };
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
      n.state = !n.up ? 'down' : n.phase !== 'running' ? 'warn' : mine.includes('down') || mine.includes('warn') ? 'warn' : mine.includes('busy') || (n.st.outbox && n.st.outbox.pending > 0) ? 'busy' : 'ok';
    });
    const requests = [];
    nodes.forEach(n => (n.up && n.st.join_requests || []).forEach(r => requests.push({ ...r, holder: n.id })));
    return { self: m.self, nodes, byId, links, requests };
  }

  // ---------- one-time scene build (per topology) ----------
  function buildScene(m) {
    const ids = m.nodes.map(n => n.id);
    pos = layout(ids);

    const g = [];
    const C = [CX, CY + 40];
    for (let i = -9; i <= 9; i++) {
      g.push(`<polyline class="grid-line" points="${pts([P(C, i * 90, -900), P(C, i * 90, 900)])}"/>`);
      g.push(`<polyline class="grid-line" points="${pts([P(C, -900, i * 90), P(C, 900, i * 90)])}"/>`);
    }
    $('grid').innerHTML = `<mask id="grid-fade"><ellipse cx="${CX}" cy="${CY + 40}" rx="700" ry="400" fill="white" opacity=".55"/></mask><g mask="url(#grid-fade)">${g.join('')}</g>`;

    const centroid = ids.reduce((s, id) => [s[0] + anchor(pos[id])[0] / ids.length, s[1] + anchor(pos[id])[1] / ids.length], [0, 0]);
    $('links').innerHTML = m.links.map(l => {
      const A = anchor(pos[l.a]), B = anchor(pos[l.b]);
      const mid = [(A[0] + B[0]) / 2, (A[1] + B[1]) / 2];
      const out = [mid[0] - centroid[0], mid[1] - centroid[1]];
      const len = Math.hypot(out[0], out[1]) || 1;
      const ctrl = [mid[0] + out[0] / len * 60, mid[1] + out[1] / len * 60 - 70];
      const nx = -(B[1] - A[1]), ny = B[0] - A[0], nl = Math.hypot(nx, ny) || 1;
      const off = k => [ctrl[0] + nx / nl * k, ctrl[1] + ny / nl * k];
      const path = (s, c, e) => `M${s[0].toFixed(1)} ${s[1].toFixed(1)} Q${c[0].toFixed(1)} ${c[1].toFixed(1)} ${e[0].toFixed(1)} ${e[1].toFixed(1)}`;
      const d1 = path(A, off(12), B), d2 = path(B, off(-12), A);
      const dirG = (d, key) => `<g class="link-dir" data-dir="${key}"><path class="link-bed" d="${d}"/><path class="link-glow" d="${d}"/><path class="link-signal" d="${d}"/></g>`;
      return `<g class="link" data-link="${l.a}|${l.b}">${dirG(d1, l.a + '>' + l.b)}${dirG(d2, l.b + '>' + l.a)}</g>`;
    }).join('');

    const order = [...ids].sort((a, b) => pos[a][1] - pos[b][1]);
    $('sites').innerHTML = order.map(id => {
      const o = pos[id];
      const dbc = P(o, DB.x, DB.y, SLAB_H);
      const capFrom = [dbc[0], dbc[1] - DB.h - 4];
      const capTo = P(o, RACK.x + RACK.w * 0.25, RACK.y + RACK.d, SLAB_H + RACK.h * 0.45);
      const capCtrl = [(capFrom[0] + capTo[0]) / 2, Math.min(capFrom[1], capTo[1]) - 50];
      const bottom = P(o, SLAB, SLAB, 0);
      const tagDb = [dbc[0] - 58, dbc[1] - DB.h - 34];
      const tagRack = P(o, RACK.x + RACK.w, RACK.y, SLAB_H + RACK.h * 0.7);
      const tag = (p, text, w) => `<rect class="tag" x="${(p[0] - w / 2).toFixed(1)}" y="${(p[1] - 11).toFixed(1)}" width="${w}" height="22" rx="4"/><text class="tag-text" x="${p[0].toFixed(1)}" y="${(p[1] + 4).toFixed(1)}">${text}</text>`;
      return `<g class="site" data-site="${id}">
        <ellipse class="site-halo" cx="${o[0]}" cy="${o[1] + 10}" rx="${SLAB * 2.1}" ry="${SLAB * 1.15}" filter="url(#soft)"/>
        ${box(o, -SLAB, -SLAB, 0, SLAB * 2, SLAB * 2, SLAB_H, 'slab')}
        ${cylinder(dbc, DB.r, DB.h)}
        ${box(o, RACK.x, RACK.y, SLAB_H, RACK.w, RACK.d, RACK.h, 'rack')}
        <g>${leds(o)}</g>
        <path class="capture-line" data-cap="${id}" d="M${capFrom[0].toFixed(1)} ${capFrom[1].toFixed(1)} Q${capCtrl[0].toFixed(1)} ${capCtrl[1].toFixed(1)} ${capTo[0].toFixed(1)} ${capTo[1].toFixed(1)}"/>
        ${tag(tagDb, 'POSTGRES', 92)}
        ${tag([tagRack[0] + 62, tagRack[1]], 'CONDUIT', 84)}
        <text class="site-name" x="${bottom[0].toFixed(1)}" y="${(bottom[1] + 46).toFixed(1)}">${id.toUpperCase()}</text>
        <text class="site-sub" data-sub="${id}" x="${bottom[0].toFixed(1)}" y="${(bottom[1] + 72).toFixed(1)}"></text>
      </g>`;
    }).join('');

    $('hotspots').innerHTML = ids.map((id, i) => {
      const h = P(pos[id], 58, 72, SLAB_H);
      return `<button class="hotspot" type="button" data-select="${id}" style="left:${h[0] / W * 100}%;top:${h[1] / 1024 * 100}%" aria-label="ดูสถานะไซต์ ${id}"><span>0${i + 1}</span></button>`;
    }).join('');

    $('site-list').innerHTML = ids.map((id, i) => `<button class="site-card" type="button" data-select="${id}" aria-pressed="false">
      <span class="num">0${i + 1}</span><span><strong>${id}<span class="you" hidden>YOU</span></strong><small data-card="${id}"></small></span><span class="arrow" aria-hidden="true">↗</span></button>`).join('');

    document.querySelectorAll('[data-select]').forEach(b => b.addEventListener('click', () => { selected = b.dataset.select; render(); }));
  }

  // ---------- per-tick update ----------
  const rel = iso => {
    if (!iso) return '—';
    const s = Math.max(0, (Date.now() - new Date(iso).getTime()) / 1000);
    if (s < 5) return 'เมื่อสักครู่';
    if (s < 60) return Math.round(s) + ' วินาทีที่แล้ว';
    if (s < 3600) return Math.round(s / 60) + ' นาทีที่แล้ว';
    return Math.round(s / 3600) + ' ชม.ที่แล้ว';
  };
  const esc = s => String(s ?? '').replace(/[&<>"]/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' })[c]);
  const stateText = { ok: 'ซิงก์แล้ว', busy: 'มีคิวรอส่ง', warn: 'กำลัง retry', down: 'ออฟไลน์' };
  const phaseText = { starting: 'กำลังเริ่ม', waiting_db: 'รอฐานข้อมูล', needs_config: 'รออนุญาตตั้งค่า Postgres', needs_restart: 'รอ restart Postgres', waiting_to_join: 'รอเข้าร่วม', joining: 'กำลังเข้าร่วม' };

  function render() {
    if (!mesh) return;
    const m = mesh;
    const key = m.nodes.map(n => n.id).join('|');
    if (key !== topo) { topo = key; buildScene(m); }
    if (!selected || !m.byId[selected]) selected = m.self;
    stage.classList.toggle('paused', paused);

    m.nodes.forEach(n => {
      const g = document.querySelector(`[data-site="${n.id}"]`);
      g.classList.toggle('selected', n.id === selected);
      g.classList.toggle('down', !n.up);
      g.classList.toggle('busy', n.state === 'busy');
      const cap = document.querySelector(`[data-cap="${n.id}"]`);
      cap.classList.toggle('off', !(n.up && n.st.capture && n.st.capture.connected));
      const sub = document.querySelector(`[data-sub="${n.id}"]`);
      sub.textContent = !n.up ? 'ติดต่อไม่ได้' : n.phase !== 'running' ? phaseText[n.phase] || n.phase :
        (n.offset ? `ID ลงท้าย ${n.offset % n.step}` : '') + ` · คิว ${n.st.outbox ? n.st.outbox.pending : 0}`;
      sub.classList.toggle('down', !n.up);
      const card = document.querySelector(`[data-card="${n.id}"]`);
      card.textContent = !n.up ? 'ติดต่อไม่ได้' : n.phase !== 'running' ? phaseText[n.phase] || n.phase :
        `${stateText[n.state]} · conflict ${n.st.conflicts ? n.st.conflicts.total : 0}`;
      const btn = card.closest('.site-card');
      btn.classList.toggle('selected', n.id === selected);
      btn.classList.toggle('down', !n.up);
      btn.setAttribute('aria-pressed', String(n.id === selected));
      btn.querySelector('.you').hidden = !n.self;
      const hs = document.querySelector(`.hotspot[data-select="${n.id}"]`);
      hs.classList.toggle('selected', n.id === selected);
      hs.classList.toggle('down', !n.up);
    });

    m.links.forEach(l => {
      const g = document.querySelector(`[data-link="${l.a}|${l.b}"]`);
      if (!g) return;
      g.classList.toggle('dim', l.a !== selected && l.b !== selected);
      [[l.a + '>' + l.b, l.ab], [l.b + '>' + l.a, l.ba]].forEach(([k, d]) => {
        const el = g.querySelector(`[data-dir="${k}"]`);
        el.setAttribute('class', 'link-dir ' + (d ? d.state : 'down'));
      });
    });

    // detail
    const n = m.byId[selected];
    $('detail-title').textContent = n.id;
    $('detail-dot').className = 'status-dot ' + n.state;
    $('detail-state').textContent = (n.self ? 'ไซต์นี้ · ' : '') + stateText[n.state];
    const num = (v, unit) => v == null ? '—' : v + (unit ? `<span>${unit}</span>` : '');
    $('m-outbox').innerHTML = num(n.up ? n.st.outbox?.pending : null, 'tx');
    $('m-captured').innerHTML = num(n.up ? n.st.capture?.captured_txs : null);
    $('m-conflicts').innerHTML = num(n.up ? n.st.conflicts?.total : null);
    $('m-offset').innerHTML = n.offset ? `${n.offset}<span>/ ${n.step}</span>` : '—';

    $('links-for').textContent = '· ' + n.id.toUpperCase();
    const rows = m.links.filter(l => l.a === n.id || l.b === n.id).map(l => {
      const other = l.a === n.id ? l.b : l.a;
      const out = l.a === n.id ? l.ab : l.ba;
      const inbox = (n.st.inbox || []).find(x => x.origin === other);
      const p = out && out.peer;
      const state = out ? out.state : 'down';
      const meta = !n.up ? 'ไม่มีข้อมูล' :
        `ส่งแล้ว #${p ? p.acked_seq : 0} · ค้าง ${p ? p.backlog : 0} · รับแล้ว #${inbox ? inbox.applied_seq : 0}<br>ส่งสำเร็จล่าสุด ${rel(p && p.last_ok)}`;
      const err = p && p.last_error ? `<span class="err" title="${esc(p.last_error)}">${esc(p.last_error)}</span>` : '';
      return `<li><span class="status-dot ${state}"></span><span class="route">${esc(n.id)} ⇄ ${esc(other)}</span><span class="meta">${meta}${err}</span></li>`;
    });
    $('link-rows').innerHTML = rows.join('') || '<li class="empty">ไม่มี peer</li>';

    const cs = (n.up && n.st.conflicts && n.st.conflicts.recent) || [];
    $('conflict-rows').innerHTML = cs.slice(0, 6).map(c =>
      `<li><span class="kind">${esc(c.kind)}</span><span class="what" title="${esc(c.detail)}">${esc(c.table)} ${esc(c.pk)} <span class="empty">← ${esc(c.origin)}</span></span><span class="when">${rel(c.detected_at)}</span></li>`
    ).join('') || '<li class="empty">ยังไม่มี conflict</li>';

    const up = m.nodes.filter(x => x.up).length;
    const queued = m.nodes.reduce((s, x) => s + (x.up && x.st.outbox ? x.st.outbox.pending : 0), 0);
    $('summary').textContent = `${up}/${m.nodes.length} ไซต์ออนไลน์ · คิวรวม ${queued}`;
    const worst = m.nodes.some(x => x.state === 'down') ? 'down' : m.nodes.some(x => x.state === 'warn' || x.state === 'busy') ? 'busy' : 'ok';
    $('pulse').className = 'status-dot ' + worst;
    $('updated').textContent = (demo ? 'ข้อมูลตัวอย่าง · ' : 'ข้อมูลจริง · ') + 'อัปเดต ' + new Date().toLocaleTimeString('th-TH');
    $('self-node').textContent = '/ ' + m.self;
    const ver = m.byId[m.self] && m.byId[m.self].st.version;
    $('version').textContent = ver && ver !== 'dev' ? 'v' + ver : '';

    renderCluster(m);
    document.querySelector('.pause-symbol').textContent = paused ? '▷' : 'Ⅱ';
    $('motion-label').textContent = paused ? 'เล่นการเคลื่อนไหว' : 'หยุดการเคลื่อนไหว';
    $('motion').setAttribute('aria-pressed', String(paused));
  }

  // ---------- cluster controls ----------
  let adminPw = null;
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

  function renderCluster(m) {
    const self = m.byId[m.self], st = self ? self.st : {};
    const running = !st.phase || st.phase === 'running';
    const banner = $('banner');
    const skipped = st.schema_skipped || 0;
    const msg = st.removed ? 'ไซต์นี้ถูกถอดออกจากเครือข่ายแล้ว — หยุดซิงก์' :
      st.warning || st.wal_warning || st.notice || (!running && st.error) ||
      (skipped ? `ข้ามข้อมูลไป ${skipped} รายการเพราะโครงสร้างตารางของไซต์นี้ไม่ตรงกับไซต์อื่น — แก้ตารางให้ตรงกันแล้วกดลองใหม่ (ข้อมูลไม่หาย)` : '');
    banner.hidden = !msg || demo || st.phase === 'joining';
    $('banner-text').textContent = msg;
    banner.className = 'banner' + (!st.notice && !st.warning && !st.wal_warning && !skipped && st.error ? ' error' : '');
    $('allow-pg').hidden = st.phase !== 'needs_config' || !st.admin_enabled;
    $('replay').hidden = !skipped || !st.admin_enabled || !!(st.warning || st.wal_warning || st.notice);

    const joining = st.phase === 'waiting_to_join' || st.phase === 'joining';
    $('join-panel').hidden = !joining;
    if (joining) {
      const p = st.pairing || {};
      $('pair-code').textContent = p.code || '— — —';
      $('join-title').textContent = st.phase === 'joining' ? (st.notice || 'กำลังเข้าร่วม…') :
        p.status === 'requested' ? `ส่งคำขอไปที่ ${String(p.seed_id || '').toUpperCase()} แล้ว — รออนุมัติ` :
        p.status === 'rejected' ? 'คำขอถูกปฏิเสธ — จะลองใหม่ใน 5 นาที' : 'กำลังค้นหาไซต์อื่นในวง LAN…';
      $('paste-form').hidden = st.phase === 'joining';
    }

    $('add-site').hidden = !running || demo || !st.admin_enabled;
    const reqs = m.requests || [];
    $('requests').hidden = reqs.length === 0;
    $('request-rows').innerHTML = reqs.map(r => `<li>
      <span class="rq-name">${esc(r.node_id)}</span><span class="rq-code">${esc(r.code)}</span>
      <span class="rq-meta">${esc(r.url)} · ${rel(r.created_at)}</span>
      <span class="rq-actions">${r.holder === m.self
        ? `<button type="button" data-approve="${esc(r.id)}">อนุมัติ</button><button type="button" class="reject" data-reject="${esc(r.id)}">ปฏิเสธ</button>`
        : `<span class="rq-meta">อนุมัติที่ dashboard ของ ${esc(r.holder).toUpperCase()}</span>`}</span></li>`).join('');

    const sel = m.byId[selected];
    $('remove-site').hidden = demo || !running || !st.admin_enabled || !sel || sel.id === m.self;
  }

  document.addEventListener('click', async e => {
    const a = e.target.closest('[data-approve],[data-reject]');
    if (!a) return;
    const id = a.dataset.approve || a.dataset.reject;
    if (a.dataset.approve && !confirm('รหัสจับคู่ตรงกับที่หน้าจอไซต์ใหม่แสดงใช่ไหม?')) return;
    try { await adminFetch(`v1/admin/requests/${id}/${a.dataset.approve ? 'approve' : 'reject'}`); tick(); }
    catch (err) { alert(err.message); }
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
    } catch (err) { alert(err.message); }
  });
  $('copy-invite').addEventListener('click', () => {
    navigator.clipboard.writeText($('invite-code').textContent).then(() => {
      $('copy-invite').textContent = 'คัดลอกแล้ว';
      setTimeout(() => { $('copy-invite').textContent = 'คัดลอก'; }, 1500);
    });
  });
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

  function setMode(text, cls) { const el = $('mode'); el.textContent = text; el.className = 'mode ' + (cls || ''); }

  // ---------- demo data (when opened without a Conduit server) ----------
  const demoState = { t0: Date.now(), seq: { host: 120, local: 64, branch: 31 }, acked: {} };
  function demoMesh() {
    const t = (Date.now() - demoState.t0) / 1000, cycle = t % 20;
    const localCut = cycle > 9 && cycle < 15;
    const ids = ['host', 'local', 'branch'], off = { host: 1, local: 2, branch: 3 };
    ids.forEach(id => { if (Math.random() < 0.5) demoState.seq[id] += 1; });
    const now = new Date().toISOString();
    const status = id => {
      const peers = ids.filter(p => p !== id).map(p => {
        const k = id + '>' + p, cut = localCut && (id === 'local' || p === 'local');
        if (!cut) demoState.acked[k] = Math.min(demoState.seq[id], (demoState.acked[k] ?? demoState.seq[id]) + 3);
        const acked = demoState.acked[k] ?? demoState.seq[id];
        return { id: p, url: `http://conduit-${p}:7420`, acked_seq: acked, backlog: demoState.seq[id] - acked,
          last_ok: cut ? new Date(Date.now() - (cycle - 9) * 1000).toISOString() : now,
          last_error: cut ? `Post "http://conduit-${p}:7420/v1/apply": dial tcp: lookup conduit-${p}: no such host` : '' };
      });
      const backlog = Math.max(...peers.map(p => p.backlog));
      return { node_id: id, sequences: { offset: off[id], step: 10 }, capture: { enabled: true, connected: true, captured_txs: demoState.seq[id] },
        outbox: { pending: backlog, max_seq: demoState.seq[id] }, peers,
        inbox: ids.filter(p => p !== id).map(p => ({ origin: p, applied_seq: demoState.seq[p] - 2 })),
        conflicts: id === 'local' ? { total: 2, recent: [
          { kind: 'update_update', table: 'public.customers', pk: '{"id":"231"}', origin: 'host', detected_at: new Date(Date.now() - 95e3).toISOString(), detail: 'local row was modified later' },
          { kind: 'unique_violation', table: 'public.customers', pk: '{"id":"261"}', origin: 'host', detected_at: new Date(Date.now() - 400e3).toISOString(), detail: 'duplicate key value violates unique constraint' }] }
          : { total: 0, recent: [] } };
    };
    return { self: 'host', nodes: ids.map(id => ({ id, self: id === 'host', reachable: !(localCut && id === 'local'), status: status(id) })) };
  }

  // ---------- polling ----------
  const forceDemo = new URLSearchParams(location.search).has('demo');
  async function tick() {
    let raw = null;
    if (!forceDemo && location.protocol !== 'file:') {
      try {
        const r = await fetch('v1/mesh', { cache: 'no-store' });
        if (r.ok) raw = await r.json();
      } catch (_) { /* fall through */ }
    }
    if (raw) { demo = false; everLive = true; setMode('LIVE'); }
    else if (everLive) { setMode('OFFLINE', 'offline'); $('updated').textContent = 'ติดต่อ Conduit ไม่ได้ · กำลังลองใหม่'; return; }
    else { demo = true; raw = demoMesh(); setMode('DEMO', 'demo'); }
    mesh = normalize(raw);
    render();
  }

  $('motion').addEventListener('click', () => { paused = !paused; render(); });
  (async function loop() { await tick(); setTimeout(loop, 2000); })();
})();
