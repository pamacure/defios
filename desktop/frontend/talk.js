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
import { openSignForm, signActions, signCard, signForm, signHead, signInit, signSections, statusChip } from "./sign.js";

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
  // The line selected, whose details replace the right column's: its
  // lineKey. The selection outlives leaving the page.
  selected: null,
  aboutBefore: false, // whether About showed the right column before a selection opened it
  details: null, // { key, view? }: the selected line's shell.Details, once read
  detailsSig: "", // what the details were read against (detailsSig)
  earlier: null, // { handle, runs? , problem? }: the selected run's agent's live Transcript, opened from "Earlier runs"
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
  if (streaming && patchLive()) return;
  if (tui.selected !== null && ts.Open === tui.shownOpen) {
    // A message dropped or pruned is no longer there to show.
    if (!selectedItem()) deselect({ redraw: false });
    else if (detailsSig() !== tui.detailsSig) loadDetails();
  }
  draw();
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
const addressLabel = (c) => (c === "eddsa" ? "Solana address" : "Ethereum address");
const convName = (c) => (c.ID === GROUP_TALK ? "Group" : c.Alias || `${curveName(c.Curve)} ${c.ShortKey}`);
const shortID = (id) => (id.length > 16 ? `${id.slice(0, 16)}…` : id);

// time says when a message was written: the time alone today, "Yesterday"
// or the weekday within the last week, and the date further back (with the
// year once it is not this year's).
function time(at) {
  const d = new Date(at);
  if (Number.isNaN(d.getTime()) || d.getFullYear() < 2000) return "";
  const clock = d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
  const now = new Date();
  const midnight = (x) => new Date(x.getFullYear(), x.getMonth(), x.getDate());
  const days = Math.round((midnight(now) - midnight(d)) / 86400000);
  if (days <= 0) return clock;
  if (days === 1) {
    const y = new Intl.RelativeTimeFormat([], { numeric: "auto" }).format(-1, "day");
    return `${y.charAt(0).toUpperCase()}${y.slice(1)} ${clock}`;
  }
  if (days < 7) return `${d.toLocaleDateString([], { weekday: "short" })} ${clock}`;
  const date = d.toLocaleDateString([], {
    day: "numeric",
    month: "short",
    ...(d.getFullYear() === now.getFullYear() ? {} : { year: "numeric" }),
  });
  return `${date} ${clock}`;
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
    if (tui.selected !== null) deselect({ redraw: false });
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

// focusCard selects the Sign Request ts.Focus names, once, as it was
// proposed here or a notification about it was clicked.
function focusCard() {
  if (!ts.Focus || ts.Focus === tui.focused) return;
  if (!signItem(ts.Focus)) return; // its card is not read yet
  tui.focused = ts.Focus;
  showSign(ts.Focus);
}

// signItem is Sign Request msgID's line in the open Conversation.
const signItem = (msgID) => ts.Items.find((it) => it.Kind === "sign" && it.MsgID === msgID);

// showSign selects Sign Request msgID, if its card is here: its details
// open, and its card scrolls into view.
function showSign(msgID) {
  const it = signItem(msgID);
  if (!it) return;
  tui.scroll.pinned = false;
  select(it);
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
  const sel = selectable(it);
  if (it.Kind === "sign") return signCard(it, tui.busy, sel);
  if (it.Kind === "system" || it.Kind === "job") {
    return h("div", { ...sel, class: `sysline ${sel.class}` }, h("span", {}, it.Text), h("time", {}, time(it.At)));
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
    return h("div", { ...sel, class: `msg ${it.Side} unreadable ${run} ${sel.class}` }, meta, beside(it, h("p", { class: "text" }, "This message can't be read.")));
  }
  // A run of this device's agent that posted nothing: its outcome, faded.
  if (it.Kind === "run") {
    return h("div", { ...sel, class: `msg ${it.Side} unposted ${run} ${sel.class}` }, meta, beside(it, h("p", { class: "text" }, it.Text)));
  }
  return h("div", { ...sel, class: `msg ${it.Side} ${run} ${sel.class}` },
    meta,
    beside(it, h("div", { class: "text" }, (it.Parts ?? [{ Text: it.Text }]).map(part))),
    it.Droppable && h("div", { class: "actions" },
      h("button", { class: "link", type: "button", disabled: tui.busy, onclick: () => act("DropQueued", it.MsgID) }, "Drop")));
}

// beside puts a left-side message's bubble beside its avatar column,
// bottoms aligned, so what follows the bubble (its actions) hangs below
// both: the avatar on the last message of a run, the column
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
  const textarea = h("textarea", { class: "field", rows: 1, placeholder: "Write a message — @ to summon an agent", "aria-label": "Message" });
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
    grow();
    fit();
  };
  // grow sizes the textarea to its text, from one line up to its
  // max-height; only past that does it scroll.
  const grow = () => {
    const cs = getComputedStyle(textarea);
    const max = parseFloat(cs.maxHeight);
    textarea.style.height = "auto";
    const want = textarea.scrollHeight + parseFloat(cs.borderTopWidth) + parseFloat(cs.borderBottomWidth);
    textarea.style.height = `${Math.min(want, max)}px`;
    textarea.style.overflowY = want > max ? "auto" : "hidden";
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
  // A change of width rewraps the text, so the height follows it.
  new ResizeObserver(() => { grow(); fit(); }).observe(textarea);
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
        e.stopPropagation(); // it closed the suggestions, nothing else
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

// Selecting a line: its details replace the right column's. Every line
// can be selected, a Sign Request's card too.

// lineKey names a line across redraws: its msg_id, a run's chain and
// step, a Job line's Job Run and text, or a system line's time and text.
const lineKey = (it) => {
  if (it.MsgID) return it.MsgID;
  if (it.Kind === "run") return `run:${it.Chain}/${it.Step}`;
  if (it.Kind === "job") return `job:${it.JobRun}|${it.Text}`;
  return `sys:${it.At}|${it.Text}`;
};

// selectable is a line's attributes for selecting it: a click selects it,
// or deselects it if it is the one selected, unless the click ends a text
// selection or lands on something of its own to click.
function selectable(it) {
  const key = lineKey(it);
  return {
    "data-sel": key,
    class: tui.selected === key ? "selected" : "",
    onclick: (e) => {
      if (e.target.closest("button, a, input, summary, .avatar")) return;
      const sel = window.getSelection();
      if (sel && !sel.isCollapsed && sel.containsNode(e.currentTarget, true)) return;
      if (tui.selected === key) deselect();
      else select(it);
    },
  };
}

// selectedItem is the line selected in the open Conversation, if it is
// still there.
function selectedItem() {
  if (tui.selected === null || !ts?.Items) return null;
  return ts.Items.find((it) => lineKey(it) === tui.selected) ?? null;
}

// narrow: the window is too narrow for the right column, which About
// opens over the transcript (style.css).
const narrow = () => window.matchMedia("(max-width: 999px)").matches;

// select selects it, opening the right column if About had hidden it.
function select(it) {
  if (tui.selected === null) tui.aboutBefore = tui.about;
  if (narrow()) tui.about = true;
  tui.selected = lineKey(it);
  tui.details = null;
  tui.earlier = null;
  loadDetails();
  draw();
  queueMicrotask(() => mount?.querySelector("[data-sel].selected")?.scrollIntoView({ block: "nearest" }));
}

// deselect brings back the right column's Conversation content, as shown
// or hidden before the selection.
function deselect({ redraw = true } = {}) {
  tui.selected = null;
  tui.details = null;
  tui.earlier = null;
  tui.about = tui.aboutBefore;
  if (redraw) draw();
}

// step moves the selection to the next selectable line by, 1 or -1.
function step(by) {
  const i = ts.Items.findIndex((it) => lineKey(it) === tui.selected);
  const next = ts.Items[i + by];
  if (i >= 0 && next) select(next);
}

// detailsSig is what the selected line's details depend on beyond the
// line itself: the talk said, the chains running, and the live
// Transcripts here. Details are read again when it changes.
function detailsSig() {
  return JSON.stringify([ts.Items.map((it) => [it.MsgID, it.Chain, it.Step, it.JobRun]), ts.Live.map((c) => c.Chain), ts.Transcripts.map((t) => t.Handle)]);
}

// loadDetails reads the selected line's details: a message's, the run of
// a run that posted nothing, or a Job line's Job Run; a system line has
// none to read.
async function loadDetails() {
  const it = selectedItem();
  if (!it) return;
  const key = tui.selected;
  tui.detailsSig = detailsSig();
  let read;
  if (it.Kind === "run") read = call("RunDetails", it.Chain, it.Step);
  else if (it.Kind === "job") read = call("JobRunDetails", it.JobRun);
  else if (it.Kind !== "system" && it.Kind !== "sign") read = call("MessageDetails", it.MsgID);
  else read = Promise.resolve({});
  const view = await read.catch((err) => ({ Problem: callFailed(err) }));
  if (tui.selected !== key) return;
  tui.details = { key, view };
  draw();
}

// loadEarlier reads the live Transcript of the selected run's agent.
async function loadEarlier(handle) {
  const key = tui.selected;
  tui.earlier = { handle };
  draw();
  const v = await call("EarlierRuns", handle).catch((err) => ({ Problem: callFailed(err) }));
  if (tui.selected !== key) return;
  tui.earlier = v.Problem ? { handle, problem: v.Problem } : { handle, runs: v.Runs ?? [] };
  draw();
}

// utc is the exact time at, in UTC.
function utc(at) {
  const d = new Date(at);
  if (Number.isNaN(d.getTime()) || d.getFullYear() < 2000) return "";
  return `${d.toISOString().slice(0, 19).replace("T", " ")} UTC`;
}

// copyText copies text, saying so for a moment in a small label over
// the button clicked, as copyable does.
async function copyText(e, text) {
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
}

const OUTCOME = {
  said: ["Posted", "ok"],
  silent: ["Stayed silent", ""],
  error: ["Failed", "bad"],
  cancelled: ["Cancelled", "bad"],
  skipped: ["Skipped", ""],
  running: ["Running…", "warn"],
  waiting: ["Not reached yet", ""],
  "cut off": ["Cut off", "warn"],
  "": ["Cut off", "warn"],
};
const outcomeChip = (o) => {
  const [label, cls] = OUTCOME[o] ?? [o, ""];
  return h("span", { class: `chip ${cls}` }, label);
};
const SIGN_TOOL = { sign_accept: "Accepted", sign_refuse: "Refused", sign_propose: "Proposed" };

// detailsPanel is the right column showing the selected line: who wrote
// it and when, what can be done with it, where it sits, the run that
// wrote it or the chain it started, and that run's turn and output. A
// section that does not apply is left out.
function detailsPanel(it) {
  const v = tui.details?.key === tui.selected ? tui.details.view : null;
  const run = v?.Run ?? null;
  const job = v?.Job ?? null;
  const system = it.Kind === "system" || it.Kind === "job";
  const name = system || it.From === "This Device" ? "This Device" : it.Side === "right" ? "You" : it.From;
  const row = (label, value) => value !== null && value !== undefined && value !== false && value !== "" &&
    h("div", { class: "kv" }, h("span", { class: "label" }, label), typeof value === "object" ? value : h("div", { class: "value" }, String(value)));

  const head = h("header", { class: "dhead" },
    avatar(it.Livery, { label: name }),
    h("div", { class: "who" },
      h("strong", {}, name),
      it.Handle && h("span", { class: `handle ${liveryClass(it.Livery)}` }, `@${it.Handle}`),
      it.Job && h("span", { class: "handle" }, "Job"),
      h("time", { datetime: it.At }, utc(it.At))),
    it.Kind === "message" && h("button", {
      class: "icon-btn copy", type: "button", "aria-label": "Copy text", title: "Copy text", onclick: (e) => copyText(e, it.Text),
    }, icon("copy")),
    h("button", { class: "icon-btn close", type: "button", "aria-label": "Close details", title: "Close (Esc)", onclick: () => deselect() }, "×"));

  const clearable = run && !run.Cleared && ts.Transcripts.some((t) => t.Handle === run.Handle);
  const copyJob = job && (job.Kind === "exec" ? ["Copy output", jobOutput(job)] : job.Kind === "webhook" ? ["Copy request/response", jobExchange(job)] : null);
  const actions = (it.Droppable || clearable || copyJob) && h("div", { class: "row" },
    copyJob && h("button", { class: "btn small", type: "button", onclick: (e) => copyText(e, copyJob[1]) }, copyJob[0]),
    it.Droppable && h("button", { class: "btn small", type: "button", disabled: tui.busy, onclick: () => act("DropQueued", it.MsgID) }, "Drop"),
    clearable && h("button", {
      class: "btn small", type: "button", disabled: tui.busy || !!tui.clearing,
      // The Clear above the transcript, for this agent alone.
      onclick: () => { tui.clearing = { Conversation: ts.Open, Agents: [run.Handle] }; draw(); },
    }, `Clear @${run.Handle}`));

  const device = v && v.DeviceID && h("div", { class: "value" },
    h("span", { class: `dot ${v.Online ? "on" : "off"}` }), v.Device, h("span", { class: "hint" }, v.Online ? "online" : "offline"));
  const details = v && !v.Problem && h("section", {},
    h("h3", {}, "Details"),
    row("Seq", v.Seq || (it.Queued ? "Queued" : null)),
    v.MsgID && row("Message ID", copyable(v.MsgID, "Message ID")),
    v.Chain && row("Chain", copyable(v.Chain, "chain")),
    row("Step", v.Step || null),
    row("Depth", v.Depth || v.Step ? String(v.Depth) : null),
    row("Device", device),
    v.DeviceID && row("Device ID", copyable(v.DeviceID, "Device ID")));

  let body = null;
  if (!v) body = h("p", { class: "placeholder" }, "Loading…");
  else if (v.Problem) body = problemBox(v.Problem);
  else if (run) body = runBody(run);
  else if (job) body = jobBody(job);
  else if (v.Steps?.length) body = chainSteps(v.Steps, v.Chain);
  else if (it.Job && it.From !== "This Device") body = h("p", { class: "hint" }, `Job, on ${it.From}`);
  else if (it.Job) body = h("p", { class: "hint" }, "This device no longer keeps the Job Run that wrote it.");

  return h("aside", { class: "about details", "aria-label": "Message details" },
    h("article", { class: "dcard" },
      // Who wrote it, when, and what can be done with it stay in view
      // as the rest scrolls under them.
      h("div", { class: "dtop" }, head, actions),
      details,
      job && jobFacts(job),
      run && runFacts(run),
      body,
      run && !run.Cleared && earlier(run)));
}

// signPanel is the right column showing the selected Sign Request: who
// proposed it and when, and, kept in view, what it asks and what can be
// done with it; then what it signs, each device's stance, its Broadcast
// Receipts, this device's Hooks on it, and its History.
function signPanel(it) {
  const c = it.Sign;
  const name = it.Side === "right" ? "You" : it.From;
  const head = h("header", { class: "dhead" },
    avatar(it.Livery, { label: name }),
    h("div", { class: "who" },
      h("strong", {}, name),
      it.Handle && h("span", { class: `handle ${liveryClass(it.Livery)}` }, `@${it.Handle}`),
      it.Job && h("span", { class: "handle" }, "Job"),
      h("time", { datetime: it.At }, utc(it.At))),
    h("button", { class: "icon-btn close", type: "button", "aria-label": "Close details", title: "Close (Esc)", onclick: () => deselect() }, "×"));
  // The run of this device's agent, or the Job Run of its exec Job, that
  // proposed it, as its line here.
  const k = c.Cause;
  const by = k && ts.Items.find((x) => (k.JobRun ? x.Kind === "job" && x.JobRun === k.JobRun
    : k.MsgID ? x.MsgID === k.MsgID : x.Kind === "run" && x.Chain === k.Chain && x.Step === k.Step));
  const cause = by && h("button", { class: "link", type: "button", onclick: () => select(by) },
    k.JobRun ? "Show the Job Run that proposed it" : `Show @${it.Handle}'s run that proposed it`);
  return h("aside", { class: "about details", "aria-label": "Sign Request details" },
    h("article", { class: "dcard signpanel" },
      h("div", { class: "dtop" }, head, signHead(c, cause), signActions(c, tui.busy)),
      signSections(c)));
}

const JOB_STATE = {
  ok: ["Finished", "ok"],
  failed: ["Failed", "bad"],
  running: ["Running…", "warn"],
  pending: ["Waiting", ""],
  not_run: ["Not run", ""],
};

// jobFacts is a Job Run's section: what fired it on what, how it ended
// and when, and its exit code or HTTP statuses.
function jobFacts(job) {
  const row = (label, value) => value && h("div", { class: "kv" }, h("span", { class: "label" }, label), h("div", { class: "value" }, value));
  const [label, cls] = JOB_STATE[job.State] ?? [job.State, ""];
  const on = job.Event === "schedule" ? "Its schedule" : job.Event;
  return h("section", {},
    h("h3", {}, `Job ${job.Job}`),
    h("div", { class: "row" }, h("span", { class: `chip ${cls}` }, label), h("span", { class: "chip" }, job.Kind)),
    job.Error && h("pre", { class: "error-text" }, job.Error),
    row("Trigger", job.ByHand ? "None: run by hand" : job.Trigger),
    row("Event", on && h("span", {}, on, job.EventMsgID && h("code", {}, ` ${shortID(job.EventMsgID)}`))),
    row("Fired", utc(job.Fired)),
    row("Started", utc(job.Started)),
    row("Ended", utc(job.Ended)),
    job.ExitCode !== null && job.ExitCode !== undefined && row("Exit code", String(job.ExitCode)),
    job.Attempts?.length > 0 && row("HTTP status", job.Attempts.map((a) => a.Status || "none").join(", ")),
    job.Signs?.length > 0 && row("Sign Requests", signRows(job.Signs)));
}

// jobBody is what a Job Run produced: an exec Job's command and output,
// or a webhook's request and last response. An agent Job's is its run.
function jobBody(job) {
  if (job.Kind === "exec") {
    return h("section", {},
      h("h3", {}, "Output"),
      job.Command?.length > 0 && h("pre", {}, job.Command.join(" ")),
      job.Stdout && h("pre", {}, job.Stdout),
      job.Stderr && h("pre", { class: "error-text" }, job.Stderr),
      !job.Stdout && !job.Stderr && h("p", { class: "hint" }, "It printed nothing."));
  }
  if (job.Kind === "webhook") {
    return h("section", {},
      h("h3", {}, "Request"),
      job.Method && h("pre", {}, `${job.Method} ${job.URL}`),
      (job.Attempts ?? []).map((a, i) => a.Error && h("pre", { class: "error-text" }, `Attempt ${i + 1}: ${a.Error}`)),
      h("h3", {}, "Response"),
      job.ContentType && h("p", { class: "hint" }, job.ContentType),
      job.Body ? h("pre", {}, job.Body) : h("p", { class: "hint" }, "No response body."));
  }
  return null;
}

// jobOutput is an exec Job Run's output, to copy: its stdout, then its
// stderr.
const jobOutput = (job) => [job.Stdout, job.Stderr].filter(Boolean).join(job.Stdout?.endsWith("\n") ? "" : "\n");

// jobExchange is a webhook Job Run's request and last response, to copy.
function jobExchange(job) {
  const statuses = (job.Attempts ?? []).map((a) => a.Status ? `HTTP ${a.Status}` : a.Error || "no response").join(", ");
  const lines = [`${job.Method} ${job.URL}`, "", statuses];
  if (job.ContentType) lines.push(`Content-Type: ${job.ContentType}`);
  lines.push("", job.Body ?? "");
  return lines.join("\n");
}

// runFacts is a run's section: how it ended, what it cost and ran on, the
// Sign Requests it acted on, and what was granted to it.
function runFacts(run) {
  const row = (label, value) => h("div", { class: "kv" }, h("span", { class: "label" }, label), value);
  const grants = [...(run.Tools ?? []), ...(run.Skills ?? [])];
  return h("section", {},
    h("h3", {}, "Run"),
    h("div", { class: "row" }, outcomeChip(run.Outcome), run.Cleared && h("span", { class: "chip" }, "From a cleared Transcript")),
    run.Error && h("pre", { class: "error-text" }, run.Error),
    row("Tokens", h("div", { class: "value" }, `${run.InputTokens.toLocaleString()} in · ${run.OutputTokens.toLocaleString()} out`)),
    run.Provider && row("Provider", h("div", { class: "value" }, h("strong", {}, run.Provider), run.ModelID && h("code", {}, run.ModelID), run.Shape && h("span", { class: "hint" }, run.Shape))),
    run.Signs?.length > 0 && row("Sign Requests", signRows(run.Signs)),
    grants.length > 0 && h("details", { class: "fold-in" },
      h("summary", {}, `Tools and Skills (${grants.length})`),
      run.Tools?.length > 0 && h("p", {}, h("span", { class: "label" }, "Tools "), run.Tools.join(", ")),
      run.Skills?.length > 0 && h("p", {}, h("span", { class: "label" }, "Skills "), run.Skills.join(", "))));
}

// signRows are the Sign Requests a run or Job Run acted on, each opening
// its details, marked with what it did.
function signRows(signs) {
  return h("div", { class: "signrows" }, signs.map((sg) => h("button", {
    class: "signrow", type: "button", onclick: () => showSign(sg.MsgID),
  }, h("span", { class: "text" }, sg.Reason || "No reason"), h("span", { class: "chip" }, SIGN_TOOL[sg.Tool] ?? sg.Tool))));
}

// runBody is only this run: its opening, headed by what summoned it, that
// in full and the rest it was given folded; then what it generated,
// headed by its Handle.
function runBody(run) {
  const o = run.Opening ?? {};
  const also = (o.Also ?? "").trim();
  return h("div", { class: "runbody" },
    h("section", {},
      h("h3", {}, o.Heading || "Its turn"),
      o.Summons ? h("pre", { class: "summons" }, o.Summons) : h("p", { class: "hint" }, "Nothing recorded summoned it."),
      also && h("details", { class: "fold-in" }, h("summary", {}, "Also given…"), h("pre", {}, also))),
    h("section", {},
      h("h3", {}, `@${run.Handle}`),
      run.Output?.length ? h("div", { class: "blocks" }, outputBlocks(run.Output)) : h("p", { class: "hint" }, "It generated nothing.")));
}

// outputBlocks are what a run generated, each tool result nested under
// its own call.
function outputBlocks(raws) {
  const parse = (raw) => {
    if (typeof raw !== "string") return raw;
    try { return JSON.parse(raw); } catch { return { type: "text", text: raw }; }
  };
  const bs = raws.map(parse);
  const calls = new Set(bs.filter((b) => b?.type === "tool_use" && b.id).map((b) => b.id));
  const results = new Map(bs.filter((b) => b?.type === "tool_result" && calls.has(b.tool_use_id)).map((b) => [b.tool_use_id, b]));
  return bs.filter((b) => !(b?.type === "tool_result" && results.get(b.tool_use_id) === b)).map((b) => {
    if (b?.type !== "tool_use") return block(b);
    const res = results.get(b.id);
    return h("div", { class: "blk tool call" },
      h("span", { class: "kind" }, `Tool call · ${b.name ?? ""}`),
      h("pre", {}, JSON.stringify(b.input ?? {}, null, 2)),
      res ? h("div", { class: "nested" }, block(res)) : null);
  });
}

// earlier is the "Earlier runs" link, and once followed, every run of the
// agent's live Transcript, each folded.
function earlier(run) {
  const e = tui.earlier?.handle === run.Handle ? tui.earlier : null;
  if (!e) return h("button", { class: "link", type: "button", onclick: () => loadEarlier(run.Handle) }, "Earlier runs");
  if (e.problem) return problemBox(e.problem);
  if (!e.runs) return h("p", { class: "placeholder" }, "Loading…");
  return h("section", {},
    h("h3", {}, `@${run.Handle}'s live Transcript here`),
    e.runs.length ? e.runs.map((r) => h("details", { class: "fold-in earlier" },
      h("summary", {}, `${r.Opening?.Heading || "Run"} · `, outcomeChip(r.Outcome)),
      runBody(r))) : h("p", { class: "hint" }, "It has no live Transcript here."));
}

// chainSteps is the chain this person's message started, step by step;
// each step that posted selects its message, and each that posted nothing
// its run.
function chainSteps(steps, chain) {
  return h("section", {},
    h("h3", {}, "Its chain"),
    h("ol", { class: "steps" }, steps.map((st) => {
      const said = st.MsgID
        ? ts.Items.find((it) => it.MsgID === st.MsgID)
        : ts.Items.find((it) => it.Kind === "run" && it.Chain === chain && it.Step === st.Step);
      const label = h("span", { class: "who" }, h("span", { class: "n" }, `${st.Step}.`), h("strong", {}, `@${st.Handle}`));
      return h("li", { style: `padding-left: ${(st.Depth - 1) * 14}px` },
        said
          ? h("button", { class: "step", type: "button", title: said.Kind === "run" ? "Show its run" : "Show what it posted", onclick: () => select(said) }, label, outcomeChip(st.State))
          : h("div", { class: "step" }, label, outcomeChip(st.State)),
        st.Error && h("pre", { class: "error-text" }, st.Error));
    })));
}

// The right column: what the open Conversation is about.

function right() {
  const it = selectedItem();
  if (it) return it.Kind === "sign" ? signPanel(it) : detailsPanel(it);
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
    k.Address && row(addressLabel(k.Curve), copyable(k.Address, "Address")),
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

// signRequests lists this Key's Sign Requests, newest first; each opens
// its details.
function signRequests() {
  const rows = [...ts.SignRequests].reverse();
  return fold("Sign Requests",
    rows.length
      ? h("div", { class: "signrows" }, rows.map((r) => h("button", {
        class: "signrow", type: "button", onclick: () => showSign(r.MsgID),
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

signInit({ ts: () => ts, act, draw, select: (it) => select(it), blocks: transcriptBlocks, convName, curveName });

document.addEventListener("click", (e) => {
  if (!e.target.isConnected) return; // a click that redrew the page
  if (tui.roster && !e.target.closest(".online-menu")) { tui.roster = false; draw(); }
  if (tui.about && !e.target.closest(".about, .about-toggle, .livery-pop, [data-sel]")) {
    // A click away from the selection's details closes them.
    if (tui.selected !== null) deselect();
    else { tui.about = false; draw(); }
  }
  if (tui.livery && !e.target.closest(".livery-pop")) closeLivery();
});
// Esc closes what is open, innermost first; ↑ and ↓ move the selection,
// unless focus is in a field or the details, which scroll.
document.addEventListener("keydown", (e) => {
  const here = mount?.isConnected;
  if (e.key === "Escape") {
    if (tui.livery) closeLivery();
    else if (tui.roster) { tui.roster = false; draw(); }
    else if (here && tui.clearing) { tui.clearing = null; draw(); }
    else if (here && tui.selected !== null) deselect();
    else if (tui.about) { tui.about = false; draw(); }
    return;
  }
  if ((e.key === "ArrowUp" || e.key === "ArrowDown") && here && tui.selected !== null &&
    !e.target.closest?.("input, textarea, select, [contenteditable], .about")) {
    e.preventDefault();
    step(e.key === "ArrowDown" ? 1 : -1);
  }
});
// The picker sits where its avatar was as it opened: scrolling, which
// moves the avatar, closes it.
document.addEventListener("wheel", (e) => {
  if (tui.livery && !e.target.closest?.(".livery-pop")) closeLivery();
}, { passive: true });
