// The DeFiOS desktop frontend. It renders the shell.State the Go side
// reports (main.UI) and calls back into it; it holds no Node state of its
// own beyond which page is open. UI text says "device" for a Node.
import { Call, Events } from "/wails/runtime.js";

const call = (method, ...args) => Call.ByName(`main.UI.${method}`, ...args);

const SECTIONS = [
  { id: "talk", label: "Talk", issue: 149 },
  { id: "vault", label: "Vault", issue: 151 },
  { id: "agents", label: "Agents", issue: 152 },
  { id: "plugins", label: "Plugins", issue: 153 },
  { id: "triggers", label: "Triggers", issue: 154 },
  { id: "networks", label: "Networks", issue: 155 },
];
const SETTINGS = { id: "settings", label: "Settings", issue: 150 };

const ICONS = {
  talk: '<path d="M4 5h16v11H9l-5 4z"/>',
  vault: '<rect x="4" y="4" width="16" height="16" rx="2"/><circle cx="12" cy="12" r="3.5"/><path d="M12 8.5V6M15.5 12H18"/>',
  agents: '<rect x="5" y="8" width="14" height="11" rx="2"/><path d="M12 4v4M9 13h.01M15 13h.01M9 16h6"/>',
  plugins: '<path d="M9 4v4M15 4v4M7 8h10v4a5 5 0 0 1-10 0zM12 17v3"/>',
  triggers: '<path d="M13 3 5 14h6l-1 7 8-11h-6z"/>',
  networks: '<circle cx="12" cy="12" r="8"/><path d="M4 12h16M12 4c2.5 2.5 2.5 13.5 0 16M12 4c-2.5 2.5-2.5 13.5 0 16"/>',
  settings: '<circle cx="12" cy="12" r="3"/><path d="M12 3v3M12 18v3M3 12h3M18 12h3M5.6 5.6l2.1 2.1M16.3 16.3l2.1 2.1M5.6 18.4l2.1-2.1M16.3 7.7l2.1-2.1"/>',
};

// What only the frontend keeps: the open page and transient UI.
const ui = { page: "talk", menuOpen: false, addingGroup: false, busy: false };
let state = null;
let shownKey = "";

// h builds an element; children are nodes or strings (never parsed as
// HTML, so Group aliases and device names cannot inject markup).
function h(tag, attrs = {}, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (v === false || v == null) continue;
    if (k.startsWith("on")) el.addEventListener(k.slice(2), v);
    else if (k === "class") el.className = v;
    else el.setAttribute(k, v === true ? "" : v);
  }
  for (const c of children.flat()) {
    if (c == null || c === false) continue;
    el.append(c instanceof Node ? c : String(c));
  }
  return el;
}

function icon(name) {
  const span = h("span", { "aria-hidden": "true" });
  span.innerHTML = `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round">${ICONS[name]}</svg>`;
  return span;
}

const logo = (cls = "logo") => h("img", { class: cls, src: "/logo.png", alt: "DeFiOS" });
const nodeName = (s) => s.Node.Name || s.Node.ID.slice(0, 12);

function apply(next) {
  if (!next || (state && next.Seq < state.Seq)) return; // a stale State
  state = next;
  if (state.View !== "served") ui.menuOpen = false;
  render();
}

async function act(method, ...args) {
  ui.busy = true;
  render(true);
  try {
    apply(await call(method, ...args));
  } finally {
    ui.busy = false;
    render(true);
  }
}

// render redraws the app, unless only fields the current screen does not
// show changed: a redraw would drop a half-typed passphrase.
function render(force = false) {
  if (!state) return;
  const s = state;
  const key = JSON.stringify([s.View, s.Selected, s.Groups, s.Problem, s.Node, ui.addingGroup,
    s.View === "served" ? [s.Seq, ui] : ui.busy]);
  if (!force && key === shownKey) return;
  shownKey = key;
  const root = document.getElementById("app");
  root.replaceChildren(screen(s));
  const focus = root.querySelector("[data-autofocus]");
  if (focus) focus.focus();
}

function screen(s) {
  if (ui.addingGroup && s.View === "served") return onboarding(s, 2, true);
  switch (s.View) {
    case "no-node": return onboarding(s, 1, false);
    case "no-group": return onboarding(s, 2, false);
    case "locked": return lockScreen(s);
    case "unlocking": return loading(s.Selected ? `Unlocking ${s.Selected}…` : "Unlocking…");
    case "served-elsewhere": return servedElsewhere(s);
    case "served": return shell(s);
    default: return loading("Starting…");
  }
}

function loading(text) {
  return h("main", { class: "center" }, logo(), h("div", { class: "progress", role: "progressbar", "aria-label": text }), h("p", {}, text));
}

function problemText(s) {
  return s.Problem ? h("p", { class: "error", role: "alert" }, s.Problem.Message) : null;
}

function lockScreen(s) {
  const select = h("select", { class: "field", "aria-label": "Group", disabled: ui.busy },
    s.Groups.map((g) => h("option", { value: g.Alias, selected: g.Alias === s.Selected }, g.Alias)));
  const pass = h("input", {
    class: "field", type: "password", placeholder: "Vault Locked", "aria-label": "Passphrase",
    autocomplete: "current-password", "data-autofocus": true, disabled: ui.busy,
    "aria-invalid": s.Problem?.Code === "bad-passphrase" ? "true" : null,
  });
  const form = h("form", {
    class: "form",
    onsubmit: (e) => {
      e.preventDefault();
      if (pass.value) act("Unlock", select.value, pass.value);
    },
  },
  select,
  pass,
  h("button", { class: "btn primary", type: "submit", disabled: ui.busy }, "Unlock"),
  problemText(s));
  return h("main", { class: "center" },
    logo(),
    h("div", { class: "device" }, h("strong", {}, nodeName(s))),
    form);
}

function servedElsewhere(s) {
  return h("main", { class: "center" },
    logo(),
    h("h1", {}, "DeFiOS is running elsewhere"),
    h("p", {}, "Another program on this computer is already running DeFiOS for this device, such as ",
      h("code", {}, "defios serve"), ". Stop it and this window will let you unlock."),
    h("div", { class: "progress", role: "progressbar", "aria-label": "Waiting for the other program to stop" }),
    h("div", { class: "device" }, h("strong", {}, nodeName(s))));
}

// onboarding is the placeholder for onboarding steps 1 and 2 until those
// pages land (#148): it points at the CLI.
function onboarding(s, step, adding) {
  const steps = {
    1: {
      title: "Set up this device",
      body: ["DeFiOS isn't set up on this computer yet. Until setup comes to the app, run ", h("code", {}, "defios init --name <device name>"), " in a terminal, then check again."],
    },
    2: {
      title: adding ? "Add a Group" : "Join a Group",
      body: ["Until this comes to the app, create a Group for this device in a terminal with ", h("code", {}, "defios group new <alias> --headscale <url>"), adding ? ", then pick it from the Group menu." : ", then check again."],
    },
  }[step];
  return h("main", { class: "center" },
    logo(),
    h("h1", {}, steps.title),
    h("p", {}, ...steps.body),
    problemText(s),
    h("div", { class: "form" },
      adding
        ? h("button", { class: "btn", onclick: () => { ui.addingGroup = false; render(true); } }, "Back")
        : h("button", { class: "btn primary", disabled: ui.busy, onclick: () => act("Refresh") }, "Check again")));
}

function shell(s) {
  return h("div", { class: "shell" }, rail(), header(s), page(s));
}

function rail() {
  const item = (sec) => h("button", {
    "aria-label": sec.label, "aria-current": ui.page === sec.id ? "page" : null,
    onclick: () => { ui.page = sec.id; render(true); },
  }, icon(sec.id), h("span", { class: "tip" }, sec.label));
  return h("nav", { class: "rail", "aria-label": "Sections" },
    logo("mark"), SECTIONS.map(item), h("div", { class: "spacer" }), item(SETTINGS));
}

function onlineDot(s) {
  if (!s.RosterSize) return s.TailnetUp ? "on" : "";
  if (s.Online === s.RosterSize) return "on";
  return s.MajorityReachable ? "partial" : "";
}

function header(s) {
  const online = s.RosterSize ? `${s.Online} of ${s.RosterSize} devices online` : (s.TailnetUp ? "Online" : "Connecting…");
  const others = s.Groups.filter((g) => g.Alias !== s.Served);
  const menu = ui.menuOpen && h("div", { class: "dropdown", role: "menu" },
    s.Groups.map((g) => h("button", {
      class: "item", role: "menuitem",
      onclick: () => { ui.menuOpen = false; if (g.Alias !== s.Served) act("Switch", g.Alias); else render(true); },
    }, h("span", { class: `dot ${g.Alias === s.Served ? "on" : ""}` }), g.Alias,
    h("span", { class: "state" }, g.Alias === s.Served ? "Online" : "Offline"))),
    others.length > 0 && h("div", { class: "note" },
      `This device serves one Group at a time: while ${s.Served} is online, ${others.length === 1 ? others[0].Alias + " is" : "your other Groups are"} offline here. Choosing one switches to it.`),
    h("button", { class: "item add", role: "menuitem", onclick: () => { ui.menuOpen = false; ui.addingGroup = true; render(true); } }, "+ Add Group"));
  return h("header", { class: "header" },
    h("div", { class: "groupmenu" },
      h("button", { "aria-haspopup": "menu", "aria-expanded": ui.menuOpen ? "true" : "false", onclick: () => { ui.menuOpen = !ui.menuOpen; render(true); } },
        h("span", { class: "label" }, "Group"), h("span", { class: "name" }, s.Served),
        h("span", { class: `dot ${onlineDot(s)}` }), h("span", { class: "online" }, online)),
      menu),
    h("div", { class: "who" }, "Device ", h("strong", {}, nodeName(s))));
}

function page(s) {
  const sec = [...SECTIONS, SETTINGS].find((x) => x.id === ui.page) ?? SECTIONS[0];
  return h("main", { class: "page" },
    h("h1", {}, sec.label),
    s.Problem && h("div", { class: "banner", role: "alert" }, s.Problem.Message),
    h("section", { class: "part", "aria-label": "This Device" },
      h("header", {}, h("h2", {}, "This Device")),
      h("div", { class: "body" }, sec.id === "settings" ? nodeSettings(s) : h("p", { class: "placeholder" }, `${sec.label} for this device comes later.`))),
    h("section", { class: "part", "aria-label": "Group" },
      h("header", {}, h("h2", {}, "Group"), editTarget(s)),
      !s.EditGroupServed && h("div", { class: "notice" }, `${s.EditGroup} is not online on this device. Changes here apply the next time it is.`),
      h("div", { class: "body" }, h("p", { class: "placeholder" }, `${sec.label} for ${s.EditGroup} comes later.`))));
}

// editTarget picks the Group the Group parts edit. The choice is kept
// across pages.
function editTarget(s) {
  return h("select", { "aria-label": "Group to edit", onchange: (e) => act("SetEditGroup", e.target.value) },
    s.Groups.map((g) => h("option", { value: g.Alias, selected: g.Alias === s.EditGroup },
      g.Alias === s.Served ? `${g.Alias} (online)` : g.Alias)));
}

function nodeSettings(s) {
  const toggle = (label, checked, method, ...extra) => h("label", { class: "toggle" },
    h("input", { type: "checkbox", checked, disabled: ui.busy, onchange: (e) => act(method, e.target.checked) }),
    h("span", { class: "text" }, h("strong", {}, label), ...extra));
  return h("div", {},
    toggle("Start at login", s.Prefs.StartAtLogin, "SetStartAtLogin",
      h("span", {}, "Start DeFiOS hidden in the tray when you log in. Without the keychain, it tells you it is locked.")),
    toggle("Keep the passphrase in the keychain", s.Prefs.Keychain, "SetKeychain",
      h("span", {}, "Unlock the last Group automatically when DeFiOS starts at login."),
      h("span", { class: "warn" }, "Anyone who can use this computer account can unlock DeFiOS on this device.")),
    toggle("Notifications", s.Prefs.Notifications, "SetNotifications",
      h("span", {}, "Let DeFiOS post notifications.")));
}

document.addEventListener("click", (e) => {
  if (ui.menuOpen && !e.target.closest(".groupmenu")) { ui.menuOpen = false; render(true); }
});
document.addEventListener("keydown", (e) => {
  if (e.key === "Escape" && ui.menuOpen) { ui.menuOpen = false; render(true); }
});

Events.On("shell:state", (ev) => apply(ev.data));
call("State").then(apply);
