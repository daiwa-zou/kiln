"use strict";
const $ = (id) => document.getElementById(id);
const state = { workspace: null, benches: [], pages: [], slugs: new Set(), view: "overview" };

const TOKEN_KEY = "kiln.token";
const WORKSPACE_KEY = "kiln.workspace";

// Conditional-request cache: the server ETags most reads off the wiki
// revision, which only moves on import, so a 304 saves the whole body.
// Cached payloads are frozen because 304s hand back the same reference.
const etagCache = new Map();

// nav is the navigation token: each route change increments it, and any
// in-flight view render checks it before touching the DOM. Without this a
// slow fetch could overwrite a newer view.
let nav = 0;

// viewCleanups holds teardown for whatever the current view created --
// observers, timers, document-level listeners. beginView drains it before a
// new view renders, so a stale scrollspy or hover timer can never touch the
// next view's DOM. Any view that registers one MUST use onViewCleanup.
let viewCleanups = [];
const onViewCleanup = (fn) => viewCleanups.push(fn);
const runViewCleanups = () => {
  for (const fn of viewCleanups) { try { fn(); } catch { /* teardown is best effort */ } }
  viewCleanups = [];
};

// Motion is opt-out: --dur drives the CSS; this drives JS scroll behavior.
const REDUCED = matchMedia("(prefers-reduced-motion: reduce)");
const scrollBehavior = () => (REDUCED.matches ? "auto" : "smooth");

// localStorage that survives private-mode Safari, where setItem throws.
const lsGet = (key, fallback) => {
  try {
    const v = JSON.parse(localStorage.getItem(key));
    return v ?? fallback;
  } catch { return fallback; }
};
const lsSet = (key, val) => {
  try { localStorage.setItem(key, JSON.stringify(val)); } catch { /* private mode */ }
};

// byTitle is THE page ordering: the tree and the prev/next pager must agree.
const byTitle = (a, b) => (a.title || a.slug).localeCompare(b.title || b.slug);

// relTime renders a date as distance ("3 days ago"); the absolute value rides
// in datetime/title so precision is a hover away.
function relTime(iso) {
  const d = new Date(iso);
  if (isNaN(d)) return String(iso ?? "");
  const days = Math.round((Date.now() - d.getTime()) / 86400000);
  if (days <= 0) return "today";
  if (days === 1) return "yesterday";
  if (days < 30) return `${days} days ago`;
  if (days < 365) { const m = Math.round(days / 30); return `${m} month${m === 1 ? "" : "s"} ago`; }
  const y = Math.round(days / 365);
  return `${y} year${y === 1 ? "" : "s"} ago`;
}
const timeTag = (iso, prefix = "") => iso
  ? `<time datetime="${esc(iso)}" title="${esc(iso)}">${esc(prefix + relTime(iso))}</time>`
  : "";

// toast shows transient feedback bottom-corner. Successes toast; errors stay
// inline next to the control they correct. textContent only -- messages may
// carry server text.
function toast(msg, opts = {}) {
  const box = $("toasts");
  while (box.children.length >= 2) box.firstChild.remove();

  const el = document.createElement("div");
  el.className = "toast";
  const text = document.createElement("span");
  text.textContent = msg;
  el.append(text);

  const dismiss = () => el.remove();
  if (opts.action) {
    const act = document.createElement("button");
    act.className = "toast-act";
    act.textContent = opts.action;
    act.addEventListener("click", () => { dismiss(); opts.onAction?.(); });
    el.append(act);
  }
  const x = document.createElement("button");
  x.className = "toast-x";
  x.setAttribute("aria-label", "Dismiss");
  x.textContent = "×";
  x.addEventListener("click", dismiss);
  el.append(x);

  box.append(el);
  if (!opts.sticky) setTimeout(dismiss, 6000);
  return { dismiss };
}

// openOverlay shows a modal element with the full dialog contract: focus
// capture and restoration, Escape, Tab trapped inside, backdrop click.
function openOverlay(el, onClose) {
  const before = document.activeElement;
  el.hidden = false;

  const focusables = () => [...el.querySelectorAll(
    "input, button, [href], select, textarea, [tabindex]:not([tabindex='-1'])")];
  focusables()[0]?.focus();

  const close = () => {
    el.hidden = true;
    el.removeEventListener("keydown", onKey);
    el.removeEventListener("mousedown", onBackdrop);
    before?.focus?.();
    onClose?.();
  };
  const onKey = (e) => {
    if (e.key === "Escape") { e.stopPropagation(); close(); return; }
    if (e.key !== "Tab") return;
    const f = focusables();
    if (!f.length) return;
    const first = f[0], last = f[f.length - 1];
    if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last.focus(); }
    else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first.focus(); }
  };
  const onBackdrop = (e) => { if (e.target === el) close(); };
  el.addEventListener("keydown", onKey);
  el.addEventListener("mousedown", onBackdrop);
  return close;
}

// fuzzy is the one matcher behind both the palette and the tree filter:
// case-insensitive subsequence with bonuses for word starts and runs.
function fuzzy(q, text) {
  const needle = q.toLowerCase(), hay = text.toLowerCase();
  let score = 0, hi = 0, idx = [], prev = -2;
  for (let i = 0; i < needle.length; i++) {
    hi = hay.indexOf(needle[i], hi);
    if (hi === -1) return null;
    score += 1;
    if (hi === 0 || " -_/".includes(hay[hi - 1])) score += 3; // word start
    if (hi === prev + 1) score += 2;                          // consecutive run
    idx.push(hi);
    prev = hi;
    hi += 1;
  }
  score -= Math.floor(idx[idx.length - 1] / 8); // earlier matches rank higher
  return { score, idx };
}

// fuzzyHi rebuilds the text with matched runs in <mark>. Indices are computed
// on the raw string; escaping happens per run, so they never desynchronize.
function fuzzyHi(text, idx) {
  const hit = new Set(idx);
  let out = "", run = "", inMark = false;
  const flush = () => { out += inMark ? `<mark>${esc(run)}</mark>` : esc(run); run = ""; };
  for (let i = 0; i < text.length; i++) {
    if (hit.has(i) !== inMark) { flush(); inMark = hit.has(i); }
    run += text[i];
  }
  flush();
  return out;
}

// matchPages scores the whole corpus. Results are wrapper objects -- cached
// page objects are frozen and must never be annotated.
function matchPages(q) {
  const out = [];
  for (const p of state.pages) {
    const fields = [["title", p.title || ""], ["slug", p.slug], ["tags", (p.tags || []).join(" ")]];
    let best = null;
    for (const [field, text] of fields) {
      if (!text) continue;
      const m = fuzzy(q, text);
      if (m && (!best || m.score > best.score)) best = { p, field, text, ...m };
    }
    if (best) out.push(best);
  }
  return out.sort((a, b) => b.score - a.score);
}

// csrfToken reads the double-submit cookie a GitHub session sets. Its
// presence is also the "signed in with a session" signal, since the session
// cookie itself is HttpOnly.
const csrfToken = () =>
  document.cookie.split("; ").find((c) => c.startsWith("kiln_csrf="))?.slice(10) || "";

const api = async (path, opts = {}) => {
  const method = opts.method || "GET";
  const headers = {};
  const token = localStorage.getItem(TOKEN_KEY);
  if (token) headers["Authorization"] = `Bearer ${token}`;
  // Cookie-authenticated mutations must echo the CSRF cookie in a header;
  // harmless to send alongside a bearer token, which ignores it.
  if (method !== "GET" && csrfToken()) headers["X-CSRF-Token"] = csrfToken();

  let body;
  if (opts.body !== undefined) {
    headers["Content-Type"] = "application/json";
    body = JSON.stringify(opts.body);
  }
  const cached = method === "GET" ? etagCache.get(path) : null;
  if (cached) headers["If-None-Match"] = cached.etag;

  const res = await fetch(`/api/v1${path}`, { method, headers, body });
  if (res.status === 304 && cached) return cached.data;
  if (res.status === 401) {
    showTokenForm(Boolean(token));
    const err = new Error("401 unauthorized");
    err.handled = true; // the form is already on screen; don't overwrite it
    throw err;
  }
  if (!res.ok) {
    // The server answers errors as {"error": ...}; surface that message
    // rather than a bare status line when it is present.
    let msg = `${res.status} ${res.statusText}`;
    try {
      const j = await res.json();
      if (j.error) msg = j.error;
    } catch { /* not JSON; keep the status line */ }
    throw new Error(msg);
  }

  const data = await res.json();
  const etag = res.headers.get("ETag");
  if (method === "GET" && etag) etagCache.set(path, { etag, data: Object.freeze(data) });
  return data;
};

// The API requires a bearer token (kiln admin token create). The token lives
// in localStorage; a rejected one falls back here rather than looping. The
// guard stops concurrent 401s from re-rendering the form under the user's
// fingers.
let tokenFormShown = false;
function showTokenForm(hadToken) {
  if (tokenFormShown) return;
  tokenFormShown = true;
  $("main").innerHTML = `
    ${hadToken ? `<div class="banner" role="alert">That token was rejected. It may be revoked or expired.</div>` : ""}
    <svg class="logo logo-signin" aria-hidden="true"><use href="#logo-mark"/></svg>
    <h1>Sign in</h1>
    ${state.githubSignIn ? `
      <p class="hint">Sign in with your GitHub account:</p>
      <a class="btn" href="/auth/github/login">Sign in with GitHub</a>
      <p class="hint" style="margin-top:20px">Or use an access token.</p>` : `
      <p class="hint">This kiln requires an access token. Mint one with
        <code>kiln admin token create --login you</code> and paste it here.
        It is stored only in this browser.</p>`}
    <input id="token-input" type="password" placeholder="kiln_…" aria-label="Access token"
           autocomplete="off" spellcheck="false">
    <button id="token-save" class="btn">Save token</button>`;
  const save = () => {
    const v = $("token-input").value.trim();
    if (!v) return;
    localStorage.setItem(TOKEN_KEY, v);
    location.reload();
  };
  $("token-save").addEventListener("click", save);
  $("token-input").addEventListener("keydown", (e) => { if (e.key === "Enter") save(); });
  $("token-input").focus();
}

const esc = (s) => String(s ?? "").replace(/[&<>"']/g, (c) =>
  ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));

// unesc inverts esc for text captured AFTER whole-source escaping -- wikilink
// targets must be compared against real slugs, not entity-encoded ones.
// &amp; must be last or double-encoded input would double-decode.
const unesc = (s) => s.replace(/&lt;/g, "<").replace(/&gt;/g, ">")
  .replace(/&quot;/g, '"').replace(/&#39;/g, "'").replace(/&amp;/g, "&");

// safeURL admits only destinations that cannot execute script: http(s) and
// internal hash routes. Anything else renders as text, which fails safe.
const safeURL = (u) => /^(https?:\/\/|#\/)/.test(u) ? u : null;

// A very small markdown subset: enough to read a generated page without
// shipping a parser. Everything is escaped first, so page content is data.
// headingOffset shifts markdown headings: 0 for artifacts (whose # is the
// page title), 1 for pages (whose <h1> is rendered by the chrome).
function renderMarkdown(src, headingOffset = 1) {
  // Artifacts arrive with their frontmatter fence intact; metadata is chrome
  // here, not prose, and the --- fences would otherwise render as rules.
  src = src.replace(/^﻿?\s*---\n[\s\S]*?\n---\n/, "");
  const lines = esc(src).split("\n");
  let html = "", inCode = false, listType = null, para = [];

  const closeList = () => { if (listType) { html += `</${listType}>`; listType = null; } };
  const openList = (t) => { if (listType !== t) { closeList(); html += `<${t}>`; listType = t; } };
  // Consecutive prose lines join into one paragraph; markdown hard-wrapped at
  // 80 columns must not become one <p> per line.
  const flushPara = () => {
    if (para.length) { html += `<p>${inline(para.join(" "))}</p>`; para = []; }
  };
  const isTableRow = (s) => /^\s*\|.*\|\s*$/.test(s);
  const isTableRule = (s) => /^\s*\|[\s:|-]+\|\s*$/.test(s);
  const splitRow = (s) => s.trim().replace(/^\||\|$/g, "").split("|").map((c) => c.trim());

  for (let i = 0; i < lines.length; i++) {
    const raw = lines[i];
    if (raw.trimStart().startsWith("```")) {
      flushPara(); closeList();
      html += inCode ? "</code></pre>" : "<pre><code>";
      inCode = !inCode;
      continue;
    }
    if (inCode) { html += raw + "\n"; continue; }

    // A pipe row followed by a |---| rule opens a table; rows are consumed
    // until the first non-row line.
    if (isTableRow(raw) && isTableRule(lines[i + 1] ?? "")) {
      flushPara(); closeList();
      html += "<table><thead><tr>" +
        splitRow(raw).map((c) => `<th>${inline(c)}</th>`).join("") +
        "</tr></thead><tbody>";
      i += 1;
      while (isTableRow(lines[i + 1] ?? "")) {
        i += 1;
        html += "<tr>" + splitRow(lines[i]).map((c) => `<td>${inline(c)}</td>`).join("") + "</tr>";
      }
      html += "</tbody></table>";
      continue;
    }

    const heading = raw.match(/^(#{1,6})\s+(.*)$/);
    if (heading) {
      flushPara(); closeList();
      const level = Math.min(heading[1].length + headingOffset, 6);
      html += `<h${level}>${inline(heading[2])}</h${level}>`;
      continue;
    }
    if (/^\s*(---+|\*\*\*+)\s*$/.test(raw)) { flushPara(); closeList(); html += "<hr>"; continue; }
    if (/^\s*[-*]\s+/.test(raw)) {
      flushPara(); openList("ul");
      html += `<li>${inline(raw.replace(/^\s*[-*]\s+/, ""))}</li>`;
      continue;
    }
    if (/^\s*\d+\.\s+/.test(raw)) {
      flushPara(); openList("ol");
      html += `<li>${inline(raw.replace(/^\s*\d+\.\s+/, ""))}</li>`;
      continue;
    }
    if (raw.trimStart().startsWith("&gt;")) {
      flushPara(); closeList();
      html += `<blockquote>${inline(raw.replace(/^\s*&gt;\s?/, ""))}</blockquote>`;
      continue;
    }
    if (raw.trim() === "") { flushPara(); closeList(); continue; }
    closeList();
    para.push(raw.trim());
  }
  flushPara();
  closeList();
  if (inCode) html += "</code></pre>";
  return html;
}

// inline runs the span-level transforms on non-code text only: code spans are
// carved out first so `**ptr` stays literal and paths in backticks are never
// linkified -- paths and shas are load-bearing here.
function inline(text) {
  return text.split(/(`[^`]+`)/).map((seg) => {
    if (seg.startsWith("`") && seg.endsWith("`") && seg.length > 1) {
      return `<code>${seg.slice(1, -1)}</code>`;
    }
    return inlineText(seg);
  }).join("");
}

function inlineText(text) {
  return text
    // Images before links, since the syntaxes nest.
    .replace(/!\[([^\]]*)\]\(([^)\s]+)\)/g, (m, alt, url) => {
      const u = safeURL(url);
      return u ? `<img src="${u}" alt="${alt}" loading="lazy" referrerpolicy="no-referrer">` : m;
    })
    .replace(/\[([^\]]+)\]\(([^)\s]+)\)/g, (m, label, url) => {
      const u = safeURL(url);
      if (!u) return m;
      const ext = u.startsWith("#/") ? "" : ` target="_blank" rel="noopener"`;
      return `<a href="${u}"${ext}>${label}</a>`;
    })
    .replace(/\*\*([^*]+)\*\*/g, "<strong>$1</strong>")
    .replace(/(^|[^*\w])\*([^*\n]+)\*(?!\w)/g, "$1<em>$2</em>")
    .replace(/~~([^~]+)~~/g, "<del>$1</del>")
    .replace(/\[\[([^\]|]+)(?:\|([^\]]+))?\]\]/g, (_, target, alias) => {
      // The captured target is entity-encoded (the whole source was escaped
      // first); decode before comparing with real slugs or any page whose
      // name contains & or an apostrophe renders as a false dead link.
      const slug = unesc(target.split("/").pop().replace(/\.md$/, ""));
      const label = alias || esc(slug);
      // A link to a page that does not exist is drawn as a gap rather than
      // hidden: it is the wiki saying what it wants and does not have.
      return state.slugs.has(slug)
        ? `<a class="wikilink" href="#/page/${encodeURIComponent(slug)}">${label}</a>`
        : `<span class="wikilink dead" title="No page for ${esc(slug)} yet">${label}</span>`;
    });
}

const TYPE_ORDER = ["synthesis", "entity", "concept", "source", "comparison", "query"];
const TYPE_LABELS = {
  synthesis: "Synthesis", entity: "Entities", concept: "Concepts",
  source: "Sources", comparison: "Comparisons", query: "Queries",
};

// treeFilter is the sidebar quick-filter; lastActiveSlug lets filter
// re-renders keep aria-current without each caller re-passing it.
let treeFilter = "";
let lastActiveSlug = null;

const collapsedKey = () => `kiln.tree.collapsed.${state.workspace}`;
const recentKey = () => `kiln.recent.${state.workspace}`;

// rememberRecent MRUs a visited page for the sidebar's Recent group.
function rememberRecent(slug) {
  const rec = lsGet(recentKey(), []).filter((s) => s !== slug);
  rec.unshift(slug);
  lsSet(recentKey(), rec.slice(0, 5));
}

function treeLink(p, active, idx) {
  const current = p.slug === active ? ' aria-current="page"' : "";
  const label = idx ? fuzzyHi(p.title || p.slug, idx) : esc(p.title || p.slug);
  return `<a href="#/page/${encodeURIComponent(p.slug)}"${current}>${label}</a>`;
}

function treeGroup(type, label, links, collapsed) {
  return `<details class="tree-group" data-type="${esc(type)}"${collapsed ? "" : " open"}>
    <summary class="group-label">${esc(label)} <span class="count">${links.length}</span></summary>
    ${links.join("")}
  </details>`;
}

function renderTree(active) {
  lastActiveSlug = active;
  const byType = {};
  for (const p of state.pages) (byType[p.type] ||= []).push(p);

  // Filter mode: one flat matcher pass, groups forced open, matches
  // highlighted -- the same fuzzy the palette uses.
  if (treeFilter) {
    let html = "", matches = 0;
    for (const type of TYPE_ORDER) {
      const group = byType[type];
      if (!group) continue;
      const hits = [...group].sort(byTitle)
        .map((p) => ({ p, m: fuzzy(treeFilter, p.title || p.slug) }))
        .filter((x) => x.m);
      if (!hits.length) continue;
      matches += hits.length;
      html += treeGroup(type, TYPE_LABELS[type],
        hits.map((x) => treeLink(x.p, active, x.m.idx)), false);
    }
    $("filter-status").textContent = `${matches} page${matches === 1 ? "" : "s"} match`;
    $("tree").innerHTML = html || `<div class="hint">No pages match.</div>`;
    return;
  }
  $("filter-status").textContent = "";

  const collapsed = new Set(lsGet(collapsedKey(), []));
  let html = "";

  // Recent first: the pages someone actually returns to. Deleted pages drop
  // out by checking against the live slug set.
  const titleBySlug = new Map(state.pages.map((p) => [p.slug, p]));
  const recent = lsGet(recentKey(), []).filter((s) => state.slugs.has(s));
  if (recent.length) {
    html += treeGroup("__recent", "Recent",
      recent.map((s) => treeLink(titleBySlug.get(s), active)), collapsed.has("__recent"));
  }

  for (const type of TYPE_ORDER) {
    const group = byType[type];
    if (!group) continue;
    const sorted = [...group].sort(byTitle);
    html += treeGroup(type, TYPE_LABELS[type],
      sorted.map((p) => treeLink(p, active)), collapsed.has(type));
  }
  $("tree").innerHTML = html || `<div class="hint">No pages yet.</div>`;

  // Persist collapse state -- but never while a filter forces groups open.
  for (const det of $("tree").querySelectorAll("details.tree-group")) {
    det.addEventListener("toggle", () => {
      if (treeFilter) return;
      const set = new Set(lsGet(collapsedKey(), []));
      if (det.open) set.delete(det.dataset.type);
      else set.add(det.dataset.type);
      lsSet(collapsedKey(), [...set]);
    });
  }
}

function setNav(view) {
  for (const a of document.querySelectorAll(".nav a")) {
    if (a.dataset.view === view) a.setAttribute("aria-current", "page");
    else a.removeAttribute("aria-current");
  }
}

const setTitle = (s) => { document.title = s ? `${s} · kiln` : "kiln"; };

// No inline handlers anywhere: the CSP allows only the hashed script block,
// so a retry button is wired up after insertion.
const banner = (err, retry) => `<div class="banner" role="alert">${esc(err.message)}
  ${retry ? `<button id="banner-retry">Reload</button>` : ""}</div>`;
const wireBannerRetry = () =>
  $("banner-retry")?.addEventListener("click", () => location.reload());

// beginView starts a navigation: bumps the token, resets scroll and focus
// (the SPA equivalent of a page load), and shows a delayed loading state so
// slow fetches don't read as dead clicks while fast ones don't flash.
function beginView(title, view, activeSlug) {
  // Tear down the departing view's observers and timers BEFORE the token
  // moves: nothing stale may ever touch the incoming view's DOM.
  runViewCleanups();
  const my = ++nav;
  setTitle(title);
  setNav(view);
  renderTree(activeSlug ?? null);
  document.body.classList.remove("nav-open");
  $("menu").setAttribute("aria-expanded", "false");
  const m = $("main");
  m.setAttribute("aria-busy", "true");
  m.focus({ preventScroll: true });
  window.scrollTo(0, 0);
  // Skeleton only when loading is actually slow: the 150ms delay keeps the
  // common cached/304 path flash-free.
  setTimeout(() => {
    if (my === nav && m.getAttribute("aria-busy") === "true") {
      m.innerHTML = `<div class="skel" aria-hidden="true">
        <div class="skel-title"></div>
        <div class="skel-line"></div><div class="skel-line"></div>
        <div class="skel-line short"></div>
      </div><span class="sr-only">Loading…</span>`;
    }
  }, 150);
  return {
    current: () => my === nav,
    done: (html) => {
      if (my !== nav) return false;
      m.removeAttribute("aria-busy");
      m.innerHTML = html;
      // Entrance animation on already-committed content only: an exit
      // animation would delay the swap behind a timer, which is exactly the
      // race the nav token exists to prevent.
      m.classList.remove("view-in");
      void m.offsetWidth;
      m.classList.add("view-in");
      return true;
    },
  };
}

const freshnessChip = (ref) => ref
  ? `<span class="chip fresh">as of <span class="mono">${esc(ref)}</span></span>`
  : `<span class="chip stale">source revision unknown</span>`;

// pagerFor renders prev/next within the page's type group, in exactly the
// order the sidebar shows -- byTitle over a copy, never sorting state.pages
// in place (its arrays may alias frozen cache entries).
function pagerFor(p) {
  const group = state.pages.filter((x) => x.type === p.type).slice().sort(byTitle);
  const i = group.findIndex((x) => x.slug === p.slug);
  if (i === -1 || group.length < 2) return "";
  const prev = group[i - 1], next = group[i + 1];
  const link = (pg, rel, label) => pg
    ? `<a href="#/page/${encodeURIComponent(pg.slug)}" rel="${rel}">${label}</a>`
    : `<span></span>`;
  return `<nav class="pager" aria-label="Adjacent pages">
    ${link(prev, "prev", `← ${esc(prev?.title || prev?.slug || "")}`)}
    ${link(next, "next", `${esc(next?.title || next?.slug || "")} →`)}
  </nav>`;
}

// buildTOC post-processes the rendered page: ids are assigned to real DOM
// nodes (property assignment injects nothing), the renderer stays untouched.
// TOC entries are buttons, not #fragment links -- a bare fragment would
// rewrite location.hash and remount the whole view through the router.
function buildTOC() {
  const headings = [...document.querySelectorAll("#main .prose h2, #main .prose h3")];
  if (headings.length < 3) return;

  const seen = new Map();
  for (const h of headings) {
    let id = "h-" + (h.textContent || "").toLowerCase().trim()
      .replace(/[^a-z0-9]+/g, "-").replace(/^-+|-+$/g, "");
    const n = (seen.get(id) || 0) + 1;
    seen.set(id, n);
    if (n > 1) id += `-${n}`;
    h.id = id;
    h.tabIndex = -1; // focus target for TOC jumps
  }

  const toc = document.createElement("details");
  toc.className = "toc";
  toc.open = !matchMedia("(max-width: 760px)").matches;
  const summary = document.createElement("summary");
  summary.textContent = "Contents";
  const list = document.createElement("ul");
  for (const h of headings) {
    const li = document.createElement("li");
    li.className = h.tagName.toLowerCase();
    const btn = document.createElement("button");
    btn.className = "toc-link";
    btn.textContent = h.textContent;
    btn.addEventListener("click", () => {
      h.scrollIntoView({ behavior: scrollBehavior(), block: "start" });
      h.focus({ preventScroll: true });
    });
    li.append(btn);
    list.append(li);
  }
  toc.append(summary, list);
  toc.setAttribute("aria-label", "Contents");
  document.querySelector("#main .page-head")?.after(toc);

  // Scrollspy: highlight the section currently in the top third of the
  // viewport. Disconnected via onViewCleanup so a stale observer can never
  // touch the next view.
  const buttons = [...list.querySelectorAll(".toc-link")];
  const mark = (i) => buttons.forEach((b, j) => {
    b.classList.toggle("active", i === j);
    if (i === j) b.setAttribute("aria-current", "true");
    else b.removeAttribute("aria-current");
  });
  const observer = new IntersectionObserver((entries) => {
    for (const e of entries) {
      if (e.isIntersecting) { mark(headings.indexOf(e.target)); break; }
    }
  }, { rootMargin: "0px 0px -70% 0px" });
  for (const h of headings) observer.observe(h);
  onViewCleanup(() => observer.disconnect());
}

// ---- wikilink hover previews ------------------------------------------------
// Pointer-only: on touch the first tap must navigate, and long-press fights
// text selection, so the whole feature is skipped there.
const HOVERABLE = matchMedia("(hover: hover) and (pointer: fine)");
let previewTimer = null, previewSlug = null;

// hidePreview returns whether a card was actually dismissed, so the global
// Escape handler can consume the key press.
function hidePreview() {
  clearTimeout(previewTimer);
  previewTimer = null;
  previewSlug = null;
  const card = $("preview");
  const had = !card.hidden;
  card.hidden = true;
  document.querySelector("[aria-describedby='preview']")?.removeAttribute("aria-describedby");
  return had;
}

// plainText flattens markdown to prose for the excerpt: fences and inline
// tokens out, labels kept.
function plainText(md) {
  return md
    .replace(/^﻿?\s*---\n[\s\S]*?\n---\n/, "")
    .replace(/```[\s\S]*?```/g, " ")
    .replace(/\[\[([^\]|]+)(?:\|([^\]]+))?\]\]/g, (_, t, a) => a || t.split("/").pop().replace(/\.md$/, ""))
    .replace(/!?\[([^\]]*)\]\([^)]*\)/g, "$1")
    .replace(/[#>*`~_|-]+/g, " ")
    .replace(/\s+/g, " ")
    .trim();
}

function showPreviewFor(link) {
  const href = link.getAttribute("href") || "";
  const slug = decodeURIComponent(href.replace("#/page/", ""));
  clearTimeout(previewTimer);
  previewSlug = slug;

  // Three layers against fetch storms: this cancelable dwell timer, the
  // single-slug in-flight guard below, and the etagCache underneath.
  previewTimer = setTimeout(async () => {
    let p;
    try {
      p = await api(`/workspaces/${encodeURIComponent(state.workspace)}/pages/${encodeURIComponent(slug)}`);
    } catch { return; } // preview is decoration; failures stay silent
    if (previewSlug !== slug || !link.isConnected) return;

    const card = $("preview");
    card.textContent = "";
    const title = document.createElement("div");
    title.className = "pv-title";
    title.textContent = p.title || p.slug;
    const type = document.createElement("div");
    type.className = "pv-type";
    type.textContent = p.type;
    const body = document.createElement("div");
    body.className = "pv-body";
    const words = plainText(p.body).split(" ").slice(0, 40).join(" ");
    body.textContent = words + (words ? "…" : "");
    card.append(title, type, body);

    const r = link.getBoundingClientRect();
    card.hidden = false;
    const below = r.bottom + 12 + card.offsetHeight < innerHeight;
    card.style.top = `${below ? r.bottom + 8 : Math.max(8, r.top - card.offsetHeight - 8)}px`;
    card.style.left = `${Math.min(Math.max(12, r.left), innerWidth - card.offsetWidth - 12)}px`;
    link.setAttribute("aria-describedby", "preview");
  }, 350);
}

// ---- live content -----------------------------------------------------------
let pollTimer = null, knownRevision = null, revToast = null;

function updateReviewsBadge(count) {
  const b = $("reviews-badge");
  b.hidden = count <= 0;
  b.textContent = count > 0 ? String(count) : "";
  b.setAttribute("aria-label", `${count} open review${count === 1 ? "" : "s"}`);
}

async function refreshReviewsBadge() {
  try {
    // Rides the revision-keyed etagCache: idle ticks are 304s.
    const open = await api(`/workspaces/${encodeURIComponent(state.workspace)}/reviews?status=open`);
    updateReviewsBadge(open.length);
  } catch { /* decoration; the next tick retries */ }
}

async function pollTick() {
  const ws = state.workspace;
  let w;
  try {
    // Deliberately un-cached: the workspace summary route sets no ETag, so
    // every tick is a real ~120-byte 200. The comparison is on the revision
    // FIELD, never on status codes, so a future server-side ETag stays safe.
    w = await api(`/workspaces/${encodeURIComponent(ws)}`);
  } catch (err) {
    if (err.handled) clearInterval(pollTimer); // 401: token form is up, stop
    return;
  }
  if (ws !== state.workspace) return; // raced a workspace switch

  refreshReviewsBadge();
  // Age the visible relative timestamps while we are here.
  for (const t of document.querySelectorAll("time[datetime]")) {
    t.textContent = relTime(t.getAttribute("datetime"));
  }

  if (knownRevision === null) { knownRevision = w.revision; return; }
  if (w.revision !== knownRevision && !revToast) {
    revToast = toast("Wiki updated — new content is available", {
      sticky: true, action: "Refresh",
      onAction: () => {
        revToast = null;
        knownRevision = null;
        // loadWorkspace re-routes the current view; the revision-keyed server
        // ETags make the cache self-invalidating, and nothing is reloaded, so
        // unsaved steering text survives until the user opts in.
        loadWorkspace(state.workspace);
      },
    });
  }
}

async function showPage(slug) {
  const view = beginView(slug, null, slug);
  try {
    const p = await api(`/workspaces/${encodeURIComponent(state.workspace)}/pages/${encodeURIComponent(slug)}`);

    // Backlinks and corrections degrade to absent panels on failure; null
    // means "unavailable", [] means "genuinely none" -- different renders.
    const [backlinks, corrections] = await Promise.all([
      api(`/workspaces/${encodeURIComponent(state.workspace)}/backlinks/${encodeURIComponent(p.slug)}`).catch(() => null),
      api(`/workspaces/${encodeURIComponent(state.workspace)}/corrections/${encodeURIComponent(p.slug)}`).catch(() => null),
    ]);
    if (!view.current()) return;

    setTitle(p.title || p.slug);
    rememberRecent(p.slug);
    view.done(`
      <div class="page-head">
        <h1>${esc(p.title || p.slug)}</h1>
        <div class="meta">
          <span class="chip">${esc(p.type)}</span>
          ${freshnessChip(p.builtAtRef)}
          ${p.updated ? `<span>updated ${timeTag(p.updated)}</span>` : ""}
          ${(p.tags || []).map((t) => `<span class="chip">${esc(t)}</span>`).join("")}
        </div>
      </div>
      <div class="prose">${renderMarkdown(p.body)}</div>
      ${pagerFor(p)}
      ${backlinks === null ? "" : `<div class="page-head" style="margin-top:32px;border-bottom:none">
        <div class="group-label">Linked from</div>
        ${backlinks.length ? `<div class="meta">${backlinks.map((b) =>
          `<a class="chip" href="#/page/${encodeURIComponent(b.slug)}">${esc(b.title || b.slug)}</a>`).join("")}</div>`
        : `<div class="hint">No pages link here yet.</div>`}
      </div>`}
      ${(p.sources || []).length ? `<div class="page-head" style="margin-top:16px;border-bottom:none">
        <div class="group-label">Derived from</div>
        <div class="meta">${p.sources.map((s) => `<span class="chip mono">${esc(s)}</span>`).join("")}</div>
      </div>` : ""}
      ${correctionsPanel(corrections)}`);

    buildTOC();
    wireCorrections(p.slug);
  } catch (err) {
    if (err.handled || !view.current()) return;
    view.done(`<div class="banner" role="alert">Could not load that page: ${esc(err.message)}</div>`);
  }
}

// correctionsPanel renders pinned corrections and the form to add one. Pages
// are never hand-edited -- regeneration would clobber the edit -- so this is
// the sanctioned way to teach the wiki something it got wrong.
function correctionsPanel(corrections) {
  if (corrections === null) return ""; // endpoint unavailable
  const items = corrections.map((c) => `
    <div class="correction ${c.active ? "" : "inactive"}">
      <div class="body">${esc(c.body)}</div>
      <div class="tools">${timeTag(c.created)} ·
        <button class="linkish" data-correction="${esc(c.id)}" data-active="${!c.active}">
          ${c.active ? "deactivate" : "reactivate"}</button></div>
    </div>`).join("");
  return `
    <details class="panel" ${corrections.some((c) => c.active) ? "open" : ""}>
      <summary>Corrections (${corrections.filter((c) => c.active).length} active)</summary>
      <p class="hint">A correction is pinned beside the page and re-injected into
        every future rebuild. Use it when the wiki states something wrong; the
        next regeneration will honor it.</p>
      ${items}
      <label class="hint" for="correction-body">New correction</label>
      <textarea id="correction-body" rows="3" placeholder="What should the wiki know about this page?"></textarea>
      <button class="btn" id="correction-pin">Pin correction</button>
      <span class="hint" id="correction-note" role="status"></span>
    </details>`;
}

// once guards a button against double submission: disabled while in flight.
const once = (btn, fn) => btn.addEventListener("click", async () => {
  if (btn.disabled) return;
  btn.disabled = true;
  try { await fn(); } finally { btn.disabled = false; }
});

function wireCorrections(slug) {
  const note = (msg, isErr) => {
    const el = $("correction-note");
    if (el) { el.textContent = msg; el.classList.toggle("error", Boolean(isErr)); }
  };
  const pin = $("correction-pin");
  if (pin) once(pin, async () => {
    const body = $("correction-body").value.trim();
    if (!body) return;
    try {
      await api(`/workspaces/${encodeURIComponent(state.workspace)}/corrections/${encodeURIComponent(slug)}`,
        { method: "POST", body: { body } });
      toast("Correction pinned — applies from the next rebuild");
      showPage(slug);
    } catch (err) {
      if (!err.handled) note(err.message, true);
    }
  });
  for (const b of document.querySelectorAll("[data-correction]")) {
    once(b, async () => {
      try {
        await api(`/workspaces/${encodeURIComponent(state.workspace)}/correction/${encodeURIComponent(b.dataset.correction)}`,
          { method: "PATCH", body: { active: b.dataset.active === "true" } });
        showPage(slug);
      } catch (err) {
        if (!err.handled) note(err.message, true);
      }
    });
  }
}

const artifactTitles = { overview: "Overview", index: "Index", log: "Log" };

async function showArtifact(kind) {
  const view = beginView(artifactTitles[kind], kind);
  try {
    const { body } = await api(`/workspaces/${encodeURIComponent(state.workspace)}/${kind}`);
    if (!body.trim()) {
      view.done(`<div class="empty">
        No ${kind} yet. Run <code>kiln build &lt;path&gt;</code> to generate one.</div>`);
      return;
    }
    // Artifacts carry their own # heading, so no offset: it becomes the h1.
    view.done(`<div class="prose">${renderMarkdown(body, 0)}</div>`);
  } catch (err) {
    if (!err.handled) view.done(banner(err));
  }
}

async function showGaps() {
  const view = beginView("Gaps", "gaps");
  try {
    const gaps = await api(`/workspaces/${encodeURIComponent(state.workspace)}/gaps`);
    if (!gaps.length) {
      view.done(`<h1>Gaps</h1><div class="empty">
        Every wikilink resolves. Nothing is missing.</div>`);
      return;
    }
    view.done(`<h1>Gaps</h1>
      <p class="hint">Pages the wiki links to but does not have — the most precise
      signal of what is missing, since the wiki asked for these itself.</p>
      ${gaps.map((g) => `<div class="row">
        <span class="mono">${esc(g.slug)}</span>
        <span class="count">wanted by ${esc(g.wantedBy)} page${g.wantedBy === 1 ? "" : "s"}</span>
      </div>`).join("")}`);
  } catch (err) {
    if (!err.handled) view.done(banner(err));
  }
}

// showReviews renders the wiki's questions for its humans: contradictions and
// uncertainties the agent flagged, and deletions awaiting approval.
async function showReviews(all) {
  const view = beginView("Reviews", "reviews");
  try {
    const status = all ? "" : "open";
    const reviews = await api(`/workspaces/${encodeURIComponent(state.workspace)}/reviews?status=${status}`);
    const toggle = all
      ? `<a href="#/reviews">show open only</a>`
      : `<a href="#/reviews/all">show resolved too</a>`;

    if (!reviews.length) {
      view.done(`<h1>Reviews</h1>
        <p class="hint">${toggle}</p>
        <div class="empty">No ${all ? "" : "open "}reviews. When a build is uncertain
        about something — or a source disappears — the question lands here.</div>`);
      return;
    }
    if (!view.done(`<h1>Reviews</h1>
      <p class="hint">Questions the wiki cannot answer alone. Resolving one records
        your judgment; approving a deletion authorizes the next build to act on it.
        ${toggle}</p>
      <div id="review-note" class="hint" role="status"></div>
      ${reviews.map((r) => `
        <div class="review">
          <div class="meta">
            <span class="chip">${esc(r.kind)}</span>
            ${r.pageSlug ? `<a class="chip" href="#/page/${encodeURIComponent(r.pageSlug)}">${esc(r.pageSlug)}</a>` : ""}
            ${timeTag(r.created)}
            ${r.status !== "open" ? `<span class="chip">${esc(r.status)}${r.resolved ? " " + esc(r.resolved) : ""}</span>` : ""}
          </div>
          <strong>${esc(r.title)}</strong>
          <div class="detail">${esc(r.detail)}</div>
          ${r.status === "open" ? (r.actions && r.actions.length ? r.actions : ["dismiss"]).map((a) =>
            `<button class="btn ${a === "approve" ? "" : "quiet"}" data-review="${esc(r.id)}" data-action="${esc(a)}">${esc(a)}</button>`
          ).join("") : ""}
        </div>`).join("")}`)) return;

    for (const b of document.querySelectorAll("[data-review]")) {
      once(b, async () => {
        // Approval authorizes the next build's deletion cascade -- the most
        // consequential click in the UI gets a two-step confirm, inline.
        if (b.dataset.action === "approve" && b.dataset.armed !== "true") {
          b.dataset.armed = "true";
          b.textContent = "confirm approve";
          b.classList.add("danger");
          setTimeout(() => {
            if (b.isConnected) {
              b.dataset.armed = "false";
              b.textContent = "approve";
              b.classList.remove("danger");
            }
          }, 4000);
          return;
        }
        try {
          await api(`/workspaces/${encodeURIComponent(state.workspace)}/reviews/${encodeURIComponent(b.dataset.review)}/resolve`,
            { method: "POST", body: { action: b.dataset.action } });
          // Decrement the badge locally: resolution may not bump the wiki
          // revision, so an ETag'd refetch could 304 to the stale list.
          updateReviewsBadge(Number($("reviews-badge").textContent || 1) - 1);
          toast(`Review ${b.dataset.action === "approve" ? "approved" : "resolved"}`);
          showReviews(all);
        } catch (err) {
          if (err.handled) return;
          const note = $("review-note");
          if (note) { note.textContent = err.message; note.classList.add("error"); }
        }
      });
    }
  } catch (err) {
    if (!err.handled) view.done(banner(err));
  }
}

// showGraph renders the page graph from the materialized links: every live
// page, every resolved wikilink. A tiny force simulation, no libraries —
// repulsion, springs along edges, gravity to the center — run to rest before
// first paint so the layout is stable, not a spinning mobile.
async function showGraph() {
  const view = beginView("Graph", "graph");
  try {
    const { nodes, edges, totalPages } = await api(`/workspaces/${encodeURIComponent(state.workspace)}/graph`);
    if (!nodes.length) {
      view.done(`<h1>Graph</h1><div class="empty">No pages yet.</div>`);
      return;
    }

    const W = 900, H = 640;
    const bySlug = new Map(nodes.map((n, i) => [n.slug, i]));
    const pts = nodes.map((_, i) => ({
      // Deterministic golden-angle disc seeding: same graph, same picture.
      x: W / 2 + Math.sqrt(i + 1) * 22 * Math.cos(i * 2.39996),
      y: H / 2 + Math.sqrt(i + 1) * 22 * Math.sin(i * 2.39996),
      vx: 0, vy: 0,
    }));
    const links = edges
      .map((e) => [bySlug.get(e.from), bySlug.get(e.to)])
      .filter(([a, b]) => a !== undefined && b !== undefined && a !== b);

    const tick = (k) => {
      for (let i = 0; i < pts.length; i++) {
        for (let j = i + 1; j < pts.length; j++) {
          let dx = pts[i].x - pts[j].x, dy = pts[i].y - pts[j].y;
          const d2 = Math.max(64, dx * dx + dy * dy);
          const f = (2600 / d2) * k;
          const d = Math.sqrt(d2);
          dx /= d; dy /= d;
          pts[i].vx += dx * f; pts[i].vy += dy * f;
          pts[j].vx -= dx * f; pts[j].vy -= dy * f;
        }
      }
      for (const [a, b] of links) {
        const dx = pts[b].x - pts[a].x, dy = pts[b].y - pts[a].y;
        const d = Math.max(1, Math.hypot(dx, dy));
        const f = (d - 90) * 0.015 * k;
        pts[a].vx += (dx / d) * f; pts[a].vy += (dy / d) * f;
        pts[b].vx -= (dx / d) * f; pts[b].vy -= (dy / d) * f;
      }
      for (const p of pts) {
        p.vx += (W / 2 - p.x) * 0.004 * k;
        p.vy += (H / 2 - p.y) * 0.004 * k;
        p.x += Math.max(-8, Math.min(8, p.vx));
        p.y += Math.max(-8, Math.min(8, p.vy));
        p.vx *= 0.55; p.vy *= 0.55;
        p.x = Math.max(20, Math.min(W - 20, p.x));
        p.y = Math.max(20, Math.min(H - 20, p.y));
      }
    };

    const hue = { entity: "var(--ember)", synthesis: "#7c5cbf", source: "#3f7d5d",
                  concept: "#b3762e", query: "#5b7fa6", comparison: "#a65b6b" };
    const r = (n) => 5 + Math.min(9, Math.sqrt(n.links || 0) * 2.2);

    const svg = () => `
        ${links.map(([a, b]) => `<line x1="${pts[a].x.toFixed(1)}" y1="${pts[a].y.toFixed(1)}"
            x2="${pts[b].x.toFixed(1)}" y2="${pts[b].y.toFixed(1)}"
            stroke="var(--border)" stroke-width="1"/>`).join("")}
        ${nodes.map((n, i) => `<a href="#/page/${encodeURIComponent(n.slug)}">
          <circle cx="${pts[i].x.toFixed(1)}" cy="${pts[i].y.toFixed(1)}" r="${r(n)}"
                  fill="${hue[n.type] || "var(--ink-dim)"}" opacity="0.85">
            <title>${esc(n.title || n.slug)} (${esc(n.type)}, ${n.links} inbound)</title>
          </circle>
          ${(n.links >= 2 || nodes.length <= 30) ? `<text x="${(pts[i].x + r(n) + 3).toFixed(1)}"
              y="${(pts[i].y + 3).toFixed(1)}" font-size="10" fill="var(--ink-dim)">${esc(n.slug)}</text>` : ""}
        </a>`).join("")}`;

    const truncated = totalPages > nodes.length
      ? ` Showing the ${nodes.length} best-connected of ${totalPages} pages.` : "";
    if (!view.done(`<h1>Graph</h1>
      <p class="hint">${nodes.length} pages, ${links.length} links. Node size is
      inbound links; color is page type. Click a node to open its page.${esc(truncated)}</p>
      <svg id="graph-svg" viewBox="0 0 ${W} ${H}" style="width:100%;height:auto" role="img"
           aria-label="Page link graph">${svg()}</svg>`)) return;

    // The layout settles across animation frames rather than blocking the
    // main thread: a handful of ticks per frame, painting as it goes, so a
    // 300-node graph never freezes the page. Navigation stops it via the
    // view token.
    let frame = 0;
    const settle = () => {
      if (!view.current() || frame >= 26) return;
      for (let i = 0; i < 10; i++) tick(1 - (frame * 10 + i) / 300);
      frame++;
      const el = $("graph-svg");
      if (el) el.innerHTML = svg();
      requestAnimationFrame(settle);
    };
    requestAnimationFrame(settle);
  } catch (err) {
    if (!err.handled) view.done(banner(err));
  }
}

// showRuns renders the build dashboard: what the queue is doing and what each
// run cost, with the rebuild button that feeds it.
async function showRuns() {
  const view = beginView("Runs", "runs");
  try {
    const runs = await api(`/workspaces/${encodeURIComponent(state.workspace)}/runs`);
    const active = runs.some((r) => r.status === "queued" || r.status === "running");
    const money = (v) => `$${Number(v || 0).toFixed(2)}`;
    const pages = (r) => {
      const parts = [];
      if (r.pagesCreated) parts.push(`${r.pagesCreated} created`);
      if (r.pagesUpdated) parts.push(`${r.pagesUpdated} updated`);
      if (r.pagesDeleted) parts.push(`${r.pagesDeleted} removed`);
      return parts.join(", ");
    };
    if (!view.done(`<h1>Runs</h1>
      <p class="hint">Each run syncs this bench's sources and regenerates only what
      changed; an unchanged bench costs nothing. Runs are built by the worker —
      one at a time per bench.</p>
      <button class="btn" id="run-now" ${active ? "disabled" : ""}>
        ${active ? "A run is already queued or running" : "Rebuild now"}</button>
      <span class="hint" id="run-note" role="status"></span>
      ${runs.map((r) => `
        <div class="review">
          <div class="meta">
            <span class="chip run-${esc(r.status)}">${esc(r.status)}</span>
            <span class="chip">${esc(r.trigger)}</span>
            ${r.ref ? `<span class="chip mono">${esc(r.ref)}</span>` : ""}
            ${timeTag(r.created)}
          </div>
          <strong>${money(r.costUsd)}</strong>
          <span class="count">${pages(r) || (r.status === "no_changes" ? "nothing changed — no cost" : "")}</span>
          ${r.error ? `<div class="detail">${esc(r.error)}</div>` : ""}
          ${r.costUsd > 0 ? `<details class="panel" data-run-items="${esc(r.id)}">
            <summary>cost by unit</summary>
            <div class="detail">loading…</div>
          </details>` : ""}
        </div>`).join("") || `<div class="empty">No runs yet. Rebuild now, or push
          a build from the CLI with <span class="mono">kiln build</span>.</div>`}`)) return;

    // Per-unit cost attribution, fetched lazily on first expand: where the
    // money went, costliest unit first, estimate beside actual.
    for (const d of document.querySelectorAll("[data-run-items]")) {
      d.addEventListener("toggle", async () => {
        if (!d.open || d.dataset.loaded) return;
        d.dataset.loaded = "true";
        const box = d.querySelector(".detail");
        try {
          const items = await api(`/workspaces/${encodeURIComponent(state.workspace)}/runs/${encodeURIComponent(d.dataset.runItems)}/items`);
          box.innerHTML = items.map((it) => `<div class="row">
              <span class="mono">${esc(it.key)}${it.status !== "succeeded" ? ` <span class="chip run-failed">${esc(it.status)}</span>` : ""}</span>
              <span class="count">${money(it.costUsd)}${it.estCostUsd ? ` (est ${money(it.estCostUsd)})` : ""}</span>
            </div>`).join("") || "no unit records";
        } catch (err) {
          if (!err.handled) box.textContent = err.message;
        }
      });
    }

    const btn = $("run-now");
    if (btn && !btn.disabled) once(btn, async () => {
      try {
        const res = await api(`/workspaces/${encodeURIComponent(state.workspace)}/runs`,
          { method: "POST", body: {} });
        toast(res.created ? "Run queued" : "A run was already waiting — joined it");
        showRuns();
      } catch (err) {
        if (err.handled) return;
        const note = $("run-note");
        if (note) { note.textContent = err.message; note.classList.add("error"); }
      }
    });

    // Live-ish while something is moving: re-render on a short leash, guarded
    // by the nav token so leaving the view stops the poll.
    if (active) setTimeout(() => { if (view.current()) showRuns(); }, 5000);
  } catch (err) {
    if (!err.handled) view.done(banner(err));
  }
}

// showMembers manages the org behind this bench: who belongs, with what
// role. Owners and instance admins only; everyone else sees the explanation
// rather than a broken form.
async function showMembers() {
  const view = beginView("Members", "members");
  try {
    let members;
    try {
      members = await api(`/workspaces/${encodeURIComponent(state.workspace)}/members`);
    } catch (err) {
      if (err.handled) return;
      view.done(`<h1>Members</h1><div class="empty">${esc(err.message)}</div>`);
      return;
    }
    const roleSelect = (m) => `<select data-member="${esc(m.userId)}" aria-label="Role for ${esc(m.login)}">
      ${["viewer", "member", "owner"].map((r) =>
        `<option value="${r}" ${m.role === r ? "selected" : ""}>${r}</option>`).join("")}
    </select>`;

    if (!view.done(`<h1>Members</h1>
      <p class="hint">Roles: viewers read, members write content, owners manage
      connectors, credentials, and this list. The last owner cannot be removed.</p>
      <div id="member-note" class="hint" role="status"></div>
      ${members.map((m) => `
        <div class="row">
          <span><strong>${esc(m.login)}</strong>${m.name ? ` <span class="count">${esc(m.name)}</span>` : ""}</span>
          <span>${roleSelect(m)}
            <button class="btn quiet" data-remove="${esc(m.userId)}">remove</button></span>
        </div>`).join("")}
      <label class="group-label" for="member-login">Add a member</label>
      <div class="row">
        <input id="member-login" placeholder="github login" autocomplete="off" spellcheck="false">
        <span>
          <select id="member-role" aria-label="Role for new member">
            <option>viewer</option><option selected>member</option><option>owner</option>
          </select>
          <button class="btn" id="member-add">Add</button>
        </span>
      </div>`)) return;

    const note = (msg, isError) => {
      const n = $("member-note");
      if (n) { n.textContent = msg; n.classList.toggle("error", Boolean(isError)); }
    };
    for (const sel of document.querySelectorAll("select[data-member]")) {
      sel.addEventListener("change", async () => {
        try {
          await api(`/workspaces/${encodeURIComponent(state.workspace)}/members/${encodeURIComponent(sel.dataset.member)}`,
            { method: "PATCH", body: { role: sel.value } });
          toast("Role updated");
        } catch (err) {
          if (!err.handled) { note(err.message, true); showMembers(); }
        }
      });
    }
    for (const b of document.querySelectorAll("[data-remove]")) {
      once(b, async () => {
        try {
          await api(`/workspaces/${encodeURIComponent(state.workspace)}/members/${encodeURIComponent(b.dataset.remove)}`,
            { method: "DELETE" });
          toast("Member removed");
          showMembers();
        } catch (err) {
          if (!err.handled) note(err.message, true);
        }
      });
    }
    once($("member-add"), async () => {
      const login = $("member-login").value.trim();
      if (!login) return;
      try {
        await api(`/workspaces/${encodeURIComponent(state.workspace)}/members`,
          { method: "POST", body: { login, role: $("member-role").value } });
        toast(`Added ${login}`);
        showMembers();
      } catch (err) {
        if (!err.handled) note(err.message, true);
      }
    });
  } catch (err) {
    if (!err.handled) view.done(banner(err));
  }
}

// showSteering edits the purpose and schema documents: the main lever for
// changing a wiki's character, injected into every prompt from the next run.
async function showSteering() {
  const view = beginView("Steering", "steering");
  try {
    const docs = await api(`/workspaces/${encodeURIComponent(state.workspace)}/steering`);
    const label = { purpose: "Purpose — what this wiki is for and who reads it",
                    schema: "Schema — page conventions the builds should follow" };
    const placeholder = {
      purpose: "e.g. Documents the dispatch subsystem for on-call engineers. Assume Go fluency; explain domain terms.",
      schema: "e.g. One entity page per service. Comparisons only for alternatives we actually evaluated.",
    };
    if (!view.done(`<h1>Steering</h1>
      <p class="hint">These documents are injected into every generation prompt.
      Edit them to change what future builds emphasize; existing pages update as
      their sources next change (or with a forced rebuild).</p>
      ${["purpose", "schema"].map((k) => `
        <label class="group-label" for="steering-${k}">${esc(label[k])}</label>
        <textarea id="steering-${k}" rows="8" placeholder="${esc(placeholder[k])}">${esc(docs[k] || "")}</textarea>
        <button class="btn" data-steer="${k}">Save ${k}</button>
        <span class="hint" id="steering-note-${k}" role="status"></span>`).join("")}`)) return;

    for (const b of document.querySelectorAll("[data-steer]")) {
      once(b, async () => {
        const kind = b.dataset.steer;
        const note = $(`steering-note-${kind}`);
        try {
          await api(`/workspaces/${encodeURIComponent(state.workspace)}/steering/${kind}`,
            { method: "PUT", body: { body: $(`steering-${kind}`).value } });
          note.textContent = "";
          note.classList.remove("error");
          toast(`Saved ${kind} — applies from the next build`);
        } catch (err) {
          if (!err.handled) { note.textContent = err.message; note.classList.add("error"); }
        }
      });
    }
  } catch (err) {
    if (!err.handled) view.done(banner(err));
  }
}

async function showSearch(query) {
  const view = beginView(`Search: ${query}`, null);
  $("search").value = query;
  try {
    const hits = await api(`/workspaces/${encodeURIComponent(state.workspace)}/search?q=${encodeURIComponent(query)}`);
    // Snippets arrive with [[[match]]] markers; content is escaped first, so
    // swapping the markers for <mark> afterwards cannot introduce markup.
    // Pairwise replacement: a stray [[[ in page text stays literal.
    const snippet = (s) => esc(s).replace(/\[\[\[([\s\S]*?)\]\]\]/g, "<mark>$1</mark>");
    view.done(`<h1>Search</h1>
      <p class="hint">${hits.length} result${hits.length === 1 ? "" : "s"} for
        <strong>${esc(query)}</strong></p>
      ${hits.map((h) => `<div class="hit">
        <a href="#/page/${encodeURIComponent(h.slug)}">${esc(h.title || h.slug)}</a>
        <span class="count"> · ${esc(h.type)}</span>
        ${h.snippet ? `<div class="snippet">${snippet(h.snippet)}</div>` : ""}
      </div>`).join("") || `<div class="empty">Nothing matched.</div>`}`);
  } catch (err) {
    if (!err.handled) view.done(banner(err));
  }
}

// loadAllPages pages through the summaries endpoint until it runs dry. The
// server caps a single response; stopping at one page silently truncated the
// sidebar and -- worse -- made links to pages beyond the cap render as dead.
async function loadAllPages(slug) {
  const limit = 1000;
  const all = [];
  for (let offset = 0; ; offset += limit) {
    const batch = await api(`/workspaces/${encodeURIComponent(slug)}/pages?limit=${limit}&offset=${offset}`);
    all.push(...batch);
    if (batch.length < limit) return all;
  }
}

async function loadWorkspace(slug) {
  state.workspace = slug;
  localStorage.setItem(WORKSPACE_KEY, slug);
  const meta = state.benches.find((w) => w.slug === slug);
  if (meta) $("bench-name").textContent = `${meta.name} (${meta.pageCount})`;
  for (const b of $("bench-list").querySelectorAll("button"))
    b.setAttribute("aria-current", String(b.dataset.slug === slug));
  $("bench").open = false;
  state.pages = await loadAllPages(slug);
  state.slugs = new Set(state.pages.map((p) => p.slug));
  route();

  // Arm the live poll for this workspace. Clearing first makes workspace
  // switches safe; the first tick captures the baseline revision.
  knownRevision = null;
  revToast?.dismiss?.();
  revToast = null;
  clearInterval(pollTimer);
  pollTimer = setInterval(pollTick, 30000);
  pollTick();
}

function route() {
  const hash = location.hash.replace(/^#\//, "");
  if (hash.startsWith("page/")) return showPage(decodeURIComponent(hash.slice(5)));
  if (hash.startsWith("search/")) return showSearch(decodeURIComponent(hash.slice(7)));
  if (["index", "overview", "log"].includes(hash)) return showArtifact(hash);
  if (hash === "gaps") return showGaps();
  if (hash === "graph") return showGraph();
  if (hash === "reviews") return showReviews(false);
  if (hash === "reviews/all") return showReviews(true);
  if (hash === "runs") return showRuns();
  if (hash === "members") return showMembers();
  if (hash === "steering") return showSteering();
  return showArtifact("overview");
}

function setDrawer(open) {
  document.body.classList.toggle("nav-open", open);
  $("menu").setAttribute("aria-expanded", String(open));
  if (open) $("sidebar").querySelector("summary, input, a")?.focus();
  else $("menu").focus();
}

// ---- command palette --------------------------------------------------------
// Client-side jump over titles/slugs/tags -- the corpus is already loaded, so
// no keystroke costs a request. Full-text search remains the sidebar's job;
// the palette's last row hands off to it.
const VIEW_COMMANDS = [
  { title: "Overview", hash: "#/overview" }, { title: "Index", hash: "#/index" },
  { title: "Graph", hash: "#/graph" },
  { title: "Gaps", hash: "#/gaps" }, { title: "Reviews", hash: "#/reviews" },
  { title: "Runs", hash: "#/runs" }, { title: "Members", hash: "#/members" },
  { title: "Log", hash: "#/log" }, { title: "Steering", hash: "#/steering" },
];
let closePalette = null, palRows = [], palSelection = 0;

function paletteRows(q) {
  const rows = [];
  if (!q) {
    for (const v of VIEW_COMMANDS) rows.push({ html: esc(v.title), kind: "view", hash: v.hash });
    const bySlug = new Map(state.pages.map((p) => [p.slug, p]));
    for (const s of lsGet(recentKey(), []).filter((s) => state.slugs.has(s))) {
      const p = bySlug.get(s);
      rows.push({ html: esc(p.title || p.slug), kind: "recent", hash: `#/page/${encodeURIComponent(p.slug)}` });
    }
    return rows;
  }
  for (const v of VIEW_COMMANDS) {
    const m = fuzzy(q, v.title);
    if (m) rows.push({ html: fuzzyHi(v.title, m.idx), kind: "view", hash: v.hash, score: m.score + 4 });
  }
  for (const r of matchPages(q)) {
    rows.push({
      html: r.field === "title" ? fuzzyHi(r.text, r.idx) : esc(r.p.title || r.p.slug),
      kind: r.field === "title" ? r.p.type : `${r.p.type} · ${r.field}`,
      hash: `#/page/${encodeURIComponent(r.p.slug)}`,
      score: r.score,
    });
  }
  rows.sort((a, b) => (b.score || 0) - (a.score || 0));
  // Score everything, render 15: the cap bounds DOM churn, not match quality.
  rows.length = Math.min(rows.length, 15);
  rows.push({ html: `Search full text for “${esc(q)}”`, kind: "search", hash: `#/search/${encodeURIComponent(q)}` });
  return rows;
}

function renderPalette(q) {
  palRows = paletteRows(q.trim());
  palSelection = 0;
  $("palette-list").innerHTML = palRows.length
    ? palRows.map((r, i) => `<li id="pal-opt-${i}" role="option" aria-selected="${i === 0}"${
        i === 0 ? ' class="active"' : ""}><span>${r.html}</span><span class="kind">${esc(r.kind)}</span></li>`).join("")
    : `<li class="pal-empty">No pages yet.</li>`;
  $("palette-input").setAttribute("aria-activedescendant", palRows.length ? "pal-opt-0" : "");
}

function movePaletteSelection(delta) {
  if (!palRows.length) return;
  palSelection = (palSelection + delta + palRows.length) % palRows.length;
  [...$("palette-list").children].forEach((li, i) => {
    li.classList.toggle("active", i === palSelection);
    li.setAttribute("aria-selected", String(i === palSelection));
  });
  $("palette-input").setAttribute("aria-activedescendant", `pal-opt-${palSelection}`);
  document.getElementById(`pal-opt-${palSelection}`)?.scrollIntoView({ block: "nearest" });
}

function pickPalette(i) {
  const row = palRows[i];
  if (!row) return;
  closePalette?.();
  // Same-hash picks (e.g. re-opening the current page) never fire
  // hashchange, so route explicitly.
  if (location.hash === row.hash) route();
  else location.hash = row.hash;
}

function openPalette() {
  if (closePalette) return;
  $("palette-input").value = "";
  renderPalette("");
  closePalette = openOverlay($("palette"), () => { closePalette = null; });
}

async function boot() {
  $("menu").addEventListener("click", () =>
    setDrawer(!document.body.classList.contains("nav-open")));
  document.addEventListener("keydown", (e) => {
    // Escape priority: palette (handled inside its own overlay) -> hover
    // preview -> drawer.
    if (e.key === "Escape") {
      if (hidePreview()) return;
      if (document.body.classList.contains("nav-open")) setDrawer(false);
      return;
    }
    // "/" or cmd/ctrl-K opens the command palette from anywhere outside a field.
    const inField = /^(INPUT|TEXTAREA|SELECT)$/.test(document.activeElement?.tagName ?? "");
    if (!inField && (e.key === "/" || ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k"))) {
      e.preventDefault();
      openPalette();
    }
  });

  // Palette wiring: type to filter, arrows to move, Enter to go.
  $("palette-input").addEventListener("input", (e) => renderPalette(e.target.value));
  $("palette-input").addEventListener("keydown", (e) => {
    if (e.key === "ArrowDown") { e.preventDefault(); movePaletteSelection(1); }
    else if (e.key === "ArrowUp") { e.preventDefault(); movePaletteSelection(-1); }
    else if (e.key === "Enter") { e.preventDefault(); pickPalette(palSelection); }
  });
  // mousedown, not click: it wins the race against the input losing focus.
  $("palette-list").addEventListener("mousedown", (e) => {
    const li = e.target.closest("li[role=option]");
    if (li) { e.preventDefault(); pickPalette([...$("palette-list").children].indexOf(li)); }
  });

  // Sidebar quick-filter: instant, client-side, Enter opens the first match.
  $("tree-filter").addEventListener("input", (e) => {
    treeFilter = e.target.value.trim();
    renderTree(lastActiveSlug);
  });
  $("tree-filter").addEventListener("keydown", (e) => {
    if (e.key === "Enter") {
      const first = $("tree").querySelector("a");
      if (first) {
        e.target.value = "";
        treeFilter = "";
        location.hash = first.getAttribute("href");
      }
    } else if (e.key === "Escape" && e.target.value) {
      e.stopPropagation();
      e.target.value = "";
      treeFilter = "";
      renderTree(lastActiveSlug);
    }
  });
  // Tapping the backdrop strip dismisses the drawer.
  document.addEventListener("click", (e) => {
    if (document.body.classList.contains("nav-open") &&
        !$("sidebar").contains(e.target) && e.target !== $("menu")) {
      setDrawer(false);
    }
  });

  // Wikilink previews: one delegated listener pair, pointer devices only.
  if (HOVERABLE.matches) {
    const target = (e) => e.target.closest?.("a.wikilink:not(.dead)");
    $("main").addEventListener("mouseover", (e) => { const a = target(e); if (a) showPreviewFor(a); });
    $("main").addEventListener("mouseout", (e) => { if (target(e)) hidePreview(); });
    $("main").addEventListener("focusin", (e) => { const a = target(e); if (a) showPreviewFor(a); });
    $("main").addEventListener("focusout", (e) => { if (target(e)) hidePreview(); });
  }
  // Navigation always dismisses a lingering card or pending timer.
  window.addEventListener("hashchange", () => hidePreview());

  // Back-to-top after ~2 viewports; focus returns to main to keep tab order.
  const toTop = $("to-top");
  let scrollScheduled = false;
  window.addEventListener("scroll", () => {
    if (scrollScheduled) return;
    scrollScheduled = true;
    requestAnimationFrame(() => {
      scrollScheduled = false;
      toTop.hidden = scrollY < innerHeight * 2;
    });
  }, { passive: true });
  toTop.addEventListener("click", () => {
    window.scrollTo({ top: 0, behavior: scrollBehavior() });
    $("main").focus({ preventScroll: true });
  });

  try {
    const { version, githubSignIn } = await api("/version");
    $("version").textContent = version;
    state.githubSignIn = Boolean(githubSignIn);

    const workspaces = await api("/workspaces");
    if (!workspaces.length) {
      $("main").innerHTML = `<div class="empty">
        No benches yet. Run <code>kiln build &lt;path&gt;</code> to create one.</div>`;
      return;
    }
    state.benches = workspaces;
    $("bench-list").innerHTML = workspaces
      .map((w) => `<button type="button" data-slug="${esc(w.slug)}">${esc(w.name)} (${esc(w.pageCount)})</button>`)
      .join("");

    $("bench-list").addEventListener("click", (e) => {
      const b = e.target.closest("button[data-slug]");
      if (!b) return;
      if (b.dataset.slug === state.workspace) { $("bench").open = false; return; }
      // A new workspace starts at its overview: carrying the previous page's
      // hash across would greet it with "page not found".
      location.hash = "#/overview";
      loadWorkspace(b.dataset.slug);
    });
    window.addEventListener("hashchange", route);
    $("search").addEventListener("keydown", (e) => {
      if (e.key === "Enter" && e.target.value.trim()) {
        // Search is a route like any other view: Back returns to the results
        // and the query survives reload and sharing.
        location.hash = "#/search/" + encodeURIComponent(e.target.value.trim());
      }
    });
    if (localStorage.getItem(TOKEN_KEY) || csrfToken()) {
      const so = $("signout");
      so.hidden = false;
      so.addEventListener("click", async () => {
        localStorage.removeItem(TOKEN_KEY);
        // A GitHub session is server-side: revoke it, not just the cookie.
        if (csrfToken()) {
          try {
            await fetch("/auth/logout", {
              method: "POST", headers: { "X-CSRF-Token": csrfToken() },
            });
          } catch { /* the reload lands on the sign-in form either way */ }
        }
        location.reload();
      });
    }

    // The workspace choice survives a reload; a remembered slug that no
    // longer exists falls back to the first.
    const stored = localStorage.getItem(WORKSPACE_KEY);
    const initial = workspaces.some((w) => w.slug === stored) ? stored : workspaces[0].slug;
    await loadWorkspace(initial);
  } catch (err) {
    if (err.handled) return;
    $("main").innerHTML = banner(err, true);
    wireBannerRetry();
  }
}

boot();
