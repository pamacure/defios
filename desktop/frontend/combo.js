// A text field that suggests values from a list, drawn in the look of the
// app's <select> lists (select.js): focusing or typing opens the options
// that contain what is typed, and a pick fills the field and fires input
// and change. Anything may still be typed; the list only suggests.
import { h } from "./dom.js";

let open = null; // { input, menu, items, active, frame }

// combo makes input suggest from its options, which setOptions replaces:
// each { value, label }, label shown beside the value when it differs.
export function combo(input) {
  input.setAttribute("role", "combobox");
  input.setAttribute("aria-autocomplete", "list");
  input.comboOptions = [];
  input.addEventListener("focus", () => show(input));
  input.addEventListener("click", () => { if (open?.input !== input) show(input); });
  input.addEventListener("input", (e) => { if (e.isTrusted) show(input); });
  input.addEventListener("blur", () => { if (open?.input === input) close(); });
  input.addEventListener("keydown", (e) => key(input, e));
  return input;
}

// setOptions replaces input's options, redrawing its list if it is open.
export function setOptions(input, options) {
  input.comboOptions = options;
  if (open?.input === input) show(input);
}

function show(input) {
  close();
  const q = input.value.trim().toLowerCase();
  const matches = input.comboOptions.filter((o) => !q || o.value.toLowerCase().includes(q) || o.label?.toLowerCase().includes(q));
  // Nothing to suggest, or only what is typed already: no list.
  if (!matches.length || (matches.length === 1 && matches[0].value === input.value.trim())) return;
  const items = matches.map((o) => {
    const li = h("li", { role: "option", class: "opt", "aria-selected": o.value === input.value.trim() ? "true" : "false" },
      h("span", { class: "mono" }, o.value), o.label && o.label !== o.value && h("span", { class: "muted" }, ` ${o.label}`));
    const item = { o, li };
    li.addEventListener("mousemove", () => activate(items.indexOf(item)));
    li.addEventListener("click", () => choose(item));
    return item;
  });
  const menu = h("ul", { class: "selectmenu combomenu", role: "listbox" }, items.map((it) => it.li));
  // A press in the list keeps the focus in the field.
  menu.addEventListener("mousedown", (e) => e.preventDefault());
  document.body.append(menu);
  open = { input, menu, items, active: -1, frame: 0 };
  input.setAttribute("aria-expanded", "true");
  place();
  follow();
}

// follow keeps the list under its field, and shuts it when a redraw takes
// the field away.
function follow() {
  if (!open) return;
  if (!open.input.isConnected || open.input.disabled) { close(); return; }
  place();
  open.frame = requestAnimationFrame(follow);
}

function place() {
  const { input, menu } = open;
  const r = input.getBoundingClientRect();
  const gap = 6, room = 12;
  menu.style.minWidth = `${r.width}px`;
  menu.style.left = `${Math.max(room, Math.min(r.left, innerWidth - menu.offsetWidth - room))}px`;
  const below = innerHeight - r.bottom - gap - room, above = r.top - gap - room;
  const up = menu.scrollHeight > below && above > below;
  menu.style.maxHeight = `${Math.max(120, Math.min(320, up ? above : below))}px`;
  menu.style.top = up ? `${r.top - gap - menu.offsetHeight}px` : `${r.bottom + gap}px`;
  menu.classList.toggle("up", up);
}

function close() {
  if (!open) return;
  cancelAnimationFrame(open.frame);
  open.menu.remove();
  open.input.removeAttribute("aria-expanded");
  open = null;
}

function activate(i) {
  const { items } = open;
  if (i < 0 || i >= items.length) return;
  if (open.active >= 0) items[open.active].li.classList.remove("active");
  open.active = i;
  items[i].li.classList.add("active");
  items[i].li.scrollIntoView({ block: "nearest" });
}

function choose(item) {
  const input = open.input;
  close();
  input.value = item.o.value;
  input.dispatchEvent(new Event("input", { bubbles: true }));
  input.dispatchEvent(new Event("change", { bubbles: true }));
}

function key(input, e) {
  if (open?.input !== input) {
    if (e.key === "ArrowDown") { e.preventDefault(); show(input); if (open) activate(0); }
    return;
  }
  const { items, active } = open;
  switch (e.key) {
    case "ArrowDown": e.preventDefault(); activate(Math.min(items.length - 1, active + 1)); return;
    case "ArrowUp": e.preventDefault(); activate(Math.max(0, active - 1)); return;
    case "Enter":
      // Enter picks the one highlighted; with none, it submits as typed.
      if (active >= 0) { e.preventDefault(); choose(items[active]); }
      return;
    case "Escape": e.preventDefault(); e.stopPropagation(); close(); return;
    case "Tab": close(); return;
  }
}

window.addEventListener("resize", () => close());
document.addEventListener("scroll", (e) => { if (open && !open.menu.contains(e.target)) close(); }, true);
