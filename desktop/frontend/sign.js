// Sign Requests on the Talk page: the card each shows as in the transcript,
// and the "+" form that proposes one. The card renders a shell.SignCard; the
// form keeps only what is being typed, and every step goes through the Go
// side (main.UI.SignForm, FillSign, ProposeSign). UI text says "device" for
// a Node.
import { call, h } from "./dom.js";

// ctx is the Talk page's: { ts() the TalkState, act(method, ...args),
// draw(), blocks(view) rendering a Transcript view }.
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
        h("span", { class: "label" }, c.Needed ? `${accepted} of ${c.Needed} needed have accepted` : "Devices"),
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
        if (next && !next.Problem) { cui.refusing = ""; cui.reason = ""; ctx.draw(); }
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
        j.Error && h("p", { class: "error" }, j.Error),
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
  const v = await call("SignJobRun", j.Conversation, j.Chain, j.Handle);
  if (cui.runs.has(key)) cui.runs.set(key, v.Problem ? { problem: v.Problem } : { blocks: v.Blocks ?? [] });
  ctx.draw();
}

// The "+" form: Key, a Network and a transfer (or the intent written out)
// per transaction, and a reason; Fill completes them for review; Propose.

const blankTx = () => ({ network: "", to: "", amount: "", data: "", intent: "", raw: false });

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

// openSignForm opens the form on the Key of conv, or the first Key.
export async function openSignForm() {
  const ts = ctx.ts();
  const keys = ts.Conversations.filter((c) => c.ID !== "");
  Object.assign(form, { open: true, step: "edit", reason: "", txs: [blankTx()], filled: null, problem: null, networks: [] });
  form.conv = keys.some((k) => k.ID === ts.Open) ? ts.Open : keys[0]?.ID ?? "";
  ctx.draw();
  await loadNetworks();
}

async function loadNetworks() {
  if (!form.conv) return;
  const v = await call("SignForm", form.conv);
  form.networks = v.Networks ?? [];
  form.problem = v.Problem ?? null;
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
      const raw = tx.raw || type === "bitcoin";
      return { Network: tx.network, Type: type, To: raw ? "" : tx.to, Amount: raw ? "" : tx.amount, Data: raw ? "" : tx.data, Intent: raw ? tx.intent : "" };
    }),
  };
}

async function step(method, ...args) {
  form.busy = true;
  form.problem = null;
  ctx.draw();
  try {
    return await call(method, ...args);
  } finally {
    form.busy = false;
    ctx.draw();
  }
}

async function fill() {
  const v = await step("FillSign", draft());
  if (v.Problem) { form.problem = v.Problem; ctx.draw(); return; }
  form.filled = v;
  form.step = "review";
  ctx.draw();
}

async function propose() {
  const v = await step("ProposeSign", form.conv, form.reason.trim(), form.filled.Transactions);
  if (v.Problem) { form.problem = v.Problem; ctx.draw(); return; }
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
  const ts = ctx.ts();
  const keys = ts.Conversations.filter((c) => c.ID !== "");
  const body = form.step === "review" ? review() : edit(keys);
  return h("div", { class: "modal-back", onclick: (e) => { if (e.target === e.currentTarget) closeForm(); } },
    h("div", { class: "modal signform", role: "dialog", "aria-modal": "true", "aria-label": "New Sign Request" },
      h("header", {}, h("h2", {}, form.step === "review" ? "Review the Sign Request" : "New Sign Request"),
        h("button", { class: "btn small", type: "button", "aria-label": "Close", onclick: closeForm }, "✕")),
      form.problem && h("div", { class: "banner", role: "alert" }, form.problem.Message),
      body));
}

function edit(keys) {
  if (!keys.length) {
    return h("p", { class: "hint" }, "This Group has no Keys yet. Make one with + New Key first.");
  }
  const key = h("select", {
    class: "field", "aria-label": "Key",
    onchange: (e) => { form.conv = e.target.value; loadNetworks(); },
  }, keys.map((k) => h("option", { value: k.ID, selected: k.ID === form.conv },
    `${k.Alias || k.ShortKey} · ${k.T} of ${k.N}`)));
  const noNets = form.networks.length === 0;
  return h("form", {
    class: "stack",
    onsubmit: (e) => { e.preventDefault(); fill(); },
  },
  h("label", {}, "Key", key),
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
  const raw = tx.raw || type === "bitcoin";
  const sel = h("select", {
    class: "field", "aria-label": "Network",
    onchange: (e) => { tx.network = e.target.value; ctx.draw(); },
  }, form.networks.map((n) => h("option", { value: n.Alias, selected: n.Alias === tx.network }, `${n.Alias} · ${n.Chain}`)));
  const unit = type === "solana" ? "lamports" : "wei";
  return h("fieldset", { class: "tx-fields" },
    h("legend", {}, `Transaction ${i + 1}`),
    h("label", {}, "Network", sel),
    raw
      ? h("label", {}, `Intent (${type || "chain"} JSON)`,
        field(tx, "intent", { rows: 5, required: true, class: "field mono", "data-field": `intent-${i}`, placeholder: type === "bitcoin" ? '{"inputs": [...], "outputs": [...]}' : "{ ... }" }, "textarea"))
      : [
        h("label", {}, "To", field(tx, "to", { required: true, "data-field": `to-${i}`, placeholder: type === "solana" ? "Solana address" : "0x address" })),
        h("label", {}, `Amount (${unit})`, field(tx, "amount", { required: true, inputmode: "numeric", "data-field": `amount-${i}`, placeholder: "0" })),
        type === "ethereum" && h("label", {}, "Data (optional hex)", field(tx, "data", { "data-field": `data-${i}`, placeholder: "0x" })),
      ],
    h("div", { class: "row" },
      type !== "bitcoin" && h("button", { class: "link", type: "button", onclick: () => { tx.raw = !tx.raw; ctx.draw(); } },
        tx.raw ? "Use a transfer instead" : "Write the intent out"),
      form.txs.length > 1 && h("button", { class: "link", type: "button", onclick: () => { form.txs.splice(i, 1); ctx.draw(); } }, "Remove")));
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
