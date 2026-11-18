// Liveries (ADR 0037): the colour and brick this device marks a party of
// talk with, written <colour>-<brick>, "" for none. Shared by Talk, which
// draws avatars and sets any party's Livery from them, and the Agents page,
// which picks a profile's.
import { h, put } from "./dom.js";

// COLOURS and BRICKS are internal/node's LiveryColours and LiveryBricks,
// in the order the picker shows them.
const COLOURS = ["blue", "indigo", "violet", "magenta", "orange", "cyan", "slate", "brown"];
const BRICKS = ["stud", "twin", "plate", "tile", "slope", "arch", "round", "corner"];

const split = (l) => (l ? [l.slice(0, l.indexOf("-")), l.slice(l.indexOf("-") + 1)] : ["", ""]);

export const liveryClass = (l) => (l ? `lv-${split(l)[0]}` : "");

// brick is the Livery's brick in currentColor; null for none.
export function brick(l) {
  return l ? h("span", { class: `brick brick-${split(l)[1]}`, "aria-hidden": "true" }) : null;
}

// avatar is a party's square: its colour with its brick cut out, plain
// gray for none; hidden, it only keeps the column. With open, it is a
// button that opens the party's picker.
export function avatar(l, { shown = true, open = null, label = "" } = {}) {
  const cls = `avatar ${liveryClass(l)} ${shown ? "" : "blank"}`;
  if (!shown || !open) return h("span", { class: cls, "aria-hidden": "true" }, brick(l));
  return h("button", {
    class: cls, type: "button", title: `${label}: change Livery`, "aria-label": `Livery of ${label}`,
    onclick: (e) => { e.stopPropagation(); open(e.currentTarget); },
  }, brick(l));
}

// partial is what each picker has chosen toward a Livery not yet whole, a
// colour with no brick or a brick with no colour, by its key: kept apart
// from the picker so a redraw of the page keeps it.
const partial = new Map();

// forgetPicks drops what pickers had chosen toward a Livery not yet
// whole, as the picker closes.
export const forgetPicks = () => partial.clear();

// liveryPicker picks the Livery of the party key, whose Livery is current.
// held is every Livery given, by party key: each row greys out what
// would form a Livery another party holds, given what the other row has
// selected, and strikes through a colour whose 8 bricks are all held.
// onPick is called with each whole Livery picked, or "" for None.
export function liveryPicker({ key, current, held, onPick, disabled = false }) {
  const taken = new Set(Object.entries(held ?? {}).filter(([p]) => p !== key).map(([, l]) => l));
  const free = (c, b) => !taken.has(`${c}-${b}`);
  const el = h("div", { class: "picker" });
  const pick = (l) => {
    partial.delete(key);
    if (l !== current) {
      current = l;
      onPick(l);
    }
    draw();
  };
  const draw = () => {
    const [c0, b0] = split(current);
    const sel = partial.get(key) ?? { colour: c0, brick: b0 };
    const choose = (next) => {
      if (next.colour && next.brick) {
        pick(`${next.colour}-${next.brick}`);
        return;
      }
      partial.set(key, next);
      draw();
    };
    const colours = COLOURS.map((c) => {
      const full = BRICKS.every((b) => !free(c, b));
      return h("button", {
        type: "button", class: `swatch lv-${c} ${full ? "full" : ""}`, title: full ? `${c}: every brick is taken` : c, "aria-label": c,
        "aria-pressed": sel.colour === c ? "true" : "false",
        disabled: disabled || full || (!!sel.brick && !free(c, sel.brick)),
        onclick: () => choose({ ...sel, colour: c }),
      });
    });
    const bricks = BRICKS.map((b) => h("button", {
      type: "button", class: `bpick ${sel.colour ? `lv-${sel.colour}` : ""}`, title: b, "aria-label": b,
      "aria-pressed": sel.brick === b ? "true" : "false",
      disabled: disabled || (!!sel.colour && !free(sel.colour, b)),
      onclick: () => choose({ ...sel, brick: b }),
    }, h("span", { class: `brick brick-${b}`, "aria-hidden": "true" })));
    const pool = COLOURS.flatMap((c) => BRICKS.filter((b) => free(c, b)).map((b) => `${c}-${b}`)).filter((l) => l !== current);
    put(el,
      h("div", { class: "prow", role: "group", "aria-label": "Colour" }, colours),
      h("div", { class: "prow", role: "group", "aria-label": "Brick" }, bricks),
      h("div", { class: "pacts" },
        h("button", { class: "btn small", type: "button", disabled: disabled || !pool.length, title: pool.length ? "A random Livery no other party holds" : "Every Livery is taken",
          onclick: () => pick(pool[Math.floor(Math.random() * pool.length)]) }, "Shuffle"),
        h("button", { class: "btn small", type: "button", disabled: disabled || (!current && !partial.has(key)), onclick: () => pick("") }, "None")));
  };
  draw();
  return el;
}

// randomFree is a random Livery of free, "" when none is.
export const randomFree = (free) => (free?.length ? free[Math.floor(Math.random() * free.length)] : "");
