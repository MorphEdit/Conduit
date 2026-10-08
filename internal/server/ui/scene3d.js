/*
 * Copyright 2026 MorphEdit (https://github.com/MorphEdit). All rights reserved.
 * Licensed under the PolyForm Strict License 1.0.0 - see LICENSE and NOTICE.
 * Required Notice: Copyright 2026 MorphEdit (https://github.com/MorphEdit)
 */
// The 3D map: every site is a glowing platform carrying its Postgres and its
// Conduit server; each pair of sites is joined by two arcs (one per direction)
// with packets travelling along them. Built with three.js (vendor/three).
import * as THREE from 'three';
import { OrbitControls } from 'three/addons/OrbitControls.js';
import { CSS2DRenderer, CSS2DObject } from 'three/addons/CSS2DRenderer.js';

const COLOR = { ok: 0x5fe5d9, busy: 0xf2b95c, warn: 0xf2b95c, down: 0xf27a7a, idle: 0x3a4a52 };
const PLAT = 110, PLAT_H = 6;   // platform size and thickness
const SPEED = { ok: 0.16, busy: 0.42, warn: 0.08 };

const esc = s => String(s ?? '').replace(/[&<>"]/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' })[c]);

export function createScene(container, { onSelect }) {
  let renderer;
  try {
    renderer = new THREE.WebGLRenderer({ antialias: true, alpha: true });
  } catch (_) {
    return null; // no WebGL: the panels still show everything
  }
  renderer.setPixelRatio(Math.min(window.devicePixelRatio || 1, 2));
  const canvas = renderer.domElement;
  canvas.tabIndex = 0;
  canvas.setAttribute('role', 'img');
  canvas.setAttribute('aria-label', 'ผัง 3 มิติของไซต์และเส้นซิงก์ — ใช้ลูกศรหรือ W A S D เพื่อเลื่อน, ปุ่มด้านซ้ายเพื่อซูมและหมุน');
  container.appendChild(canvas);
  const labels = new CSS2DRenderer();
  labels.domElement.className = 'label-layer';
  container.appendChild(labels.domElement);

  const scene = new THREE.Scene();
  scene.fog = new THREE.Fog(0x080c10, 900, 2200);
  const camera = new THREE.PerspectiveCamera(40, 1, 1, 5000);
  const controls = new OrbitControls(camera, canvas);
  controls.enableDamping = true;
  controls.dampingFactor = 0.08;
  controls.maxPolarAngle = Math.PI * 0.47;
  controls.minDistance = 120;
  controls.maxDistance = 4000;
  controls.screenSpacePanning = false;
  controls.keyPanSpeed = 18;
  controls.listenToKeyEvents(canvas); // arrow keys, only while the map has focus

  scene.add(new THREE.HemisphereLight(0xaee6ff, 0x0b1117, 1.1));
  const sun = new THREE.DirectionalLight(0xffffff, 1.4);
  sun.position.set(260, 520, 220);
  scene.add(sun);

  const grid = new THREE.GridHelper(2400, 60, 0x1f3d47, 0x132630);
  grid.material.transparent = true;
  grid.material.opacity = 0.55;
  scene.add(grid);

  const world = new THREE.Group();
  scene.add(world);

  let sites = new Map(), links = new Map(), pickables = [], radius = 200, topo = '';
  let paused = false, clock = new THREE.Clock(), t = 0;

  // ---------- building blocks ----------
  const mat = (color, extra = {}) => new THREE.MeshStandardMaterial({ color, roughness: 0.55, metalness: 0.25, ...extra });

  function makeSite(id) {
    const g = new THREE.Group();
    const plat = new THREE.Mesh(new THREE.BoxGeometry(PLAT, PLAT_H, PLAT), mat(0x17323b, { transparent: true, opacity: 0.6 }));
    plat.position.y = PLAT_H / 2;
    const edges = new THREE.LineSegments(new THREE.EdgesGeometry(plat.geometry), new THREE.LineBasicMaterial({ color: COLOR.ok }));
    edges.position.copy(plat.position);

    // Postgres: a cylinder with glowing rings
    const db = new THREE.Mesh(new THREE.CylinderGeometry(17, 17, 36, 40), mat(0x1c2b32));
    db.position.set(-24, PLAT_H + 18, 20);
    const ringMat = new THREE.MeshBasicMaterial({ color: COLOR.ok, transparent: true, opacity: 0.7 });
    const rings = [10, 22].map(h => {
      const r = new THREE.Mesh(new THREE.TorusGeometry(17.2, 0.7, 6, 48), ringMat);
      r.rotation.x = Math.PI / 2;
      r.position.set(-24, PLAT_H + h, 20);
      return r;
    });

    // Conduit: a server box with blinking LED strips on its front
    const srv = new THREE.Mesh(new THREE.BoxGeometry(32, 60, 26), mat(0x1e2a30));
    srv.position.set(22, PLAT_H + 30, -12);
    const ledMat = new THREE.MeshBasicMaterial({ color: COLOR.ok });
    const ledsGeo = new THREE.PlaneGeometry(20, 2.2);
    const leds = [];
    for (let k = 0; k < 6; k++) {
      const l = new THREE.Mesh(ledsGeo, ledMat.clone());
      l.position.set(22, PLAT_H + 52 - k * 8, 1.2);
      leds.push(l);
    }

    // captured changes flowing from Postgres to Conduit
    const cap = new THREE.QuadraticBezierCurve3(new THREE.Vector3(-24, PLAT_H + 37, 20), new THREE.Vector3(0, PLAT_H + 70, 6), new THREE.Vector3(14, PLAT_H + 46, 1));
    const capLine = new THREE.Line(new THREE.BufferGeometry().setFromPoints(cap.getPoints(24)),
      new THREE.LineDashedMaterial({ color: COLOR.ok, dashSize: 3, gapSize: 3, transparent: true, opacity: 0.8 }));
    capLine.computeLineDistances();
    const capDot = new THREE.Mesh(new THREE.SphereGeometry(1.8, 10, 10), new THREE.MeshBasicMaterial({ color: COLOR.ok }));

    // status halo on the floor: pulses when something is wrong
    const halo = new THREE.Mesh(new THREE.RingGeometry(PLAT * 0.78, PLAT * 0.86, 64),
      new THREE.MeshBasicMaterial({ color: COLOR.ok, transparent: true, opacity: 0, side: THREE.DoubleSide, depthWrite: false }));
    halo.rotation.x = -Math.PI / 2;
    halo.position.y = 0.5;

    const el = document.createElement('div');
    el.className = 'site-label';
    el.addEventListener('click', () => onSelect(id));
    const label = new CSS2DObject(el);
    label.position.set(0, PLAT_H + 92, 0);

    g.add(plat, edges, db, ...rings, srv, ...leds, capLine, capDot, halo, label);
    [plat, db, srv].forEach(o => { o.userData.site = id; pickables.push(o); });
    world.add(g);
    return { g, plat, edges, rings, ringMat, leds, cap, capLine, capDot, halo, el, state: 'ok', up: true, capOn: true };
  }

  function makeLane(a, b, side) {
    const pa = a.clone().setY(PLAT_H + 64), pb = b.clone().setY(PLAT_H + 64);
    const dir = pb.clone().sub(pa), perp = new THREE.Vector3(-dir.z, 0, dir.x).normalize().multiplyScalar(side * 7);
    pa.add(perp); pb.add(perp);
    const mid = pa.clone().add(pb).multiplyScalar(0.5).add(new THREE.Vector3(0, Math.min(90, dir.length() * 0.22), 0));
    const curve = new THREE.QuadraticBezierCurve3(pa, mid, pb);
    const tubeMat = new THREE.MeshBasicMaterial({ color: COLOR.ok, transparent: true, opacity: 0.55, depthWrite: false });
    const tube = new THREE.Mesh(new THREE.TubeGeometry(curve, 48, 1.1, 6, false), tubeMat);
    const dashed = new THREE.Line(new THREE.BufferGeometry().setFromPoints(curve.getPoints(64)),
      new THREE.LineDashedMaterial({ color: COLOR.down, dashSize: 8, gapSize: 7, transparent: true, opacity: 0.9 }));
    dashed.computeLineDistances();
    const pkts = [0, 1 / 3, 2 / 3].map(o => {
      const core = new THREE.Mesh(new THREE.SphereGeometry(2.4, 12, 12), new THREE.MeshBasicMaterial({ color: 0xdffff9 }));
      const glow = new THREE.Mesh(new THREE.SphereGeometry(6, 12, 12), new THREE.MeshBasicMaterial({ color: COLOR.ok, transparent: true, opacity: 0.28, depthWrite: false, blending: THREE.AdditiveBlending }));
      core.add(glow);
      core.userData.offset = o;
      return core;
    });
    world.add(tube, dashed, ...pkts);
    return { curve, tube, tubeMat, dashed, pkts, state: 'ok', pos: 0 };
  }

  function clear() {
    world.traverse(o => {
      if (o.geometry) o.geometry.dispose();
      if (o.material) [].concat(o.material).forEach(m => m.dispose());
      if (o.isCSS2DObject) o.element.remove();
    });
    world.clear();
    sites = new Map(); links = new Map(); pickables = [];
  }

  // Lay the sites on a circle, this site nearest the camera.
  function build(model) {
    clear();
    const ids = model.nodes.map(n => n.id), n = ids.length;
    radius = n <= 1 ? 0 : Math.max(170, n * 62);
    const pos = {};
    const start = Math.PI / 2 + (n === 2 ? Math.PI / 3 : 0); // two sites: side by side, not one behind the other
    ids.forEach((id, i) => {
      const a = start + i * 2 * Math.PI / n;
      pos[id] = new THREE.Vector3(radius * Math.cos(a), 0, radius * Math.sin(a));
      const s = makeSite(id);
      s.g.position.copy(pos[id]);
      sites.set(id, s);
    });
    model.links.forEach(l => {
      const key = l.a + '|' + l.b;
      const el = document.createElement('div');
      el.className = 'link-label';
      const label = new CSS2DObject(el);
      const ab = makeLane(pos[l.a], pos[l.b], 1), ba = makeLane(pos[l.b], pos[l.a], 1);
      label.position.copy(ab.curve.getPoint(0.5)).add(ba.curve.getPoint(0.5)).multiplyScalar(0.5).add(new THREE.Vector3(0, 10, 0));
      world.add(label);
      links.set(key, { a: l.a, b: l.b, ab, ba, el });
    });
    home();
  }

  // ---------- per-poll update ----------
  function paintLane(lane, state, dim) {
    lane.state = state;
    const down = state === 'down';
    lane.tube.visible = !down;
    lane.dashed.visible = down;
    lane.tubeMat.color.setHex(COLOR[state] ?? COLOR.idle);
    lane.tubeMat.opacity = dim ? 0.18 : 0.55;
    lane.dashed.material.opacity = dim ? 0.35 : 0.9;
    lane.pkts.forEach(p => {
      p.visible = !down;
      p.children[0].material.color.setHex(COLOR[state] ?? COLOR.idle);
      p.children[0].material.opacity = dim ? 0.12 : 0.28;
      p.material.opacity = dim ? 0.4 : 1;
      p.material.transparent = dim;
    });
  }

  function update(model, selected, text) {
    const key = model.nodes.map(n => n.id).join('|') + '#' + model.links.map(l => l.a + l.b).join(',');
    if (key !== topo) { topo = key; build(model); }
    model.nodes.forEach(n => {
      const s = sites.get(n.id);
      if (!s) return;
      const state = !n.up ? 'down' : n.state;
      s.state = state; s.up = n.up;
      s.selected = n.id === selected;
      const c = COLOR[state];
      s.edges.material.color.setHex(s.selected ? 0xb9fff2 : c);
      s.plat.material.color.setHex(s.selected ? 0x1d4a48 : n.up ? 0x17323b : 0x2a1a1c);
      s.ringMat.color.setHex(n.up ? COLOR.ok : COLOR.down);
      s.leds.forEach(l => l.material.color.setHex(n.up ? (state === 'ok' ? COLOR.ok : COLOR.busy) : COLOR.down));
      s.capOn = n.up && n.st.capture && n.st.capture.connected;
      s.capLine.material.color.setHex(s.capOn ? COLOR.ok : COLOR.idle);
      s.capDot.visible = s.capOn;
      s.halo.material.color.setHex(state === 'ok' ? COLOR.ok : c);
      s.el.className = 'site-label ' + state + (s.selected ? ' selected' : '');
      s.el.innerHTML = `<span class="n">${esc(n.id.toUpperCase())}</span><span class="s">${esc(text(n))}</span>`;
      s.el.setAttribute('role', 'button');
      s.el.setAttribute('aria-label', `เลือกไซต์ ${n.id}`);
    });
    model.links.forEach(l => {
      const L = links.get(l.a + '|' + l.b);
      if (!L) return;
      const dim = selected && l.a !== selected && l.b !== selected;
      paintLane(L.ab, l.ab ? l.ab.state : 'down', dim);
      paintLane(L.ba, l.ba ? l.ba.state : 'down', dim);
      const backlog = Math.max(l.ab && l.ab.peer ? l.ab.peer.backlog : 0, l.ba && l.ba.peer ? l.ba.peer.backlog : 0);
      const down = l.state === 'down';
      L.el.hidden = !(down || backlog > 0);
      L.el.className = 'link-label' + (down ? ' down' : '');
      L.el.textContent = down ? 'ขาดการเชื่อมต่อ' : `ค้าง ${backlog.toLocaleString('th-TH')}`;
    });
  }

  // ---------- camera ----------
  function home() {
    const r = Math.max(radius, 120);
    // narrow views (phones, a wide side panel) need the camera further back
    const w = container.clientWidth - inset.left - inset.right, h = container.clientHeight || 1;
    const k = Math.max(1, Math.pow(1.5 / Math.max(w / h, 0.3), 0.45));
    controls.target.set(0, 30, 0);
    camera.position.set(r * 0.3 * k, (r * 1.45 + 300) * k, (r * 2.1 + 360) * k);
    controls.update();
  }
  function zoom(f) {
    const off = camera.position.clone().sub(controls.target).multiplyScalar(f);
    const d = THREE.MathUtils.clamp(off.length(), controls.minDistance, controls.maxDistance);
    camera.position.copy(controls.target).add(off.setLength(d));
  }
  function rotate(deg) {
    const off = camera.position.clone().sub(controls.target).applyAxisAngle(new THREE.Vector3(0, 1, 0), THREE.MathUtils.degToRad(deg));
    camera.position.copy(controls.target).add(off);
  }
  function pan(dx, dz) {
    // move along the floor, relative to where the camera looks
    const fwd = controls.target.clone().sub(camera.position).setY(0).normalize();
    const right = new THREE.Vector3().crossVectors(fwd, new THREE.Vector3(0, 1, 0));
    const step = right.multiplyScalar(dx).add(fwd.multiplyScalar(dz)).multiplyScalar(Math.max(radius, 150) * 0.25);
    camera.position.add(step);
    controls.target.add(step);
  }
  const wasd = { KeyW: [0, 1], KeyS: [0, -1], KeyA: [-1, 0], KeyD: [1, 0] };
  canvas.addEventListener('keydown', e => { const m = wasd[e.code]; if (m) { pan(m[0] * 0.3, m[1] * 0.3); e.preventDefault(); } });

  // click (not drag) on a site selects it
  const ray = new THREE.Raycaster(), ptr = new THREE.Vector2();
  let down = null;
  canvas.addEventListener('pointerdown', e => { down = [e.clientX, e.clientY]; });
  canvas.addEventListener('pointerup', e => {
    if (!down || Math.hypot(e.clientX - down[0], e.clientY - down[1]) > 5) return;
    const r = canvas.getBoundingClientRect();
    ptr.set(((e.clientX - r.left) / r.width) * 2 - 1, -((e.clientY - r.top) / r.height) * 2 + 1);
    ray.setFromCamera(ptr, camera);
    const hit = ray.intersectObjects(pickables, false)[0];
    if (hit) onSelect(hit.object.userData.site);
  });
  canvas.addEventListener('pointermove', e => {
    const r = canvas.getBoundingClientRect();
    ptr.set(((e.clientX - r.left) / r.width) * 2 - 1, -((e.clientY - r.top) / r.height) * 2 + 1);
    ray.setFromCamera(ptr, camera);
    canvas.style.cursor = ray.intersectObjects(pickables, false).length ? 'pointer' : '';
  });

  // ---------- size + render loop ----------
  // Panels float over the map; centre the picture in the space between them.
  let inset = { left: 0, right: 0 };
  function resize() {
    const w = container.clientWidth, h = container.clientHeight;
    if (!w || !h) return;
    renderer.setSize(w, h);
    labels.setSize(w, h);
    camera.aspect = w / h;
    const shift = (inset.right - inset.left) / 2;
    if (shift) camera.setViewOffset(w, h, shift, 0, w, h); else camera.clearViewOffset();
    camera.updateProjectionMatrix();
  }
  new ResizeObserver(resize).observe(container);
  resize();

  function frame() {
    requestAnimationFrame(frame);
    if (document.hidden) return;
    const dt = Math.min(clock.getDelta(), 0.1);
    if (!paused) t += dt;
    sites.forEach(s => {
      const bad = s.state !== 'ok';
      const p = bad && !paused ? 1 + 0.12 * Math.sin(t * 4) : 1;
      s.halo.scale.setScalar(p);
      s.halo.material.opacity = bad ? 0.55 + (paused ? 0 : 0.25 * Math.sin(t * 4)) : s.selected ? 0.45 : 0.12;
      s.leds.forEach((l, i) => { l.material.opacity = 1; l.visible = !s.up || paused || Math.sin(t * 6 + i * 1.7) > -0.6; });
      if (s.capDot.visible) s.capDot.position.copy(s.cap.getPointAt((t * 0.8) % 1));
    });
    links.forEach(L => [L.ab, L.ba].forEach(lane => {
      if (lane.state === 'down') return;
      lane.pos += paused ? 0 : dt * (SPEED[lane.state] ?? SPEED.ok);
      lane.pkts.forEach(p => p.position.copy(lane.curve.getPointAt((lane.pos + p.userData.offset) % 1)));
    }));
    controls.update();
    renderer.render(scene, camera);
    labels.render(scene, camera);
  }
  frame();

  return {
    update,
    setPaused(p) { paused = p; },
    setInsets(left, right) { inset = { left, right }; resize(); },
    zoom, rotate, pan, home,
  };
}
