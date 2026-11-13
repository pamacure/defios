// The Vault page. This Device: Back up now and the passphrase change.
// Group: the edit-target Group's Secrets, each with where it comes from
// and the config fields that use it, to set, reveal and remove. A value is
// revealed only with the passphrase typed for it, every time. It renders
// the shell.VaultView the Go side reports (main.UI.Vault) and redraws only
// on its own changes or a new edit-target Group, so a redraw of the app
// never drops what is being typed. UI text says "device" for a Node.
import { call, h, put } from "./dom.js";
import { SOURCE, forgetSecrets, sourceChip } from "./secretfield.js";

let vv = null; // the VaultView shown
let group = ""; // the Group it is for
let loading = "";
let busy = false;
let setting = ""; // the Secret whose value form shows; NEW for a new one
let revealing = ""; // the Secret whose passphrase prompt shows
let removing = ""; // the Secret whose Remove asks to confirm
let shown = null; // { name, value } revealed, until the next draw
// The parts' bodies, made once: a view loaded while the page is not on
// screen draws into them, ready for when it is.
const deviceMount = h("div", { class: "vault" });
const groupMount = h("div", { class: "vault" });
let pending = null; // the load under way: its promise
// The passphrase form's fields, kept across draws so nothing typed is lost.
let passFields = null;

const NEW = "\u0000new";

// vaultDevicePart is This Device's part: Backup and the passphrase. The
// app shows it only once vaultReady.
export function vaultDevicePart() {
  return deviceMount;
}

// vaultGroupPart is the Group part: the edit-target Group's Secrets.
export function vaultGroupPart() {
  if (shown && !groupMount.isConnected) {
    // Back on the page: a value revealed before is hidden again.
    shown = null;
    draw();
  }
  return groupMount;
}

// vaultReady: the view for s.EditGroup is loaded, so the page can show
// whole.
export const vaultReady = (s) => !!vv && group === s.EditGroup;

// vaultLoad loads the view for s.EditGroup unless it is loaded or loading,
// and resolves once that load is done.
export function vaultLoad(s) {
  if (vaultReady(s)) return Promise.resolve();
  if (loading === s.EditGroup) return pending;
  pending = load(s.EditGroup);
  return pending;
}

// vaultRefresh reads the view again, as when the page is opened anew, and
// redraws only if it changed and no form is open: what is typed stays.
export async function vaultRefresh() {
  if (!vv || loading || busy) return;
  const g = group;
  const next = await call("Vault");
  if (!next || next.Group !== g || g !== group || busy || setting || revealing || removing) return;
  if (deviceMount.contains(document.activeElement) || groupMount.contains(document.activeElement)) return; // being typed in
  if (JSON.stringify(next) === JSON.stringify(vv)) return;
  vv = next;
  draw();
}

// backupNow asks where to write a Backup, then writes it: from the page
// or the "Back up now" banner. It resolves to the VaultView after, or null
// when no file was chosen.
export async function backupNow() {
  const file = await call("ChooseBackupDestination");
  if (!file) return null;
  const next = await call("BackupTo", file);
  if (next && next.Group === group) {
    vv = next;
    draw();
  }
  return next;
}

async function load(g) {
  loading = g;
  let next;
  try {
    next = await call("Vault");
  } finally {
    loading = "";
  }
  if (!next || next.Group !== g) return;
  if (g !== group) {
    // A Group not shown before: nothing of the one before carries over.
    setting = revealing = removing = "";
    shown = null;
  }
  group = g;
  vv = next;
  draw();
}

// act calls method and shows the view it returns; after runs on success.
// A change to the Secrets passes changed, so the Secret field lists them
// anew.
async function act(method, args, after = () => {}) {
  if (busy) return;
  busy = true;
  shown = null;
  draw();
  let next;
  try {
    next = await call(method, ...args);
  } finally {
    busy = false;
  }
  if (next && !next.Problem) after();
  if (next && next.Group === group) vv = next;
  draw();
}

const problemAt = (where) => vv?.Problem && vv.Where === where ? vv.Problem : null;
const doneAt = (where, done) => !vv?.Problem && vv?.Where === where && vv?.Done === done;

function draw() {
  if (!vv) return;
  if (deviceMount) put(deviceMount, backupSection(), passphraseSection());
  if (groupMount) put(groupMount, secretsSection());
}

function when(t) {
  const d = new Date(t);
  return d.toLocaleString(undefined, { dateStyle: "medium", timeStyle: "short" });
}

function backupSection() {
  const err = problemAt("backup");
  return h("section", { class: "vsec", "aria-label": "Backup" },
    h("h3", {}, "Backup"),
    h("p", { class: "hint" }, "A Backup holds this device's keys, Secrets and settings for every Group, sealed under its passphrase. Keep it off this computer: with it and the passphrase, this device can be restored elsewhere."),
    h("div", { class: "vrow" },
      h("button", { class: "btn primary", type: "button", disabled: busy, onclick: () => { if (!busy) backupNow(); } }, "Back up now"),
      h("span", { class: doneAt("backup", "backup") ? "saved" : "hint", "aria-live": "polite" },
        doneAt("backup", "backup") ? `Backed up ${when(vv.LastBackup)}.`
          : vv.LastBackup ? `Last backed up ${when(vv.LastBackup)}.` : "Not backed up from this app yet.")),
    err && h("p", { class: "error", role: "alert" }, err.Message));
}

function passphraseSection() {
  passFields ??= {
    old: h("input", { class: "field left", id: "vp-old", type: "password", autocomplete: "current-password" }),
    next: h("input", { class: "field left", id: "vp-next", type: "password", autocomplete: "new-password" }),
    again: h("input", { class: "field left", id: "vp-again", type: "password", autocomplete: "new-password" }),
  };
  const err = problemAt("passphrase");
  const row = (name, label) => {
    const input = passFields[name];
    const bad = err && vv.Field === name;
    input.setAttribute("aria-invalid", bad ? "true" : "false");
    input.disabled = busy;
    return h("div", { class: "frow" },
      h("label", { for: input.id }, label),
      h("div", { class: "fcell" }, input, bad && h("p", { class: "error", role: "alert" }, err.Message)));
  };
  const done = doneAt("passphrase", "passphrase");
  const submit = h("button", { class: "btn primary", type: "submit", disabled: busy }, "Change passphrase");
  return h("section", { class: "vsec", "aria-label": "Passphrase" },
    h("h3", {}, "Passphrase"),
    h("div", { class: "notice inline" },
      "Changing it stops this device serving its Group, then brings it back online with the new passphrase. Backups taken before still open only with the old one, so take a new Backup after."),
    h("form", {
      class: "gform",
      onsubmit: (e) => {
        e.preventDefault();
        const { old, next, again } = passFields;
        act("ChangePassphrase", [old.value, next.value, again.value], () => {
          old.value = next.value = again.value = "";
        }).then(() => { if (vv?.Field && passFields[vv.Field]) passFields[vv.Field].focus(); });
      },
    },
    row("old", "Current passphrase"),
    row("next", "New passphrase"),
    row("again", "New passphrase again"),
    err && !vv.Field && h("p", { class: "error", role: "alert" }, err.Message),
    h("div", { class: "factions" },
      h("span", { class: "saved", "aria-live": "polite" }, done ? "Passphrase changed. This device is online again." : ""),
      submit)));
}

function secretsSection() {
  const list = vv.Secrets ?? [];
  return h("section", { class: "vsec first", "aria-label": "Secrets" },
    h("h3", {}, "Secrets"),
    h("p", { class: "hint" }, `Values the config of ${group} refers to by name, such as API keys. A Secret in the environment wins over the Vault's.`),
    vv.SecretsProblem
      ? h("p", { class: "error", role: "alert" }, vv.SecretsProblem.Message)
      : list.length === 0
        ? h("p", { class: "placeholder" }, "This Group has no Secrets yet.")
        : h("div", { class: "secrets" }, list.map(secretRow)),
    setting === NEW
      ? valueForm(null)
      : h("button", { class: "btn", type: "button", disabled: busy, onclick: () => { setting = NEW; revealing = removing = ""; draw(); } }, "+ New Secret"));
}

function secretRow(s) {
  const where = `secret:${s.Name}`;
  const err = problemAt(where);
  const open = (what) => () => {
    setting = what === "set" ? s.Name : "";
    revealing = what === "reveal" ? s.Name : "";
    removing = what === "remove" ? s.Name : "";
    shown = null;
    draw();
  };
  const revealed = shown?.name === s.Name;
  return h("div", { class: "secret" },
    h("div", { class: "secrethead" },
      h("code", {}, s.Name),
      h("span", { class: sourceChip(s.Source) }, SOURCE[s.Source] ?? s.Source),
      h("div", { class: "actions" },
        h("button", { class: "link", type: "button", disabled: busy, onclick: open("set") }, s.Source === "vault" ? "Replace value" : "Set value"),
        s.Source !== "none" && h("button", { class: "link", type: "button", disabled: busy, onclick: revealed ? () => { shown = null; draw(); } : open("reveal") }, revealed ? "Hide" : "Reveal"),
        s.Source !== "none" && h("button", { class: "link danger", type: "button", disabled: busy, onclick: open("remove") }, "Remove"))),
    h("p", { class: "hint" },
      (s.Uses ?? []).length === 0 ? "No config field uses it."
        : ["Used by ", ...s.Uses.flatMap((u, i) => [i > 0 && ", ", h("code", {}, u.Field), ` (${u.Place})`])]),
    s.Source === "env" && setting === s.Name && h("p", { class: "notice inline" }, "This device's environment supplies it, so a value set here is used only once the environment's is gone."),
    setting === s.Name && valueForm(s.Name),
    revealing === s.Name && revealForm(s.Name),
    revealed && h("div", { class: "revealed" },
      h("code", { class: "value" }, shown.value || "(empty)"),
      shown.source === "env" && h("span", { class: "hint" }, "From this device's environment.")),
    removing === s.Name && h("div", { class: "confirm" },
      s.Source === "env" ? `Remove the Vault's value of ${s.Name}, if it has one? The environment's stays in use.`
        : (s.Uses ?? []).length > 0 ? `Remove ${s.Name}? The fields that use it get no value until it is set again.` : `Remove ${s.Name} from the Vault?`,
      h("button", { class: "btn danger", type: "button", disabled: busy, onclick: () => act("RemoveVaultSecret", [s.Name], () => { removing = ""; forgetSecrets(); }) }, "Remove"),
      h("button", { class: "btn", type: "button", onclick: () => { removing = ""; draw(); } }, "Cancel")),
    err && h("p", { class: "error", role: "alert" }, err.Message),
    doneAt(where, "set") && h("p", { class: "saved" }, "Saved."));
}

// valueForm sets a Secret's value: name's, or a new one's when null.
function valueForm(name) {
  const where = name ? `secret:${name}` : "new-secret";
  const err = problemAt(where);
  const nameIn = !name && h("input", { class: "field left", placeholder: "NAME_OF_SECRET", autocomplete: "off", "aria-label": "Secret name",
    "aria-invalid": err && vv.Field === "name" ? "true" : "false" });
  const value = h("input", { class: "field left", type: "password", placeholder: "Value", autocomplete: "off", "aria-label": "Secret value",
    "aria-invalid": err && vv.Field === "value" ? "true" : "false" });
  setTimeout(() => (nameIn || value).focus());
  return h("form", {
    class: "newsecret",
    onsubmit: (e) => {
      e.preventDefault();
      act("SaveVaultSecret", [{ New: !name, Name: name ?? nameIn.value.trim(), Value: value.value }], () => { setting = ""; forgetSecrets(); });
    },
  },
  nameIn, value,
  h("p", { class: "hint" }, "Kept in this device's Vault for this Group."),
  !name && err && h("p", { class: "error", role: "alert" }, err.Message),
  h("div", { class: "row" },
    h("button", { class: "btn primary", type: "submit", disabled: busy }, "Save Secret"),
    h("button", { class: "btn", type: "button", onclick: () => { setting = ""; draw(); } }, "Cancel")));
}

// revealForm asks for the passphrase, every time, to show name's value.
function revealForm(name) {
  const pass = h("input", { class: "field left", type: "password", placeholder: "Passphrase", autocomplete: "current-password", "aria-label": "Passphrase" });
  const err = h("p", { class: "error", role: "alert" });
  const go = h("button", { class: "btn primary", type: "submit" }, "Reveal");
  setTimeout(() => pass.focus());
  return h("form", {
    class: "newsecret",
    onsubmit: async (e) => {
      e.preventDefault();
      go.disabled = true;
      let r;
      try {
        r = await call("RevealSecret", name, pass.value);
      } finally {
        go.disabled = false;
        pass.value = "";
      }
      if (!r || r.Problem) {
        err.textContent = r?.Problem?.Message ?? "";
        pass.setAttribute("aria-invalid", "true");
        pass.focus();
        return;
      }
      revealing = "";
      vv = { ...vv, Problem: null, Where: "", Done: "" };
      shown = { name, value: r.Value, source: r.Source };
      draw();
    },
  },
  pass,
  h("p", { class: "hint" }, "Enter this device's passphrase to show the value."),
  err,
  h("div", { class: "row" }, go, h("button", { class: "btn", type: "button", onclick: () => { revealing = ""; draw(); } }, "Cancel")));
}
