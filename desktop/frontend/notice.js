// How the app shows a shell.Problem. A change refused, or anything else
// that happened once, is a toast: it shows in one place, above every page,
// goes after long enough to read it (longer while pointed at, focused or
// with its Details open), on its ×, or when the page it is about is left.
// The field a refusal names is outlined until it is edited. A refusal on
// a screen that is one field and little else, as the composer or the lock
// screen, is a problemLine beside it instead, there until the field is
// edited: a toast would take it away from where it is read. A state that
// lasts, such as a config.toml that does not load, is a problemBox in the
// page instead, there for as long as the state is. A Problem's Detail, the
// error as a library words it, shows only under Details.
import { h } from "./dom.js";

const region = h("div", { class: "toasts", "aria-label": "Notifications" });
document.body.append(region);

const MAX = 4;
const shown = []; // { el, scope, key, dismiss }

// showProblem shows problem as a toast. scope is the page it is about
// ("agents"), left by leavePage; null for one about no page. actions are
// buttons { label, run } that dismiss it, then run.
export function showProblem(problem, { scope = null, actions = [] } = {}) {
  if (!problem) return;
  const key = JSON.stringify([scope, problem.Code, problem.Message, problem.Detail ?? ""]);
  // The same refusal again restarts it rather than stacking a copy.
  shown.find((t) => t.key === key)?.dismiss(true);
  while (shown.length >= MAX) shown[0].dismiss(true);

  const total = Math.min(15000, Math.max(6000, 4000 + 60 * (problem.Message?.length ?? 0)));
  let left = total;
  let started = 0;
  let timer = null;
  const holds = new Set(); // why it is paused: "hover", "focus", "details"

  const bar = h("span", { class: "bar" });
  bar.style.animationDuration = `${total}ms`;
  const details = problem.Detail && h("details", {
    ontoggle: (e) => hold("details", e.target.open),
  }, h("summary", {}, "Details"), h("pre", {}, problem.Detail));
  const el = h("div", {
    class: "toast", role: "alert",
    onmouseenter: () => hold("hover", true), onmouseleave: () => hold("hover", false),
    onfocusin: () => hold("focus", true),
    onfocusout: (e) => { if (!el.contains(e.relatedTarget)) hold("focus", false); },
  },
  h("span", { class: "mark", "aria-hidden": "true" }, "!"),
  h("div", { class: "body" },
    h("p", {}, problem.Message),
    details,
    actions.length > 0 && h("div", { class: "row" }, actions.map((a) => h("button", {
      class: "btn small", type: "button", onclick: () => { t.dismiss(); a.run(); },
    }, a.label)))),
  h("button", { class: "dismiss", type: "button", "aria-label": "Dismiss", title: "Dismiss", onclick: () => t.dismiss() }, "×"),
  bar);

  const run = () => {
    started = Date.now();
    timer = setTimeout(() => t.dismiss(), left);
    el.classList.remove("held");
  };
  function hold(why, on) {
    const was = holds.size > 0;
    if (on) holds.add(why); else holds.delete(why);
    if (!was && holds.size > 0) {
      clearTimeout(timer);
      left = Math.max(1500, left - (Date.now() - started));
      el.classList.add("held");
    } else if (was && holds.size === 0) {
      run();
    }
  }
  const t = {
    el, scope, key,
    dismiss(now = false) {
      const i = shown.indexOf(t);
      if (i < 0) return;
      shown.splice(i, 1);
      clearTimeout(timer);
      if (now) { el.remove(); return; }
      el.classList.add("leaving");
      setTimeout(() => el.remove(), 180);
    },
  };
  shown.push(t);
  region.append(el);
  run();
}

// leavePage dismisses the toasts about a page other than page, as it is
// opened.
export function leavePage(page) {
  for (const t of [...shown]) if (t.scope && t.scope !== page) t.dismiss(true);
}

// dismissScope dismisses the toasts about scope, as when the Group a page
// edits changes under them.
export function dismissScope(scope) {
  for (const t of [...shown]) if (t.scope === scope) t.dismiss(true);
}

// problemBox is a state that lasts, in the page: its Message, its Detail
// under Details, and whatever else is given, such as a button to fix it.
export function problemBox(problem, ...extra) {
  if (!problem) return null;
  return h("div", { class: "problembox", role: "alert" },
    h("p", {}, problem.Message),
    problem.Detail && h("details", {}, h("summary", {}, "Details"), h("pre", {}, problem.Detail)),
    extra.length > 0 && h("div", { class: "row" }, extra));
}

// problemLine is a refusal shown inline, beside the one field it is about
// on a screen that is little else (the composer, the lock screen): its
// Message, and its Detail under Details. It goes as the field is edited.
export function problemLine(problem, attrs = {}) {
  if (!problem) return null;
  return h("div", { class: "problemline", role: "alert", ...attrs },
    h("p", {}, problem.Message),
    problem.Detail && h("details", {}, h("summary", {}, "Details"), h("pre", {}, problem.Detail)));
}

// markInvalid outlines input as the field a refusal names, until it is
// next edited; cleared, if given, then runs, so a redraw does not outline
// it again.
export function markInvalid(input, cleared = () => {}) {
  input.setAttribute("aria-invalid", "true");
  const clear = () => {
    input.removeAttribute("aria-invalid");
    input.removeEventListener("input", clear);
    input.removeEventListener("change", clear);
    cleared();
  };
  input.addEventListener("input", clear);
  input.addEventListener("change", clear);
  return input;
}

// callFailed is the Problem for a call to the Go side that threw rather
// than answering with a view: nothing the page can say more of.
export const callFailed = (err) => ({
  Code: "call-failed",
  Message: "Something went wrong, and DeFiOS could not do that.",
  Detail: String(err?.message ?? err ?? ""),
});
