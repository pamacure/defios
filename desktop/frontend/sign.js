// Sign Requests on the Talk page: the card each shows as in the transcript,
// and the "+" form that proposes one. The card renders a shell.SignCard; the
// form keeps only what is being typed, and every step goes through the Go
// side (main.UI.SignForm, FillSign, ProposeSign). UI text says "device" for
// a Node.
import { call, h } from "./dom.js";
import { callFailed, markInvalid, problemBox, showProblem } from "./notice.js";

// ctx is the Talk page's: { ts() the TalkState, act(method, ...args),
// draw(), blocks(view) rendering a Transcript view, convName(c) and
// curveName(curve) as the Conversations list words them }.
let ctx = null;
export function signInit(c) { ctx = c; }

// What only the frontend keeps for cards: the Refuse reason being
// written, and the Job runs opened.
const cui = {
  refusing: "", // msg_id
  reason: "",
  runs: new Map(), // "chain/handle" -> { blocks } | { problem } | {}
};

const STATUS = {
  open: ["Waiting for devices", "warn"],
  waiting: ["Enough accepted · waiting to sign", "warn"],
  signing: ["Signing…", "ok"],
  signed: ["Signed", "ok"],
  refused: ["Refused", "bad"],
  aborted: ["Aborted", "bad"],
  expired: ["Expired", "bad"],
};

const hhmm = (at) => {
  const d = new Date(at);
  if (Number.isNaN(d.getTime()) || d.getFullYear() < 2000) return "";
  return d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
};

export function statusChip(status) {
  const [label, cls] = STATUS[status] ?? [status, ""];
  return h("span", { class: `chip ${cls}` }, label);
}

// signCard is the transcript line for it, a KindSign Item.
export function signCard(it, busy) {
  const c = it.Sign;
  const who = it.Side === "left" && h("span", { class: "who" },
    h("strong", {}, it.From),
    it.Handle && h("span", { class: "handle" }, `@${it.Handle}`),
    it.Job && h("span", { class: "handle" }, "Job"));
  const accepted = c.Stances.filter((s) => s.Stance === "accepted" && !s.Queued).length;
  return h("div", { class: `msg ${it.Side} signmsg`, id: `card-${c.MsgID}` },
    h("div", { class: "meta" }, who, h("time", {}, hhmm(it.At))),
    h("article", { class: "signcard", "aria-label": "Sign Request" },
      h("header", {}, h("strong", {}, "Sign Request"), statusChip(c.Status)),
      h("p", { class: "reason" }, c.Reason),
      h("div", { class: "txs" }, c.Transactions.map((tx, i) => h("div", { class: "tx" },
        h("span", { class: "label" }, `${c.Transactions.length > 1 ? `${i + 1}. ` : ""}${tx.Chain}`),
        h("ul", {}, tx.Lines.map((l) => h("li", {}, l)))))),
      h("div", { class: "stances" },
        c.Needed ? quorum(c, accepted) : h("span", { class: "label" }, "Devices"),
        c.Stances.map(stanceRow)),
      c.Status === "open" && c.Deadline && h("p", { class: "hint" }, `Lapses at ${hhmm(c.Deadline)} unless enough devices accept.`),
      c.Queued && h("p", { class: "hint queued" }, "This device's stance waits here and is sent once enough devices are online."),
      actions(c, busy),
      c.Receipts.length > 0 && h("div", { class: "receipts" },
        h("span", { class: "label" }, "Broadcast"),
        c.Receipts.map((r) => h("div", { class: "receipt" },
          h("span", {}, `${r.Device} · ${hhmm(r.At)}`),
          r.Transactions.map((tx) => h("code", { title: tx.TxHash }, `${tx.Chain}: ${tx.TxHash}`))))),
      c.Runs.length > 0 && h("div", { class: "runs" },
        h("span", { class: "label" }, "This device's Hooks"),
        c.Runs.map(runRow))));
}

// quorum is the card's meter: one segment per device needed, filled as
// they accept. motion.js animates the ones just filled, by its data.
function quorum(c, accepted) {
  const filled = Math.min(accepted, c.Needed);
  const state = filled >= c.Needed ? "met" : DEAD.has(c.Status) ? "dead" : "";
  return h("div", { class: `quorum ${state}`, "data-key": c.MsgID, "data-n": filled, "data-needed": c.Needed },
    h("span", { class: "segs", "aria-hidden": "true" },
      Array.from({ length: c.Needed }, (_, i) => h("span", { class: i < filled ? "seg on" : "seg" }))),
    h("span", { class: "label" }, h("strong", { class: "count" }, `${accepted} of ${c.Needed}`), " needed have accepted"));
}

const DEAD = new Set(["refused", "aborted", "expired"]);

function stanceRow(s) {
  const label = { accepted: "Accepted", refused: "Refused", unaddressed: "No answer yet" }[s.Stance] ?? s.Stance;
  return h("div", { class: "stance" },
    h("span", { class: `dot ${s.Stance === "accepted" ? "on" : s.Stance === "refused" ? "off" : ""}` }),
    h("span", { class: "who" }, h("strong", {}, s.Device), s.Handle && h("span", { class: "handle" }, `@${s.Handle}`)),
    h("span", { class: `chip ${s.Stance === "accepted" ? "ok" : s.Stance === "refused" ? "bad" : ""}` }, label),
    s.Queued && h("span", { class: "chip warn" }, "Queued"),
    s.Reason && h("span", { class: "why" }, `“${s.Reason}”`));
}

function actions(c, busy) {
  if (cui.refusing === c.MsgID && c.CanAnswer) {
    return h("form", {
      class: "refuse",
      onsubmit: async (e) => {
        e.preventDefault();
        const next = await ctx.act("RefuseSign", c.MsgID, cui.reason.trim());
        if (next && !next.Problem) { cui.refusing = ""; cui.reason = ""; ctx.draw(); return; }
        // The toast says why; the reason is what to change.
        const why = document.querySelector('[data-field="refuse-reason"]');
        if (why && next?.Problem?.Code === "invalid") { markInvalid(why); why.focus(); }
      },
    },
    reasonField(),
    h("p", { class: "warn-text" }, "A stance is final: once refused, this device can't accept this request."),
    h("div", { class: "row" },
      h("button", { class: "btn small", type: "button", onclick: () => { cui.refusing = ""; ctx.draw(); } }, "Back"),
      h("button", { class: "btn small danger", type: "submit", disabled: busy }, "Refuse")));
  }
  const row = [];
  if (c.CanAnswer) {
    row.push(h("button", { class: "btn small bright", type: "button", disabled: busy, onclick: () => ctx.act("AcceptSign", c.MsgID) }, "Accept"));
    row.push(h("button", { class: "btn small", type: "button", disabled: busy, onclick: () => { cui.refusing = c.MsgID; cui.reason = ""; ctx.draw(); } }, "Refuse"));
  }
  if (c.CanBroadcast) {
    row.push(h("button", { class: "btn small bright", type: "button", disabled: busy, onclick: () => ctx.act("BroadcastSign", c.MsgID) },
      c.Receipts.length ? "Broadcast again" : "Broadcast"));
  }
  return row.length > 0 && h("div", { class: "row" }, row);
}

function reasonField() {
  const el = h("textarea", {
    class: "field", rows: 2, required: true, "data-field": "refuse-reason", "data-autofocus": true,
    placeholder: "Why refuse? The other devices see this.", "aria-label": "Reason for refusing",
  });
  el.value = cui.reason;
  el.addEventListener("input", () => { cui.reason = el.value; });
  return el;
}

const JOB_STATE = { pending: "Waiting", running: "Running…", ok: "Done", failed: "Failed", not_run: "Not run" };

function runRow(r) {
  return h("div", { class: "run" },
    h("span", { class: "hint" }, `Hook ${r.Trigger}`),
    r.Jobs.map((j) => {
      const key = `${j.Chain}/${j.Handle}`;
      const open = cui.runs.get(key);
      return h("div", { class: "job" },
        h("div", { class: "row" },
          h("span", {}, j.Handle ? `@${j.Handle}` : `Job ${j.Job}`),
          h("span", { class: `chip ${j.State === "ok" ? "ok" : j.State === "failed" ? "bad" : ""}` }, JOB_STATE[j.State] ?? j.State),
          j.Decided && h("span", { class: "chip ok" }, "Took this device's stance"),
          j.Chain && h("button", {
            class: "link", type: "button", "aria-expanded": open ? "true" : "false", onclick: () => toggleRun(j),
          }, open ? "Hide run" : "See its run")),
        // The chip says it failed; why, as the Job words it, is there to open.
        j.State === "failed" && j.Error && h("details", { class: "why" }, h("summary", {}, "Why it failed"), h("pre", {}, j.Error)),
        j.State !== "failed" && j.Error && h("p", { class: "hint" }, j.Error),
        open && ctx.blocks(open));
    }));
}

async function toggleRun(j) {
  const key = `${j.Chain}/${j.Handle}`;
  if (cui.runs.has(key)) {
    cui.runs.delete(key);
    ctx.draw();
    return;
  }
  cui.runs.set(key, {});
  ctx.draw();
  const v = await call("RunTranscript", j.Conversation, j.Chain, j.Handle).catch((err) => ({ Problem: callFailed(err) }));
  if (cui.runs.has(key)) cui.runs.set(key, v.Problem ? { problem: v.Problem } : { blocks: v.Blocks ?? [] });
  ctx.draw();
}

// The "+" form, for the Key of the Conversation open when it opened: per
// transaction a Network and what it does, as a transfer, an Ethereum
// contract call or its intent written out; and a reason. Fill completes
// them for review; Propose.

const blankTx = () => ({ network: "", mode: "transfer", to: "", amount: "", data: "", sig: "", args: [], intent: "" });

// MODES are the ways to write a transaction, by its chain: a Bitcoin
// payment needs its inputs chosen, so its intent is written out.
const MODES = { ethereum: ["transfer", "call", "intent"], solana: ["transfer", "intent"] };
const MODE_LABEL = { transfer: "Transfer", call: "Contract call", intent: "Intent JSON" };
const modesOf = (type) => MODES[type] ?? ["intent"];
const modeOf = (tx, type) => (modesOf(type).includes(tx.mode) ? tx.mode : modesOf(type)[0]);

// callParams are the parameters of signature sig as written, for a field
// each: [{ type, name }], or [] until it reads as name(...). The Go side
// (internal/evmabi) reads it again, and says what is wrong with it.
function callParams(sig) {
  const m = /^\s*(?:function\s+)?[A-Za-z_$][\w$]*\s*\(([\s\S]*)\)\s*$/.exec(sig);
  if (!m || !m[1].trim()) return [];
  const parts = [];
  let depth = 0;
  let from = 0;
  for (let i = 0; i < m[1].length; i++) {
    const c = m[1][i];
    if (c === "(" || c === "[") depth++;
    else if (c === ")" || c === "]") depth--;
    else if (c === "," && depth === 0) { parts.push(m[1].slice(from, i)); from = i + 1; }
  }
  parts.push(m[1].slice(from));
  return parts.map((p) => {
    const words = [];
    let d = 0;
    let w = "";
    for (const c of p.trim()) {
      if (c === "(" || c === "[") d++;
      else if (c === ")" || c === "]") d--;
      if (d === 0 && /\s/.test(c)) { if (w) words.push(w); w = ""; } else w += c;
    }
    if (w) words.push(w);
    const named = words.slice(1).filter((x) => !["memory", "calldata", "storage", "payable"].includes(x));
    return { type: words[0] ?? "", name: named.at(-1) ?? "" };
  });
}

// argHint is the placeholder for an argument of type typ.
function argHint(typ) {
  if (typ.endsWith("]") || typ.startsWith("(")) return "JSON array, such as [1, 2]";
  if (typ === "address") return "0x address";
  if (typ === "bool") return "true or false";
  if (/^u?int\d*$/.test(typ)) return "0";
  if (/^bytes\d*$/.test(typ)) return "0x";
  return "";
}

const form = {
  open: false,
  step: "edit", // edit | review
  conv: "",
  reason: "",
  txs: [blankTx()],
  networks: [], // for conv: { Alias, Type, Chain }
  filled: null, // the SignFormView Fill returned
  problem: null,
  busy: false,
};

export const formOpen = () => form.open;

// openSignForm opens the form on the Key of the open Conversation. Group
// talk has no Key, so no form; the Key stays the one opened on.
export async function openSignForm() {
  const conv = ctx.ts().Open;
  if (conv === "") return;
  Object.assign(form, { open: true, step: "edit", conv, reason: "", txs: [blankTx()], filled: null, unread: null, networks: [] });
  ctx.draw();
  await loadNetworks();
}

async function loadNetworks() {
  let v;
  try {
    v = await call("SignForm", form.conv);
  } catch (err) {
    v = { Problem: callFailed(err) };
  }
  form.networks = v.Networks ?? [];
  // Networks that can't be read stay so while the form is open: shown in it.
  form.unread = v.Problem ?? null;
  for (const tx of form.txs) if (!form.networks.some((n) => n.Alias === tx.network)) tx.network = form.networks[0]?.Alias ?? "";
  ctx.draw();
}

const closeForm = () => { form.open = false; ctx.draw(); };
const netOf = (alias) => form.networks.find((n) => n.Alias === alias);

function draft() {
  return {
    Conversation: form.conv,
    Reason: form.reason.trim(),
    Transactions: form.txs.map((tx) => {
      const type = netOf(tx.network)?.Type ?? "";
      const out = { Network: tx.network, Type: type, To: "", Amount: "", Data: "", Call: "", Args: [], Intent: "" };
      switch (modeOf(tx, type)) {
        case "transfer":
          return { ...out, To: tx.to, Amount: tx.amount, Data: type === "ethereum" ? tx.data : "" };
        case "call":
          return {
            ...out, To: tx.to, Amount: tx.amount.trim() || "0", Call: tx.sig,
            Args: callParams(tx.sig).map((_, i) => tx.args[i] ?? ""),
          };
        default:
          return { ...out, Intent: tx.intent };
      }
    }),
  };
}

// step calls method for the form; a refusal is a toast, above the form,
// and leaves the form as it was.
async function step(method, ...args) {
  form.busy = true;
  ctx.draw();
  try {
    const v = await call(method, ...args);
    showProblem(v?.Problem, { scope: "talk" });
    return v;
  } catch (err) {
    showProblem(callFailed(err), { scope: "talk" });
    return null;
  } finally {
    form.busy = false;
    ctx.draw();
  }
}

async function fill() {
  const v = await step("FillSign", draft());
  if (!v || v.Problem) return;
  form.filled = v;
  form.step = "review";
  ctx.draw();
}

async function propose() {
  const v = await step("ProposeSign", form.conv, form.reason.trim(), form.filled.Transactions);
  if (!v || v.Problem) return;
  form.open = false;
  await ctx.act("FocusSign", ctx.ts().Group, form.conv, v.MsgID);
}

// field is an input bound to obj[name].
function field(obj, name, attrs = {}, tag = "input") {
  const el = h(tag, { class: "field", autocomplete: "off", spellcheck: "false", "data-field": attrs["data-field"] ?? name, ...attrs });
  el.value = obj[name] ?? "";
  el.addEventListener("input", () => { obj[name] = el.value; });
  return el;
}

export function signForm() {
  if (!form.open) return null;
  const key = ctx.ts().Conversations.find((c) => c.ID === form.conv);
  const body = !key ? h("p", { class: "hint" }, "This Key is no longer in the Group.")
    : form.step === "review" ? review() : edit();
  return h("div", { class: "modal-back", onclick: (e) => { if (e.target === e.currentTarget) closeForm(); } },
    h("div", { class: "modal signform", role: "dialog", "aria-modal": "true", "aria-label": "New Sign Request" },
      h("header", {}, h("h2", {}, form.step === "review" ? "Review the Sign Request" : "New Sign Request"),
        h("button", { class: "btn small", type: "button", "aria-label": "Close", onclick: closeForm }, "✕")),
      key && h("p", { class: "for" },
        h("span", { class: "label" }, "Key"),
        h("strong", {}, ctx.convName(key)),
        h("span", { class: "sub" }, `${ctx.curveName(key.Curve)} · ${key.T} of ${key.N}`)),
      problemBox(form.unread),
      body));
}

function edit() {
  const noNets = form.networks.length === 0;
  return h("form", {
    class: "stack",
    onsubmit: (e) => { e.preventDefault(); fill(); },
  },
  noNets
    ? h("p", { class: "hint" }, "This device has no Network for this Key's chains. Add one on the Networks page.")
    : form.txs.map((tx, i) => txFields(tx, i)),
  !noNets && h("button", { class: "link", type: "button", onclick: () => { form.txs.push({ ...blankTx(), network: form.networks[0].Alias }); ctx.draw(); } }, "+ Add a transaction"),
  h("label", {}, "Reason", field(form, "reason", { required: true, rows: 2, placeholder: "What this pays for. Every device sees it.", "data-field": "sign-reason" }, "textarea")),
  h("p", { class: "hint" }, "Fill completes the nonce, fees and blockhash from this device's Networks, for you to review before proposing. Proposing counts as this device's accept."),
  h("div", { class: "row end" },
    h("button", { class: "btn small", type: "button", onclick: closeForm }, "Cancel"),
    h("button", { class: "btn small bright", type: "submit", disabled: form.busy || noNets }, form.busy ? "Filling…" : "Fill and review")));
}

function txFields(tx, i) {
  const net = netOf(tx.network);
  const type = net?.Type ?? "";
  const mode = modeOf(tx, type);
  const sel = h("select", {
    class: "field", "aria-label": "Network",
    onchange: (e) => { tx.network = e.target.value; ctx.draw(); },
  }, form.networks.map((n) => h("option", { value: n.Alias, selected: n.Alias === tx.network }, `${n.Alias} · ${n.Chain}`)));
  const modes = modesOf(type);
  const tabs = modes.length > 1 && h("div", { class: "tabs", role: "tablist", "aria-label": "Write it as" },
    modes.map((m) => h("button", {
      type: "button", role: "tab", "aria-selected": m === mode ? "true" : "false",
      onclick: () => { tx.mode = m; ctx.draw(); },
    }, MODE_LABEL[m])));
  const body = { transfer: transferFields, call: callFields, intent: intentFields }[mode](tx, i, type);
  return h("fieldset", { class: "tx-fields" },
    h("legend", {}, `Transaction ${i + 1}`),
    h("label", {}, "Network", sel),
    tabs,
    body,
    form.txs.length > 1 && h("div", { class: "row" },
      h("button", { class: "link", type: "button", onclick: () => { form.txs.splice(i, 1); ctx.draw(); } }, "Remove")));
}

function transferFields(tx, i, type) {
  const unit = type === "solana" ? "lamports" : "wei";
  return [
    h("label", {}, "To", field(tx, "to", { required: true, "data-field": `to-${i}`, placeholder: type === "solana" ? "Solana address" : "0x address" })),
    h("label", {}, `Amount (${unit})`, field(tx, "amount", { required: true, inputmode: "numeric", "data-field": `amount-${i}`, placeholder: "0" })),
    type === "ethereum" && h("label", {}, "Data (optional hex)", field(tx, "data", { "data-field": `data-${i}`, placeholder: "0x" })),
  ];
}

// callFields are a contract call's: its address, the function's signature,
// a field per parameter as the signature names them, and the value sent.
function callFields(tx, i) {
  const params = callParams(tx.sig);
  const sig = field(tx, "sig", { required: true, class: "field mono", "data-field": `sig-${i}`, placeholder: "transfer(address to, uint256 amount)" });
  // The argument fields follow the signature as it is typed.
  sig.addEventListener("input", () => {
    const next = callParams(tx.sig);
    if (next.map((p) => p.type + " " + p.name).join() !== params.map((p) => p.type + " " + p.name).join()) ctx.draw();
  });
  return [
    h("label", {}, "Contract", field(tx, "to", { required: true, "data-field": `to-${i}`, placeholder: "0x address" })),
    h("label", {}, "Function", sig),
    h("p", { class: "hint" }, "Paste it from the contract's source or an explorer; names are fine. Arrays and tuples are written as JSON."),
    params.map((p, n) => h("label", {}, h("span", {}, `${p.name || `Argument ${n + 1}`} `, h("span", { class: "type" }, p.type)),
      field(tx.args, n, { required: true, class: "field mono", "data-field": `arg-${i}-${n}`, placeholder: argHint(p.type) }))),
    h("label", {}, "Value (wei)", field(tx, "amount", { inputmode: "numeric", "data-field": `amount-${i}`, placeholder: "0" })),
  ];
}

function intentFields(tx, i, type) {
  return [
    h("label", {}, `Intent (${type || "chain"} JSON)`,
      field(tx, "intent", { rows: 5, required: true, class: "field mono", "data-field": `intent-${i}`, placeholder: type === "bitcoin" ? '{"inputs": [...], "outputs": [...]}' : "{ ... }" }, "textarea")),
    h("p", { class: "hint" }, type === "bitcoin"
      ? "A Bitcoin payment needs its inputs chosen, so it is written out as the chain's intent. Fill completes the fees."
      : "The transaction as the chain's intent, as defios sign propose --transactions takes it. Fill completes what it leaves out."),
  ];
}

function review() {
  const f = form.filled;
  return h("div", { class: "stack" },
    h("p", { class: "reason" }, form.reason.trim()),
    f.Review.map((rv, i) => h("section", { class: "tx-review" },
      h("strong", {}, `${i + 1}. ${rv.Network}${rv.Chain ? ` · ${rv.Chain}` : ""}`),
      h("dl", {}, rv.Fields.map((fd) => [
        h("dt", {}, fd.Name, rv.Filled.includes(fd.Name) && h("span", { class: "chip ok" }, "filled")),
        h("dd", {}, h("code", {}, fd.Value)),
      ])),
      rv.Warning && h("p", { class: "warn-text" }, rv.Warning))),
    h("div", { class: "row end" },
      h("button", { class: "btn small", type: "button", onclick: () => { form.step = "edit"; ctx.draw(); } }, "Back"),
      h("button", { class: "btn small bright", type: "button", disabled: form.busy, onclick: propose }, form.busy ? "Proposing…" : "Propose")));
}
