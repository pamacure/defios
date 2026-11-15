// Motion: what makes the app's animations survive its redraws. Every page
// draws by replacing its elements, so an element's animation would start
// again on each redraw, and a looping one would lose its place. This
// watches what is added to the document and, before it is painted:
//
// - starts every looping animation at the timeline's zero, so each has the
//   phase it would have had if it had run all along;
// - starts an animation that plays once (the logo dropping onto a screen,
//   a Sign Request's stances filling) when what it belongs to first came,
//   so a redraw in the middle of it carries on exactly where it was;
// - slides the rail's marker to the page shown;
// - resolves a revealed value from scrambled characters.
//
// style.css has the animations themselves; nothing here is needed for the
// app to work, and none of it moves with prefers-reduced-motion.

const reduce = matchMedia("(prefers-reduced-motion: reduce)");

// style.css's looping animations.
const LOOPS = new Set(["ring", "slide", "scan"]);

const SCENE_MS = 700;
const SNAP_MS = 450;
const SWEEP_MS = 1000;
const MARKER_MS = 320;
const DECODE_MS = 600;
const SNAP = "cubic-bezier(.3, 1.45, .5, 1)";
const LONG_AGO = -1e9;

let scene = { key: null, at: LONG_AGO };
const quorums = new Map(); // a Sign Request -> { n, from, at }
let marker = null; // { from, to, at }
const sliding = new WeakMap(); // a marker element -> the slide it is playing
const decodes = new Map(); // a revealed value's place -> when

// Times are the document timeline's, which animations start on.
const timeline = () => document.timeline.currentTime ?? performance.now();
let now = 0; // the batch being handled

// mark has el play cls's animation as begun `age` ago, once: an element
// still on screen is already playing it.
const starts = new WeakMap(); // a marked element -> when its animation began
function mark(el, cls, age) {
  if (starts.has(el)) return;
  starts.set(el, now - age);
  el.classList.add(cls);
}

// pin gives each animation just made its true start: a loop's is zero, an
// entrance's is when what it belongs to was marked. Left to start when
// first painted, each redraw would set it back a frame.
const pinned = new WeakSet();
function pin() {
  for (const a of document.getAnimations()) {
    if (!(a instanceof CSSAnimation) || pinned.has(a)) continue;
    pinned.add(a);
    if (LOOPS.has(a.animationName)) { a.startTime = 0; continue; }
    let el = a.effect?.target;
    while (el && !starts.has(el)) el = el.parentElement;
    if (el) a.startTime = starts.get(el);
  }
}

// The scene is the screen or page shown: it enters when it changes, not
// when it is redrawn.
function sceneEnter() {
  const main = document.querySelector("#app main");
  if (!main) return false;
  // A page is its section, whatever is open on it; a screen that stands
  // alone is told by its title, or by its form when it has no title.
  const page = document.body.dataset.page;
  const busy = main.hasAttribute("aria-busy");
  const title = main.querySelector("h1")?.textContent;
  const key = page ? `page|${page}|${busy}`
    : [main.className.replace(" enter", ""), title, title ? "" : !!main.querySelector("form")].join("|");
  const changed = key !== scene.key;
  if (changed) scene = { key, at: now };
  const age = now - scene.at;
  if (age < SCENE_MS) mark(main, "enter", age);
  return changed;
}

// A Sign Request's meter: the segments just filled snap in, and the card
// is swept once as the last one needed does.
function quorum() {
  const seen = new Set();
  for (const el of document.querySelectorAll(".quorum[data-key]")) {
    const key = el.dataset.key;
    const n = Number(el.dataset.n);
    const needed = Number(el.dataset.needed);
    seen.add(key);
    let q = quorums.get(key);
    if (!q) q = { n, from: n, at: LONG_AGO };
    else if (n > q.n) q = { n, from: q.n, at: now };
    else if (n < q.n) q = { n, from: n, at: LONG_AGO };
    quorums.set(key, q);
    const age = now - q.at;
    if (age < SNAP_MS) {
      const segs = el.querySelectorAll(".seg");
      for (let i = q.from; i < q.n && i < segs.length; i++) mark(segs[i], "snap", age - (i - q.from) * 90);
    }
    if (age < SWEEP_MS && q.from < needed && q.n >= needed) {
      const card = el.closest(".signcard");
      if (card) mark(card, "sweep", age);
    }
  }
  for (const key of quorums.keys()) if (!seen.has(key)) quorums.delete(key);
}

// The rail's marker sits beside the page shown and slides to the next.
function railMarker(settle = false) {
  const m = document.querySelector(".rail .marker");
  if (!m) { marker = null; return; }
  const cur = m.parentElement.querySelector('[aria-current="page"]');
  if (!cur) { m.style.opacity = "0"; return; }
  const y = cur.offsetTop + (cur.offsetHeight - m.offsetHeight) / 2;
  if (!marker || settle) marker = { from: y, to: y, at: LONG_AGO };
  else if (marker.to !== y) marker = { from: marker.to, to: y, at: now };
  m.style.transform = `translateY(${y}px)`;
  m.style.opacity = "1";
  const age = now - marker.at;
  if (age < MARKER_MS && !reduce.matches && sliding.get(m) !== marker.at) {
    sliding.set(m, marker.at);
    const slide =
    m.animate([{ transform: `translateY(${marker.from}px)` }, { transform: `translateY(${y}px)` }],
      { duration: MARKER_MS, easing: SNAP });
    slide.startTime = marker.at;
  }
}

const GLYPHS = "ABCDEF0123456789#$%&*+=<>/";
const scramble = (n) => Array.from({ length: n }, () => GLYPHS[Math.floor(Math.random() * GLYPHS.length)]).join("");

// A value marked data-decode resolves from scrambled characters, left to
// right, once.
function decode() {
  const seen = new Set();
  document.querySelectorAll("[data-decode]").forEach((el, i) => {
    const key = `decode#${i}`;
    seen.add(key);
    const t = performance.now();
    if (!decodes.has(key)) decodes.set(key, t);
    const at = decodes.get(key);
    const final = el.textContent;
    if (reduce.matches || t - at >= DECODE_MS) return;
    const step = () => {
      if (!el.isConnected) return;
      const n = Math.floor(final.length * (performance.now() - at) / DECODE_MS);
      if (n >= final.length) { el.textContent = final; return; }
      el.textContent = final.slice(0, n) + scramble(final.length - n);
      requestAnimationFrame(step);
    };
    step();
  });
  for (const key of decodes.keys()) if (!seen.has(key)) decodes.delete(key);
}

new MutationObserver((records) => {
  const added = [];
  for (const r of records) {
    // decode's own writes.
    if (r.target.nodeType === 1 && r.target.hasAttribute("data-decode")) continue;
    for (const n of r.addedNodes) if (n.nodeType === 1 && n.isConnected) added.push(n);
  }
  if (!added.length) return;
  now = timeline();
  sceneEnter();
  quorum();
  railMarker();
  decode();
  pin();
}).observe(document.body, { childList: true, subtree: true });

// The rail's layout settles once its mark has loaded.
document.addEventListener("load", (e) => {
  if (e.target.matches?.(".rail .mark")) { now = timeline(); railMarker(true); }
}, true);
addEventListener("resize", () => { now = timeline(); railMarker(true); });
