"use strict";
const $ = (id) => document.getElementById(id);
const state = {
  workspace: null, benches: [], pages: [], slugs: new Set(), view: "overview",
  // runs is the last feed the top bar read: it drives the run pill, the run
  // popover, and -- through the ordered list of distinct refs below -- how far
  // behind its sources each page has fallen.
  runs: [], runUnits: [], refs: [],
};

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

// ---- freshness --------------------------------------------------------------
// A page records the source revision it was written from (built_at_ref, on the
// page summary). The bench's own position is the ref of its newest run. The
// distance between the two is how far the page has fallen behind, counted in
// BUILDS rather than commits: builds are what the run feed actually knows, and
// a number invented from timestamps would be worse than an honest coarse one.
//
// Every state carries a word as well as a colour -- the reader's chip says
// "Fired · current with sources", not just an ember dot.
const FRESH_UNKNOWN = { key: "unknown", word: "Freshness unknown", why: "" };

function freshnessOf(page) {
  const ref = page?.builtAtRef;
  if (!ref || !state.refs.length) return FRESH_UNKNOWN;
  const behind = state.refs.indexOf(ref);
  if (behind === 0) return { key: "fired", word: "Fired", why: "current with sources" };
  // A ref this feed has never seen is older than the ten runs we asked for.
  if (behind < 0) {
    return { key: "stale", word: "Stale",
             why: page.updated
               ? `written ${relTime(page.updated)}, before the recent builds`
               : "written before the recent builds" };
  }
  return { key: "cooling", word: "Cooling",
           why: `${behind} build${behind === 1 ? "" : "s"} behind` };
}

// freshDot is the 5px mark the tree, the index and the overview all use. The
// title carries the word, so the colour never stands alone.
function freshDot(page, size = "") {
  const f = freshnessOf(page);
  const label = f.why ? `${f.word} — ${f.why}` : f.word;
  return `<span class="dot f-${f.key}${size}" title="${esc(label)}" aria-hidden="true"></span>`;
}

// typeDot colours by page type instead: the graph, the palette and the graph
// rail all name a type the same way.
const typeDot = (type, size = " dot-6") =>
  `<span class="dot${size} t-${esc(type || "concept")}" aria-hidden="true"></span>`;

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
  $("main").classList.remove("bleed");
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

// autoHeadingOffset picks the shift that makes a body's shallowest heading an
// <h2>. The chrome renders the page's own <h1>, so a page whose sections are
// all "##" would open at <h3> and skip a level -- WCAG 1.3.1 asks for an
// unbroken outline, and a fixed offset cannot give one across pages that
// disagree about where their headings start. Relative nesting is preserved:
// only the whole ladder moves.
function autoHeadingOffset(src) {
  let min = 7;
  for (const m of String(src ?? "").matchAll(/^(#{1,6})\s+/gm)) {
    min = Math.min(min, m[1].length);
  }
  return min === 7 ? 1 : 2 - min;
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
  return `<a href="#/page/${encodeURIComponent(p.slug)}"${current}>${freshDot(p)}<span>${label}</span></a>`;
}

function treeGroup(type, label, links, collapsed) {
  return `<details class="tree-group" data-type="${esc(type)}"${collapsed ? "" : " open"}>
    <summary class="group-label">${esc(label)}<span class="count">${links.length}</span></summary>
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
  // Document-wide, not .nav-only: Members and Agent access live in the avatar
  // menu in the top bar and would otherwise never mark themselves.
  for (const a of document.querySelectorAll("a[data-view]")) {
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
  // The label only -- the entry also carries a glyph, which textContent on the
  // anchor itself would delete.
  const overview = document.querySelector('.nav a[data-view="overview"] span');
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

// sizeBars applies the percentage widths written as data-w. A bar's width is
// data, but the CSP (style-src 'self') refuses a style attribute, so it is set
// through the CSSOM after insertion rather than baked into the markup.
function sizeBars(root = document) {
  for (const el of root.querySelectorAll("[data-w]")) el.style.width = `${el.dataset.w}%`;
}

// beginView starts a navigation: bumps the token, resets scroll and focus
// (the SPA equivalent of a page load), and shows a delayed loading state so
// slow fetches don't read as dead clicks while fast ones don't flash.
let lastViewKey = null;
function beginView(title, view, activeSlug) {
  // Tear down the departing view's observers and timers BEFORE the token
  // moves: nothing stale may ever touch the incoming view's DOM.
  runViewCleanups();
  const my = ++nav;
  setTitle(title);
  setNav(view);
  renderTree(activeSlug ?? null);
  document.body.classList.remove("nav-open", "inbox-detail-open");
  $("menu").setAttribute("aria-expanded", "false");
  // Navigating dismisses the run popover: it is anchored to chrome, not to the
  // view, and would otherwise hang over whatever came next. Keyed on which view
  // this is, not on the fact that beginView ran: several views re-render
  // themselves on a timer while a build moves, and closing the popover on a
  // refresh would snatch it shut every five seconds -- while a build moves is
  // exactly when someone has it open.
  const key = `${view || "page"}:${activeSlug ?? ""}`;
  if (key !== lastViewKey) { closeRunPop(); lastViewKey = key; }
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
      // Three views run a full-bleed layout that supplies its own padding, and
      // the same three also render ordinary content -- an empty graph, a page
      // that failed to load, a search result list. Which one just happened is
      // a property of the markup, not of the view's name, so it is read back
      // off the markup rather than assumed from the route.
      m.classList.toggle("bleed",
        Boolean(m.querySelector(":scope > .reader, :scope > .inbox, :scope > .graph-view")));
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
  const box = $("toc-box"), rows = $("toc-rows");
  if (!box || !rows) return;
  const headings = [...document.querySelectorAll("#main .prose h2, #main .prose h3")];
  // Under three headings the contents list is a restatement of the page.
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

  for (const h of headings) {
    const btn = document.createElement("button");
    btn.className = "toc-link";
    btn.textContent = h.textContent;
    btn.addEventListener("click", () => {
      h.scrollIntoView({ behavior: scrollBehavior(), block: "start" });
      h.focus({ preventScroll: true });
    });
    rows.append(btn);
  }
  box.hidden = false;

  // Scrollspy: highlight the section currently in the top third of the
  // viewport. Disconnected via onViewCleanup so a stale observer can never
  // touch the next view.
  const buttons = [...rows.querySelectorAll(".toc-link")];
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

// ---- the top bar ------------------------------------------------------------
// Build state is the one piece of state that changes while you are somewhere
// else in the app, so it lives in chrome that persists rather than on the
// Ingest view alone. The pill says whether the bench is firing; the popover
// behind it says what it is firing and what it has spent.
//
// Named apart from sources.js's kilnFiring: both files are classic scripts
// sharing one global scope, where a repeated top-level binding is a SyntaxError
// that takes down the whole UI.
const kilnMark = `<svg class="kiln-flame" viewBox="0 0 24 24" aria-hidden="true">
    <path class="kiln-shell" fill-rule="evenodd" d="M3 22 L3 12 Q3 2 12 2 Q21 2 21 12 L21 22 Z
      M8 22 L8 15 Q8 10 12 10 Q16 10 16 15 L16 22 Z"/>
    <circle class="kiln-glow" cx="12" cy="18.5" r="4.6"/>
    <circle class="kiln-ember" cx="12" cy="18.5" r="2.2"/>
  </svg>`;

const activeRun = () => state.runs.find((r) => r.status === "running")
  || state.runs.find((r) => r.status === "queued") || null;

// renderRunPill draws the bar's rightmost control from whatever the last feed
// said. Both states name themselves in words; the ember only reinforces.
function renderRunPill() {
  const pill = $("run-pill");
  if (!pill) return;
  const live = state.runs.find((r) => r.status === "running");
  const queued = state.runs.find((r) => r.status === "queued");
  $("ingest-live").hidden = !(live || queued);

  if (live) {
    const done = live.unitsDone || 0, total = live.unitsTotal || 0;
    const pct = total ? Math.round((done / total) * 100) : 0;
    const count = total ? ` · ${done}/${total}` : "";
    pill.className = "run-pill";
    pill.hidden = false;
    pill.title = total ? `Firing — ${done} of ${total} units done` : "Firing";
    pill.setAttribute("aria-label", pill.title);
    pill.innerHTML = `${kilnMark}<span class="run-pill-text">Firing${esc(count)}</span>
      ${total ? `<span class="run-track"><span class="run-pill-fill"></span></span>` : ""}`;
    // CSSOM, not a style attribute: the CSP (style-src 'self') refuses those.
    const fill = pill.querySelector(".run-pill-fill");
    if (fill) fill.style.width = `${pct}%`;
    return;
  }

  // Idle, the pill names what opening it shows: the last build. It used to
  // recite the next-build sentence, which is a good sentence about the
  // schedule and a bad label for a button whose popover is about a run that
  // already happened. The schedule moved into the popover, where it sits
  // beside the run it is the sequel to.
  const last = state.runs[0];
  const label = queued ? "Build queued"
    : last ? `Last build · ${relTime(last.created)}`
    : "No builds yet";
  pill.className = `run-pill${queued ? "" : " idle"}`;
  pill.hidden = false;
  pill.title = `${label} — open build detail`;
  pill.setAttribute("aria-label", pill.title);
  pill.innerHTML = `<span class="run-pill-text">${esc(label)}</span>`;
}

let runPopOpen = false;
function closeRunPop() {
  if (!runPopOpen) return false;
  runPopOpen = false;
  $("run-pop").hidden = true;
  $("run-pill")?.setAttribute("aria-expanded", "false");
  return true;
}

// renderRunPop lists the units of the run in flight. Everything here comes from
// the same /runs and /runs/{id}/items calls the Ingest feed already makes.
function renderRunPop() {
  const box = $("run-pop");
  const run = activeRun() || state.runs[0];
  // When the next build comes is the natural sequel to what the last one did,
  // so the schedule sentence lives here rather than on the pill -- there is
  // room for a sentence in a 360px panel and none on a chip.
  const next = nextBuildLine(state.runs, state.connectors || [],
    state.sourcePollIntervalSeconds || 0);
  if (!run) {
    box.innerHTML = `<div class="empty">No builds yet.</div>
      <div class="run-pop-next">${esc(next)}</div>
      <div class="run-pop-foot"><span></span><a href="#/ingest">Open Ingest →</a></div>`;
    return;
  }
  const total = run.unitsTotal || 0, done = run.unitsDone || 0;
  const pct = total ? Math.round((done / total) * 100) : 0;
  const items = run.status === "running" ? state.runUnits : [];
  const dotFor = (s) => s === "running" ? "u-running"
    : s === "failed" ? "u-failed"
    : (s === "pending" || s === "deferred") ? "" : "u-done";
  const shown = items.filter((it) => it.status !== "pending").slice(0, 6);
  const queued = items.length - shown.length;

  box.innerHTML = `
    <div class="run-pop-head">
      <strong>Run ${esc((run.id || "").slice(0, 7))}</strong>
      <span class="mono">${esc(run.trigger || "manual")}${run.ref ? ` · ${esc(run.ref)}` : ""}</span>
    </div>
    ${total ? `<div class="run-pop-bar"><div class="run-pop-fill"></div></div>` : ""}
    <div class="run-pop-units">
      ${shown.map((it) => {
        const s = unitSource(it.key);
        const right = it.status === "running" ? "writing…"
          : it.status === "failed" ? "failed"
          : it.costUsd > 0 ? `$${Number(it.costUsd).toFixed(2)}` : "done";
        return `<div class="run-pop-unit">
          <span class="unit-dot ${dotFor(it.status)}" aria-hidden="true"></span>
          <span class="mono">${esc(s.name || it.key)}</span>
          <span class="when">${esc(right)}</span>
        </div>`;
      }).join("")}
      ${queued > 0 ? `<div class="run-pop-unit">
        <span class="unit-dot" aria-hidden="true"></span>
        <span class="mono">${queued} queued</span></div>` : ""}
      ${!shown.length && !queued ? `<div class="run-pop-unit">
        <span class="unit-dot ${dotFor(run.status)}" aria-hidden="true"></span>
        <span class="mono">${esc(runOutcome(run))}</span></div>` : ""}
    </div>
    <div class="run-pop-next">${esc(next)}</div>
    <div class="run-pop-foot">
      <span>${(() => {
        // A run's own costUsd is written when it ends, so mid-flight it reads
        // zero while the units beneath it are already reporting spend. Sum the
        // units while it moves, and read the settled figure once it stops.
        const spent = run.status === "running"
          ? items.reduce((n, it) => n + (Number(it.costUsd) || 0), 0)
          : Number(run.costUsd) || 0;
        if (spent <= 0) return "No cost yet";
        return `$${spent.toFixed(2)}${run.status === "running" ? " so far" : " spent"}`;
      })()}</span>
      <a href="#/ingest">Open Ingest →</a>
    </div>`;
  const fill = box.querySelector(".run-pop-fill");
  if (fill) fill.style.width = `${pct}%`;
}

function toggleRunPop() {
  if (closeRunPop()) return;
  runPopOpen = true;
  renderRunPop();
  $("run-pop").hidden = false;
  $("run-pill").setAttribute("aria-expanded", "true");
}

// adoptRunFeed publishes a feed the top bar can draw from. It also republishes
// state.refs, which is what every freshness dot on screen is measured against,
// so a build landing re-dates the tree without a reload.
//
// Separate from the fetch so the Ingest view's own 5s poll can hand over what
// it just read: that poll and this one want the same two calls, and making
// them twice per tick would double the traffic of a page whose whole job is to
// be watched while a build runs.
function adoptRunFeed(runs, activeUnits) {
  state.runs = runs;
  // Newest first, deduplicated: the position of a page's ref in this list is
  // how many builds it is behind.
  state.refs = [...new Set(runs.map((r) => r.ref).filter(Boolean))];
  state.runUnits = activeUnits;
  renderRunPill();
  if (runPopOpen) renderRunPop();
}

async function refreshRunState() {
  const ws = state.workspace;
  try {
    const { runs, activeUnits } = await fetchRunFeed(encodeURIComponent(ws));
    if (ws !== state.workspace) return; // raced a bench switch
    adoptRunFeed(runs, activeUnits);
  } catch {
    // The run routes are unmounted on a read-only deployment. That is not an
    // error to paint: the pill simply has nothing to say.
    if (ws === state.workspace) adoptRunFeed([], []);
  }
}

// The pill polls only while something is moving; a settled bench costs nothing.
let runPollTimer = null;
function armRunPoll() {
  clearTimeout(runPollTimer);
  if (!activeRun()) return;
  runPollTimer = setTimeout(async () => {
    await refreshRunState();
    armRunPoll();
  }, 5000);
}

// ---- live content -----------------------------------------------------------
let pollTimer = null, knownRevision = null, revToast = null;

function updateReviewsBadge(count) {
  const b = $("reviews-badge");
  b.hidden = count <= 0;
  b.textContent = count > 0 ? String(count) : "";
  b.setAttribute("aria-label", `${count} open review${count === 1 ? "" : "s"}`);
}

// The rail's Gaps entry carries what it would cost you to look: a bench with
// no gaps says nothing rather than "0".
async function refreshGapsCount() {
  const el = $("gaps-count");
  if (!el) return;
  try {
    const gaps = await api(`/workspaces/${encodeURIComponent(state.workspace)}/gaps`);
    el.hidden = !gaps.length;
    el.textContent = String(gaps.length);
  } catch { el.hidden = true; /* decoration; the next tick retries */ }
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
  refreshGapsCount();
  // A build can start from a webhook, the CLI, or another tab. The 5s run poll
  // only runs while this tab already knows one is live, so the slow tick is
  // what notices a build that began while nobody was looking.
  if (!activeRun()) refreshRunState().then(armRunPoll);
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

// deckOf is the standfirst: one sentence saying what the page is. Pages carry
// no summary field, so it is lifted from the prose -- the first sentence of a
// generated page is written to be exactly this. Nothing is invented: if the
// body opens with something too long to be a sentence, there is no deck.
function deckOf(body) {
  const flat = plainText(body || "");
  const end = flat.search(/[.!?](\s|$)/);
  if (end < 0) return "";
  const first = flat.slice(0, end + 1).trim();
  return first.length >= 20 && first.length <= 220 ? first : "";
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

    const fresh = freshnessOf(p);
    const deck = deckOf(p.body);
    const sources = p.sources || [];

    if (!view.done(`<div class="reader">
      <article>
        <div class="crumb">
          <span class="mono">${esc(p.type)}</span><span>/</span><span>${esc(p.title || p.slug)}</span>
        </div>
        <h1>${esc(p.title || p.slug)}</h1>
        ${deck ? `<p class="deck">${esc(deck)}</p>` : ""}
        <div class="meta">
          ${fresh === FRESH_UNKNOWN ? "" : `<span class="fresh-chip f-${fresh.key}">
            <span class="dot dot-6 f-${fresh.key}" aria-hidden="true"></span>${esc(fresh.word)}${fresh.why ? ` · ${esc(fresh.why)}` : ""}</span>`}
          ${(p.tags || []).map((t) => `<span class="chip">${esc(t)}</span>`).join("")}
          ${p.updated ? `<span class="meta-when">written ${timeTag(p.updated)}</span>` : ""}
        </div>
        <div class="prose">${renderMarkdown(p.body, autoHeadingOffset(p.body))}</div>
        ${pagerFor(p)}
      </article>
      <div class="context-rail">
        <div id="toc-box" hidden>
          <div class="group-label">Contents</div>
          <div class="rail-rows" id="toc-rows"></div>
        </div>
        ${p.builtAtRef || sources.length ? `<div>
          <div class="group-label">Provenance</div>
          <div class="prov">
            ${p.builtAtRef ? `<div class="prov-row"><span>Written from</span><span class="mono">${esc(p.builtAtRef)}</span></div>` : ""}
            ${sources.length ? `<div class="prov-row"><span>Sources</span><span>${sources.length} file${sources.length === 1 ? "" : "s"}</span></div>` : ""}
            ${sources.length ? `<div class="prov-files">${sources.map((s) =>
              `<span>${esc(s)}</span>`).join("")}</div>` : ""}
          </div>
        </div>` : ""}
        ${backlinks === null ? "" : `<div>
          <div class="group-label">Linked from · ${backlinks.length}</div>
          ${backlinks.length
            ? `<div class="rail-rows">${backlinks.map((b) =>
                `<a href="#/page/${encodeURIComponent(b.slug)}">${esc(b.title || b.slug)}</a>`).join("")}</div>`
            : `<div class="hint">No pages link here yet.</div>`}
        </div>`}
        ${correctionsPanel(corrections)}
      </div>
    </div>`)) return;

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
    <div class="correction ${c.active ? "" : "inactive"}">${esc(c.body)}<div class="tools">${
      c.created ? `pinned ${esc(relTime(c.created))} · ` : ""}<button class="linkish" data-correction="${esc(c.id)}" data-active="${!c.active}">${
      c.active ? "deactivate" : "reactivate"}</button></div>
    </div>`).join("");
  return `
    <div>
      <div class="group-label">Corrections · ${corrections.length}</div>
      ${items}
      <button class="pin-open" id="correction-open">+ Pin a correction</button>
      <div class="pin-form" id="correction-form" hidden>
        <label class="sr-only" for="correction-body">New correction</label>
        <textarea id="correction-body" rows="3"
          placeholder="What should the wiki know about this page?"></textarea>
        <button class="btn" id="correction-pin">Pin correction</button>
        <p class="hint">Pages are never hand-edited — a rebuild would clobber the
          edit — so a correction is how you teach the wiki something it got wrong.
          It applies from the next rebuild.</p>
        <span class="hint" id="correction-note" role="status"></span>
      </div>
    </div>`;
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
  // The form expands in place rather than standing open: on a page with no
  // correction to make, an empty textarea in the rail is furniture.
  const open = $("correction-open");
  if (open) open.addEventListener("click", () => {
    open.hidden = true;
    $("correction-form").hidden = false;
    $("correction-body").focus();
  });
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

// ---- overview ---------------------------------------------------------------
// The overview is a generated artifact (markdown rendered from frontmatter), and
// it stays one: the stat strip and the freshness bars are computed here from the
// page summaries the rail has already loaded, and the artifact's prose is
// rendered below them.
async function showOverview() {
  const view = beginView("Overview", "overview");
  const ws = encodeURIComponent(state.workspace);
  try {
    const [artifact, open] = await Promise.all([
      api(`/workspaces/${ws}/overview`).catch(() => ({ body: "" })),
      api(`/workspaces/${ws}/reviews?status=open`).catch(() => null),
    ]);
    if (!view.current()) return;

    const bench = state.benches.find((b) => b.slug === state.workspace);
    const total = state.pages.length;
    const fresh = state.pages.filter((p) => freshnessOf(p).key === "fired").length;
    const cooling = state.pages.filter((p) => freshnessOf(p).key === "cooling").length;
    const known = Boolean(state.refs.length);

    // Clusters are the page types: the grouping the tree, the index and the
    // graph legend already use, so a reader meets one taxonomy, not two.
    const byType = {};
    for (const p of state.pages) (byType[p.type] ||= []).push(p);
    const clusters = TYPE_ORDER.filter((t) => byType[t]).map((t) => {
      const group = byType[t];
      const f = group.filter((p) => freshnessOf(p).key === "fired").length;
      const c = group.filter((p) => freshnessOf(p).key === "cooling").length;
      return { t, n: group.length, f, c };
    });

    // Newest first. `updated` is a calendar day, so ties are common; the title
    // order breaks them the same way the tree does.
    const recent = state.pages.filter((p) => p.updated).slice()
      .sort((a, b) => (b.updated || "").localeCompare(a.updated || "") || byTitle(a, b))
      .slice(0, 6);

    const stat = (label, value, note) => `<div class="stat">
      <div class="stat-label">${esc(label)}</div>
      <div class="stat-value">${esc(value)}</div>
      <div class="stat-note">${esc(note)}</div>
    </div>`;

    if (!view.done(`
      <h1>${esc(bench?.name || state.workspace)}</h1>
      <p class="overview-deck">A compiled wiki, kept current as its sources change.</p>
      <div class="stats ${known ? "n3" : "n2"}">
        ${stat("Pages", String(total), `across ${clusters.length} type${clusters.length === 1 ? "" : "s"}`)}
        ${known ? stat("Fresh", `${total ? Math.round((fresh / total) * 100) : 0}%`,
          `${fresh} written from the current build`) : ""}
        ${stat("Open questions", open === null ? "—" : String(open.length),
          open === null ? "reviews unavailable" : "waiting for a decision in Reviews")}
      </div>

      ${known && clusters.length ? `
        <div class="sec-head"><div class="group-label">Freshness by cluster</div></div>
        <div class="card pad cluster-rows">
          ${clusters.map((c) => `<div class="cluster">
            <span class="cluster-name">${esc(TYPE_LABELS[c.t] || c.t)}</span>
            <div class="bar">
              <span class="fill-ember" data-w="${Math.round(c.f / c.n * 100)}"></span>
              <span class="fill-soft" data-w="${Math.round(c.c / c.n * 100)}"></span>
            </div>
            <span class="cluster-note">${c.c ? `${c.c} page${c.c === 1 ? "" : "s"} cooling`
              : `all ${c.n} current`}</span>
          </div>`).join("")}
        </div>
        <p class="hint">${cooling} page${cooling === 1 ? "" : "s"} across the bench
          ${cooling === 1 ? "is" : "are"} behind the newest build.</p>` : ""}

      ${recent.length ? `
        <div class="sec-head"><div class="group-label">Recently rewritten</div></div>
        <div class="recent-rows">
          ${recent.map((p) => `<a class="recent-row" href="#/page/${encodeURIComponent(p.slug)}">
            ${freshDot(p, " dot-6")}
            <strong>${esc(p.title || p.slug)}</strong>
            <span class="why">${p.builtAtRef ? `written from ${esc(p.builtAtRef)}` : esc(p.type)}</span>
            <span class="when">${esc(relTime(p.updated))}</span>
          </a>`).join("")}
        </div>` : ""}

      ${artifact.body?.trim()
        // The artifact opens with its own "# Overview", which would be a second
        // <h1> under the bench name above. Shifted down one, it becomes the
        // heading of the generated prose section, which is what it now is.
        ? `<div class="prose">${renderMarkdown(artifact.body, 1)}</div>` : ""}`)) return;
    sizeBars($("main"));
  } catch (err) {
    if (!err.handled) view.done(banner(err));
  }
}

// showIndex is the whole corpus grouped by type. The dot is freshness, and the
// sub-line says so -- the colour is never the only thing carrying it.
async function showIndex() {
  const view = beginView("Index", "index");
  const byType = {};
  for (const p of state.pages) (byType[p.type] ||= []).push(p);
  const types = TYPE_ORDER.filter((t) => byType[t]);
  if (!types.length) {
    view.done(`<h1>Index</h1><div class="empty">No pages yet — the index is
      written by the first ingest. <a href="#/overview">Start here</a>.</div>`);
    return;
  }
  view.done(`<h1>Index</h1>
    <p class="view-deck">Every page, grouped by type. The dot is freshness — hover
      one for the word.</p>
    <div class="type-cards">
      ${types.map((t) => `<div class="type-card">
        <div class="type-card-head">
          <div class="group-label">${esc(TYPE_LABELS[t] || t)}</div>
          <span class="count">${byType[t].length}</span>
        </div>
        ${[...byType[t]].sort(byTitle).map((p) =>
          `<a href="#/page/${encodeURIComponent(p.slug)}">${freshDot(p)}<span>${esc(p.title || p.slug)}</span></a>`).join("")}
      </div>`).join("")}
    </div>`);
}

// ---- the build log ----------------------------------------------------------
// The durable, page-level record of what happened to this wiki and when. It is
// deliberately NOT the Ingest run timeline: that view is the current state of
// the machine, this one is the history, and both are worth having.
//
// kiln generates a `log` artifact on every run, but it is markdown-only -- a
// heading and three bullets per entry, with no page list, no unit counts and no
// prose to render. So the entries are read from the run records the Ingest feed
// already uses, which is the fallback the design names. The artifact itself is
// untouched and still served to agents over MCP.
let logFilter = "all";

// Titles are the log's own vocabulary, not the timeline's: a line in a history
// says what a build DID ("Imported 6 pages"), where a live feed says what is
// happening to it now ("Ingesting — 4 of 9 units done").
function logTitle(r) {
  if (r.status === "running") return "Firing";
  if (r.status === "queued") return "Queued";
  if (r.status === "failed") return "Failed";
  if (r.status === "over_budget") return "Stopped at the budget cap";
  const n = (r.pagesCreated || 0) + (r.pagesUpdated || 0) + (r.pagesDeleted || 0);
  if (!n) return "Nothing changed";
  return `Imported ${n} page${n === 1 ? "" : "s"}`;
}

// One or two sentences in kiln's voice. Every clause is read off the run row --
// nothing here is invented, and a build that changed nothing gets the sentence
// that says so is the normal, successful outcome rather than an empty state.
function logSummary(r) {
  if (r.status === "running") {
    const total = r.unitsTotal || 0, done = r.unitsDone || 0;
    if (!total) return "Planning what to rebuild.";
    return `${total} unit${total === 1 ? "" : "s"} planned; ${done} written so far.`;
  }
  if (r.status === "queued") return "Waiting for a worker to claim it.";
  if (r.status === "failed") return r.error ? String(r.error).split("\n")[0] : "The build did not finish.";
  if (r.status === "over_budget") return "The run reached its budget cap and stopped before finishing.";
  const parts = [];
  if (r.pagesCreated) parts.push(`${r.pagesCreated} created`);
  if (r.pagesUpdated) parts.push(`${r.pagesUpdated} updated`);
  if (r.pagesDeleted) parts.push(`${r.pagesDeleted} removed`);
  if (!parts.length) return "Every unit matched its cached content hash. No model calls were made.";
  const n = (r.pagesCreated || 0) + (r.pagesUpdated || 0) + (r.pagesDeleted || 0);
  return `${n === 1 ? "One page" : `${n} pages`} changed: ${parts.join(", ")}.`;
}

// The footer's three facts. Units read "N planned · M written" -- how many the
// content-hash gate SPARED is the number this line wants and the run row does
// not carry, so it says what was planned rather than implying a total.
function logFooter(r) {
  const out = [];
  if (r.unitsTotal) {
    out.push(`${r.unitsTotal} planned · ${r.unitsDone || 0} written`);
  }
  if (r.tokens > 0) out.push(humanTokens(r.tokens));
  if (r.costUsd > 0) out.push(money(r.costUsd) + (r.status === "running" ? " so far" : ""));
  else if (r.status !== "running" && r.status !== "queued") out.push("$0.00");
  return out;
}

async function showLog() {
  const view = beginView("Log", "log");
  try {
    // Deeper than the top bar's ten: this is the record, not the dashboard.
    const runs = await api(`/workspaces/${encodeURIComponent(state.workspace)}/runs?limit=50`);
    if (!view.current()) return;

    const changed = (r) => (r.pagesCreated || 0) + (r.pagesUpdated || 0) + (r.pagesDeleted || 0) > 0;
    const failed = (r) => r.status === "failed" || r.status === "over_budget";
    const shown = logFilter === "changed" ? runs.filter(changed)
      : logFilter === "failures" ? runs.filter(failed)
      : runs;

    // Runs group under the calendar day they started, newest first, in the same
    // words relTime() uses everywhere else.
    const days = [];
    for (const r of shown) {
      const label = relTime(r.created);
      if (!days.length || days[days.length - 1].label !== label) {
        days.push({ label, iso: r.created, runs: [] });
      }
      days[days.length - 1].runs.push(r);
    }

    const pill = (key, label) =>
      `<button class="pill" data-log-filter="${key}" aria-pressed="${logFilter === key}">${label}</button>`;

    view.done(`
      <div class="head-row">
        <h1>Log</h1>
        <div class="pills head-actions" role="group" aria-label="Filter the log">
          ${pill("all", "All")}${pill("changed", "Changed pages")}${pill("failures", "Failures")}
        </div>
      </div>
      <p class="view-deck">Written by kiln on every run, never by the agent.
        What each build read, wrote, gated and spent.</p>
      ${days.length ? days.map((d) => {
        const spent = d.runs.reduce((n, r) => n + (Number(r.costUsd) || 0), 0);
        return `<section class="log-day">
          <div class="log-day-head">
            <!-- The DATE is the <time>, not the total: the poll re-renders
                 every time[datetime] from relTime() so "today" becomes
                 "yesterday" without a reload, and a total living in one would
                 be overwritten with a date on the first tick. -->
            <time class="group-label" datetime="${esc(d.iso)}" title="${esc(d.iso)}">${esc(d.label)}</time>
            <span class="log-rule"></span>
            <span class="log-day-total">${d.runs.length} run${
              d.runs.length === 1 ? "" : "s"} · ${money(spent)}</span>
          </div>
          ${d.runs.map((r) => `<article class="log-entry">
            <div class="log-entry-head">
              <span class="tl-dot run-${esc(r.status)}" aria-hidden="true"></span>
              <strong>${esc(logTitle(r))}</strong>
              ${r.ref ? `<span class="mono">${esc(r.ref)}</span>` : ""}
              ${r.trigger ? `<span class="chip">${esc(r.trigger)}</span>` : ""}
              <span class="log-when">${r.status === "running" ? "in progress" : esc(relTime(r.created))}</span>
            </div>
            <p class="log-summary">${esc(logSummary(r))}</p>
            ${(() => {
              const f = logFooter(r);
              return f.length ? `<div class="log-foot">${f.map((x) => `<span>${esc(x)}</span>`).join("")}</div>` : "";
            })()}
          </article>`).join("")}
        </section>`;
      }).join("")
      : `<div class="empty">${logFilter === "all"
          ? `Nothing logged yet — kiln writes an entry on every run.
             <a href="#/ingest">Ingest to start one</a>.`
          : "No runs match this filter."}</div>`}`);

    for (const b of document.querySelectorAll("[data-log-filter]")) {
      b.addEventListener("click", () => {
        if (logFilter === b.dataset.logFilter) return;
        logFilter = b.dataset.logFilter;
        showLog();
      });
    }
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
    // Demand is scaled against the most-wanted gap, so the bars compare with
    // each other rather than with an invented ceiling.
    const peak = Math.max(...gaps.map((g) => Number(g.wantedBy) || 0), 1);
    if (!view.done(`${viewHead("Gaps", "gaps")}
      <p class="view-deck">Pages existing content links to but that have never been
        written — the wiki's own account of what it is missing.</p>
      <div id="gap-note" class="hint" role="status"></div>
      <div class="card rows">
        ${gaps.map((g) => `<div class="gap-row">
          <span class="slug">${esc(g.slug)}</span>
          <div class="bar"><span class="fill-soft" data-w="${Math.round((Number(g.wantedBy) || 0) / peak * 100)}"></span></div>
          <span class="count">wanted by ${esc(g.wantedBy)} page${g.wantedBy === 1 ? "" : "s"}</span>
          <button class="btn quiet gap-ask" data-gap="${esc(g.slug)}">Ask for it</button>
        </div>`).join("")}
      </div>`)) return;
    sizeBars($("main"));

    // Asking files a gap review, so the next plan covers the page. The server
    // deduplicates open items on kind and title -- asking twice asks once --
    // and the button says so rather than pretending each click did something.
    // Not once(): that helper hands the button back on the way out, and this
    // one must stay spent -- the answer to "ask again?" is that you already did.
    for (const b of document.querySelectorAll("[data-gap]")) {
      b.addEventListener("click", async () => {
        if (b.disabled) return;
        b.disabled = true;
        try {
          await api(`/workspaces/${encodeURIComponent(state.workspace)}/gaps/${encodeURIComponent(b.dataset.gap)}/request`,
            { method: "POST", body: {} });
          b.textContent = "Asked";
          b.classList.add("asked");
          toast("Asked — filed as a question in Reviews");
          refreshReviewsBadge();
        } catch (err) {
          b.disabled = false;
          if (err.handled) return;
          const n = $("gap-note");
          if (n) { n.textContent = err.message; n.classList.add("error"); }
        }
      });
    }
  } catch (err) {
    if (!err.handled) view.done(banner(err));
  }
}

// ---- review icons -----------------------------------------------------------
// A review list repeats the same handful of words down the page: what is being
// asked about. As a glyph in a tinted square that column is scannable at a
// glance; as text it was a column of near-identical chips to be read one by
// one. Drawn in currentColor so the kind palette applies, aria-hidden because
// the kind's name rides alongside in text -- replaced on screen, never actually
// removed. Names are distinct from sources.js's: both files are classic scripts
// sharing one global scope, where a repeated top-level const is a SyntaxError
// that would take down the whole UI.
const iconSave = `<svg class="icon" viewBox="0 0 16 16" aria-hidden="true"><path fill-rule="evenodd" d="M2 2h9.2L14 4.8V14H2V2zM5.2 3h4.4v3.2H5.2zM4 9h8v4H4z"/></svg>`;
const iconDeletion = `<svg class="icon" viewBox="0 0 16 16" aria-hidden="true"><path fill-rule="evenodd" d="M6 1h4v1h4v2H2V2h4V1zM3 5h10l-.8 10H3.8L3 5zm3 2v6h1V7H6zm3 0v6h1V7H9z"/></svg>`;
const iconContradiction = `<svg class="icon" viewBox="0 0 16 16" aria-hidden="true"><path d="M2.6 5.6h10.8v1.8H2.6zM2.6 9h10.8v1.8H2.6zM10.4 1.4l1.7.9-6.5 12.4-1.7-.9z"/></svg>`;
const iconUncertain = `<svg class="icon" viewBox="0 0 16 16" aria-hidden="true"><path d="M8 1.6c-2.3 0-4 1.5-4.2 3.6l2 .2C5.9 4.2 6.8 3.5 8 3.5c1.2 0 2 .6 2 1.5 0 .7-.4 1.2-1.3 1.9-1.1.9-1.7 1.6-1.7 2.9v.6h2v-.5c0-.8.3-1.2 1.2-1.9C11.4 7.1 12 6.2 12 5c0-2-1.6-3.4-4-3.4zM6.9 12.1h2.2v2.3H6.9z"/></svg>`;
const iconGap = `<svg class="icon" viewBox="0 0 16 16" aria-hidden="true"><path d="M2 2h4.4v1.8H3.8v2.6H2V2zM9.6 2H14v4.4h-1.8V3.8H9.6V2zM2 9.6h1.8v2.6h2.6V14H2V9.6zM12.2 9.6H14V14H9.6v-1.8h2.6V9.6z"/></svg>`;
const iconBudget = `<svg class="icon" viewBox="0 0 16 16" aria-hidden="true"><path fill-rule="evenodd" d="M1 3h14v10H1V3zm1.8 1.8v6.4h10.4V4.8H2.8z"/><path d="M8 5.9a2.1 2.1 0 1 1 0 4.2 2.1 2.1 0 0 1 0-4.2z"/></svg>`;
const iconStorage = `<svg class="icon" viewBox="0 0 16 16" aria-hidden="true"><path d="M8 1.2c3.3 0 5.8 1 5.8 2.2S11.3 5.6 8 5.6 2.2 4.6 2.2 3.4 4.7 1.2 8 1.2zM2.2 5.4c1.3.9 3.4 1.4 5.8 1.4s4.5-.5 5.8-1.4v2.4c0 1.2-2.5 2.2-5.8 2.2s-5.8-1-5.8-2.2V5.4zM2.2 9.6c1.3.9 3.4 1.4 5.8 1.4s4.5-.5 5.8-1.4V12c0 1.2-2.5 2.2-5.8 2.2S2.2 13.2 2.2 12V9.6z"/></svg>`;

const REVIEW_KIND_ICONS = {
  deletion: iconDeletion, contradiction: iconContradiction, uncertain: iconUncertain,
  gap: iconGap, budget: iconBudget, storage: iconStorage,
};

// iconChip swaps a word for its glyph and keeps the word as the tooltip and the
// accessible name. `kind` is free-form text chosen by whatever filed the review,
// so anything unmapped falls back to the word it always was -- a chip with no
// glyph and no label would be a decision nobody can read. `extra` carries a
// palette class (the kind-* colours) for callers that have one.
const iconChip = (icons, name, extra = "") => icons[name]
  ? `<span class="chip icon-chip ${extra}" title="${esc(name)}">${icons[name]}<span class="sr-only">${esc(name)}</span></span>`
  : `<span class="chip ${extra}">${esc(name)}</span>`;

// ---- reviews: a decision inbox ----------------------------------------------
// The old view was a vertical stack of cards where the evidence a decision
// rests on was prose inside the card. List and detail split them, so the
// question is on the left and everything needed to answer it is on the right.
//
// reviewFilter is which queue is showing; selectedReviewIndex is the row the
// keyboard acts on. Both are view state, not persisted: the inbox should open
// on what needs you.
let reviewFilter = "open";
let selectedReviewIndex = 0;
let reviewRows = [];

// Action labels are per kind: "approve" means something different for a
// deletion than for a gap, and a button reading "approve" says neither.
// Deletion's approve is the one destructive click in the UI -- it authorizes
// the next build's delete cascade -- so it wears the danger palette and keeps
// the two-step confirm.
const REVIEW_ACTIONS = {
  contradiction: {
    approve: { key: "A", label: (r) => {
      const name = r.unit ? unitSource(r.unit).name : "";
      return name ? `Accept ${name}` : "Accept this source";
    } },
    keep: { key: "K", label: () => "Keep both, note the conflict", quiet: true },
    dismiss: { key: "X", label: () => "Dismiss", quiet: true },
  },
  uncertain: {
    approve: { key: "A", label: () => "Treat as aspirational" },
    keep: { key: "K", label: () => "Ask the author", quiet: true },
    dismiss: { key: "X", label: () => "Dismiss", quiet: true },
  },
  deletion: {
    approve: { key: "A", label: () => "Approve deletion", danger: true },
    keep: { key: "K", label: () => "Keep the pages", quiet: true },
    dismiss: { key: "X", label: () => "Decide later", quiet: true },
  },
  gap: {
    approve: { key: "A", label: () => "Plan the page" },
    keep: { key: "K", label: () => "Not needed", quiet: true },
    dismiss: { key: "X", label: () => "Dismiss", quiet: true },
  },
};
// A kind nobody wrote a vocabulary for still resolves; it just uses the queue's
// own words rather than a wrong sentence.
const REVIEW_ACTIONS_FALLBACK = {
  approve: { key: "A", label: () => "Approve" },
  keep: { key: "K", label: () => "Keep", quiet: true },
  dismiss: { key: "X", label: () => "Dismiss", quiet: true },
};
const actionsFor = (kind) => REVIEW_ACTIONS[kind] || REVIEW_ACTIONS_FALLBACK;
const kindClass = (kind) =>
  REVIEW_ACTIONS[kind] ? `k-${kind}` : "k-other";

// showReviews keeps its old signature: `true` is the resolved queue, which is
// now a filter pill rather than a separate route. Every hash it has ever had
// stays routable.
async function showReviews(history) {
  if (history) reviewFilter = "answered";
  const view = beginView("Reviews", "reviews");
  try {
    // "answered" covers both statuses a resolution writes, 'resolved' and
    // 'approved'; asking for either alone would hide half the history.
    const wanted = reviewFilter === "answered" ? "answered" : "open";
    const [open, shown] = await Promise.all([
      api(`/workspaces/${encodeURIComponent(state.workspace)}/reviews?status=open`),
      wanted === "open"
        ? null
        : api(`/workspaces/${encodeURIComponent(state.workspace)}/reviews?status=answered`),
    ]);
    if (!view.current()) return;

    const pool = wanted === "open" ? open : shown;
    // "Researching" is a flag on an open review, not a status of its own: a
    // question someone handed to a worker is still waiting for a human.
    reviewRows = reviewFilter === "open" ? pool.filter((r) => !r.researching)
      : reviewFilter === "researching" ? pool.filter((r) => r.researching)
      : pool;
    if (selectedReviewIndex >= reviewRows.length) selectedReviewIndex = 0;

    const counts = {
      open: open.filter((r) => !r.researching).length,
      researching: open.filter((r) => r.researching).length,
    };
    const pill = (key, label) =>
      `<button class="pill" data-filter="${key}" aria-pressed="${reviewFilter === key}">${label}</button>`;

    if (!view.done(`<div class="inbox">
      <div class="inbox-list">
        <div class="inbox-head">
          <div class="view-head">
            <h1>Reviews</h1>
            <button class="help-btn" data-help="reviews"
              aria-label="What is this page for?" title="What is this page for?">?</button>
          </div>
          <p>Decisions a build could not make on its own.</p>
          <div class="pills" role="group" aria-label="Filter reviews">
            ${pill("open", `Open ${counts.open}`)}
            ${pill("researching", `Researching ${counts.researching}`)}
            ${pill("answered", "Resolved")}
          </div>
        </div>
        <div class="inbox-scroll" id="inbox-scroll" role="listbox" aria-label="Reviews">
          ${reviewRows.length ? reviewRows.map((r, i) => `
            <button class="inbox-row ${kindClass(r.kind)}" role="option" data-idx="${i}"
                    aria-current="${i === selectedReviewIndex}"
                    aria-selected="${i === selectedReviewIndex}">
              <span class="inbox-row-top">
                <span class="kind-sq">${REVIEW_KIND_ICONS[r.kind] || iconUncertain}</span>
                <span class="kind-name">${esc(r.kind)}</span>
                <span class="inbox-row-when">${esc(relTime(r.created))}</span>
              </span>
              <span class="inbox-row-title">${esc(r.title)}</span>
              <span class="inbox-row-unit">${esc(r.unit ? unitSource(r.unit).name : (r.pageSlug || ""))}</span>
            </button>`).join("")
            : `<div class="empty">${reviewFilter === "answered"
                ? "Nothing resolved yet. Answered reviews are kept here as a record of what was decided."
                : reviewFilter === "researching"
                  ? "Nothing is being researched right now."
                  : `No open reviews. Builds file one here when they need a decision,
                     such as confirming a deletion after a source disappears.`}</div>`}
        </div>
      </div>
      <div class="inbox-detail" id="inbox-detail">${reviewDetailHTML(reviewRows[selectedReviewIndex])}</div>
    </div>`)) return;

    wireInbox(view);
  } catch (err) {
    if (!err.handled) view.done(banner(err));
  }
}

// reviewDetailHTML is the right pane: the question, what it rests on, and the
// buttons that answer it. Rendered from the row already in memory, so moving
// the selection costs no request.
function reviewDetailHTML(r) {
  if (!r) return `<div class="empty">Select a review to see it here.</div>`;
  const acts = actionsFor(r.kind);
  const keys = (r.actions && r.actions.length ? r.actions : ["dismiss"]);
  return `
    <button class="btn quiet inbox-back" id="inbox-back">← All reviews</button>
    <div class="meta">
      <span class="kind-pill ${kindClass(r.kind)}">${esc(r.kind)}</span>
      ${r.unit ? `<span class="mono">${esc(unitSource(r.unit).name)}</span>` : ""}
      ${r.pageSlug ? `<a href="#/page/${encodeURIComponent(r.pageSlug)}">${esc(r.pageSlug)}</a>` : ""}
      <span class="meta-when">${r.status === "open"
        ? `filed ${esc(relTime(r.created))}`
        : `${esc(r.status)} ${esc(relTime(r.resolved || r.created))}`}</span>
    </div>
    <h2>${esc(r.title)}</h2>
    <div class="detail">${esc(r.detail)}</div>
    ${r.research ? `<div class="research-findings">
      <div class="rf-label">Research findings · ${esc(relTime(r.researched))}</div>
      <div class="detail">${esc(r.research)}</div>
    </div>` : ""}
    ${r.researching ? `<p class="hint" role="status">A worker is reading the sources
      for this question. Findings appear here when it finishes.</p>` : ""}
    ${r.status !== "open" ? "" : `<div class="action-bar">
      ${keys.map((a) => {
        const spec = acts[a] || REVIEW_ACTIONS_FALLBACK[a] || { key: "", label: () => a };
        const cls = spec.danger ? "btn danger" : spec.quiet ? "btn quiet" : "btn";
        return `<button class="${cls}" data-review="${esc(r.id)}" data-action="${esc(a)}"
          data-key="${esc(spec.key)}">${esc(spec.label(r))}${spec.key ? `<kbd>${esc(spec.key)}</kbd>` : ""}</button>`;
      }).join("")}
      ${r.researchable && !r.researching
        ? `<button class="btn quiet" data-research="${esc(r.id)}">Research</button>` : ""}
      <span class="action-hint">↑↓ to move · shortcuts work anywhere on this screen</span>
    </div>`}
    <div id="review-note" class="hint" role="status"></div>`;
}

// wireInbox binds the pane once per render: rows, pills, actions, and the
// keyboard that makes this an inbox rather than a list.
function wireInbox(view) {
  const note = (msg) => {
    const n = $("review-note");
    if (n) { n.textContent = msg; n.classList.add("error"); }
  };

  const select = (i, focusDetail) => {
    if (!reviewRows.length) return;
    selectedReviewIndex = (i + reviewRows.length) % reviewRows.length;
    for (const el of document.querySelectorAll(".inbox-row")) {
      const on = Number(el.dataset.idx) === selectedReviewIndex;
      el.setAttribute("aria-current", String(on));
      el.setAttribute("aria-selected", String(on));
      if (on) el.scrollIntoView({ block: "nearest" });
    }
    $("inbox-detail").innerHTML = reviewDetailHTML(reviewRows[selectedReviewIndex]);
    wireDetail();
    if (focusDetail) document.body.classList.add("inbox-detail-open");
  };

  for (const b of document.querySelectorAll(".inbox-row")) {
    b.addEventListener("click", () => select(Number(b.dataset.idx), true));
  }
  for (const b of document.querySelectorAll("[data-filter]")) {
    b.addEventListener("click", () => {
      if (reviewFilter === b.dataset.filter) return;
      reviewFilter = b.dataset.filter;
      selectedReviewIndex = 0;
      showReviews(false);
    });
  }

  const resolve = async (btn) => {
    const r = reviewRows[selectedReviewIndex];
    // The delete cascade is the one irreversible thing a click here can
    // authorize, so approving a deletion arms first and commits second.
    if (btn.dataset.action === "approve" && r?.kind === "deletion"
        && !armButton(btn, "approve")) return;
    try {
      await api(`/workspaces/${encodeURIComponent(state.workspace)}/reviews/${encodeURIComponent(btn.dataset.review)}/resolve`,
        { method: "POST", body: { action: btn.dataset.action } });
      // Decrement the badge locally: resolution may not bump the wiki
      // revision, so an ETag'd refetch could 304 to the stale list.
      updateReviewsBadge(Number($("reviews-badge").textContent || 1) - 1);
      toast(`Review ${btn.dataset.action === "approve" ? "approved" : "resolved"}`);
      // The row leaves the list and the next one takes the selection, so a
      // queue can be worked through without reaching for the mouse.
      document.body.classList.remove("inbox-detail-open");
      showReviews(false);
    } catch (err) {
      if (!err.handled) note(err.message);
    }
  };

  function wireDetail() {
    $("inbox-back")?.addEventListener("click", () =>
      document.body.classList.remove("inbox-detail-open"));
    for (const b of document.querySelectorAll("#inbox-detail [data-review]")) {
      once(b, () => resolve(b));
    }
    for (const b of document.querySelectorAll("#inbox-detail [data-research]")) {
      once(b, async () => {
        try {
          await api(`/workspaces/${encodeURIComponent(state.workspace)}/reviews/${encodeURIComponent(b.dataset.research)}/research`,
            { method: "POST", body: {} });
          // Nothing is resolved, so the badge is left alone: the question is
          // still waiting for a human, now with a reader working on it.
          toast("Research queued");
          showReviews(false);
        } catch (err) {
          if (!err.handled) note(err.message);
        }
      });
    }
  }
  wireDetail();

  // Document-level, so the shortcuts work anywhere on this screen rather than
  // only while a row has focus. Torn down with the view.
  const onKey = (e) => {
    if (!view.current()) return;
    if (e.metaKey || e.ctrlKey || e.altKey) return;
    // Suppressed while typing: without this, an "a" in the rail's filter box
    // would resolve whatever review happens to be selected.
    const t = e.target;
    if (t?.isContentEditable || /^(INPUT|TEXTAREA|SELECT)$/.test(t?.tagName ?? "")) return;
    if (e.key === "ArrowDown") { e.preventDefault(); select(selectedReviewIndex + 1); return; }
    if (e.key === "ArrowUp") { e.preventDefault(); select(selectedReviewIndex - 1); return; }
    // Length-checked before the membership test: "".includes is vacuously
    // true, and a key event with no name (some synthetic and IME events have
    // one) would otherwise fall through to the action lookup.
    const key = e.key.toUpperCase();
    if (key.length !== 1 || !"AKX".includes(key)) return;
    const btn = document.querySelector(`#inbox-detail [data-key="${key}"]`);
    if (btn) { e.preventDefault(); btn.click(); }
  };
  document.addEventListener("keydown", onKey);
  onViewCleanup(() => document.removeEventListener("keydown", onKey));
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

    // One palette for page type, shared with the tree dots, the index cards and
    // the palette rows, so a colour learned anywhere reads everywhere.
    const hue = { entity: "var(--type-entity)", synthesis: "var(--type-synthesis)",
                  source: "var(--type-source)", concept: "var(--type-concept)",
                  query: "var(--type-query)", comparison: "var(--type-comparison)" };
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

    // The canvas takes the whole column; the toolbar floats over it and the
    // rail beside it says what is selected. Nothing is chrome above the graph
    // any more -- the graph is the view.
    // The canvas is the view, so the heading it needs is one nobody has to see:
    // without it the graph's only heading is the rail's node name, and the view
    // opens at <h2> under nothing.
    if (!view.done(`<div class="graph-view">
      <h1 class="sr-only">Graph</h1>
      <div id="graph-wrap">
        <div class="graph-toolbar">
          <div class="graph-legend" role="group" aria-label="Filter by page type">
            ${Object.entries(typeCounts).map(([t, c]) =>
              `<button class="chip" data-type="${esc(t)}" aria-pressed="true">${typeDot(t, "")}${esc(t)} ${c}</button>`).join("")}
          </div>
          <div class="graph-controls" role="group" aria-label="View controls">
            <button id="graph-zoom-out" aria-label="Zoom out" title="Zoom out">&minus;</button>
            <span class="graph-zoom-level" id="graph-zoom-level" title="Zoom level">100%</span>
            <button id="graph-zoom-in" aria-label="Zoom in" title="Zoom in">+</button>
            <button id="graph-reset" aria-label="Reset view" title="Reset view">${iconReset}</button>
            <button id="graph-full" aria-label="Full screen" title="Full screen">${iconMax}</button>
          </div>
        </div>
        <svg id="graph-svg" role="img" aria-label="Page link graph"></svg>
      </div>
      <div class="graph-rail" id="graph-rail">
        <div class="group-label">Selected</div>
        <h2 id="graph-sel-name">Nothing yet</h2>
        <div class="sub" id="graph-sel-sub">${esc(plural(nodes.length, "page"))} · ${esc(plural(links.length, "link"))}${esc(truncated)}</div>
        <p class="why" id="graph-sel-why">Click any node to inspect it here. Neighbours
          are listed below; the graph dims everything else.</p>
        <div id="graph-sel-box" hidden>
          <div class="group-label">Neighbours</div>
          <div class="neighbours" id="graph-neighbours"></div>
          <a class="btn" id="graph-open" href="#">Open page</a>
        </div>
      </div>
    </div>`)) return;

    const svg = $("graph-svg");
    {
      const rect = svg.getBoundingClientRect();
      W = Math.max(320, Math.round(rect.width));
      H = Math.max(320, Math.round(rect.height));
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
      const c = mk("circle", { class: "graph-dot", r: r(n),
        fill: hue[n.type] || "var(--ink-dim)", opacity: "0.85" });
      const title = mk("title", {});
      title.textContent = `${n.title || n.slug} (${n.type}, ${n.links} inbound)`;
      c.appendChild(title);
      g.appendChild(c);
      if (n.links >= 2 || nodes.length <= 30) {
        const t = mk("text", { "font-size": "11.5", fill: "var(--ink-dim)" });
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
        const halo = g.querySelector("circle.graph-halo");
        const c = g.querySelector("circle.graph-dot");
        c.setAttribute("cx", pts[i].x.toFixed(1));
        c.setAttribute("cy", pts[i].y.toFixed(1));
        if (halo) {
          halo.setAttribute("cx", pts[i].x.toFixed(1));
          halo.setAttribute("cy", pts[i].y.toFixed(1));
        }
        const t = g.querySelector("text");
        if (t) {
          t.setAttribute("x", (pts[i].x + radiusOf(i) + 6).toFixed(1));
          t.setAttribute("y", (pts[i].y + 4).toFixed(1));
        }
      });
    };

    // ---- selection --------------------------------------------------------
    // Selecting is not navigating: the rail inspects a node, and opening its
    // page is a separate, explicit act. The old click-to-open survives as
    // double-click, which is what "I meant it" looks like on a canvas.
    let selected = -1;
    const radiusOf = (i) => (i === selected ? 13 : r(nodes[i]));

    const paintSelection = () => {
      nodeEls.forEach((g, i) => {
        const on = i === selected;
        const near = selected < 0 || on || neighbors[selected].has(i);
        g.classList.toggle("graph-dim", !near);
        g.querySelector("circle.graph-dot").setAttribute("r", radiusOf(i));
        let halo = g.querySelector("circle.graph-halo");
        if (on && !halo) {
          halo = mk("circle", { class: "graph-halo",
            fill: hue[nodes[i].type] || "var(--ink-dim)", opacity: "0.14" });
          g.insertBefore(halo, g.firstChild);
        } else if (!on && halo) halo.remove();
        if (halo) halo.setAttribute("r", radiusOf(i) + 7);
        const t = g.querySelector("text");
        if (t) {
          t.setAttribute("font-size", on ? "13" : "11.5");
          t.setAttribute("font-weight", on ? "600" : "400");
          t.setAttribute("fill", on ? "var(--ink)" : "var(--ink-dim)");
        }
      });
      for (const el of edgeEls) {
        const hot = selected >= 0 && (el._a === selected || el._b === selected);
        el.classList.toggle("graph-hot", hot);
        el.classList.toggle("graph-dim", selected >= 0 && !hot);
      }
      position();
    };

    const rail = {
      name: $("graph-sel-name"), sub: $("graph-sel-sub"), why: $("graph-sel-why"),
      box: $("graph-sel-box"), list: $("graph-neighbours"), open: $("graph-open"),
    };
    const select = (i) => {
      selected = i;
      paintSelection();
      const n = nodes[i];
      rail.name.textContent = n.title || n.slug;
      rail.sub.textContent = `${n.type} · ${plural(neighbors[i].size, "link")}`;
      rail.why.textContent = neighbors[i].size
        ? "Its neighbours are listed below; everything else is dimmed."
        : "Nothing links to this page and it links to nothing — usually worth a look.";
      rail.list.innerHTML = [...neighbors[i]].map((j) =>
        `<a href="#/page/${encodeURIComponent(nodes[j].slug)}">${typeDot(nodes[j].type)}<span>${esc(nodes[j].title || nodes[j].slug)}</span></a>`).join("");
      rail.open.href = `#/page/${encodeURIComponent(n.slug)}`;
      rail.box.hidden = false;
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
      H = Math.max(320, Math.round(rect.height));
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
    const endDrag = () => {
      if (!drag) return;
      if (drag.i !== undefined) {
        pts[drag.i].pinned = false;
        // A press that never travels is a click, so navigation survives the
        // drag handlers -- but it selects rather than leaves the graph.
        if (drag.moved < 3) select(drag.i);
      }
      drag = null;
    };
    svg.addEventListener("pointerup", endDrag);
    svg.addEventListener("pointercancel", endDrag);
    svg.addEventListener("dblclick", (e) => {
      const g = e.target.closest("g.graph-node");
      if (g) location.hash = "#/page/" + encodeURIComponent(nodes[g.dataset.i].slug);
    });
    for (const g of nodeEls) {
      g.addEventListener("keydown", (e) => {
        if (e.key === "Enter") select(Number(g.dataset.i));
      });
      g.addEventListener("focus", () => select(Number(g.dataset.i)));
    }

    // --- hover: light the neighborhood, dim the rest ----------------------
    // Only while nothing is selected: a selection is a standing answer, and a
    // passing cursor must not overwrite it.
    svg.addEventListener("pointerover", (e) => {
      if (selected >= 0) return;
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
      if (selected >= 0) return;
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
    const gloss = { purpose: "what this wiki is for and who reads it",
                    schema: "page conventions the builds should follow" };
    const placeholder = {
      purpose: "e.g. Documents the dispatch subsystem for on-call engineers. Assume Go fluency; explain domain terms.",
      schema: "e.g. One entity page per service. Comparisons only for alternatives we actually evaluated.",
    };
    if (!view.done(`${viewHead("Steering", "steering")}
      <p class="view-deck">Standing instructions handed to the agent on every build.
        Edits apply to future builds: steering shapes pages as they are written
        rather than rewriting what is already there.</p>
      ${["purpose", "schema"].map((k) => `
        <div class="steer-card">
          <div class="steer-head">
            <strong>${esc(k)}</strong>
            <span class="gloss">${esc(gloss[k])}</span>
          </div>
          <label class="sr-only" for="steering-${k}">${esc(k)}</label>
          <textarea id="steering-${k}" rows="6" placeholder="${esc(placeholder[k])}">${esc(docs[k] || "")}</textarea>
          <div class="steer-foot">
            <button class="btn" data-steer="${k}">${iconSave}Save ${esc(k)}</button>
            <span class="hint" id="steering-note-${k}" role="status"></span>
          </div>
        </div>`).join("")}`)) return;

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

// searchURL is one query string for both consumers of search, so the palette's
// hand-off and the results page can never disagree about what the query meant.
// prefix=1 is the as-you-type contract: the trailing word is still being typed.
const searchURL = (query, limit) =>
  `/workspaces/${encodeURIComponent(state.workspace)}/search?q=${encodeURIComponent(query)}` +
  `&prefix=1${limit ? `&limit=${limit}` : ""}`;

function searchHTML(query, hits) {
  if (!query) {
    return `<h1>Search</h1><div class="empty">Type in the rail's filter box, or press
      ⌘K, to search every page.</div>`;
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
// because the caret is in the filter box and every one of them would yank it out
// or make the list strobe.
async function showSearch(query, live = false) {
  const title = query ? `Search: ${query}` : "Search";
  const view = live ? null : beginView(title, null);
  const myNav = nav;
  const main = $("main");
  const paint = (html) => {
    if (nav !== myNav) return false; // the user navigated away mid-flight
    if (view) return view.done(html);
    // Live search never renders a full-bleed layout, and it writes here
    // directly rather than through view.done(), so it clears the class itself.
    main.classList.remove("bleed");
    main.innerHTML = html;
    return true;
  };
  if (live) {
    setTitle(title);
  } else {
    // The rail's box holds the query too: the tree narrows to the same words,
    // and refining the search means typing in the field the query is already
    // in. beginView moved focus to the results, as it should for a navigation;
    // this view is the exception, because the next thing anyone does with a
    // result list is refine it.
    $("filter").value = query;
    if (treeFilter !== query) { treeFilter = query; renderTree(lastActiveSlug); }
    $("filter").focus({ preventScroll: true });
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
  if (meta) $("bench-label").textContent = meta.name;
  $("bench-name").title = meta ? `${meta.name} — ${meta.pageCount} pages` : slug;
  for (const b of $("bench-list").querySelectorAll("button"))
    b.setAttribute("aria-current", String(b.dataset.slug === slug));
  $("bench").open = false;
  // A filter is about the bench that was open, not the one being opened.
  treeFilter = "";
  $("filter").value = "";

  // The run feed comes first: every freshness dot in the tree is measured
  // against the refs it publishes, so loading it after the pages would paint
  // the whole rail grey and then repaint it.
  state.connectors = await api(`/workspaces/${encodeURIComponent(slug)}/connectors`)
    .catch(() => []); // owner-gated; a 403 only costs the next-build sentence
  await refreshRunState();
  if (state.workspace !== slug) return; // raced a bench switch
  state.pages = await loadAllPages(slug);
  state.slugs = new Set(state.pages.map((p) => p.slug));
  route();
  armRunPoll();
  refreshGapsCount();

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
  if (hash === "overview") return overviewOrGetStarted();
  if (hash === "index") return showIndex();
  if (hash === "log") return showLog();
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
  return overviewOrGetStarted();
}

// A bench with no pages has no overview to show. Rather than an empty
// artifact, it gets the checklist that leads to one.
function overviewOrGetStarted() {
  return state.pages.length ? showOverview() : showGetStarted();
}

function setDrawer(open) {
  document.body.classList.toggle("nav-open", open);
  $("menu").setAttribute("aria-expanded", String(open));
  if (open) $("sidebar").querySelector("summary, input, a")?.focus();
  else $("menu").focus();
}

// ---- the command palette ----------------------------------------------------
// One box for both halves of finding something. The corpus is already in
// memory, so no keystroke costs a request; the last row hands the query off to
// full-text search, which is the one question the client cannot answer itself.
//
// The rail's filter box narrows the page tree. This answers "take me there".
const PALETTE_DESTINATIONS = [
  { title: "Overview", hash: "#/overview" }, { title: "Index", hash: "#/index" },
  { title: "Graph", hash: "#/graph" }, { title: "Gaps", hash: "#/gaps" },
  { title: "Log", hash: "#/log" }, { title: "Reviews", hash: "#/reviews" },
  { title: "Ingest", hash: "#/ingest" }, { title: "Steering", hash: "#/steering" },
  { title: "Members", hash: "#/members" }, { title: "Agent access (MCP)", hash: "#/mcp" },
];

// ingestNow queues a build from wherever you are. The one palette entry that
// does something rather than going somewhere.
async function ingestNow() {
  try {
    const res = await api(`/workspaces/${encodeURIComponent(state.workspace)}/runs`,
      { method: "POST", body: {} });
    toast(res.created ? "Run queued" : "A run was already waiting — joined it");
    await refreshRunState();
    armRunPoll();
  } catch (err) {
    if (!err.handled) toast(err.message);
  }
}

let closePalette = null, palRows = [], palSelection = 0;

// paletteGroups returns [{label, items}]; items carry the flat order the
// keyboard walks, so a group header can never be landed on.
function paletteGroups(q) {
  const groups = [];
  const pageRow = (p, html) => ({
    html: html ?? esc(p.title || p.slug), hint: p.type, dot: `t-${p.type}`,
    hash: `#/page/${encodeURIComponent(p.slug)}`,
  });

  if (!q) {
    const bySlug = new Map(state.pages.map((p) => [p.slug, p]));
    const recent = lsGet(recentKey(), []).filter((s) => state.slugs.has(s))
      .map((s) => pageRow(bySlug.get(s)));
    if (recent.length) groups.push({ label: "Recent", items: recent });
    groups.push({ label: "Actions", items: [
      { html: "Ingest now", hint: "queue a build", dot: "t-action", act: ingestNow },
      ...PALETTE_DESTINATIONS.map((v) =>
        ({ html: esc(v.title), hint: "go to", dot: "t-action", hash: v.hash })),
    ] });
    return groups;
  }

  // Six pages: past that the list stops being a shortlist and becomes a search
  // result, which is what the last row is for.
  // A row always shows the title, but the field that WON the match is often
  // the slug or a tag. Re-matching against the title is what puts the marks
  // where the reader is looking: highlighting only when the title happened to
  // be the best field meant they almost never appeared.
  const pages = matchPages(q).slice(0, 6).map((m) => {
    const title = m.p.title || m.p.slug;
    const inTitle = fuzzy(q, title);
    return pageRow(m.p, inTitle ? fuzzyHi(title, inTitle.idx) : esc(title));
  });
  if (pages.length) groups.push({ label: "Pages", items: pages });

  const actions = [];
  const ingest = fuzzy(q, "Ingest now");
  if (ingest) actions.push({ html: fuzzyHi("Ingest now", ingest.idx), hint: "queue a build",
    dot: "t-action", act: ingestNow, score: ingest.score });
  for (const v of PALETTE_DESTINATIONS) {
    const m = fuzzy(q, v.title);
    if (m) actions.push({ html: fuzzyHi(v.title, m.idx), hint: "go to", dot: "t-action",
      hash: v.hash, score: m.score });
  }
  actions.sort((a, b) => (b.score || 0) - (a.score || 0));
  actions.push({
    html: `Search full text for “${esc(q)}”`, hint: "every page", dot: "t-action",
    hash: `#/search/${encodeURIComponent(q)}`,
  });
  groups.push({ label: "Actions", items: actions });
  return groups;
}

function renderPalette(q) {
  const groups = paletteGroups(q.trim());
  palRows = groups.flatMap((g) => g.items);
  palSelection = 0;
  let i = 0;
  const html = groups.map((g) => `<li class="pal-group" role="presentation">${esc(g.label)}</li>` +
    g.items.map((r) => {
      const idx = i++;
      return `<li id="pal-opt-${idx}" role="option" aria-selected="${idx === 0}"${
        idx === 0 ? ' class="active"' : ""}>
        <span class="dot dot-6 ${r.dot}" aria-hidden="true"></span>
        <span class="pal-label">${r.html}</span>
        <span class="kind">${esc(r.hint)}</span></li>`;
    }).join("")).join("");
  $("palette-list").innerHTML = palRows.length ? html
    : `<li class="pal-empty" role="presentation">Nothing matched.</li>`;
  $("palette-input").setAttribute("aria-activedescendant", palRows.length ? "pal-opt-0" : "");
}

function movePaletteSelection(delta) {
  if (!palRows.length) return;
  palSelection = (palSelection + delta + palRows.length) % palRows.length;
  for (const li of $("palette-list").querySelectorAll("li[role=option]")) {
    const on = li.id === `pal-opt-${palSelection}`;
    li.classList.toggle("active", on);
    li.setAttribute("aria-selected", String(on));
    if (on) li.scrollIntoView({ block: "nearest" });
  }
  $("palette-input").setAttribute("aria-activedescendant", `pal-opt-${palSelection}`);
}

function pickPalette(i) {
  const row = palRows[i];
  if (!row) return;
  closePalette?.();
  if (row.act) { row.act(); return; }
  // Same-hash picks (e.g. re-opening the current page) never fire hashchange,
  // so route explicitly.
  if (location.hash === row.hash) route();
  else location.hash = row.hash;
}

function openPalette() {
  if (closePalette) return;
  $("palette-input").value = "";
  renderPalette("");
  closePalette = openOverlay($("palette"), () => { closePalette = null; });
}

// ---- the rail's filter box --------------------------------------------------
// It narrows the page tree as you type -- "which pages are CALLED this" -- and
// Enter hands the query to full-text search, which answers what the pages
// actually SAY. The palette above covers the jump.
function onFilterInput() {
  const q = $("filter").value.trim();
  if (treeFilter !== q) {
    treeFilter = q;
    renderTree(lastActiveSlug);
  }
  if (!onSearchView()) return;
  // The results view keeps up with the box. replaceState rather than assigning
  // the hash: one history entry per keystroke would turn Back into a
  // spellcheck of everything typed on the way here.
  clearTimeout(filterTimer);
  filterTimer = setTimeout(() => {
    if ($("filter").value.trim() !== q) return;
    history.replaceState(null, "", `#/search/${encodeURIComponent(q)}`);
    showSearch(q, true);
  }, SEARCH_DEBOUNCE);
}

const SEARCH_DEBOUNCE = 160;
let filterTimer = null;

function wireFilterBox() {
  const input = $("filter");
  input.addEventListener("input", onFilterInput);
  input.addEventListener("keydown", (e) => {
    if (e.key === "Enter") {
      e.preventDefault();
      const q = input.value.trim();
      // Search is a route like any other view: Back returns to the results,
      // and the query survives a reload or a shared link.
      if (q) location.hash = "#/search/" + encodeURIComponent(q);
    } else if (e.key === "Escape") {
      // Stopped here so it never reaches the document handler and closes the
      // mobile drawer out from under someone clearing a filter.
      e.stopPropagation();
      if (input.value) { input.value = ""; onFilterInput(); }
    }
  });
}

// showAccount fills the account sheet with whoever is signed in. Three
// answers are possible and each is worth saying plainly: a named user, a
// deployment with authentication switched off, and a bearer token with no
// profile behind it.
// initialsOf takes at most two letters out of a name or login, which is all a
// 26px circle can hold. A name with no letters at all falls back to the glyph
// rather than to an empty circle.
function initialsOf(name) {
  const parts = String(name || "").split(/[\s._-]+/).filter(Boolean);
  const letters = parts.map((p) => p[0]).filter((c) => /[a-z0-9]/i.test(c));
  return (letters.slice(0, 2).join("") || "·").toUpperCase();
}

async function showAccount() {
  const who = $("account-who");
  if (!who) return;
  const setAvatar = (label, name) => {
    const a = $("avatar");
    if (!a) return;
    a.textContent = initialsOf(name);
    a.setAttribute("aria-label", label);
    a.title = label;
  };
  try {
    const me = await api("/me");
    if (me?.anonymous) {
      who.textContent = "Signed in as nobody — this instance has authentication disabled";
      setAvatar("Account — authentication is disabled", "");
      return;
    }
    who.textContent = me?.login || me?.name || "Signed in";
    if (me?.name && me?.login) who.title = me.name;
    setAvatar(`Account — ${me?.login || me?.name || "signed in"}`, me?.name || me?.login);
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
    setAvatar(`Account — ${who.textContent}`, "");
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
  // The bench picker and the avatar menu are popups over chrome, so they close
  // the way popups do: on a click outside, and on choosing something inside.
  document.addEventListener("click", (e) => {
    const bench = $("bench");
    if (bench?.open && !e.target.closest("#bench")) bench.open = false;
    // Anywhere outside the pill and the popover itself dismisses the popover.
    if (runPopOpen && !e.target.closest("#run-pop, #run-pill")) closeRunPop();
  });

  document.addEventListener("keydown", (e) => {
    // cmd/ctrl-K opens the palette from anywhere, INCLUDING while a text field
    // has focus: it is the way out of any box, not a shortcut that only works
    // when your hands are already off the keyboard.
    if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") {
      e.preventDefault();
      openPalette();
      return;
    }
    // Escape priority: palette (handled inside its own overlay) -> menus ->
    // run popover -> hover preview -> drawer.
    if (e.key === "Escape") {
      const openMenu = document.querySelector(".footer-menu[open], #bench[open]");
      if (openMenu) { openMenu.open = false; openMenu.querySelector("summary")?.focus(); return; }
      if (closeRunPop()) { $("run-pill").focus(); return; }
      if (hidePreview()) return;
      if (document.body.classList.contains("nav-open")) setDrawer(false);
      return;
    }
    // Everything below is a bare letter, so it must never fire while someone
    // is typing one.
    const t = e.target;
    if (t?.isContentEditable || /^(INPUT|TEXTAREA|SELECT)$/.test(t?.tagName ?? "")) return;
    if (e.metaKey || e.ctrlKey || e.altKey) return;
    if (e.key === "/") { e.preventDefault(); $("filter")?.focus(); }
  });

  $("cmd-trigger").addEventListener("click", openPalette);
  $("run-pill").addEventListener("click", toggleRunPop);

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
    if (!li) return;
    e.preventDefault();
    pickPalette([...$("palette-list").querySelectorAll("li[role=option]")].indexOf(li));
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
  // Navigation always dismisses a lingering preview card or pending timer.
  window.addEventListener("hashchange", hidePreview);

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
      // over no pages, and ten views that would all render nothing. The
      // shell hides itself so the one thing worth doing is the only thing on
      // screen.
      document.body.classList.add("no-bench");
      // A dead end before this: a signed-in user with no bench was told to
      // run a CLI command on a machine they may not have.
      $("main").classList.remove("bleed");
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
    wireFilterBox();
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
    $("main").classList.remove("bleed");
    $("main").innerHTML = banner(err, true);
    wireBannerRetry();
  }
}

boot();
