// The shared Secret field: any field that takes a Secret of the
// edit-target Group. It offers the Group's Secret names, "New Secret…",
// which keeps a value in this device's Vault and selects it, and a typed
// name for a Secret the environment supplies. A fixed field is one whose
// name is given, such as a variable an MCP Server asks for: it shows
// where that Secret comes from and sets its value. Values are never read
// back. The Go side is main.UI.Secrets and main.UI.NewSecret.
import { call, h, put } from "./dom.js";

let listed = null; // { group, names: Promise<SecretName[]> }

// secrets is the edit-target Group's Secrets, fetched once per Group until
// a new one is set.
function secrets(group) {
  if (listed?.group !== group) {
    listed = { group, names: call("Secrets").then((v) => (v?.Problem ? [] : v?.Secrets ?? [])) };
  }
  return listed.names;
}

const SOURCE = { env: "From the environment", vault: "In the Vault", none: "Not set" };
const NEW = "\u0000new";
const OTHER = "\u0000other";

// newSecret is the inline "New Secret…" form; fixed names the Secret, so
// only its value is asked for. done gets the name set, or null on cancel.
function newSecret(group, fixed, done) {
  const name = h("input", { class: "field left", placeholder: "NAME_OF_SECRET", autocomplete: "off", value: fixed ?? "", "aria-label": "Secret name" });
  const value = h("input", { class: "field left", type: "password", placeholder: "Value", autocomplete: "off", "aria-label": "Secret value" });
  const err = h("p", { class: "error", role: "alert" });
  const save = h("button", { class: "btn primary", type: "button" }, "Save Secret");
  save.addEventListener("click", async () => {
    save.disabled = true;
    try {
      const v = await call("NewSecret", name.value.trim(), value.value);
      if (!v || v.Problem) {
        err.textContent = v?.Problem?.Message ?? "";
        (v?.Field === "name" ? name : value).setAttribute("aria-invalid", "true");
        return;
      }
      listed = { group, names: Promise.resolve(v.Secrets ?? []) };
      done(v.Set);
    } finally {
      save.disabled = false;
    }
  });
  const box = h("div", { class: "newsecret" },
    !fixed && name, value,
    h("p", { class: "hint" }, "Kept in this device's Vault for this Group. It is never shown again here."),
    err,
    h("div", { class: "row" }, save, h("button", { class: "btn", type: "button", onclick: () => done(null) }, "Cancel")));
  setTimeout(() => (fixed ? value : name).focus());
  return box;
}

// secretField is a field taking a Secret of group. value is the name
// chosen, "" for none; onChange gets each new name. With fixed, the name
// is that and only its value can be set. It returns { el, value }.
export function secretField({ group, id, value = "", fixed = null, noneLabel = "None", onChange = () => {} }) {
  const el = h("div", { class: "secretfield" });
  const field = { el, value: fixed ?? value };
  const set = (name) => {
    field.value = name;
    onChange(name);
  };

  async function draw(mode = "") {
    const names = await secrets(group);
    if (fixed) {
      const s = names.find((x) => x.Name === fixed);
      const source = s?.Source ?? "none";
      put(el,
        h("div", { class: "secretrow" },
          h("code", {}, `$${fixed}`),
          h("span", { class: `chip ${source === "none" ? "warn" : "ok"}` }, SOURCE[source]),
          // The environment wins over the Vault, so a value set here would not be used.
          mode !== "new" && source !== "env" && h("button", { class: "link", type: "button", onclick: () => draw("new") }, source === "vault" ? "Replace value…" : "Set value…")),
        mode === "new" && newSecret(group, fixed, () => draw()));
      return;
    }
    const select = h("select", { id, class: "field left secretselect" },
      h("option", { value: "" }, noneLabel),
      names.map((s) => h("option", { value: s.Name },
        s.Source === "vault" ? s.Name : `${s.Name} (${SOURCE[s.Source].toLowerCase()})`)),
      field.value && !names.some((s) => s.Name === field.value) && h("option", { value: field.value }, `${field.value} (from the environment)`),
      h("option", { value: OTHER }, "Another name, from the environment…"),
      h("option", { value: NEW }, "New Secret…"));
    select.value = mode === "other" ? OTHER : mode === "new" ? NEW : field.value;
    select.addEventListener("change", () => {
      if (select.value === NEW) draw("new");
      else if (select.value === OTHER) draw("other");
      else { set(select.value); draw(); }
    });
    let extra = null;
    if (mode === "new") {
      extra = newSecret(group, null, (name) => { if (name) set(name); draw(); });
    } else if (mode === "other") {
      const typed = h("input", { class: "field left", placeholder: "NAME_IN_THE_ENVIRONMENT", autocomplete: "off", "aria-label": "Environment variable name" });
      const use = () => { const n = typed.value.trim(); if (n) set(n); draw(); };
      typed.addEventListener("keydown", (e) => { if (e.key === "Enter") { e.preventDefault(); use(); } });
      extra = h("div", { class: "newsecret" }, typed,
        h("p", { class: "hint" }, "A Secret this device's environment supplies, by its variable name."),
        h("div", { class: "row" }, h("button", { class: "btn primary", type: "button", onclick: use }, "Use"),
          h("button", { class: "btn", type: "button", onclick: () => draw() }, "Cancel")));
      setTimeout(() => typed.focus());
    }
    put(el, select, extra);
  }
  draw();
  return field;
}
