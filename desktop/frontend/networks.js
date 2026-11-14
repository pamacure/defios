// The Networks page, the edit-target Group's only: each chain this
// device signs for and the RPC endpoints it reaches it by, each a plain
// URL or a Secret's (the shared Secret field). A config.toml that does not
// load shows as such, apart from a change refused. It renders the
// shell.NetworksView the Go side reports (main.UI.Networks) and redraws
// only on its own changes or a new edit-target Group, so a redraw of the
// app never drops what is typed. UI text says "device" for a Node.
import { call, h, put } from "./dom.js";
import { secretField } from "./secretfield.js";

let nv = null; // the NetworksView shown
let group = ""; // the Group it is for
let loading = "";
let pending = null;
let busy = false;
// The form open: "network:<alias>" or "new-network"; "" for none. draft
// is what was typed in it, to draw it again; read reads the form shown.
let open = "";
let draft = null;
let read = null;
let removing = ""; // the form whose Remove asks to confirm
let done = ""; // what the last change did, for the list
let goTo = () => {}; // opens another page, as go in app.js
const mount = h("div", { class: "agents networks" });

const TYPES = { ethereum: "Ethereum", bitcoin: "Bitcoin", solana: "Solana" };

// networksGroupPart is the Group part; go opens another page.
export function networksGroupPart(go) {
  goTo = go;
  return mount;
}

export const networksReady = (s) => !!nv && group === s.EditGroup && nv.Served === s.EditGroupServed;

export function networksLoad(s) {
  if (networksReady(s)) return Promise.resolve();
  if (loading === s.EditGroup) return pending;
  pending = load(s.EditGroup);
  return pending;
}

// networksRefresh reads the view again, as when the page is opened anew,
// and redraws only if it changed and nothing is being edited.
export async function networksRefresh() {
  if (!nv || loading || busy || open || removing) return;
  const g = group;
  const next = await call("Networks");
  if (!next || next.Group !== g || g !== group || busy || open || removing) return;
  if (mount.contains(document.activeElement)) return;
  if (JSON.stringify(next) === JSON.stringify(nv)) return;
  nv = next;
  draw();
}

async function load(g) {
  loading = g;
  let next;
  try {
    next = await call("Networks");
  } finally {
    loading = "";
  }
  if (!next || next.Group !== g) return;
  if (g !== group) {
    open = removing = done = "";
    draft = null;
  }
  group = g;
  nv = next;
  draw();
}

// act calls method; on success the form closes and done says what it did.
async function act(method, args, did) {
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
    nv = next;
    if (!next.Problem) {
      open = removing = "";
      draft = null;
      done = did;
    }
  }
  draw();
  if (next?.Problem && next.Field) document.getElementById(`nw-${next.Field}`)?.focus();
}

function show(form) {
  open = form;
  draft = null;
  removing = done = "";
  if (nv) nv.Problem = null;
  draw();
}

function problemAt(where, field) {
  if (!nv.Problem || nv.Where !== where) return null;
  if (field === "") return nv.Field ? null : nv.Problem;
  return nv.Field === field ? nv.Problem : null;
}

function row(where, field, label, input, hint) {
  const err = problemAt(where, field);
  if (err) input.setAttribute("aria-invalid", "true");
  if (!input.id) input.id = `nw-${field}`;
  return h("div", { class: "frow" },
    h("label", { for: input.id }, label),
    h("div", { class: "fcell" }, input,
      err ? h("p", { class: "error", role: "alert" }, err.Message) : hint && h("p", { class: "hint" }, hint)));
}

const text = (value, attrs = {}) => h("input", { class: "field left", autocomplete: "off", spellcheck: "false", value: value ?? "", ...attrs });
const choose = (options, value, labels = {}) => h("select", { class: "field left" },
  options.map((o) => h("option", { value: o, selected: o === value }, labels[o] ?? o)));

function draw() {
  if (!nv) return;
  read = null;
  if (nv.ConfigProblem) {
    put(mount, h("div", { class: "error", role: "alert" }, nv.ConfigProblem.Message, " ",
      h("button", { class: "link", type: "button", onclick: () => goTo("triggers", { toml: true }) }, "Open Edit TOML")));
    return;
  }
  const list = nv.Networks ?? [];
  put(mount,
    h("p", { class: "hint lead" }, `The chains this device signs for in ${group}, and how it reaches each. A Sign Request names a chain, never an alias, so other devices may call it differently. This device accepts only Sign Requests for chains it has a Network for.`),
    done && h("p", { class: "saved", "aria-live": "polite" }, done),
    h("div", { class: "providers" },
      list.length === 0 && open !== "new-network" && h("p", { class: "placeholder" }, "No Networks yet."),
      list.map((n) => h("div", { class: "provider" }, networkRow(n), open === `network:${n.Alias}` && networkForm(n))),
      open === "new-network"
        ? networkForm(null)
        : h("button", { class: "btn", type: "button", disabled: busy, onclick: () => show("new-network") }, "+ Add Network")));
}

function networkRow(n) {
  const rpc = n.RPCURLs.map((u) => (u.Secret ? `$${u.Secret}` : u.URL)).join(", ");
  return h("div", { class: "prow" },
    h("div", { class: "who" }, h("strong", {}, n.Alias),
      rpc ? h("code", { title: rpc }, rpc) : h("span", { class: "hint" }, "No RPC endpoint: Fill and Broadcast can't reach it.")),
    h("span", { class: "chip" }, n.Chain),
    h("span", { class: "chip", title: "The curve its Keys sign with" }, n.Curve.toUpperCase()),
    open !== `network:${n.Alias}` && h("button", { class: "link", type: "button", disabled: busy, onclick: () => show(`network:${n.Alias}`) }, "Edit"));
}

function networkForm(n) {
  const where = n ? `network:${n.Alias}` : "new-network";
  const d = draft ?? (n
    ? { ...n, RPCURLs: n.RPCURLs.map((u) => ({ ...u })) }
    : { Alias: "", Type: "ethereum", ChainID: "", BitcoinNetwork: "mainnet", Cluster: "mainnet-beta", GenesisHash: "", RPCURLs: [{ URL: "", Secret: "" }] });
  const alias = text(d.Alias, { placeholder: "eth" });
  const type = choose(nv.Types ?? Object.keys(TYPES), d.Type, TYPES);
  let fields = [];
  let readType = () => ({});
  if (d.Type === "ethereum") {
    const id = text(d.ChainID, { inputmode: "numeric", placeholder: "1" });
    fields = [row(where, "chain_id", "Chain ID", id, "1 for Ethereum mainnet, 8453 for Base, 11155111 for Sepolia.")];
    readType = () => ({ ChainID: id.value.trim() });
  } else if (d.Type === "bitcoin") {
    const net = choose(nv.BitcoinNetworks ?? [], d.BitcoinNetwork || "mainnet");
    fields = [row(where, "bitcoin_network", "Network", net)];
    readType = () => ({ BitcoinNetwork: net.value });
  } else {
    const cluster = choose(nv.SolanaClusters ?? [], d.Cluster || "mainnet-beta");
    const genesis = text(d.GenesisHash, { placeholder: "Optional, base58" });
    fields = [row(where, "cluster", "Cluster", cluster),
      row(where, "genesis_hash", "Genesis hash", genesis, "Optional: pins the cluster, so a Sign Request for another is refused.")];
    readType = () => ({ Cluster: cluster.value, GenesisHash: genesis.value.trim() });
  }
  const urls = (d.RPCURLs.length ? d.RPCURLs : [{ URL: "", Secret: "" }]).map((u, i) => {
    const plain = text(u.URL, { type: "url", placeholder: "https://rpc.example", "aria-label": `Endpoint ${i + 1} URL` });
    const fromSecret = secretField({ group, value: u.Secret, noneLabel: "None: the URL typed", onChange: (name) => { plain.hidden = !!name; } });
    plain.hidden = !!u.Secret;
    const remove = h("button", { class: "link", type: "button", "aria-label": `Remove endpoint ${i + 1}`,
      onclick: () => { const next = readAll(); next.RPCURLs.splice(i, 1); draft = next; draw(); } }, "Remove");
    return { el: h("div", { class: "endpoint" }, h("span", { class: "hint" }, `${i + 1}.`), h("div", { class: "stack" }, plain, fromSecret.el), remove),
      read: () => ({ URL: fromSecret.value ? "" : plain.value.trim(), Secret: fromSecret.value }) };
  });
  const addURL = h("button", { class: "link", type: "button", onclick: () => { const next = readAll(); next.RPCURLs.push({ URL: "", Secret: "" }); draft = next; draw(); } }, "+ Add an endpoint");
  const readAll = () => ({ ...d, Alias: n ? n.Alias : alias.value.trim(), Type: type.value, ...readType(), RPCURLs: urls.map((u) => u.read()) });
  read = readAll;
  type.addEventListener("change", () => { draft = { ...readAll(), Type: type.value }; draw(); });
  const err = problemAt(where, "");
  return h("form", {
    class: n ? "gform pform" : "gform pform new",
    onsubmit: (e) => {
      e.preventDefault();
      const v = readAll();
      act("SaveNetwork", [{ New: !n, ...v, RPCURLs: v.RPCURLs.filter((u) => u.URL || u.Secret) }], n ? `Saved ${n.Alias}.` : `Added ${v.Alias}.`);
    },
  },
  !n && h("h3", {}, "New Network"),
  !n && row(where, "alias", "Alias", alias, "What this device calls it: lowercase letters, digits, - and _."),
  row(where, "type", "Type", type),
  fields,
  row(where, "rpc_urls", "RPC endpoints", h("div", { class: "stack" }, urls.map((u) => u.el), addURL),
    "Tried in order. A plain URL may not carry a key: a URL with a key in it belongs in a Secret."),
  err && h("div", { class: "error", role: "alert" }, err.Message),
  removing === where && h("div", { class: "warning", role: "alert" },
    h("p", {}, `Remove ${n.Alias}? This device then refuses Sign Requests for ${n.Chain} in ${group}.`),
    h("div", { class: "row" },
      h("button", { class: "btn danger", type: "button", disabled: busy, onclick: () => act("RemoveNetwork", [n.Alias], `Removed ${n.Alias}.`) }, "Remove"),
      h("button", { class: "btn", type: "button", onclick: () => { removing = ""; draw(); } }, "Keep"))),
  h("div", { class: "factions" },
    n && removing !== where && h("button", { class: "btn", type: "button", disabled: busy, onclick: () => { draft = readAll(); removing = where; draw(); } }, "Remove…"),
    h("button", { class: "btn", type: "button", onclick: () => { open = removing = ""; draft = null; nv.Problem = null; draw(); } }, "Cancel"),
    h("button", { class: "btn primary", type: "submit", disabled: busy }, n ? "Save" : "Add Network")));
}
