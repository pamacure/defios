// The Talk page: the served Group's Conversations on the left, the open
// one's transcript and composer in the middle, and what it is about on the
// right. It renders the shell.TalkState the Go side reports (main.UI.Talk
// and the talk:state event), which it keeps current from the Node's events;
// it holds only what is being typed or is open on screen. UI text says
// "device" for a Node.
import { Clipboard } from "/wails/runtime.js";
import { call, h, icon, put } from "./dom.js";
import { avatar, brick, forgetPicks, liveryClass, liveryPicker } from "./livery.js";
import { mentionSpans } from "./mention.js";
import { callFailed, problemBox, problemLine, showProblem } from "./notice.js";
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
  readTo: new Map(), // Group/Conversation -> the seq last marked read there
  clearing: null, // the shell.ClearView Clear chooses from; with done, what it then cleared
  livery: null, // the Livery picker open from an avatar: { party, label, x, y }
  // The right column's sections folded into their headings: what is
  // about the Key starts open, the rest folded.
  folded: new Set(["Agents here", "Devices here", "Overrides"]),
};

// talkPage is the page, drawn into a new element; go opens another page.
export function talkPage(go) {
  nav = go;
  mount ??= h("div", { class: "talk" });
  draw();
  return mount;
}

// talkReady: Talk has the served Group's state, so the page can show
// whole.
export const talkReady = () => !!ts?.Group;

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

// act calls method and shows the TalkState it returns. Its Problem, on
// that TalkState only, is a toast; a Refused is the composer's to show.
async function act(method, ...args) {
  tui.busy = true;
  draw();
  try {
    const next = await call(method, ...args);
    talkUpdate(next);
    showProblem(next?.Problem, { scope: "talk" });
    return next;
  } catch (err) {
    showProblem(callFailed(err), { scope: "talk" });
    return null;
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
    tui.scroll = { top: 0, pinned: true }; // a Conversation opens at its end
    composer.closeSuggestions();
  }
  // A field being typed in is made anew: focus its successor.
  const active = document.activeElement;
  const field = mount.contains(active) ? active.dataset?.field : undefined;
  const caret = field ? [active.selectionStart, active.selectionEnd] : null;
  const focused = active === composer.input;
  mount.classList.toggle("about-open", tui.about);
  mount.replaceChildren(left(), center(), right(), signForm() ?? "", liveryPop() ?? "");
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
  queueMicrotask(markRead); // once the page is in the document
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

// markRead marks the open Conversation read to its last message, as its
// end is in view: the page on screen, the window focused, and the
// transcript scrolled to the end. What came while it was not stays unread.
function markRead() {
  if (!ts?.Group || !mount?.isConnected || !tui.scroll.pinned) return;
  if (document.visibilityState !== "visible" || !document.hasFocus()) return;
  const seq = ts.Items.reduce((m, it) => Math.max(m, it.Seq || 0), 0);
  const at = `${ts.Group}/${ts.Open}`;
  if (seq <= (tui.readTo.get(at) ?? 0)) return;
  tui.readTo.set(at, seq);
  // Failing leaves it unread here, to try again with the next message.
  call("ReadTo", ts.Open, seq).catch(() => tui.readTo.delete(at));
}
window.addEventListener("focus", markRead);
document.addEventListener("visibilitychange", markRead);

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
  marks(c)));
  return h("nav", { class: "convs", "aria-label": "Conversations" },
    h("div", { class: "list" }, rows, ts.Keygens.map(keygenRow)),
    newKey());
}

// marks are a row's badges, side by side: its open Sign Requests, what
// people said since it was last open, and a dot if agents or Jobs did.
function marks(c) {
  const signs = c.OpenSignRequests > 0 && h("span", { class: "badge sign", title: "Open Sign Requests" }, String(c.OpenSignRequests));
  const people = c.Unread > 0 && h("span", { class: "badge unread", title: "Unread messages" }, String(c.Unread));
  const auto = c.UnreadAutomated && h("span", { class: "dot-unread", title: "New from agents or Jobs", role: "img", "aria-label": "New from agents or Jobs" });
  if (!signs && !people && !auto) return null;
  return h("span", { class: "marks" }, signs, people, auto);
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
  // The dot and colour say whether messages can be delivered; the text counts devices.
  const dot = !total ? "" : !ts.MajorityReachable ? "off" : ts.Online === total ? "on" : "partial";
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
      transcriptSizes(),
      h("button", { class: "btn small", type: "button", disabled: tui.busy || !!tui.clearing, onclick: askClear }, "Clear"),
      h("button", {
        class: "btn small about-toggle", type: "button", "aria-expanded": tui.about ? "true" : "false",
        onclick: () => { tui.about = !tui.about; draw(); },
      }, "About")),
    total > 0 && !ts.MajorityReachable && h("div", { class: "queue-banner", role: "status" },
      "Not connected, so messages can't be delivered yet. What you send waits here and goes once connected."),
    ts.Unread && problemBox(ts.Unread),
    tui.clearing && clearConfirm(conv),
    transcript(),
    composer.el);
}

// transcriptSizes shows, beside Clear, how large each of this device's
// agents' live Transcripts here has grown: the input tokens of its last
// model call. A Transcript only grows, so this says when to clear it.
function transcriptSizes() {
  if (!ts.Transcripts?.length) return null;
  return h("span", { class: "transcript-sizes", title: "Each agent's Transcript here: the input tokens of its last model call. It only grows; Clear starts it afresh." },
    ts.Transcripts.map((s) => h("span", { class: "chip" }, `@${s.Handle} ${tokens(s.Tokens)}`)));
}

// tokens is a token count, short: 950, 12.4k, 1.2M tokens.
function tokens(n) {
  if (n < 1000) return `${n} tokens`;
  if (n < 1e6) return `${(n / 1000).toFixed(n < 1e4 ? 1 : 0)}k tokens`;
  return `${(n / 1e6).toFixed(1)}M tokens`;
}

// askClear asks which agents Clear can clear here, to choose among them.
async function askClear() {
  let v;
  try {
    v = await call("ClearCheck");
  } catch (err) {
    showProblem(callFailed(err), { scope: "talk" });
    return;
  }
  // Nothing to choose when it can't say whom Clear would clear.
  if (v?.Problem) { showProblem(v.Problem, { scope: "talk" }); return; }
  if (!v || v.Conversation !== ts.Open) return;
  tui.clearing = v;
  draw();
}

// clearConfirm asks which agent to clear, or all of them here, and says
// what clearing one does; once done, it says who was cleared.
function clearConfirm(conv) {
  const v = tui.clearing;
  const close = (label) => h("button", { class: "btn small", type: "button", onclick: () => { tui.clearing = null; draw(); } }, label);
  if (v.done || !v.Agents.length) {
    return h("div", { class: "clear-confirm", role: "status" },
      h("p", {}, v.done ?? "No agent has anything to clear here."), h("div", { class: "row" }, close("OK")));
  }
  // clear clears handle, or every agent here when it is "".
  const clear = (handle, label, done) => h("button", { class: "btn small", type: "button", disabled: tui.busy, onclick: async () => {
    const next = await act("ClearAgents", v.Conversation, handle);
    // Say so only where it was asked: another conversation may be open by now.
    tui.clearing = next && !next.Problem && ts.Open === v.Conversation ? { ...v, done } : null;
    draw();
  } }, label);
  return h("div", { class: "clear-confirm", role: "alertdialog", "aria-label": "Clear" },
    h("p", {}, h("strong", {}, `Clear which agent in ${convName(conv)}?`),
      " A cleared agent starts afresh at its next run here: it no longer remembers its earlier work in this conversation. That work is kept on this device, and the messages stay."),
    h("div", { class: "row" },
      v.Agents.map((a) => clear(a, `@${a}`, `Cleared @${a}.`)),
      v.Agents.length > 1 && clear("", "All agents here", "Cleared all agents here."),
      close("Cancel")));
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
      markRead();
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
  const left = it.Side === "left";
  // A run of one sender's messages names its author on the first only.
  const who = left && it.Author && h("span", { class: "who" },
    h("strong", {}, it.From),
    it.Handle && h("span", { class: `handle ${liveryClass(it.Livery)}` }, `@${it.Handle}`),
    it.Job && h("span", { class: "handle" }, "Job"));
  const run = left && !it.Author ? "cont" : "";
  const meta = h("div", { class: "meta" }, who, h("time", {}, time(it.At)),
    it.Queued && h("span", { class: "chip warn" }, "Queued"));
  if (it.Kind === "unreadable") {
    return h("div", { class: `msg ${it.Side} unreadable ${run}` }, meta, beside(it, h("p", { class: "text" }, "This message can't be read.")));
  }
  const open = tui.expanded.get(it.MsgID);
  return h("div", { class: `msg ${it.Side} ${run}` },
    meta,
    beside(it, h("div", { class: "text" }, (it.Parts ?? [{ Text: it.Text }]).map(part))),
    (it.Droppable || it.Transcript) && h("div", { class: "actions" },
      it.Droppable && h("button", { class: "link", type: "button", disabled: tui.busy, onclick: () => act("DropQueued", it.MsgID) }, "Drop"),
      it.Transcript && h("button", {
        class: "link", type: "button", "aria-expanded": open ? "true" : "false", onclick: () => toggleTranscript(it.MsgID, () => call("Transcript", it.MsgID)),
      }, open ? "Hide transcript" : "Transcript")),
    open && transcriptBlocks(open));
}

// beside puts a left-side message's bubble beside its avatar column,
// bottoms aligned, so what follows the bubble (its actions, a Transcript)
// hangs below both: the avatar on the last message of a run, the column
// kept empty on the others.
function beside(it, bubble) {
  if (it.Side !== "left") return bubble;
  const label = it.Handle ? (it.From === "This Device" ? `@${it.Handle}` : `@${it.Handle} on ${it.From}`) : it.From;
  return h("div", { class: "line" }, avatar(it.Livery, { shown: it.Avatar, open: it.Party && openLivery(it.Party, label), label }), bubble);
}

// part is a run of a sent message's text: a mention is a chip in the
// Livery of the agent it names on the device that sent it.
function part(p) {
  if (!p.Handle) return p.Text;
  return h("span", { class: `mention ${liveryClass(p.Livery)}` }, brick(p.Livery), p.Text);
}

// openLivery opens the Livery picker of party beside the avatar clicked.
const openLivery = (party, label) => (el) => {
  const r = el.getBoundingClientRect();
  tui.livery = { party, label, x: r.right + 8, y: r.top };
  draw();
};

// closeLivery closes the Livery picker, forgetting a Livery half chosen.
function closeLivery() {
  tui.livery = null;
  forgetPicks();
  draw();
}

// liveryPop is the Livery picker open from an avatar, for any party: a
// change applies at once, wherever the party is marked.
function liveryPop() {
  const l = tui.livery;
  if (!l) return null;
  const x = Math.max(8, Math.min(l.x, window.innerWidth - 310));
  const y = Math.max(8, Math.min(l.y, window.innerHeight - 190));
  return h("div", { class: "popover livery-pop", role: "dialog", "aria-label": `Livery of ${l.label}`, style: `left: ${x}px; top: ${y}px` },
    h("strong", {}, `Livery of ${l.label}`),
    liveryPicker({
      key: l.party, current: ts.Liveries?.[l.party] ?? "", held: ts.Liveries, disabled: tui.busy,
      onPick: (v) => act("SetLivery", l.party, v || "none"),
    }),
    h("p", { class: "hint" }, "Only this device sees it."));
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
  const v = await fetch().catch((err) => ({ Problem: callFailed(err) }));
  if (tui.expanded.has(key)) tui.expanded.set(key, v.Problem ? { problem: v.Problem } : { blocks: v.Blocks ?? [] });
  draw();
}

function transcriptBlocks(t) {
  // No Transcript to show is what this panel says, not an error to act on.
  if (t.problem) return h("div", { class: "blocks" }, h("p", { class: "placeholder" }, t.problem.Message));
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
      h("div", { class: "meta" }, h("span", { class: "who" }, h("strong", {}, "This Device"), h("span", { class: `handle ${liveryClass(a.Livery)}` }, `@${a.Handle}`))),
      h("div", { class: "line" }, avatar(a.Livery, { open: openLivery(a.Party, `@${a.Handle}`), label: `@${a.Handle}` }), h("div", { class: "body" },
        a.Blocks.length
          ? a.Blocks.map((b) => h("div", { class: `blk ${b.Kind}` },
            b.Kind !== "text" && h("span", { class: "kind" }, b.Kind === "thinking" ? "Thinking" : "Tool"),
            h("div", {}, b.Text)))
          : h("p", { class: "placeholder" }, "Thinking…"))))));
}

// The composer: @ autocompletes the Handles included here; \@ writes an @
// that summons nobody. Enter sends, Shift+Enter is a new line. A mirror
// behind the textarea, whose own text is transparent, marks each mention:
// an included Handle in a wash of its colour, any other underlined in red,
// as Send would refuse it.

function makeComposer() {
  const textarea = h("textarea", { class: "field", rows: 2, placeholder: "Write a message — @ to summon an agent", "aria-label": "Message" });
  const mirror = h("div", { class: "mirror", "aria-hidden": "true" });
  const list = h("ul", { class: "suggest", role: "listbox", hidden: true });
  const refusal = h("div", { class: "refusal" });
  const plus = h("button", { class: "btn plus", type: "button", "aria-label": "New Sign Request", title: "New Sign Request", onclick: () => openSignForm() }, icon("plus"));
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
    shown = at ? ts.Handles.filter((hd) => hd.Handle.startsWith(at.word)) : [];
    if (pick >= shown.length) pick = 0;
    list.hidden = shown.length === 0;
    list.replaceChildren(...shown.map((hd, i) => h("li", {
      role: "option", "aria-selected": i === pick ? "true" : "false", class: liveryClass(hd.Livery),
      onmousedown: (e) => { e.preventDefault(); choose(hd.Handle); },
    }, brick(hd.Livery) ?? h("span", { class: "brick none", "aria-hidden": "true" }), `@${hd.Handle}`)));
  };
  // paint writes the textarea's text into the mirror, marking mentions.
  const paint = () => {
    const text = textarea.value;
    const included = new Map((ts?.Handles ?? []).map((hd) => [hd.Handle, hd.Livery]));
    const out = [];
    let at = 0;
    for (const sp of mentionSpans(text)) {
      out.push(text.slice(at, sp.start));
      const cls = included.has(sp.handle) ? `on ${liveryClass(included.get(sp.handle))}` : "off";
      out.push(h("span", { class: cls }, text.slice(sp.start, sp.end)));
      at = sp.end;
    }
    // A last empty line has height in a textarea, not in a div.
    out.push(text.slice(at), text.endsWith("\n") ? " " : "");
    mirror.replaceChildren(...out.filter((c) => c !== ""));
    fit();
  };
  // fit lays the mirror exactly over the textarea's text: as wide as it
  // less any scrollbar, as tall, and scrolled as far.
  const fit = () => {
    const cs = getComputedStyle(textarea);
    mirror.style.width = `${textarea.clientWidth + parseFloat(cs.borderLeftWidth) + parseFloat(cs.borderRightWidth)}px`;
    mirror.style.height = `${textarea.offsetHeight}px`;
    mirror.scrollTop = textarea.scrollTop;
  };
  textarea.addEventListener("scroll", fit);
  new ResizeObserver(fit).observe(textarea);
  const choose = (handle) => {
    const at = mentionAt();
    if (!at) return;
    const v = textarea.value;
    const end = textarea.selectionStart;
    textarea.value = `${v.slice(0, at.start)}@${handle} ${v.slice(end)}`;
    const caret = at.start + handle.length + 2;
    textarea.setSelectionRange(caret, caret);
    paint();
    suggest();
  };
  const submit = async () => {
    const text = textarea.value.trim();
    if (!text || tui.busy) return;
    const conv = ts.Open;
    textarea.value = "";
    paint();
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
    paint();
    put(refusal, problemLine(refused));
    refusal.hidden = !refused;
    textarea.setAttribute("aria-invalid", refused ? "true" : "false");
    send.disabled = tui.busy;
    stop.hidden = !ts?.Live?.some((c) => c.Chain);
    stop.disabled = tui.busy;
    plus.hidden = !ts || ts.Open === GROUP_TALK;
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
        choose(shown[pick].Handle);
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
    list, refusal, h("div", { class: "row" }, plus, h("div", { class: "compose" }, mirror, textarea), stop, send));
  return {
    el, input: textarea, update,
    closeSuggestions: () => { list.hidden = true; refused = null; update(); },
  };
}

// The right column: what the open Conversation is about.

function right() {
  return h("aside", { class: "about", "aria-label": "About this conversation" },
    ts.Key ? keyDetails(ts.Key) : fold("Group talk",
      h("p", { class: "hint" }, "Talk with everyone in the Group. Each Key has a conversation of its own.")),
    ts.Key && signRequests(),
    agents(),
    devices(),
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
  return fold("Key",
    row("Name", alias),
    row("Curve", h("div", { class: "value" }, curveName(k.Curve))),
    row("Signers", h("div", { class: "value" }, `${k.T} of ${k.N} devices`)),
    row("Public key", copyable(k.PublicKey, "Public key")),
    k.Address && row("Address", copyable(k.Address, "Address")),
    row("This device", h("div", { class: "value" }, k.Share ? "Holds a share" : "Holds no share")));
}

// copyable is text shown whole that a click copies, saying so for a
// moment in a small label over it. The label is put in place, not drawn,
// so a redraw meanwhile only ends it early.
function copyable(text, what) {
  return h("button", {
    class: "copyable", type: "button", title: `Copy ${what}`,
    onclick: async (e) => {
      const el = e.currentTarget;
      try {
        await Clipboard.SetText(text);
      } catch (err) {
        showProblem(callFailed(err), { scope: "talk" });
        return;
      }
      el.querySelector(".copied")?.remove();
      const label = h("span", { class: "copied", role: "status" }, "Copied");
      el.append(label);
      setTimeout(() => label.remove(), 900);
    },
  }, h("code", { class: "full" }, text));
}

// fold is a right-column section headed title, which its heading folds
// away to only the heading, and back.
function fold(title, ...body) {
  const folded = tui.folded.has(title);
  return h("section", { class: folded ? "folded" : "" },
    h("h3", {}, h("button", {
      class: "fold", type: "button", "aria-expanded": String(!folded),
      onclick: () => { if (folded) tui.folded.delete(title); else tui.folded.add(title); draw(); },
    }, title)),
    !folded && body);
}

// signRequests lists this Key's Sign Requests, newest first; each scrolls
// to its card.
function signRequests() {
  const rows = [...ts.SignRequests].reverse();
  return fold("Sign Requests",
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
    ? ts.Agents.map((a) => h("div", {
      class: "agentrow",
      // The whole row opens the agent's Profile, but for its checkbox and
      // its avatar, which act on their own.
      onclick: (e) => { if (!e.target.closest("input, .avatar")) nav("agents", { profile: a.Handle }); },
    },
    h("input", {
      type: "checkbox", checked: a.Included, disabled: tui.busy, "aria-label": `@${a.Handle} answers here`,
      onchange: (e) => act("SetAgentIncluded", a.Handle, e.target.checked),
    }),
    avatar(a.Livery, { open: openLivery(a.Party, `@${a.Handle}`), label: `@${a.Handle}` }),
    h("button", { class: "handle", type: "button", title: `Open the Profile of @${a.Handle}` }, `@${a.Handle}`)))
    : h("p", { class: "hint" }, "This device has no agents yet. Make one on the Agents page.");
  return fold("Agents here",
    h("p", { class: "hint" }, "The agents of this device that answer when mentioned here."),
    list);
}

// devices is Devices here: the Roster, and under each other device the
// agents of it seen writing in this Conversation, each with its avatar.
function devices() {
  const party = (p, label, ...rest) => h("div", { class: "party" },
    p.Party ? avatar(p.Livery, { open: openLivery(p.Party, label), label }) : avatar(""), ...rest);
  return fold("Devices here",
    ts.Devices.length
      ? h("div", { class: "parties" }, ts.Devices.map((d) => h("div", { class: "device" },
        party(d, d.Name, h("strong", {}, d.Name)),
        d.Agents.map((a) => party(a, `@${a.Handle} on ${d.Name}`, h("span", { class: `handle ${liveryClass(a.Livery)}` }, `@${a.Handle}`))))))
      : h("p", { class: "hint" }, "Connecting…"),
    h("p", { class: "hint" }, "Click an avatar to change the Livery this device marks it with."));
}

function overrides() {
  if (ts.Open === GROUP_TALK) return null;
  return fold("Overrides",
    ts.Overrides.length
      ? ts.Overrides.map((o) => h("div", { class: "kv" },
        h("span", { class: "label" }, o.Kind === "provider" ? "Provider" : "Network"),
        h("div", { class: "value" }, h("strong", {}, o.Alias), h("span", { class: "hint" }, o.Detail),
          h("button", { class: "link", type: "button", onclick: () => nav(o.Kind === "provider" ? "agents" : "networks", { [o.Kind]: o.Alias, conversation: ts.Open }) }, "Edit"))))
      : h("p", { class: "hint" }, "Uses the Group's Providers and Networks."));
}

signInit({ ts: () => ts, act, draw, blocks: transcriptBlocks, convName, curveName });

document.addEventListener("click", (e) => {
  if (!e.target.isConnected) return; // a click that redrew the page
  if (tui.roster && !e.target.closest(".online-menu")) { tui.roster = false; draw(); }
  if (tui.about && !e.target.closest(".about, .about-toggle, .livery-pop")) { tui.about = false; draw(); }
  if (tui.livery && !e.target.closest(".livery-pop")) closeLivery();
});
document.addEventListener("keydown", (e) => {
  if (e.key === "Escape" && tui.livery) { closeLivery(); } else if (e.key === "Escape" && tui.roster) { tui.roster = false; draw(); } else if (e.key === "Escape" && tui.about) { tui.about = false; draw(); }
});
// The picker sits where its avatar was as it opened: scrolling, which
// moves the avatar, closes it.
document.addEventListener("wheel", (e) => {
  if (tui.livery && !e.target.closest?.(".livery-pop")) closeLivery();
}, { passive: true });
