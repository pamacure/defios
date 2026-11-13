// The DeFiOS desktop frontend. It renders the shell.State the Go side
// reports (main.UI) and calls back into it; it holds no Node state of its
// own beyond which page is open. UI text says "device" for a Node.
import { Events } from "/wails/runtime.js";
import { agentsDevicePart, agentsGroupPart, agentsLoad, agentsOpen, agentsReady, agentsRefresh } from "./agents.js";
import { call, h, icon } from "./dom.js";
import { settingsLoad, settingsPart, settingsReady, settingsRefresh } from "./settings.js";
import { talkPage, talkReady, talkUpdate } from "./talk.js";
import { backupNow, vaultDevicePart, vaultGroupPart, vaultLoad, vaultReady, vaultRefresh } from "./vault.js";

const SECTIONS = [
  { id: "talk", label: "Talk", issue: 149 },
  { id: "vault", label: "Vault", issue: 152 },
  { id: "agents", label: "Agents", issue: 151 },
  { id: "plugins", label: "Plugins", issue: 153 },
  { id: "triggers", label: "Triggers", issue: 154 },
  { id: "networks", label: "Networks", issue: 155 },
];
const SETTINGS = { id: "settings", label: "Settings", issue: 155 };

// What each page with content needs read before it shows: ready, whether
// it is for the State s; load, to read it; refresh, to read it again when
// the page is opened while already loaded. A page shows nothing of itself
// until ready, so it never fills in part by part.
const loadTalk = () => call("Talk").then(talkUpdate);
const LOADS = {
  talk: { ready: talkReady, load: loadTalk, refresh: () => {} },
  agents: { ready: agentsReady, load: agentsLoad, refresh: agentsRefresh },
  vault: { ready: vaultReady, load: vaultLoad, refresh: vaultRefresh },
  settings: { ready: settingsReady, load: settingsLoad, refresh: settingsRefresh },
};
// The loads the gate waits on, by page and Group: "loading" until done,
// then "failed" if the page is still not ready, so a load that keeps
// failing asks for a retry rather than trying again on every redraw.
const attempts = new Map();

// What only the frontend keeps: the open page and transient UI. For
// onboarding, too: the Backup chosen at step 1, step 3's tab, the device
// picked and whether a Decline is being written, and every typed field, so
// a redraw never drops what was typed.
const ui = {
  page: "talk", focus: null, menuOpen: false, addingGroup: false, busy: false,
  restore: null, invite: "propose", pick: "", declining: false, form: {},
  backingUp: false, backupProblem: null, backupDismissed: -1,
};
let state = null;
let shownKey = "";

const logo = () => h("img", { class: "logo", src: "/logo.png", alt: "DeFiOS" });
const mark = () => h("img", { class: "mark", src: "/mark.png", alt: "DeFiOS" });
const nodeName = (s) => s.Node.Name || s.Node.ID.slice(0, 12);

function apply(next) {
  if (!next || (state && next.Seq < state.Seq)) return; // a stale State
  const was = state?.View;
  state = next;
  if (state.View !== "served") ui.menuOpen = false;
  if (state.View !== "served") ui.addingGroup = false;
  // Past a step, forget what was typed for it; once formed, land on Talk.
  if (["locked", "served", "forming"].includes(state.View)) ui.restore = null;
  if (!["no-node", "creating", "restoring", "no-group"].includes(state.View) && !ui.addingGroup) ui.form = {};
  if (state.View !== "forming") { ui.pick = ""; ui.declining = false; }
  if (was === "forming" && state.View === "served") ui.page = "talk";
  preload(state);
  discovery();
  render();
}

// preload reads every page's content in the background once a Group is
// served, and again for a new edit-target Group, so that opening a page
// seldom waits.
let preloaded = "";
function preload(s) {
  const key = s.View === "served" ? `${s.Served}\u0000${s.EditGroup}` : "";
  if (key === preloaded) return;
  preloaded = key;
  attempts.clear();
  if (!key) return;
  for (const [id, p] of Object.entries(LOADS)) {
    if (!p.ready(s)) wait(id, s);
  }
}

// wait runs page id's load for s unless one is under way or failed, and
// redraws once it is done.
function wait(id, s) {
  const key = `${id}\u0000${s.EditGroup}`;
  if (attempts.has(key)) return;
  attempts.set(key, "loading");
  const p = LOADS[id];
  p.load(s).catch(() => {}).finally(() => {
    attempts.set(key, p.ready(state) ? "done" : "failed");
    if (ui.page === id) render(true);
  });
}

// discovery looks for devices every few seconds while step 3 lists them.
let discoverTimer = null;
function discovery() {
  const f = state.View === "forming" ? state.Forming : null;
  const want = !!f && !f.Proposal && ui.invite === "propose";
  if (want && !discoverTimer) {
    const tick = () => call("Discover").then(apply, () => {});
    discoverTimer = setInterval(tick, 3000);
    tick();
  } else if (!want && discoverTimer) {
    clearInterval(discoverTimer);
    discoverTimer = null;
  }
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
    s.Progress, s.Forming, s.Restored, s.PassphraseHeld, ui.restore, ui.invite, ui.pick, ui.declining,
    s.View === "served" && !ui.addingGroup ? [s.Seq, ui.page, ui.menuOpen, ui.busy] : ui.busy]);
  if (!force && key === shownKey) return;
  shownKey = key;
  const root = document.getElementById("app");
  // A field kept across redraws, as Talk's composer is, keeps focus.
  const active = document.activeElement;
  root.replaceChildren(screen(s));
  if (active && active !== document.body && active.isConnected) {
    active.focus();
    return;
  }
  const focus = root.querySelector("[data-autofocus]");
  if (focus) focus.focus();
}

function screen(s) {
  if (ui.addingGroup && s.View === "served") return stepTwo(s, true);
  switch (s.View) {
    case "no-node": return ui.restore ? restorePage(s) : stepOne(s);
    case "creating": return creating(s);
    case "restoring": return loading("Restoring this device from the Backup…");
    case "no-group": return stepTwo(s, false);
    case "forming": return stepThree(s);
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
    form,
    s.Restored && restoredNote(s));
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

// Onboarding, one page per step (#148): (1) create this device or restore
// it from a Backup, (2) join a tailnet with a new Group, (3) form that
// Group with other devices. "+ Add Group" runs steps 2 and 3 again.

// field is an input whose value lives in ui.form[name], so a redraw keeps
// it.
function field(name, attrs = {}) {
  const el = h("input", { class: "field", ...attrs, value: ui.form[name] ?? "" });
  el.addEventListener("input", () => { ui.form[name] = el.value; });
  return el;
}

function card(...children) {
  return h("main", { class: "onboard" }, h("div", { class: "card" }, ...children));
}

const steps = (n, adding) => h("p", { class: "eyebrow" },
  adding ? `Add a Group · step ${n - 1} of 2` : `Set up · step ${n} of 3`);

const lead = (text, forId) => h("label", { class: "lead", for: forId }, text);

const shortID = (id) => (id.length > 16 ? `${id.slice(0, 16)}…` : id);

const formed = (g) => g.Status === "valid" || g.Status === "error";

// The problem shown inline on an onboarding page; not-reached has its own.
function problemFor(s) {
  return s.Problem && s.Problem.Code !== "not-reached" ? problemText(s) : null;
}

function stepOne(s) {
  const nameErr = h("p", { class: "error", "aria-live": "polite" });
  const name = field("name", {
    id: "name", placeholder: "Device name (e.g. ‘work-laptop’)", autocomplete: "off",
    "data-autofocus": true, disabled: ui.busy,
  });
  let seq = 0;
  name.addEventListener("input", async () => {
    const mine = ++seq;
    const p = name.value ? await call("CheckDisplayName", name.value) : null;
    if (mine !== seq) return; // a later keystroke answers instead
    nameErr.textContent = p ? p.Message : "";
    name.setAttribute("aria-invalid", p ? "true" : "false");
  });
  const pass = field("pass", { type: "password", placeholder: "Passphrase (irrecoverable if forgotten)", "aria-label": "Passphrase", autocomplete: "new-password", disabled: ui.busy });
  const again = field("again", { type: "password", placeholder: "Repeat passphrase", "aria-label": "Repeat passphrase", autocomplete: "new-password", disabled: ui.busy });
  const matchErr = h("p", { class: "error", "aria-live": "polite" });
  const checkMatch = (final) => {
    const bad = (final || again.value.length >= pass.value.length) && again.value !== pass.value;
    matchErr.textContent = bad ? "The passphrases do not match." : "";
    again.setAttribute("aria-invalid", bad ? "true" : "false");
    return !bad;
  };
  pass.addEventListener("input", () => again.value && checkMatch(false));
  again.addEventListener("input", () => checkMatch(false));

  const form = h("form", {
    class: "stack",
    onsubmit: (e) => {
      e.preventDefault();
      if (!checkMatch(true) || !name.value || !pass.value) return;
      act("Create", name.value, pass.value);
    },
  },
  lead("To start fresh, pick a unique device name…", "name"),
  name, nameErr,
  h("p", { class: "lead" }, "…and a passphrase to encrypt your Vault"),
  pass, again, matchErr,
  h("button", { class: "btn bright", type: "submit", disabled: ui.busy }, "Build"));

  return card(
    h("h1", {}, "Welcome to DeFiOS"),
    h("p", { class: "sub" }, "DeFiOS isn't set up on this computer yet."),
    form,
    h("div", { class: "or", role: "separator" }, h("span", {}, "or")),
    h("div", { class: "stack" },
      h("p", { class: "lead" }, "Build from a previously backed up device"),
      h("button", { class: "btn soft", type: "button", disabled: ui.busy, onclick: chooseBackup }, "Choose File")),
    problemFor(s));
}

async function chooseBackup() {
  const file = await call("ChooseBackup");
  if (!file) return;
  const st = await call("CheckBackup", file);
  if (st && !st.Problem) ui.restore = { file };
  apply(st);
  render(true);
}

function restorePage(s) {
  const pass = field("restorePass", {
    id: "restore-pass", type: "password", placeholder: "Passphrase", autocomplete: "current-password",
    "data-autofocus": true, disabled: ui.busy,
    "aria-invalid": s.Problem?.Code === "bad-passphrase" ? "true" : null,
  });
  return card(
    h("h1", {}, "Build from Backup"),
    h("p", { class: "sub file", title: ui.restore.file }, ui.restore.file.split(/[\\/]/).pop()),
    h("div", { class: "warning", role: "note" },
      h("strong", {}, "The installation this Backup came from must never run again."),
      " A device can't run in two places: nothing tells the two copies apart. The old computer itself is free to reuse once DeFiOS is gone from it."),
    h("form", {
      class: "stack",
      onsubmit: (e) => {
        e.preventDefault();
        if (pass.value) act("Restore", ui.restore.file, pass.value);
      },
    },
    lead("Enter the Backup's passphrase", "restore-pass"),
    pass,
    h("button", { class: "btn bright", type: "submit", disabled: ui.busy }, "Build")),
    problemFor(s),
    h("button", { class: "link", type: "button", onclick: () => { ui.restore = null; ui.form = {}; render(true); } }, "Back"));
}

function creating(s) {
  return card(
    h("h1", {}, "Building this device"),
    h("p", { class: "sub" }, "Making this device's keys takes a few minutes. Keep DeFiOS open until it's done."),
    h("div", { class: "progress wide", role: "progressbar", "aria-label": "Building this device" }),
    h("ul", { class: "lines", "aria-live": "polite" }, (s.Progress ?? []).map((line) => h("li", {}, line))));
}

// restoredNote is what is left for the person after a Restore.
function restoredNote(s) {
  return h("div", { class: "warning left", role: "note" },
    h("strong", {}, "Restored from Backup."),
    h("ul", {},
      h("li", {}, "Never run DeFiOS from the original installation again."),
      h("li", {}, "In headscale, remove the original computer's registration."),
      h("li", {}, "If a Group's tailnet auth key was single-use or has expired, it needs a fresh one before that Group can connect.")),
    s.Restored.PluginsProblem && h("p", {}, `Plugins were not fetched again: ${s.Restored.PluginsProblem}`));
}

function stepTwo(s, adding) {
  const alias = field("alias", { id: "alias", placeholder: "Group name (e.g. ‘family’)", autocomplete: "off", "data-autofocus": true, disabled: ui.busy });
  const url = field("url", { id: "url", type: "url", placeholder: "https://headscale.example.com", autocomplete: "off", disabled: ui.busy });
  const key = field("authKey", { id: "auth-key", type: "password", placeholder: "Tailnet auth key", autocomplete: "off", disabled: ui.busy });
  const pass = !s.PassphraseHeld && field("groupPass", {
    id: "group-pass", type: "password", placeholder: "Passphrase", autocomplete: "current-password", disabled: ui.busy,
    "aria-invalid": s.Problem?.Code === "bad-passphrase" ? "true" : null,
  });
  const form = h("form", {
    class: "stack",
    onsubmit: (e) => {
      e.preventDefault();
      act("AddGroup", alias.value.trim(), url.value.trim(), key.value.trim(), pass ? pass.value : "");
    },
  },
  lead("A name for this Group, only this device sees", "alias"),
  alias,
  h("p", { class: "hint" }, "Lowercase letters, digits, - and _."),
  lead("The headscale server of the Group's tailnet", "url"),
  url,
  lead("A pre-auth key from that server", "auth-key"),
  key,
  h("p", { class: "hint" }, "It's kept in this device's Vault."),
  pass && lead("This device's passphrase", "group-pass"),
  pass,
  adding && h("div", { class: "notice" },
    `This device serves one Group at a time: setting up the new Group takes ${s.Served} offline here. You can switch back from the Group menu.`),
  h("button", { class: "btn bright", type: "submit", disabled: ui.busy }, "Continue"));

  return card(
    steps(2, adding),
    h("h1", {}, adding ? "Add a Group" : "Join a tailnet"),
    h("p", { class: "sub" }, "A Group's devices reach each other over a tailnet run by a headscale server."),
    s.Restored && restoredNote(s),
    form,
    problemFor(s),
    adding && h("button", { class: "link", type: "button", onclick: () => { ui.addingGroup = false; ui.form = {}; render(true); } }, "Cancel"));
}

// deviceRow shows a device by name and Device ID, with what follows.
function deviceRow(d, ...rest) {
  return h("div", { class: "devrow" },
    h("div", { class: "who" },
      h("strong", {}, d.Name || "Unnamed device"),
      h("code", { title: d.ID }, shortID(d.ID))),
    ...rest);
}

const chip = (text, kind = "") => h("span", { class: `chip ${kind}` }, text);

function stepThree(s) {
  const f = s.Forming;
  const p = f.Proposal;
  let body;
  if (p && !p.Mine && !p.Accepted) body = invitation(s, p);
  else if (p) body = waiting(s, f, p);
  else body = connect(s, f);
  const later = h("div", { class: "later" },
    h("button", {
      class: "link", type: "button", disabled: ui.busy,
      onclick: () => (f.Back ? act("Switch", f.Back) : act("Lock")),
    }, f.Back ? `Back to ${f.Back}` : "Set up later"),
    h("p", { class: "hint" }, `${f.Group} stays in your Group list; choose it there to finish setting it up.`));
  return card(steps(3, !!f.Back), ...body, problemFor(s), later);
}

// connect is step 3's choice: invite a device, or wait for an invite.
function connect(s, f) {
  const tab = (id, label) => h("button", {
    type: "button", role: "tab", "aria-selected": ui.invite === id ? "true" : "false",
    onclick: () => { ui.invite = id; discovery(); render(true); },
  }, label);
  const tabs = h("div", { class: "tabs", role: "tablist" }, tab("propose", "Invite a device"), tab("wait", "Wait for an invite"));
  return [
    h("h1", {}, `Connect ${f.Group}`),
    h("p", { class: "sub" }, "A Group is formed once every device in it accepts. One device invites; the others accept."),
    tabs,
    ui.invite === "propose" ? propose(s, f) : waitForInvite(s),
  ];
}

function propose(s, f) {
  const unreached = (f.NotReached ?? []).length > 0;
  const found = f.Discovered ?? [];
  if (unreached && !found.some((d) => d.ID === ui.pick)) ui.pick = f.Proposed[0]?.ID ?? "";
  const list = found.length
    ? h("div", { class: "devices", role: "radiogroup", "aria-label": "Devices on this tailnet" },
      found.map((d) => h("label", { class: "devrow pick" },
        h("input", {
          type: "radio", name: "device", value: d.ID, checked: ui.pick === d.ID, disabled: ui.busy,
          onchange: () => { ui.pick = d.ID; render(true); },
        }),
        h("div", { class: "who" }, h("strong", {}, d.Name || "Unnamed device"), h("code", { title: d.ID }, shortID(d.ID))),
        (f.NotReached ?? []).includes(d.ID) && chip("Not reached", "bad"))))
    : h("div", { class: "searching" },
      h("div", { class: "progress wide", role: "progressbar", "aria-label": "Looking for devices" }),
      h("p", { class: "hint" }, f.DiscoverProblem?.Code === "tailnet-down" || !f.DiscoverProblem
        ? "Looking for devices on this tailnet…"
        : f.DiscoverProblem.Message));
  const picked = found.find((d) => d.ID === ui.pick) ?? (f.Proposed ?? []).find((d) => d.ID === ui.pick);
  return h("div", { class: "stack" },
    unreached && h("div", { class: "warning", role: "alert" },
      h("strong", {}, `The invitation didn't reach ${(f.Proposed ?? []).filter((d) => f.NotReached.includes(d.ID)).map((d) => d.Name || shortID(d.ID)).join(", ")}.`),
      " It was withdrawn. Check that device is online with DeFiOS open on this Group, then try again."),
    h("p", { class: "lead" }, "Devices on this tailnet"),
    list,
    h("p", { class: "hint" }, "For now a Group is two devices: this one and one other."),
    h("button", {
      class: "btn bright", type: "button", disabled: ui.busy || !picked,
      onclick: () => act("Propose", [ui.pick]),
    }, unreached && picked && f.NotReached.includes(picked.ID) ? "Try again" : "Send invitation"));
}

function waitForInvite(s) {
  return h("div", { class: "stack" },
    h("p", { class: "lead" }, "On the other device, choose Invite a device and pick this one:"),
    h("div", { class: "devrow self" },
      h("div", { class: "who" }, h("strong", {}, `${nodeName(s)} (This Device)`), h("code", { class: "full" }, s.Node.ID))),
    h("div", { class: "progress wide", role: "progressbar", "aria-label": "Waiting for an invitation" }),
    h("p", { class: "hint" }, "Waiting for an invitation…"));
}

function invitation(s, p) {
  const from = p.Roster.find((m) => m.ID === p.Initiator);
  const reason = ui.declining && field("declineReason", { id: "decline-reason", placeholder: "Reason", "data-autofocus": true, autocomplete: "off", required: true, disabled: ui.busy });
  return [
    h("h1", {}, "You're invited to a Group"),
    h("p", { class: "sub" }, `${from?.Name || "Another device"} invites this device to form a Group with:`),
    h("div", { class: "devices" }, p.Roster.map((m) => deviceRow(m,
      m.Self ? chip("This Device") : m.ID === p.Initiator ? chip("Invited you", "ok") : null))),
    ui.declining
      ? h("form", {
        class: "stack",
        onsubmit: (e) => { e.preventDefault(); if (reason.value.trim()) act("Decline", reason.value.trim()); },
      },
      lead("Why decline? The other devices see this in their logs.", "decline-reason"),
      reason,
      h("div", { class: "row" },
        h("button", { class: "btn", type: "button", disabled: ui.busy, onclick: () => { ui.declining = false; render(true); } }, "Back"),
        h("button", { class: "btn danger", type: "submit", disabled: ui.busy }, "Decline")))
      : h("div", { class: "row" },
        h("button", { class: "btn", type: "button", disabled: ui.busy, onclick: () => { ui.declining = true; render(true); } }, "Decline"),
        h("button", { class: "btn bright", type: "button", disabled: ui.busy, onclick: () => act("Accept") }, "Accept")),
  ];
}

// waiting is a proposal this device made or accepted, until all accept.
function waiting(s, f, p) {
  const undelivered = f.Undelivered ?? [];
  return [
    h("h1", {}, "Waiting for devices to accept"),
    h("p", { class: "sub" }, `${f.Group} is formed once every device in it accepts.`),
    h("div", { class: "devices" }, p.Roster.map((m) => deviceRow(m,
      m.Self ? chip("This Device") : null,
      undelivered.includes(m.ID) ? chip("Didn't hear back", "bad")
        : m.Accepted ? chip("Accepted", "ok") : chip("Waiting…")))),
    undelivered.length > 0 && h("div", { class: "warning", role: "alert" },
      "Some devices haven't heard that this one accepted. Check they're online, then try again.",
      h("button", { class: "btn bright", type: "button", disabled: ui.busy, onclick: () => act("Accept") }, "Try again")),
    undelivered.length === 0 && h("div", { class: "progress wide", role: "progressbar", "aria-label": "Waiting for devices to accept" }),
  ];
}

function shell(s) {
  return h("div", { class: "shell" }, rail(), header(s), page(s));
}

function rail() {
  const item = (sec) => h("button", {
    "aria-label": sec.label, "aria-current": ui.page === sec.id ? "page" : null,
    onclick: () => go(sec.id),
  }, icon(sec.id), h("span", { class: "tip" }, sec.label));
  return h("nav", { class: "rail", "aria-label": "Sections" },
    mark(), SECTIONS.map(item), h("div", { class: "spacer" }), item(SETTINGS));
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
    h("span", { class: "state" }, g.Alias === s.Served ? "Online" : formed(g) ? "Offline" : "Not set up"))),
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

// go opens a section page; focus is what on it to open, such as
// { profile: "auditor" }, for the page to show once it has content
// (#151 to #155). A page opened anew reads its content again.
function go(id, focus = null) {
  const again = ui.page !== id;
  ui.page = id;
  ui.focus = focus;
  if (id === "agents" && focus?.profile) agentsOpen(focus.profile);
  if (again && state && LOADS[id]?.ready(state)) LOADS[id].refresh();
  render(true);
}

// pageLoading stands for page sec until its content is in: nothing of the
// page itself, so it shows whole or not at all.
function pageLoading(sec, s) {
  const key = `${sec.id}\u0000${s.EditGroup}`;
  if (attempts.get(key) === "failed") {
    return h("main", { class: "page center" },
      h("p", { role: "alert" }, `${sec.label} could not be loaded.`),
      h("button", { class: "btn", type: "button", onclick: () => { attempts.delete(key); render(true); } }, "Try again"));
  }
  wait(sec.id, s);
  const text = `Loading ${sec.label}…`;
  return h("main", { class: "page center", "aria-busy": "true" },
    h("div", { class: "progress", role: "progressbar", "aria-label": text }), h("p", {}, text));
}

function page(s) {
  const sec = [...SECTIONS, SETTINGS].find((x) => x.id === ui.page) ?? SECTIONS[0];
  if (LOADS[sec.id] && !LOADS[sec.id].ready(s)) return pageLoading(sec, s);
  // Talk is the served Group's, so it has no This Device / Group parts.
  if (sec.id === "talk") {
    return h("main", { class: "page talkpage" },
      s.Problem && h("div", { class: "banner", role: "alert" }, s.Problem.Message),
      backupBanner(s),
      talkPage(go));
  }
  return h("main", { class: "page" },
    h("h1", {}, sec.label),
    s.Problem && h("div", { class: "banner", role: "alert" }, s.Problem.Message),
    backupBanner(s),
    h("section", { class: "part", "aria-label": "This Device" },
      h("header", {}, h("h2", {}, "This Device")),
      h("div", { class: "body" }, nodePart(sec, s))),
    h("section", { class: "part", "aria-label": "Group" },
      h("header", {}, h("h2", {}, "Group"), editTarget(s)),
      !s.EditGroupServed && h("div", { class: "notice" }, `${s.EditGroup} is not online on this device. Changes here apply the next time it is.`),
      h("div", { class: "body" }, groupPart(sec, s))));
}

// nodePart is a section page's This Device part.
function nodePart(sec, s) {
  switch (sec.id) {
    case "settings": return nodeSettings(s);
    case "agents": return agentsDevicePart();
    case "vault": return vaultDevicePart();
  }
  return h("p", { class: "placeholder" }, `${sec.label} for this device comes later.`);
}

// groupPart is a section page's Group part, for s.EditGroup.
function groupPart(sec, s) {
  switch (sec.id) {
    case "settings": return settingsPart();
    case "agents": return agentsGroupPart();
    case "vault": return vaultGroupPart();
  }
  return h("p", { class: "placeholder" }, `${sec.label} for ${s.EditGroup} comes later.`);
}

// backupBanner asks for a Backup once a Key is minted, until one is
// taken from the app. Dismissed, it stays away until the next Key or until
// the app starts again: backupDismissed is the KeysMinted it was dismissed at.
function backupBanner(s) {
  if (!s.BackupDue || ui.backupDismissed === s.KeysMinted) return null;
  const run = async () => {
    if (ui.backingUp) return;
    ui.backingUp = true;
    ui.backupProblem = null;
    render(true);
    try {
      const v = await backupNow();
      ui.backupProblem = v?.Problem ?? null;
    } finally {
      ui.backingUp = false;
      render(true);
    }
  };
  return h("div", { class: "backupbanner", role: "status" },
    h("div", { class: "text" },
      h("strong", {}, "Back up this device"),
      h("span", {}, "A new Key was made. Its share on this device is in no Backup yet, so losing this computer would lose it."),
      ui.backupProblem && h("span", { class: "error", role: "alert" }, ui.backupProblem.Message)),
    h("button", { class: "btn bright", type: "button", disabled: ui.backingUp, onclick: run }, "Back up now"),
    h("button", { class: "dismiss", type: "button", "aria-label": "Dismiss until the next Key or until DeFiOS starts again", title: "Dismiss",
      onclick: () => { ui.backupDismissed = s.KeysMinted; render(true); } }, "\u00d7"));
}

// editTarget picks the Group the Group parts edit. The choice is kept
// across pages.
function editTarget(s) {
  return h("select", { "aria-label": "Group to edit", onchange: (e) => act("SetEditGroup", e.target.value) },
    s.Groups.map((g) => h("option", { value: g.Alias, selected: g.Alias === s.EditGroup },
      g.Alias === s.Served ? `${g.Alias} (online)` : g.Alias)));
}

// nodeSettings are This Device's preferences: the app's own, kept outside
// the Node directory.
function nodeSettings(s) {
  const toggle = (label, checked, method, attrs, ...extra) => h("label", { class: attrs.sub ? "toggle sub" : "toggle" },
    h("input", { type: "checkbox", checked, disabled: ui.busy || attrs.disabled, onchange: (e) => act(method, e.target.checked) }),
    h("span", { class: "text" }, h("strong", {}, label), ...extra));
  return h("div", {},
    toggle("Notifications", s.Prefs.Notifications, "SetNotifications", {},
      h("span", {}, "Let DeFiOS notify you, such as when a Sign Request needs you.")),
    toggle("New-message notifications", s.Prefs.Notifications && s.Prefs.MessageNotifications, "SetMessageNotifications",
      { sub: true, disabled: !s.Prefs.Notifications },
      h("span", {}, "Also notify of each new message from another device or an agent.")),
    toggle("Start at login", s.Prefs.StartAtLogin, "SetStartAtLogin", {},
      h("span", {}, "Start DeFiOS hidden in the tray when you log in. Without the keychain, it tells you it is locked.")),
    toggle("Keep the passphrase in the keychain", s.Prefs.Keychain, "SetKeychain", {},
      h("span", {}, "Unlock the last Group automatically when DeFiOS starts at login."),
      s.Prefs.Keychain && h("span", { class: "warn", role: "note" }, "Anyone who can use this computer account can unlock this device.")));
}

document.addEventListener("click", (e) => {
  if (ui.menuOpen && !e.target.closest(".groupmenu")) { ui.menuOpen = false; render(true); }
});
document.addEventListener("keydown", (e) => {
  if (e.key === "Escape" && ui.menuOpen) { ui.menuOpen = false; render(true); }
});

Events.On("shell:state", (ev) => apply(ev.data));
Events.On("talk:state", (ev) => {
  const was = talkReady();
  talkUpdate(ev.data);
  if (!was && talkReady() && ui.page === "talk") render(true);
});
// A notification was clicked: show the page it is about. The Go side has
// already opened its Conversation and card on Talk.
Events.On("app:notice", (ev) => {
  if (ev.data?.Group && state?.View === "served") {
    ui.page = "talk";
    ui.menuOpen = false;
    render(true);
  }
});
call("State").then(apply);
loadTalk();
