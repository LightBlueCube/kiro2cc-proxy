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
  for (const child of children.flat()) {
    if (child === null || child === undefined || child === false) continue;
    node.append(child instanceof Node ? child : String(child));
  }
  return node;
}

let toastTimer;
function toast(message, bad) {
  const t = document.getElementById("toast");
  t.textContent = message;
  t.className = "show" + (bad ? " bad" : "");
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { t.className = ""; }, bad ? 6000 : 2500);
}

// guarded runs an async action, reporting failures as a toast.
async function guarded(action, button) {
  if (button) button.disabled = true;
  try { return await action(); }
  catch (err) { if (err.status !== 401) toast(err.message, true); }
  finally { if (button) button.disabled = false; }
}

const fmt = {
  int: n => (n ?? 0).toLocaleString("en-US"),
  compact: n => {
    n = n ?? 0;
    if (Math.abs(n) >= 1e9) return (n / 1e9).toFixed(2) + "B";
    if (Math.abs(n) >= 1e6) return (n / 1e6).toFixed(2) + "M";
    if (Math.abs(n) >= 1e4) return (n / 1e3).toFixed(1) + "k";
    return fmt.int(n);
  },
  usd: v => v === null || v === undefined ? "—" : "$" + (v < 0.01 && v > 0 ? v.toFixed(4) : v.toFixed(2)),
  pct: (v, digits = 1) => v === null || v === undefined ? "—" : (v * 100).toFixed(digits) + "%",
  time: s => s ? new Date(s).toLocaleString() : "—",
  clock: s => s ? new Date(s).toLocaleTimeString() : "—",
  ms: ms => ms === null || ms === undefined ? "—" : ms >= 10000 ? (ms / 1000).toFixed(0) + "s" : ms >= 1000 ? (ms / 1000).toFixed(1) + "s" : ms + "ms",
  until: s => {
    const ms = new Date(s) - Date.now();
    if (ms <= 0) return "now";
    const m = Math.round(ms / 60000);
    if (m < 60) return m + "m";
    const hrs = Math.floor(m / 60);
    return hrs < 48 ? `${hrs}h ${m % 60}m` : `${Math.floor(hrs / 24)}d ${hrs % 24}h`;
  },
};

// term marks a word whose meaning shows on hover.
const term = (text, hint) => hint ? h("span", { class: "term", title: hint }, text) : text;

// stack shows a main value over smaller secondary lines; empty lines are
// left out.
function stack(main, ...lines) {
  return h("div", {}, h("div", {}, main),
    lines.filter(l => l !== null && l !== undefined && l !== "").map(l => h("div", { class: "sub" }, l)));
}

// table renders rows; a column's hint explains its title on hover.
function table(columns, rows, rowClass) {
  return h("div", { class: "table-wrap" }, h("table", {},
    h("thead", {}, h("tr", {}, columns.map(c => h("th", { class: c.num ? "num" : "" }, term(c.title, c.hint))))),
    h("tbody", {}, rows.length === 0
      ? h("tr", {}, h("td", { colspan: columns.length, class: "muted" }, "Nothing yet."))
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
window.addEventListener("DOMContentLoaded", start);

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

const GROUP_NAMES = { "gemini": "Gemini", "claude-gpt": "Claude + GPT", "argon": "Gemini 4 Argon" };
const groupName = g => GROUP_NAMES[g] || g;

const LIMIT_HINT = "Limits cap a key's total quota use per quota group since the key was created; it never resets, so raise the limit "
  + "to allow more. 100% = one account's whole weekly bucket of that group; blank = unlimited. Use is the share attributed from quota "
  + "samples plus an estimate for calls not sampled yet. A turn already running when the key reaches its limit finishes; the key's "
  + "next request is refused.";

// limitInputs makes one percent field per quota group; blank = unlimited.
function limitInputs(groups, limits) {
  const inputs = Object.fromEntries(groups.map(g => [g, h("input", {
    type: "number", min: 0, max: 1000, step: "any", placeholder: "unlimited", style: "width:110px",
    value: limits[g] === undefined ? "" : String(+(limits[g] * 100).toFixed(4)),
  })]));
  return {
    nodes: groups.map(g => h("label", {}, `${groupName(g)} limit %`, inputs[g])),
    read: () => Object.fromEntries(groups.filter(g => inputs[g].value.trim() !== "").map(g => [g, Number(inputs[g].value) / 100])),
  };
}

function usageCell(k, groups) {
  const shown = groups.filter(g => k.limits[g] !== undefined || k.usage[g]);
  if (!shown.length) return h("span", { class: "muted" }, "unlimited · unused");
  return shown.map(g => {
    const u = k.usage[g] || { attributed: 0, estimated: 0 };
    const used = u.attributed + u.estimated;
    const limit = k.limits[g];
    const title = `${fmt.pct(u.attributed, 2)} attributed + ${fmt.pct(u.estimated, 2)} estimated (calls not sampled yet)`;
    if (limit === undefined) return h("div", { title }, `${groupName(g)}: ${fmt.pct(used, 2)} · unlimited`);
    const level = used >= limit ? "low" : used >= limit * 0.8 ? "mid" : "";
    const fill = limit > 0 ? Math.min(100, (used / limit) * 100) : 100;
    return h("div", { class: "row", title },
      h("span", { style: "width:120px" }, groupName(g)),
      h("div", { class: "bar " + level, style: "width:120px" }, h("span", { style: `width:${fill}%` })),
      h("span", { class: "mono" }, `${fmt.pct(used, 2)} / ${fmt.pct(limit, 1)}`));
  });
}

// modelPicker lists the catalog's models as checkboxes; none checked
// allows every model. Selected ids missing from the catalog stay listed,
// so saving does not drop them.
function modelPicker(catalog, selected) {
  const names = Object.fromEntries(catalog.map(m => [m.id, m.name]));
  const ids = [...new Set([...catalog.map(m => m.id), ...selected])];
  const boxes = ids.map(id => h("input", { type: "checkbox", value: id, checked: selected.includes(id) }));
  return {
    node: h("div", { class: "picker" },
      ids.length ? ids.map((id, i) => h("label", { class: "check", title: names[id] ? id : "Not in the current model list" },
        boxes[i], names[id] ? `${names[id]} ` : "", h("span", { class: "mono" }, id)))
        : h("span", { class: "muted" }, "No models known yet: add an account first.")),
    read: () => boxes.filter(b => b.checked).map(b => b.value),
  };
}

const MODELS_HINT = "None checked = every model, including ones added later.";

views.keys = container => {
  const created = h("div");
  const editor = h("div");
  const list = h("div");
  let groups = [];
  let catalog = [];
  let newLimits = null;
  let newModels = null;

  const name = h("input", { required: true, maxlength: 64, placeholder: "alice" });
  const note = h("input", { placeholder: "optional" });
  const modelsBox = h("div");
  const limitsRow = h("div", { class: "row" });
  const form = h("form", { onsubmit: event => {
    event.preventDefault();
    guarded(async () => {
      const data = await api("POST", "keys", { name: name.value, note: note.value, models: newModels.read(), limits: newLimits.read() });
      created.replaceChildren(h("div", { class: "card" },
        h("h2", {}, `Key for ${name.value}`),
        h("p", { class: "muted" }, "Copy it now. It is shown only once; the server keeps only its hash."),
        h("div", { class: "secret" }, data.key)));
      name.value = note.value = "";
      newLimits = newModels = null;
      await load();
    }, event.submitter);
  } },
    h("div", { class: "row" },
      h("label", {}, "Name", name),
      h("label", { class: "grow" }, "Note", note)),
    h("div", { class: "field", title: MODELS_HINT }, "Allowed models (none checked = all)", modelsBox),
    limitsRow,
    h("p", { class: "muted" }, LIMIT_HINT),
    h("button", { type: "submit" }, "Create key"));

  async function load() {
    const [data, models] = await Promise.all([api("GET", "keys"), api("GET", "models")]);
    groups = data.groups;
    catalog = models;
    if (!newLimits) {
      newLimits = limitInputs(groups, {});
      limitsRow.replaceChildren(...newLimits.nodes);
    }
    if (!newModels) {
      newModels = modelPicker(catalog, []);
      modelsBox.replaceChildren(newModels.node);
    }
    list.replaceChildren(table([
      { title: "Name", cell: k => k.name },
      { title: "Note", cell: k => k.note || h("span", { class: "muted" }, "—") },
      { title: "Models", cell: k => k.models.length ? k.models.join(", ") : h("span", { class: "muted" }, "all") },
      { title: "Quota used (total) / limit", hint: LIMIT_HINT, cell: k => usageCell(k, groups) },
      { title: "Created", cell: k => fmt.time(k.created_at) },
      { title: "", cell: k => k.revoked_at ? `revoked ${fmt.time(k.revoked_at)}` : h("span", { class: "row" },
          h("button", { onclick: () => editKey(k) }, "Edit"),
          h("button", { class: "danger", onclick: e => revoke(k, e.target) }, "Revoke")) },
    ], data.keys, k => k.revoked_at ? "revoked" : ""));
  }

  function editKey(k) {
    const noteInput = h("input", { value: k.note });
    const models = modelPicker(catalog, k.models);
    const limits = limitInputs(groups, k.limits);
    const save = event => {
      event.preventDefault();
      guarded(async () => {
        await api("PATCH", `keys/${k.id}`, { note: noteInput.value, models: models.read(), limits: limits.read() });
        editor.replaceChildren();
        toast("Saved");
        await load();
      }, event.submitter);
    };
    editor.replaceChildren(h("div", { class: "card" },
      h("h2", {}, `Edit ${k.name}`),
      h("form", { onsubmit: save },
        h("div", { class: "row" },
          h("label", { class: "grow" }, "Note", noteInput)),
        h("div", { class: "field", title: MODELS_HINT }, "Allowed models (none checked = all)", models.node),
        h("div", { class: "row" }, limits.nodes),
        h("div", { class: "row", style: "margin-top:8px" },
          h("button", { type: "submit", class: "primary" }, "Save"),
          h("button", { type: "button", onclick: () => editor.replaceChildren() }, "Cancel")))));
  }

  function revoke(k, button) {
    if (!confirm(`Revoke the key ${k.name}? Clients using it stop working at once.`)) return;
    guarded(async () => { await api("POST", `keys/${k.id}/revoke`); await load(); }, button);
  }

  container.append(h("h1", {}, "API keys"), h("div", { class: "card" }, form), created, editor, h("div", { class: "card" }, list));
  guarded(load);
};

// ---- settings --------------------------------------------------------

// resetButton puts a field back to its default value; it is disabled while
// the field holds the default. The change applies on save.
function resetButton(input, value) {
  const isBox = input.type === "checkbox";
  const current = () => isBox ? input.checked : input.value;
  const button = h("button", { type: "button", class: "reset", title: `Reset to the default: ${value === "" ? "empty" : value}` }, "↺");
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
  const reset = h("button", { type: "button", class: "reset", title: "Put back the default rows" }, "↺ Defaults");
  const sync = () => { reset.disabled = JSON.stringify(read()) === JSON.stringify(defaults); };
  const add = row => {
    const inputs = fields.map(f => f.input(row[f.key]));
    const tr = h("tr", {}, inputs.map(i => h("td", {}, i)),
      h("td", {}, h("button", { type: "button", class: "danger", onclick: () => { tr.remove(); sync(); } }, "Remove")));
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
  container.append(h("h1", {}, "Settings"), general, password, sessions, audit);

  async function loadGeneral() {
    const [s, catalog] = await Promise.all([api("GET", "settings"), api("GET", "models")]);
    const d = s.defaults;
    const turns = h("input", { type: "number", min: 1, max: 64, value: s.turns_per_account });
    const ttl = h("input", { value: s.snapshot_ttl, pattern: "[0-9hms.]+" });
    const idleClose = h("input", { value: s.session_idle_close, pattern: "[0-9hms.]+" });
    const level = h("select", {}, ["debug", "info", "warn", "error"].map(l => h("option", { value: l, selected: l === s.log_level }, l)));
    const retention = h("input", { type: "number", min: 1, max: 3650, value: s.retention_days });
    const fallback = h("input", { value: s.fallback_proxy, placeholder: "http://127.0.0.1:7890" });
    const direct = h("input", { type: "checkbox", checked: s.allow_direct });
    const processMode = h("select", {}, [["per_account", "One process per account"], ["per_session", "One worker process per session"]]
      .map(([value, label]) => h("option", { value, selected: value === s.process_mode }, label)));

    const modelInput = value => h("input", { value, list: "settings-models", required: true });
    const number = (min, step) => value => h("input", { type: "number", min, step, value, style: "width:110px" });
    const prices = rowEditor([
      { key: "model", title: "Model", input: modelInput, read: i => i.value.trim() },
      { key: "input", title: "Input", input: number(0, "any"), read: i => Number(i.value) },
      { key: "cached", title: "Cached input", input: number(0, "any"), read: i => Number(i.value) },
      { key: "cache_write", title: "Cache write", input: number(0, "any"), read: i => Number(i.value) },
      { key: "output", title: "Output", input: number(0, "any"), read: i => Number(i.value) },
    ], s.prices, d.prices, { model: "", input: 0, cached: 0, cache_write: 0, output: 0 }, "Add price");
    const windows = rowEditor([
      { key: "model", title: "Model", input: modelInput, read: i => i.value.trim() },
      { key: "tokens", title: "Context window (tokens)", input: number(1, 1), read: i => Number(i.value) },
    ], s.context_windows, d.context_windows, { model: "", tokens: 200000 }, "Add window");

    const form = h("form", { onsubmit: event => {
      event.preventDefault();
      guarded(async () => {
        await api("PUT", "settings", {
          turns_per_account: Number(turns.value), snapshot_ttl: ttl.value, session_idle_close: idleClose.value, log_level: level.value,
          retention_days: Number(retention.value), prices: prices.read(), context_windows: windows.read(),
          fallback_proxy: fallback.value.trim(), allow_direct: direct.checked, process_mode: processMode.value,
        });
        toast("Saved");
        await loadGeneral();
      }, event.submitter);
    } },
      h("datalist", { id: "settings-models" }, catalog.map(m => h("option", { value: m.id }, m.name))),
      h("div", { class: "row" },
        h("label", {}, "Concurrent turns per account", withReset(turns, d.turns_per_account)),
        h("label", {}, "Idle session reuse (snapshot TTL)", withReset(ttl, d.snapshot_ttl)),
        h("label", { title: "An idle session is closed in agy after this; under process-per-session its worker (~250 MiB) exits. The conversation's next request reopens it from disk in a few seconds." },
          "Close idle sessions after", withReset(idleClose, d.session_idle_close)),
        h("label", {}, "Log level", withReset(level, d.log_level)),
        h("label", {}, "Keep call records (days)", withReset(retention, d.retention_days))),
      h("div", { class: "row" },
        h("label", { title: "Per account: one agy process per account and client tool set serves every session; starts sessions faster and uses less memory. Per session: each session gets its own worker process (~250 MiB), isolating sessions from each other." },
          "Process mode", withReset(processMode, d.process_mode))),
      h("p", { class: "muted" }, "Changing the mode applies at once: idle agy processes exit, busy ones after their running turns, and conversations reopen in new processes on their next request."),
      h("h3", {}, "Outbound traffic"),
      h("div", { class: "row" },
        h("label", { class: "grow", title: "Carries the traffic of accounts without their own proxy" }, "Fallback HTTP proxy", withReset(fallback, d.fallback_proxy)),
        h("span", { class: "with-reset" }, h("label", { class: "check" }, direct, "Allow direct connections for accounts with no proxy at all"), resetButton(direct, d.allow_direct))),
      h("p", { class: "muted" }, "Every agy connects only to agyrelay's local egress proxy, which forwards through the account's proxy, else the fallback proxy, else directly when allowed. Otherwise the connection is refused and the account is blocked, so a forgotten proxy never turns into a direct connection. Leave direct off unless this server is abroad."),
      h("details", { class: "fold" },
        h("summary", {}, `Context windows (${s.context_windows.length} models, tokens)`),
        h("p", { class: "muted" }, "A request whose prompt is estimated above its model's window fails with context_length_exceeded before it reaches an account, and clients such as opencode and pi then compact the conversation. Models without a row are left to upstream's limit. The defaults are upstream's limits (gpt-oss: 131072 minus the 32768 output tokens agy reserves). The estimate counts with each model family's tokenizer plus agy's own prompt and tool declarations, and runs above upstream's count: by about 1-2k tokens on Gemini and gpt-oss, and up to 15% on Claude, whose tokenizer is approximated."),
        windows.node),
      h("details", { class: "fold" },
        h("summary", {}, `API list prices (${s.prices.length} models, USD per 1M tokens)`),
        h("p", { class: "muted" }, "Used for the list-price estimate of each call. Input is uncached prompt tokens, output includes reasoning. Defaults are the vendors' published prices as of 2026-11-10; gpt-oss-120b uses Google Vertex's price as listed on OpenRouter. agy does not report cache writes: when a model has a cache-write price, all its uncached input is priced at it (close for multi-turn use, high for a first turn); 0 = no cache-write price."),
        prices.node),
      h("div", { class: "row", style: "margin-top:12px" },
        h("span", { class: "muted grow" }, "↺ puts a field back to its default; Save applies the changes."),
        h("button", { type: "submit" }, "Save settings")));
    general.replaceChildren(h("h2", {}, "General"), form);
  }

  function renderPassword() {
    const current = h("input", { type: "password", autocomplete: "current-password", required: true });
    const next = h("input", { type: "password", autocomplete: "new-password", minlength: 12, required: true });
    const again = h("input", { type: "password", autocomplete: "new-password", required: true });
    const form = h("form", { class: "row", onsubmit: event => {
      event.preventDefault();
      if (next.value !== again.value) { toast("The new passwords differ", true); return; }
      guarded(async () => {
        const data = await api("POST", "password", { current: current.value, new: next.value });
        csrf = data.csrf;
        current.value = next.value = again.value = "";
        toast("Password changed; other sessions were signed out");
        await loadSessions();
      }, event.submitter);
    } },
      h("label", {}, "Current", current), h("label", {}, "New (12+ characters)", next), h("label", {}, "New again", again),
      h("button", { type: "submit" }, "Change password"));
    password.replaceChildren(h("h2", {}, "Admin password"), form);
  }

  async function loadSessions() {
    const list = await api("GET", "sessions");
    sessions.replaceChildren(h("h2", {}, "Signed-in browsers"), table([
      { title: "Last seen", cell: s => fmt.time(s.seen_at) },
      { title: "IP", cell: s => s.ip },
      { title: "Browser", cell: s => h("span", { class: "mono" }, s.user_agent) },
      { title: "Expires", cell: s => fmt.time(s.expires_at) },
      { title: "", cell: s => s.current ? h("span", { class: "muted" }, "this browser")
          : h("button", { class: "danger", onclick: e => guarded(async () => { await api("DELETE", `sessions/${s.id}`); await loadSessions(); }, e.target) }, "Sign out") },
    ], list));
  }

  async function loadAudit() {
    const entries = await api("GET", "audit");
    audit.replaceChildren(h("h2", {}, "Audit log"), table([
      { title: "Time", cell: e => fmt.time(e.at) },
      { title: "IP", cell: e => e.ip },
      { title: "Action", cell: e => h("span", { class: e.action.endsWith("failed") ? "bad" : "" }, e.action) },
      { title: "Detail", cell: e => e.detail },
    ], entries));
  }

  renderPassword();
  guarded(loadGeneral);
  guarded(loadSessions);
  guarded(loadAudit);
};

// ---- accounts --------------------------------------------------------

const BUCKET_WINDOWS = {
  "5h": "Short-term bucket with a 5-hour window. Requests fail while it is empty, but key limits and the Quota column do not use it",
  weekly: "Weekly bucket: key limits and the Quota column are shares of it",
};

views.accounts = container => {
  const cards = h("div", { class: "grid" });
  const signins = new Map(); // account name → {url, code}

  const name = h("input", { required: true, maxlength: 64, placeholder: "main", pattern: "[A-Za-z0-9][A-Za-z0-9_.\\-]*" });
  const proxy = h("input", { placeholder: "http://127.0.0.1:7890" });
  const env = h("textarea", { rows: 1, placeholder: "NAME=value (optional, one per line)" });
  const form = h("form", { class: "row", onsubmit: event => {
    event.preventDefault();
    guarded(async () => {
      await api("POST", "accounts", { name: name.value.trim(), proxy: proxy.value, env: env.value.split("\n").map(s => s.trim()).filter(Boolean) });
      const added = name.value.trim();
      name.value = proxy.value = env.value = "";
      await startSignin(added);
    }, event.submitter);
  } },
    h("label", {}, "Name", name),
    h("label", { title: "agy's traffic, sign-in included, goes through this HTTP proxy (e.g. mihomo's HTTP port)" }, "HTTP proxy", proxy),
    h("label", { class: "grow" }, "Environment for its agy", env),
    h("button", { type: "submit" }, "Add and sign in"));

  function editProxy(a, button) {
    const value = prompt(`HTTP proxy for ${a.name}, e.g. http://127.0.0.1:7890 (empty = the fallback in Settings).\n`
      + "Open connections close and agy reconnects through the new route.", a.proxy);
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
      toast(`Signed in as ${data.email}; sampling quota…`);
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
    const code = h("input", { class: "grow", placeholder: "Paste the code or the whole URL of the page you landed on", value: state.code,
      oninput: e => { state.code = e.target.value; } });
    return h("div", { class: "card" },
      h("h3", {}, "Sign in"),
      h("ol", {},
        h("li", {}, h("a", { href: state.url, target: "_blank", rel: "noopener noreferrer" }, "Open the Google sign-in"), " and sign in with the account to add."),
        h("li", {}, "Google sends you to antigravity.google. Copy that page's full URL (or the code it shows) and paste it here.")),
      h("div", { class: "row" }, code,
        h("button", { class: "primary", onclick: e => submitSignin(a.name, e.target) }, "Finish"),
        h("button", { onclick: () => cancelSignin(a.name) }, "Cancel")));
  }

  function status(a) {
    if (a.paused) return h("span", { class: "badge warn" }, "paused");
    if (a.disabled) return h("span", { class: "badge bad", title: a.disabled }, "needs sign-in");
    if (a.blocked) return h("span", { class: "badge bad", title: a.blocked }, "no proxy");
    if (Object.keys(a.cooldowns).length) return h("span", { class: "badge warn" }, "cooling down");
    if (!a.user) return h("span", { class: "badge muted" }, "not checked");
    return h("span", { class: "badge ok" }, "ready");
  }

  function quota(a) {
    if (!a.quota || !a.quota.length) return h("p", { class: "muted" }, a.sample_error ? "" : "No quota sample yet. Refresh to check the account.");
    return a.quota.map(g => h("div", {},
      h("h3", { title: g.description || "" }, g.name),
      g.buckets.map(b => {
        const level = b.remaining <= 0.1 ? "low" : b.remaining <= 0.35 ? "mid" : "";
        return h("div", { class: "row", title: b.name },
          h("span", { class: "muted", style: "width:52px" }, term(b.window, BUCKET_WINDOWS[b.window])),
          h("div", { class: "bar grow " + level }, h("span", { style: `width:${Math.max(0, Math.min(100, b.remaining * 100))}%` })),
          h("span", { class: "mono", style: "width:150px;text-align:right" }, `${fmt.pct(b.remaining, 1)} left · ${fmt.until(b.reset_at)}`));
      })));
  }

  function card(a) {
    const enc = encodeURIComponent(a.name);
    const act = (label, action, cls) => h("button", { class: cls || "", onclick: e => guarded(async () => { await action(); await load(); }, e.target) }, label);
    return h("div", { class: "card" },
      h("div", { class: "row" }, h("h2", { class: "grow" }, a.name), status(a)),
      a.user ? h("p", {}, `${a.user.email} · ${a.user.tier || a.user.plan}`) : null,
      a.disabled ? h("p", { class: "bad" }, a.disabled) : null,
      a.blocked ? h("p", { class: "bad" }, a.blocked) : null,
      Object.entries(a.cooldowns).map(([group, until]) => h("p", { class: "warn" }, `${group} quota cooling down for ${fmt.until(until)}`)),
      a.sample_error ? h("p", { class: "bad mono" }, `Quota check failed: ${a.sample_error}`) : null,
      h("p", { class: "muted" }, `${a.turns} turn(s) running · ${a.procs} agy process(es) · quota checked ${a.sampled_at ? fmt.time(a.sampled_at) : "never"}`
        + ` · traffic: ${a.route || "unknown"}${a.proxy ? "" : " (no own proxy)"}`
        + (a.env_names.length ? ` · env: ${a.env_names.join(", ")}` : "")),
      quota(a),
      signinPanel(a),
      h("div", { class: "row", style: "margin-top:12px" },
        signins.has(a.name) ? null : h("button", { onclick: e => startSignin(a.name, e.target) }, a.user || a.disabled ? "Sign in again" : "Sign in"),
        act("Refresh quota", () => api("POST", `accounts/${enc}/refresh`)),
        h("button", { onclick: e => editProxy(a, e.target) }, "Proxy…"),
        a.paused ? act("Resume", () => api("POST", `accounts/${enc}/resume`)) : act("Pause", () => api("POST", `accounts/${enc}/pause`)),
        h("span", { class: "grow" }),
        h("button", { class: "danger", onclick: e => {
          if (!confirm(`Remove ${a.name}? Its running turns fail. Its files stay on disk, so adding it again under the same name keeps its sign-in.`)) return;
          guarded(async () => { await api("DELETE", `accounts/${enc}`); await load(); }, e.target);
        } }, "Remove")));
  }

  async function load() {
    const accounts = await api("GET", "accounts");
    for (const a of accounts) if (a.login_pending === false) signins.delete(a.name);
    cards.replaceChildren(...(accounts.length ? accounts.map(card) : [h("p", { class: "muted" }, "No accounts yet. Add one above.")]));
  }

  container.append(h("h1", {}, "Accounts"), h("div", { class: "card" }, form), cards);
  // Re-render on a timer unless a sign-in is being typed.
  return polling(container, 10000, async () => { if (signins.size === 0) await load(); });
};

// ---- workers ---------------------------------------------------------

const bytes = n => n >= 1 << 30 ? (n / (1 << 30)).toFixed(2) + " GiB" : n >= 1 << 20 ? (n / (1 << 20)).toFixed(0) + " MiB" : (n / 1024).toFixed(0) + " KiB";

// span formats the time from a to b (ISO strings or Dates) as "3m 20s".
function span(a, b) {
  const s = Math.max(0, Math.round((new Date(b) - new Date(a)) / 1000));
  if (s < 60) return s + "s";
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ${s % 60}s`;
  const hrs = Math.floor(m / 60);
  return hrs < 48 ? `${hrs}h ${m % 60}m` : `${Math.floor(hrs / 24)}d ${hrs % 24}h`;
}

const PROCESS_KINDS = {
  agy: "One agy process (per account and client tool set) that runs all its sessions",
  front: "Per-session mode: the front process hands each session to its own worker process",
  worker: "A worker process of the per-session mode, running the sessions listed",
};

const TURN_STATES = {
  idle: "No turn running; the session waits for its conversation's next request",
  starting: "The turn is being set up: the session is opening, resuming, or rewinding",
  running: "The model is working and a client is reading the response",
  "waiting for tool results": "The turn called client tools and waits for the request that carries their results",
  "running unobserved": "No client is reading the turn, e.g. after its client disconnected; it is being cancelled or handed to the next request",
  closing: "The idle session is being closed in agy",
};

views.workers = container => {
  const cards = h("div");
  container.append(h("h1", {}, "Workers"), cards);

  const action = (label, cls, confirmText, run) => h("button", { class: cls, onclick: e => {
    if (confirmText && !confirm(confirmText)) return;
    guarded(async () => { await run(); await load(); }, e.target);
  } }, label);

  function processes(a, now) {
    if (!a.procs.length) return h("p", { class: "muted" }, "No agy running. The next request or quota refresh starts one.");
    const rows = [];
    for (const p of a.procs) {
      rows.push({ kind: p.per_session ? "front" : "agy", pid: p.pid, agent: p.agent, up: span(p.started, now), rss: p.rss_bytes, cpu: p.cpu_seconds, sessions: "", error: p.error });
      for (const w of p.workers) {
        rows.push({ kind: `worker ${w.number}`, pid: w.pid, agent: "", up: "", rss: w.rss_bytes, cpu: w.cpu_seconds, sessions: w.sessions.map(id => id.slice(0, 8)).join(", ") });
      }
    }
    const total = rows.reduce((sum, r) => sum + r.rss, 0);
    return [
      table([
        { title: "Process", cell: r => term(r.kind, PROCESS_KINDS[r.kind.split(" ")[0]]) },
        { title: "PID", num: true, cell: r => r.pid },
        { title: "Profile", cell: r => h("span", { class: "mono" }, r.agent) },
        { title: "Up", cell: r => r.up },
        { title: "Memory", num: true, cell: r => bytes(r.rss) },
        { title: "CPU", num: true, cell: r => r.cpu.toFixed(1) + "s" },
        { title: "Sessions", cell: r => h("span", { class: "mono" }, r.sessions) },
        { title: "", cell: r => r.error ? h("span", { class: "bad" }, r.error) : "" },
      ], rows),
      h("p", { class: "muted" }, `${rows.length} process(es), ${bytes(total)} resident in total.`),
    ];
  }

  function sessions(a, now) {
    const enc = encodeURIComponent(a.name);
    const where = s => s.worker ? term(`worker ${s.worker}`, PROCESS_KINDS.worker)
      : s.pid ? term("in process", "Open in the account's agy process")
      : term("closed", "Closed in agy after being idle; its next request reopens it from disk");
    const turnTime = s => !s.turn || !s.turn.started ? "" : s.turn.parked_at
      ? `${span(s.turn.started, now)} · waiting ${span(s.turn.parked_at, now)}, cancelled in ${span(now, s.turn.park_timeout)}`
      : span(s.turn.started, now);
    return table([
      { title: "Session · process", cell: s => stack(h("span", { class: "mono", title: s.id }, s.id.slice(0, 8)), where(s)) },
      { title: "Key", cell: s => s.key },
      { title: "State · turn", cell: s => !s.turn ? stack(term("idle", TURN_STATES.idle), `last used ${span(s.last_used, now)} ago`)
          : stack(h("span", { class: (s.turn.state.startsWith("waiting") ? "warn" : "ok") + " term", title: TURN_STATES[s.turn.state] || "" }, s.turn.state),
            [s.turn.model, s.turn.model && turnTime(s) ? " · " : "", turnTime(s)]) },
      { title: "", cell: s => {
          const path = `workers/${enc}/sessions/${encodeURIComponent(s.id)}`;
          if (s.turn) return action("Cancel turn", "danger", `Cancel the turn of ${s.key}? Its client gets an error.`, () => api("POST", path + "/cancel"));
          if (!s.pid) return "";
          return action("Close", "", "", () => api("POST", path + "/close"));
        } },
    ], a.sessions);
  }

  function egress(a) {
    const e = a.egress;
    if (!e) return null;
    return h("div", {},
      h("h3", {}, "Outbound"),
      h("p", {}, `Route: ${e.route} · ${e.open} open · ${fmt.int(e.connections)} connection(s) · ${bytes(e.bytes_out)} out / ${bytes(e.bytes_in)} in`
        + (e.refused ? ` · ${fmt.int(e.refused)} refused` : "")),
      e.last_error ? h("p", { class: "bad mono" }, `${fmt.time(e.last_error_at)}: ${e.last_error}`) : null,
      e.hosts.length ? h("p", { class: "muted mono" }, e.hosts.map(x => `${x.host} ×${x.connections}`).join("  ·  ")) : null);
  }

  function card(a, now) {
    const badge = a.paused ? h("span", { class: "badge warn" }, "paused")
      : a.disabled ? h("span", { class: "badge bad" }, "needs sign-in")
      : a.blocked ? h("span", { class: "badge bad" }, "no proxy")
      : h("span", { class: "badge ok" }, `${a.turns} turn(s)`);
    return h("div", { class: "card" },
      h("div", { class: "row" }, h("h2", { class: "grow" }, a.name), badge,
        action("Restart agy", "danger", `Restart ${a.name}'s agy? Its running turns fail; sessions reopen from disk on their next request.`,
          () => api("POST", `workers/${encodeURIComponent(a.name)}/restart`))),
      h("h3", {}, "Processes"), processes(a, now),
      h("h3", {}, "Sessions"), sessions(a, now),
      egress(a));
  }

  async function load() {
    const data = await api("GET", "workers");
    cards.replaceChildren(...(data.accounts.length ? data.accounts.map(a => card(a, data.now)) : [h("p", { class: "muted" }, "No accounts.")]));
  }
  return polling(container, 3000, load);
};

// ---- calls -----------------------------------------------------------

// How a request reached its agy session.
const MODES = {
  continue: "Returned tool results to the turn waiting for them; the same agy turn went on",
  warm: "Reused the agy session that holds this conversation; only the new messages were sent",
  rewind: "Rewound an agy session that holds a later version of this conversation (the client edited or dropped messages) to the shared part, then sent the rest",
  fresh: "A new conversation: a new agy session with nothing to replay",
  replay: "A new agy session that got the whole history as text, because no session could be reused (see the reason)",
};

// Request outcomes: finish reasons of successes, then error statuses.
const STATUSES = {
  stop: "The model finished its reply",
  tool_calls: "The reply ended on tool calls for the client to run; the turn waits for their results",
  length: "The reply hit the output token limit",
  ok: "Succeeded",
  context_length: "The prompt is larger than the model's context window (agyrelay's estimate or upstream's error); clients compact the conversation",
  quota: "The account's quota ran out; tried on another account first when there was one",
  key_limit: "The API key used up its quota limit",
  overloaded: "Upstream is over capacity",
  unavailable: "agyrelay could not serve it: no account free, or an internal check failed",
  upstream_error: "agy or the model failed; hover the row for the message",
  invalid_request: "The request was malformed",
  unauthorized: "Bad or missing API key",
  forbidden: "The API key may not use this model",
  unknown_model: "No such model",
};

const STREAM_HINT = "Streamed (stream: true); without the arrow the client waited for the whole response";

// Why a request with history replayed it into a new session.
const replayReasons = {
  no_snapshot: "No session holds this conversation's history: new to the relay, idle past the snapshot TTL, or the history was edited",
  tools_changed: "The client's tool set differs from the session's",
  warm_failed: "The session could not be reopened or rewound",
  retry: "An earlier attempt failed on another account",
};

const PALETTE = ["#2563eb", "#16a34a", "#d97706", "#dc2626", "#7c3aed", "#0891b2", "#db2777", "#65a30d", "#9333ea", "#ea580c"];

function cachePct(input, cached) { return input > 0 ? cached / input : null; }

const TOKENS_HINT = "Prompt (input) and output tokens; below, the share of the prompt read from the cache and the output spent on reasoning";
const COST_HINT = "What the tokens would cost at the vendors' API list prices (Settings), and the share of one account's weekly quota they used";

// tokensCell shows in/out over cache hit and reasoning, for a request or a
// sum of them.
function tokensCell(t) {
  if (!t.input && !t.output) return h("span", { class: "muted" }, "—");
  return stack(`${fmt.compact(t.input)} in · ${fmt.compact(t.output)} out`,
    [`${fmt.pct(cachePct(t.input, t.cached))} cached`, t.reasoning ? ` · ${fmt.compact(t.reasoning)} reasoning` : ""]);
}

// stackedBars draws one bar per day, stacked by series, as SVG.
function stackedBars(days, series, valueOf, label) {
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
        .append(Object.assign(document.createElementNS(ns, "title"), { textContent: `${day} · ${s}: ${label(v)}` }));
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
  container.append(h("h1", {}, "Calls"), h("div", { class: "card" }, filters), totals, chart, byKey, list);

  const ranges = { "24h": 1, "7d": 7, "30d": 30, "90d": 90 };
  const since = () => Date.now() - ranges[state.range] * 86400e3;
  const query = extra => {
    const q = new URLSearchParams({ since: String(Math.floor(since())), ...extra });
    for (const f of ["key", "model", "account"]) if (state[f]) q.set(f, state[f]);
    return q.toString();
  };

  async function renderFilters() {
    const values = await api("GET", "usage/filters?" + new URLSearchParams({ since: String(Math.floor(since())) }));
    const select = (field, options, all) => h("select", { onchange: e => { state[field] = e.target.value; reload(); } },
      h("option", { value: "" }, all), options.map(v => h("option", { value: v, selected: v === state[field] }, v)));
    filters.replaceChildren(
      h("label", {}, "Range", h("select", { onchange: e => { state.range = e.target.value; reload(); } },
        Object.keys(ranges).map(r => h("option", { value: r, selected: r === state.range }, "last " + r)))),
      h("label", {}, "Key", select("key", values.key, "all keys")),
      h("label", {}, "Model", select("model", values.model, "all models")),
      h("label", {}, "Account", select("account", values.account, "all accounts")),
      h("label", {}, "Chart", h("select", { onchange: e => { state.metric = e.target.value; renderChart(); } },
        [["cost", "list price"], ["tokens", "tokens (in + out)"], ["quota", "weekly quota"]].map(([v, t]) => h("option", { value: v, selected: v === state.metric }, t)))),
      h("span", { class: "grow" }),
      h("button", { onclick: e => guarded(reload, e.target) }, "Refresh"));
  }

  function renderTotals(sums) {
    const t = sums.reduce((a, s) => {
      for (const k of ["requests", "errors", "input", "cached", "output", "reasoning", "cost_usd", "unpriced", "quota_used"]) a[k] = (a[k] || 0) + s[k];
      return a;
    }, {});
    const stat = (label, value, hint) => h("div", { title: hint || "" }, h("div", { class: "muted" }, label), h("div", { style: "font-size:18px;font-weight:600" }, value));
    totals.replaceChildren(h("div", { class: "row", style: "gap:28px" },
      stat("Requests", fmt.int(t.requests || 0)),
      stat("Errors", fmt.int(t.errors || 0)),
      stat("Input", fmt.compact(t.input)),
      stat("Cached", fmt.compact(t.cached)),
      stat("Cache hit", fmt.pct(cachePct(t.input, t.cached))),
      stat("Output", fmt.compact(t.output)),
      stat("API list price", fmt.usd(t.cost_usd || 0) + (t.unpriced ? ` (+${t.unpriced} unpriced)` : ""), "What these tokens would cost at the vendors' API list prices (Settings → Prices)"),
      stat("Weekly quota used", fmt.pct(t.quota_used || 0, 2), "Sum of attributed weekly-quota shares; 100% = one account's whole weekly bucket")));
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
    for (let t = since(); localDay(t) <= localDay(Date.now()); t += 86400e3) {
      if (days[days.length - 1] !== localDay(t)) days.push(localDay(t));
    }
    const keys = [...new Set(dayKey.map(s => s.group.split("\x1f")[1]))].sort();
    const cell = new Map(dayKey.map(s => [s.group, s]));
    const valueOf = (day, key) => {
      const s = cell.get(day + "\x1f" + key);
      if (!s) return 0;
      return state.metric === "cost" ? s.cost_usd : state.metric === "quota" ? s.quota_used : s.input + s.output;
    };
    const label = state.metric === "cost" ? fmt.usd : state.metric === "quota" ? (v => fmt.pct(v, 2)) : fmt.compact;
    chart.replaceChildren(h("h2", {}, "Per day"),
      keys.length ? stackedBars(days, keys, valueOf, label) : h("p", { class: "muted" }, "No calls in this range."),
      h("div", { class: "legend" }, keys.map((k, i) => h("span", {}, h("i", { style: `background:${PALETTE[i % PALETTE.length]}` }), k))));
  }

  function renderByKey(sums) {
    byKey.replaceChildren(h("h2", {}, "Per key"), table([
      { title: "Key", cell: s => s.group },
      { title: "Requests", num: true, cell: s => stack(fmt.int(s.requests), s.errors ? h("span", { class: "bad" }, `${fmt.int(s.errors)} failed`) : null) },
      { title: "Tokens", num: true, hint: TOKENS_HINT, cell: s => tokensCell(s) },
      { title: "List price · quota", num: true, hint: COST_HINT, cell: s => stack(
          [fmt.usd(s.cost_usd), s.unpriced ? term("*", `${s.unpriced} call(s) of models without a price`) : ""],
          `${fmt.pct(s.quota_used, 2)} quota`) },
    ], sums));
  }

  let rows = [];
  async function loadRows(more) {
    const before = more && rows.length ? { before: String(rows[rows.length - 1].id) } : {};
    const page = await api("GET", "requests?" + query({ limit: "100", ...before }));
    rows = more ? rows.concat(page) : page;
    list.replaceChildren(h("div", {}, h("h2", {}, "Requests"), table([
      { title: "Time", hint: "When the request came · total time · time to first output", cell: r => stack(fmt.time(r.at),
          r.ttft_ms === null ? fmt.ms(r.duration_ms) : `${fmt.ms(r.duration_ms)} · first output ${fmt.ms(r.ttft_ms)}`) },
      { title: "Key · account · API", cell: r => stack(r.key,
          [r.account || "no account", " · ", r.api, r.stream ? term(" ⇢", STREAM_HINT) : ""]) },
      { title: "Model · mode", hint: "The model, and how the request reached its agy session", cell: r => stack(
          h("span", { title: r.slug }, r.model),
          r.mode ? [term(r.mode, MODES[r.mode]), r.replay_reason ? [" · ", term(r.replay_reason, replayReasons[r.replay_reason])] : ""] : null) },
      { title: "Status", cell: r => r.status === "ok" ? term(r.finish || "ok", STATUSES[r.finish || "ok"])
          : h("span", { class: "bad term", title: `${STATUSES[r.status] || r.status}\n\n${r.error}` }, r.status) },
      { title: "Tokens", num: true, hint: TOKENS_HINT, cell: r => tokensCell(r) },
      { title: "List price · quota", num: true, hint: COST_HINT, cell: r => !r.input && !r.output ? h("span", { class: "muted" }, "—") : stack(fmt.usd(r.cost_usd),
          r.quota_used === null ? term("quota pending", "Attributed once the account's quota is sampled again") : `${fmt.pct(r.quota_used, 3)} quota`) },
    ], rows),
    page.length === 100 ? h("button", { style: "margin-top:8px", onclick: e => guarded(() => loadRows(true), e.target) }, "Load more") : null,
    h("p", { class: "muted" }, "A turn that calls client tools spans several requests; its tokens are counted on the request that ends it. Quota shares appear once the account's quota is sampled again (every 5 minutes while its agy runs).")));
  }

  async function reload() {
    const tz = String(-new Date().getTimezoneOffset());
    const [keySums, dayKeySums] = await Promise.all([api("GET", "usage?" + query({ by: "key", tz })), api("GET", "usage?" + query({ by: "day_key", tz }))]);
    dayKey = dayKeySums;
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
    h("option", { value: "" }, "all components"),
    ["gateway", "pool", "bridge", "usage", "control", "admin"].map(c => h("option", { value: c }, c)));
  const live = h("input", { type: "checkbox", checked: state.live, onchange: e => { state.live = e.target.checked; restart(); } });

  container.append(h("h1", {}, "Logs"),
    h("div", { class: "card row" },
      h("label", {}, "Level", level),
      h("label", {}, "Component", component),
      h("label", {}, "Account", filterInput("account", "any")),
      h("label", { class: "grow" }, "Search", filterInput("q", "text in message or fields")),
      h("label", { style: "flex-direction:row;align-items:center" }, live, "Follow"),
      h("button", { onclick: () => { box.replaceChildren(); lines = 0; count.textContent = ""; } }, "Clear"),
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
    count.textContent = `${lines} lines`;
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
    source.onerror = () => { count.textContent = "stream interrupted; retrying…"; };
  }

  restart();
  return stop;
};
