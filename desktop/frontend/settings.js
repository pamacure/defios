// The Settings page's Group part, for the edit-target Group: its settings
// form, its devices (the Roster), Device Usage (Node Usage) and Token
// Usage. It renders the shell.GroupSettingsView the Go side reports
// (main.UI.GroupSettings) and keeps the devices and usage current while it
// is on screen. The form is drawn once per Group, so a redraw of the app
// never drops what is being typed. UI text says "device" for a Node.
import { call, h } from "./dom.js";

let gv = null; // the GroupSettingsView shown
let mount = null; // the part's body, while on screen
let formBox = null; // the form, kept across redraws
let statusBox = null; // devices and usage, redrawn as they change
let formGroup = ""; // the Group the form is drawn for
let dirty = false; // something has been typed since the form was drawn
let busy = false;
let loading = "";
let timer = null;
let unreadable = false; // the settings could not be read, so no form shows

const REFRESH_MS = 5000;

// settingsPart is the Group part's body for s.EditGroup.
export function settingsPart(s) {
  mount ??= h("div", { class: "gsettings" });
  if (!formBox) {
    formBox = h("div", {});
    statusBox = h("div", { class: "gstatus" });
    mount.append(formBox, statusBox);
  }
  if (gv?.Group !== s.EditGroup && loading !== s.EditGroup) {
    // Show nothing of the Group chosen before while this one loads.
    formBox.replaceChildren();
    statusBox.replaceChildren();
    load(s.EditGroup);
  }
  poll();
  return mount;
}

async function load(group) {
  loading = group;
  let next;
  try {
    next = await call("GroupSettings");
  } finally {
    loading = "";
  }
  if (!next || next.Group !== group) return;
  unreadable = !!next.Problem;
  show(next, true);
}

// show takes next as the view; redrawForm draws the form from it anew.
function show(next, redrawForm) {
  gv = next;
  if (redrawForm || formGroup !== gv.Group || !dirty) drawForm();
  drawStatus();
}

// poll refreshes the devices and usage while the part is on screen.
function poll() {
  if (timer) return;
  timer = setInterval(async () => {
    if (!mount?.isConnected) {
      clearInterval(timer);
      timer = null;
      return;
    }
    if (busy || !gv) return;
    const next = await call("GroupSettings");
    if (!next || next.Group !== gv.Group) return;
    // Only the status: the form keeps what was typed and any refusal.
    gv = { ...gv, Roster: next.Roster, RosterProblem: next.RosterProblem, Served: next.Served,
      Usage: next.Usage, UsageProblem: next.UsageProblem, Tokens: next.Tokens, TokensProblem: next.TokensProblem };
    drawStatus();
  }, REFRESH_MS);
}

const FIELDS = [
  { name: "DisplayName", field: "display_name", label: "Device name in this Group", hint: "What the other devices in this Group call this one." },
  { name: "HeadscaleURL", field: "headscale_url", label: "Headscale server", type: "url", placeholder: "https://headscale.example.com" },
  { name: "Hostname", field: "hostname", label: "Tailnet hostname", hint: "This device's name on the Group's tailnet." },
  { name: "Prune", field: "prune", label: "Delete old talk after", placeholder: "Keep everything", hint: "An age such as 90d or 12h. Leave empty to keep all talk on this device." },
];

function drawForm() {
  formGroup = gv.Group;
  dirty = false;
  if (!gv.Group) {
    formBox.replaceChildren();
    return;
  }
  if (unreadable) {
    formBox.replaceChildren(h("p", { class: "error", role: "alert" }, gv.Problem.Message));
    return;
  }
  const inputs = {};
  // edited marks the form typed in, so a refresh keeps it as it is.
  const edited = (el) => el.addEventListener("input", () => { dirty = true; saved.textContent = ""; });
  const row = (f, input) => {
    const err = gv.Field === f.field && gv.Problem;
    if (err) input.setAttribute("aria-invalid", "true");
    return h("div", { class: "frow" },
      h("label", { for: `gs-${f.field}` }, f.label),
      h("div", { class: "fcell" },
        input,
        err ? h("p", { class: "error", role: "alert" }, gv.Problem.Message) : f.hint && h("p", { class: "hint" }, f.hint)));
  };
  const rows = FIELDS.map((f) => {
    const input = h("input", {
      class: "field left", id: `gs-${f.field}`, type: f.type ?? "text", placeholder: f.placeholder ?? "",
      autocomplete: "off", value: gv.Settings[f.name] ?? "",
    });
    edited(input);
    inputs[f.name] = input;
    return row(f, input);
  });
  const key = h("input", {
    class: "field left", id: "gs-tailnet_auth_key", type: "password", autocomplete: "off",
    placeholder: "Leave empty to keep the current key",
  });
  edited(key);
  inputs.AuthKey = key;
  rows.push(row({
    field: "tailnet_auth_key", label: "Tailnet auth key",
    hint: gv.AuthKeySecret
      ? `Kept in this device's Vault as ${gv.AuthKeySecret}. Paste a new pre-auth key to replace it.`
      : "None is set. Paste a pre-auth key from the headscale server to keep it in this device's Vault.",
  }, key));

  const saved = h("span", { class: "saved", "aria-live": "polite" }, gv.Saved && !gv.Problem ? "Saved." : "");
  const submit = h("button", { class: "btn primary", type: "submit", disabled: busy }, "Save");
  const form = h("form", {
    class: "gform",
    onsubmit: async (e) => {
      e.preventDefault();
      const d = {};
      for (const [k, el] of Object.entries(inputs)) d[k] = el.value.trim();
      busy = true;
      submit.disabled = true;
      try {
        const next = await call("SaveGroupSettings", d);
        if (next && next.Group === formGroup) show(next, true);
      } finally {
        busy = false;
        submit.disabled = false;
      }
      // Back to the field refused, with what was typed kept.
      if (gv.Field) formBox.querySelector(`#gs-${gv.Field}`)?.focus();
    },
  },
  rows,
  gv.Problem && !gv.Field && h("p", { class: "error", role: "alert" }, gv.Problem.Message),
  h("div", { class: "factions" }, saved, submit));
  formBox.replaceChildren(
    h("h3", {}, "Group settings"),
    gv.Served && h("div", { class: "notice inline" },
      `${gv.Group} is online on this device: changes apply the next time it comes online here, such as after Lock and Unlock.`),
    form);
}

function drawStatus() {
  if (!gv.Group) {
    statusBox.replaceChildren();
    return;
  }
  statusBox.replaceChildren(devices(), deviceUsage(), tokenUsage());
}

const shortID = (id) => (id.length > 16 ? `${id.slice(0, 16)}…` : id);

function devices() {
  const list = gv.Roster ?? [];
  const online = list.filter((d) => d.Reachable).length;
  return h("section", { class: "gsec", "aria-label": "Devices" },
    h("h3", {}, "Devices",
      gv.Served && list.length > 0 && h("span", { class: "count" }, `${online} of ${list.length} devices online`)),
    gv.RosterProblem
      ? h("p", { class: "hint" }, gv.RosterProblem.Message)
      : h("div", { class: "devices" }, list.map((d) => h("div", { class: "devrow" },
        h("div", { class: "who" },
          h("strong", {}, d.Name || "Unnamed device", d.Self ? " (This Device)" : ""),
          h("code", { title: d.ID }, `Device ID ${shortID(d.ID)}`)),
        gv.Served
          ? h("span", { class: `chip ${d.Reachable ? "ok" : "bad"}` }, d.Reachable ? "Online" : "Offline")
          : null))),
    !gv.Served && !gv.RosterProblem && h("p", { class: "hint" }, "Which devices are online shows while this Group is online here."));
}

function bytes(n) {
  const units = ["B", "KB", "MB", "GB", "TB"];
  let i = 0;
  while (n >= 1024 && i < units.length - 1) { n /= 1024; i++; }
  return `${i === 0 ? n : n.toFixed(n < 10 ? 1 : 0)} ${units[i]}`;
}

function duration(secs) {
  if (secs < 60) return `${secs.toFixed(1)} s`;
  const m = Math.floor(secs / 60);
  if (m < 60) return `${m} min ${Math.round(secs % 60)} s`;
  return `${Math.floor(m / 60)} h ${m % 60} min`;
}

const stat = (label, value) => h("div", { class: "stat" }, h("span", { class: "label" }, label), h("strong", {}, value));

// deviceUsage is Node Usage: what DeFiOS takes up on this computer. It
// says nothing of agents' tokens, which tokenUsage shows apart.
function deviceUsage() {
  const u = gv.Usage;
  const p = u?.Process;
  return h("section", { class: "gsec", "aria-label": "Device Usage" },
    h("h3", {}, "Device Usage"),
    h("p", { class: "hint" }, "What DeFiOS takes up on this computer."),
    gv.UsageProblem
      ? h("p", { class: "error" }, gv.UsageProblem.Message)
      : u && h("div", { class: "stats" },
        stat(`Disk for ${gv.Group}`, bytes(u.GroupBytes)),
        stat("Disk for this device", bytes(u.NodeBytes)),
        p ? stat("Memory", p.Memory == null ? "—" : bytes(p.Memory)) : null,
        p ? stat("CPU time", p.CPUSeconds == null ? "—" : duration(p.CPUSeconds)) : null),
    u && p && p.Group !== gv.Group && h("p", { class: "hint" }, `Memory and CPU are of DeFiOS running ${p.Group}.`),
    u && !p && h("p", { class: "hint" }, "Memory and CPU show while a Group is online on this device."));
}

// tokenUsage is Token Usage: what this device's agents consumed calling
// their Providers in this Group.
function tokenUsage() {
  const list = gv.Tokens ?? [];
  const n = (x) => x.toLocaleString();
  return h("section", { class: "gsec", "aria-label": "Token Usage" },
    h("h3", {}, "Token Usage"),
    h("p", { class: "hint" }, `Tokens this device's agents used in ${gv.Group}, from the runs it kept.`),
    gv.TokensProblem
      ? h("p", { class: "error" }, gv.TokensProblem.Message)
      : list.length === 0
        ? h("p", { class: "placeholder" }, "No agent has run on this device in this Group yet.")
        : h("table", { class: "tokens" },
          h("thead", {}, h("tr", {}, h("th", {}, "Agent"), h("th", {}, "Input tokens"), h("th", {}, "Output tokens"))),
          h("tbody", {}, list.map((t) => h("tr", {},
            h("td", {}, `@${t.Handle}`), h("td", {}, n(t.Input)), h("td", {}, n(t.Output)))))));
}
