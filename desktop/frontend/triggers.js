// The Triggers page, the edit-target Group's only: its Jobs, and the Hooks
// and Schedules that run them, as forms, with Run now on a Job of the
// served Group and "Edit TOML" over its config.toml for what a form can't
// change. It renders the shell.TriggersView the Go side reports
// (main.UI.Triggers) and redraws only on its own changes or a new
// edit-target Group, so a redraw of the app never drops what is typed. UI
// text says "device" for a Node.
import { call, h, put } from "./dom.js";
import { secretField } from "./secretfield.js";

let tv = null; // the TriggersView shown
let group = ""; // the Group it is for
let loading = "";
let pending = null;
let busy = false;
// The form open: "job:<name>", "new-job", "hook:<name>", "new-hook",
// "schedule:<name>" or "new-schedule"; "" for none. draft is what was
// typed in it, to draw it again; read reads the form shown.
let open = "";
let draft = null;
let read = null;
let deleting = ""; // the form whose Delete asks to confirm
let done = ""; // what the last change did, as "Deleted." for the list
const running = new Set(); // the Jobs Run now is running
const ran = new Map(); // a Job's last Run now: { ok, text }
// Edit TOML: shown, its text, why the save was refused, and the table to
// find in it once shown.
let toml = null; // { text, problem, table }
const mount = h("div", { class: "agents triggers" });

// Choices that stand for a value typed beside them.
const WORKSPACE = "\u0000ws";
const NODE_IDS = "\u0000ids";
const KINDS = { agent: "Agent", exec: "Exec", webhook: "Webhook" };

export const triggersGroupPart = () => mount;
// triggersReady: the view for s.EditGroup is loaded, as served or not.
export const triggersReady = (s) => !!tv && group === s.EditGroup && tv.Served === s.EditGroupServed;

export function triggersLoad(s) {
  if (triggersReady(s)) return Promise.resolve();
  if (loading === s.EditGroup) return pending;
  pending = load(s.EditGroup);
  return pending;
}

// triggersRefresh reads the view again, as when the page is opened anew,
// and redraws only if it changed and nothing is being edited.
export async function triggersRefresh() {
  if (!tv || loading || busy || open || toml || deleting) return;
  const g = group;
  const next = await call("Triggers");
  if (!next || next.Group !== g || g !== group || busy || open || toml || deleting) return;
  if (mount.contains(document.activeElement)) return;
  if (JSON.stringify(next) === JSON.stringify(tv)) return;
  tv = next;
  draw();
}

async function load(g) {
  loading = g;
  let next;
  try {
    next = await call("Triggers");
  } finally {
    loading = "";
  }
  if (!next || next.Group !== g) return;
  if (g !== group || next.Served !== tv?.Served) {
    open = deleting = done = "";
    draft = toml = null;
    running.clear();
    ran.clear();
  }
  group = g;
  tv = next;
  draw();
}

// act calls method; on success the form closes and done says what it did.
async function act(method, args, did = "") {
  if (busy) return;
  if (read) draft = read();
  busy = true;
  draw();
  let next;
  try {
    next = await call(method, ...args);
  } finally {
    busy = false;
  }
  if (next?.Group === group) {
    tv = next;
    if (!next.Problem) {
      open = deleting = "";
      draft = null;
      done = did;
    }
  }
  draw();
  if (next?.Problem && next.Field) document.getElementById(`tg-${next.Field}`)?.focus();
}

function show(form) {
  open = form;
  draft = null;
  deleting = done = "";
  if (tv) tv.Problem = null;
  draw();
}

// problemAt is the refusal of the form at where, for field ("" for the
// form's own), or null.
function problemAt(where, field) {
  if (!tv.Problem || tv.Where !== where) return null;
  if (field === "") return tv.Field ? null : tv.Problem;
  return tv.Field === field ? tv.Problem : null;
}

function row(where, field, label, input, hint) {
  const err = problemAt(where, field);
  if (err) input.setAttribute("aria-invalid", "true");
  if (!input.id) input.id = `tg-${field}`;
  return h("div", { class: "frow" },
    h("label", { for: input.id }, label),
    h("div", { class: "fcell" }, input,
      err ? h("p", { class: "error", role: "alert" }, err.Message) : hint && h("p", { class: "hint" }, hint)));
}

const text = (value, attrs = {}) => h("input", { class: "field left", autocomplete: "off", spellcheck: "false", value: value ?? "", ...attrs });
const area = (value, attrs = {}) => h("textarea", { class: "field left area", spellcheck: "false", ...attrs }, value ?? "");
const lines = (s) => s.split("\n").map((l) => l.trim()).filter(Boolean);
const list = (s) => s.split(",").map((x) => x.trim()).filter(Boolean);

function draw() {
  if (!tv) return;
  if (toml) {
    put(mount, tomlEditor());
    return;
  }
  read = null;
  put(mount,
    h("div", { class: "trighead" },
      h("p", { class: "hint lead" }, `What this device does on its own in ${group}: Jobs are what it runs; Hooks run them on events in talk, Schedules at set times. They are this device's config, never shared with the Group.`),
      h("button", { class: "btn", type: "button", disabled: busy, onclick: () => openTOML("") }, "Edit TOML")),
    tv.ListProblem && h("p", { class: "error", role: "alert" }, tv.ListProblem.Message, " ",
      h("button", { class: "link", type: "button", onclick: () => openTOML("") }, "Fix it in Edit TOML")),
    done && h("p", { class: "saved", "aria-live": "polite" }, done),
    !tv.ListProblem && [jobsSection(), hooksSection(), schedulesSection()]);
}

// section is one list of Triggers or Jobs, its rows and its Add form.
function section(title, hint, kind, items, rowOf, formOf, addLabel) {
  return h("section", { class: "tsec", "aria-label": title },
    h("h3", {}, title),
    h("p", { class: "hint" }, hint),
    h("div", { class: "providers" },
      items.length === 0 && open !== `new-${kind}` && h("p", { class: "placeholder" }, `No ${title} yet.`),
      items.map((it) => h("div", { class: "provider" }, rowOf(it), open === `${kind}:${it.Name}` && formOf(it))),
      open === `new-${kind}`
        ? formOf(null)
        : h("button", { class: "btn", type: "button", disabled: busy, onclick: () => show(`new-${kind}`) }, addLabel)));
}

const editButton = (key) => open !== key && h("button", { class: "link", type: "button", disabled: busy, onclick: () => show(key) }, "Edit");

// ---- Jobs ----

function jobsSection() {
  const el = section("Jobs", "What a Hook or Schedule runs: summon an agent of this device, run a program, or call a webhook.",
    "job", tv.Jobs ?? [], jobRow, jobForm, "+ Add Job");
  if (!tv.Served) el.querySelector(".hint").after(h("p", { class: "notice inline" }, `Run now needs ${group} online on this device: switch to it from the Group menu.`));
  return el;
}

function jobSummary(j) {
  switch (j.Kind) {
    case "agent": return `@${j.Agent}${j.Target === "group" ? " in Group talk" : j.Target ? ` in ${j.Target}` : ""}`;
    case "exec": return j.Exec.join(" ");
    case "webhook": return j.WebhookSecret ? `$${j.WebhookSecret}` : "a URL";
  }
  return "";
}

function jobRow(j) {
  const isRunning = running.has(j.Name);
  const last = ran.get(j.Name);
  return h("div", {},
    h("div", { class: "prow" },
      h("div", { class: "who" }, h("strong", {}, j.Name), h("code", { title: jobSummary(j) }, jobSummary(j))),
      h("span", { class: "chip" }, KINDS[j.Kind] ?? j.Kind),
      h("span", { class: `chip ${j.UsedBy.length ? "ok" : ""}` }, j.UsedBy.length ? `Run by ${j.UsedBy.join(", ")}` : "No Trigger runs it"),
      h("button", {
        class: "btn small", type: "button", disabled: !tv.Served || isRunning,
        title: tv.Served ? "Run it now, as no Trigger. Its outcome also shows in Talk." : `Run now needs ${group} online on this device.`,
        onclick: () => runNow(j.Name),
      }, isRunning ? "Running…" : "Run now"),
      editButton(`job:${j.Name}`)),
    (last || problemAt(`run:${j.Name}`, "")) && h("p", { class: last?.ok ? "saved ranline" : "error ranline", role: "status" },
      problemAt(`run:${j.Name}`, "")?.Message ?? last.text));
}

async function runNow(name) {
  if (running.has(name)) return;
  running.add(name);
  ran.delete(name);
  draw();
  let next;
  try {
    next = await call("RunJob", name);
  } finally {
    running.delete(name);
  }
  if (next?.Group === group) {
    if (next.Ran) ran.set(name, { ok: next.Ran.OK, text: next.Ran.OK ? "Finished. It shows in Talk too." : `Failed: ${next.Ran.Error}` });
    // The rest of the view is left as shown, so an open form keeps what is typed.
    if (next.Problem) { tv.Problem = next.Problem; tv.Where = next.Where; tv.Field = ""; }
  }
  draw();
}

function jobForm(j) {
  const where = j ? `job:${j.Name}` : "new-job";
  const d = draft ?? (j
    ? { ...j, Headers: j.Headers.map((x) => ({ ...x })), Exec: [...j.Exec], Secrets: [...j.Secrets] }
    : { Name: "", Kind: "exec", Agent: "", Prompt: "", Target: "", Exec: [], Secrets: [], Webhook: "", WebhookSecret: "", Headers: [], Timeout: "" });
  const name = text(d.Name, { placeholder: "notify_me" });
  const kind = h("select", { class: "field left" }, Object.entries(KINDS).map(([k, label]) => h("option", { value: k, selected: k === d.Kind }, label)));
  const timeout = text(d.Timeout, { placeholder: d.Kind === "exec" ? "60s" : "10s" });
  let fields = [];
  let readKind = () => ({});

  if (d.Kind === "agent") {
    const handles = tv.Handles ?? [];
    const agent = h("select", { class: "field left" },
      h("option", { value: "", selected: !d.Agent }, handles.length ? "Choose an agent" : "No agents on this device yet"),
      handles.map((x) => h("option", { value: x, selected: x === d.Agent }, `@${x}`)),
      d.Agent && !handles.includes(d.Agent) && h("option", { value: d.Agent, selected: true }, `@${d.Agent} (not on this device)`));
    const prompt = area(d.Prompt, { rows: 3, placeholder: "Review the Sign Request and accept it if it matches policy." });
    const known = d.Target === "" || d.Target === "group";
    const target = h("select", { class: "field left" },
      h("option", { value: "", selected: d.Target === "" }, "The event's conversation"),
      h("option", { value: "group", selected: d.Target === "group" }, "Group talk"),
      h("option", { value: WORKSPACE, selected: !known }, "A Key's conversation, by its Workspace name…"));
    const workspace = text(known ? "" : d.Target, { placeholder: "ecdsa-02ab34cd", "aria-label": "Workspace name" });
    workspace.hidden = known;
    target.addEventListener("change", () => { workspace.hidden = target.value !== WORKSPACE; });
    fields = [
      row(where, "agent", "Agent", agent, "It runs here, on this device."),
      row(where, "prompt", "Prompt", prompt, "Its instruction, with the event that fired."),
      row(where, "target", "Answers in", h("div", { class: "stack" }, target, workspace),
        "A Schedule has no event, so a Job it runs must name Group talk or a Workspace. In Group talk the agent must be included there."),
    ];
    readKind = () => ({ Agent: agent.value, Prompt: prompt.value, Target: target.value === WORKSPACE ? workspace.value.trim() : target.value });
  } else if (d.Kind === "exec") {
    const exec = area(d.Exec.join("\n"), { rows: 3, placeholder: "defios\nsign\naccept\n$DEFIOS_SIGN_REQUEST_ID" });
    const secrets = d.Secrets.map((n) => secretField({ group, value: n, noneLabel: "Choose a Secret" }));
    const addSecret = h("button", { class: "link", type: "button", onclick: () => { draft = readAll(); draft.Secrets.push(""); draw(); } }, "+ Add a Secret");
    fields = [
      row(where, "exec", "Program and arguments", exec, "One per line, the program first, run with no shell. $DEFIOS_* variables are filled in from the event."),
      row(where, "secrets", "Secrets", h("div", { class: "stack" }, secrets.map((f) => f.el), addSecret), "Set in its environment, each under its own name."),
      row(where, "timeout", "Timeout", timeout, "How long it may run. Leave empty for 60s."),
    ];
    readKind = () => ({ Exec: lines(exec.value), Secrets: secrets.map((f) => f.value).filter(Boolean), Timeout: timeout.value.trim() });
  } else {
    const url = text(d.Webhook, { type: "url", placeholder: "https://ntfy.example/topic" });
    const fromSecret = secretField({ group, value: d.WebhookSecret, noneLabel: "None: call the URL above" });
    const headers = d.Headers.map((x) => {
      const hn = text(x.Name, { placeholder: "Authorization", "aria-label": "Header name" });
      const hv = text(x.Value, { placeholder: "Value", "aria-label": "Header value" });
      const hs = secretField({ group, value: x.Secret, noneLabel: "The value typed", onChange: (n) => { hv.hidden = !!n; } });
      hv.hidden = !!x.Secret;
      return { el: h("div", { class: "header" }, hn, hv, hs.el), read: () => ({ Name: hn.value.trim(), Value: hv.value, Secret: hs.value }) };
    });
    const addHeader = h("button", { class: "link", type: "button", onclick: () => { draft = readAll(); draft.Headers.push({ Name: "", Value: "", Secret: "" }); draw(); } }, "+ Add a header");
    fields = [
      row(where, "webhook", "URL", url, "An http or https URL it POSTs the event to."),
      row(where, "webhook_secret", "Or from a Secret", fromSecret.el, "A URL that carries a token is better kept as a Secret; set, it is used instead."),
      row(where, "headers", "Headers", h("div", { class: "stack" }, headers.map((x) => x.el), addHeader), "Each a value typed here, or a Secret's."),
      row(where, "timeout", "Timeout", timeout, "How long each attempt may take. Leave empty for 10s."),
    ];
    readKind = () => ({ Webhook: url.value.trim(), WebhookSecret: fromSecret.value, Headers: headers.map((x) => x.read()).filter((x) => x.Name), Timeout: timeout.value.trim() });
  }

  const readAll = () => ({ ...d, Name: j ? j.Name : name.value.trim(), Kind: kind.value, ...readKind() });
  read = readAll;
  kind.addEventListener("change", () => { const next = readAll(); next.Kind = kind.value; draft = next; draw(); });
  const used = j?.UsedBy ?? [];
  return h("form", {
    class: j ? "gform pform" : "gform pform new",
    onsubmit: (e) => {
      e.preventDefault();
      act("SaveJob", [{ New: !j, ...readAll() }]);
    },
  },
  !j && h("h3", {}, "New Job"),
  !j && row(where, "name", "Name", name, "What Hooks and Schedules name it by: lowercase letters, digits, - and _."),
  row(where, "kind", "Kind", kind),
  fields,
  formActions(where, j, "DeleteJob", "Add Job",
    used.length ? `${j?.Name} is run by ${used.join(" and ")}. Take it out of them first, then it can be deleted.` : null));
}

// formActions are a form's refusal, Delete with its confirmation, Cancel
// and Save. blocked, when set, says why Delete can't go ahead.
function formActions(where, item, deleteMethod, addLabel, blocked = null) {
  const err = problemAt(where, "");
  return [
    err && h("div", { class: "error", role: "alert" }, err.Message,
      err.Code === "not-in-place" && h("div", {}, h("button", { class: "btn small", type: "button", onclick: () => openTOML(tv.Table) }, "Edit TOML"))),
    deleting === where && h("div", { class: "warning", role: "alert" },
      h("p", {}, blocked ?? `Delete ${item.Name}? It is removed from this device's config.toml for ${group}.`),
      h("div", { class: "row" },
        h("button", { class: "btn danger", type: "button", disabled: busy || !!blocked, onclick: () => act(deleteMethod, [item.Name], `Deleted ${item.Name}.`) }, "Delete"),
        h("button", { class: "btn", type: "button", onclick: () => { deleting = ""; draw(); } }, "Keep"))),
    h("div", { class: "factions" },
      item && deleting !== where && h("button", { class: "btn", type: "button", disabled: busy, onclick: () => { if (read) draft = read(); deleting = where; draw(); } }, "Delete…"),
      h("button", { class: "btn", type: "button", onclick: () => { open = deleting = ""; draft = null; tv.Problem = null; draw(); } }, "Cancel"),
      h("button", { class: "btn primary", type: "submit", disabled: busy }, item ? "Save" : addLabel)),
  ];
}

// jobsPicker chooses the Jobs a Trigger runs, in order, from the Group's.
function jobsPicker(chosen, redraw) {
  const names = (tv.Jobs ?? []).map((j) => j.Name);
  const picks = chosen.length ? [...chosen] : [""];
  const selects = picks.map((value, i) => {
    const sel = h("select", { class: "field left", "aria-label": `Job ${i + 1}` },
      h("option", { value: "", selected: !value }, names.length ? "Choose a Job" : "No Jobs yet: add one above"),
      names.map((n) => h("option", { value: n, selected: n === value }, n)),
      value && !names.includes(value) && h("option", { value, selected: true }, `${value} (not defined)`));
    const remove = picks.length > 1 && h("button", { class: "link", type: "button", "aria-label": `Remove Job ${i + 1}`, onclick: () => { const v = picked(); v.splice(i, 1); redraw(v); } }, "Remove");
    return { sel, el: h("div", { class: "jobpick" }, h("span", { class: "hint" }, `${i + 1}.`), sel, remove) };
  });
  const picked = () => selects.map((s) => s.sel.value);
  const add = h("button", { class: "link", type: "button", onclick: () => redraw([...picked(), ""]) }, "+ Then another Job");
  return { el: h("div", { class: "stack" }, selects.map((s) => s.el), add), value: () => picked().filter(Boolean) };
}

// ---- Hooks ----

function hooksSection() {
  return section("Hooks", "Fire on an event in talk that matches every filter set, running their Jobs in order until one fails.",
    "hook", tv.Hooks ?? [], hookRow, hookForm, "+ Add Hook");
}

function hookRow(hk) {
  const on = hk.Events.length ? hk.Events.join(", ") : "every event";
  return h("div", { class: "prow" },
    h("div", { class: "who" }, h("strong", {}, hk.Name), h("code", { title: on }, `on ${on}`)),
    h("span", { class: "chip" }, `Runs ${hk.Jobs.join(", ")}`),
    editButton(`hook:${hk.Name}`));
}

const FROM = { "": "Anyone", self: "This device", others: "Other devices" };

function hookForm(hk) {
  const where = hk ? `hook:${hk.Name}` : "new-hook";
  const d = draft ?? (hk ? { ...hk } : { Name: "", Events: ["sign.request"], From: [], Handles: [], Conversation: [], Text: "", Jobs: [] });
  const name = text(d.Name, { placeholder: "on_sign_request" });
  const chosen = new Set(d.Events);
  const events = h("div", { class: "events" }, (tv.Events ?? []).map((ev) => h("label", { class: "check" },
    h("input", { type: "checkbox", checked: chosen.has(ev), onchange: (e) => { if (e.target.checked) chosen.add(ev); else chosen.delete(ev); } }),
    h("code", {}, ev))));
  const fromKey = d.From.length === 0 ? "" : d.From.length === 1 && FROM[d.From[0]] ? d.From[0] : NODE_IDS;
  const from = h("select", { class: "field left" },
    Object.entries(FROM).map(([k, label]) => h("option", { value: k, selected: k === fromKey }, label)),
    fromKey === NODE_IDS && h("option", { value: NODE_IDS, selected: true }, `These devices: ${d.From.join(", ")}`));
  const handles = text(d.Handles.join(", "), { placeholder: "user, auditor" });
  const conv = text(d.Conversation.join(", "), { placeholder: "group" });
  const re = text(d.Text, { placeholder: "^approve" });
  const readAll = () => ({
    ...d, Name: hk ? hk.Name : name.value.trim(), Events: (tv.Events ?? []).filter((ev) => chosen.has(ev)),
    From: from.value === "" ? [] : from.value === NODE_IDS ? d.From : [from.value],
    Handles: list(handles.value), Conversation: list(conv.value), Text: re.value, Jobs: jobs.value(),
  });
  const jobs = jobsPicker(d.Jobs, (next) => { draft = { ...readAll(), Jobs: next }; draw(); });
  read = readAll;
  return h("form", {
    class: hk ? "gform pform" : "gform pform new",
    onsubmit: (e) => { e.preventDefault(); act("SaveHook", [{ New: !hk, ...readAll() }]); },
  },
  !hk && h("h3", {}, "New Hook"),
  !hk && row(where, "name", "Name", name, "Lowercase letters, digits, - and _."),
  row(where, "events", "Fires on", events, "None chosen fires on every event. request.<status> is a Sign Request reaching that status."),
  row(where, "from", "Written by", from),
  row(where, "handles", "Handles", handles, "Agents' Handles, or user for a person, separated by commas. Empty matches all."),
  row(where, "conversation", "In", conv, "group for Group talk, or Workspace names, separated by commas. Empty matches all."),
  row(where, "text", "Text matches", re, "A regular expression a message's text must match. Empty matches all."),
  row(where, "jobs", "Runs", jobs.el, "In order, until one fails."),
  formActions(where, hk, "DeleteHook", "Add Hook"));
}

// ---- Schedules ----

function schedulesSection() {
  return section("Schedules", "Fire at set times while this device serves the Group; a time missed while it doesn't is skipped.",
    "schedule", tv.Schedules ?? [], scheduleRow, scheduleForm, "+ Add Schedule");
}

const when = (s) => (s.At ? `once at ${s.At}` : `cron ${s.Cron}${s.TZ ? ` (${s.TZ})` : ""}`);

function scheduleRow(s) {
  const next = s.Next ? new Date(s.Next) : null;
  return h("div", { class: "prow" },
    h("div", { class: "who" }, h("strong", {}, s.Name), h("code", {}, when(s))),
    h("span", { class: `chip ${next ? "ok" : ""}`, title: next ? next.toString() : "" }, next ? `Next: ${next.toLocaleString()}` : "Fires no more"),
    h("span", { class: "chip" }, `Runs ${s.Jobs.join(", ")}`),
    editButton(`schedule:${s.Name}`));
}

function scheduleForm(s) {
  const where = s ? `schedule:${s.Name}` : "new-schedule";
  const d = draft ?? (s ? { ...s } : { Name: "", Cron: "", TZ: "", At: "", Jobs: [] });
  const name = text(d.Name, { placeholder: "daily_report" });
  const mode = h("select", { class: "field left" },
    h("option", { value: "cron", selected: !d.At }, "Repeatedly, on a cron spec"),
    h("option", { value: "at", selected: !!d.At }, "Once, at a time"));
  const cron = text(d.Cron, { placeholder: "0 9 * * 1-5" });
  const tz = text(d.TZ, { placeholder: Intl.DateTimeFormat().resolvedOptions().timeZone || "Europe/Paris" });
  const at = text(d.At, { placeholder: "2026-12-01T09:00:00Z" });
  const cronRows = [row(where, "cron", "Cron", cron, "Five fields: minute, hour, day of month, month, day of week."),
    row(where, "tz", "Time zone", tz, "An IANA zone. Leave empty for this device's.")];
  const atRow = row(where, "at", "At", at, "An RFC 3339 time with its offset.");
  const pick = () => { for (const r of cronRows) r.hidden = mode.value !== "cron"; atRow.hidden = mode.value !== "at"; };
  mode.addEventListener("change", pick);
  pick();
  const readAll = () => ({
    ...d, Name: s ? s.Name : name.value.trim(),
    Cron: mode.value === "cron" ? cron.value : "", TZ: mode.value === "cron" ? tz.value : "", At: mode.value === "at" ? at.value : "",
    Jobs: jobs.value(),
  });
  const jobs = jobsPicker(d.Jobs, (next) => { draft = { ...readAll(), Jobs: next }; draw(); });
  read = readAll;
  return h("form", {
    class: s ? "gform pform" : "gform pform new",
    onsubmit: (e) => { e.preventDefault(); act("SaveSchedule", [{ New: !s, ...readAll() }]); },
  },
  !s && h("h3", {}, "New Schedule"),
  !s && row(where, "name", "Name", name, "Lowercase letters, digits, - and _."),
  row(where, "mode", "Fires", mode),
  cronRows, atRow,
  row(where, "jobs", "Runs", jobs.el, "In order, until one fails. An agent Job here must name where it answers."),
  formActions(where, s, "DeleteSchedule", "Add Schedule"));
}

// ---- Edit TOML ----

// openTOML shows the editor over config.toml, at table ("jobs.<name>")
// when set.
async function openTOML(table) {
  if (busy) return;
  busy = true;
  let v;
  try {
    v = await call("TriggersTOML");
  } finally {
    busy = false;
  }
  if (!v || v.Group !== group) return;
  toml = { text: v.Text, problem: v.Problem, table };
  draw();
}

function tomlEditor() {
  const editor = area(toml.text, { rows: 26, class: "field left area mono tomlarea", "aria-label": `config.toml of ${group}` });
  editor.addEventListener("input", () => { toml.text = editor.value; });
  const close = () => { toml = null; draw(); };
  const save = async () => {
    if (busy) return;
    busy = true;
    toml.text = editor.value;
    draw();
    let v;
    try {
      v = await call("SaveTriggersTOML", toml.text);
    } finally {
      busy = false;
    }
    if (v?.Group !== group) return;
    if (v.Problem) {
      toml.problem = v.Problem;
      draw();
      return;
    }
    toml = null;
    open = deleting = "";
    draft = null;
    done = "config.toml saved.";
    tv = (await call("Triggers")) ?? tv;
    draw();
  };
  if (toml.table) {
    // Find the table, as [jobs.x], or as the line defining x under [jobs].
    const [tbl, name] = toml.table.split(".");
    let m = new RegExp(`^\\s*\\[${tbl}\\.${name}\\]`, "m").exec(toml.text);
    const parent = m ? null : new RegExp(`^\\s*\\[${tbl}\\]\\s*$`, "m").exec(toml.text);
    if (parent) {
      const from = parent.index + parent[0].length;
      const end = toml.text.slice(from).search(/^\s*\[/m);
      const body = toml.text.slice(from, end < 0 ? undefined : from + end);
      const line = new RegExp(`^\\s*${name}\\s*=`, "m").exec(body);
      if (line) m = { index: from + line.index, 0: line[0] };
    }
    if (m) setTimeout(() => { editor.focus(); editor.setSelectionRange(m.index, m.index + m[0].length); });
    toml.table = "";
  }
  return h("div", { class: "tomledit" },
    h("h3", {}, `config.toml of ${group}`),
    h("p", { class: "hint" }, "This device's config for the Group, as text. Saving checks it loads first: text that doesn't is refused, and nothing is written."),
    editor,
    toml.problem && h("p", { class: "error", role: "alert" }, toml.problem.Message),
    h("div", { class: "factions" },
      h("button", { class: "btn", type: "button", onclick: close }, "Cancel"),
      h("button", { class: "btn primary", type: "button", disabled: busy, onclick: save }, "Save")));
}
