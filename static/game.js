// Webshooter client with client-side prediction and entity interpolation.
//
// The server is authoritative. To hide network latency we:
//   * PREDICT our own player: apply inputs locally and immediately, then
//     reconcile against the server's authoritative position by replaying any
//     inputs it hasn't acknowledged yet.
//   * INTERPOLATE everyone else: render them ~RENDER_DELAY ms in the past,
//     smoothly blending between the two snapshots that bracket that time, so
//     30Hz server updates look fluid.

const canvas = document.getElementById("game");
const ctx = canvas.getContext("2d");

// How far in the past we render remote entities. Must comfortably exceed the
// server's snapshot interval (~33ms at 30Hz) so we always have two snapshots to
// interpolate between.
const RENDER_DELAY = 100;
const INPUT_HZ = 60;

// Movement constants — overwritten by the server's init message so prediction
// uses identical math. Defaults match the server.
let cfg = { width: 1024, height: 768, speed: 220, radius: 14, winScore: 10 };
let obstacles = [];
let level = { num: 1, count: 1, name: "" };

let myID = null;
let ws = null;

// --- tank sprites ---
// Grayscale hull + turret SVGs, tinted per team color at load time. If they
// fail to load, drawPlayer falls back to procedural drawing.
const TEAM_COLORS = ["#4cc9f0", "#ef476f"];
const tankSprites = { hull: null, turret: null };
const tintCache = {}; // color -> { hull: <canvas>, turret: <canvas> }
let spritesReady = false;

function loadSprites() {
  const hull = new Image();
  const turret = new Image();
  let loaded = 0;
  const onReady = () => {
    if (++loaded < 2) return;
    tankSprites.hull = hull;
    tankSprites.turret = turret;
    for (const c of TEAM_COLORS) tintFor(c);
    spritesReady = true;
  };
  hull.onload = onReady;
  turret.onload = onReady;
  hull.onerror = turret.onerror = () => { spritesReady = false; };
  hull.src = "sprites/tank-hull.svg";
  turret.src = "sprites/tank-turret.svg";
}

// tintFor returns (and caches) the team-colored hull/turret for a color, by
// multiplying the grayscale sprite by the color and clipping to its alpha.
function tintFor(color) {
  if (tintCache[color]) return tintCache[color];
  if (!tankSprites.hull) return null;
  tintCache[color] = {
    hull: makeTinted(tankSprites.hull, color),
    turret: makeTinted(tankSprites.turret, color),
  };
  return tintCache[color];
}

function makeTinted(img, color) {
  const c = document.createElement("canvas");
  c.width = img.width;
  c.height = img.height;
  const x = c.getContext("2d");
  x.drawImage(img, 0, 0);                       // grayscale shading
  x.globalCompositeOperation = "multiply";      // tint, preserving shading
  x.fillStyle = color;
  x.fillRect(0, 0, c.width, c.height);
  x.globalCompositeOperation = "destination-in"; // clip color back to sprite alpha
  x.drawImage(img, 0, 0);
  return c;
}

loadSprites();

// Per-player hull facing: the hull rotates toward the player's movement
// direction (smoothed), independently of the turret which tracks aim.
const hullState = {}; // id -> { x, y, ang }

function hullFacing(id, pos, fallbackAng) {
  let s = hullState[id];
  if (!s) {
    s = hullState[id] = { x: pos.x, y: pos.y, ang: fallbackAng };
  }
  const dx = pos.x - s.x, dy = pos.y - s.y;
  const d = Math.hypot(dx, dy);
  if (d > 0.4 && d < 60) {
    // Moving (and not a respawn teleport): ease the hull toward travel direction.
    s.ang = lerpAngle(s.ang, Math.atan2(dy, dx), 0.3);
  }
  s.x = pos.x;
  s.y = pos.y;
  return s.ang;
}

function lerpAngle(a, b, t) {
  const diff = Math.atan2(Math.sin(b - a), Math.cos(b - a)); // shortest path
  return a + diff * t;
}

// --- audio ---
// Sounds are synthesized with the Web Audio API (no asset files). The context
// must be created/resumed after a user gesture, which we do on the click that
// locks the mouse. Volume is scaled by distance from our own player.
let audioCtx = null;
let masterGain = null;
let maxBulletSeen = 0; // highest bullet id we've heard fire (for shot detection)

function initAudio() {
  if (audioCtx) {
    if (audioCtx.state === "suspended") audioCtx.resume();
    return;
  }
  const AC = window.AudioContext || window.webkitAudioContext;
  if (!AC) return;
  audioCtx = new AC();
  masterGain = audioCtx.createGain();
  masterGain.gain.value = 0.45;
  masterGain.connect(audioCtx.destination);
}

// Louder when the event is near our player, quieter far away.
function spatialGain(x, y) {
  if (!predicted) return 1;
  const d = Math.hypot(x - predicted.x, y - predicted.y);
  return Math.max(0.15, 1 - d / 900);
}

// A short pitched zap for firing.
function playShot(vol) {
  if (!audioCtx) return;
  const t = audioCtx.currentTime;
  const o = audioCtx.createOscillator();
  const g = audioCtx.createGain();
  o.type = "square";
  o.frequency.setValueAtTime(820, t);
  o.frequency.exponentialRampToValueAtTime(140, t + 0.11);
  g.gain.setValueAtTime(0.0001, t);
  g.gain.exponentialRampToValueAtTime(0.3 * vol, t + 0.004);
  g.gain.exponentialRampToValueAtTime(0.0001, t + 0.12);
  o.connect(g).connect(masterGain);
  o.start(t);
  o.stop(t + 0.13);
}

// A filtered noise burst with a falling cutoff for the boom.
function playExplosion(vol) {
  if (!audioCtx) return;
  const t = audioCtx.currentTime;
  const dur = 0.5;
  const buf = audioCtx.createBuffer(1, Math.floor(audioCtx.sampleRate * dur), audioCtx.sampleRate);
  const data = buf.getChannelData(0);
  for (let i = 0; i < data.length; i++) data[i] = Math.random() * 2 - 1;

  const src = audioCtx.createBufferSource();
  src.buffer = buf;
  const lp = audioCtx.createBiquadFilter();
  lp.type = "lowpass";
  lp.frequency.setValueAtTime(1100, t);
  lp.frequency.exponentialRampToValueAtTime(120, t + dur);
  const g = audioCtx.createGain();
  g.gain.setValueAtTime(0.8 * vol, t);
  g.gain.exponentialRampToValueAtTime(0.0001, t + dur);
  src.connect(lp).connect(g).connect(masterGain);
  src.start(t);
  src.stop(t + dur);
}

// Local prediction state.
let predicted = null; // {x, y} of our own player
let seq = 0;
let pending = []; // inputs sent but not yet acknowledged by the server

// Snapshot buffer for interpolation. Each entry: {t, players:{id->view}, bullets}.
let snapshots = [];

// Active explosions and the last-known alive state per player (to detect the
// alive->dead transition that triggers one). Purely a client-side effect.
const explosions = [];
const lastAlive = {};
const EXPLOSION_MS = 600;

function spawnExplosion(x, y) {
  const particles = [];
  const n = 14;
  for (let i = 0; i < n; i++) {
    const a = (i / n) * Math.PI * 2 + Math.random() * 0.5;
    particles.push({ a, speed: 70 + Math.random() * 140, size: 2 + Math.random() * 3 });
  }
  explosions.push({ x, y, start: performance.now(), particles });
}

const keys = { forward: false, back: false, strafeLeft: false, strafeRight: false, fire: false };

// Facing is a heading angle (radians). 0 points right (+x); +angle turns
// clockwise on screen (toward +y). The aim vector we send is (cos, sin) of it.
//
// Mouse motion accumulates into turnAccum (in pixels) and is integrated into
// heading once per fixed input tick — so turning is driven by the fixed
// timestep, not the asynchronous mousemove event rate or the render frame rate.
let heading = 0;
let turnAccum = 0; // unprocessed horizontal mouse delta, in pixels
const TURN_SENSITIVITY = 0.0028; // radians per pixel of mouse movement

// --- networking ---
function connect() {
  const proto = location.protocol === "https:" ? "wss" : "ws";
  ws = new WebSocket(`${proto}://${location.host}/ws`);

  ws.onmessage = (ev) => {
    const msg = JSON.parse(ev.data);
    if (msg.type === "init") {
      myID = msg.id;
      cfg = { width: msg.width, height: msg.height, speed: msg.speed, radius: msg.radius, winScore: msg.winScore };
      obstacles = msg.obstacles || [];
      level = { num: msg.level, count: msg.levelCount, name: msg.levelName };
      canvas.width = msg.width;
      canvas.height = msg.height;
    } else if (msg.type === "level") {
      // Level changed between rounds: swap obstacles (used for both rendering
      // and prediction collision) and update the HUD.
      obstacles = msg.obstacles || [];
      level = { num: msg.level, count: msg.levelCount, name: msg.levelName };
    } else if (msg.type === "state") {
      onState(msg);
    }
  };

  ws.onclose = () => {
    predicted = null;
    pending = [];
    // The server may have restarted; reset id tracking so its fresh (low) ids
    // are recognised as new and don't suppress shot sounds / explosions.
    maxBulletSeen = 0;
    for (const k in lastAlive) delete lastAlive[k];
    setTimeout(connect, 1000); // auto-reconnect
  };
}

function onState(msg) {
  // Buffer the snapshot (keyed by player id) for interpolation.
  const players = {};
  for (const p of msg.players || []) players[p.id] = p;

  // Spawn an explosion (and boom) wherever a player just died (alive -> dead).
  // The server leaves a dead player at the spot they were hit until respawn.
  for (const id in players) {
    const p = players[id];
    if (lastAlive[id] === true && !p.alive) {
      spawnExplosion(p.pos.x, p.pos.y);
      playExplosion(spatialGain(p.pos.x, p.pos.y));
    }
    lastAlive[id] = p.alive;
  }

  // A bullet whose id we haven't seen yet means a shot was just fired. Bullet
  // ids increase monotonically, so anything above maxBulletSeen is new.
  let shotPos = null;
  let newMax = maxBulletSeen;
  for (const b of msg.bullets || []) {
    if (b.id > maxBulletSeen && !shotPos) shotPos = b.pos;
    if (b.id > newMax) newMax = b.id;
  }
  maxBulletSeen = newMax;
  if (shotPos) playShot(spatialGain(shotPos.x, shotPos.y));
  snapshots.push({
    t: performance.now(),
    players,
    bullets: msg.bullets || [],
    // Carry match state through so the HUD/overlay (which read latest()) see it.
    phase: msg.phase,
    winner: msg.winner,
    timeLeft: msg.timeLeft,
    resetIn: msg.resetIn,
  });
  while (snapshots.length > 2 && snapshots[0].t < performance.now() - 1000) {
    snapshots.shift();
  }

  // Reconcile our own player: snap to the authoritative position, drop acked
  // inputs, then replay everything the server hasn't seen yet.
  const me = players[myID];
  if (me) {
    if (!predicted) predicted = { x: me.pos.x, y: me.pos.y };
    else {
      predicted.x = me.pos.x;
      predicted.y = me.pos.y;
    }
    pending = pending.filter((cmd) => cmd.seq > me.seq);
    for (const cmd of pending) integrate(predicted, cmd);
  }
}

// --- input + prediction loop (fixed rate, independent of rendering) ---
let lastInputT = performance.now();

function inputTick() {
  const now = performance.now();
  let dt = (now - lastInputT) / 1000;
  lastInputT = now;
  if (dt > 0.05) dt = 0.05; // matches server maxInputDt clamp

  if (!predicted) return; // wait until we know where we are

  // Integrate accumulated mouse motion into the heading at the fixed timestep.
  heading += turnAccum * TURN_SENSITIVITY;
  turnAccum = 0;

  // Aim is the unit vector of our heading (driven by mouse turn, not cursor).
  const cmd = {
    seq: ++seq,
    forward: keys.forward,
    back: keys.back,
    strafeLeft: keys.strafeLeft,
    strafeRight: keys.strafeRight,
    aimX: Math.cos(heading),
    aimY: Math.sin(heading),
    fire: keys.fire,
    dt,
  };

  integrate(predicted, cmd); // predict locally, right now
  pending.push(cmd);
  if (ws && ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify(cmd));
}

// integrate applies one input command to a position. Movement is relative to
// the aim direction (forward = along heading, strafe = perpendicular). This
// MUST match the server's applyInput math or prediction will visibly snap.
function integrate(pos, cmd) {
  // forward unit vector from the aim; strafe-right is 90° clockwise of it.
  const am = Math.hypot(cmd.aimX, cmd.aimY);
  const fx = am > 0 ? cmd.aimX / am : 0;
  const fy = am > 0 ? cmd.aimY / am : 0;
  const rx = -fy, ry = fx;

  let dx = 0, dy = 0;
  if (cmd.forward) { dx += fx; dy += fy; }
  if (cmd.back) { dx -= fx; dy -= fy; }
  if (cmd.strafeRight) { dx += rx; dy += ry; }
  if (cmd.strafeLeft) { dx -= rx; dy -= ry; }

  const m = Math.hypot(dx, dy);
  if (m > 0) { dx /= m; dy /= m; }
  pos.x = clamp(pos.x + dx * cfg.speed * cmd.dt, cfg.radius, cfg.width - cfg.radius);
  pos.y = clamp(pos.y + dy * cfg.speed * cmd.dt, cfg.radius, cfg.height - cfg.radius);
  resolveObstacles(pos, cfg.radius);
}

// Push the circle out of any overlapping obstacle. Mirror of the server's
// resolveObstacles/pushOut — keep the math identical or prediction will snap.
function resolveObstacles(pos, r) {
  for (const o of obstacles) pushOut(pos, r, o);
}

function pushOut(pos, r, o) {
  const minX = o.x, minY = o.y, maxX = o.x + o.w, maxY = o.y + o.h;
  const cx = clamp(pos.x, minX, maxX);
  const cy = clamp(pos.y, minY, maxY);
  const dx = pos.x - cx, dy = pos.y - cy;
  const d2 = dx * dx + dy * dy;
  if (d2 >= r * r) return;
  if (d2 > 1e-9) {
    const d = Math.sqrt(d2), push = r - d;
    pos.x += (dx / d) * push;
    pos.y += (dy / d) * push;
    return;
  }
  // Center inside the rect: eject along the nearest edge.
  const left = pos.x - minX, right = maxX - pos.x;
  const top = pos.y - minY, bottom = maxY - pos.y;
  const mn = Math.min(left, right, top, bottom);
  if (mn === left) pos.x = minX - r;
  else if (mn === right) pos.x = maxX + r;
  else if (mn === top) pos.y = minY - r;
  else pos.y = maxY + r;
}

// --- input handling ---
const keyMap = {
  KeyW: "forward", ArrowUp: "forward",
  KeyS: "back", ArrowDown: "back",
  KeyA: "strafeLeft", ArrowLeft: "strafeLeft",
  KeyD: "strafeRight", ArrowRight: "strafeRight",
};

window.addEventListener("keydown", (e) => {
  if (keyMap[e.code]) { keys[keyMap[e.code]] = true; e.preventDefault(); }
  if (e.code === "Space") { keys.fire = true; e.preventDefault(); }
});
window.addEventListener("keyup", (e) => {
  if (keyMap[e.code]) { keys[keyMap[e.code]] = false; e.preventDefault(); }
  if (e.code === "Space") { keys.fire = false; e.preventDefault(); }
});

// Mouse turning via Pointer Lock: horizontal motion rotates the heading. Lock
// is required so deltas keep arriving (the cursor never reaches a screen edge).
const isLocked = () => document.pointerLockElement === canvas;

canvas.addEventListener("mousedown", () => {
  initAudio(); // create/resume audio on the user gesture (autoplay policy)
  if (isLocked()) keys.fire = true;
  else canvas.requestPointerLock(); // first click grabs the mouse
});
window.addEventListener("mouseup", () => { keys.fire = false; });

document.addEventListener("mousemove", (e) => {
  if (isLocked()) turnAccum += e.movementX; // applied in inputTick
});

// Stop firing if the pointer is released (Esc); turning resumes on next lock.
document.addEventListener("pointerlockchange", () => {
  if (!isLocked()) keys.fire = false;
});

canvas.addEventListener("contextmenu", (e) => e.preventDefault());

// --- interpolation ---
// Returns remote players (excluding us) positioned at RENDER_DELAY in the past.
function interpolatedRemotes() {
  if (snapshots.length === 0) return [];
  const rt = performance.now() - RENDER_DELAY;

  // Find the first snapshot at or after the render time.
  let i = 0;
  while (i < snapshots.length && snapshots[i].t < rt) i++;

  // Not enough history (render time precedes oldest) or no future snapshot yet:
  // fall back to the nearest available snapshot without blending.
  if (i === 0) return remotesFrom(snapshots[0].players, null, 0);
  if (i >= snapshots.length) return remotesFrom(snapshots[snapshots.length - 1].players, null, 0);

  const s0 = snapshots[i - 1], s1 = snapshots[i];
  const span = s1.t - s0.t;
  const a = span > 0 ? (rt - s0.t) / span : 0;
  return remotesFrom(s1.players, s0.players, a);
}

// Builds the remote-player list from a target snapshot, optionally lerping from
// a previous one by factor a.
function remotesFrom(target, prev, a) {
  const out = [];
  for (const id in target) {
    if (id === myID) continue;
    const p1 = target[id];
    const p0 = prev ? prev[id] : null;
    if (!p0) { out.push(p1); continue; }
    out.push({
      id,
      pos: { x: lerp(p0.pos.x, p1.pos.x, a), y: lerp(p0.pos.y, p1.pos.y, a) },
      aim: p1.aim,
      score: p1.score,
      alive: p1.alive,
    });
  }
  return out;
}

function latest() {
  return snapshots.length ? snapshots[snapshots.length - 1] : null;
}

// --- rendering ---
function draw() {
  ctx.clearRect(0, 0, canvas.width, canvas.height);
  drawGrid();
  drawObstacles();

  const snap = latest();

  // Bullets: a hot core with a soft glow.
  for (const b of (snap ? snap.bullets : [])) {
    const g = ctx.createRadialGradient(b.pos.x, b.pos.y, 0, b.pos.x, b.pos.y, 9);
    g.addColorStop(0, "rgba(255, 233, 150, 0.95)");
    g.addColorStop(0.4, "rgba(255, 209, 102, 0.55)");
    g.addColorStop(1, "rgba(255, 209, 102, 0)");
    ctx.fillStyle = g;
    ctx.beginPath();
    ctx.arc(b.pos.x, b.pos.y, 9, 0, Math.PI * 2);
    ctx.fill();
    ctx.fillStyle = "#fff6da";
    ctx.beginPath();
    ctx.arc(b.pos.x, b.pos.y, 2.5, 0, Math.PI * 2);
    ctx.fill();
  }

  // Remote players, interpolated.
  for (const p of interpolatedRemotes()) {
    drawPlayer(p.pos, p.aim, p.alive, "#ef476f", false, p.id);
  }

  // Our own player, predicted (no interpolation — instant response).
  if (predicted && snap) {
    const me = snap.players[myID];
    const alive = me ? me.alive : true;
    const aim = { x: Math.cos(heading), y: Math.sin(heading) };
    drawPlayer(predicted, aim, alive, "#4cc9f0", true, myID);
  }

  drawExplosions();

  drawScores(snap);
  if (snap) drawClock(snap);
  if (snap && snap.phase === "ended") drawWinOverlay(snap);
  requestAnimationFrame(draw);
}

// drawExplosions renders and ages active explosions: an expanding shockwave
// ring, a fiery central flash, and flung debris particles. Each lasts
// EXPLOSION_MS, then is removed.
function drawExplosions() {
  const now = performance.now();
  for (let i = explosions.length - 1; i >= 0; i--) {
    const e = explosions[i];
    const t = (now - e.start) / EXPLOSION_MS; // 0..1 progress
    if (t >= 1) {
      explosions.splice(i, 1);
      continue;
    }
    const tSec = (now - e.start) / 1000;

    // Fiery central flash, fading out.
    const flashR = cfg.radius * (1.8 - t * 1.2);
    const fg = ctx.createRadialGradient(e.x, e.y, 0, e.x, e.y, flashR);
    fg.addColorStop(0, `rgba(255, 245, 200, ${1 - t})`);
    fg.addColorStop(0.5, `rgba(255, 140, 50, ${(1 - t) * 0.7})`);
    fg.addColorStop(1, "rgba(255, 80, 30, 0)");
    ctx.fillStyle = fg;
    ctx.beginPath();
    ctx.arc(e.x, e.y, flashR, 0, Math.PI * 2);
    ctx.fill();

    // Expanding shockwave ring.
    ctx.globalAlpha = (1 - t) * 0.7;
    ctx.strokeStyle = "#ffd166";
    ctx.lineWidth = 1 + 3 * (1 - t);
    ctx.beginPath();
    ctx.arc(e.x, e.y, 8 + t * cfg.radius * 3, 0, Math.PI * 2);
    ctx.stroke();

    // Debris flung outward, decelerating in apparent size.
    ctx.globalAlpha = 1 - t;
    ctx.fillStyle = "#ffb347";
    for (const p of e.particles) {
      const d = p.speed * tSec;
      ctx.beginPath();
      ctx.arc(e.x + Math.cos(p.a) * d, e.y + Math.sin(p.a) * d, p.size * (1 - t), 0, Math.PI * 2);
      ctx.fill();
    }
    ctx.globalAlpha = 1;
  }
}

function drawClock(snap) {
  if (!Number.isFinite(snap.timeLeft)) return; // old/missing field — skip rather than render NaN
  const secs = Math.max(0, Math.ceil(snap.timeLeft));
  const mm = Math.floor(secs / 60);
  const ss = String(secs % 60).padStart(2, "0");
  ctx.font = "bold 18px system-ui, sans-serif";
  ctx.textAlign = "center";
  ctx.fillStyle = secs <= 10 && snap.phase === "playing" ? "#ef476f" : "#cdd";
  ctx.fillText(`${mm}:${ss}`, canvas.width / 2, 24);

  // Level indicator under the clock.
  if (level.count > 1) {
    ctx.font = "12px system-ui, sans-serif";
    ctx.fillStyle = "#889";
    const name = level.name ? ` · ${level.name}` : "";
    ctx.fillText(`level ${level.num}/${level.count}${name}`, canvas.width / 2, 42);
  }
  ctx.textAlign = "left";
}

function drawWinOverlay(snap) {
  ctx.fillStyle = "rgba(17, 19, 26, 0.72)";
  ctx.fillRect(0, 0, canvas.width, canvas.height);

  const cx = canvas.width / 2;
  ctx.textAlign = "center";

  const isDraw = !snap.winner;
  const winner = snap.players[snap.winner];
  let label, color;
  if (isDraw) {
    label = "DRAW";
    color = "#cdd";
  } else if (snap.winner === myID) {
    label = "YOU WIN!";
    color = "#4cc9f0";
  } else {
    label = `PLAYER ${snap.winner} WINS`;
    color = "#ef476f";
  }
  ctx.fillStyle = color;
  ctx.font = "bold 48px system-ui, sans-serif";
  ctx.fillText(label, cx, canvas.height / 2 - 30);

  if (!isDraw) {
    ctx.fillStyle = "#cdd";
    ctx.font = "20px system-ui, sans-serif";
    const frags = winner ? winner.score : cfg.winScore;
    ctx.fillText(`${frags} frags`, cx, canvas.height / 2 + 6);
  }

  ctx.fillStyle = "#889";
  ctx.font = "16px system-ui, sans-serif";
  // With multiple levels, each win advances to the next one.
  const next = level.count > 1 ? "Next level" : "New round";
  ctx.fillText(`${next} in ${Math.ceil(snap.resetIn)}…`, cx, canvas.height / 2 + 40);

  ctx.textAlign = "left";
}

// drawPlayer renders a tank with an independent hull (faces movement) and
// turret (faces aim), using tinted sprites when available, with a drop shadow
// and a ring marker on self. Falls back to procedural drawing if sprites
// haven't loaded.
function drawPlayer(pos, aim, alive, color, isMe, id) {
  const r = cfg.radius;
  const turretAng = Math.atan2(aim.y, aim.x);
  const hullAng = hullFacing(id, pos, turretAng);

  ctx.save();
  ctx.globalAlpha = alive ? 1 : 0.3;

  // Drop shadow (in world space, slightly offset).
  ctx.fillStyle = "rgba(0, 0, 0, 0.35)";
  ctx.beginPath();
  ctx.arc(pos.x + 2, pos.y + 3, r + 2, 0, Math.PI * 2);
  ctx.fill();

  const tint = spritesReady ? tintFor(color) : null;
  if (tint) {
    const scale = (r * 3.0) / tankSprites.hull.width;
    drawSprite(tint.hull, pos, hullAng, scale);   // hull faces movement
    drawSprite(tint.turret, pos, turretAng, scale); // turret faces aim
  } else {
    drawTankProcedural(pos, aim, color);
  }

  // Your own tank gets a white identifier ring.
  if (isMe) {
    ctx.strokeStyle = "rgba(255, 255, 255, 0.7)";
    ctx.lineWidth = 1.5;
    ctx.beginPath();
    ctx.arc(pos.x, pos.y, r + 6, 0, Math.PI * 2);
    ctx.stroke();
  }

  ctx.restore();
  ctx.globalAlpha = 1;
}

// drawSprite blits a (pre-tinted) sprite canvas centered at pos, rotated.
function drawSprite(img, pos, angle, scale) {
  ctx.save();
  ctx.translate(pos.x, pos.y);
  ctx.rotate(angle);
  const w = img.width * scale, h = img.height * scale;
  ctx.drawImage(img, -w / 2, -h / 2, w, h);
  ctx.restore();
}

// drawTankProcedural is the asset-free fallback: a shaded body + barrel along
// the aim. Alpha/transform are managed by the caller.
function drawTankProcedural(pos, aim, color) {
  const r = cfg.radius;
  ctx.save();
  ctx.translate(pos.x, pos.y);
  ctx.rotate(Math.atan2(aim.y, aim.x));

  ctx.fillStyle = shade(color, -0.4);
  ctx.beginPath();
  ctx.roundRect(r - 3, -3.5, r + 7, 7, 3);
  ctx.fill();

  const g = ctx.createRadialGradient(-r * 0.35, -r * 0.35, r * 0.2, 0, 0, r);
  g.addColorStop(0, shade(color, 0.4));
  g.addColorStop(1, color);
  ctx.fillStyle = g;
  ctx.beginPath();
  ctx.arc(0, 0, r, 0, Math.PI * 2);
  ctx.fill();

  ctx.lineWidth = 2;
  ctx.strokeStyle = shade(color, -0.5);
  ctx.stroke();

  ctx.fillStyle = shade(color, -0.25);
  ctx.beginPath();
  ctx.arc(0, 0, r * 0.42, 0, Math.PI * 2);
  ctx.fill();
  ctx.restore();
}

// shade lightens (amt>0, toward white) or darkens (amt<0, toward black) a
// #rrggbb color by the given fraction, returning an rgb() string.
function shade(hex, amt) {
  const n = parseInt(hex.slice(1), 16);
  const r = (n >> 16) & 255, g = (n >> 8) & 255, b = n & 255;
  const target = amt < 0 ? 0 : 255;
  const t = Math.abs(amt);
  const mix = (c) => Math.round(c + (target - c) * t);
  return `rgb(${mix(r)}, ${mix(g)}, ${mix(b)})`;
}

// drawObstacles renders each obstacle as a top-down brick building: a drop
// shadow for height, a mortar base, an offset brick pattern clipped to the
// footprint, and an outline with a top-edge highlight.
const BRICK_W = 34, BRICK_H = 16, MORTAR = 3;
const BRICK_SHADES = ["#9c5b47", "#a4624c", "#8f5340", "#b06b54"];

function drawObstacles() {
  for (const o of obstacles) {
    // Drop shadow.
    ctx.fillStyle = "rgba(0, 0, 0, 0.35)";
    ctx.fillRect(o.x + 3, o.y + 4, o.w, o.h);

    // Mortar base shows through the gaps between bricks.
    ctx.fillStyle = "#2e2622";
    ctx.fillRect(o.x, o.y, o.w, o.h);

    // Brick courses, clipped to the building footprint so edges are clean.
    ctx.save();
    ctx.beginPath();
    ctx.rect(o.x, o.y, o.w, o.h);
    ctx.clip();
    const rowH = BRICK_H + MORTAR;
    let row = 0;
    for (let ry = o.y; ry < o.y + o.h; ry += rowH, row++) {
      const offset = (row % 2) * (BRICK_W / 2); // running bond
      for (let rx = o.x - offset; rx < o.x + o.w; rx += BRICK_W) {
        ctx.fillStyle = brickShade(rx, ry);
        ctx.fillRect(rx + MORTAR, ry + MORTAR, BRICK_W - MORTAR, BRICK_H - MORTAR);
      }
    }
    ctx.restore();

    // Outline + a subtle lit top edge for depth.
    ctx.strokeStyle = "#1d1714";
    ctx.lineWidth = 2;
    ctx.strokeRect(o.x, o.y, o.w, o.h);
    ctx.strokeStyle = "rgba(255, 255, 255, 0.12)";
    ctx.lineWidth = 1;
    ctx.beginPath();
    ctx.moveTo(o.x + 1, o.y + 1.5);
    ctx.lineTo(o.x + o.w - 1, o.y + 1.5);
    ctx.stroke();
  }
}

// brickShade picks a stable per-brick color from its position, so the texture
// doesn't flicker between frames.
function brickShade(x, y) {
  const h = (Math.imul(x | 0, 73856093) ^ Math.imul(y | 0, 19349663)) >>> 0;
  return BRICK_SHADES[h % BRICK_SHADES.length];
}

function drawGrid() {
  const step = 64;
  ctx.strokeStyle = "#262b38";
  ctx.lineWidth = 1;
  ctx.beginPath();
  for (let x = 0; x <= canvas.width; x += step) { ctx.moveTo(x, 0); ctx.lineTo(x, canvas.height); }
  for (let y = 0; y <= canvas.height; y += step) { ctx.moveTo(0, y); ctx.lineTo(canvas.width, y); }
  ctx.stroke();
}

function drawScores(snap) {
  if (!snap) return;
  const sorted = Object.values(snap.players).sort((a, b) => b.score - a.score);
  ctx.font = "14px system-ui, sans-serif";
  ctx.textAlign = "left";
  ctx.fillStyle = "#667";
  ctx.fillText(`first to ${cfg.winScore}`, 12, 20);
  let y = 42;
  for (const p of sorted) {
    ctx.fillStyle = p.id === myID ? "#4cc9f0" : "#cdd";
    ctx.fillText(`${p.id === myID ? "you" : "player " + p.id}: ${p.score}`, 12, y);
    y += 18;
  }
}

// --- helpers ---
function clamp(v, lo, hi) { return v < lo ? lo : v > hi ? hi : v; }
function lerp(a, b, t) { return a + (b - a) * t; }

connect();
setInterval(inputTick, 1000 / INPUT_HZ);
requestAnimationFrame(draw);
