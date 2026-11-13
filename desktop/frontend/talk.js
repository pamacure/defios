// The Talk page: the served Group's Conversations on the left, the open
// one's transcript and composer in the middle, and what it is about on the
// right. It renders the shell.TalkState the Go side reports (main.UI.Talk
// and the talk:state event), which it keeps current from the Node's events;
// it holds only what is being typed or is open on screen. UI text says
// "device" for a Node.
import { call, h } from "./dom.js";
import { openSignForm, signCard, signForm, signInit, statusChip } from "./sign.js";

let ts = null; // the TalkState shown
let mount = null; // the element the page draws into, while on screen
let nav = () => {}; // opens another section page, and what on it

// GROUP_TALK is Group talk's Conversation ID, node.GroupTalk.
const GROUP_TALK = "";

// What only the frontend keeps.
const tui = {
  roster: false, // the Roster popover is open
  about: false, // the right column is open over the transcript, on a narrow window
  newKey: false, // the + New Key form is open
  aborting: "", // the keygen Session whose Abort reason is being written
  editAlias: false,
  busy: false,
  expanded: new Map(), // msg_id, or run:chain/handle for a silent run -> its Transcript: { blocks } | { problem } | {} while loading
  drafts: new Map(), // Conversation ID -> what was typed there
  form: { curve: "ecdsa", t: "", alias: "", reason: "", keyAlias: "" },
  shownOpen: null, // the Conversation the composer's draft belongs to
  // The message being sent, shown at once: Send returns only once it
  // commits, or after Raft's ApplyTimeout once it is queued.
  sending: null, // { conv, text }
  // The transcript's scroll, kept as it scrolls: a redraw of the whole
  // app makes it anew.
  scroll: { top: 0, pinned: true },
  focused: "", // the Sign Request whose card was last brought into view
  clearing: null, // the shell.ClearView Clear asks to confirm
};

// talkPage is the page, drawn into a new element; go opens another page.
export function talkPage(go) {
  nav = go;
  mount ??= h("div", { class: "talk" });
  draw();
  return mount;
}

// talkUpdate shows next, unless it is older than what is shown.
export function talkUpdate(next) {
  if (!next || (ts && next.Seq < ts.Seq)) return;
  const streaming = ts && onlyLiveChanged(ts, next);
  ts = next;
  if (!streaming || !patchLive()) draw();
}

// onlyLiveChanged: next differs from shown only in what the agents
// running now have said so far, as for each streamed delta.
function onlyLiveChanged(shown, next) {
  const rest = (s) => JSON.stringify({ ...s, Seq: 0, Live: s.Live.map((c) => c.Chain) });
  return rest(shown) === rest(next);
}

// patchLive redraws only the running chains' output, so a delta, many a
// second, leaves the rest of the page as it is: a click there is never
// lost to a redraw, and nothing flickers. It reports whether it could.
function patchLive() {
  const tr = mount?.isConnected && mount.querySelector(".transcript");
  const shown = tr ? tr.querySelectorAll(":scope > .live") : [];
  if (!tr || shown.length !== ts.Live.length) return false;
  ts.Live.forEach((c, i) => shown[i].replaceWith(liveChain(c)));
  if (tui.scroll.pinned) tr.scrollTop = tr.scrollHeight;
  return true;
}

async function act(method, ...args) {
  tui.busy = true;
  draw();
  try {
    const next = await call(method, ...args);
    talkUpdate(next);
    return next;
  } finally {
    tui.busy = false;
    draw();
  }
}

const curveName = (c) => ({ ecdsa: "ECDSA", eddsa: "EdDSA" }[c] ?? c);
const convName = (c) => (c.ID === GROUP_TALK ? "Group" : c.Alias || `${curveName(c.Curve)} ${c.ShortKey}`);
const shortID = (id) => (id.length > 16 ? `${id.slice(0, 16)}…` : id);

function time(at) {
  const d = new Date(at);
  if (Number.isNaN(d.getTime()) || d.getFullYear() < 2000) return "";
  return d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
}

// The composer is made once and kept across redraws, so a redraw never
// drops what is being typed, the caret or focus.
const composer = makeComposer();

// draw redraws the page, keeping the transcript's scroll: pinned to the
// end if it was there.
function draw() {
  if (!mount) return;
  if (!ts || !ts.Group) {
    mount.replaceChildren(h("div", { class: "talk-empty" }, h("p", { class: "placeholder" }, "Loading…")));
    return;
  }
  if (tui.shownOpen !== ts.Open) {
    if (tui.shownOpen !== null) tui.drafts.set(tui.shownOpen, composer.input.value);
    composer.input.value = tui.drafts.get(ts.Open) ?? "";
    tui.shownOpen = ts.Open;
    tui.editAlias = false;
    tui.clearing = null;
    composer.closeSuggestions();
  }
  // A field being typed in is made anew: focus its successor.
  const active = document.activeElement;
  const field = mount.contains(active) ? active.dataset?.field : undefined;
  const caret = field ? [active.selectionStart, active.selectionEnd] : null;
  const focused = active === composer.input;
  mount.classList.toggle("about-open", tui.about);
  mount.replaceChildren(left(), center(), right(), signForm() ?? "");
  const tr = mount.querySelector(".transcript");
  const scroll = () => { if (tr.isConnected) tr.scrollTop = tui.scroll.pinned ? tr.scrollHeight : tui.scroll.top; };
  if (tr) {
    scroll();
    // A redraw of the whole app moves the page out of the document and
    // back, which resets its scroll: set it again once it is back.
    queueMicrotask(scroll);
  }
  composer.update();
  focusCard();
  const again = field && mount.querySelector(`[data-field="${field}"]`);
  if (again) {
    again.focus();
    if (caret && again.setSelectionRange && again.type !== "number") again.setSelectionRange(...caret);
  } else if (focused) {
    composer.input.focus();
  } else if (!mount.contains(document.activeElement)) {
    mount.querySelector("[data-autofocus]")?.focus();
  }
}

// focusCard brings the card ts.Focus names into view, once, as a
// notification about it was clicked.
function focusCard() {
  if (!ts.Focus || ts.Focus === tui.focused) return;
  const el = mount.querySelector(`#card-${CSS.escape(ts.Focus)}`);
  if (!el) return; // its card is not read yet
  tui.focused = ts.Focus;
  showCard(el);
}

// showCard scrolls the transcript to a card and marks it for a moment.
function showCard(el) {
  tui.scroll.pinned = false;
  queueMicrotask(() => {
    el.scrollIntoView({ block: "center", behavior: "smooth" });
    el.classList.add("flash");
    setTimeout(() => el.classList.remove("flash"), 1600);
  });
}

// The left column: Group, then each Key, then Keys being made.

function left() {
  const open = (id) => () => { tui.roster = false; act("OpenConversation", id); };
  const rows = ts.Conversations.map((c) => h("button", {
    class: "conv", "aria-current": c.ID === ts.Open ? "true" : null, onclick: open(c.ID),
  },
  h("span", { class: "name" }, convName(c)),
  c.ID === GROUP_TALK ? h("span", { class: "sub" }, "Everyone in the Group")
    : h("span", { class: "sub" }, `${curveName(c.Curve)} · ${c.T} of ${c.N}`),
  c.OpenSignRequests > 0 && h("span", { class: "badge", title: "Open Sign Requests" }, String(c.OpenSignRequests))));
  return h("nav", { class: "convs", "aria-label": "Conversations" },
    h("div", { class: "list" }, rows, ts.Keygens.map(keygenRow)),
    newKey());
}

function keygenRow(k) {
  const name = k.Alias || `New ${curveName(k.Curve)} Key`;
  const aborting = tui.aborting === k.SessionID;
  return h("div", { class: "conv provisional" },
    h("span", { class: "name" }, name),
    h("span", { class: "sub" }, `Being made · ${curveName(k.Curve)} · ${k.T} of ${k.N}`),
    aborting
      ? h("form", {
        class: "inline",
        onsubmit: (e) => {
          e.preventDefault();
          tui.aborting = "";
          act("AbortKeygen", k.SessionID, tui.form.reason.trim());
          tui.form.reason = "";
        },
      },
      input("reason", { placeholder: "Why abort? (optional)", "aria-label": "Reason", "data-autofocus": true }),
      h("div", { class: "row" },
        h("button", { class: "btn small", type: "button", onclick: () => { tui.aborting = ""; draw(); } }, "Back"),
        h("button", { class: "btn small danger", type: "submit", disabled: tui.busy }, "Abort")))
      : h("div", { class: "row" },
        k.Accepted
          ? h("span", { class: "chip" }, "Accepted · waiting for the others")
          : h("button", { class: "btn small bright", type: "button", disabled: tui.busy, onclick: () => act("AcceptKeygen", k.SessionID, "") }, "Accept"),
        h("button", { class: "btn small", type: "button", disabled: tui.busy, onclick: () => { tui.aborting = k.SessionID; draw(); } }, "Abort")));
}

// input is a field whose value lives in tui.form[name].
function input(name, attrs = {}) {
  const el = h("input", { class: "field", autocomplete: "off", "data-field": name, ...attrs, value: tui.form[name] ?? "" });
  el.addEventListener("input", () => { tui.form[name] = el.value; });
  return el;
}

function newKey() {
  if (!tui.newKey) {
    return h("button", { class: "btn newkey", type: "button", onclick: () => { tui.newKey = true; draw(); } }, "+ New Key");
  }
  const n = ts.Roster.length || 1;
  if (!tui.form.t) tui.form.t = String(Math.min(2, n));
  const curve = h("select", { class: "field", "aria-label": "Curve", onchange: (e) => { tui.form.curve = e.target.value; } },
    h("option", { value: "ecdsa", selected: tui.form.curve === "ecdsa" }, "ECDSA (Ethereum, Bitcoin)"),
    h("option", { value: "eddsa", selected: tui.form.curve === "eddsa" }, "EdDSA (Solana)"));
  return h("form", {
    class: "newkey-form",
    onsubmit: async (e) => {
      e.preventDefault();
      const next = await act("NewKey", tui.form.curve, Number(tui.form.t), tui.form.alias.trim());
      if (next && !next.Problem) {
        tui.newKey = false;
        tui.form.alias = "";
        draw();
      }
    },
  },
  h("strong", {}, "New Key"),
  h("label", {}, "Curve", curve),
  h("label", {}, `Signers needed, of ${n} devices`, input("t", { type: "number", min: 1, max: n, required: true })),
  h("label", {}, "Name (optional)", input("alias", { placeholder: "e.g. treasury" })),
  h("p", { class: "hint" }, "Every device in the Group takes part in making it, and each must accept."),
  h("div", { class: "row" },
    h("button", { class: "btn small", type: "button", onclick: () => { tui.newKey = false; draw(); } }, "Cancel"),
    h("button", { class: "btn small bright", type: "submit", disabled: tui.busy }, "Make Key")));
}

// The middle: header, banners, transcript and composer.

function openConv() {
  return ts.Conversations.find((c) => c.ID === ts.Open) ?? { ID: ts.Open, Alias: "", Curve: "", ShortKey: "" };
}

function center() {
  const conv = openConv();
  const total = ts.Roster.length;
  const dot = !total ? "" : ts.Online === total ? "on" : ts.MajorityReachable ? "partial" : "off";
  const online = h("div", { class: "online-menu" },
    h("button", {
      class: `online ${ts.MajorityReachable ? "ok" : "bad"}`, type: "button", "aria-haspopup": "dialog",
      "aria-expanded": tui.roster ? "true" : "false", disabled: !total,
      onclick: () => { tui.roster = !tui.roster; draw(); },
    }, h("span", { class: `dot ${dot}` }), total ? `${ts.Online} of ${total} devices online` : "Connecting…"),
    tui.roster && roster());
  const key = ts.Key;
  const sign = key && h("span", { class: `chip ${key.CanSign ? "ok" : "bad"}` },
    key.CanSign ? "can sign" : `can't sign: ${key.Reachable} of ${key.T} needed online`);
  return h("section", { class: "chat", "aria-label": convName(conv) },
    h("header", { class: "chat-head" },
      h("h2", {}, convName(conv)), sign, h("span", { class: "spacer" }), online,
      h("button", { class: "btn small", type: "button", disabled: tui.busy || !!tui.clearing, onclick: askClear }, "Clear"),
      h("button", {
        class: "btn small about-toggle", type: "button", "aria-expanded": tui.about ? "true" : "false",
        onclick: () => { tui.about = !tui.about; draw(); },
      }, "About")),
    total > 0 && !ts.MajorityReachable && h("div", { class: "queue-banner", role: "status" },
      "Not enough devices are online for messages to be delivered. What you send waits here and goes once they are."),
    ts.Problem && h("div", { class: "banner", role: "alert" }, ts.Problem.Message),
    tui.clearing && clearConfirm(conv),
    transcript(),
    composer.el);
}

// askClear asks what Clear would destroy, to confirm it.
async function askClear() {
  const v = await call("ClearCheck");
  if (!v || v.Conversation !== ts.Open && !v.Problem) return;
  tui.clearing = v;
  draw();
}

// clearConfirm says what Clear destroys on this device, and what follows.
function clearConfirm(conv) {
  const v = tui.clearing;
  const cancel = h("button", { class: "btn small", type: "button", onclick: () => { tui.clearing = null; draw(); } }, v.Messages ? "Cancel" : "OK");
  if (v.Problem || !v.Messages) {
    return h("div", { class: "clear-confirm", role: "status" },
      h("p", {}, v.Problem ? v.Problem.Message : "There is nothing to clear here."), h("div", { class: "row" }, cancel));
  }
  const n = v.Messages === 1 ? "1 message" : `${v.Messages} messages`;
  return h("div", { class: "clear-confirm", role: "alertdialog", "aria-label": "Clear" },
    h("p", {}, h("strong", {}, `Clear ${n} from ${convName(conv)} on this device?`),
      " They are deleted here for good. The other devices keep their copies."),
    v.Unaccounted && h("p", { class: "warn" }, "This device's agents have no recap of this talk, so they forget it too."),
    v.NoPolicy && h("p", { class: "warn" }, "This Group deletes no talk on its own, so it can come back if this device restores talk from another device. Set \u201cDelete old talk after\u201d in Settings to keep it cleared."),
    h("div", { class: "row" },
      h("button", { class: "btn small danger", type: "button", disabled: tui.busy, onclick: async () => {
        const at = tui.clearing;
        tui.clearing = null;
        await act("ClearConversation", at.Conversation, at.Through);
      } }, "Clear"),
      cancel));
}

function roster() {
  return h("div", { class: "popover", role: "dialog", "aria-label": "Devices" },
    h("strong", {}, "Devices in this Group"),
    ts.Roster.map((r) => h("div", { class: "devrow" },
      h("span", { class: `dot ${r.Reachable ? "on" : "off"}` }),
      h("div", { class: "who" },
        h("strong", {}, r.Self ? `${r.Name || "Unnamed device"} (This Device)` : r.Name || "Unnamed device"),
        h("code", { title: r.ID }, `Device ID ${shortID(r.ID)}`)),
      h("span", { class: "state" }, r.Reachable ? "Online" : "Offline"))));
}

function transcript() {
  const items = ts.Items.map(item);
  const live = ts.Live.map(liveChain);
  const sending = tui.sending && tui.sending.conv === ts.Open && h("div", { class: "msg right sending" },
    h("div", { class: "meta" }, h("span", { class: "chip" }, "Sending…")),
    h("div", { class: "text" }, tui.sending.text));
  let empty = null;
  if (!items.length && !live.length && !sending) {
    empty = ts.Open === GROUP_TALK && ts.Conversations.length <= 1
      ? h("div", { class: "empty" },
        h("strong", {}, "Group talk"),
        h("p", {}, "There are no Keys in this Group yet. Make the first with + New Key."))
      : h("p", { class: "placeholder" }, "No messages here.");
  }
  return h("div", {
    class: "transcript", role: "log", "aria-live": "polite",
    onscroll: (e) => {
      const el = e.target;
      tui.scroll = { top: el.scrollTop, pinned: el.scrollHeight - el.scrollTop - el.clientHeight < 40 };
    },
  }, empty, items, live, sending);
}

function item(it) {
  if (it.Kind === "sign") return signCard(it, tui.busy);
  if (it.Kind === "system") {
    if (!it.Transcript) return h("div", { class: "sysline" }, h("span", {}, it.Text), h("time", {}, time(it.At)));
    // An agent's run that posted nothing: what it wrote is in its Transcript.
    const key = `run:${it.Chain}/${it.Handle}`;
    const open = tui.expanded.get(key);
    return h("div", { class: "sysrun" },
      h("div", { class: "sysline" }, h("span", {}, it.Text),
        h("button", {
          class: "link", type: "button", "aria-expanded": open ? "true" : "false",
          onclick: () => toggleTranscript(key, () => call("RunTranscript", tui.shownOpen, it.Chain, it.Handle)),
        }, open ? "Hide transcript" : "Transcript"),
        h("time", {}, time(it.At))),
      open && transcriptBlocks(open));
  }
  const who = it.Side === "left" && h("span", { class: "who" },
    h("strong", {}, it.From),
    it.Handle && h("span", { class: "handle" }, `@${it.Handle}`),
    it.Job && h("span", { class: "handle" }, "Job"));
  const meta = h("div", { class: "meta" }, who, h("time", {}, time(it.At)),
    it.Queued && h("span", { class: "chip warn" }, "Queued"));
  if (it.Kind === "unreadable") {
    return h("div", { class: `msg ${it.Side} unreadable` }, meta, h("p", { class: "text" }, "This message can't be read."));
  }
  const open = tui.expanded.get(it.MsgID);
  return h("div", { class: `msg ${it.Side}` },
    meta,
    h("div", { class: "text" }, it.Text),
    (it.Droppable || it.Transcript) && h("div", { class: "actions" },
      it.Droppable && h("button", { class: "link", type: "button", disabled: tui.busy, onclick: () => act("DropQueued", it.MsgID) }, "Drop"),
      it.Transcript && h("button", {
        class: "link", type: "button", "aria-expanded": open ? "true" : "false", onclick: () => toggleTranscript(it.MsgID, () => call("Transcript", it.MsgID)),
      }, open ? "Hide transcript" : "Transcript")),
    open && transcriptBlocks(open));
}

// toggleTranscript shows or hides the Transcript under key, read by fetch.
async function toggleTranscript(key, fetch) {
  if (tui.expanded.has(key)) {
    tui.expanded.delete(key);
    draw();
    return;
  }
  tui.expanded.set(key, {});
  draw();
  const v = await fetch();
  if (tui.expanded.has(key)) tui.expanded.set(key, v.Problem ? { problem: v.Problem } : { blocks: v.Blocks ?? [] });
  draw();
}

function transcriptBlocks(t) {
  if (t.problem) return h("div", { class: "blocks" }, h("p", { class: "error" }, t.problem.Message));
  if (!t.blocks) return h("div", { class: "blocks" }, h("p", { class: "placeholder" }, "Loading…"));
  return h("div", { class: "blocks" }, t.blocks.map(block));
}

// block is one Transcript block: thinking, text, a tool call or its result.
function block(raw) {
  let b = raw;
  if (typeof raw === "string") {
    try { b = JSON.parse(raw); } catch { b = { type: "text", text: raw }; }
  }
  switch (b?.type) {
    case "thinking":
      return h("div", { class: "blk thinking" }, h("span", { class: "kind" }, "Thinking"), h("div", {}, b.thinking ?? b.text ?? ""));
    case "text":
      return h("div", { class: "blk" }, h("div", {}, b.text ?? ""));
    case "tool_use":
      return h("div", { class: "blk tool" }, h("span", { class: "kind" }, `Tool call · ${b.name ?? ""}`), h("pre", {}, JSON.stringify(b.input ?? {}, null, 2)));
    case "tool_result": {
      const content = Array.isArray(b.content) ? b.content.map((c) => c.text ?? JSON.stringify(c)).join("\n") : String(b.content ?? "");
      return h("div", { class: `blk tool ${b.is_error ? "bad" : ""}` }, h("span", { class: "kind" }, b.is_error ? "Tool error" : "Tool result"), h("pre", {}, content));
    }
  }
  return h("div", { class: "blk tool" }, h("pre", {}, JSON.stringify(b, null, 2)));
}

function liveChain(c) {
  return h("div", { class: "live" },
    h("div", { class: "live-head" },
      h("span", { class: "pulse", "aria-hidden": "true" }),
      h("span", {}, c.Chain ? "Agents are answering…" : "Running…")),
    c.Agents.map((a) => h("div", { class: "msg left streaming" },
      h("div", { class: "meta" }, h("span", { class: "who" }, h("strong", {}, "This Device"), h("span", { class: "handle" }, `@${a.Handle}`))),
      a.Blocks.length
        ? a.Blocks.map((b) => h("div", { class: `blk ${b.Kind}` },
          b.Kind !== "text" && h("span", { class: "kind" }, b.Kind === "thinking" ? "Thinking" : "Tool"),
          h("div", {}, b.Text)))
        : h("p", { class: "placeholder" }, "Thinking…"))));
}

// The composer: @ autocompletes the Handles included here; \@ writes an @
// that summons nobody. Enter sends, Shift+Enter is a new line.

function makeComposer() {
  const textarea = h("textarea", { class: "field", rows: 2, placeholder: "Write a message — @ to summon an agent", "aria-label": "Message" });
  const list = h("ul", { class: "suggest", role: "listbox", hidden: true });
  const refusal = h("p", { class: "error refusal", role: "alert" });
  const plus = h("button", { class: "btn plus", type: "button", "aria-label": "New Sign Request", title: "New Sign Request", onclick: () => openSignForm() }, "+");
  const send = h("button", { class: "btn bright", type: "submit" }, "Send");
  // Stop cancels the chains of agents answering here; it sits by Send, not
  // in the output that grows above it.
  const stop = h("button", {
    class: "btn stop", type: "button", "aria-label": "Stop the agents answering", hidden: true,
    onclick: async () => {
      for (const c of ts.Live.filter((l) => l.Chain)) await act("CancelChain", c.Chain);
    },
  }, "Stop");
  let shown = []; // the Handles suggested
  let pick = 0;
  let refused = null; // the refusal shown, until the text changes

  // mentionAt is the @ being typed before the caret, as internal/mention
  // reads one: not after a word character, @ or \.
  const mentionAt = () => {
    const before = textarea.value.slice(0, textarea.selectionStart);
    const m = /(^|[^\p{L}\p{M}\p{N}@\\])@([A-Za-z0-9_-]*)$/u.exec(before);
    return m ? { start: before.length - m[2].length - 1, word: m[2].toLowerCase() } : null;
  };
  const suggest = () => {
    const at = ts ? mentionAt() : null;
    shown = at ? ts.Handles.filter((hd) => hd.startsWith(at.word)) : [];
    if (pick >= shown.length) pick = 0;
    list.hidden = shown.length === 0;
    list.replaceChildren(...shown.map((hd, i) => h("li", {
      role: "option", "aria-selected": i === pick ? "true" : "false",
      onmousedown: (e) => { e.preventDefault(); choose(hd); },
    }, `@${hd}`)));
  };
  const choose = (handle) => {
    const at = mentionAt();
    if (!at) return;
    const v = textarea.value;
    const end = textarea.selectionStart;
    textarea.value = `${v.slice(0, at.start)}@${handle} ${v.slice(end)}`;
    const caret = at.start + handle.length + 2;
    textarea.setSelectionRange(caret, caret);
    suggest();
  };
  const submit = async () => {
    const text = textarea.value.trim();
    if (!text || tui.busy) return;
    const conv = ts.Open;
    textarea.value = "";
    tui.drafts.delete(conv);
    list.hidden = true;
    tui.scroll.pinned = true;
    tui.sending = { conv, text };
    let next;
    try {
      next = await act("SendMessage", text);
    } finally {
      tui.sending = null;
    }
    refused = next?.Refused ?? null;
    // Refused: what was sent goes back to be fixed, unless more was typed.
    if (!next || next.Refused) {
      if (ts.Open !== conv) tui.drafts.set(conv, tui.drafts.get(conv) || text);
      else if (!textarea.value) textarea.value = text;
    }
    update();
    draw();
  };
  const update = () => {
    refusal.textContent = refused?.Message ?? "";
    refusal.hidden = !refused;
    textarea.setAttribute("aria-invalid", refused ? "true" : "false");
    send.disabled = tui.busy;
    stop.hidden = !ts?.Live?.some((c) => c.Chain);
    stop.disabled = tui.busy;
  };
  textarea.addEventListener("input", () => { refused = null; update(); suggest(); });
  textarea.addEventListener("click", suggest);
  textarea.addEventListener("keydown", (e) => {
    if (!list.hidden) {
      if (e.key === "ArrowDown" || e.key === "ArrowUp") {
        e.preventDefault();
        pick = (pick + (e.key === "ArrowDown" ? 1 : shown.length - 1)) % shown.length;
        suggest();
        return;
      }
      if (e.key === "Enter" || e.key === "Tab") {
        e.preventDefault();
        choose(shown[pick]);
        return;
      }
      if (e.key === "Escape") {
        list.hidden = true;
        return;
      }
    }
    if (e.key === "Enter" && !e.shiftKey && !e.isComposing) {
      e.preventDefault();
      submit();
    }
  });
  const el = h("form", { class: "composer", onsubmit: (e) => { e.preventDefault(); submit(); } },
    list, refusal, h("div", { class: "row" }, plus, textarea, stop, send));
  return {
    el, input: textarea, update,
    closeSuggestions: () => { list.hidden = true; refused = null; update(); },
  };
}

// The right column: what the open Conversation is about.

function right() {
  return h("aside", { class: "about", "aria-label": "About this conversation" },
    ts.Key ? keyDetails(ts.Key) : h("section", {},
      h("h3", {}, "Group talk"),
      h("p", { class: "hint" }, "Talk with everyone in the Group. Each Key has a conversation of its own.")),
    ts.Key && signRequests(),
    agents(),
    overrides());
}

function keyDetails(k) {
  const alias = tui.editAlias
    ? h("form", {
      class: "inline",
      onsubmit: async (e) => {
        e.preventDefault();
        const next = await act("SetKeyAlias", tui.form.keyAlias.trim());
        if (next && !next.Problem) { tui.editAlias = false; draw(); }
      },
    },
    input("keyAlias", { "aria-label": "Key name", "data-autofocus": true }),
    h("div", { class: "row" },
      h("button", { class: "btn small", type: "button", onclick: () => { tui.editAlias = false; draw(); } }, "Cancel"),
      h("button", { class: "btn small bright", type: "submit", disabled: tui.busy }, "Save")),
    h("p", { class: "hint" }, "Only this device sees this name."))
    : h("div", { class: "value" }, k.Alias || h("span", { class: "placeholder" }, "No name"),
      h("button", { class: "link", type: "button", onclick: () => { tui.form.keyAlias = k.Alias; tui.editAlias = true; draw(); } }, "Rename"));
  const row = (label, value) => h("div", { class: "kv" }, h("span", { class: "label" }, label), value);
  return h("section", {},
    h("h3", {}, "Key"),
    row("Name", alias),
    row("Curve", h("div", { class: "value" }, curveName(k.Curve))),
    row("Signers", h("div", { class: "value" }, `${k.T} of ${k.N} devices`)),
    row("Public key", h("code", { class: "full" }, k.PublicKey)),
    k.Address && row("Address", h("code", { class: "full" }, k.Address)),
    row("This device", h("div", { class: "value" }, k.Share ? "Holds a share" : "Holds no share")));
}

// signRequests lists this Key's Sign Requests, newest first; each scrolls
// to its card.
function signRequests() {
  const rows = [...ts.SignRequests].reverse();
  return h("section", {},
    h("h3", {}, "Sign Requests"),
    rows.length
      ? h("div", { class: "signrows" }, rows.map((r) => h("button", {
        class: "signrow", type: "button",
        onclick: () => {
          tui.about = false;
          draw();
          const el = mount.querySelector(`#card-${CSS.escape(r.MsgID)}`);
          if (el) showCard(el);
        },
      }, h("span", { class: "text" }, r.Reason || "No reason"), statusChip(r.Status))))
      : h("p", { class: "hint" }, "None yet. Propose one with + beside the message box."));
}

function agents() {
  const list = ts.Agents.length
    ? ts.Agents.map((a) => h("label", { class: "toggle" },
      h("input", { type: "checkbox", checked: a.Included, disabled: tui.busy, onchange: (e) => act("SetAgentIncluded", a.Handle, e.target.checked) }),
      h("span", { class: "text" }, h("strong", {}, `@${a.Handle}`)),
      h("button", { class: "link", type: "button", onclick: () => nav("agents", { profile: a.Handle }) }, "Profile")))
    : h("p", { class: "hint" }, "This device has no agents yet. Make one on the Agents page.");
  return h("section", {},
    h("h3", {}, "Agents here"),
    h("p", { class: "hint" }, "The agents of this device that answer when mentioned here."),
    list);
}

function overrides() {
  if (ts.Open === GROUP_TALK) return null;
  return h("section", {},
    h("h3", {}, "Overrides"),
    ts.Overrides.length
      ? ts.Overrides.map((o) => h("div", { class: "kv" },
        h("span", { class: "label" }, o.Kind === "provider" ? "Provider" : "Network"),
        h("div", { class: "value" }, h("strong", {}, o.Alias), h("span", { class: "hint" }, o.Detail),
          h("button", { class: "link", type: "button", onclick: () => nav(o.Kind === "provider" ? "agents" : "networks", { [o.Kind]: o.Alias, conversation: ts.Open }) }, "Edit"))))
      : h("p", { class: "hint" }, "Uses the Group's Providers and Networks."));
}

signInit({ ts: () => ts, act, draw, blocks: transcriptBlocks });

document.addEventListener("click", (e) => {
  if (!e.target.isConnected) return; // a click that redrew the page
  if (tui.roster && !e.target.closest(".online-menu")) { tui.roster = false; draw(); }
  if (tui.about && !e.target.closest(".about, .about-toggle")) { tui.about = false; draw(); }
});
document.addEventListener("keydown", (e) => {
  if (e.key === "Escape" && tui.roster) { tui.roster = false; draw(); } else if (e.key === "Escape" && tui.about) { tui.about = false; draw(); }
});
