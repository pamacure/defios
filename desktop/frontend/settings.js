// The Settings page's Group part, for the edit-target Group: its settings
// form, its devices (the Roster), Device Usage (Node Usage) and Token
// Usage. It renders the shell.GroupSettingsView the Go side reports
// (main.UI.GroupSettings) and keeps the devices and usage current while it
// is on screen. The devices come apart (main.UI.GroupRoster): which are
// online is asked of them over the tailnet, so they may come last. The
// form is drawn once per Group, so a redraw of the app never drops what is
// being typed. UI text says "device" for a Node.
import { call, h } from "./dom.js";

let gv = null; // the GroupSettingsView shown
let rv = null; // the Roster shown: the GroupRoster view, for gv's Group
// The part's body, made once: the form, kept across redraws, and the
// devices and usage, redrawn as they change.
const formBox = h("div", {});
const statusBox = h("div", { class: "gstatus" });
const mount = h("div", { class: "gsettings" }, formBox, statusBox);
let pending = null; // the settings load under way: its promise
let rosterLoading = ""; // the Group whose Roster is being read
let formGroup = ""; // the Group the form is drawn for
let dirty = false; // something has been typed since the form was drawn
let busy = false;
let loading = "";
let timer = null;
let unreadable = false; // the settings could not be read, so no form shows

const REFRESH_MS = 5000;

// settingsPart is the Group part's body for s.EditGroup. The app shows it
// only once settingsReady.
export function settingsPart() {
  poll();
  return mount;
}

// settingsReady: the settings for s.EditGroup are loaded, so the page can
// show whole. The devices need not be: they say they are being checked.
export const settingsReady = (s) => gv?.Group === s.EditGroup;

// settingsLoad loads the settings for s.EditGroup unless loaded or
// loading, and resolves once they are; the devices load alongside.
export function settingsLoad(s) {
  if (settingsReady(s)) return Promise.resolve();
  if (loading === s.EditGroup) return pending;
  pending = load(s.EditGroup);
  return pending;
}

// settingsRefresh reads the settings again, as when the page is opened
// anew. The form is drawn anew only if nothing was typed in it.
export async function settingsRefresh() {
  if (!gv || loading || busy) return;
  const g = gv.Group;
  loadRoster(g);
  const next = await call("GroupSettings");
  if (!next || next.Group !== g || gv.Group !== g || busy) return;
  unreadable = !!next.Problem;
  show(next, false);
}

async function load(group) {
  loading = group;
  loadRoster(group);
  let next;
  try {
    next = await call("GroupSettings");
  } finally {
    loading = "";
  }
  if (!next || next.Group !== group) return;
  if (rv && rv.Group !== group) rv = null;
  unreadable = !!next.Problem;
  show(next, true);
}

// loadRoster reads group's Roster, which asks the devices over the
// tailnet, and shows it once in.
async function loadRoster(group) {
  if (rosterLoading === group) return;
  rosterLoading = group;
  let next;
  try {
    next = await call("GroupRoster");
  } catch {
    return; // the next poll tries again
  } finally {
    if (rosterLoading === group) rosterLoading = "";
  }
  if (!next || next.Group !== group) return;
  rv = next;
  if (gv?.Group === group) drawStatus();
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
    if (!mount.isConnected) {
      clearInterval(timer);
      timer = null;
      return;
    }
    if (busy || !gv) return;
    loadRoster(gv.Group);
    const next = await call("GroupSettings");
    if (!next || next.Group !== gv.Group) return;
    // Only the status: the form keeps what was typed and any refusal.
    gv = { ...gv, Served: next.Served, Usage: next.Usage, UsageProblem: next.UsageProblem,
      Tokens: next.Tokens, TokensProblem: next.TokensProblem };
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
  const r = rv?.Group === gv.Group ? rv : null;
  if (!r) {
    // The Roster is asked of the devices over the tailnet: say so, so the
    // wait reads as the network's and not the app's.
    return h("section", { class: "gsec", "aria-label": "Devices", "aria-busy": "true" },
      h("h3", {}, "Devices"),
      h("div", { class: "pinging", role: "status" },
        h("div", { class: "progress" }),
        h("p", { class: "hint" }, gv.Served
          ? `Pinging ${gv.Group}'s devices over the tailnet to see which are online…`
          : `Reading ${gv.Group}'s devices…`)));
  }
  const list = r.Roster ?? [];
  const online = list.filter((d) => d.Reachable).length;
  return h("section", { class: "gsec", "aria-label": "Devices" },
    h("h3", {}, "Devices",
      r.Served && list.length > 0 && h("span", { class: "count" }, `${online} of ${list.length} devices online`)),
    r.RosterProblem
      ? h("p", { class: "hint" }, r.RosterProblem.Message)
      : h("div", { class: "devices" }, list.map((d) => h("div", { class: "devrow" },
        h("div", { class: "who" },
          h("strong", {}, d.Name || "Unnamed device", d.Self ? " (This Device)" : ""),
          h("code", { title: d.ID }, `Device ID ${shortID(d.ID)}`)),
        r.Served
          ? h("span", { class: `chip ${d.Reachable ? "ok" : "bad"}` }, d.Reachable ? "Online" : "Offline")
          : null))),
    !r.Served && !r.RosterProblem && h("p", { class: "hint" }, "Which devices are online shows while this Group is online here."));
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
