// The Agents page. This Device: the Agent Profiles, a list and a form per
// profile with its grants and where it is included. Group: the
// edit-target Group's Providers. It renders the shell.AgentsView the Go
// side reports (main.UI.Agents) and redraws only on its own changes or a
// new edit-target Group, so a redraw of the app never drops what is
// being typed. UI text says "device" for a Node.
import { call, h, put } from "./dom.js";
import { secretField } from "./secretfield.js";

let av = null; // the AgentsView shown
let group = ""; // the Group it is for
let loading = "";
let open = ""; // the profile shown
let creating = false; // the new-profile form shows
let deleting = false; // Delete asks to confirm
let provOpen = ""; // the Provider whose form shows; NEW for a new one
let removing = ""; // the Provider whose Remove asks to confirm
let busy = false;
// What was typed in a form whose save was refused, to draw it again.
let profDraft = null;
let provDraft = null;
let profInputs = null; // the profile form's fields, while it shows
// The Secrets chosen for an MCP Server not granted yet, by its ref.
const pendingEnv = new Map();
// The parts' bodies, made once: a view loaded while the page is not on
// screen draws into them, ready for when it is.
const deviceMount = h("div", { class: "agents" });
const groupMount = h("div", { class: "agents" });
let pending = null; // the load under way: its promise

const NEW = "\u0000new";

// agentsDevicePart is This Device's part: the Agent Profiles. The app
// shows it only once agentsReady.
export function agentsDevicePart() {
  return deviceMount;
}

// agentsGroupPart is the Group part: the edit-target Group's Providers.
export function agentsGroupPart() {
  return groupMount;
}

// agentsReady: the view for s.EditGroup is loaded, so the page can show
// whole.
export const agentsReady = (s) => !!av && group === s.EditGroup;

// agentsLoad loads the view for s.EditGroup unless it is loaded or
// loading, and resolves once that load is done.
export function agentsLoad(s) {
  if (agentsReady(s)) return Promise.resolve();
  if (loading === s.EditGroup) return pending;
  pending = load(s.EditGroup);
  return pending;
}

// agentsRefresh reads the view again, as when the page is opened anew, and
// redraws only if it changed and no form is open: what is typed stays.
export async function agentsRefresh() {
  if (!av || loading || busy) return;
  const g = group;
  const next = await call("Agents", open);
  if (!next || next.Group !== g || g !== group || busy || creating || deleting || provOpen || removing) return;
  if (deviceMount.contains(document.activeElement) || groupMount.contains(document.activeElement)) return; // being typed in
  if (JSON.stringify(next) === JSON.stringify(av)) return;
  av = next;
  if (!av.Open) open = "";
  draw();
}

// agentsOpen opens the profile handle, as from Talk's "Profile" link.
export function agentsOpen(handle) {
  if (av && !loading) openProfile(handle);
  else open = handle; // the load under way, or the next, opens it
}

async function load(g) {
  loading = g;
  let next;
  try {
    next = await call("Agents", open);
  } finally {
    loading = "";
  }
  if (!next || next.Group !== g) return;
  if (g !== group) {
    // A Group not shown before: nothing of the one before carries over.
    provOpen = removing = "";
    provDraft = null;
  }
  group = g;
  av = next;
  if (!av.Open) open = "";
  draw();
}

// act calls method and shows the view it returns. Drafts typed are kept
// unless after, run on success, clears them.
async function act(method, args, after = () => {}) {
  if (busy) return;
  busy = true;
  draw();
  let next;
  try {
    next = await call(method, ...args);
  } finally {
    busy = false;
  }
  if (!next || next.Group !== group) {
    draw();
    return;
  }
  av = next;
  if (!next.Problem) after(next);
  // A new profile added though its SYSTEM.md was not written shows as made.
  if (next.Open && next.Where === "profile" && creating) { creating = false; open = next.Open.Handle; }
  if (!av.Open && !creating) open = "";
  draw();
  if (next.Problem && next.Field) {
    document.getElementById(fieldID(next.Where, next.Field))?.focus();
  }
}

function draw() {
  if (!av) return;
  drawDevice();
  drawGroup();
}

const fieldID = (where, field) => `${where.startsWith("provider") || where === "new-provider" ? "pv" : "ag"}-${field}`;

// problemAt is the refusal of the form where, for field ("" for the
// form's own), or null.
function problemAt(where, field) {
  if (!av.Problem || av.Where !== where) return null;
  if (field === "") return av.Field ? null : av.Problem;
  return av.Field === field ? av.Problem : null;
}

// row is a labelled form row with its hint, or the refusal of its field.
function row(where, field, label, input, hint) {
  const err = problemAt(where, field);
  if (err) input.setAttribute("aria-invalid", "true");
  input.id = fieldID(where, field);
  return h("div", { class: "frow" },
    h("label", { for: input.id }, label),
    h("div", { class: "fcell" }, input,
      err ? h("p", { class: "error", role: "alert" }, err.Message) : hint && h("p", { class: "hint" }, hint)));
}

const text = (value, attrs = {}) => h("input", { class: "field left", autocomplete: "off", spellcheck: "false", value: value ?? "", ...attrs });
const area = (value, attrs = {}) => h("textarea", { class: "field left area", spellcheck: "false", ...attrs }, value ?? "");

// ---- This Device: Agent Profiles ----

function drawDevice() {
  const list = h("nav", { class: "plist", "aria-label": "Agent Profiles" },
    (av.Profiles ?? []).map((p) => h("button", {
      class: "pitem", "aria-current": !creating && p.Handle === open ? "true" : false, disabled: busy,
      onclick: () => openProfile(p.Handle),
    }, h("strong", {}, `@${p.Handle}`), h("span", {}, [p.Provider, p.Model].filter(Boolean).join(" · ")))),
    h("button", { class: "pitem add", "aria-current": creating ? "true" : false, disabled: busy, onclick: () => {
      creating = true; deleting = false; profDraft = null; av.Problem = null; draw();
    } }, "+ New agent"));
  let detail;
  if (creating) detail = profileForm(null);
  else if (av.Open) detail = h("div", {}, profileForm(av.Open), grants(av.Open), included(av.Open), deleteBox(av.Open));
  else detail = h("p", { class: "placeholder" }, av.Profiles?.length ? "Choose an agent to edit it." : "No agents on this device yet. Add one to include it in talk.");
  put(deviceMount,
    h("p", { class: "hint lead" }, "Agents are this device's own: each runs here, and answers in the Groups and conversations that include it."),
    av.ProfilesProblem && h("p", { class: "error", role: "alert" }, av.ProfilesProblem.Message),
    av.Where === "delete" && av.Saved && h("p", { class: "saved", "aria-live": "polite" }, "Deleted."),
    h("div", { class: "pgrid" }, list, h("div", { class: "pdetail" }, detail)));
}

async function openProfile(handle) {
  creating = deleting = false;
  profDraft = null;
  pendingEnv.clear();
  open = handle;
  const next = await call("Agents", handle);
  if (next?.Group === group) av = next;
  draw();
}

function profileForm(o) {
  const where = o ? "profile" : "new-profile";
  const d = profDraft?.New === !o ? profDraft : o
    ? { Handle: o.Handle, Provider: o.Provider, Model: o.Model, MaxTurns: o.MaxTurns, Body: o.Body, System: o.System }
    : { Handle: "", Provider: "", Model: "", MaxTurns: "", Body: "", System: "" };
  const inputs = {
    Handle: text(d.Handle, { placeholder: "auditor" }),
    Provider: providerSelect(d.Provider, !o),
    Model: text(d.Model, { placeholder: "claude-opus-5-5" }),
    MaxTurns: text(d.MaxTurns, { inputmode: "numeric", placeholder: "Default" }),
    Body: area(d.Body, { rows: 4, placeholder: '{"max_tokens": 4096}' }),
    System: area(d.System, { rows: 10, class: "field left area mono", placeholder: o ? "" : "A placeholder prompt is written if this is left empty." }),
  };
  profInputs = o ? inputs : null;
  const saved = h("span", { class: "saved", "aria-live": "polite" }, av.Where === where && av.Saved ? "Saved." : "");
  for (const el of Object.values(inputs)) el.addEventListener("input", () => { saved.textContent = ""; });
  const submit = h("button", { class: "btn primary", type: "submit", disabled: busy }, o ? "Save" : "Add agent");
  const providers = av.Providers ?? [];
  return h("form", {
    class: "gform",
    onsubmit: (e) => {
      e.preventDefault();
      const draft = { New: !o, Handle: o ? o.Handle : inputs.Handle.value.trim() };
      for (const k of ["Provider", "Model", "MaxTurns"]) draft[k] = inputs[k].value.trim();
      draft.Body = inputs.Body.value;
      draft.System = inputs.System.value;
      profDraft = draft;
      act("SaveAgentProfile", [draft], () => { open = draft.Handle; creating = false; profDraft = null; });
    },
  },
  h("h3", {}, o ? `@${o.Handle}` : "New agent"),
  !o && row(where, "handle", "Handle", inputs.Handle, "How talk mentions it, as @handle: lowercase letters and digits, joined by - or _."),
  row(where, "provider", "Provider", inputs.Provider, providers.length
    ? `A Provider of ${group}. In each Group it answers in, it calls the Provider by this name there.`
    : `${group} has no Providers yet: add one under Group below.`),
  row(where, "model", "Model ID", inputs.Model, "The model its Provider runs, sent as the body's model."),
  row(where, "max_turns", "Max turns", inputs.MaxTurns, o?.MaxTurns
    ? "How many turns one run may take. Once set, it can be changed but not cleared."
    : "How many turns one run may take. Leave empty for the default."),
  row(where, "body", "Request body", inputs.Body, "Other request options, as a JSON object. Leave empty for none."),
  row(where, "system", "SYSTEM.md", inputs.System, "Its system prompt. Each run reads it afresh, so a change applies to the next."),
  problemAt(where, "") && h("p", { class: "error", role: "alert" }, av.Problem.Message),
  h("div", { class: "factions" }, saved,
    !o && h("button", { class: "btn", type: "button", onclick: () => { creating = false; profDraft = null; av.Problem = null; draw(); } }, "Cancel"),
    submit));
}

function providerSelect(value, isNew) {
  const aliases = (av.Providers ?? []).map((p) => p.Alias);
  return h("select", { class: "field left" },
    isNew && h("option", { value: "", selected: value === "" }, "Choose a Provider"),
    aliases.map((a) => h("option", { value: a, selected: a === value }, a)),
    value && !aliases.includes(value) && h("option", { value, selected: true }, `${value} (not in ${group})`));
}

// grant writes a grant or revoke at once, as the CLI's verbs do, keeping
// what is typed in the profile form above.
function grant(method, args) {
  if (profInputs) {
    const d = { New: false, Handle: open };
    for (const [k, el] of Object.entries(profInputs)) d[k] = k === "Body" || k === "System" ? el.value : el.value.trim();
    profDraft = d;
  }
  act(method, args);
}

// grants are a profile's Tools, Skills and MCP Servers.
function grants(o) {
  const cat = av.Catalog ?? {};
  const union = (listed, granted) => [...new Set([...(listed ?? []), ...(granted ?? [])])].sort();
  const problem = (ref) => problemAt(`grant:${ref}`, "");
  const toggle = (ref, granted, label, desc) => h("div", { class: "grant" },
    h("label", { class: "check" },
      h("input", { type: "checkbox", checked: granted, disabled: busy,
        onchange: (e) => grant(e.target.checked ? "GrantAgent" : "RevokeAgent", e.target.checked ? [o.Handle, ref, []] : [o.Handle, ref]) }),
      h("span", {}, h("code", {}, label), desc && h("span", { class: "hint" }, desc))),
    problem(ref) && h("p", { class: "error", role: "alert" }, problem(ref).Message));

  const tools = union(cat.Tools, o.Tools).map((t) => toggle(t, o.Tools.includes(t), t));
  const skillDesc = Object.fromEntries((cat.Skills ?? []).map((s) => [s.Ref, s.Description]));
  const skills = union((cat.Skills ?? []).map((s) => s.Ref), o.Skills).map((ref) => toggle(ref, o.Skills.includes(ref), ref, skillDesc[ref] ?? "Its Plugin is not on this device."));
  const servers = new Map((cat.Servers ?? []).map((s) => [s.Ref, s]));
  for (const ref of Object.keys(o.MCP ?? {})) if (!servers.has(ref)) servers.set(ref, { Ref: ref, Env: o.MCP[ref], CommandLine: "" });
  const mcp = [...servers.values()].sort((a, b) => a.Ref.localeCompare(b.Ref)).map((s) => server(o, s, problem(s.Ref)));

  return h("section", { class: "gsec", "aria-label": "Grants" },
    h("h3", {}, "Grants"),
    h("p", { class: "hint" }, "What it may use. A change applies at once, from its next run."),
    h("h4", {}, "Tools"), h("div", { class: "grants" }, tools),
    h("h4", {}, "Skills"), skills.length ? h("div", { class: "grants" }, skills) : h("p", { class: "placeholder" }, "No Plugin on this device brings a Skill."),
    h("h4", {}, "MCP Servers"), mcp.length ? h("div", { class: "grants" }, mcp) : h("p", { class: "placeholder" }, "No Plugin on this device brings an MCP Server."));
}

// server is an MCP Server's grant: on or off, and which of the variables
// it asks for it is given, each the Secret of that name in this Group.
function server(o, s, err) {
  const granted = Object.hasOwn(o.MCP ?? {}, s.Ref);
  if (!granted && !pendingEnv.has(s.Ref)) pendingEnv.set(s.Ref, new Set(s.Env));
  const given = granted ? new Set(o.MCP[s.Ref]) : pendingEnv.get(s.Ref);
  const regrant = () => grant("GrantAgent", [o.Handle, s.Ref, s.Env.filter((n) => given.has(n))]);
  return h("div", { class: "grant server" },
    h("label", { class: "check" },
      h("input", { type: "checkbox", checked: granted, disabled: busy,
        onchange: (e) => {
          if (e.target.checked) { regrant(); return; }
          pendingEnv.set(s.Ref, new Set(o.MCP[s.Ref]));
          grant("RevokeAgent", [o.Handle, s.Ref]);
        } }),
      h("span", {}, h("code", {}, s.Ref), s.CommandLine && h("span", { class: "hint" }, `Runs ${s.CommandLine}`))),
    s.Env.length > 0 && h("div", { class: "envs" },
      h("p", { class: "hint" }, granted ? "Secrets it gets, from this Group:" : "Secrets it will get once granted, from this Group:"),
      s.Env.map((name) => h("div", { class: "env" },
        h("input", { type: "checkbox", checked: given.has(name), disabled: busy, "aria-label": `Give it $${name}`,
          onchange: (e) => {
            if (e.target.checked) given.add(name); else given.delete(name);
            if (granted) regrant();
          } }),
        secretField({ group, fixed: name }).el))),
    err && h("p", { class: "error", role: "alert" }, err.Message));
}

function included(o) {
  const places = o.IncludedIn ?? [];
  return h("section", { class: "gsec", "aria-label": "Included in" },
    h("h3", {}, "Included in"),
    places.length
      ? h("ul", { class: "places" }, places.map((p) => h("li", {}, h("strong", {}, p.Group), ` — ${p.Where}`)))
      : h("p", { class: "placeholder" }, "Nowhere yet. Include it in a conversation from Talk."));
}

function deleteBox(o) {
  const places = o.IncludedIn ?? [];
  const err = problemAt("delete", "");
  if (!deleting) {
    return h("section", { class: "gsec" },
      h("button", { class: "btn", disabled: busy, onclick: () => { deleting = true; av.Problem = null; draw(); } }, `Delete @${o.Handle}…`),
      err && h("p", { class: "error", role: "alert" }, err.Message));
  }
  return h("section", { class: "gsec" },
    h("div", { class: "warning", role: "alert" },
      places.length
        ? [h("p", {}, `@${o.Handle} is included in:`),
          h("ul", {}, places.map((p) => h("li", {}, `${p.Group} — ${p.Where}`))),
          h("p", {}, "Exclude it there first, on Talk. Then it can be deleted.")]
        : h("p", {}, `Delete @${o.Handle}? Its config and SYSTEM.md are removed from this device.`),
      h("div", { class: "row" },
        h("button", { class: "btn danger", disabled: busy || places.length > 0,
          onclick: () => act("DeleteAgentProfile", [o.Handle], () => { open = ""; deleting = false; profDraft = null; }) }, "Delete"),
        h("button", { class: "btn", onclick: () => { deleting = false; draw(); } }, "Cancel"))),
    err && h("p", { class: "error", role: "alert" }, err.Message));
}

// ---- Group: Providers ----

function drawGroup() {
  const list = av.Providers ?? [];
  put(groupMount,
    h("h3", {}, "Providers"),
    h("p", { class: "hint lead" }, `The LLM APIs agents call in ${group}. They are this device's own config, never shared with the Group.`),
    av.ProvidersProblem && h("p", { class: "error", role: "alert" }, av.ProvidersProblem.Message),
    av.Where === "provider:" + removing && av.Saved && h("p", { class: "saved", "aria-live": "polite" }, "Removed."),
    h("div", { class: "providers" },
      list.length === 0 && provOpen !== NEW && h("p", { class: "placeholder" }, `${group} has no Providers yet.`),
      list.map((p) => providerRow(p)),
      provOpen === NEW
        ? providerForm(null)
        : h("button", { class: "btn", disabled: busy, onclick: () => { provOpen = NEW; provDraft = null; av.Problem = null; draw(); } }, "+ Add Provider")));
}

function providerRow(p) {
  const head = h("div", { class: "prow" },
    h("div", { class: "who" }, h("strong", {}, p.Alias), h("code", { title: p.URL }, p.URL)),
    h("span", { class: "chip" }, p.Shape),
    h("span", { class: `chip ${p.APIKey ? "ok" : ""}` }, p.APIKey ? `Key: ${p.APIKey}` : "No API key"),
    provOpen !== p.Alias && h("button", { class: "link", disabled: busy, onclick: () => { provOpen = p.Alias; provDraft = null; removing = ""; av.Problem = null; draw(); } }, "Edit"));
  return h("div", { class: "provider" }, head, provOpen === p.Alias && providerForm(p));
}

function providerForm(p) {
  const where = p ? `provider:${p.Alias}` : "new-provider";
  const d = provDraft?.New === !p ? provDraft : p
    ? { Alias: p.Alias, Preset: "", BaseURL: p.BaseURL, APIPath: p.APIPath, APIKey: p.APIKey, Body: p.Body }
    : { Alias: "", Preset: "", BaseURL: "", APIPath: "", APIKey: "", Body: "" };
  const alias = text(d.Alias, { placeholder: "claude" });
  const preset = h("select", { class: "field left" },
    h("option", { value: "", selected: !d.Preset }, "Blank: give its URL and API path"),
    (av.Presets ?? []).map((n) => h("option", { value: n, selected: n === d.Preset }, n)));
  const baseURL = text(d.BaseURL, { type: "url", placeholder: "https://api.example.com" });
  const apiPath = text(d.APIPath, { placeholder: "/v1/messages" });
  const fromPreset = () => {
    const on = !!preset.value;
    baseURL.placeholder = on ? "From the preset" : "https://api.example.com";
    apiPath.placeholder = on ? "From the preset" : "/v1/messages";
    if (on && !alias.value) alias.value = preset.value;
  };
  preset.addEventListener("change", fromPreset);
  if (d.Preset) fromPreset();
  const key = secretField({ group, id: `pv-api_key`, value: d.APIKey, noneLabel: "No API key" });
  const body = area(d.Body, { rows: 3, placeholder: '{"max_tokens": 4096}' });
  const keyRow = row(where, "api_key", "API key", h("div", {}), "A Secret of this Group, sent only to this Provider's base URL.");
  keyRow.querySelector(".fcell").firstChild.replaceWith(key.el);
  const saved = h("span", { class: "saved", "aria-live": "polite" }, av.Where === where && av.Saved ? "Saved." : "");
  return h("form", {
    class: p ? "gform pform" : "gform pform new",
    onsubmit: (e) => {
      e.preventDefault();
      const draft = { New: !p, Alias: p ? p.Alias : alias.value.trim(), Preset: p ? "" : preset.value,
        BaseURL: baseURL.value.trim(), APIPath: apiPath.value.trim(), APIKey: key.value, Body: body.value };
      provDraft = draft;
      act("SaveProvider", [draft], () => { provOpen = ""; provDraft = null; });
    },
  },
  !p && h("h3", {}, "New Provider"),
  !p && row(where, "alias", "Name", alias, "What agents name it by: lowercase letters, digits, - and _."),
  !p && row(where, "preset", "Start from", preset, "A preset fills the URL and API path; anything typed below overrides it."),
  row(where, "base_url", "Base URL", baseURL),
  row(where, "api_path", "API path", apiPath, "Its suffix picks the API: /messages, /chat/completions or /responses."),
  keyRow,
  row(where, "body", "Request body", body, "Options every agent on it sends, as a JSON object. Leave empty for none."),
  problemAt(where, "") && h("p", { class: "error", role: "alert" }, av.Problem.Message),
  h("div", { class: "factions" }, saved,
    p && (removing === p.Alias
      ? h("span", { class: "confirm" }, `Remove ${p.Alias}? Agents on it stop working in ${group}.`,
        h("button", { class: "btn danger", type: "button", disabled: busy, onclick: () => act("RemoveProvider", [p.Alias], () => { provOpen = ""; provDraft = null; }) }, "Remove"),
        h("button", { class: "btn", type: "button", onclick: () => { removing = ""; draw(); } }, "Keep"))
      : h("button", { class: "btn", type: "button", disabled: busy, onclick: () => { removing = p.Alias; draw(); } }, "Remove…")),
    h("button", { class: "btn", type: "button", onclick: () => { provOpen = removing = ""; provDraft = null; av.Problem = null; draw(); } }, "Cancel"),
    h("button", { class: "btn primary", type: "submit", disabled: busy }, p ? "Save" : "Add Provider")));
}
