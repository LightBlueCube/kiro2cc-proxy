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

views.keys = container => {
  const created = h("div");
  const list = h("div");
  const modelsHint = h("datalist", { id: "model-ids" });

  const name = h("input", { required: true, maxlength: 64, placeholder: "alice" });
  const note = h("input", { placeholder: "optional" });
  const models = h("input", { list: "model-ids", placeholder: "all models" });
  const form = h("form", { class: "row", onsubmit: event => {
    event.preventDefault();
    guarded(async () => {
      const data = await api("POST", "keys", { name: name.value, note: note.value, models: splitList(models.value) });
      created.replaceChildren(h("div", { class: "card" },
        h("h2", {}, `Key for ${name.value}`),
        h("p", { class: "muted" }, "Copy it now. It is shown only once; the server keeps only its hash."),
        h("div", { class: "secret" }, data.key)));
      name.value = note.value = models.value = "";
      await load();
    }, event.submitter);
  } },
    h("label", {}, "Name", name),
    h("label", { class: "grow" }, "Note", note),
    h("label", { class: "grow" }, "Allowed models (comma-separated)", models),
    h("button", { type: "submit" }, "Create key"));

  async function load() {
    const [keys, catalog] = await Promise.all([api("GET", "keys"), api("GET", "models")]);
    modelsHint.replaceChildren(...catalog.map(m => h("option", { value: m.id }, m.name)));
    list.replaceChildren(table([
      { title: "Name", cell: k => k.name },
      { title: "Note", cell: k => k.note || h("span", { class: "muted" }, "—") },
      { title: "Models", cell: k => k.models.length ? k.models.join(", ") : h("span", { class: "muted" }, "all") },
      { title: "Created", cell: k => fmt.time(k.created_at) },
      { title: "", cell: k => k.revoked_at ? `revoked ${fmt.time(k.revoked_at)}` : h("span", { class: "row" },
          h("button", { onclick: e => editKey(k, e.target) }, "Edit"),
          h("button", { class: "danger", onclick: e => revoke(k, e.target) }, "Revoke")) },
    ], keys, k => k.revoked_at ? "revoked" : ""));
  }

  function editKey(k, button) {
    const newNote = prompt(`Note for ${k.name}`, k.note);
    if (newNote === null) return;
    const newModels = prompt(`Allowed models for ${k.name} (comma-separated; empty = all)`, k.models.join(", "));
    if (newModels === null) return;
    guarded(async () => {
      await api("PATCH", `keys/${k.id}`, { note: newNote, models: splitList(newModels) });
      await load();
    }, button);
  }

  function revoke(k, button) {
    if (!confirm(`Revoke the key ${k.name}? Clients using it stop working at once.`)) return;
    guarded(async () => { await api("POST", `keys/${k.id}/revoke`); await load(); }, button);
  }

  container.append(h("h1", {}, "API keys"), h("div", { class: "card" }, form, modelsHint), created, h("div", { class: "card" }, list));
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
    const s = await api("GET", "settings");
    const turns = h("input", { type: "number", min: 1, max: 64, value: s.turns_per_account });
    const ttl = h("input", { value: s.snapshot_ttl, pattern: "[0-9hms.]+" });
    const level = h("select", {}, ["debug", "info", "warn", "error"].map(l => h("option", { value: l, selected: l === s.log_level }, l)));
    const form = h("form", { onsubmit: event => {
      event.preventDefault();
      guarded(async () => {
        await api("PUT", "settings", { ...s, turns_per_account: Number(turns.value), snapshot_ttl: ttl.value, log_level: level.value });
        toast("Saved");
        await loadGeneral();
      }, event.submitter);
    } },
      h("div", { class: "row" },
        h("label", {}, "Concurrent turns per account", turns),
        h("label", {}, "Idle session reuse (snapshot TTL)", ttl),
        h("label", {}, "Log level", level)),
      h("p", { class: "muted" }, `Process per session: ${s.process_per_session ? "on" : "off"} (set in the config file; needs a restart).`),
      h("button", { type: "submit" }, "Save"));
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
  const env = h("textarea", { rows: 1, placeholder: "HTTPS_PROXY=http://127.0.0.1:3128 (optional, one per line)" });
  const form = h("form", { class: "row", onsubmit: event => {
    event.preventDefault();
    guarded(async () => {
      await api("POST", "accounts", { name: name.value.trim(), env: env.value.split("\n").map(s => s.trim()).filter(Boolean) });
      const added = name.value.trim();
      name.value = env.value = "";
      await startSignin(added);
    }, event.submitter);
  } },
    h("label", {}, "Name", name),
    h("label", { class: "grow" }, "Environment for its agy", env),
    h("button", { type: "submit" }, "Add and sign in"));

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
        + (a.env_names.length ? ` · env: ${a.env_names.join(", ")}` : "")),
      quota(a),
      signinPanel(a),
      h("div", { class: "row", style: "margin-top:12px" },
        signins.has(a.name) ? null : h("button", { onclick: e => startSignin(a.name, e.target) }, a.user || a.disabled ? "Sign in again" : "Sign in"),
        act("Refresh quota", () => api("POST", `accounts/${enc}/refresh`)),
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

// ---- placeholders until their phase lands ------------------------------

for (const name of ["accounts", "calls", "logs"]) {
  views[name] = views[name] || (container => { container.append(h("h1", {}, name), h("p", { class: "muted" }, "Not available yet.")); });
}
