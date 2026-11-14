"use strict";

// ---- helpers ---------------------------------------------------------

let csrf = "";

class APIError extends Error {
  constructor(status, message) { super(message); this.status = status; }
}

async function api(method, path, body) {
  const headers = { "Accept": "application/json" };
  if (body !== undefined) headers["Content-Type"] = "application/json";
  if (method !== "GET") headers["X-CSRF-Token"] = csrf;
  const resp = await fetch("/admin/api/" + path, {
    method, headers, credentials: "same-origin",
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const data = await resp.json().catch(() => ({}));
  if (resp.status === 401 && path !== "login") {
    showLogin();
    throw new APIError(401, "signed out");
  }
  if (!resp.ok) throw new APIError(resp.status, data.error || resp.statusText);
  return data;
}

// h builds an element. props: class, text, on<event>, dataset-free attrs;
// children are nodes or strings (always text, never HTML).
function h(tag, props, ...children) {
  const node = document.createElement(tag);
  for (const [key, value] of Object.entries(props || {})) {
    if (value === undefined || value === null || value === false) continue;
    if (key === "class") node.className = value;
    else if (key === "text") node.textContent = value;
    else if (key.startsWith("on")) node.addEventListener(key.slice(2), value);
    else if (key === "value") node.value = value;
    // CSSOM, not the style attribute: the CSP forbids inline styles.
    else if (key === "style") node.style.cssText = value;
    else node.setAttribute(key, value === true ? "" : value);
  }
  for (const child of children.flat(Infinity)) {
    if (child === null || child === undefined || child === false) continue;
    node.append(child instanceof Node ? child : String(child));
  }
  return node;
}

let toastTimer;
function toast(message, bad) {
  const box = document.getElementById("toast");
  box.textContent = message;
  box.className = "show" + (bad ? " bad" : "");
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { box.className = ""; }, bad ? 6000 : 2500);
}

// guarded runs an async action, reporting failures as a toast.
async function guarded(action, button) {
  if (button) button.disabled = true;
  try { return await action(); }
  catch (err) { if (err.status !== 401) toast(err.message, true); }
  finally { if (button) button.disabled = false; }
}

// term shows a word with its meaning on hover.
const term = (text, hint) => hint ? h("span", { title: hint }, text) : text;

// stack shows a main value over smaller secondary lines; empty lines are
// left out.
function stack(main, ...lines) {
  return h("div", {}, h("div", {}, main),
    lines.filter(l => l !== null && l !== undefined && l !== "").map(l => h("div", { class: "sub" }, l)));
}

// joined puts the non-empty parts on one line, separated by " · ".
const joined = (...parts) => parts.filter(Boolean).join(" · ");

// A key's hash (hex SHA-256 of the key) identifies it; the UI shows a
// prefix, like git's short hashes, and the whole hash on hover.
const SHORT_HASH = 8;
const shortHash = hash => hash.slice(0, SHORT_HASH);
const hashText = hash => h("span", { class: "mono muted", title: hash }, shortHash(hash));
// keyName is the key's name and short hash; text-only places (menus,
// chart tips) use keyText.
const keyName = (name, hash) => h("span", {}, name ? name + " " : "", hashText(hash));
const keyText = (name, hash) => name ? `${name} (${shortHash(hash)})` : shortHash(hash);

// table renders rows; a column's hint explains its title on hover.
function table(columns, rows, rowClass) {
  return h("div", { class: "table-wrap" }, h("table", {},
    h("thead", {}, h("tr", {}, columns.map(c => h("th", { class: c.num ? "num" : "" }, term(c.title, c.hint))))),
    h("tbody", {}, rows.length === 0
      ? h("tr", {}, h("td", { colspan: columns.length, class: "muted" }, t("common.nothing_yet")))
      : rows.map(row => h("tr", { class: rowClass ? rowClass(row) : "" },
          columns.map(c => h("td", { class: c.num ? "num" : "" }, c.cell(row))))))));
}

// ---- shell -----------------------------------------------------------

const views = {};
let cleanup = null;

function showLogin() {
  stopView();
  document.getElementById("app").hidden = true;
  document.getElementById("login").hidden = false;
  document.getElementById("login-password").focus();
}

function stopView() {
  if (cleanup) cleanup();
  cleanup = null;
}

function route() {
  stopView();
  const name = (location.hash || "#accounts").slice(1);
  const view = views[name] ? name : "accounts";
  for (const a of document.querySelectorAll("#tabs a")) a.classList.toggle("active", a.getAttribute("href") === "#" + view);
  const container = document.getElementById("view");
  container.replaceChildren();
  cleanup = views[view](container) || null;
}

async function start() {
  try {
    const session = await api("GET", "session");
    csrf = session.csrf;
  } catch (err) {
    if (err.status !== 401) toast(err.message, true);
    return;
  }
  document.getElementById("login").hidden = true;
  document.getElementById("app").hidden = false;
  route();
}

// boot loads the UI text before anything is shown; without it the page
// cannot render, so the failure replaces the page.
async function boot() {
  try {
    await i18n.load();
  } catch (err) {
    document.body.textContent = `agyrelay: could not load the UI text (${err.message})`;
    return;
  }
  i18n.apply(document);
  start();
}

document.getElementById("login-form").addEventListener("submit", async event => {
  event.preventDefault();
  const input = document.getElementById("login-password");
  const error = document.getElementById("login-error");
  error.textContent = "";
  try {
    const data = await api("POST", "login", { password: input.value });
    csrf = data.csrf;
    input.value = "";
    start();
  } catch (err) {
    error.textContent = err.message;
  }
});

document.getElementById("logout").addEventListener("click", () => guarded(async () => {
  await api("POST", "logout");
  csrf = "";
  showLogin();
}));

window.addEventListener("hashchange", route);
window.addEventListener("DOMContentLoaded", boot);

// polling refreshes a view every interval while it is shown.
function polling(container, interval, render) {
  let timer = null;
  let stopped = false;
  const tick = async () => {
    await guarded(render);
    if (!stopped) timer = setTimeout(tick, interval);
  };
  tick();
  return () => { stopped = true; clearTimeout(timer); };
}

// ---- keys ------------------------------------------------------------

const groupName = g => lookup("group", g) || g;


// limitEditor picks a key's spend limit in list-price USD: none, one
// total, or one per quota group (a blank group is unlimited).
function limitEditor(groups, k) {
  const usd = value => h("input", { type: "number", min: 0, step: "any", placeholder: t("keys.limit.placeholder"), style: "width:110px",
    value: value === undefined || value === null ? "" : String(value) });
  const workers = h("input", { type: "number", min: 1, max: 1024, step: 1, placeholder: t("keys.limit.placeholder"), style: "width:110px",
    value: k.max_workers === undefined || k.max_workers === null ? "" : String(k.max_workers) });
  const total = usd(k.limit_total);
  const perGroup = Object.fromEntries(groups.map(g => [g, usd(k.limits[g])]));
  const initial = k.limit_total !== null ? "total" : Object.keys(k.limits).length ? "group" : "none";
  const fields = h("div", { class: "row" });
  const show = mode => {
    total.required = mode === "total";
    fields.replaceChildren(...(mode === "total" ? [h("label", {}, t("keys.limit.total"), total)]
      : mode === "group" ? groups.map(g => h("label", {}, t("keys.limit.label", { group: groupName(g) }), perGroup[g])) : []));
  };
  const mode = h("select", { onchange: e => show(e.target.value) },
    [["none", t("keys.limit.mode.none")], ["total", t("keys.limit.mode.total")], ["group", t("keys.limit.mode.group")]]
      .map(([m, label]) => h("option", { value: m, selected: m === initial }, label)));
  show(initial);
  const groupLimits = () => Object.fromEntries(groups.filter(g => perGroup[g].value.trim() !== "").map(g => [g, Number(perGroup[g].value)]));
  return {
    nodes: [h("label", {}, t("keys.workers"), workers), h("label", {}, t("keys.limit.mode"), mode), fields],
    read: () => ({
      max_workers: workers.value.trim() !== "" ? Number(workers.value) : null,
      limit_total: mode.value === "total" ? Number(total.value) : null,
      limits: mode.value === "group" ? groupLimits() : {},
    }),
  };
}

function spendBar(label, spent, limit, title) {
  const level = spent >= limit ? "low" : spent >= limit * 0.8 ? "mid" : "";
  const fill = limit > 0 ? Math.min(100, (spent / limit) * 100) : 100;
  return h("div", { class: "row", title },
    h("span", { style: "width:120px" }, label),
    h("div", { class: "bar " + level, style: "width:120px" }, h("span", { style: `width:${fill}%` })),
    h("span", { class: "mono" }, `${fmt.usd(spent)} / ${fmt.usd(limit)}`));
}

function spendCell(k, groups) {
  const spent = g => k.spent[g] || 0;
  if (k.limit_total !== null) {
    const byGroup = groups.filter(g => spent(g) > 0).map(g => `${groupName(g)}: ${fmt.usd(spent(g))}`).join("\n");
    return spendBar(t("keys.limit.mode.total"), Object.values(k.spent).reduce((a, b) => a + b, 0), k.limit_total, byGroup);
  }
  const shown = groups.filter(g => k.limits[g] !== undefined || spent(g) > 0);
  if (!shown.length) return h("span", { class: "muted" }, t("keys.usage.none"));
  return shown.map(g => k.limits[g] === undefined
    ? h("div", {}, t("keys.usage.unlimited", { group: groupName(g), spent: fmt.usd(spent(g)) }))
    : spendBar(groupName(g), spent(g), k.limits[g]));
}

// totalsCell shows a key's successful calls and tokens; hovering lists
// them by model.
function totalsCell(totals) {
  if (!totals.requests) return h("span", { class: "muted" }, t("keys.totals.none"));
  const byModel = totals.models
    .map(m => t("keys.totals.model", { model: m.model, count: m.requests, input: fmt.int(m.input), output: fmt.int(m.output) }))
    .join("\n");
  return term(t("keys.totals", { count: totals.requests, input: fmt.compact(totals.input), output: fmt.compact(totals.output) }), byModel);
}

// modelPicker lists model ids as checkboxes; none checked allows every
// model. Other ids can be typed in; selected ids not on offer stay listed,
// so saving does not drop them.
function modelPicker(offered, selected) {
  const known = new Set(offered);
  const boxes = [];
  const list = h("div", { class: "picker" });
  const add = (id, checked) => {
    const existing = boxes.find(b => b.value === id);
    if (existing) { existing.checked ||= checked; return; }
    const box = h("input", { type: "checkbox", value: id, checked });
    boxes.push(box);
    list.append(h("label", { class: "check", title: known.has(id) ? null : t("keys.models.not_offered") }, box, h("span", { class: "mono" }, id)));
  };
  offered.forEach(id => add(id, selected.includes(id)));
  selected.forEach(id => add(id, true));

  const typed = h("input", { class: "mono", placeholder: t("keys.models.other"), style: "width:260px" });
  const addTyped = () => {
    const id = typed.value.trim();
    if (!id) return;
    add(id, true);
    typed.value = "";
  };
  // Enter adds the typed id instead of submitting the form.
  typed.addEventListener("keydown", e => { if (e.key === "Enter") { e.preventDefault(); addTyped(); } });
  return {
    node: h("div", {}, list,
      h("div", { class: "row", style: "margin-top:6px" }, typed, h("button", { type: "button", onclick: addTyped }, t("keys.models.add")))),
    // An id typed but not yet added still counts.
    read: () => { addTyped(); return boxes.filter(b => b.checked).map(b => b.value); },
  };
}

views.keys = container => {
  const created = h("div");
  const editor = h("div");
  const list = h("div");
  let groups = [];
  let offered = [];
  let newLimits = null;
  let newModels = null;

  const name = h("input", { required: true, maxlength: 64, placeholder: t("keys.name.placeholder") });
  const note = h("input", { placeholder: t("common.optional") });
  const modelsBox = h("div");
  const limitsRow = h("div", { class: "row" });
  const form = h("form", { onsubmit: event => {
    event.preventDefault();
    guarded(async () => {
      const data = await api("POST", "keys", { name: name.value, note: note.value, models: newModels.read(), ...newLimits.read() });
      created.replaceChildren(h("div", { class: "card" },
        h("h2", {}, t("keys.created.title", { name: name.value })),
        h("p", { class: "muted" }, t("keys.created.note")),
        h("div", { class: "secret" }, data.key)));
      name.value = note.value = "";
      newLimits = newModels = null;
      await load();
    }, event.submitter);
  } },
    h("div", { class: "row" },
      h("label", {}, t("common.name"), name),
      h("label", { class: "grow" }, t("common.note"), note)),
    h("div", { class: "field" }, t("keys.models.label"), modelsBox),
    limitsRow,
    h("p", { class: "muted" }, t("keys.limit.hint")),
    h("button", { type: "submit" }, t("keys.create")));

  async function load() {
    const [data, catalog] = await Promise.all([api("GET", "keys"), api("GET", "models")]);
    groups = data.groups;
    offered = catalog.map(m => m.id);
    if (!newLimits) {
      newLimits = limitEditor(groups, { max_workers: null, limit_total: null, limits: {} });
      limitsRow.replaceChildren(...newLimits.nodes);
    }
    if (!newModels) {
      newModels = modelPicker(offered, []);
      modelsBox.replaceChildren(newModels.node);
    }
    list.replaceChildren(table([
      { title: t("common.name"), cell: k => k.name },
      { title: t("keys.col.hash"), hint: t("keys.col.hash.hint"), cell: k => hashText(k.hash) },
      { title: t("common.note"), cell: k => k.note || h("span", { class: "muted" }, "—") },
      { title: t("keys.col.models"), cell: k => k.models.length ? k.models.join(", ") : h("span", { class: "muted" }, t("keys.models.all")) },
      { title: t("keys.col.workers"), cell: k => k.max_workers !== null ? k.max_workers : h("span", { class: "muted" }, t("keys.limit.placeholder")) },
      { title: t("keys.col.usage"), cell: k => spendCell(k, groups) },
      { title: t("keys.col.totals"), hint: t("keys.col.totals.hint"), cell: k => totalsCell(k.totals) },
      { title: t("keys.col.created"), cell: k => fmt.time(k.created_at) },
      { title: "", cell: k => k.revoked_at
          ? h("span", { class: "row" }, t("keys.revoked", { time: fmt.time(k.revoked_at) }),
            h("button", { onclick: e => restore(k, e.target) }, t("keys.restore")),
            h("button", { class: "danger", onclick: e => remove(k, e.target) }, t("keys.delete")))
          : h("span", { class: "row" },
            h("button", { onclick: () => editKey(k) }, t("keys.edit")),
            h("button", { class: "danger", onclick: e => revoke(k, e.target) }, t("keys.revoke"))) },
    ], data.keys, k => k.revoked_at ? "revoked" : ""));
  }

  function editKey(k) {
    const noteInput = h("input", { value: k.note });
    const models = modelPicker(offered, k.models);
    const limits = limitEditor(groups, k);
    const save = event => {
      event.preventDefault();
      guarded(async () => {
        await api("PATCH", `keys/${k.hash}`, { note: noteInput.value, models: models.read(), ...limits.read() });
        editor.replaceChildren();
        toast(t("common.saved"));
        await load();
      }, event.submitter);
    };
    editor.replaceChildren(h("div", { class: "card" },
      h("h2", {}, t("keys.edit.title", { name: k.name })),
      h("form", { onsubmit: save },
        h("div", { class: "row" },
          h("label", { class: "grow" }, t("common.note"), noteInput)),
        h("div", { class: "field" }, t("keys.models.label"), models.node),
        h("div", { class: "row" }, limits.nodes),
        h("div", { class: "row", style: "margin-top:8px" },
          h("button", { type: "submit", class: "primary" }, t("common.save")),
          h("button", { type: "button", onclick: () => editor.replaceChildren() }, t("common.cancel"))))));
  }

  function revoke(k, button) {
    if (!confirm(t("keys.revoke.confirm", { name: k.name }))) return;
    guarded(async () => { await api("POST", `keys/${k.hash}/revoke`); await load(); }, button);
  }

  function restore(k, button) {
    guarded(async () => { await api("POST", `keys/${k.hash}/restore`); await load(); }, button);
  }

  function remove(k, button) {
    if (!confirm(t("keys.delete.confirm", { name: k.name }))) return;
    guarded(async () => { await api("DELETE", `keys/${k.hash}`); await load(); }, button);
  }

  container.append(h("h1", {}, t("keys.title")), h("div", { class: "card" }, form), created, editor, h("div", { class: "card" }, list));
  guarded(load);
};

// ---- settings --------------------------------------------------------

// resetButton puts a field back to its default value; it is disabled while
// the field holds the default. The change applies on save.
function resetButton(input, value) {
  const isBox = input.type === "checkbox";
  const current = () => isBox ? input.checked : input.value;
  const button = h("button", { type: "button", class: "reset", title: t("settings.reset.title", { value: value === "" ? t("settings.reset.empty") : value }) }, "↺");
  const sync = () => { button.disabled = String(current()) === String(value); };
  button.addEventListener("click", () => {
    if (isBox) input.checked = value; else input.value = value;
    sync();
  });
  input.addEventListener("input", sync);
  input.addEventListener("change", sync);
  sync();
  return button;
}

const withReset = (input, value) => h("span", { class: "with-reset" }, input, resetButton(input, value));

// rowEditor edits a list of objects as table rows, one input per field,
// with buttons to add a row and to put back the default rows.
function rowEditor(fields, rows, defaults, blank, addLabel) {
  const body = h("tbody");
  const read = () => [...body.children].map(tr => tr.read());
  const reset = h("button", { type: "button", class: "reset" }, t("settings.defaults"));
  const sync = () => { reset.disabled = JSON.stringify(read()) === JSON.stringify(defaults); };
  const add = row => {
    const inputs = fields.map(f => f.input(row[f.key]));
    const tr = h("tr", {}, inputs.map(i => h("td", {}, i)),
      h("td", {}, h("button", { type: "button", class: "danger", onclick: () => { tr.remove(); sync(); } }, t("common.remove"))));
    tr.read = () => Object.fromEntries(fields.map((f, i) => [f.key, f.read(inputs[i])]));
    tr.addEventListener("input", sync);
    body.append(tr);
  };
  reset.addEventListener("click", () => { body.replaceChildren(); defaults.forEach(add); sync(); });
  rows.forEach(add);
  sync();
  return {
    node: h("div", {},
      h("div", { class: "table-wrap" }, h("table", {}, h("thead", {}, h("tr", {}, fields.map(f => h("th", {}, f.title)), h("th"))), body)),
      h("div", { class: "row", style: "margin-top:8px" },
        h("button", { type: "button", onclick: () => { add(blank); sync(); } }, addLabel), reset)),
    read,
  };
}

views.settings = container => {
  const general = h("div", { class: "card" });
  const password = h("div", { class: "card" });
  const sessions = h("div", { class: "card" });
  const audit = h("div", { class: "card" });
  container.append(h("h1", {}, t("settings.title")), general, password, sessions, audit);

  async function loadGeneral() {
    const [s, catalog] = await Promise.all([api("GET", "settings"), api("GET", "models")]);
    const d = s.defaults;
    const turns = h("input", { type: "number", min: 1, max: 64, value: s.turns_per_account });
    const maxWorkers = h("input", { type: "number", min: 0, max: 1024, value: s.max_workers });
    const ttl = h("input", { value: s.snapshot_ttl, pattern: "[0-9hms.]+" });
    const idleClose = h("input", { value: s.session_idle_close, pattern: "[0-9hms.]+" });
    const level = h("select", {}, ["debug", "info", "warn", "error"].map(l => h("option", { value: l, selected: l === s.log_level }, l)));
    const retention = h("input", { type: "number", min: 1, max: 3650, value: s.retention_days });
    const fallback = h("input", { value: s.fallback_proxy, placeholder: "http://127.0.0.1:7890" });
    const direct = h("input", { type: "checkbox", checked: s.allow_direct });
    const contextCheck = h("input", { type: "checkbox", checked: s.context_check });

    const modelInput = value => h("input", { value, list: "settings-models", required: true, style: "width:260px" });
    const number = (min, step) => value => h("input", { type: "number", min, step, value, style: "width:110px" });
    const prices = rowEditor([
      { key: "model", title: t("common.model"), input: modelInput, read: i => i.value.trim() },
      { key: "input", title: t("settings.prices.input"), input: number(0, "any"), read: i => Number(i.value) },
      { key: "output", title: t("settings.prices.output"), input: number(0, "any"), read: i => Number(i.value) },
      { key: "cached", title: t("settings.prices.cached"), input: number(0, "any"), read: i => Number(i.value) },
      { key: "cache_write", title: t("settings.prices.cache_write"), input: number(0, "any"), read: i => Number(i.value) },
    ], s.prices, d.prices, { model: "", input: 0, cached: 0, cache_write: 0, output: 0 }, t("settings.prices.add"));
    const windows = rowEditor([
      { key: "model", title: t("common.model"), input: modelInput, read: i => i.value.trim() },
      { key: "tokens", title: t("settings.windows.tokens"), input: number(1, 1), read: i => Number(i.value) },
    ], s.context_windows, d.context_windows, { model: "", tokens: 200000 }, t("settings.windows.add"));

    const form = h("form", { onsubmit: event => {
      event.preventDefault();
      guarded(async () => {
        await api("PUT", "settings", {
          turns_per_account: Number(turns.value), max_workers: Number(maxWorkers.value), snapshot_ttl: ttl.value, session_idle_close: idleClose.value, log_level: level.value,
          retention_days: Number(retention.value), prices: prices.read(), context_check: contextCheck.checked, context_windows: windows.read(),
          fallback_proxy: fallback.value.trim(), allow_direct: direct.checked,
        });
        toast(t("common.saved"));
        await loadGeneral();
      }, event.submitter);
    } },
      h("datalist", { id: "settings-models" }, catalog.map(m => h("option", { value: m.id }, m.name))),
      h("div", { class: "row" },
        h("label", {}, t("settings.turns"), withReset(turns, d.turns_per_account)),
        h("label", {}, t("settings.max_workers"), withReset(maxWorkers, d.max_workers)),
        h("label", {}, t("settings.ttl"), withReset(ttl, d.snapshot_ttl)),
        h("label", { title: t("settings.idle_close.hint") }, t("settings.idle_close"), withReset(idleClose, d.session_idle_close)),
        h("label", {}, t("settings.log_level"), withReset(level, d.log_level)),
        h("label", {}, t("settings.retention"), withReset(retention, d.retention_days))),
      h("h3", {}, t("settings.outbound")),
      h("div", { class: "row" },
        h("label", { class: "grow" }, t("settings.fallback"), withReset(fallback, d.fallback_proxy)),
        h("span", { class: "with-reset" }, h("label", { class: "check" }, direct, t("settings.direct")), resetButton(direct, d.allow_direct))),
      h("p", { class: "muted" }, t("settings.outbound.note")),
      h("details", { class: "fold" },
        h("summary", {}, s.context_check
          ? t("settings.windows.summary.on", { count: s.context_windows.length })
          : t("settings.windows.summary.off", { count: s.context_windows.length })),
        h("div", { class: "field" }, h("span", { class: "with-reset" },
          h("label", { class: "check", title: t("settings.context_check.hint") }, contextCheck, t("settings.context_check")),
          resetButton(contextCheck, d.context_check))),
        h("p", { class: "muted" }, t("settings.windows.note")),
        windows.node),
      h("details", { class: "fold" },
        h("summary", {}, t("settings.prices.summary", { count: s.prices.length })),
        h("p", { class: "muted" }, t("settings.prices.note")),
        prices.node),
      h("div", { class: "row", style: "margin-top:12px" },
        h("span", { class: "grow" }),
        h("button", { type: "submit" }, t("settings.save"))));
    general.replaceChildren(h("h2", {}, t("settings.general")), form);
  }

  function renderPassword() {
    const current = h("input", { type: "password", autocomplete: "current-password", required: true });
    const next = h("input", { type: "password", autocomplete: "new-password", minlength: 12, required: true });
    const again = h("input", { type: "password", autocomplete: "new-password", required: true });
    const form = h("form", { class: "row", onsubmit: event => {
      event.preventDefault();
      if (next.value !== again.value) { toast(t("password.differ"), true); return; }
      guarded(async () => {
        const data = await api("POST", "password", { current: current.value, new: next.value });
        csrf = data.csrf;
        current.value = next.value = again.value = "";
        toast(t("password.changed"));
        await loadSessions();
      }, event.submitter);
    } },
      h("label", {}, t("password.current"), current), h("label", {}, t("password.new"), next), h("label", {}, t("password.again"), again),
      h("button", { type: "submit" }, t("password.change")));
    password.replaceChildren(h("h2", {}, t("password.title")), form);
  }

  async function loadSessions() {
    const list = await api("GET", "sessions");
    sessions.replaceChildren(h("h2", {}, t("browsers.title")), table([
      { title: t("browsers.last_seen"), cell: s => fmt.time(s.seen_at) },
      { title: t("common.ip"), cell: s => s.ip },
      { title: t("browsers.browser"), cell: s => h("span", { class: "mono" }, s.user_agent) },
      { title: t("browsers.expires"), cell: s => fmt.time(s.expires_at) },
      { title: "", cell: s => s.current ? h("span", { class: "muted" }, t("browsers.this"))
          : h("button", { class: "danger", onclick: e => guarded(async () => { await api("DELETE", `sessions/${s.id}`); await loadSessions(); }, e.target) }, t("browsers.sign_out")) },
    ], list));
  }

  async function loadAudit() {
    const entries = await api("GET", "audit");
    audit.replaceChildren(h("h2", {}, t("audit.title")), table([
      { title: t("common.time"), cell: e => fmt.time(e.at) },
      { title: t("common.ip"), cell: e => e.ip },
      { title: t("audit.action"), cell: e => h("span", { class: e.action.endsWith("failed") ? "bad" : "" }, e.action) },
      { title: t("audit.detail"), cell: e => e.detail },
    ], entries));
  }

  renderPassword();
  guarded(loadGeneral);
  guarded(loadSessions);
  guarded(loadAudit);
};

// ---- accounts --------------------------------------------------------

// accountBadge shows why an account is out of rotation, or ready when the
// account's checks passed.
function accountBadge(a, ready) {
  if (a.paused) return h("span", { class: "badge warn" }, t("account.paused"));
  if (a.disabled) return h("span", { class: "badge bad", title: a.disabled }, t("account.needs_signin"));
  if (a.blocked) return h("span", { class: "badge bad", title: a.blocked }, t("account.no_proxy"));
  if (a.verify) return h("span", { class: "badge bad" }, t("account.needs_verification"));
  return ready;
}

views.accounts = container => {
  const cards = h("div", { class: "grid" });
  const modelsCard = h("div", { class: "card" });
  const signins = new Map(); // account name → {url, code}
  const imports = new Map(); // account name → {token}
  const routings = new Set(); // account names being edited

  const name = h("input", { required: true, maxlength: 64, placeholder: t("accounts.name.placeholder"), pattern: "[A-Za-z0-9][A-Za-z0-9_.\\-]*" });
  const proxy = h("input", { placeholder: "http://127.0.0.1:7890" });
  const env = h("textarea", { rows: 1, placeholder: t("accounts.env.placeholder") });
  const form = h("form", { class: "row", onsubmit: event => {
    event.preventDefault();
    const importing = event.submitter.dataset.then === "import";
    guarded(async () => {
      await api("POST", "accounts", { name: name.value.trim(), proxy: proxy.value, env: env.value.split("\n").map(s => s.trim()).filter(Boolean) });
      const added = name.value.trim();
      name.value = proxy.value = env.value = "";
      if (!importing) return startSignin(added);
      imports.set(added, { token: "" });
      await load();
    }, event.submitter);
  } },
    h("label", {}, t("common.name"), name),
    h("label", { title: t("accounts.proxy.hint") }, t("accounts.proxy"), proxy),
    h("label", { class: "grow" }, t("accounts.env"), env),
    h("button", { type: "submit" }, t("accounts.add")),
    h("button", { type: "submit", "data-then": "import" }, t("accounts.add_import")));

  function editProxy(a, button) {
    const value = prompt(t("accounts.proxy.prompt", { name: a.name }), a.proxy);
    if (value === null) return;
    guarded(async () => {
      await api("PUT", `accounts/${encodeURIComponent(a.name)}/proxy`, { proxy: value });
      await load();
    }, button);
  }

  async function startSignin(account, button) {
    await guarded(async () => {
      const data = await api("POST", `accounts/${encodeURIComponent(account)}/signin`);
      signins.set(account, { url: data.url, code: "" });
      await load();
    }, button);
  }

  async function submitSignin(account, button) {
    const state = signins.get(account);
    await guarded(async () => {
      const data = await api("POST", `accounts/${encodeURIComponent(account)}/signin/code`, { code: state.code });
      signins.delete(account);
      toast(t("accounts.signed_in", { email: data.email }));
      await load();
      setTimeout(() => guarded(load), 8000);
    }, button);
  }

  async function cancelSignin(account) {
    signins.delete(account);
    await guarded(async () => { await api("DELETE", `accounts/${encodeURIComponent(account)}/signin`); await load(); });
  }

  function signinPanel(a) {
    const state = signins.get(a.name);
    if (!state) return null;
    const code = h("input", { class: "grow", placeholder: t("accounts.signin.code"), value: state.code,
      oninput: e => { state.code = e.target.value; } });
    const link = h("a", { href: state.url, target: "_blank", rel: "noopener noreferrer" }, t("accounts.signin.link"));
    return h("div", { class: "card" },
      h("h3", {}, t("accounts.signin.title")),
      h("ol", {},
        h("li", {}, tx("accounts.signin.step1", { link })),
        h("li", {}, t("accounts.signin.step2"))),
      h("div", { class: "row" }, code,
        h("button", { class: "primary", onclick: e => submitSignin(a.name, e.target) }, t("accounts.signin.finish")),
        h("button", { onclick: () => cancelSignin(a.name) }, t("common.cancel"))));
  }

  async function submitImport(account, button) {
    const state = imports.get(account);
    await guarded(async () => {
      const data = await api("POST", `accounts/${encodeURIComponent(account)}/signin/import`, { token: state.token });
      imports.delete(account);
      toast(data.verify ? t("accounts.imported_verify") : t("accounts.imported", { email: data.email }), data.verify);
      await load();
    }, button);
  }

  function importPanel(a) {
    const state = imports.get(a.name);
    if (!state) return null;
    const token = h("textarea", { class: "grow mono", rows: 3, autocomplete: "off", spellcheck: "false",
      placeholder: t("accounts.import.placeholder"), oninput: e => { state.token = e.target.value; } });
    token.value = state.token;
    return h("div", { class: "card" },
      h("h3", {}, t("accounts.import.title")),
      h("p", { class: "muted" }, t("accounts.import.hint")),
      h("div", { class: "row" }, token,
        h("button", { class: "primary", onclick: e => submitImport(a.name, e.target) }, t("accounts.import.submit")),
        h("button", { onclick: () => { imports.delete(a.name); guarded(load); } }, t("common.cancel"))));
  }

  function routingPanel(a) {
    if (!routings.has(a.name)) return null;
    const priority = h("input", { type: "number", step: 1, value: String(a.priority), style: "width:90px" });
    const choices = [...new Set([...(a.models || []).map(m => m.id), ...catalog])];
    const models = modelPicker(choices, a.allowed_models);
    const save = event => {
      event.preventDefault();
      guarded(async () => {
        await api("PUT", `accounts/${encodeURIComponent(a.name)}/routing`, {
          priority: Math.trunc(Number(priority.value) || 0),
          allowed_models: models.read(),
        });
        routings.delete(a.name);
        toast(t("common.saved"));
        await load();
      }, event.submitter);
    };
    return h("form", { class: "card", onsubmit: save },
      h("h3", {}, t("accounts.routing.title")),
      h("div", { class: "row" },
        h("label", { title: t("accounts.routing.priority.hint") }, t("accounts.routing.priority"), priority)),
      h("div", { class: "field" }, t("accounts.routing.models"), models.node),
      h("div", { class: "row", style: "margin-top:8px" },
        h("button", { type: "submit", class: "primary" }, t("common.save")),
        h("button", { type: "button", onclick: () => { routings.delete(a.name); guarded(load); } }, t("common.cancel"))));
  }

  // verifyNotice tells what to do about Google's verification request.
  function verifyNotice(a) {
    if (!a.verify) return null;
    const link = a.verify_url
      ? h("a", { href: a.verify_url, target: "_blank", rel: "noopener noreferrer" }, t("accounts.verify.link"))
      : h("span", {}, t("accounts.verify.no_link"), " ", h("a", { href: "#workers" }, t("nav.workers")));
    return h("div", { class: "card" }, h("p", { class: "bad" }, t("accounts.verify")), h("p", {}, link));
  }

  const modelQuotaGroup = id => id.startsWith("claude-") || id.startsWith("gpt-oss-") ? "claude-gpt"
    : id.startsWith("gemini-4-argon") ? "argon" : "gemini";
  const bucketQuotaGroup = id => id.startsWith("3p-") ? "claude-gpt"
    : id.startsWith("gemini-argon-") ? "argon"
    : id.startsWith("gemini-") ? "gemini" : "";

  function quotaExhausted(a, m) {
    const group = modelQuotaGroup(m.id);
    if (a.cooldowns && a.cooldowns[group]) return true;
    const buckets = (a.quota || []).flatMap(g => g.buckets).filter(b => bucketQuotaGroup(b.id) === group);
    if (buckets.length) return buckets.some(b => b.remaining <= 0);
    const qi = m.upstream && m.upstream.quotaInfo;
    return Boolean(qi && !(qi.remainingFraction > 0));
  }

  function allPoolsExhausted(a) {
    if (a.quota && a.quota.length) {
      return a.quota.every(g => g.buckets.some(b => b.remaining <= 0 || Boolean(a.cooldowns && a.cooldowns[bucketQuotaGroup(b.id)])));
    }
    if (a.models && a.models.length) return a.models.every(m => quotaExhausted(a, m));
    return ["gemini", "claude-gpt"].every(g => a.cooldowns && a.cooldowns[g]);
  }

  function status(a) {
    if (allPoolsExhausted(a)) return accountBadge(a, h("span", { class: "badge warn" }, t("account.cooling_down")));
    if (!a.user) return accountBadge(a, h("span", { class: "badge muted" }, t("account.not_checked")));
    return accountBadge(a, h("span", { class: "badge ok" }, t("account.ready")));
  }

  function poolUsageLine(u) {
    if (!u) return null;
    const since = t("accounts.quota.used_since", { time: fmt.time(u.since) });
    if (!u.requests) return h("div", { class: "sub", style: "margin-top:2px", title: since }, t("accounts.quota.used_none"));
    const cached = u.input > 0 ? t("calls.tokens.cached", { pct: fmt.pct(cachePct(u.input, u.cached)) }) : "";
    const cost = fmt.usd(u.cost_usd) + (u.unpriced ? " " + t("calls.total.unpriced", { count: u.unpriced }) : "");
    const summary = joined(t("accounts.quota.used", { cost, input: fmt.compact(u.input), output: fmt.compact(u.output) }), cached);
    const models = u.models.map(m => {
      const mCached = m.input > 0 ? t("calls.tokens.cached", { pct: fmt.pct(cachePct(m.input, m.cached)) }) : "";
      const mCost = fmt.usd(m.cost_usd) + (m.unpriced ? " " + t("calls.total.unpriced", { count: m.unpriced }) : "");
      return joined(t("accounts.quota.used_model", { model: m.model, cost: mCost, count: m.requests, input: fmt.int(m.input), output: fmt.int(m.output) }), mCached);
    });
    return h("div", { class: "sub", style: "margin-top:2px", title: [since, ...models].join("\n") }, summary);
  }

  function quota(a) {
    if (!a.quota || !a.quota.length) return h("p", { class: "muted" }, a.sample_error ? "" : t("accounts.quota.none"));
    const poolUsage = g => {
      const group = g.buckets.map(b => bucketQuotaGroup(b.id)).find(Boolean);
      return group && a.pool_usage && a.pool_usage[group];
    };
    return a.quota.map(g => h("div", {},
      h("h3", { title: g.description || "" }, g.name),
      g.buckets.map(b => {
        const level = b.remaining <= 0.1 ? "low" : b.remaining <= 0.35 ? "mid" : "";
        return h("div", { class: "row", title: b.name },
          h("span", { class: "muted", style: "width:52px" }, b.window),
          h("div", { class: "bar grow " + level }, h("span", { style: `width:${Math.max(0, Math.min(100, b.remaining * 100))}%` })),
          h("span", { class: "mono", style: "width:150px;text-align:right" }, t("accounts.quota.left", { left: fmt.pct(b.remaining, 1), reset: fmt.until(b.reset_at) })));
      }),
      poolUsageLine(poolUsage(g))));
  }

  function card(a) {
    const enc = encodeURIComponent(a.name);
    const act = (label, action, cls) => h("button", { class: cls || "", onclick: e => guarded(async () => { await action(); await load(); }, e.target) }, label);
    const route = a.route || t("accounts.route_unknown");
    return h("div", { class: "card" },
      h("div", { class: "row" }, h("h2", { class: "grow" }, a.name), status(a)),
      a.user ? h("p", {}, `${a.user.email} · ${a.user.tier || a.user.plan}`) : null,
      a.disabled ? h("p", { class: "bad" }, a.disabled) : null,
      a.blocked ? h("p", { class: "bad" }, a.blocked) : null,
      verifyNotice(a),
      Object.entries(a.cooldowns).map(([group, until]) => h("p", { class: "warn" }, t("accounts.cooldown", { group, time: fmt.until(until) }))),
      a.sample_error ? h("p", { class: "bad mono" }, t("accounts.sample_error", { error: a.sample_error })) : null,
      h("p", { class: "muted" }, joined(
        t("accounts.priority", { priority: a.priority }),
        t("accounts.turns", { count: a.turns }),
        t("accounts.procs", { count: a.procs }),
        a.sampled_at ? t("accounts.sampled", { time: fmt.time(a.sampled_at) }) : t("accounts.not_sampled"),
        a.procs ? "" : t("accounts.sampled_when_running"),
        a.proxy ? t("accounts.traffic", { route }) : t("accounts.traffic.no_own_proxy", { route }),
        a.env_names.length ? t("accounts.env_names", { names: a.env_names.join(", ") }) : "")),
      h("p", {}, a.models ? t("accounts.models", { models: a.models.map(m => m.id).join(", ") || "—" }) : h("span", { class: "muted" }, t("accounts.models.unknown"))),
      a.allowed_models.length ? h("p", {}, t("accounts.allowed_models", { models: a.allowed_models.join(", ") })) : null,
      quota(a),
      signinPanel(a),
      importPanel(a),
      routingPanel(a),
      h("div", { class: "row", style: "margin-top:12px" },
        signins.has(a.name) ? null : h("button", { onclick: e => startSignin(a.name, e.target) }, a.user || a.disabled ? t("accounts.signin_again") : t("accounts.signin")),
        imports.has(a.name) ? null : h("button", { onclick: () => { imports.set(a.name, { token: "" }); guarded(load); } }, t("accounts.import")),
        act(t("accounts.refresh"), () => api("POST", `accounts/${enc}/refresh`)),
        routings.has(a.name) ? null : h("button", { onclick: () => { routings.add(a.name); guarded(load); } }, t("accounts.routing.edit")),
        h("button", { onclick: e => editProxy(a, e.target) }, t("accounts.proxy.edit")),
        a.paused ? act(t("accounts.resume"), () => api("POST", `accounts/${enc}/resume`)) : act(t("accounts.pause"), () => api("POST", `accounts/${enc}/pause`)),
        h("span", { class: "grow" }),
        h("button", { class: "danger", onclick: e => {
          if (!confirm(t("accounts.remove.confirm", { name: a.name }))) return;
          guarded(async () => { await api("DELETE", `accounts/${enc}`); await load(); }, e.target);
        } }, t("common.remove"))));
  }

  const outOfRotation = a => a.paused ? t("account.paused") : a.disabled ? t("account.needs_signin") : a.blocked ? t("account.no_proxy")
    : a.verify ? t("account.needs_verification") : "";

  // accountSet shows the first account in rotation and how many more
  // there are, "main (+2)"; all of them, with why one is out of rotation,
  // show on hover.
  function accountSet(accounts) {
    const ready = accounts.filter(a => !outOfRotation(a));
    const hover = accounts.map(a => joined(a.name, outOfRotation(a))).join("\n");
    if (!ready.length) return h("span", { class: "muted", title: hover }, t("accounts.models.none_ready"));
    return h("span", { title: hover }, ready[0].name, ready.length > 1 ? ` (+${ready.length - 1})` : "");
  }

  // modelGroups gathers the models accounts reported into available,
  // quota-exhausted, manually disabled (excluded by allowed_models), and
  // upstream-unavailable sets, each grouped by the set of accounts.
  function modelGroups(accounts) {
    const available = new Map();
    const exhausted = new Map();
    const manuallyDisabled = new Map();
    const disabled = new Map();
    const add = (byId, m, a) => {
      if (!byId.has(m.id)) {
        byId.set(m.id, { ...m, accounts: [], exhaustedOn: [], manuallyDisabledOn: [], disabledOn: [] });
      }
      const entry = byId.get(m.id);
      entry.accounts.push(a);
      entry.upstream ||= m.upstream;
    };
    const allowsModel = (a, id) => !a.allowed_models.length || a.allowed_models.includes(id);
    for (const a of accounts) {
      for (const m of a.models || []) {
        if (!allowsModel(a, m.id)) add(manuallyDisabled, m, a);
        else if (quotaExhausted(a, m)) add(exhausted, m, a);
        else add(available, m, a);
      }
      for (const m of a.unavailable) add(disabled, m, a);
    }
    for (const [id, m] of exhausted) {
      if (!available.has(id)) continue;
      available.get(id).exhaustedOn = m.accounts;
      exhausted.delete(id);
    }
    for (const [id, m] of manuallyDisabled) {
      if (!available.has(id)) continue;
      available.get(id).manuallyDisabledOn = m.accounts;
      manuallyDisabled.delete(id);
    }
    for (const [id, m] of disabled) {
      const target = available.get(id) || exhausted.get(id) || manuallyDisabled.get(id);
      if (!target) continue;
      target.disabledOn = m.accounts;
      disabled.delete(id);
    }
    const group = byId => {
      const groups = new Map();
      for (const m of byId.values()) {
        const key = m.accounts.map(a => a.name).join("\n");
        if (!groups.has(key)) groups.set(key, { accounts: m.accounts, models: [] });
        groups.get(key).models.push(m);
      }
      return [...groups.values()];
    };
    const unavailableCount = new Set([...exhausted.keys(), ...manuallyDisabled.keys(), ...disabled.keys()]).size;
    return {
      available: group(available),
      exhausted: group(exhausted),
      manuallyDisabled: group(manuallyDisabled),
      unavailable: group(disabled),
      counts: [available.size, unavailableCount],
    };
  }

  // upstreamText lists a model's upstream record, one field per line.
  const upstreamText = record => Object.entries(record || {})
    .map(([key, value]) => `${key}: ${typeof value === "object" ? JSON.stringify(value) : value}`).join("\n");

  function modelTable(models, unavailable) {
    const up = (m, key) => m.upstream && m.upstream[key];
    const notice = m => {
      const tag = up(m, "tagDescription") || "";
      const exhausted = m.exhaustedOn.length ? t("accounts.models.exhausted_elsewhere", { accounts: m.exhaustedOn.map(a => a.name).join(", ") }) : "";
      const manual = m.manuallyDisabledOn.length ? t("accounts.models.manually_disabled_elsewhere", { accounts: m.manuallyDisabledOn.map(a => a.name).join(", ") }) : "";
      const elsewhere = m.disabledOn.length ? t("accounts.models.disabled_on", { accounts: m.disabledOn.map(a => a.name).join(", ") }) : "";
      return h("span", { class: unavailable ? "warn" : "muted" }, joined(tag, exhausted, manual, elsewhere));
    };
    return h("div", { class: "compact" }, table([
      { title: t("common.model"), cell: m => h("span", { title: upstreamText(m.upstream) || null },
          h("span", { class: "mono" }, m.id), m.name && m.name !== m.id ? h("span", { class: "muted" }, " " + m.name) : "") },
      { title: t("accounts.models.context"), num: true, cell: m => up(m, "maxTokens") ? fmt.compact(Number(up(m, "maxTokens"))) : "—" },
      { title: t("accounts.models.output"), num: true, cell: m => up(m, "maxOutputTokens") ? fmt.compact(Number(up(m, "maxOutputTokens"))) : "—" },
      { title: t("accounts.models.tiers"), cell: m => m.efforts.join("/") },
      { title: t("accounts.models.notice"), cell: notice },
    ], models));
  }

  // The model list keeps its open/closed state across reloads.
  const modelsSummary = h("summary");
  const modelsBody = h("div");
  const modelsFold = h("details", { class: "card fold models" }, modelsSummary, modelsBody);

  function renderModels(accounts) {
    const { available, exhausted, manuallyDisabled, unavailable, counts } = modelGroups(accounts);
    const unreported = accounts.filter(a => !a.models).length;
    modelsSummary.textContent = joined(t("accounts.models.title"), t("accounts.models.available", { count: counts[0] }),
      counts[1] ? t("accounts.models.unavailable", { count: counts[1] }) : "");
    modelsBody.replaceChildren(
      ...available.map(g => h("div", { class: "model-group" }, h("h3", {}, accountSet(g.accounts)), modelTable(g.models, false))),
      ...exhausted.map(g => h("div", { class: "model-group" },
        h("h3", { class: "warn" }, t("accounts.models.exhausted_on"), " ", accountSet(g.accounts)), modelTable(g.models, true))),
      ...manuallyDisabled.map(g => h("div", { class: "model-group" },
        h("h3", { class: "warn" }, t("accounts.models.manually_disabled_on"), " ", accountSet(g.accounts)), modelTable(g.models, true))),
      ...unavailable.map(g => h("div", { class: "model-group" },
        h("h3", { class: "warn" }, t("accounts.models.unavailable_on"), " ", accountSet(g.accounts)), modelTable(g.models, true))),
      ...(unreported ? [h("p", { class: "muted" }, t("accounts.models.unreported", { count: unreported }))] : []),
      h("p", { class: "muted" }, t("accounts.models.hint")));
  }

  let accounts = [];
  let catalog = [];
  async function load() {
    const [list, offered] = await Promise.all([api("GET", "accounts"), api("GET", "models")]);
    accounts = list;
    catalog = offered.map(m => m.id);
    for (const a of accounts) if (a.login_pending === false) signins.delete(a.name);
    renderModels(accounts);
    cards.replaceChildren(...(accounts.length ? accounts.map(card) : [h("p", { class: "muted" }, t("accounts.empty"))]));
  }

  // refreshAll refreshes every account; the server runs them one at a time.
  function refreshAll(button) {
    guarded(async () => {
      const results = await Promise.allSettled(accounts.map(a => api("POST", `accounts/${encodeURIComponent(a.name)}/refresh`)));
      const failed = results.flatMap((r, i) => r.status === "rejected" && r.reason.status !== 401 ? [`${accounts[i].name}: ${r.reason.message}`] : []);
      if (failed.length) toast(failed.join("; "), true);
      else toast(t("accounts.refreshed_all", { count: results.length }));
      await load();
    }, button);
  }

  container.append(
    h("div", { class: "row" }, h("h1", { class: "grow" }, t("accounts.title")),
      h("button", { onclick: e => refreshAll(e.target) }, t("accounts.refresh_all"))),
    h("div", { class: "card" }, form), modelsFold, cards);
  // Re-render on a timer unless a sign-in, import, or routing edit is open.
  return polling(container, 10000, async () => { if (signins.size === 0 && imports.size === 0 && routings.size === 0) await load(); });
};

// ---- workers ---------------------------------------------------------

// agyLogViewer reads an account's agy logs: its own card, which the
// Workers page keeps across its refreshes.
function agyLogViewer() {
  const state = { account: "", file: "", level: "", q: "" };
  const box = h("div", { class: "logs" });
  const info = h("p", { class: "muted" });
  const accountSelect = h("select", { onchange: e => { state.account = e.target.value; state.file = ""; guarded(reload); } });
  const fileSelect = h("select", { onchange: e => { state.file = e.target.value; guarded(loadEntries); } });
  const level = h("select", { onchange: e => { state.level = e.target.value; guarded(loadEntries); } },
    ["", "W", "E"].map(l => h("option", { value: l }, lookup("agy_log_level", l || "all"))));
  const search = h("input", { placeholder: t("workers.logs.search.placeholder"),
    onchange: e => { state.q = e.target.value.trim(); guarded(loadEntries); } });
  const card = h("div", { class: "card" },
    h("h2", {}, t("workers.logs.title")),
    h("div", { class: "row" },
      h("label", {}, t("common.account"), accountSelect),
      h("label", {}, t("workers.logs.file"), fileSelect),
      h("label", {}, t("logs.level"), level),
      h("label", { class: "grow" }, t("logs.search"), search),
      h("button", { onclick: e => guarded(reload, e.target) }, t("workers.logs.reload"))),
    info, box);
  const enc = () => encodeURIComponent(state.account);

  async function loadFiles() {
    const data = await api("GET", `workers/${enc()}/logs`);
    if (!data.files.some(f => f.name === state.file)) state.file = "";
    fileSelect.replaceChildren(h("option", { value: "" }, t("workers.logs.all_files")),
      ...data.files.map(f => h("option", { value: f.name, selected: f.name === state.file, title: f.name },
        joined(lookup("agy_log_kind", f.kind) || f.kind || f.name, f.pid ? `pid ${f.pid}` : "", fmt.bytes(f.size), fmt.time(f.modified),
          f.running ? t("workers.logs.running") : ""))));
  }

  async function loadEntries() {
    const q = new URLSearchParams({ limit: "1000" });
    for (const f of ["file", "level", "q"]) if (state[f]) q.set(f, state[f]);
    const data = await api("GET", `workers/${enc()}/logs/entries?${q}`);
    const rows = [];
    let file = "";
    for (const e of data.entries) {
      if (e.file !== file) rows.push(h("div", { class: "muted" }, `— ${e.file}`));
      file = e.file;
      rows.push(h("div", { class: e.level === "W" ? "WARN" : e.level === "E" || e.level === "F" ? "ERROR" : "" }, e.text));
    }
    box.replaceChildren(...rows);
    box.scrollTop = box.scrollHeight;
    info.textContent = joined(t("workers.logs.matched", { count: data.matched, shown: data.entries.length }),
      data.partial.length ? t("workers.logs.partial", { files: data.partial.join(", ") }) : "");
  }

  async function reload() {
    if (!state.account) return;
    await loadFiles();
    await loadEntries();
  }

  return {
    card,
    // setAccounts keeps the account list current; the first account is
    // shown until another is picked.
    setAccounts(names) {
      if (accountSelect.options.length !== names.length || names.some((n, i) => accountSelect.options[i].value !== n)) {
        accountSelect.replaceChildren(...names.map(n => h("option", { value: n }, n)));
      }
      if (!names.includes(state.account)) {
        state.account = names[0] || "";
        state.file = "";
        guarded(reload);
      }
      accountSelect.value = state.account;
    },
    show(name) {
      state.account = name;
      state.file = "";
      accountSelect.value = name;
      card.scrollIntoView({ behavior: "smooth" });
      guarded(reload);
    },
  };
}

views.workers = container => {
  const cards = h("div");
  const logs = agyLogViewer();
  container.append(h("h1", {}, t("workers.title")), cards, logs.card);

  const action = (label, cls, confirmText, run) => h("button", { class: cls, onclick: e => {
    if (confirmText && !confirm(confirmText)) return;
    guarded(async () => { await run(); await load(); }, e.target);
  } }, label);

  function processes(a, now) {
    if (!a.procs.length) return h("p", { class: "muted" }, t("workers.no_procs"));
    const rows = [];
    for (const p of a.procs) {
      rows.push({ kind: "front", label: t("workers.kind.front"), pid: p.pid, agent: p.agent, up: fmt.span(p.started, now), rss: p.rss_bytes, cpu: p.cpu_seconds, sessions: "", error: p.error });
      for (const w of p.workers) {
        rows.push({ kind: "worker", label: t("workers.kind.worker", { number: w.number }), pid: w.pid, agent: "", up: "", rss: w.rss_bytes, cpu: w.cpu_seconds,
          sessions: w.sessions.map(id => id.slice(0, 8)).join(", ") });
      }
    }
    const total = rows.reduce((sum, r) => sum + r.rss, 0);
    return [
      table([
        { title: t("workers.process"), cell: r => term(r.label, lookup("process", r.kind)) },
        { title: t("workers.pid"), num: true, cell: r => r.pid },
        { title: t("workers.profile"), cell: r => h("span", { class: "mono" }, r.agent) },
        { title: t("workers.up"), cell: r => r.up },
        { title: t("workers.memory"), num: true, cell: r => fmt.bytes(r.rss) },
        { title: t("workers.cpu"), num: true, cell: r => fmt.seconds(r.cpu) },
        { title: t("workers.sessions"), cell: r => h("span", { class: "mono" }, r.sessions) },
        { title: "", cell: r => r.error ? h("span", { class: "bad" }, r.error) : "" },
      ], rows),
      h("p", { class: "muted" }, t("workers.procs_total", { count: rows.length, memory: fmt.bytes(total) })),
    ];
  }

  function sessions(a, now) {
    const enc = encodeURIComponent(a.name);
    const where = s => s.worker ? term(t("workers.kind.worker", { number: s.worker }), lookup("process", "worker"))
      : s.pid ? t("workers.in_process")
      : term(t("workers.closed"), t("workers.closed.hint"));
    const turnTime = s => !s.turn || !s.turn.started ? "" : s.turn.parked_at
      ? t("workers.turn.parked", { elapsed: fmt.span(s.turn.started, now), waiting: fmt.span(s.turn.parked_at, now), left: fmt.span(now, s.turn.park_timeout) })
      : fmt.span(s.turn.started, now);
    return table([
      { title: t("workers.session"), cell: s => stack(h("span", { class: "mono", title: s.id }, s.id.slice(0, 8)), where(s)) },
      { title: t("common.key"), cell: s => keyName(s.key, s.key_hash) },
      { title: t("workers.state"), cell: s => !s.turn ? stack(term(t("workers.idle"), lookup("turn_state", "idle")), t("workers.last_used", { time: fmt.span(s.last_used, now) }))
          : stack(h("span", { class: s.turn.state.startsWith("waiting") ? "warn" : "ok", title: lookup("turn_state", s.turn.state) }, s.turn.state),
            joined(s.turn.model, turnTime(s))) },
      { title: "", cell: s => {
          const path = `workers/${enc}/sessions/${encodeURIComponent(s.id)}`;
          if (s.turn) return action(t("workers.cancel_turn"), "danger", t("workers.cancel_turn.confirm", { key: keyText(s.key, s.key_hash) }), () => api("POST", path + "/cancel"));
          if (!s.pid) return "";
          return action(t("workers.close"), "", "", () => api("POST", path + "/close"));
        } },
    ], a.sessions);
  }

  function egress(a) {
    const e = a.egress;
    if (!e) return null;
    return h("div", {},
      h("h3", {}, t("workers.outbound")),
      h("p", {}, joined(
        t("workers.egress.route", { route: e.route }),
        t("workers.egress.open", { count: e.open }),
        t("workers.egress.connections", { count: e.connections }),
        t("workers.egress.bytes", { out: fmt.bytes(e.bytes_out), in: fmt.bytes(e.bytes_in) }),
        e.refused ? t("workers.egress.refused", { count: e.refused }) : "")),
      e.last_error ? h("p", { class: "bad mono" }, `${fmt.time(e.last_error_at)}: ${e.last_error}`) : null,
      e.hosts.length ? h("p", { class: "muted mono" }, e.hosts.map(x => `${x.host} ×${x.connections}`).join("  ·  ")) : null);
  }

  function card(a, now) {
    const badge = accountBadge(a, h("span", { class: "badge ok" }, t("workers.turns", { count: a.turns })));
    return h("div", { class: "card" },
      h("div", { class: "row" }, h("h2", { class: "grow" }, a.name), badge,
        h("button", { onclick: () => logs.show(a.name) }, t("workers.logs.open")),
        action(t("workers.restart"), "danger", t("workers.restart.confirm", { name: a.name }),
          () => api("POST", `workers/${encodeURIComponent(a.name)}/restart`))),
      h("h3", {}, t("workers.processes")), processes(a, now),
      h("h3", {}, t("workers.sessions")), sessions(a, now),
      egress(a));
  }

  async function load() {
    const data = await api("GET", "workers");
    logs.setAccounts(data.accounts.map(a => a.name));
    cards.replaceChildren(...(data.accounts.length ? data.accounts.map(a => card(a, data.now)) : [h("p", { class: "muted" }, t("workers.empty"))]));
  }
  return polling(container, 3000, load);
};

// ---- calls -----------------------------------------------------------

const PALETTE = ["#2563eb", "#16a34a", "#d97706", "#dc2626", "#7c3aed", "#0891b2", "#db2777", "#65a30d", "#9333ea", "#ea580c"];

function cachePct(input, cached) { return input > 0 ? cached / input : null; }

// tokensCell shows in/out over cache hit and reasoning, for a request or a
// sum of them.
function tokensCell(row) {
  if (!row.input && !row.output) return h("span", { class: "muted" }, "—");
  return stack(t("calls.tokens.in_out", { in: fmt.compact(row.input), out: fmt.compact(row.output) }),
    joined(t("calls.tokens.cached", { pct: fmt.pct(cachePct(row.input, row.cached)) }),
      row.reasoning ? t("calls.tokens.reasoning", { n: fmt.compact(row.reasoning) }) : ""));
}

// stackedBars draws one bar per day, stacked by series, as SVG.
function stackedBars(days, series, seriesName, valueOf, label) {
  const ns = "http://www.w3.org/2000/svg";
  const svg = document.createElementNS(ns, "svg");
  svg.setAttribute("class", "chart");
  const width = 900, height = 180, left = 50, bottom = 20;
  svg.setAttribute("viewBox", `0 0 ${width} ${height}`);
  svg.setAttribute("preserveAspectRatio", "none");
  const totals = days.map(d => series.reduce((sum, s) => sum + valueOf(d, s), 0));
  const top = Math.max(...totals, 0) || 1;
  const make = (tag, attrs, text) => {
    const node = document.createElementNS(ns, tag);
    for (const [k, v] of Object.entries(attrs)) node.setAttribute(k, v);
    if (text !== undefined) node.textContent = text;
    svg.append(node);
    return node;
  };
  make("line", { x1: left, y1: height - bottom, x2: width, y2: height - bottom, class: "axis" });
  make("text", { x: 2, y: 10 }, label(top));
  make("text", { x: 2, y: height - bottom }, label(0));
  const slot = (width - left) / Math.max(days.length, 1);
  const barWidth = Math.max(Math.min(slot * 0.7, 40), 1);
  days.forEach((day, i) => {
    let y = height - bottom;
    const x = left + i * slot + (slot - barWidth) / 2;
    series.forEach((s, j) => {
      const v = valueOf(day, s);
      if (v <= 0) return;
      const barHeight = (v / top) * (height - bottom - 12);
      y -= barHeight;
      make("rect", { x, y, width: barWidth, height: barHeight, fill: PALETTE[j % PALETTE.length] })
        .append(Object.assign(document.createElementNS(ns, "title"), { textContent: `${day} · ${seriesName(s)}: ${label(v)}` }));
    });
    const labelEvery = Math.ceil(days.length / 15);
    if (i % labelEvery === 0) make("text", { x: x, y: height - 6 }, day.slice(5));
  });
  return svg;
}

views.calls = container => {
  const state = { range: "7d", key: "", model: "", account: "", metric: "cost" };
  const filters = h("div", { class: "row" });
  const totals = h("div", { class: "card" });
  const chart = h("div", { class: "card" });
  const byKey = h("div", { class: "card" });
  const list = h("div", { class: "card" });
  container.append(h("h1", {}, t("calls.title")), h("div", { class: "card" }, filters), totals, chart, byKey, list);

  const ranges = { "24h": 1, "7d": 7, "30d": 30, "90d": 90 };
  const since = () => Date.now() - ranges[state.range] * 86400e3;
  const query = extra => {
    const q = new URLSearchParams({ since: String(Math.floor(since())), ...extra });
    for (const f of ["key", "model", "account"]) if (state[f]) q.set(f, state[f]);
    return q.toString();
  };

  async function renderFilters() {
    const values = await api("GET", "usage/filters?" + new URLSearchParams({ since: String(Math.floor(since())) }));
    // options are [value, label] pairs.
    const select = (field, options, all) => h("select", { onchange: e => { state[field] = e.target.value; reload(); } },
      h("option", { value: "" }, all), options.map(([v, label]) => h("option", { value: v, selected: v === state[field] }, label)));
    const plain = values => values.map(v => [v, v]);
    const metrics = [["cost", t("calls.metric.cost")], ["tokens", t("calls.metric.tokens")]];
    filters.replaceChildren(
      h("label", {}, t("calls.range"), h("select", { onchange: e => { state.range = e.target.value; reload(); } },
        Object.keys(ranges).map(r => h("option", { value: r, selected: r === state.range }, t("calls.range.last", { range: r }))))),
      h("label", {}, t("common.key"), select("key", values.key.map(k => [k.hash, keyText(k.name, k.hash)]), t("calls.all_keys"))),
      h("label", {}, t("common.model"), select("model", plain(values.model), t("calls.all_models"))),
      h("label", {}, t("common.account"), select("account", plain(values.account), t("calls.all_accounts"))),
      h("label", {}, t("calls.chart"), h("select", { onchange: e => { state.metric = e.target.value; renderChart(); } },
        metrics.map(([v, label]) => h("option", { value: v, selected: v === state.metric }, label)))),
      h("span", { class: "grow" }),
      h("button", { onclick: e => guarded(reload, e.target) }, t("common.refresh")));
  }

  const stat = (label, value, hint) => h("div", { title: hint || "" }, h("div", { class: "muted" }, label), h("div", { style: "font-size:18px;font-weight:600" }, value));

  // quota is the weekly-quota report: uses by key, account, and group, and
  // by group the accounts with that weekly bucket.
  let quota = { accounts: {}, uses: [] };
  // quotaText lists shares by group: "Gemini 0.05% · Claude + GPT 0.11%".
  const quotaText = (byGroup, digits) => joined(...Object.entries(byGroup)
    .map(([g, share]) => [groupName(g), share])
    .sort(([a], [b]) => a.localeCompare(b))
    .map(([name, share]) => `${name} ${fmt.pct(share, digits)}`));

  // accountQuota adds up each account's shares by group; shares of
  // different accounts stay apart, since plans differ in bucket size.
  function accountQuota() {
    const byAccount = {};
    for (const u of quota.uses) {
      const groups = byAccount[u.account] ||= {};
      groups[u.group] = (groups[u.group] || 0) + u.used;
    }
    const accounts = Object.keys(byAccount).sort();
    if (!accounts.length) return "—";
    return accounts.map(a => h("div", { class: "mono", style: "font-size:13px" }, `${a}: ${quotaText(byAccount[a], 2)}`));
  }

  // keyQuota shows the key's average share over the accounts with each
  // group's bucket (an account it did not use counts as 0), and each
  // account's share on hover.
  function keyQuota(hash) {
    const mine = quota.uses.filter(u => u.key_hash === hash);
    if (!mine.length) return null;
    const average = {};
    for (const u of mine) average[u.group] = (average[u.group] || 0) + u.used / quota.accounts[u.group].length;
    const byAccount = mine.map(u => `${u.account} · ${groupName(u.group)}: ${fmt.pct(u.used, 3)}`);
    return term(t("calls.quota_average", { quota: quotaText(average, 2) }), [t("calls.quota_average.hint"), ...byAccount].join("\n"));
  }

  // quotaCell is a request's attributed quota share, or why it has none.
  function quotaCell(r) {
    if (r.quota_used !== null) return t("calls.quota_share", { pct: fmt.pct(r.quota_used, 3) });
    if (r.quota_skip) return term(t("calls.quota_skipped"), lookup("quota_skip", r.quota_skip) || r.quota_skip);
    return term(t("calls.quota_pending"), t("calls.quota_pending.hint"));
  }

  function renderTotals(sums) {
    const sum = sums.reduce((a, s) => {
      for (const k of ["requests", "errors", "input", "cached", "output", "reasoning", "cost_usd", "unpriced"]) a[k] = (a[k] || 0) + s[k];
      return a;
    }, {});
    const price = fmt.usd(sum.cost_usd || 0) + (sum.unpriced ? " " + t("calls.total.unpriced", { count: sum.unpriced }) : "");
    totals.replaceChildren(h("div", { class: "row", style: "gap:28px" },
      stat(t("common.requests"), fmt.int(sum.requests || 0)),
      stat(t("calls.total.errors"), fmt.int(sum.errors || 0)),
      stat(t("calls.total.input"), fmt.compact(sum.input)),
      stat(t("calls.total.cached"), fmt.compact(sum.cached)),
      stat(t("calls.total.cache_hit"), fmt.pct(cachePct(sum.input, sum.cached))),
      stat(t("calls.total.output"), fmt.compact(sum.output)),
      stat(t("calls.total.price"), price),
      h("div", { title: t("calls.total.quota.hint") }, h("div", { class: "muted" }, t("calls.total.quota")), accountQuota())));
  }

  let dayKey = [];
  // localDay formats a time as the browser's YYYY-MM-DD, like the server's
  // tz-shifted grouping.
  const localDay = ms => {
    const d = new Date(ms);
    return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, "0")}-${String(d.getDate()).padStart(2, "0")}`;
  };

  function renderChart() {
    const days = [];
    for (let at = since(); localDay(at) <= localDay(Date.now()); at += 86400e3) {
      if (days[days.length - 1] !== localDay(at)) days.push(localDay(at));
    }
    const names = new Map(dayKey.map(s => [s.group.split("\x1f")[1], s.key_name]));
    const keys = [...names.keys()].sort((a, b) => names.get(a).localeCompare(names.get(b)) || a.localeCompare(b));
    const cell = new Map(dayKey.map(s => [s.group, s]));
    const valueOf = (day, key) => {
      const s = cell.get(day + "\x1f" + key);
      if (!s) return 0;
      return state.metric === "cost" ? s.cost_usd : s.input + s.output;
    };
    const label = state.metric === "cost" ? fmt.usd : fmt.compact;
    chart.replaceChildren(h("h2", {}, t("calls.per_day")),
      keys.length ? stackedBars(days, keys, k => keyText(names.get(k), k), valueOf, label) : h("p", { class: "muted" }, t("calls.no_calls")),
      h("div", { class: "legend" }, keys.map((k, i) => h("span", {}, h("i", { style: `background:${PALETTE[i % PALETTE.length]}` }), keyName(names.get(k), k)))));
  }

  function renderByKey(sums) {
    byKey.replaceChildren(h("h2", {}, t("calls.per_key")), table([
      { title: t("common.key"), cell: s => keyName(s.key_name, s.group) },
      { title: t("common.requests"), num: true, cell: s => stack(fmt.int(s.requests), s.errors ? h("span", { class: "bad" }, t("calls.failed", { count: s.errors })) : null) },
      { title: t("calls.tokens"), num: true, cell: s => tokensCell(s) },
      { title: t("calls.cost"), num: true, hint: t("calls.cost.hint"), cell: s => stack(
          [fmt.usd(s.cost_usd), s.unpriced ? term("*", t("calls.unpriced", { count: s.unpriced })) : ""],
          keyQuota(s.group)) },
    ], sums));
  }

  let rows = [];
  async function loadRows(more) {
    const before = more && rows.length ? { before: String(rows[rows.length - 1].id) } : {};
    const page = await api("GET", "requests?" + query({ limit: "100", ...before }));
    rows = more ? rows.concat(page) : page;
    list.replaceChildren(h("div", {}, h("h2", {}, t("calls.requests")), table([
      { title: t("common.time"), cell: r => stack(fmt.time(r.at),
          r.ttft_ms === null ? fmt.ms(r.duration_ms) : t("calls.time.first_output", { total: fmt.ms(r.duration_ms), first: fmt.ms(r.ttft_ms) })) },
      { title: t("calls.who"), cell: r => stack(keyName(r.key, r.key_hash),
          [r.account || t("calls.no_account"), " · ", r.api, r.stream ? term(" ⇢", t("calls.stream.hint")) : ""]) },
      { title: t("calls.model_mode"), cell: r => stack(
          [h("span", { title: r.slug }, r.model), r.effort ? term(" · " + r.effort, t("calls.effort.hint")) : ""],
          r.mode ? [term(r.mode, lookup("mode", r.mode)), r.replay_reason ? [" · ", term(r.replay_reason, lookup("replay", r.replay_reason))] : ""] : null) },
      { title: t("calls.status"), cell: r => r.status === "ok" ? term(r.finish || "ok", lookup("status", r.finish || "ok"))
          : h("span", { class: "bad", title: joined(lookup("status", r.status), r.error) }, r.status) },
      { title: t("calls.tokens"), num: true, cell: r => tokensCell(r) },
      { title: t("calls.cost"), num: true, hint: t("calls.cost.hint"), cell: r => !r.input && !r.output ? h("span", { class: "muted" }, "—") : stack(fmt.usd(r.cost_usd),
          quotaCell(r)) },
    ], rows),
    page.length === 100 ? h("button", { style: "margin-top:8px", onclick: e => guarded(() => loadRows(true), e.target) }, t("calls.load_more")) : null,
    h("p", { class: "muted" }, t("calls.requests.note"))));
  }

  async function reload() {
    const tz = String(-new Date().getTimezoneOffset());
    const [keySums, dayKeySums, quotaReport] = await Promise.all([
      api("GET", "usage?" + query({ by: "key", tz })), api("GET", "usage?" + query({ by: "day_key", tz })), api("GET", "usage/quota?" + query({}))]);
    dayKey = dayKeySums;
    quota = quotaReport;
    renderTotals(keySums);
    renderChart();
    renderByKey(keySums);
    await loadRows(false);
  }

  guarded(async () => { await renderFilters(); await reload(); });
};

// ---- logs ------------------------------------------------------------

const MAX_LOG_LINES = 5000;

views.logs = container => {
  const state = { level: "info", component: "", account: "", q: "", live: true };
  const box = h("div", { class: "logs" });
  const count = h("span", { class: "muted" });
  let source = null;
  let lines = 0;

  const filterInput = (field, placeholder) => h("input", { placeholder, value: state[field],
    onchange: e => { state[field] = e.target.value.trim(); restart(); } });
  const level = h("select", { onchange: e => { state.level = e.target.value; restart(); } },
    ["debug", "info", "warn", "error"].map(l => h("option", { value: l, selected: l === state.level }, l + "+")));
  const component = h("select", { onchange: e => { state.component = e.target.value; restart(); } },
    h("option", { value: "" }, t("logs.all_components")),
    ["gateway", "pool", "bridge", "usage", "control", "admin"].map(c => h("option", { value: c }, c)));
  const live = h("input", { type: "checkbox", checked: state.live, onchange: e => { state.live = e.target.checked; restart(); } });

  container.append(h("h1", {}, t("logs.title")),
    h("div", { class: "card row" },
      h("label", {}, t("logs.level"), level),
      h("label", {}, t("logs.component"), component),
      h("label", {}, t("common.account"), filterInput("account", t("logs.any"))),
      h("label", { class: "grow" }, t("logs.search"), filterInput("q", t("logs.search.placeholder"))),
      h("label", { style: "flex-direction:row;align-items:center" }, live, t("logs.follow")),
      h("button", { onclick: () => { box.replaceChildren(); lines = 0; count.textContent = ""; } }, t("logs.clear")),
      count),
    box);

  const params = () => {
    const q = new URLSearchParams({ level: state.level });
    for (const f of ["component", "account", "q"]) if (state[f]) q.set(f, state[f]);
    return q;
  };

  function append(entries) {
    const atBottom = box.scrollTop + box.clientHeight >= box.scrollHeight - 20;
    for (const e of entries) {
      const fields = e.attrs.filter(a => a.key !== "component").map(a => `${a.key}=${/\s/.test(a.value) ? JSON.stringify(a.value) : a.value}`).join(" ");
      box.append(h("div", { class: e.level }, `${fmt.clock(e.time)} ${e.level.padEnd(5)} ${e.attrs.find(a => a.key === "component")?.value || "-"} ${e.message} ${fields}`));
      lines++;
    }
    while (lines > MAX_LOG_LINES) { box.firstChild.remove(); lines--; }
    count.textContent = t("logs.lines", { count: lines });
    if (atBottom) box.scrollTop = box.scrollHeight;
  }

  function stop() { if (source) { source.close(); source = null; } }

  async function restart() {
    stop();
    box.replaceChildren();
    lines = 0;
    await guarded(async () => {
      append(await api("GET", "logs?" + params().toString() + "&limit=1000"));
      box.scrollTop = box.scrollHeight;
    });
    if (!state.live) return;
    // EventSource sends the session cookie; GET needs no CSRF token.
    source = new EventSource("/admin/api/logs/stream?" + params().toString());
    source.onmessage = msg => append([JSON.parse(msg.data)]);
    source.onerror = () => { count.textContent = t("logs.interrupted"); };
  }

  restart();
  return stop;
};
