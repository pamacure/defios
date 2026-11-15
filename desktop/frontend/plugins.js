// The Plugins page, this device's only: its Plugins, built in, installed
// or missing, with the Skills and MCP Servers each brings. An install or
// update is fetched first and previewed with what it brings; only
// Install or Update puts it in place. Remove is refused while an agent
// grants an item of the Plugin, naming them. It renders the
// shell.PluginsView the Go side reports (main.UI.Plugins) and redraws only
// on its own changes, so a redraw of the app never drops what is typed.
// UI text says "device" for a Node.
import { call, h, put } from "./dom.js";
import { callFailed, markInvalid, problemBox, showProblem } from "./notice.js";

let pv = null; // the PluginsView shown
let loading = null; // the load under way: its promise
let busy = ""; // what runs: "fetch", "commit", "remove:<name>" or ""
let source = ""; // the source typed
let removing = ""; // the Plugin whose Remove asks to confirm
let badSource = false; // the source was refused: outlined until edited
let goTo = () => {}; // opens another page, as go in app.js
const mount = h("div", { class: "agents plugins" });

// pluginsDevicePart is This Device's part; go opens another page.
export function pluginsDevicePart(go) {
  goTo = go;
  return mount;
}

export const pluginsReady = () => !!pv;

export function pluginsLoad() {
  if (pv) return Promise.resolve();
  loading ??= call("Plugins").then((v) => { if (v) { pv = v; draw(); } }).finally(() => { loading = null; });
  return loading;
}

// pluginsRefresh reads the view again, as when the page is opened anew,
// and redraws only if it changed and nothing is under way.
export async function pluginsRefresh() {
  if (!pv || busy || removing || pv.Stage) return;
  const next = await call("Plugins");
  if (!next || busy || removing || mount.contains(document.activeElement)) return;
  if (JSON.stringify(next) === JSON.stringify(pv)) return;
  pv = next;
  draw();
}

async function act(what, method, ...args) {
  if (busy) return;
  busy = what;
  badSource = false;
  draw();
  let next;
  try {
    next = await call(method, ...args);
  } catch (err) {
    showProblem(callFailed(err), { scope: "plugins" });
  } finally {
    busy = "";
  }
  if (next) {
    const p = next.Problem;
    // Refused while agents grant its items: a way to each, to revoke them.
    const actions = p?.Code === "plugin-granted"
      ? (next.Grants ?? []).map((g) => ({ label: `Open @${g.Handle}`, run: () => goTo("agents", { profile: g.Handle }) }))
      : [];
    showProblem(p, { scope: "plugins", actions });
    badSource = !!p && next.Field === "source";
    pv = { ...next, Problem: null };
    removing = "";
    if (method === "StagePluginInstall" && next.Stage) source = "";
  }
  draw();
  if (badSource) document.getElementById("pl-source")?.focus();
}
const shortPin = (pin) => (/^[0-9a-f]{40}$/.test(pin) ? pin.slice(0, 12) : pin);

function draw() {
  if (!pv) return;
  const installed = (pv.Plugins ?? []).filter((p) => !p.BuiltIn);
  put(mount,
    h("p", { class: "hint lead" }, "Plugins bring Skills and MCP Servers that this device's agents may be granted on the Agents page. A Plugin is this device's own: it is never shared with a Group."),
    pv.Stage ? preview(pv.Stage) : installForm(installed.length > 0),
    pv.Done && h("p", { class: "saved", "aria-live": "polite" }, pv.Done),
    pv.ListProblem
      ? problemBox(pv.ListProblem)
      : h("div", { class: "providers" }, (pv.Plugins ?? []).map(pluginCard)));
}

// installForm takes a source to fetch a Plugin from, and offers Update all.
function installForm(anyInstalled) {
  const input = h("input", {
    id: "pl-source", class: "field left mono", autocomplete: "off", spellcheck: "false", value: source,
    placeholder: "https://github.com/acme/audit-plugin@v1.2.0 or /path/on/this/device",
    disabled: !!busy,
    oninput: (e) => { source = e.target.value; },
  });
  if (badSource) markInvalid(input, () => { badSource = false; });
  return h("form", {
    class: "gform plinstall",
    onsubmit: (e) => { e.preventDefault(); act("fetch", "StagePluginInstall", input.value); },
  },
  h("div", { class: "frow" },
    h("label", { for: "pl-source" }, "Install a Plugin"),
    h("div", { class: "fcell" },
      h("div", { class: "inline" }, input,
        h("button", { class: "btn primary", type: "submit", disabled: !!busy }, busy === "fetch" ? "Fetching…" : "Fetch")),
      h("p", { class: "hint" }, "A git URL, url@ref for a branch, tag or commit, or a path on this device. You see what it brings before anything is installed."))),
  anyInstalled && h("div", { class: "frow" },
    h("span", {}),
    h("div", { class: "fcell" },
      h("button", { class: "btn start", type: "button", disabled: !!busy, onclick: () => act("fetch", "StagePluginUpdate", "") },
        busy === "fetch" ? "Fetching…" : "Update all…"))));
}

// preview shows what the staged install or update brings, to Commit or
// Cancel.
function preview(stage) {
  const plugins = stage.Plugins ?? [];
  const toCommit = plugins.filter((p) => !p.UpToDate && !p.Problem);
  const title = stage.Install ? `Install ${plugins[0]?.Name}?`
    : toCommit.length === 0 ? "Nothing to update" : `Update ${toCommit.map((p) => p.Name).join(", ")}?`;
  return h("section", { class: "plpreview", "aria-label": "Preview" },
    h("h3", {}, title),
    h("p", { class: "hint" }, stage.Install
      ? "Fetched and read, not installed yet. Its MCP Servers run these programs on this device when an agent granted them is summoned."
      : "Fetched again from each Plugin's source, at the ref it was installed at. Nothing changes until you update."),
    plugins.map((sp) => h("div", { class: "provider" },
      sp.Problem
        ? [h("div", { class: "prow" }, h("div", { class: "who" }, h("strong", {}, sp.Name)), h("span", { class: "chip bad" }, "Not fetched")),
          problemBox(sp.Problem)]
        : stagedBody(sp, stage.Install))),
    h("div", { class: "factions" },
      h("button", { class: "btn", type: "button", disabled: !!busy, onclick: () => act("cancel", "CancelPlugins") }, toCommit.length ? "Cancel" : "Close"),
      toCommit.length > 0 && h("button", { class: "btn primary", type: "button", disabled: !!busy, onclick: () => act("commit", "CommitPlugins", stage.ID) },
        busy === "commit" ? "Installing…" : stage.Install ? "Install" : "Update")));
}

function stagedBody(sp, install) {
  const p = sp.Plugin;
  const version = install ? p.Version : sp.UpToDate ? `${p.Version}, up to date` : `${sp.Was.Version || "?"} → ${p.Version}`;
  return h("div", {},
    h("div", { class: "prow" },
      h("div", { class: "who" }, h("strong", {}, p.Name), h("code", { title: sp.Pin.Source }, sp.Pin.Source)),
      h("span", { class: `chip ${sp.UpToDate ? "" : "ok"}` }, version),
      sp.Pin.Pin && h("span", { class: "chip", title: sp.Pin.Pin }, `at ${shortPin(sp.Pin.Pin)}`)),
    !sp.UpToDate && h("div", { class: "pldetail" }, brings(p),
      sp.Diff && h("details", { open: sp.DiffWhole ? null : true },
        h("summary", {}, sp.DiffWhole ? "Its plugin.toml (the old commit is no longer in the source)" : "Changes to plugin.toml"),
        h("pre", { class: "diff" }, sp.Diff))));
}

// brings lists a Plugin's Skills and MCP Servers.
function brings(p) {
  return [
    p.Description && h("p", {}, p.Description),
    h("h4", {}, "Skills"),
    p.Skills.length === 0 ? h("p", { class: "placeholder" }, "None.")
      : h("ul", { class: "plitems" }, p.Skills.map((s) => h("li", {}, h("code", {}, `${p.Name}/${s.Name}`), s.Description && h("span", {}, s.Description)))),
    h("h4", {}, "MCP Servers"),
    p.Servers.length === 0 ? h("p", { class: "placeholder" }, "None.")
      : h("ul", { class: "plitems" }, p.Servers.map((s) => h("li", {},
        h("code", {}, `${p.Name}/${s.Name}`),
        h("span", {}, "Runs ", h("code", { class: "cmd" }, s.CommandLine)),
        (s.Env ?? []).length > 0 && h("span", {}, "Asks for ", s.Env.flatMap((v, i) => [i > 0 && ", ", h("code", {}, `$${v}`)]))))),
  ];
}

function pluginCard(p) {
  const kind = p.BuiltIn ? ["chip", "Built in"] : p.Missing ? ["chip warn", "Missing"] : ["chip ok", "Installed"];
  const where = `remove:${p.Name}`;
  return h("div", { class: "provider" },
    h("div", { class: "prow" },
      h("div", { class: "who" }, h("strong", {}, p.Name),
        !p.BuiltIn && h("code", { title: p.Source }, `${p.Source}${p.Pin ? ` at ${shortPin(p.Pin)}` : ""}`)),
      p.Version && h("span", { class: "chip" }, p.Version),
      h("span", { class: kind[0] }, kind[1]),
      !p.BuiltIn && h("button", { class: "btn small", type: "button", disabled: !!busy || !!pv.Stage, onclick: () => act("fetch", "StagePluginUpdate", p.Name) }, "Update…"),
      !p.BuiltIn && removing !== p.Name && h("button", { class: "link", type: "button", disabled: !!busy, onclick: () => { removing = p.Name; draw(); } }, "Remove…")),
    p.Missing && h("p", { class: "notice inline" }, "Pinned on this device but not installed, as a restore leaves a Plugin it could not fetch. Update fetches it again."),
    h("div", { class: "pldetail" }, brings(p)),
    removing === p.Name && h("div", { class: "warning", role: "alert" },
      h("p", {}, `Remove ${p.Name} from this device? Its Skills and MCP Servers can no longer be granted.`),
      h("div", { class: "row" },
        h("button", { class: "btn danger", type: "button", disabled: !!busy, onclick: () => act(where, "RemovePlugin", p.Name) }, "Remove"),
        h("button", { class: "btn", type: "button", onclick: () => { removing = ""; draw(); } }, "Keep"))));
}
