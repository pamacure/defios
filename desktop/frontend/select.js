// A <select>'s list, drawn by the app: the webview's own is the OS's
// widget, which no style reaches, so it showed in the system's colours,
// borders and corners on every page. This opens a list in the app's look
// in its place, for every single-choice <select> wherever it is, and
// leaves the <select> holding the value: a pick sets it and fires input
// and change as the native list would, so the pages that build them need
// nothing of this.
import { h } from "./dom.js";

let open = null; // { select, menu, items, active, frame }

const single = (el) => el instanceof HTMLSelectElement && !el.multiple && el.size <= 1;

document.addEventListener("mousedown", (e) => {
  if (open && !open.menu.contains(e.target)) {
    const same = e.target === open.select;
    close();
    if (same) { e.preventDefault(); return; } // a second press shuts it, as the native one does
  }
  const sel = e.target.closest?.("select");
  if (e.button !== 0 || !single(sel) || sel.disabled) return;
  e.preventDefault();
  sel.focus();
  show(sel);
}, true);

document.addEventListener("keydown", (e) => {
  if (open) { key(e); return; }
  const sel = e.target;
  if (!single(sel) || sel.disabled) return;
  const opens = e.key === "Enter" || e.key === " " || e.key === "F4" ||
    e.key === "ArrowDown" || e.key === "ArrowUp";
  if (!opens) return;
  e.preventDefault();
  show(sel);
}, true);

window.addEventListener("resize", () => close());
document.addEventListener("scroll", (e) => { if (open && !open.menu.contains(e.target)) close(); }, true);

function show(sel) {
  const items = [];
  const row = (opt) => {
    const li = h("li", {
      role: "option", class: "opt",
      "aria-selected": opt.selected ? "true" : "false",
      "aria-disabled": opt.disabled || opt.parentElement?.disabled ? "true" : false,
    }, opt.label || opt.text);
    li.addEventListener("mousemove", () => { if (!disabled(li)) activate(items.indexOf(item)); });
    li.addEventListener("click", () => { if (!disabled(li)) choose(item); });
    const item = { opt, li };
    items.push(item);
    return li;
  };
  const rows = [...sel.children].map((c) => c instanceof HTMLOptGroupElement
    ? [h("li", { role: "presentation", class: "group" }, c.label), [...c.children].map(row)]
    : c instanceof HTMLOptionElement ? row(c) : null);
  const menu = h("ul", { class: "selectmenu", role: "listbox", "aria-label": sel.getAttribute("aria-label") || false }, rows);
  // A press in the list keeps the focus on the <select>, for the keys.
  menu.addEventListener("mousedown", (e) => e.preventDefault());
  document.body.append(menu);
  open = { select: sel, menu, items, active: -1, frame: 0 };
  sel.setAttribute("aria-expanded", "true");
  place();
  activate(Math.max(0, items.findIndex((it) => it.opt.selected)), true);
  follow();
}

// follow keeps the list under its <select> while it is open, and shuts it
// when a redraw takes the <select> away.
function follow() {
  if (!open) return;
  if (!open.select.isConnected || open.select.disabled) { close(); return; }
  place();
  open.frame = requestAnimationFrame(follow);
}

function place() {
  const { select, menu } = open;
  const r = select.getBoundingClientRect();
  const gap = 6, room = 12;
  menu.style.minWidth = `${r.width}px`;
  menu.style.left = `${Math.max(room, Math.min(r.left, innerWidth - menu.offsetWidth - room))}px`;
  const below = innerHeight - r.bottom - gap - room, above = r.top - gap - room;
  const up = menu.scrollHeight > below && above > below;
  menu.style.maxHeight = `${Math.max(120, up ? above : below)}px`;
  menu.style.top = up ? `${r.top - gap - menu.offsetHeight}px` : `${r.bottom + gap}px`;
  menu.classList.toggle("up", up);
}

function close() {
  if (!open) return;
  cancelAnimationFrame(open.frame);
  open.menu.remove();
  open.select.removeAttribute("aria-expanded");
  open = null;
}

const disabled = (li) => li.getAttribute("aria-disabled") === "true";

function activate(i, centre = false) {
  const { items } = open;
  if (i < 0 || i >= items.length) return;
  if (open.active >= 0) items[open.active].li.classList.remove("active");
  open.active = i;
  const li = items[i].li;
  li.classList.add("active");
  li.scrollIntoView({ block: centre ? "center" : "nearest" });
}

// step moves the highlight by dir past the disabled options, to the end at most.
function step(from, dir) {
  const { items } = open;
  for (let i = from + dir; i >= 0 && i < items.length; i += dir) if (!disabled(items[i].li)) return i;
  return from;
}

// near is i if it can be picked, else the nearest that can, dir first.
function near(i, dir) {
  if (!disabled(open.items[i].li)) return i;
  const on = step(i, dir);
  return on !== i ? on : step(i, -dir);
}

function choose(item) {
  const sel = open.select;
  close();
  if (item.opt.selected) return;
  item.opt.selected = true;
  sel.dispatchEvent(new Event("input", { bubbles: true }));
  sel.dispatchEvent(new Event("change", { bubbles: true }));
}

let typed = "", typedAt = 0;

function key(e) {
  const { items, active } = open;
  const swallow = () => { e.preventDefault(); e.stopPropagation(); };
  switch (e.key) {
    case "ArrowDown": swallow(); activate(step(active, 1)); return;
    case "ArrowUp": swallow(); activate(step(active, -1)); return;
    case "Home": swallow(); activate(step(-1, 1)); return;
    case "End": swallow(); activate(step(items.length, -1)); return;
    case "PageDown": swallow(); activate(near(Math.min(items.length - 1, active + 7), 1)); return;
    case "PageUp": swallow(); activate(near(Math.max(0, active - 7), -1)); return;
    case "Enter": case " ":
      swallow();
      if (active >= 0 && !disabled(items[active].li)) choose(items[active]); else close();
      return;
    case "Escape": swallow(); close(); return;
    case "Tab": close(); return;
  }
  // Typing finds the next option starting with what was typed.
  if (e.key.length !== 1 || e.ctrlKey || e.metaKey || e.altKey) return;
  swallow();
  const now = Date.now();
  typed = now - typedAt > 700 ? e.key.toLowerCase() : typed + e.key.toLowerCase();
  typedAt = now;
  const n = items.length;
  const start = typed.length === 1 ? active + 1 : active;
  for (let k = 0; k < n; k++) {
    const i = (start + k + n) % n;
    if (!disabled(items[i].li) && items[i].li.textContent.trim().toLowerCase().startsWith(typed)) { activate(i); return; }
  }
}
