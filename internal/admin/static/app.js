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

function table(columns, rows, rowClass) {
  return h("div", { class: "table-wrap" }, h("table", {},
    h("thead", {}, h("tr", {}, columns.map(c => h("th", { class: c.num ? "num" : "" }, c.title)))),
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

views.keys = container => {
  const created = h("div");
  const editor = h("div");
  const list = h("div");
  const modelsHint = h("datalist", { id: "model-ids" });
  let groups = [];
  let newLimits = null;

  const name = h("input", { required: true, maxlength: 64, placeholder: "alice" });
  const note = h("input", { placeholder: "optional" });
  const models = h("input", { list: "model-ids", placeholder: "all models" });
  const limitsRow = h("div", { class: "row" });
  const form = h("form", { onsubmit: event => {
    event.preventDefault();
    guarded(async () => {
      const data = await api("POST", "keys", { name: name.value, note: note.value, models: splitList(models.value), limits: newLimits.read() });
      created.replaceChildren(h("div", { class: "card" },
        h("h2", {}, `Key for ${name.value}`),
        h("p", { class: "muted" }, "Copy it now. It is shown only once; the server keeps only its hash."),
        h("div", { class: "secret" }, data.key)));
      name.value = note.value = models.value = "";
      newLimits = null;
      await load();
    }, event.submitter);
  } },
    h("div", { class: "row" },
      h("label", {}, "Name", name),
      h("label", { class: "grow" }, "Note", note),
      h("label", { class: "grow" }, "Allowed models (comma-separated)", models)),
    limitsRow,
    h("p", { class: "muted" }, LIMIT_HINT),
    h("button", { type: "submit" }, "Create key"));

  async function load() {
    const [data, catalog] = await Promise.all([api("GET", "keys"), api("GET", "models")]);
    groups = data.groups;
    if (!newLimits) {
      newLimits = limitInputs(groups, {});
      limitsRow.replaceChildren(...newLimits.nodes);
    }
    modelsHint.replaceChildren(...catalog.map(m => h("option", { value: m.id }, m.name)));
    list.replaceChildren(table([
      { title: "Name", cell: k => k.name },
      { title: "Note", cell: k => k.note || h("span", { class: "muted" }, "—") },
      { title: "Models", cell: k => k.models.length ? k.models.join(", ") : h("span", { class: "muted" }, "all") },
      { title: "Quota used (total) / limit", cell: k => usageCell(k, groups) },
      { title: "Created", cell: k => fmt.time(k.created_at) },
      { title: "", cell: k => k.revoked_at ? `revoked ${fmt.time(k.revoked_at)}` : h("span", { class: "row" },
          h("button", { onclick: () => editKey(k) }, "Edit"),
          h("button", { class: "danger", onclick: e => revoke(k, e.target) }, "Revoke")) },
    ], data.keys, k => k.revoked_at ? "revoked" : ""));
  }

  function editKey(k) {
    const noteInput = h("input", { value: k.note });
    const modelsInput = h("input", { list: "model-ids", value: k.models.join(", "), placeholder: "all models" });
    const limits = limitInputs(groups, k.limits);
    const save = event => {
      event.preventDefault();
      guarded(async () => {
        await api("PATCH", `keys/${k.id}`, { note: noteInput.value, models: splitList(modelsInput.value), limits: limits.read() });
        editor.replaceChildren();
        toast("Saved");
        await load();
      }, event.submitter);
    };
    editor.replaceChildren(h("div", { class: "card" },
      h("h2", {}, `Edit ${k.name}`),
      h("form", { onsubmit: save },
        h("div", { class: "row" },
          h("label", { class: "grow" }, "Note", noteInput),
          h("label", { class: "grow" }, "Allowed models (comma-separated; empty = all)", modelsInput)),
        h("div", { class: "row" }, limits.nodes),
        h("div", { class: "row", style: "margin-top:8px" },
          h("button", { type: "submit", class: "primary" }, "Save"),
          h("button", { type: "button", onclick: () => editor.replaceChildren() }, "Cancel")))));
  }

  function revoke(k, button) {
    if (!confirm(`Revoke the key ${k.name}? Clients using it stop working at once.`)) return;
    guarded(async () => { await api("POST", `keys/${k.id}/revoke`); await load(); }, button);
  }

  container.append(h("h1", {}, "API keys"), h("div", { class: "card" }, form, modelsHint), created, editor, h("div", { class: "card" }, list));
  guarded(load);
};

function splitList(text) {
  return text.split(",").map(s => s.trim()).filter(Boolean);
}

// ---- settings --------------------------------------------------------

views.settings = container => {
  const general = h("div", { class: "card" });
  const password = h("div", { class: "card" });
  const sessions = h("div", { class: "card" });
  const audit = h("div", { class: "card" });
  container.append(h("h1", {}, "Settings"), general, password, sessions, audit);

  async function loadGeneral() {
    const [s, catalog] = await Promise.all([api("GET", "settings"), api("GET", "models")]);
    const turns = h("input", { type: "number", min: 1, max: 64, value: s.turns_per_account });
    const ttl = h("input", { value: s.snapshot_ttl, pattern: "[0-9hms.]+" });
    const level = h("select", {}, ["debug", "info", "warn", "error"].map(l => h("option", { value: l, selected: l === s.log_level }, l)));
    const retention = h("input", { type: "number", min: 1, max: 3650, value: s.retention_days });

    const priceRows = h("tbody");
    const number = value => h("input", { type: "number", min: 0, step: "any", value, style: "width:90px" });
    const addPriceRow = p => {
      const cells = {
        model: h("input", { value: p.model, list: "price-models", required: true }),
        input: number(p.input), cached: number(p.cached), output: number(p.output),
      };
      const row = h("tr", {},
        ["model", "input", "cached", "output"].map(k => h("td", {}, cells[k])),
        h("td", {}, h("button", { type: "button", class: "danger", onclick: () => row.remove() }, "Remove")));
      row.read = () => ({ model: cells.model.value.trim(),
        input: Number(cells.input.value), cached: Number(cells.cached.value), output: Number(cells.output.value) });
      priceRows.append(row);
    };
    s.prices.forEach(addPriceRow);

    const form = h("form", { onsubmit: event => {
      event.preventDefault();
      guarded(async () => {
        await api("PUT", "settings", {
          turns_per_account: Number(turns.value), snapshot_ttl: ttl.value, log_level: level.value,
          retention_days: Number(retention.value), prices: [...priceRows.children].map(row => row.read()),
        });
        toast("Saved");
        await loadGeneral();
      }, event.submitter);
    } },
      h("div", { class: "row" },
        h("label", {}, "Concurrent turns per account", turns),
        h("label", {}, "Idle session reuse (snapshot TTL)", ttl),
        h("label", {}, "Log level", level),
        h("label", {}, "Keep call records (days)", retention)),
      h("p", { class: "muted" }, `Process per session: ${s.process_per_session ? "on" : "off"} (set in the config file; needs a restart).`),
      h("details", { class: "prices" },
        h("summary", {}, `API list prices (${s.prices.length} models, USD per 1M tokens)`),
        h("p", { class: "muted" }, "Used for the list-price estimate of each call. Input is uncached prompt tokens, output includes reasoning. Defaults are the vendors' published prices as of 2026-11-10; gpt-oss-120b has no official price."),
        h("datalist", { id: "price-models" }, catalog.map(m => h("option", { value: m.id }))),
        h("div", { class: "table-wrap" }, h("table", {},
          h("thead", {}, h("tr", {}, ["Model", "Input", "Cached input", "Output", ""].map(t => h("th", {}, t)))),
          priceRows)),
        h("button", { type: "button", style: "margin-top:8px", onclick: () => addPriceRow({ model: "", input: 0, cached: 0, output: 0 }) }, "Add price")),
      h("div", { class: "row", style: "margin-top:12px" },
        h("span", { class: "grow" }),
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
    const value = prompt(`HTTP proxy for ${a.name}, e.g. http://127.0.0.1:7890 (empty = direct).\n`
      + "Changing it restarts the account's agy; turns running there fail.", a.proxy);
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
          h("span", { class: "muted", style: "width:52px" }, b.window),
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
      Object.entries(a.cooldowns).map(([group, until]) => h("p", { class: "warn" }, `${group} quota cooling down for ${fmt.until(until)}`)),
      a.sample_error ? h("p", { class: "bad mono" }, `Quota check failed: ${a.sample_error}`) : null,
      h("p", { class: "muted" }, `${a.turns} turn(s) running · ${a.procs} agy process(es) · quota checked ${a.sampled_at ? fmt.time(a.sampled_at) : "never"}`
        + ` · proxy: ${a.proxy || "direct"}`
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

// ---- calls -----------------------------------------------------------

const PALETTE = ["#2563eb", "#16a34a", "#d97706", "#dc2626", "#7c3aed", "#0891b2", "#db2777", "#65a30d", "#9333ea", "#ea580c"];

function cachePct(input, cached) { return input > 0 ? cached / input : null; }

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
      { title: "Requests", num: true, cell: s => fmt.int(s.requests) },
      { title: "Errors", num: true, cell: s => s.errors ? h("span", { class: "bad" }, fmt.int(s.errors)) : "0" },
      { title: "Input", num: true, cell: s => fmt.compact(s.input) },
      { title: "Cached", num: true, cell: s => fmt.compact(s.cached) },
      { title: "Cache %", num: true, cell: s => fmt.pct(cachePct(s.input, s.cached)) },
      { title: "Output", num: true, cell: s => fmt.compact(s.output) },
      { title: "List price", num: true, cell: s => fmt.usd(s.cost_usd) + (s.unpriced ? "*" : "") },
      { title: "Weekly quota", num: true, cell: s => fmt.pct(s.quota_used, 2) },
    ], sums));
  }

  let rows = [];
  async function loadRows(more) {
    const before = more && rows.length ? { before: String(rows[rows.length - 1].id) } : {};
    const page = await api("GET", "requests?" + query({ limit: "100", ...before }));
    rows = more ? rows.concat(page) : page;
    list.replaceChildren(h("div", {}, h("h2", {}, "Requests"), table([
      { title: "Time", cell: r => fmt.time(r.at) },
      { title: "Key", cell: r => r.key },
      { title: "Account", cell: r => r.account || "—" },
      { title: "Model", cell: r => h("span", { title: r.slug }, r.model) },
      { title: "API", cell: r => r.api + (r.stream ? " ⇢" : "") },
      { title: "Mode", cell: r => r.mode || "—" },
      { title: "Status", cell: r => r.status === "ok" ? (r.finish || "ok") : h("span", { class: "bad", title: r.error }, r.status) },
      { title: "Time", num: true, cell: r => fmt.ms(r.duration_ms) },
      { title: "TTFT", num: true, cell: r => fmt.ms(r.ttft_ms) },
      { title: "In", num: true, cell: r => fmt.compact(r.input) },
      { title: "Cached", num: true, cell: r => fmt.compact(r.cached) },
      { title: "Cache %", num: true, cell: r => fmt.pct(cachePct(r.input, r.cached)) },
      { title: "Out", num: true, cell: r => fmt.compact(r.output) },
      { title: "Reasoning", num: true, cell: r => fmt.compact(r.reasoning) },
      { title: "List price", num: true, cell: r => fmt.usd(r.cost_usd) },
      { title: "Quota", num: true, cell: r => r.quota_used === null ? "—" : fmt.pct(r.quota_used, 3) },
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
