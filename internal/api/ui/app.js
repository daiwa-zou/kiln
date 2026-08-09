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
//
// Distance is counted in calendar days, not in elapsed milliseconds, because
// "today" and "yesterday" are calendar words: whether a page was written this
// morning is a question about the date, not about how many hours have passed.
// Rounding elapsed time answered the second question and got the first wrong --
// eighteen hours is nearer a day than nothing, so every page flipped to
// "yesterday" around noon on the day it was written. A timestamp late last
// night had the mirror bug, reading "today" until the small hours.
//
// A bare YYYY-MM-DD is parsed as local midnight rather than left to the
// built-in rule, which reads it as *UTC* midnight and dates every page a day
// early for readers west of UTC. Timestamps that carry a time and a zone are
// left to Date, which parses them correctly.
function relTime(iso) {
  const dateOnly = typeof iso === "string" && /^\d{4}-\d{2}-\d{2}$/.test(iso.trim());
  const d = dateOnly ? new Date(`${iso.trim()}T00:00:00`) : new Date(iso);
  if (isNaN(d)) return String(iso ?? "");
  const midnight = (x) => new Date(x.getFullYear(), x.getMonth(), x.getDate()).getTime();
  // Rounding absorbs the 23- and 25-hour days daylight saving puts between
  // two midnights; the quotient is otherwise a whole number already.
  const days = Math.round((midnight(new Date()) - midnight(d)) / 86400000);
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

// ---- what a view is for -----------------------------------------------------
// Every view used to open with a paragraph explaining itself. That paragraph is
// read once and then becomes furniture: it sits above the content the reader
// came for, on every visit, forever. The explanations are worth keeping -- this
// tool has concepts a first-time reader genuinely needs -- so they move behind a
// "?" beside the title, where someone who wants them can ask and everyone else
// gets their content at the top of the page.
//
// Keyed by view, so a view names its help rather than carrying the prose.
const VIEW_HELP = {
  ingest: {
    title: "Ingest",
    body: `<p>What this bench reads, and when it last read it. A repository, web
      pages, and uploaded documents all fire into one wiki, so pages can link
      across the boundary rather than forming separate wikis.</p>
      <p>Each ingest regenerates only the pages whose sources changed — an
      unchanged bench costs nothing. Runs execute one at a time per bench.</p>
      <p>Pausing a source skips it on the next ingest without touching the pages
      it already produced.</p>`,
  },
  reviews: {
    title: "Reviews",
    body: `<p>Decisions a build could not make on its own: a contradiction
      between sources, an assertion it could not corroborate, or a page whose
      source has disappeared.</p>
      <p>Resolving one records your answer. An approved deletion is applied by
      the next build — nothing is destroyed until you say so.</p>
      <p>A question about the material can be handed back to a worker, which
      re-reads the sources and attaches what it finds. You still make the
      call.</p>`,
  },
  gaps: {
    title: "Gaps",
    body: `<p>Pages that existing content links to but that have never been
      written. They are the wiki's own account of what it knows it is
      missing.</p>
      <p>This is what lets an agent tell <em>"the wiki says nothing about X"</em>
      from <em>"the wiki has not covered X yet"</em> — a distinction that is
      invisible without it.</p>`,
  },
  graph: {
    title: "Graph",
    body: `<p>Pages as nodes, wikilinks as edges. Clusters are subjects that
      reference each other; an isolated node is a page nothing links to,
      which is usually worth a look.</p>`,
  },
  steering: {
    title: "Steering",
    body: `<p>Standing instructions handed to the agent on every build: what this
      bench is for, what to emphasize, what to leave alone.</p>
      <p>Edits apply to future builds. Existing pages incorporate them when
      their sources next change, or on a forced rebuild — steering shapes pages
      as they are written rather than rewriting what is already there.</p>`,
  },
  mcp: {
    title: "Agent access (MCP)",
    body: `<p>MCP is how an agent reads this bench: it asks the compiled wiki
      instead of re-reading your sources every time, which is faster and far
      cheaper than handing it the raw material.</p>
      <p>The agent reads over the HTTP API, so it needs no database credentials
      and works against this instance from anywhere it can reach it. A key
      carries the read scope only and sees exactly the benches you do.</p>
      <p>Keys are shown once. kiln stores a hash, not the key, so a lost one is
      replaced rather than recovered.</p>`,
  },
  members: {
    title: "Members",
    body: `<p>Who can reach this bench and what they may do. Viewers can read,
      members can edit content, and owners manage connectors, credentials, and
      membership.</p>
      <p>The last owner cannot be removed — a bench nobody can administer is a
      bench nobody can fix.</p>`,
  },
};

// openHelp shows one view's explanation in the shared modal.
function openHelp(key) {
  const help = VIEW_HELP[key];
  if (!help) return;
  $("help-title").textContent = help.title;
  $("help-body").innerHTML = help.body;
  const close = openOverlay($("help"), () => { $("help-body").innerHTML = ""; });
  $("help-x").onclick = close;
}

// viewHead renders a view's title with its "?" beside it. Views with nothing to
// explain pass no key and get a bare heading.
const viewHead = (title, key) => `
  <div class="view-head">
    <h1>${esc(title)}</h1>
    ${VIEW_HELP[key] ? `<button class="help-btn" data-help="${esc(key)}"
      aria-label="What is this page for?" title="What is this page for?">?</button>` : ""}
  </div>`;

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
  if (opts.body instanceof FormData) {
    // Multipart uploads: the browser sets Content-Type with its boundary.
    body = opts.body;
  } else if (opts.body !== undefined) {
    headers["Content-Type"] = "application/json";
    body = JSON.stringify(opts.body);
  }
  const cached = method === "GET" ? etagCache.get(path) : null;
  if (cached) headers["If-None-Match"] = cached.etag;

  // Search-as-you-type abandons a request per keystroke. An abort is a normal
  // outcome there, not a failure, so it is pre-marked handled: every existing
  // catch already skips handled errors, and none of them should paint a banner
  // because the user kept typing.
  let res;
  try {
    res = await fetch(`/api/v1${path}`, { method, headers, body, signal: opts.signal });
  } catch (err) {
    if (err?.name === "AbortError") {
      err.aborted = true;
      err.handled = true;
      throw err;
    }
    // Any other rejected fetch is a network-level failure: the server stopped,
    // the connection dropped, the laptop slept. The browser's own words for it
    // are "Load failed" in Safari and "Failed to fetch" in Chrome, and both
    // reach the reader verbatim -- beside a filename, "Load failed" reads as
    // the document having been rejected rather than as nothing having been
    // asked. Say which of the two it was.
    const wrapped = new Error("could not reach the server — check that kiln is still running", { cause: err });
    wrapped.offline = true;
    throw wrapped;
  }
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
      <p class="hint login-alt">Or use an access token.</p>` : `
      <p class="hint">Sign-in requires an access token. Create one with
        <code>kiln admin token create --login you</code>.
        The token is stored only in this browser.</p>`}
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
  $("tree").innerHTML = html || `<div class="hint tree-empty">Pages appear here after the first ingest.</div>`;

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
  // Sidebar-wide, not .nav-only: Steering and Members moved to the settings
  // menu at the foot of the rail and would otherwise never mark themselves.
  for (const a of document.querySelectorAll("#sidebar a[data-view]")) {
    if (a.dataset.view === view) a.setAttribute("aria-current", "page");
    else a.removeAttribute("aria-current");
  }
  syncNavForEmptyBench();
}

// On a bench with nothing built, "Overview" is a promise the app cannot keep
// and the reading views lead nowhere. The entry says what it does instead,
// and the views that cannot have content yet are visibly inert rather than
// silently empty -- a disabled-looking link the user can still click beats
// four identical empty pages they have to discover one by one.
function syncNavForEmptyBench() {
  const empty = state.workspace && !state.pages.length;
  const overview = document.querySelector('.nav a[data-view="overview"]');
  if (overview) overview.textContent = empty ? "Get started" : "Overview";
  for (const view of ["index", "graph", "gaps", "log"]) {
    const a = document.querySelector(`.nav a[data-view="${view}"]`);
    if (a) a.classList.toggle("nav-waiting", Boolean(empty));
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
  // The view name drives the layout width: prose views keep the reading
  // measure, data views use the room they need.
  m.dataset.view = view || "page";
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
          ${(p.tags || []).map((t) => `<span class="chip">${esc(t)}</span>`).join("")}
          ${p.updated ? `<span class="meta-when">updated ${timeTag(p.updated)}</span>` : ""}
        </div>
      </div>
      <div class="prose">${renderMarkdown(p.body)}</div>
      ${pagerFor(p)}
      ${backlinks === null ? "" : `<div class="page-foot">
        <div class="group-label">Linked from</div>
        ${backlinks.length ? `<div class="meta">${backlinks.map((b) =>
          `<a class="chip" href="#/page/${encodeURIComponent(b.slug)}">${esc(b.title || b.slug)}</a>`).join("")}</div>`
        : `<div class="hint">No pages link here yet.</div>`}
      </div>`}
      ${(p.sources || []).length ? `<div class="page-foot tight">
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
      <p class="hint">Corrections stay attached to this page and are applied to
        every future rebuild. Pin one when the page states something incorrect;
        the next regeneration takes it into account.</p>
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
      // The reader is here because they clicked a nav entry, not because
      // they have a terminal open: name the step that produces this.
      view.done(`<div class="empty">No ${kind} yet — it is written by the first
        ingest. <a href="#/overview">Start here</a>.</div>`);
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
      view.done(`${viewHead("Gaps", "gaps")}<div class="empty">
        ${state.pages.length
          ? "No gaps — every link resolves to an existing page."
          : `Nothing to check yet — gaps are links the wiki wants and does not have. <a href="#/overview">Start here</a>.`}</div>`);
      return;
    }
    view.done(`${viewHead("Gaps", "gaps")}
      ${gaps.map((g) => `<div class="row">
        <span class="mono">${esc(g.slug)}</span>
        <span class="count">wanted by ${esc(g.wantedBy)} page${g.wantedBy === 1 ? "" : "s"}</span>
      </div>`).join("")}`);
  } catch (err) {
    if (!err.handled) view.done(banner(err));
  }
}

// ---- review icons -----------------------------------------------------------
// A review list repeats the same handful of words down the page: what is being
// asked about, and once answered, what was decided. As glyphs those two columns
// are scannable at a glance; as text they were near-identical chips to be read
// one by one. Drawn in currentColor like the source-row controls, aria-hidden
// because the word rides alongside as the accessible name -- replaced on screen,
// never actually removed. Names are distinct from sources.js's: both files are
// classic scripts sharing one global scope, where a repeated top-level const is
// a SyntaxError that would take down the whole UI.
const iconSave = `<svg class="icon" viewBox="0 0 16 16" aria-hidden="true"><path fill-rule="evenodd" d="M2 2h9.2L14 4.8V14H2V2zM5.2 3h4.4v3.2H5.2zM4 9h8v4H4z"/></svg>`;
const iconDeletion = `<svg class="icon" viewBox="0 0 16 16" aria-hidden="true"><path fill-rule="evenodd" d="M6 1h4v1h4v2H2V2h4V1zM3 5h10l-.8 10H3.8L3 5zm3 2v6h1V7H6zm3 0v6h1V7H9z"/></svg>`;
const iconContradiction = `<svg class="icon" viewBox="0 0 16 16" aria-hidden="true"><path d="M2.6 5.6h10.8v1.8H2.6zM2.6 9h10.8v1.8H2.6zM10.4 1.4l1.7.9-6.5 12.4-1.7-.9z"/></svg>`;
const iconUncertain = `<svg class="icon" viewBox="0 0 16 16" aria-hidden="true"><path d="M8 1.6c-2.3 0-4 1.5-4.2 3.6l2 .2C5.9 4.2 6.8 3.5 8 3.5c1.2 0 2 .6 2 1.5 0 .7-.4 1.2-1.3 1.9-1.1.9-1.7 1.6-1.7 2.9v.6h2v-.5c0-.8.3-1.2 1.2-1.9C11.4 7.1 12 6.2 12 5c0-2-1.6-3.4-4-3.4zM6.9 12.1h2.2v2.3H6.9z"/></svg>`;
const iconGap = `<svg class="icon" viewBox="0 0 16 16" aria-hidden="true"><path d="M2 2h4.4v1.8H3.8v2.6H2V2zM9.6 2H14v4.4h-1.8V3.8H9.6V2zM2 9.6h1.8v2.6h2.6V14H2V9.6zM12.2 9.6H14V14H9.6v-1.8h2.6V9.6z"/></svg>`;
const iconBudget = `<svg class="icon" viewBox="0 0 16 16" aria-hidden="true"><path fill-rule="evenodd" d="M1 3h14v10H1V3zm1.8 1.8v6.4h10.4V4.8H2.8z"/><path d="M8 5.9a2.1 2.1 0 1 1 0 4.2 2.1 2.1 0 0 1 0-4.2z"/></svg>`;
const iconStorage = `<svg class="icon" viewBox="0 0 16 16" aria-hidden="true"><path d="M8 1.2c3.3 0 5.8 1 5.8 2.2S11.3 5.6 8 5.6 2.2 4.6 2.2 3.4 4.7 1.2 8 1.2zM2.2 5.4c1.3.9 3.4 1.4 5.8 1.4s4.5-.5 5.8-1.4v2.4c0 1.2-2.5 2.2-5.8 2.2s-5.8-1-5.8-2.2V5.4zM2.2 9.6c1.3.9 3.4 1.4 5.8 1.4s4.5-.5 5.8-1.4V12c0 1.2-2.5 2.2-5.8 2.2S2.2 13.2 2.2 12V9.6z"/></svg>`;
const iconApproved = `<svg class="icon" viewBox="0 0 16 16" aria-hidden="true"><path fill-rule="evenodd" d="M8 1a7 7 0 1 0 0 14A7 7 0 0 0 8 1zm3.5 4.6l1.2 1.2-5.6 5.6-3.8-3.8 1.2-1.2 2.6 2.6 4.4-4.4z"/></svg>`;
const iconResolved = `<svg class="icon" viewBox="0 0 16 16" aria-hidden="true"><path fill-rule="evenodd" d="M8 1a7 7 0 1 0 0 14A7 7 0 0 0 8 1zM4.5 7h7v2h-7V7z"/></svg>`;
const iconApprove = `<svg class="icon" viewBox="0 0 16 16" aria-hidden="true"><path d="M6.2 12.6L1.9 8.3l1.6-1.6 2.7 2.7 6.3-6.3 1.6 1.6z"/></svg>`;
const iconKeep = `<svg class="icon" viewBox="0 0 16 16" aria-hidden="true"><path d="M8 1.2l5.6 2.3v3.9c0 3.3-2.3 6.1-5.6 7.4-3.3-1.3-5.6-4.1-5.6-7.4V3.5L8 1.2z"/></svg>`;
const iconDismiss = `<svg class="icon" viewBox="0 0 16 16" aria-hidden="true"><path d="M12.7 4.7l-1.4-1.4L8 6.6 4.7 3.3 3.3 4.7 6.6 8l-3.3 3.3 1.4 1.4L8 9.4l3.3 3.3 1.4-1.4L9.4 8z"/></svg>`;
const iconResearch = `<svg class="icon" viewBox="0 0 16 16" aria-hidden="true"><path fill-rule="evenodd" d="M6.9 1.4a5.5 5.5 0 1 0 3.3 9.9l3.1 3.1 1.3-1.3-3.1-3.1a5.5 5.5 0 0 0-4.6-8.6zm0 1.9a3.6 3.6 0 1 1 0 7.2 3.6 3.6 0 0 1 0-7.2z"/></svg>`;

const REVIEW_KIND_ICONS = {
  deletion: iconDeletion, contradiction: iconContradiction, uncertain: iconUncertain,
  gap: iconGap, budget: iconBudget, storage: iconStorage,
};
const REVIEW_STATUS_ICONS = { approved: iconApproved, resolved: iconResolved };
const REVIEW_ACTION_ICONS = { approve: iconApprove, keep: iconKeep, dismiss: iconDismiss };

// iconChip swaps a word for its glyph and keeps the word as the tooltip and the
// accessible name. `kind` is free-form text chosen by whatever filed the review,
// so anything unmapped falls back to the word it always was -- a chip with no
// glyph and no label would be a decision nobody can read. `extra` carries a
// palette class (the kind-* colours) for callers that have one.
const iconChip = (icons, name, extra = "") => icons[name]
  ? `<span class="chip icon-chip ${extra}" title="${esc(name)}">${icons[name]}<span class="sr-only">${esc(name)}</span></span>`
  : `<span class="chip ${extra}">${esc(name)}</span>`;

// reviewResearch renders the research half of a card: the button that hands
// the question to a worker, the note that one is already reading, and the
// findings once they land.
//
// The server decides researchable, not this: whether reading can settle a
// question is a property of the queue, and offering a button the API would
// refuse is worse than offering none.
function reviewResearch(r) {
  // The findings body carries its own newlines and is rendered pre-wrap, so
  // nothing may sit between its element tags but the text itself -- the
  // template's own indentation would otherwise print as leading whitespace.
  const findings = r.research
    ? `<div class="research-findings">
         <div class="meta">${iconChip({ research: iconResearch }, "research")}${timeTag(r.researched, "read ")}</div>
         <div class="detail">${esc(r.research)}</div>
       </div>`
    : "";
  if (r.researching) {
    return `${findings}<p class="hint" role="status">A worker is reading the sources for this
      question. Findings appear here when it finishes.</p>`;
  }
  if (!r.researchable) return findings;
  return `${findings}
    <button class="btn quiet icon-btn" data-research="${esc(r.id)}"
      aria-label="research this question" title="research this question">${iconResearch}</button>`;
}

// showReviews renders the wiki's questions for its humans: contradictions and
// uncertainties the agent flagged, and deletions awaiting approval.
// history is the resolved queue: what was decided, and when. It is a different
// question from the inbox -- "what needs me" versus "what did we settle" -- so
// it is its own view behind its own button rather than a filter toggle that
// mixed answered questions in among the unanswered ones.
async function showReviews(history) {
  const view = beginView(history ? "Resolved reviews" : "Reviews", "reviews");
  try {
    // "answered" covers both statuses a resolution writes, 'resolved' and
    // 'approved'; asking for either alone would hide half the history.
    const reviews = await api(
      `/workspaces/${encodeURIComponent(state.workspace)}/reviews?status=${history ? "answered" : "open"}`);
    const nav = history
      ? `<a class="btn quiet" href="#/reviews">Back to open reviews</a>`
      : `<a class="btn quiet" href="#/reviews/history">View resolved history</a>`;

    if (!reviews.length) {
      view.done(`${viewHead(history ? "Resolved reviews" : "Reviews", "reviews")}
        <div class="meta">${nav}</div>
        <div class="empty">${history
          ? "Nothing resolved yet. Answered reviews are kept here as a record of what was decided."
          : `No open reviews. Builds file one here when they need a decision, such
             as confirming a deletion after a source disappears.`}</div>`);
      return;
    }
    if (!view.done(`${viewHead(history ? "Resolved reviews" : "Reviews", "reviews")}
      <div class="meta">${nav}</div>
      <div id="review-note" class="hint" role="status"></div>
      ${reviews.map((r) => `
        <div class="review">
          <div class="meta">
            ${iconChip(REVIEW_KIND_ICONS, r.kind)}
            ${r.unit ? unitKeyHTML(r.unit) : ""}
            ${r.pageSlug ? `<a class="chip" href="#/page/${encodeURIComponent(r.pageSlug)}">${esc(r.pageSlug)}</a>` : ""}
            ${timeTag(r.created)}
            ${r.status !== "open" ? iconChip(REVIEW_STATUS_ICONS, r.status) : ""}
            ${r.status !== "open" ? timeTag(r.resolved, "answered ") : ""}
          </div>
          <strong>${esc(r.title)}</strong>
          <div class="detail">${esc(r.detail)}</div>
          ${reviewResearch(r)}
          ${r.status === "open" ? (r.actions && r.actions.length ? r.actions : ["dismiss"]).map((a) =>
            `<button class="btn ${a === "approve" ? "" : "quiet"}${REVIEW_ACTION_ICONS[a] ? " icon-btn" : ""}"
               data-review="${esc(r.id)}" data-action="${esc(a)}"
               aria-label="${esc(a)}" title="${esc(a)}">${REVIEW_ACTION_ICONS[a] || esc(a)}</button>`
          ).join("") : ""}
        </div>`).join("")}`)) return;

    for (const b of document.querySelectorAll("[data-research]")) {
      once(b, async () => {
        try {
          await api(`/workspaces/${encodeURIComponent(state.workspace)}/reviews/${encodeURIComponent(b.dataset.research)}/research`,
            { method: "POST", body: {} });
          // Nothing is resolved, so the badge is left alone: the question is
          // still waiting for a human, now with a reader working on it.
          toast("Research queued");
          showReviews(history);
        } catch (err) {
          if (err.handled) return;
          const note = $("review-note");
          if (note) { note.textContent = err.message; note.classList.add("error"); }
        }
      });
    }

    for (const b of document.querySelectorAll("[data-review]")) {
      once(b, async () => {
        // Approval authorizes the next build's deletion cascade -- the most
        // consequential click in the UI gets a two-step confirm. Via the shared
        // helper now that the button is a glyph: it saves and restores
        // innerHTML, where the local copy this replaced rewrote textContent and
        // would have disarmed into a button reading "approve" in bare text.
        if (b.dataset.action === "approve" && !armButton(b, "approve")) return;
        try {
          await api(`/workspaces/${encodeURIComponent(state.workspace)}/reviews/${encodeURIComponent(b.dataset.review)}/resolve`,
            { method: "POST", body: { action: b.dataset.action } });
          // Decrement the badge locally: resolution may not bump the wiki
          // revision, so an ETag'd refetch could 304 to the stale list.
          updateReviewsBadge(Number($("reviews-badge").textContent || 1) - 1);
          toast(`Review ${b.dataset.action === "approve" ? "approved" : "resolved"}`);
          showReviews(history);
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
      view.done(`${viewHead("Graph", "graph")}<div class="empty">No pages yet — the graph draws
        itself once this bench has been ingested. <a href="#/overview">Start here</a>.</div>`);
      return;
    }

    // Sized after render: the canvas fills the content column and the
    // viewport height, so the graph uses the screen it is given.
    let W = 900, H = 640;
    const bySlug = new Map(nodes.map((n, i) => [n.slug, i]));
    const pts = nodes.map(() => ({ x: 0, y: 0, vx: 0, vy: 0, pinned: false }));
    // Physics scale with the canvas: the same 19 nodes should spread over a
    // 2000px canvas the way they spread over a 900px one. Equilibrium radius
    // goes as (repulse/gravity)^(1/3), so both knobs move together.
    let repulse = 2600, springLen = 90, gravity = 0.004;
    const seed = () => pts.forEach((p, i) => {
      // Deterministic golden-angle disc seeding: same graph, same picture.
      p.x = W / 2 + Math.sqrt(i + 1) * (Math.min(W, H) / 26) * Math.cos(i * 2.39996);
      p.y = H / 2 + Math.sqrt(i + 1) * (Math.min(W, H) / 26) * Math.sin(i * 2.39996);
    });
    const links = edges
      .map((e) => [bySlug.get(e.from), bySlug.get(e.to)])
      .filter(([a, b]) => a !== undefined && b !== undefined && a !== b);
    const neighbors = nodes.map(() => new Set());
    for (const [a, b] of links) { neighbors[a].add(b); neighbors[b].add(a); }

    const tick = (k) => {
      for (let i = 0; i < pts.length; i++) {
        for (let j = i + 1; j < pts.length; j++) {
          let dx = pts[i].x - pts[j].x, dy = pts[i].y - pts[j].y;
          const d2 = Math.max(64, dx * dx + dy * dy);
          const f = (repulse / d2) * k;
          const d = Math.sqrt(d2);
          dx /= d; dy /= d;
          pts[i].vx += dx * f; pts[i].vy += dy * f;
          pts[j].vx -= dx * f; pts[j].vy -= dy * f;
        }
      }
      for (const [a, b] of links) {
        const dx = pts[b].x - pts[a].x, dy = pts[b].y - pts[a].y;
        const d = Math.max(1, Math.hypot(dx, dy));
        const f = (d - springLen) * 0.015 * k;
        pts[a].vx += (dx / d) * f; pts[a].vy += (dy / d) * f;
        pts[b].vx -= (dx / d) * f; pts[b].vy -= (dy / d) * f;
      }
      for (const p of pts) {
        if (p.pinned) { p.vx = 0; p.vy = 0; continue; }
        // Gravity aims at the content-box center (labels hang right), and
        // is the only confinement: a hard position clamp would pile nodes
        // along the canvas edges whenever the layout outgrows it.
        p.vx += (W / 2 - 65 - p.x) * gravity * k;
        p.vy += (H / 2 - p.y) * gravity * k;
        p.x += Math.max(-8, Math.min(8, p.vx));
        p.y += Math.max(-8, Math.min(8, p.vy));
        p.vx *= 0.55; p.vy *= 0.55;
      }
    };

    const hue = { entity: "var(--ember)", synthesis: "#7c5cbf", source: "#3f7d5d",
                  concept: "#b3762e", query: "#5b7fa6", comparison: "#a65b6b" };
    const r = (n) => 5 + Math.min(9, Math.sqrt(n.links || 0) * 2.2);

    const typeCounts = {};
    for (const n of nodes) typeCounts[n.type] = (typeCounts[n.type] || 0) + 1;
    const plural = (n, w) => `${n} ${w}${n === 1 ? "" : "s"}`;
    const truncated = totalPages > nodes.length
      ? ` · showing the ${nodes.length} most linked of ${totalPages}` : "";

    const icon = (d) => `<svg width="13" height="13" viewBox="0 0 24 24" fill="none"
      stroke="currentColor" stroke-width="2" stroke-linecap="round"
      stroke-linejoin="round" aria-hidden="true">${d}</svg>`;
    const iconReset = icon(`<polyline points="1 4 1 10 7 10"/>
      <path d="M3.51 15a9 9 0 1 0 2.13-9.36L1 10"/>`);
    const iconMax = icon(`<path d="M8 3H5a2 2 0 0 0-2 2v3"/><path d="M16 3h3a2 2 0 0 1 2 2v3"/>
      <path d="M16 21h3a2 2 0 0 0 2-2v-3"/><path d="M8 21H5a2 2 0 0 1-2-2v-3"/>`);
    const iconMin = icon(`<path d="M8 3v3a2 2 0 0 1-2 2H3"/><path d="M16 3v3a2 2 0 0 0 2 2h3"/>
      <path d="M16 21v-3a2 2 0 0 1 2-2h3"/><path d="M8 21v-3a2 2 0 0 0-2-2H3"/>`);

    if (!view.done(`${viewHead("Graph", "graph")}
      <p class="hint">${plural(nodes.length, "page")} · ${plural(links.length, "link")}${esc(truncated)}</p>
      <div id="graph-wrap">
      <div class="graph-toolbar">
        <div class="graph-legend" role="group" aria-label="Filter by page type">
          ${Object.entries(typeCounts).map(([t, c]) =>
            `<button class="chip" data-type="${esc(t)}" aria-pressed="true">${esc(t)} ${c}</button>`).join("")}
        </div>
        <div class="graph-controls" role="group" aria-label="View controls">
          <button class="chip quiet graph-zoom" id="graph-zoom-out" aria-label="Zoom out" title="Zoom out">&minus;</button>
          <span class="graph-zoom-level" id="graph-zoom-level" title="Zoom level">100%</span>
          <button class="chip quiet graph-zoom" id="graph-zoom-in" aria-label="Zoom in" title="Zoom in">+</button>
          <button class="chip quiet graph-icon" id="graph-reset" aria-label="Reset view" title="Reset view">${iconReset}</button>
          <button class="chip quiet graph-icon" id="graph-full" aria-label="Full screen" title="Full screen">${iconMax}</button>
        </div>
      </div>
      <svg id="graph-svg" role="img" aria-label="Page link graph"></svg>
      </div>`)) return;

    const svg = $("graph-svg");
    {
      const rect = svg.getBoundingClientRect();
      W = Math.max(700, Math.round(rect.width));
      // Cap the aspect: a portrait window would otherwise make a canvas far
      // taller than a roughly-round layout can fill.
      H = Math.max(520, Math.min(Math.round(window.innerHeight - rect.top - 28),
                                 Math.round(W * 1.2)));
      svg.style.height = H + "px";
      const scale = Math.min(W, H) / 640;
      repulse = 2600 * scale * scale;
      springLen = 90 * scale;
      gravity = 0.004 / scale;
    }
    seed();

    // Retained DOM instead of per-frame innerHTML: interaction needs stable
    // elements to drag, hover, and filter, and attribute updates are cheaper
    // than reparsing the world anyway.
    const SVGNS = "http://www.w3.org/2000/svg";
    const mk = (tag, attrs) => {
      const el = document.createElementNS(SVGNS, tag);
      for (const [k, v] of Object.entries(attrs)) el.setAttribute(k, v);
      return el;
    };
    const edgeEls = links.map(([a, b]) => {
      const el = mk("line", { stroke: "var(--border)", "stroke-width": "1" });
      el._a = a; el._b = b;
      svg.appendChild(el);
      return el;
    });
    const nodeEls = nodes.map((n, i) => {
      const g = mk("g", { class: "graph-node", "data-i": i, tabindex: "0" });
      g.setAttribute("aria-label", `${n.title || n.slug} (${n.type})`);
      const c = mk("circle", { r: r(n), fill: hue[n.type] || "var(--ink-dim)", opacity: "0.85" });
      const title = mk("title", {});
      title.textContent = `${n.title || n.slug} (${n.type}, ${n.links} inbound)`;
      c.appendChild(title);
      g.appendChild(c);
      if (n.links >= 2 || nodes.length <= 30) {
        const t = mk("text", { "font-size": "10", fill: "var(--ink-dim)" });
        t.textContent = n.slug;
        g.appendChild(t);
      }
      svg.appendChild(g);
      return g;
    });

    const position = () => {
      for (const el of edgeEls) {
        el.setAttribute("x1", pts[el._a].x.toFixed(1));
        el.setAttribute("y1", pts[el._a].y.toFixed(1));
        el.setAttribute("x2", pts[el._b].x.toFixed(1));
        el.setAttribute("y2", pts[el._b].y.toFixed(1));
      }
      nodeEls.forEach((g, i) => {
        const c = g.firstChild;
        c.setAttribute("cx", pts[i].x.toFixed(1));
        c.setAttribute("cy", pts[i].y.toFixed(1));
        const t = g.querySelector("text");
        if (t) {
          t.setAttribute("x", (pts[i].x + r(nodes[i]) + 3).toFixed(1));
          t.setAttribute("y", (pts[i].y + 3).toFixed(1));
        }
      });
    };
    position();

    // The layout settles across animation frames rather than blocking the
    // main thread; interactions can re-arm it so the graph keeps breathing
    // after a drag.
    let budget = 260;
    let settling = false;
    let autoFitted = false;
    const settle = () => {
      if (!view.current()) { settling = false; return; }
      if (budget <= 0) {
        settling = false;
        if (!autoFitted) { autoFitted = true; fitView(); }
        return;
      }
      for (let i = 0; i < 10 && budget > 0; i++, budget--) tick(Math.max(0.15, budget / 300));
      position();
      requestAnimationFrame(settle);
    };
    const reheat = (amount) => {
      budget = Math.max(budget, amount);
      if (!settling) { settling = true; requestAnimationFrame(settle); }
    };
    reheat(260);

    // --- pan & zoom -------------------------------------------------------
    const vb = { x: 0, y: 0, w: W, h: H };
    const zoomLabel = $("graph-zoom-level");
    const applyVB = () => {
      svg.setAttribute("viewBox", `${vb.x} ${vb.y} ${vb.w} ${vb.h}`);
      zoomLabel.textContent = Math.round((W / vb.w) * 100) + "%";
    };
    applyVB();
    // fitView fills the canvas by spreading the layout, never by zooming the
    // camera: nodes move apart while circles and labels keep their natural
    // size. Zooming in to fill made 19 nodes look like beach balls; leaving
    // the settled cluster alone left 80% of a big screen empty.
    const bounds = () => {
      let m = null;
      pts.forEach((p, i) => {
        if (nodeEls[i].classList.contains("graph-hidden")) return;
        if (!m) m = { x0: p.x, x1: p.x, y0: p.y, y1: p.y };
        m.x0 = Math.min(m.x0, p.x); m.x1 = Math.max(m.x1, p.x);
        m.y0 = Math.min(m.y0, p.y); m.y1 = Math.max(m.y1, p.y);
      });
      return m;
    };
    const fitView = () => {
      let m = bounds();
      if (!m) return;
      // 150px of right margin leaves room for the labels hanging off nodes;
      // the growth cap keeps a near-degenerate layout from being flung to
      // the corners.
      let s = Math.min(6,
        (W - 60 - 150) / Math.max(1, m.x1 - m.x0),
        (H - 60) / Math.max(1, m.y1 - m.y0));
      // Shrinking is allowed too (leaving full screen), but never below the
      // spacing floor that keeps nodes apart; the camera absorbs the rest.
      if (s < 1) {
        s = Math.max(s, (40 * Math.sqrt(pts.length)) /
          Math.max(1, m.x1 - m.x0, m.y1 - m.y0));
      }
      const grow = Math.abs(s - 1) > 0.02 ? s : 1;
      const cx = (m.x0 + m.x1) / 2, cy = (m.y0 + m.y1) / 2;
      for (const p of pts) {
        // Center the content box, not the node box: labels hang 150px off
        // the right, so the node cloud sits 65px left of center.
        p.x = W / 2 - 65 + (p.x - cx) * grow;
        p.y = H / 2 + (p.y - cy) * grow;
      }
      if (grow !== 1) {
        // Keep the physics in equilibrium at the rescaled spacing
        // (radius ~ (repulse/gravity)^(1/3)), or the relaxation pass
        // below would simply undo the rescale.
        repulse *= grow * grow;
        springLen *= grow;
        gravity /= grow;
      }
      position();
      m = bounds();
      const bx = m.x0 - 20, by = m.y0 - 20;
      const bw = m.x1 - m.x0 + 170, bh = m.y1 - m.y0 + 40;
      if (bw <= W && bh <= H) {
        vb.x = 0; vb.y = 0; vb.w = W; vb.h = H;
      } else {
        // Still oversized (the spacing floor refused to shrink further):
        // the camera frames it instead.
        vb.x = bx; vb.y = by;
        vb.w = Math.max(320, bw);
        vb.h = Math.max(240, bh);
      }
      applyVB();
      // Uniform rescaling magnifies the old layout's irregularities: let
      // the simulation relax into even spacing at the new scale.
      reheat(140);
    };
    const toSVG = (e) => {
      const rect = svg.getBoundingClientRect();
      return {
        x: vb.x + ((e.clientX - rect.left) / rect.width) * vb.w,
        y: vb.y + ((e.clientY - rect.top) / rect.height) * vb.h,
      };
    };
    // zoomBy scales the viewBox about an anchor point: the cursor for wheel
    // zoom, the canvas center for the toolbar buttons.
    const zoomBy = (scale, ax, ay) => {
      const next = Math.min(W * 3, Math.max(W / 8, vb.w * scale));
      const f = next / vb.w;
      vb.x = ax - (ax - vb.x) * f;
      vb.y = ay - (ay - vb.y) * f;
      vb.w *= f; vb.h *= f;
      applyVB();
    };
    svg.addEventListener("wheel", (e) => {
      e.preventDefault();
      const p = toSVG(e);
      zoomBy(e.deltaY > 0 ? 1.12 : 1 / 1.12, p.x, p.y);
    }, { passive: false });
    $("graph-zoom-in").addEventListener("click", () =>
      zoomBy(1 / 1.35, vb.x + vb.w / 2, vb.y + vb.h / 2));
    $("graph-zoom-out").addEventListener("click", () =>
      zoomBy(1.35, vb.x + vb.w / 2, vb.y + vb.h / 2));
    $("graph-reset").addEventListener("click", fitView);

    // Full screen wraps the toolbar too, so filtering and zooming keep
    // working inside it. Native fullscreen when the environment allows it;
    // a fixed overlay covering the window when it doesn't (embedded panes
    // deny the Fullscreen API). Either way the canvas is re-measured and
    // the layout re-fitted to the new geometry.
    const wrap = $("graph-wrap");
    const isFull = () => !!document.fullscreenElement || wrap.classList.contains("graph-fs");
    const resync = () => {
      const fb = $("graph-full");
      fb.innerHTML = isFull() ? iconMin : iconMax;
      fb.title = isFull() ? "Exit full screen" : "Full screen";
      fb.setAttribute("aria-label", fb.title);
      const rect = svg.getBoundingClientRect();
      W = Math.max(320, Math.round(rect.width));
      H = Math.max(320, Math.min(Math.round(window.innerHeight - rect.top - 28),
                                 Math.round(W * 1.2)));
      svg.style.height = H + "px";
      fitView();
    };
    const enterOverlay = () => {
      if (isFull()) return;
      wrap.classList.add("graph-fs");
      resync();
    };
    $("graph-full").addEventListener("click", () => {
      if (document.fullscreenElement) { document.exitFullscreen(); return; }
      if (wrap.classList.contains("graph-fs")) {
        wrap.classList.remove("graph-fs");
        resync();
        return;
      }
      wrap.requestFullscreen().catch(enterOverlay);
      // Some embedded panes leave the fullscreen promise forever pending
      // instead of rejecting; fall back if nothing materializes.
      setTimeout(enterOverlay, 400);
    });
    const onFullscreen = () => {
      if (!view.current()) {
        document.removeEventListener("fullscreenchange", onFullscreen);
        return;
      }
      resync();
    };
    document.addEventListener("fullscreenchange", onFullscreen);
    const onEsc = (e) => {
      if (!view.current()) {
        document.removeEventListener("keydown", onEsc);
        return;
      }
      if (e.key === "Escape" && wrap.classList.contains("graph-fs")) {
        wrap.classList.remove("graph-fs");
        resync();
      }
    };
    document.addEventListener("keydown", onEsc);

    // --- drag (nodes) and pan (background) --------------------------------
    // One pointer state machine; a press that never travels is a click and
    // opens the page, so navigation survives the drag handlers.
    let drag = null;
    svg.addEventListener("pointerdown", (e) => {
      const g = e.target.closest("g.graph-node");
      const p = toSVG(e);
      drag = g
        ? { i: Number(g.dataset.i), moved: 0 }
        : { pan: { x: vb.x, y: vb.y }, from: { cx: e.clientX, cy: e.clientY }, moved: 0 };
      if (g) pts[drag.i].pinned = true;
      svg.setPointerCapture(e.pointerId);
      drag.last = p;
    });
    svg.addEventListener("pointermove", (e) => {
      if (!drag) return;
      const p = toSVG(e);
      if (drag.i !== undefined) {
        drag.moved += Math.hypot(p.x - drag.last.x, p.y - drag.last.y);
        pts[drag.i].x = p.x;
        pts[drag.i].y = p.y;
        drag.last = p;
        position();
        reheat(40); // neighbors follow the dragged node
      } else {
        const rect = svg.getBoundingClientRect();
        vb.x = drag.pan.x - ((e.clientX - drag.from.cx) / rect.width) * vb.w;
        vb.y = drag.pan.y - ((e.clientY - drag.from.cy) / rect.height) * vb.h;
        drag.moved += Math.abs(e.movementX) + Math.abs(e.movementY);
        applyVB();
      }
    });
    const endDrag = (e) => {
      if (!drag) return;
      if (drag.i !== undefined) {
        pts[drag.i].pinned = false;
        if (drag.moved < 3) location.hash = "#/page/" + encodeURIComponent(nodes[drag.i].slug);
      }
      drag = null;
    };
    svg.addEventListener("pointerup", endDrag);
    svg.addEventListener("pointercancel", endDrag);
    for (const g of nodeEls) {
      g.addEventListener("keydown", (e) => {
        if (e.key === "Enter") location.hash = "#/page/" + encodeURIComponent(nodes[g.dataset.i].slug);
      });
    }

    // --- hover: light the neighborhood, dim the rest ----------------------
    svg.addEventListener("pointerover", (e) => {
      const g = e.target.closest("g.graph-node");
      if (!g) return;
      const i = Number(g.dataset.i);
      nodeEls.forEach((el, j) =>
        el.classList.toggle("graph-dim", j !== i && !neighbors[i].has(j)));
      for (const el of edgeEls) {
        const hot = el._a === i || el._b === i;
        el.classList.toggle("graph-dim", !hot);
        el.classList.toggle("graph-hot", hot);
      }
    });
    svg.addEventListener("pointerout", (e) => {
      if (!e.target.closest("g.graph-node")) return;
      for (const el of [...nodeEls, ...edgeEls]) el.classList.remove("graph-dim", "graph-hot");
    });

    // --- legend: toggle types on and off ----------------------------------
    const hidden = new Set();
    for (const b of document.querySelectorAll(".graph-legend [data-type]")) {
      // CSSOM, not a style attribute: the CSP (style-src 'self') refuses
      // inline style attributes.
      b.style.borderColor = hue[b.dataset.type] || "var(--border)";
      b.addEventListener("click", () => {
        const t = b.dataset.type;
        hidden.has(t) ? hidden.delete(t) : hidden.add(t);
        b.setAttribute("aria-pressed", String(!hidden.has(t)));
        b.classList.toggle("graph-off", hidden.has(t));
        nodeEls.forEach((el, i) =>
          el.classList.toggle("graph-hidden", hidden.has(nodes[i].type)));
        for (const el of edgeEls) {
          el.classList.toggle("graph-hidden",
            hidden.has(nodes[el._a].type) || hidden.has(nodes[el._b].type));
        }
      });
    }
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
      view.done(`${viewHead("Members", "members")}<div class="empty">${esc(err.message)}</div>`);
      return;
    }
    const roleSelect = (m) => `<select data-member="${esc(m.userId)}" aria-label="Role for ${esc(m.login)}">
      ${["viewer", "member", "owner"].map((r) =>
        `<option value="${r}" ${m.role === r ? "selected" : ""}>${r}</option>`).join("")}
    </select>`;

    if (!view.done(`${viewHead("Members", "members")}
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
// showMCP is the setup page for reading this bench from an agent. Everything an
// agent needs is here rather than in a README the reader would have to go and
// find: the command, the URL of this very instance, the bench slug, and the key
// -- which used to require a shell on the host running `kiln admin token
// create`, a step nobody with only a browser could take.
async function showMCP() {
  const view = beginView("Agent access", "mcp");
  try {
    // Whether keys exist at all is a property of the deployment. With auth
    // disabled there is no identity to attach one to, the token routes are not
    // mounted, and asking for them would surface a bare 404 -- so ask who the
    // caller is first and say the useful thing instead.
    let keys = null, keysErr = "", anonymous = false;
    try {
      const me = await api("/me");
      anonymous = Boolean(me?.anonymous);
    } catch (err) {
      if (err?.handled) return;
      // No /me at all is the same situation from the reader's point of view:
      // this deployment has no accounts, so it has no keys.
      anonymous = true;
    }
    if (!anonymous) {
      try {
        keys = await api("/tokens");
      } catch (err) {
        if (err?.handled) return;
        keysErr = err.message;
      }
    }

    const ws = state.workspace || "your-bench";
    const origin = location.origin;
    const config = JSON.stringify({
      mcpServers: {
        kiln: {
          command: "kiln",
          args: ["mcp", "--url", origin, "--workspace", ws],
          ...(anonymous ? {} : { env: { KILN_TOKEN: "<your key>" } }),
        },
      },
    }, null, 2);

    if (!view.done(`${viewHead("Agent access", "mcp")}
      <p class="hint">Point Claude, or any MCP-capable agent, at this bench so it
        answers from the compiled wiki instead of re-reading your sources.</p>

      <div class="group-label">1 · Configuration</div>
      <p class="hint">Add this to your agent's MCP configuration. The command runs
        the same <span class="mono">kiln</span> binary that serves this page.</p>
      <div class="copybox">
        <pre class="mono" id="mcp-config">${esc(config)}</pre>
        <button class="btn quiet" data-copy="mcp-config">Copy</button>
      </div>
      <p class="hint">Reading is over the HTTP API, so the agent needs no database
        credentials and works against this instance from anywhere it can reach
        <span class="mono">${esc(origin)}</span>. Drop
        <span class="mono">--workspace</span> and every tool takes a bench argument
        instead.</p>

      <div class="group-label">2 · Keys</div>
      ${keys
        ? `<p class="hint">A key carries the read scope and nothing else, and sees
             exactly the benches you do. It is shown once — kiln stores only a
             hash — so copy it when it appears.</p>
           <div class="meta">
             <input id="mcp-key-name" type="text" placeholder="What is it for? e.g. laptop Claude"
                    aria-label="Key name" maxlength="60">
             <button class="btn" id="mcp-key-new">Generate a key</button>
           </div>
           <div id="mcp-key-note" class="hint" role="status"></div>
           <div id="mcp-key-fresh"></div>
           <div id="mcp-keys">${keyRows(keys)}</div>`
        : `<div class="empty">${anonymous
             ? `This instance runs with authentication disabled, so agents connect
                without a key — leave <span class="mono">KILN_TOKEN</span> out of
                the configuration above.`
             : esc(keysErr || "This instance does not issue keys.")}</div>`}

      <div class="group-label">3 · What the agent gets</div>
      <p class="hint">Seven tools. <span class="mono">search_wiki</span> and
        <span class="mono">read_page</span> carry most traffic, with
        <span class="mono">wiki_overview</span> for orientation,
        <span class="mono">list_benches</span> and <span class="mono">list_pages</span>
        for enumeration, <span class="mono">page_backlinks</span> for context, and
        <span class="mono">wiki_gaps</span> — which is what lets an agent tell
        <em>"the wiki says nothing about X"</em> from
        <em>"the wiki has not covered X yet"</em>.</p>`)) return;

    wireCopyButtons();
    if (!keys) return;

    const note = (msg, isErr) => {
      const n = $("mcp-key-note");
      if (n) { n.textContent = msg || ""; n.classList.toggle("error", Boolean(isErr)); }
    };

    once($("mcp-key-new"), async () => {
      note("");
      try {
        const made = await api("/tokens", {
          method: "POST", body: { name: $("mcp-key-name").value },
        });
        $("mcp-key-name").value = "";
        // Shown once, in full, with the warning attached to the thing itself
        // rather than to a paragraph above it that has already been read.
        $("mcp-key-fresh").innerHTML = `
          <div class="review key-fresh">
            <strong>${esc(made.name)} — copy it now</strong>
            <div class="hint">This is the only time it is shown. kiln keeps a hash,
              so it cannot be shown again; generate another if it is lost.</div>
            <div class="copybox">
              <pre class="mono" id="mcp-key-plain">${esc(made.token)}</pre>
              <button class="btn quiet" data-copy="mcp-key-plain">Copy</button>
            </div>
          </div>`;
        wireCopyButtons();
        await refreshKeys();
      } catch (err) {
        if (!err.handled) note(err.message, true);
      }
    });

    async function refreshKeys() {
      const rows = await api("/tokens");
      $("mcp-keys").innerHTML = keyRows(rows);
      wireRevoke();
    }

    function wireRevoke() {
      for (const b of document.querySelectorAll("[data-revoke]")) {
        once(b, async () => {
          if (!armButton(b, "revoke")) return;
          try {
            await api(`/tokens/${encodeURIComponent(b.dataset.revoke)}`, { method: "DELETE" });
            toast("Key revoked");
            await refreshKeys();
          } catch (err) {
            if (!err.handled) note(err.message, true);
          }
        });
      }
    }
    wireRevoke();
  } catch (err) {
    if (!err.handled) view.done(banner(err));
  }
}

// keyRows lists live keys. Never the key itself -- only a hash is stored, so
// there is nothing here to leak even if this markup were.
//
// Expiry is an absolute date, not relTime: that renders distance into the past
// and answers "today" for everything still ahead, which for a key a year from
// expiring is the one reading that would alarm someone for no reason.
function keyRows(keys) {
  if (!keys.length) {
    return `<div class="empty">No keys yet. Generate one to connect an agent.</div>`;
  }
  return `<div class="review">${keys.map((k) => `
    <div class="row">
      <span>${esc(k.name)} <span class="count">${(k.scopes || []).join(", ")}</span></span>
      <span>
        <span class="count">${k.lastUsed ? `last used ${esc(relTime(k.lastUsed))}` : "never used"}</span>
        ${k.expires ? `<span class="count">expires ${esc(k.expires.slice(0, 10))}</span>` : ""}
        <button class="btn quiet" data-revoke="${esc(k.id)}">revoke</button>
      </span>
    </div>`).join("")}</div>`;
}

// wireCopyButtons binds every [data-copy] to the id it names. Falls back to
// selecting the text when the clipboard is unavailable -- an insecure origin,
// or a browser that refuses -- so the button is never a dead end.
function wireCopyButtons() {
  for (const b of document.querySelectorAll("[data-copy]")) {
    b.addEventListener("click", async () => {
      const src = $(b.dataset.copy);
      if (!src) return;
      try {
        await navigator.clipboard.writeText(src.textContent);
        toast("Copied");
      } catch {
        const range = document.createRange();
        range.selectNodeContents(src);
        const sel = getSelection();
        sel.removeAllRanges();
        sel.addRange(range);
        toast("Select and copy — this browser blocked the clipboard");
      }
    });
  }
}

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
    if (!view.done(`${viewHead("Steering", "steering")}
      ${["purpose", "schema"].map((k) => `
        <label class="group-label" for="steering-${k}">${esc(label[k])}</label>
        <textarea id="steering-${k}" rows="8" placeholder="${esc(placeholder[k])}">${esc(docs[k] || "")}</textarea>
        <button class="btn icon-btn" data-steer="${k}"
                aria-label="Save ${esc(k)}" title="Save ${esc(k)}">${iconSave}</button>
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

// Snippets arrive with [[[match]]] markers; content is escaped first, so
// swapping the markers for <mark> afterwards cannot introduce markup. Pairwise
// replacement: a stray [[[ in page text stays literal.
const snippetHTML = (s) => esc(s).replace(/\[\[\[([\s\S]*?)\]\]\]/g, "<mark>$1</mark>");

// searchURL is one query string for both consumers of search, so the dropdown
// and the results page can never disagree about what the query meant. prefix=1
// is the as-you-type contract: the trailing word is still being typed.
const searchURL = (query, limit) =>
  `/workspaces/${encodeURIComponent(state.workspace)}/search?q=${encodeURIComponent(query)}` +
  `&prefix=1${limit ? `&limit=${limit}` : ""}`;

function searchHTML(query, hits) {
  if (!query) {
    return `<h1>Search</h1><div class="empty">Type in the search box to search every page.</div>`;
  }
  return `<h1>Search</h1>
    <p class="hint" id="search-count" role="status">${hits.length} result${hits.length === 1 ? "" : "s"} for
      <strong>${esc(query)}</strong></p>
    <div class="hits">${hits.map((h) => `<div class="hit">
      <a href="#/page/${encodeURIComponent(h.slug)}">${esc(h.title || h.slug)}</a>
      <span class="count"> · ${esc(h.type)}</span>
      ${h.snippet ? `<div class="snippet">${snippetHTML(h.snippet)}</div>` : ""}
    </div>`).join("") || `<div class="empty">Nothing matched.</div>`}</div>`;
}

// searchSeq orders the responses a live search produces. The nav token cannot
// do this job alone: every keystroke here belongs to the same navigation, so
// without a second counter a slow request for "wor" would overwrite the
// results already rendered for "worker".
let searchSeq = 0;
let searchAbort = null;

// showSearch renders the results view, either as an arrival (a hash change, a
// reload, a shared link) or as a live update from a keystroke. The difference
// is deliberate and total: an arrival is a navigation -- it resets scroll, takes
// focus, and shows a skeleton -- while a keystroke must do none of those things,
// because the caret is in the search box and every one of them would yank it out
// or make the list strobe.
async function showSearch(query, live = false) {
  const title = query ? `Search: ${query}` : "Search";
  const view = live ? null : beginView(title, null);
  const myNav = nav;
  const main = $("main");
  const paint = (html) => {
    if (nav !== myNav) return false; // the user navigated away mid-flight
    if (view) return view.done(html);
    main.innerHTML = html;
    return true;
  };
  if (live) {
    setTitle(title);
  } else {
    $("search").value = query;
    // beginView moved focus to the results, as it should for a navigation.
    // This view is the exception: the next thing anyone does with a result
    // list is refine it, and the field they refine it in is the one they just
    // pressed Enter in. Taking the caret out of it would end the interaction
    // the results view exists to continue.
    $("search").focus({ preventScroll: true });
  }

  // Claim the view and drop any request still in flight BEFORE the empty-query
  // exit. Emptying the box is a result in its own right, and a search issued
  // for the text that was just deleted would otherwise land afterwards and
  // paint itself back over the cleared page.
  const my = ++searchSeq;
  searchAbort?.abort();
  searchAbort = null;
  if (!query) { paint(searchHTML("", [])); return; }
  searchAbort = new AbortController();
  // The pending class dims the outgoing results, but only after a quarter
  // second (the delay lives in the CSS transition). A local search answers in
  // single-digit milliseconds; flashing a loading state at that speed is worse
  // than showing none at all.
  if (live) main.querySelector(".hits")?.classList.add("pending");

  try {
    const hits = await api(searchURL(query), { signal: searchAbort.signal });
    if (my !== searchSeq) return; // a later keystroke owns the view now
    paint(searchHTML(query, hits));
  } catch (err) {
    if (err.handled || my !== searchSeq) return;
    paint(banner(err));
  }
}

const onSearchView = () => location.hash.startsWith("#/search/") || location.hash === "#/search";

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
  if (["index", "overview", "log"].includes(hash)) return overviewOrGetStarted(hash);
  if (hash === "gaps") return showGaps();
  if (hash === "graph") return showGraph();
  if (hash === "reviews") return showReviews(false);
  // reviews/all is the old hash for the same idea; keep it routable.
  if (hash === "reviews/history" || hash === "reviews/all") return showReviews(true);
  // Sources and runs merged into one view; every hash it has ever had stays
  // routable so bookmarks and habit survive.
  if (hash === "ingest" || hash === "ingestion" || hash === "sources" || hash === "runs") return showSources();
  if (hash === "members") return showMembers();
  if (hash === "mcp") return showMCP();
  if (hash === "steering") return showSteering();
  // The default landing, and the explicit one, both route through the same
  // check -- an empty hash is the common case on first load.
  return overviewOrGetStarted("overview");
}

// A bench with no pages has no overview to show. Rather than an empty
// artifact, it gets the checklist that leads to one.
function overviewOrGetStarted(kind) {
  if (kind === "overview" && !state.pages.length) return showGetStarted();
  return showArtifact(kind);
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
  { title: "Graph", hash: "#/graph" }, { title: "Gaps", hash: "#/gaps" },
  { title: "Ingest", hash: "#/ingest" }, { title: "Log", hash: "#/log" },
  { title: "Agent access (MCP)", hash: "#/mcp" },
  { title: "Reviews", hash: "#/reviews" },
  { title: "Steering", hash: "#/steering" }, { title: "Members", hash: "#/members" },
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

// ---- search suggestions -----------------------------------------------------
// The sidebar box answers on every keystroke, from two sources that answer
// different questions. The local pass -- "did you mean this page?" -- scores
// titles, slugs and tags out of the corpus already in memory, so it paints
// before the keystroke's request has left the machine. The remote pass --
// "which pages say this?" -- follows a debounce later with snippets. They are
// independent: a slow network delays the second half of the list, never the
// first, so the box never feels like it stalled.
const SUGGEST_DEBOUNCE = 160;
const SUGGEST_PAGES = 5;
const SUGGEST_HITS = 6;

let sugRows = [], sugSel = -1, sugTimer = null, sugAbort = null, sugSeq = 0, sugQuery = "";

// matchNames is name completion, deliberately not the palette's matcher. A
// subsequence match is right for a jump list someone opened on purpose and
// will read; in a search box it answers "retr" with internal/connector, which
// reads as a wrong result rather than a loose one. Here the query has to
// appear in the name, in order and unbroken.
function matchNames(q) {
  const needle = q.toLowerCase();
  const out = [];
  for (const p of state.pages) {
    // First field to match wins the row, so a page is offered under its title
    // where it has one and its slug or tags only when that is the actual hit.
    for (const [field, text] of
      [["title", p.title || ""], ["slug", p.slug], ["tags", (p.tags || []).join(" ")]]) {
      const at = text.toLowerCase().indexOf(needle);
      if (at < 0) continue;
      // Word starts beat mid-word matches, and earlier beats later: "work"
      // should offer Worker before it offers Network topology.
      const score = (at === 0 || " -_/".includes(text[at - 1]) ? 100 : 0) - at;
      // Indices, not a substring: fuzzyHi escapes each run separately, so the
      // highlight can never desynchronize from the text it marks. Length comes
      // from the needle's code units because that is what indexOf counted.
      out.push({ p, field, text, score, idx: Array.from({ length: needle.length }, (_, i) => at + i) });
      break;
    }
  }
  return out.sort((a, b) => b.score - a.score);
}

function suggestRows(q, hits) {
  const rows = [], bySlug = new Map();
  for (const r of matchNames(q).slice(0, SUGGEST_PAGES)) {
    const row = {
      html: r.field === "title" ? fuzzyHi(r.text, r.idx) : esc(r.p.title || r.p.slug),
      kind: r.field === "title" ? r.p.type : `${r.p.type} · ${r.field}`,
      hash: `#/page/${encodeURIComponent(r.p.slug)}`,
    };
    bySlug.set(r.p.slug, row);
    rows.push(row);
  }
  // A page both passes found appears once and keeps the position it was first
  // shown in -- a row that jumps when the network answers is a row someone
  // clicks by mistake -- but it takes the snippet with it, because "this page
  // is named that" and "here is the sentence you asked about" are both worth
  // knowing and only one of them was on screen.
  for (const h of hits) {
    const seen = bySlug.get(h.slug);
    if (seen) { seen.snippet ??= h.snippet; continue; }
    const row = {
      html: esc(h.title || h.slug), kind: h.type, snippet: h.snippet,
      hash: `#/page/${encodeURIComponent(h.slug)}`,
    };
    bySlug.set(h.slug, row);
    rows.push(row);
  }
  // Short enough that the query itself survives the sidebar's width: the row
  // whose whole job is to show what will be searched must not be the row that
  // truncates the search terms away.
  rows.push({
    html: `All results for “${esc(q)}”`, kind: "full text",
    hash: `#/search/${encodeURIComponent(q)}`,
  });
  return rows;
}

function renderSuggest(q, hits = []) {
  // Re-anchor the selection to the row it was on, not to its index: the remote
  // rows land in the middle of the list, so a fixed index would slide the
  // highlight onto a different result between a keystroke and its Enter.
  const held = sugRows[sugSel]?.hash;
  sugRows = suggestRows(q, hits);
  sugSel = held ? sugRows.findIndex((r) => r.hash === held) : -1;
  $("suggest").innerHTML = sugRows.map((r, i) => `<li id="sug-opt-${i}" role="option"
      aria-selected="${i === sugSel}"${i === sugSel ? ' class="active"' : ""}>
      <span class="sug-line"><span>${r.html}</span><span class="kind">${esc(r.kind)}</span></span>
      ${r.snippet ? `<span class="snippet">${snippetHTML(r.snippet)}</span>` : ""}
    </li>`).join("");
  $("suggest").hidden = false;
  $("search").setAttribute("aria-expanded", "true");
  syncSuggestActive();
  // Rows minus the standing "search full text" row: announcing "1 suggestion"
  // for a query that matched nothing would be a lie told once per keystroke.
  const n = sugRows.length - 1;
  $("suggest-status").textContent = n ? `${n} suggestion${n === 1 ? "" : "s"}` : "";
}

function closeSuggest() {
  clearTimeout(sugTimer);
  sugAbort?.abort();
  sugRows = [];
  sugSel = -1;
  $("suggest").hidden = true;
  $("suggest").innerHTML = "";
  $("suggest-status").textContent = "";
  $("search").setAttribute("aria-expanded", "false");
  $("search").removeAttribute("aria-activedescendant");
}

function syncSuggestActive() {
  const list = $("suggest");
  [...list.children].forEach((li, i) => {
    li.classList.toggle("active", i === sugSel);
    li.setAttribute("aria-selected", String(i === sugSel));
  });
  if (sugSel < 0) {
    $("search").removeAttribute("aria-activedescendant");
    return;
  }
  $("search").setAttribute("aria-activedescendant", `sug-opt-${sugSel}`);
  list.children[sugSel]?.scrollIntoView({ block: "nearest" });
}

// -1 is the typed query itself. Arrowing off either end returns to it, which
// is the only way back to "search for exactly what I wrote" once the list has
// been walked into.
function moveSuggest(delta) {
  if (!sugRows.length) return;
  sugSel += delta;
  if (sugSel < -1) sugSel = sugRows.length - 1;
  if (sugSel >= sugRows.length) sugSel = -1;
  syncSuggestActive();
}

function pickSuggest(i) {
  const row = sugRows[i];
  if (!row) return;
  closeSuggest();
  if (location.hash === row.hash) route();
  else location.hash = row.hash;
}

async function fetchSuggestions(q) {
  const my = ++sugSeq;
  sugAbort?.abort();
  sugAbort = new AbortController();
  try {
    const hits = await api(searchURL(q, SUGGEST_HITS), { signal: sugAbort.signal });
    // Two guards, because they catch different things: the sequence number
    // drops a response overtaken by a later one, and the query check drops one
    // whose box has since been cleared or navigated away from.
    if (my === sugSeq && sugQuery === q && !$("suggest").hidden) renderSuggest(q, hits);
  } catch { /* the local matches stand, and Enter still runs the real search */ }
}

// Where a keystroke's answer goes depends on what the main pane already shows.
// On the results view it goes there: those results are richer, they are the
// thing being looked at, and a dropdown over them would be a second and poorer
// copy of the same answer. Everywhere else the dropdown is the only place an
// answer can go without throwing away the page being read.
function onSearchInput() {
  const q = $("search").value.trim();
  const inline = onSearchView();
  sugQuery = q;
  clearTimeout(sugTimer);

  // The page tree narrows to the same query. This was a second input four rows
  // below this one: identical to look at, answering a different question, and
  // advertising a "/" shortcut that opens the command palette instead of
  // focusing it. One box now covers both halves of finding something -- the
  // tree shows which pages are *called* this, the dropdown offers the jump,
  // and Enter searches what the pages actually *say*.
  if (treeFilter !== q) {
    treeFilter = q;
    renderTree(lastActiveSlug);
  }
  if (inline || !q) closeSuggest();
  else renderSuggest(q); // instant, local, no network

  if (!q) {
    if (inline) { history.replaceState(null, "", "#/search/"); showSearch("", true); }
    return;
  }
  sugTimer = setTimeout(() => {
    if (sugQuery !== q) return;
    if (!onSearchView()) { fetchSuggestions(q); return; }
    // replaceState rather than assigning the hash: the URL has to keep up with
    // the box so a reload or a copied link lands on what is on screen, but one
    // history entry per keystroke would turn Back into a spellcheck of
    // everything the user typed on the way here.
    history.replaceState(null, "", `#/search/${encodeURIComponent(q)}`);
    showSearch(q, true);
  }, SUGGEST_DEBOUNCE);
}

function wireSearchBox() {
  const input = $("search");
  input.addEventListener("input", onSearchInput);
  // Returning to a box that still holds a query reopens what it was showing,
  // without spending a request to do it -- unless the results view is up, which
  // is already showing more than the dropdown could.
  input.addEventListener("focus", () => {
    if (input.value.trim() && !onSearchView()) renderSuggest(input.value.trim());
  });
  input.addEventListener("keydown", (e) => {
    if (e.key === "ArrowDown") { e.preventDefault(); moveSuggest(1); }
    else if (e.key === "ArrowUp") { e.preventDefault(); moveSuggest(-1); }
    else if (e.key === "Enter") {
      e.preventDefault();
      if (sugSel >= 0) { pickSuggest(sugSel); return; }
      const q = input.value.trim();
      // Search is a route like any other view: Back returns to the results and
      // the query survives reload and sharing.
      if (q) { closeSuggest(); location.hash = "#/search/" + encodeURIComponent(q); }
    } else if (e.key === "Escape") {
      // First Escape dismisses the list, a second clears the box. Stopped here
      // so neither ever reaches the document handler and closes the mobile
      // drawer out from under someone dismissing a dropdown.
      e.stopPropagation();
      if (!$("suggest").hidden) closeSuggest();
      else if (input.value) { input.value = ""; onSearchInput(); }
    }
  });
  // Tabbing out of the box closes the list. Picking an option never lands here:
  // the mousedown handler below preventDefaults, so focus never leaves.
  input.addEventListener("focusout", (e) => {
    if (!e.relatedTarget?.closest?.(".searchbox")) closeSuggest();
  });
  // mousedown, not click: it wins the race against the input losing focus.
  $("suggest").addEventListener("mousedown", (e) => {
    const li = e.target.closest("li[role=option]");
    if (li) { e.preventDefault(); pickSuggest([...$("suggest").children].indexOf(li)); }
  });
  document.addEventListener("click", (e) => {
    if (!e.target.closest(".searchbox")) closeSuggest();
  });
}

// showAccount fills the account sheet with whoever is signed in. Three
// answers are possible and each is worth saying plainly: a named user, a
// deployment with authentication switched off, and a bearer token with no
// profile behind it.
async function showAccount() {
  const who = $("account-who");
  if (!who) return;
  try {
    const me = await api("/me");
    if (me?.anonymous) {
      who.textContent = "Signed in as nobody — this instance has authentication disabled";
      return;
    }
    who.textContent = me?.login || me?.name || "Signed in";
    if (me?.name && me?.login) who.title = me.name;
  } catch (err) {
    if (err?.handled) return;
    // /me is absent on a deployment without browser sign-in. What that means
    // depends on how the caller got this far: with a token they are
    // authenticated and there is simply no profile behind it, and without one
    // the request would have been refused had authentication been on at all --
    // so reaching here unauthenticated means it is off. "Not signed in" would
    // read as a problem in a deployment where nothing is wrong.
    who.textContent = localStorage.getItem(TOKEN_KEY)
      ? "Signed in with a token"
      : "No account — this instance has authentication disabled";
  }
}

// slugify turns a typed name into the URL-safe slug the API accepts. The
// server validates independently; this only spares the user from having to
// know the rule.
const slugify = (name) => name.toLowerCase().trim()
  .replace(/[^a-z0-9]+/g, "-").replace(/^-+|-+$/g, "").slice(0, 63);

// wireBenchCreate binds a name field and a button to bench creation, used by
// both the first-run welcome and the "+ New bench" overlay.
function wireBenchCreate(nameID, buttonID, noteID, onDone) {
  const note = (msg) => {
    const n = $(noteID);
    if (n) { n.textContent = msg || ""; n.classList.toggle("error", Boolean(msg)); }
  };
  const submit = async () => {
    const name = $(nameID).value.trim();
    const slug = slugify(name);
    if (!slug) { note("give the bench a name"); return; }
    try {
      await api("/workspaces", { method: "POST", body: { slug, name } });
      toast(`Created ${name}`);
      onDone?.();
      // Land *in* the bench that was just created, not back in whichever one
      // was open before: creating a bench is a statement of where you want
      // to be. The stored slug is what boot() restores after the reload.
      localStorage.setItem(WORKSPACE_KEY, slug);
      // Reload rather than patch state: a first bench changes the whole
      // shell, and a fresh boot is simpler than reconciling it in place.
      // It lands on Get started, which is the next thing to do.
      location.hash = "#/overview";
      location.reload();
    } catch (err) {
      if (!err.handled) note(err.message);
    }
  };
  once($(buttonID), submit);
  $(nameID).addEventListener("keydown", (e) => { if (e.key === "Enter") submit(); });
  $(nameID).focus();
}

// openBenchCreator reuses the wizard overlay for a one-field form.
function openBenchCreator() {
  const body = $("wizard-body");
  let close = null;
  body.innerHTML = `
    <div class="wizard-head">
      <span class="wizard-steps">New bench</span>
      <button class="wizard-x" id="bench-x" aria-label="Close">&times;</button>
    </div>
    <p class="hint">A bench is one wiki and the sources it is compiled from.</p>
    <label class="hint" for="new-bench-name">Name</label>
    <input id="new-bench-name" placeholder="Team handbook" autocomplete="off">
    <div class="wizard-actions">
      <button class="btn" id="new-bench-create">Create bench</button>
    </div>
    <span class="hint" id="new-bench-note" role="status"></span>`;
  close = openOverlay($("wizard"), () => { body.innerHTML = ""; });
  $("bench-x").addEventListener("click", () => close?.());
  wireBenchCreate("new-bench-name", "new-bench-create", "new-bench-note", () => close?.());
}

async function boot() {
  $("menu").addEventListener("click", () =>
    setDrawer(!document.body.classList.contains("nav-open")));

  // Delegated so every view's "?" works without each one wiring its own, and
  // so a view that re-renders (the ingest poll) does not lose the binding.
  document.addEventListener("click", (e) => {
    const btn = e.target.closest?.("[data-help]");
    if (btn) openHelp(btn.dataset.help);

    // The footer menus are popups, so they close the way popups do: on a click
    // anywhere outside, and on choosing something inside. Opening one closes
    // the other -- two sheets over a narrow rail would overlap.
    const opened = e.target.closest?.(".footer-menu");
    for (const m of document.querySelectorAll(".footer-menu")) {
      if (!m.open) continue;
      if (m !== opened || e.target.closest(".footer-sheet a, .footer-sheet button")) {
        m.open = false;
      }
    }
  });
  document.addEventListener("keydown", (e) => {
    // Escape priority: palette (handled inside its own overlay) -> hover
    // preview -> drawer.
    if (e.key === "Escape") {
      const openMenu = document.querySelector(".footer-menu[open]");
      if (openMenu) { openMenu.open = false; openMenu.querySelector("summary")?.focus(); return; }
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
  // Navigation always dismisses a lingering card or pending timer, and any
  // suggestion list still open over the page being left.
  window.addEventListener("hashchange", () => { hidePreview(); closeSuggest(); });

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
    const { version, githubSignIn, sourcePollIntervalSeconds } = await api("/version");
    $("version").textContent = version;
    state.githubSignIn = Boolean(githubSignIn);
    state.sourcePollIntervalSeconds = Number(sourcePollIntervalSeconds) || 0;

    const workspaces = await api("/workspaces");
    if (!workspaces.length) {
      // Nothing is navigable without a bench: an empty picker, a page filter
      // over no pages, and nine views that would all render nothing. The
      // shell hides itself so the one thing worth doing is the only thing on
      // screen.
      document.body.classList.add("no-bench");
      // A dead end before this: a signed-in user with no bench was told to
      // run a CLI command on a machine they may not have.
      $("main").innerHTML = `<h1>Welcome</h1>
        <p class="hint">A bench is one wiki and the sources it is compiled from.
        Create one, then add a repository, web pages, or documents to it.</p>
        <label class="group-label" for="first-bench-name">Name</label>
        <input id="first-bench-name" placeholder="Team handbook" autocomplete="off">
        <button class="btn" id="first-bench-create">Create bench</button>
        <span class="hint" id="first-bench-note" role="status"></span>`;
      wireBenchCreate("first-bench-name", "first-bench-create", "first-bench-note");
      return;
    }
    state.benches = workspaces;
    $("bench-list").innerHTML = workspaces
      .map((w) => `<button type="button" data-slug="${esc(w.slug)}">${esc(w.name)} (${esc(w.pageCount)})</button>`)
      .join("") + `<button type="button" class="bench-new" id="bench-new">+ New bench</button>`;

    $("bench-list").addEventListener("click", (e) => {
      if (e.target.closest("#bench-new")) {
        $("bench").open = false;
        openBenchCreator();
        return;
      }
      const b = e.target.closest("button[data-slug]");
      if (!b) return;
      if (b.dataset.slug === state.workspace) { $("bench").open = false; return; }
      // A new workspace starts at its overview: carrying the previous page's
      // hash across would greet it with "page not found".
      location.hash = "#/overview";
      loadWorkspace(b.dataset.slug);
    });
    window.addEventListener("hashchange", route);
    wireSearchBox();
    // Who the account sheet is about. Asked once at boot rather than on every
    // open: it does not change while the page is loaded, and a menu that waits
    // on a request to say your own name reads as broken.
    showAccount();

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

    // The workspace choice survives a reload. A remembered slug that no longer
    // exists falls back to the first bench that actually has pages, rather
    // than the first alphabetically: landing on an empty bench shows the
    // first-run screen and its dead controls to someone whose instance is
    // fully built, which reads as "my wiki is gone".
    const stored = localStorage.getItem(WORKSPACE_KEY);
    const initial = workspaces.some((w) => w.slug === stored)
      ? stored
      : (workspaces.find((w) => w.pageCount > 0) ?? workspaces[0]).slug;
    await loadWorkspace(initial);
  } catch (err) {
    if (err.handled) return;
    $("main").innerHTML = banner(err, true);
    wireBannerRetry();
  }
}

boot();
