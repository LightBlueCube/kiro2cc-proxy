"use strict";

// ---- i18n ------------------------------------------------------------
//
// The UI text of each language lives in locales/<lang>.json under flat keys
// such as "settings.turns". A value is a string with {name} placeholders,
// or plural forms {"one": "...", "other": "..."} chosen by the count
// parameter. Numbers filling a placeholder are formatted for the locale.
// Data from the server (errors, statuses, log lines, names) is shown as is.

// LANGUAGES lists the catalogs in locales/; the first is the fallback.
const LANGUAGES = ["en"];

let messages = null;
let locale = LANGUAGES[0];
let plurals = null;

const i18n = {
  // load picks the browser's first preferred language that has a catalog
  // and fetches it. The full tag (e.g. en-GB) formats numbers and dates.
  async load() {
    const tag = navigator.languages.find(l => LANGUAGES.includes(l.split("-")[0].toLowerCase())) || LANGUAGES[0];
    const lang = tag.split("-")[0].toLowerCase();
    const resp = await fetch(`locales/${lang}.json`, { credentials: "same-origin" });
    if (!resp.ok) throw new Error(`locales/${lang}.json: ${resp.status} ${resp.statusText}`);
    messages = await resp.json();
    locale = tag;
    plurals = new Intl.PluralRules(locale);
    document.documentElement.lang = tag;
  },

  // apply fills the static page: data-i18n names the key of an element's text.
  apply(root) {
    for (const node of root.querySelectorAll("[data-i18n]")) node.textContent = t(node.dataset.i18n);
  },
};

function message(key, params) {
  const value = messages[key];
  if (value === undefined) throw new Error(`missing UI text: ${key}`);
  if (typeof value === "string") return value;
  return value[plurals.select(params.count)] ?? value.other;
}

function placeholder(key, params, name) {
  if (!(name in params)) throw new Error(`UI text ${key}: no value for {${name}}`);
  const value = params[name];
  return typeof value === "number" ? numberFormat({}).format(value) : value;
}

// t returns the text of key with its placeholders filled from params.
function t(key, params = {}) {
  return message(key, params).replace(/\{(\w+)\}/g, (_, name) => String(placeholder(key, params, name)));
}

// tx is t for text with elements in it: params may be nodes, and the
// result is a list of strings and nodes for h's children.
function tx(key, params = {}) {
  return message(key, params).split(/(\{\w+\})/).filter(Boolean).map(part => {
    const name = /^\{(\w+)\}$/.exec(part);
    return name ? placeholder(key, params, name[1]) : part;
  });
}

// lookup returns the text for a value the server sends, such as a status,
// under group, or undefined when the catalog has none.
function lookup(group, value) {
  return messages[`${group}.${value}`];
}

// ---- formatting ------------------------------------------------------

const numberFormats = new Map();
function numberFormat(options) {
  const id = JSON.stringify(options);
  if (!numberFormats.has(id)) numberFormats.set(id, new Intl.NumberFormat(locale, options));
  return numberFormats.get(id);
}

const fixed = (v, digits) => numberFormat({ minimumFractionDigits: digits, maximumFractionDigits: digits }).format(v);

// duration formats seconds as "3m 20s", "5h 2m", or "2d 4h".
function duration(seconds) {
  if (seconds < 60) return t("duration.s", { n: seconds });
  const m = Math.floor(seconds / 60);
  if (m < 60) return t("duration.m_s", { m, s: seconds % 60 });
  return hoursAndMore(m);
}

function hoursAndMore(minutes) {
  const hrs = Math.floor(minutes / 60);
  return hrs < 48 ? t("duration.h_m", { h: hrs, m: minutes % 60 }) : t("duration.d_h", { d: Math.floor(hrs / 24), h: hrs % 24 });
}

const fmt = {
  int: n => numberFormat({}).format(n ?? 0),
  compact: n => {
    n = n ?? 0;
    if (Math.abs(n) < 1e4) return fmt.int(n);
    const digits = Math.abs(n) >= 1e6 ? 2 : 1;
    return numberFormat({ notation: "compact", minimumFractionDigits: digits, maximumFractionDigits: digits }).format(n);
  },
  usd: v => v === null || v === undefined ? "—"
    : numberFormat({ style: "currency", currency: "USD", ...(v < 0.01 && v > 0 ? { minimumFractionDigits: 4, maximumFractionDigits: 4 } : {}) }).format(v),
  pct: (v, digits = 1) => v === null || v === undefined ? "—"
    : numberFormat({ style: "percent", minimumFractionDigits: digits, maximumFractionDigits: digits }).format(v),
  time: s => s ? new Date(s).toLocaleString(locale) : "—",
  clock: s => s ? new Date(s).toLocaleTimeString(locale) : "—",
  ms: ms => ms === null || ms === undefined ? "—"
    : ms >= 10000 ? t("duration.s", { n: fixed(ms / 1000, 0) })
    : ms >= 1000 ? t("duration.s", { n: fixed(ms / 1000, 1) })
    : t("duration.ms", { n: ms }),
  seconds: s => t("duration.s", { n: fixed(s, 1) }),
  // until is the time left to s, in minutes or more.
  until: s => {
    const ms = new Date(s) - Date.now();
    if (ms <= 0) return t("duration.now");
    const m = Math.round(ms / 60000);
    return m < 60 ? t("duration.m", { n: m }) : hoursAndMore(m);
  },
  // span is the time from a to b (ISO strings or Dates).
  span: (a, b) => duration(Math.max(0, Math.round((new Date(b) - new Date(a)) / 1000))),
  bytes: n => n >= 1 << 30 ? fixed(n / (1 << 30), 2) + " GiB" : n >= 1 << 20 ? fixed(n / (1 << 20), 0) + " MiB" : fixed(n / 1024, 0) + " KiB",
};
