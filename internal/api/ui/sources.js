"use strict";
// Sources: where a bench's material is configured. Connector management
// (owners and instance admins) and uploaded documents (any writing member)
// share the view because they answer the same question -- "what does this
// wiki read?" -- at two permission levels. One Add button opens a guided
// flow; the page itself stays a status surface. Loaded before app.js; only
// defines functions, and resolves shared helpers (api, esc, beginView,
// openOverlay…) at call time.

// humanTokens renders a token count for a number that is being watched while
// it climbs. Thousands are rounded to one decimal and millions to two: the
// exact digit is never the point, and a figure whose last three characters
// churn every poll reads as noise rather than as progress.
function humanTokens(n) {
  const v = Number(n) || 0;
  if (v < 1000) return `${v} tokens`;
  if (v < 1_000_000) return `${(v / 1000).toFixed(1)}k tokens`;
  return `${(v / 1_000_000).toFixed(2)}M tokens`;
}

// humanBytes renders a byte count the way a person scans a file list.
function humanBytes(n) {
  if (!Number.isFinite(n) || n < 0) return "";
  if (n < 1024) return `${n} B`;
  const units = ["KiB", "MiB", "GiB"];
  let v = n;
  for (const u of units) {
    v /= 1024;
    if (v < 1024) return `${v >= 10 ? Math.round(v) : v.toFixed(1)} ${u}`;
  }
  return `${Math.round(v)} TiB`;
}

// humanDuration renders whole seconds as friendly prose ("10 minutes").
function humanDuration(sec) {
  if (sec >= 3600 && sec % 3600 === 0) {
    const h = sec / 3600;
    return h === 1 ? "hour" : `${h} hours`;
  }
  const m = Math.max(1, Math.round(sec / 60));
  return m === 1 ? "minute" : `${m} minutes`;
}

// connectorSummary compresses a config into one scannable line.
function connectorSummary(c) {
  const cfg = c.config || {};
  if (c.kind === "git") return cfg.url || cfg.path || "";
  if (c.kind === "web") {
    const urls = Array.isArray(cfg.urls) ? cfg.urls : (cfg.url ? [cfg.url] : []);
    return urls.length === 1 ? urls[0] : `${urls.length} pages`;
  }
  if (c.kind === "upload") return cfg.path || "uploaded documents";
  return "";
}

// kilnFiring draws the one state this whole page exists to produce: a bench
// being fired. "A build is running now" spent a sentence telling the reader
// something the page could simply show them, so the kiln says it instead --
// the ember in its mouth glows while material is being turned into pages, and
// is gone the moment the run is.
//
// The count rides alongside rather than inside: it is the part a screen reader
// needs, and the part that changes every poll.
const kilnFiring = (detail) => `
  <span class="kiln-firing">
    <svg class="kiln-flame" viewBox="0 0 24 24" aria-hidden="true">
      <path class="kiln-shell" fill-rule="evenodd" d="M3 22 L3 12 Q3 2 12 2 Q21 2 21 12 L21 22 Z
        M8 22 L8 15 Q8 10 12 10 Q16 10 16 15 L16 22 Z"/>
      <circle class="kiln-glow" cx="12" cy="18.5" r="4.6"/>
      <circle class="kiln-ember" cx="12" cy="18.5" r="2.2"/>
    </svg>
    <span class="kiln-firing-text">Firing${detail ? ` — ${esc(detail)}` : ""}</span>
  </span>`;

// runningNow returns the firing kiln when a run is in motion, and nothing
// otherwise, so a caller can put the animation where its own prose would go.
function runningNow(runs) {
  const running = runs.find((r) => r.status === "running");
  if (!running) return "";
  // How far along, when that is known. A build with no plan yet has decided
  // nothing to report, and guessing at a number would be worse than silence.
  return kilnFiring(running.unitsTotal
    ? `${running.unitsDone || 0} of ${running.unitsTotal} done`
    : "");
}

// nextBuildLine answers the question every visitor to this page has: when
// does the wiki next catch up with its sources? A run already in motion is
// not a schedule and is not answered here -- runningNow draws it.
function nextBuildLine(runs, connectors, pollSeconds) {
  const queued = runs.find((r) => r.status === "queued");
  if (queued) {
    if (queued.notBefore) {
      const ms = new Date(queued.notBefore).getTime() - Date.now();
      if (ms > 45000) {
        const m = Math.round(ms / 60000);
        return `A build is queued — it starts in about ${m <= 1 ? "a minute" : `${m} minutes`}.`;
      }
    }
    return "A build is queued and starts shortly.";
  }

  const enabled = connectors.filter((c) => c.enabled);
  const polled = enabled.filter((c) => c.triggerMode === "poll");
  if (polled.length && pollSeconds > 0) {
    const every = humanDuration(pollSeconds);
    const synced = polled.map((c) => c.lastSynced).filter(Boolean)
      .map((s) => new Date(s).getTime());
    if (synced.length === polled.length) {
      const next = Math.min(...synced) + pollSeconds * 1000;
      if (next > Date.now()) {
        const m = Math.max(1, Math.round((next - Date.now()) / 60000));
        return `Sources are checked every ${every}; the next check is in about ${m === 1 ? "a minute" : `${m} minutes`}.`;
      }
      return `Sources are checked every ${every}; the next check is due now.`;
    }
    return `Sources are checked every ${every}; the next sweep picks up anything new.`;
  }

  const hooks = [];
  if (enabled.some((c) => c.kind === "git" && c.triggerMode === "webhook")) {
    hooks.push("when the connected repository receives a push");
  }
  if (enabled.some((c) => c.kind === "upload" && c.triggerMode === "webhook")) {
    hooks.push("a couple of minutes after documents change");
  }
  if (hooks.length) return `Builds start automatically ${hooks.join(", and ")}.`;

  return "Sources are ingested when you press Ingest now.";
}

// armButton is the shared two-step destructive confirm: first click arms
// (the button turns into an explicit text question), second within 4s
// commits. Returns true when the click should proceed. Restores whatever the
// button held before -- text or icon -- so icon buttons survive the round trip.
function armButton(b, label) {
  if (b.dataset.armed === "true") return true;
  const orig = b.innerHTML;
  b.dataset.armed = "true";
  b.textContent = `confirm ${label}`;
  b.classList.add("danger");
  setTimeout(() => {
    if (b.isConnected) {
      b.dataset.armed = "false";
      b.innerHTML = orig;
      b.classList.remove("danger");
    }
  }, 4000);
  return false;
}

// Icon glyphs for row controls, drawn in currentColor so the quiet-button
// palette applies. Buttons carry aria-labels; the icons are decoration.
const iconPause = `<svg class="icon" viewBox="0 0 16 16" aria-hidden="true"><path d="M4 2h3v12H4zM9 2h3v12H9z"/></svg>`;
const iconPlay = `<svg class="icon" viewBox="0 0 16 16" aria-hidden="true"><path d="M4 2l9 6-9 6z"/></svg>`;
const iconTrash = `<svg class="icon" viewBox="0 0 16 16" aria-hidden="true"><path d="M6 1h4v1h4v2H2V2h4zM3 5h10l-.8 10H3.8zM6 7v6h1V7zm3 0v6h1V7z"/></svg>`;
// The two primary actions on the ingest view. A plus adds a source; the
// funnel is the bench itself, material narrowing into one wiki.
const iconPlus = `<svg class="icon" viewBox="0 0 16 16" aria-hidden="true"><path d="M7 2h2v5h5v2H9v5H7V9H2V7h5z"/></svg>`;
const iconIngest = `<svg class="icon" viewBox="0 0 16 16" aria-hidden="true"><path d="M1.5 2h13l-5 6.2V14L6.5 12V8.2z"/></svg>`;

// Glyphs for the source a unit came from. Names are distinct from app.js's
// icon consts: both files are classic scripts sharing one global scope, where
// a repeated top-level const is a SyntaxError that takes down the whole UI.
const iconRepo = `<svg class="icon" viewBox="0 0 16 16" aria-hidden="true"><path fill-rule="evenodd" d="M3 1h8.5L14 3.4V15H3a1.4 1.4 0 0 1 0-2.8h9.2V1.9H3.9v9.4H3a2.3 2.3 0 0 0-.9.2V2.3A1.3 1.3 0 0 1 3 1z"/></svg>`;
const iconWeb = `<svg class="icon" viewBox="0 0 16 16" aria-hidden="true"><path fill-rule="evenodd" d="M8 1a7 7 0 1 0 0 14A7 7 0 0 0 8 1zM6.2 3.1a9.6 9.6 0 0 0-1 3.9H2.9a5.2 5.2 0 0 1 3.3-3.9zM8 2.9c.6.7 1.1 2.1 1.3 4.1H6.7c.2-2 .7-3.4 1.3-4.1zm1.8.2A5.2 5.2 0 0 1 13.1 7h-2.3a9.6 9.6 0 0 0-1-3.9zM2.9 8.8h2.3a9.6 9.6 0 0 0 1 4 5.2 5.2 0 0 1-3.3-4zm4.1 0h2.6c-.2 2-.7 3.4-1.3 4.2-.6-.8-1.1-2.2-1.3-4.2zm4.4 0h2.3a5.2 5.2 0 0 1-3.3 4 9.6 9.6 0 0 0 1-4z"/></svg>`;
const iconDoc = `<svg class="icon" viewBox="0 0 16 16" aria-hidden="true"><path fill-rule="evenodd" d="M3 1h6.2L13 4.8V15H3V1zm5.6 1.6v2.6h2.6L8.6 2.6zM5 8h6v1.2H5zm0 2.6h6v1.2H5z"/></svg>`;
const iconModule = `<svg class="icon" viewBox="0 0 16 16" aria-hidden="true"><path fill-rule="evenodd" d="M8 1.2l5.6 3.1v7.4L8 14.8l-5.6-3.1V4.3L8 1.2zm0 2L4.4 5.2 8 7.2l3.6-2L8 3.2zM3.9 6.6v4.3L7.2 12.8V8.4L3.9 6.6zm4.9 1.8v4.4l3.3-1.9V6.6L8.8 8.4z"/></svg>`;
const iconArch = `<svg class="icon" viewBox="0 0 16 16" aria-hidden="true"><path fill-rule="evenodd" d="M6.2 1h3.6v3.6H8.9v1.9h3.3v2.1h1.4V12h-3.4V8.6h1.3V7.4H4.5v1.2h1.3V12H2.4V8.6h1.4V6.5h3.3V4.6H6.2V1z"/></svg>`;

// SOURCE_ICONS is keyed by the words unitSource hands back, so a type with no
// glyph falls back to its word rather than vanishing.
const SOURCE_ICONS = {
  "document": iconDoc,
  "web page": iconWeb,
  "repository document": iconRepo,
  "code module": iconModule,
  "architecture": iconArch,
};

// unitSource splits a cache key into the source that produced it and the name
// of the thing itself. The namespaces are cache plumbing -- they exist so an
// uploaded README and a repo README cannot collide (internal/diff/keys.go) --
// and a reader watching a run wants the document's name, not its bucket. The
// run item's own `kind` field cannot answer this: the API sets it to the bare
// prefix, so uploads, fetched pages and repo documents all arrive as "doc".
// Mirrors diff.Namespace; anything unrecognized keeps the raw key, since a
// half-parsed key is worse than an honest one.
function unitSource(key) {
  const k = String(key || "");
  const colon = k.indexOf(":");
  if (colon < 0) return { label: "", cls: "", name: k };
  const prefix = k.slice(0, colon);
  const id = k.slice(colon + 1);
  if (prefix === "doc") {
    if (id.startsWith("upload:")) {
      return { label: "document", cls: "kind-upload", name: id.slice(7) };
    }
    if (id.startsWith("web:")) {
      return { label: "web page", cls: "kind-web", name: id.slice(4) };
    }
    return { label: "repository document", cls: "kind-git", name: id };
  }
  if (prefix === "module") return { label: "code module", cls: "kind-git", name: id };
  if (prefix === "arch") return { label: "architecture", cls: "", name: id };
  return { label: "", cls: "", name: k };
}

// unitKeyHTML renders a unit's identity: source as a glyph, name as itself.
const unitKeyHTML = (key) => {
  const s = unitSource(key);
  return `<span class="unit-key">
    ${s.label ? iconChip(SOURCE_ICONS, s.label, s.cls) : ""}
    <span class="mono">${esc(s.name)}</span>
  </span>`;
};

// uploadWorkspaceFiles pushes a FileList one request at a time, reporting
// progress into progressEl. Shared by the page dropzone and the wizard's.
// Calls onDone(okCount, lastBuildState, failedCount) when at least one file
// landed. The failure count is what lets a caller tell a clean batch from a
// partial one, which is the difference between closing the dialog and keeping
// it open with the error still readable.
async function uploadWorkspaceFiles(ws, fileList, progressEl, onDone) {
  if (!fileList.length) return;
  const progress = (msg, isErr) => {
    if (progressEl) {
      progressEl.textContent = msg;
      progressEl.classList.toggle("error", Boolean(isErr));
    }
  };
  let ok = 0, lastBuild = "";
  const failures = [];
  for (const [i, file] of [...fileList].entries()) {
    progress(`Uploading ${file.name} (${i + 1} of ${fileList.length})…`);
    const form = new FormData();
    // webkitRelativePath preserves folder structure on directory drops.
    if (file.webkitRelativePath) form.append("path", file.webkitRelativePath);
    form.append("file", file, file.name);
    try {
      const res = await api(`/workspaces/${ws}/files`, { method: "POST", body: form });
      ok++;
      lastBuild = res.build;
    } catch (err) {
      if (err.handled) return;
      failures.push(`${file.name}: ${err.message}`);
    }
  }
  progress(failures.length ? failures.join(" · ") : "", failures.length > 0);
  if (ok) {
    toast(lastBuild === "queued"
      ? `${ok} document${ok === 1 ? "" : "s"} uploaded — build queued`
      : `${ok} document${ok === 1 ? "" : "s"} uploaded — changes pending until the next build`);
    onDone?.(ok, lastBuild, failures.length);
  }
}

// wireDropzone attaches click/keyboard/drag behavior to a dropzone element
// with its hidden file input, funneling every path into onFiles(fileList).
function wireDropzone(zone, input, onFiles) {
  zone.addEventListener("click", () => input.click());
  zone.addEventListener("keydown", (e) => {
    if (e.key === "Enter" || e.key === " ") { e.preventDefault(); input.click(); }
  });
  input.addEventListener("change", () => {
    onFiles(input.files);
    input.value = "";
  });
  zone.addEventListener("dragover", (e) => {
    e.preventDefault();
    zone.classList.add("over");
  });
  zone.addEventListener("dragleave", () => zone.classList.remove("over"));
  zone.addEventListener("drop", (e) => {
    e.preventDefault();
    zone.classList.remove("over");
    onFiles(e.dataTransfer.files);
  });
}

const dropzoneHTML = (idSuffix) => `
  <div class="dropzone" id="dropzone${idSuffix}" role="button" tabindex="0"
       aria-label="Upload documents">
    Drop documents here, or <span class="linkish">choose files</span>
    <input id="file-input${idSuffix}" type="file" multiple hidden>
  </div>
  <div id="upload-progress${idSuffix}" class="hint" role="status"></div>`;

// ---- the add-source wizard --------------------------------------------------
// One guided flow for every source kind. Steps render into the static
// #wizard overlay (opened through the app's modal contract); validation
// errors from the server land inline in the step, never closing the flow.

function openSourceWizard(ctx) {
  // ctx: { ws, credentials, hasUpload, refresh }
  const body = $("wizard-body");
  let close = null;
  let changed = false;
  const finish = () => { close?.(); };

  const note = (msg) => {
    const n = $("wizard-note");
    if (n) { n.textContent = msg; n.classList.toggle("error", Boolean(msg)); }
  };

  const stepShell = (crumb, inner) => {
    body.innerHTML = `
      <div class="wizard-head">
        <span class="wizard-steps">${esc(crumb)}</span>
        <button class="wizard-x" id="wizard-x" aria-label="Close">×</button>
      </div>
      ${inner}
      <span class="hint" id="wizard-note" role="status"></span>`;
    $("wizard-x").addEventListener("click", finish);
  };

  const KINDS = [
    { kind: "git", title: "Repository",
      blurb: "Scan a git repository: an https remote, or a directory the worker is permitted to read." },
    { kind: "web", title: "Web pages",
      blurb: "Fetch https pages and fold them into the same wiki as everything else." },
    { kind: "upload", title: "Documents",
      blurb: "Upload PDFs, Office files, markdown, and HTML straight from this browser." },
  ];

  const stepPick = () => {
    stepShell("Add a source — what feeds this bench?", `
      ${KINDS.map((k) => `
        <button class="wizard-card" data-wizard-kind="${k.kind}">
          <strong>${esc(k.title)}</strong>
          <span class="hint">${esc(k.blurb)}</span>
        </button>`).join("")}
      <div class="wizard-actions">
        <button class="btn quiet" id="wizard-cancel">Cancel</button>
      </div>`);
    for (const b of body.querySelectorAll("[data-wizard-kind]")) {
      b.addEventListener("click", () => stepForm(b.dataset.wizardKind));
    }
    $("wizard-cancel").addEventListener("click", finish);
  };

  const backLink = `<button class="btn quiet" id="wizard-back">Back</button>`;
  const wireBack = () => $("wizard-back")?.addEventListener("click", stepPick);

  const submitConnector = async (payload) => {
    try {
      await api(`/workspaces/${ctx.ws}/connectors`, { method: "POST", body: payload });
      changed = true;
      return true;
    } catch (err) {
      if (!err.handled) note(err.message);
      return false;
    }
  };

  const stepForm = (kind) => {
    if (kind === "git") {
      const credOptions = ctx.credentials.map((cr) =>
        `<option value="${esc(cr.id)}">${esc(cr.kind)} · ${esc((cr.created || "").slice(0, 10))}</option>`).join("");
      stepShell("Add a repository", `
        <label class="hint" for="wiz-git-name">Name</label>
        <input id="wiz-git-name" placeholder="code" autocomplete="off" spellcheck="false">
        <label class="hint" for="wiz-git-url">HTTPS remote, or a server path under the worker's permitted roots</label>
        <input id="wiz-git-url" placeholder="https://github.com/org/repo.git" autocomplete="off" spellcheck="false">
        <label class="hint" for="wiz-git-cred">Credential for private remotes</label>
        <select id="wiz-git-cred" aria-label="Credential">
          <option value="">none (public)</option>${credOptions}
        </select>
        <input id="wiz-git-pat" type="password" placeholder="…or paste a new git PAT to seal"
               autocomplete="off" spellcheck="false">
        <label class="hint" for="wiz-git-trigger">When should it build?</label>
        <select id="wiz-git-trigger" aria-label="Trigger mode">
          <option value="manual" selected>only when I press Ingest now</option>
          <option value="webhook">automatically on pushes (GitHub webhook)</option>
          <option value="poll">on a regular poll schedule</option>
        </select>
        <div class="wizard-actions">
          ${backLink}
          <button class="btn" id="wiz-git-add">Add repository</button>
        </div>`);
      wireBack();
      once($("wiz-git-add"), async () => {
        const name = $("wiz-git-name").value.trim();
        const source = $("wiz-git-url").value.trim();
        if (!name || !source) {
          note("name and remote (or path) are required");
          return;
        }
        let credentialId = $("wiz-git-cred").value;
        const pat = $("wiz-git-pat").value.trim();
        if (pat) {
          // A pasted secret is sealed first; the connector then references
          // the sealed credential rather than ever carrying the plaintext.
          try {
            const cred = await api(`/workspaces/${ctx.ws}/credentials`,
              { method: "POST", body: { kind: "git_pat", secret: pat } });
            credentialId = cred.id;
            $("wiz-git-pat").value = "";
          } catch (err) {
            if (!err.handled) note(err.message);
            return;
          }
        }
        const config = source.startsWith("https://") ? { url: source } : { path: source };
        if (await submitConnector({
          kind: "git", name, config, credentialId,
          triggerMode: $("wiz-git-trigger").value,
        })) stepDone("Source added", `Repository “${name}” added.`);
      });
      return;
    }

    if (kind === "web") {
      stepShell("Add web pages", `
        <label class="hint" for="wiz-web-name">Name</label>
        <input id="wiz-web-name" placeholder="docs site" autocomplete="off" spellcheck="false">
        <label class="hint" for="wiz-web-urls">HTTPS addresses, one per line</label>
        <textarea id="wiz-web-urls" rows="4" placeholder="https://example.com/handbook"></textarea>
        <label class="hint" for="wiz-web-trigger">How should they stay fresh?</label>
        <select id="wiz-web-trigger" aria-label="Trigger mode">
          <option value="poll" selected>re-fetch on a regular poll schedule</option>
          <option value="manual">only when I press Ingest now</option>
        </select>
        <div class="wizard-actions">
          ${backLink}
          <button class="btn" id="wiz-web-add">Add web source</button>
        </div>`);
      wireBack();
      once($("wiz-web-add"), async () => {
        const name = $("wiz-web-name").value.trim();
        const urls = $("wiz-web-urls").value.split("\n").map((u) => u.trim()).filter(Boolean);
        if (!name || !urls.length) {
          note("name and at least one https address are required");
          return;
        }
        if (await submitConnector({
          kind: "web", name, config: { urls },
          triggerMode: $("wiz-web-trigger").value,
        })) stepDone("Source added", `Web source “${name}” added.`);
      });
      return;
    }

    // Documents: enable the upload source if the bench lacks one, then land
    // on the staging step -- files collect locally and nothing leaves the
    // browser until Upload is pressed.
    if (ctx.hasUpload) {
      stepDocuments();
      return;
    }
    stepShell("Enable document uploads", `
      <p class="hint">Uploads live with the bench: PDFs, Office files, markdown,
      and HTML are extracted and woven into the wiki alongside the other
      sources. Deleting a document later asks for review before any page goes.</p>
      <div class="wizard-actions">
        ${backLink}
        <button class="btn" id="wiz-upload-enable">Enable uploads</button>
      </div>`);
    wireBack();
    once($("wiz-upload-enable"), async () => {
      if (await submitConnector({ kind: "upload", name: "documents", config: {} })) {
        ctx.hasUpload = true;
        stepDocuments();
      }
    });
  };

  // stepDocuments stages dropped files locally and uploads only on the
  // explicit button press, so a stray drop is reversible and a batch goes up
  // as one deliberate action.
  const stepDocuments = () => {
    let staged = [];
    stepShell("Add documents", `
      <p>Gather files below, then press Upload.</p>
      ${dropzoneHTML("-wizard")}
      <div id="wizard-staged"></div>
      <div class="wizard-actions">
        ${backLink}
        <button class="btn" id="wizard-upload" disabled>Upload</button>
      </div>`);
    wireBack();

    const renderStaged = () => {
      const box = $("wizard-staged");
      box.innerHTML = staged.map((f, i) => `
        <div class="row">
          <span class="mono">${esc(f.name)}</span>
          <span>
            <span class="count">${esc(humanBytes(f.size))}</span>
            <button class="btn quiet" data-unstage="${i}">remove</button>
          </span>
        </div>`).join("");
      for (const b of box.querySelectorAll("[data-unstage]")) {
        b.addEventListener("click", () => {
          staged.splice(Number(b.dataset.unstage), 1);
          renderStaged();
        });
      }
      $("wizard-upload").disabled = staged.length === 0;
    };

    wireDropzone($("dropzone-wizard"), $("file-input-wizard"), (files) => {
      staged.push(...files);
      renderStaged();
    });

    once($("wizard-upload"), async () => {
      const batch = staged;
      await uploadWorkspaceFiles(ctx.ws, batch, $("upload-progress-wizard"), (_ok, _build, failed) => {
        changed = true;
        staged = [];
        if (!failed) {
          // Pressing Upload is the end of this errand: everything asked for
          // went up, the toast says so, and the page behind is what the reader
          // wants to see. Closing refreshes it through the overlay's own close
          // handler, so the refresh is not also done here.
          finish();
          return;
        }
        // A partial batch keeps the dialog: the failures are written into the
        // progress line, and closing over them would destroy the only account
        // of what did not make it.
        renderStaged();
        ctx.refresh();
      });
    });
  };

  const stepDone = (crumb, msg) => {
    stepShell(crumb, `
      <p>${esc(msg)}</p>
      <div class="wizard-actions">
        <button class="btn quiet" id="wizard-back">Back</button>
        <button class="btn" id="wizard-done">Done</button>
      </div>`);
    wireBack();
    $("wizard-done").addEventListener("click", finish);
  };

  close = openOverlay($("wizard"), () => {
    body.innerHTML = "";
    if (changed) ctx.refresh();
  });
  stepPick();
}

// ---- the sources view -------------------------------------------------------

// ---- run feed ----------------------------------------------------------------
// These render the Recent runs list. They live out here rather than inside
// showSources because the live poll re-renders the feed alone: rebuilding the
// whole view on a timer is what used to throw the reader back to the top of
// the page every five seconds.

const money = (v) => `$${Number(v || 0).toFixed(2)}`;

// Trigger values are queue internals; runs started by hand carry no chip at
// all (that is the normal case), automatic ones say why they started, with the
// longer explanation a hover away.
const triggerChip = {
  webhook: ["repo push", "A push to the connected repository started this run."],
  upload: ["documents changed", "Documents were uploaded or removed, so a run was queued."],
  poll: ["scheduled check", "The poll schedule came due and re-read the source."],
  continuation: ["leftover pages",
    "The previous run reached its per-run page cap; this run built the pages it had to defer."],
};

// Each run reads as one sentence about what happened, not a ledger row.
const runOutcome = (r) => {
  if (r.status === "queued") return "Waiting to start";
  if (r.status === "running") {
    // Before the plan exists there is genuinely nothing to count, and
    // "0 of 0" reads as broken rather than as early.
    if (!r.unitsTotal) return "Ingesting now…";
    const done = r.unitsDone || 0;
    return `Ingesting — ${done} of ${r.unitsTotal} ${r.unitsTotal === 1 ? "unit" : "units"} done`;
  }
  if (r.status === "failed") return "Failed";
  if (r.status === "over_budget") return "Stopped at the budget cap";
  const parts = [];
  if (r.pagesCreated) parts.push(`${r.pagesCreated} created`);
  if (r.pagesUpdated) parts.push(`${r.pagesUpdated} updated`);
  if (r.pagesDeleted) parts.push(`${r.pagesDeleted} removed`);
  if (!parts.length) return "Nothing changed — no cost";
  const n = (r.pagesCreated || 0) + (r.pagesUpdated || 0) + (r.pagesDeleted || 0);
  return `${n === 1 ? "1 page" : `${n} pages`}: ${parts.join(", ")}`;
};

// Unit states as the queue's own words. The one being written now is bolded by
// CSS, which is what separates it from the ones merely queued behind it.
const unitLabel = {
  running: ["pending", "run-running"],
  pending: ["queued", "run-queued"],
  deferred: ["left for the next run", "run-queued"],
  failed: ["failed", "run-failed"],
};

// The live list: what is being worked on, what is queued behind it, what
// already landed. Ordered by the API so the first rows are the ones that
// answer "is anything happening".
const liveUnits = (items) => {
  if (!items.length) return "";
  return `<div class="detail run-live-units">${items.map((it) => {
    const [label, cls] = unitLabel[it.status] || ["done", "run-succeeded"];
    return `<div class="row">
      ${unitKeyHTML(it.key)}
      <span>
        <span class="chip ${cls}">${esc(label)}</span>
        ${it.tokens > 0 ? `<span class="count">${esc(humanTokens(it.tokens))}</span>` : ""}
        ${it.costUsd > 0 ? `<span class="count">${money(it.costUsd)}</span>` : ""}
      </span>
    </div>`;
  }).join("")}</div>`;
};

// What the run has consumed so far. Summed from the units that have settled
// rather than read off the run row, which is written once when the run ends
// and reads zero for the whole time anyone is watching.
const liveTokens = (items) => items.reduce((n, it) => n + (Number(it.tokens) || 0), 0);

// A run's error arrives from the queue as "<cache key>: <what went wrong>",
// written for whoever reads the logs. Both halves leak: the key is the cache
// namespacing the reader was never meant to meet, and the tail is often a
// validation report addressed to whoever maintains the generator, not to the
// person who uploaded a PDF. So: say which source failed in the words used
// everywhere else on this page, say what happened in a sentence, and keep the
// report itself one click away rather than in their face. Nothing is
// discarded -- an operator who needs the detail still has it, and the CLI and
// the logs still carry the original untouched.
const KEY_RE = /^((?:doc:(?:upload:|web:)?|module:|arch:|entry:)[^\s].*?): ([\s\S]*)$/;
const VIOLATIONS_RE = /^(\d+) validation violation\(s\): ([\s\S]*)$/;

function runErrorHTML(r) {
  if (!r.error) return "";
  const keyed = KEY_RE.exec(r.error);
  // An error that does not name a unit is about the run itself; it has no
  // source to attribute and no report to hide.
  if (!keyed) return `<div class="detail hint error">${esc(r.error)}</div>`;
  const [, key, tail] = keyed;
  const bad = VIOLATIONS_RE.exec(tail);
  if (!bad) {
    return `<div class="detail hint error run-error">${unitKeyHTML(key)}<span>${esc(tail)}</span></div>`;
  }
  const n = Number(bad[1]);
  // One line, deliberately: .review .detail is pre-wrap so that a server
  // error keeps its own line breaks, which means any indentation written here
  // would reach the reader as a line break they did not ask for.
  const sentence = `was written but failed ${n === 1 ? "a check" : `${n} checks`}, `
    + `so its pages were not kept. Ingest again once the source is fixed.`;
  return `<div class="detail hint error run-error">${unitKeyHTML(key)}<span>${sentence}</span></div>
    <details class="run-units" data-keep="err:${esc(r.id)}">
      <summary>what failed the check</summary>
      <div class="detail mono">${esc(bad[2])}</div>
    </details>`;
}

// A real <progress>: it is announced to screen readers as a progress bar with
// its value, which a styled div is not.
const runProgress = (r) => {
  if (r.status !== "running" || !r.unitsTotal) return "";
  const done = r.unitsDone || 0;
  const left = (r.unitsPending || 0) + (r.unitsRunning || 0);
  return `<div class="detail run-progress">
    <progress max="${r.unitsTotal}" value="${done}"
      aria-label="Ingest progress: ${done} of ${r.unitsTotal} units done"></progress>
    <span class="hint">${left
      ? `${left} still to go${r.unitsRunning ? `, ${r.unitsRunning} being written now` : ""}`
      : "finishing up"}</span>
  </div>`;
};

// A run's shape at a glance: how much of its plan is written (ember), how much
// is still to go (ember-soft), and the trough behind both. The old row said
// what a run cost but not how much of it had happened.
//
// It deliberately does NOT claim to show the content-hash gate: how many units
// the gate spared is not on the run row, and a segment invented for it would be
// a number about kiln's central economic claim that nothing measured.
const runMeter = (r) => {
  const total = r.unitsTotal || 0;
  if (!total) return "";
  const done = r.unitsDone || 0;
  const left = (r.unitsPending || 0) + (r.unitsRunning || 0);
  return `<div class="tl-meter">
    <div class="bar">
      <span class="fill-ember" data-w="${Math.round(done / total * 100)}"></span>
      <span class="fill-soft" data-w="${Math.round(left / total * 100)}"></span>
    </div>
    <span class="bar-note">${done} of ${total} unit${total === 1 ? "" : "s"} written</span>
  </div>`;
};

const runRow = (r, activeUnits) => `
  <div class="tl-row">
    <div class="tl-when" title="${esc(r.created)}">${esc(relTime(r.created))}</div>
    <div class="tl-rail" aria-hidden="true">
      <span class="tl-dot run-${esc(r.status)}"></span><span class="tl-line"></span>
    </div>
    <div class="tl-body">
      <div class="tl-top">
        <strong>${esc(runOutcome(r))}</strong>
        ${r.ref ? `<span class="mono">${esc(r.ref)}</span>` : ""}
        ${r.trigger && r.trigger !== "manual" ? (() => {
          const known = triggerChip[r.trigger];
          // .chip.help is dashed and cursor:help -- CSS that promises a hover
          // reveals something. With no explanation to give, the plain chip is
          // the honest one: the label still says what started the run.
          if (!known) return `<span class="chip">${esc(r.trigger)}</span>`;
          const [label, why] = known;
          return `<span class="chip help" title="${esc(why)}">${esc(label)}</span>`;
        })() : ""}
        <span class="tl-cost">${(() => {
          // A live run's total climbs with its units; a finished one reports
          // what the run row settled. Both are the same question asked at
          // different moments, so they render in the same place.
          const t = r.status === "running" ? liveTokens(activeUnits) : (Number(r.tokens) || 0);
          const parts = [];
          if (t > 0) parts.push(esc(humanTokens(t)));
          if (r.costUsd > 0) parts.push(money(r.costUsd));
          return parts.join(" · ");
        })()}</span>
      </div>
      ${r.status === "running" ? runProgress(r) : runMeter(r)}
      ${runErrorHTML(r)}
      ${r.status === "running" ? liveUnits(activeUnits) : ""}
      ${r.status !== "running" && r.costUsd > 0
        ? `<details class="run-units" data-run-items="${esc(r.id)}" data-keep="items:${esc(r.id)}">
        <summary>tokens and cost by unit</summary>
        <div class="detail">loading…</div>
      </details>` : ""}
    </div>
  </div>`;

// renderRunsHTML builds the whole feed as one timeline: newest at the top, the
// relative time in its own gutter, a connector line down the rail. The day
// headers are gone -- the gutter says when, on every row.
function renderRunsHTML(runs, activeUnits) {
  if (!runs.length) {
    return `<div class="empty">No runs yet. Press Ingest now, or build from the
      CLI with <span class="mono">kiln build</span>.</div>`;
  }
  return runs.map((r) => runRow(r, activeUnits)).join("");
}

// fetchRunFeed reads what the feed needs and nothing else: the runs, plus the
// units of the one that is running. A failed item fetch still yields a feed --
// the run rows carry their own counts.
async function fetchRunFeed(ws) {
  const runs = await api(`/workspaces/${ws}/runs?limit=10`);
  const active = runs.find((r) => r.status === "running");
  let activeUnits = [];
  if (active) {
    try {
      activeUnits = await api(`/workspaces/${ws}/runs/${encodeURIComponent(active.id)}/items`);
    } catch (err) {
      if (err?.handled) throw err;
    }
  }
  return { runs, activeUnits };
}

// wireRunItems attaches the lazy per-unit cost loader. Scoped to a root so the
// poll can re-wire the feed it just replaced without touching the rest of the
// page.
function wireRunItems(root, ws) {
  for (const d of root.querySelectorAll("[data-run-items]")) {
    d.addEventListener("toggle", async () => {
      if (!d.open || d.dataset.loaded) return;
      d.dataset.loaded = "true";
      const box = d.querySelector(".detail");
      try {
        const items = await api(`/workspaces/${ws}/runs/${encodeURIComponent(d.dataset.runItems)}/items`);
        box.innerHTML = items.map((it) => `<div class="row">
            ${unitKeyHTML(it.key)}${it.status !== "succeeded" ? (() => {
              // The same words the live list uses. This branch used to print
              // the queue's own enum -- "deferred", "over_budget" -- which is
              // a status name, not a status. The fallback differs from the
              // live list's: everything here is known not to have succeeded,
              // so "done" would be a lie about a status we do not recognize.
              const [label, cls] = unitLabel[it.status] || ["did not finish", "run-failed"];
              return `<span class="chip ${cls}">${esc(label)}</span>`;
            })() : ""}
            <span>
              ${it.tokens > 0 ? `<span class="count">${esc(humanTokens(it.tokens))}</span>` : ""}
              <span class="count">${money(it.costUsd)}${it.estCostUsd ? ` (est ${money(it.estCostUsd)})` : ""}</span>
            </span>
          </div>`).join("") || "no unit records";
      } catch (err) {
        if (!err.handled) box.textContent = err.message;
      }
    });
  }
}

// pollRuns keeps the run feed live without re-rendering the view. Each tick
// swaps #run-feed alone and updates the two lines outside it that depend on run
// state, so scroll position, focus, open sections and the document list all
// survive -- the reader is scrolled down here precisely because something is
// moving, and rebuilding the view would throw them back to the top every five
// seconds. Returns { now, soon } so a queued run can refresh the feed
// immediately without a page bounce. The timer is registered for view teardown
// so leaving the page cannot leave one ticking.
function pollRuns(ws, view, connectors) {
  let timer = 0;
  onViewCleanup(() => clearTimeout(timer));
  const soon = () => { clearTimeout(timer); timer = setTimeout(tick, 5000); };

  async function tick() {
    if (!view.current()) return;
    const feed = $("run-feed");
    if (!feed) return;
    let runs, activeUnits;
    try {
      ({ runs, activeUnits } = await fetchRunFeed(ws));
    } catch (err) {
      if (err?.handled) return;
      // A blip is not worth tearing the feed down over -- the numbers on
      // screen are still the last true ones. Try again next tick.
      soon();
      return;
    }
    if (!view.current() || !feed.isConnected) return;

    // Anything the reader opened is a question they asked, and a tick is not
    // an answer to it. Re-opening a cost breakdown re-fires the toggle that
    // lazily fills it.
    const open = [...feed.querySelectorAll("[data-keep]")]
      .filter((d) => d.open).map((d) => d.dataset.keep);
    feed.innerHTML = renderRunsHTML(runs, activeUnits);
    sizeBars(feed);
    wireRunItems(feed, ws);
    for (const d of feed.querySelectorAll("[data-keep]")) {
      if (open.includes(d.dataset.keep)) d.open = true;
    }

    const active = runs.some((r) => r.status === "queued" || r.status === "running");
    const note = $("next-build");
    if (note) {
      note.innerHTML = runningNow(runs)
        || esc(nextBuildLine(runs, connectors, state.sourcePollIntervalSeconds || 0));
    }
    const btn = $("sources-build");
    if (btn) {
      btn.disabled = active;
      // The button carries its own label now, so only the reason it is
      // unavailable needs saying -- in the tooltip, not over the visible text.
      btn.title = active ? "An ingest is already queued or running" : "Ingest now";
    }
    // The top bar reads the same feed; hand it what was just fetched rather
    // than asking the server for it a second time.
    adoptRunFeed(runs, activeUnits);
    // A finished run stops the poll: the feed is settled until someone acts.
    if (active) soon();
  }

  return { now: tick, soon };
}

async function showSources() {
  const view = beginView("Ingest", "sources");
  const ws = encodeURIComponent(state.workspace);
  try {
    // Connectors and credentials are owner-gated; files and runs are readable
    // by anyone who can read the wiki. Fetch all four and let a 403 downgrade
    // the management half rather than blank the page.
    const [connRes, credRes, filesRes, runsRes] = await Promise.allSettled([
      api(`/workspaces/${ws}/connectors`),
      api(`/workspaces/${ws}/credentials`),
      api(`/workspaces/${ws}/files`),
      api(`/workspaces/${ws}/runs?limit=10`),
    ]);
    for (const r of [connRes, credRes, filesRes, runsRes]) {
      if (r.status === "rejected" && r.reason?.handled) return;
    }
    const canAdmin = connRes.status === "fulfilled";
    const connectors = canAdmin ? connRes.value : [];
    const credentials = credRes.status === "fulfilled" ? credRes.value : [];
    const files = filesRes.status === "fulfilled" ? filesRes.value : [];
    const filesErr = filesRes.status === "rejected" ? filesRes.reason.message : "";
    const runs = runsRes.status === "fulfilled" ? [...runsRes.value] : [];
    const buildActive = runs.some((r) => r.status === "queued" || r.status === "running");
    const uploadConn = connectors.find((c) => c.kind === "upload");

    // A running build's units are fetched eagerly and shown inline rather than
    // behind the expander finished runs use. Two reasons: this is the one case
    // where the list answers "what is happening and what is left" rather than
    // "what did it cost", and the view re-renders every five seconds while a
    // build is live, which would collapse an expander the reader had opened.
    const active = runs.find((r) => r.status === "running");
    let activeUnits = [];
    if (active) {
      try {
        activeUnits = await api(`/workspaces/${ws}/runs/${encodeURIComponent(active.id)}/items`);
      } catch (err) {
        if (err?.handled) return;
        // The run row still renders with its counts; only the unit list is lost.
      }
    }

    // Each source reads as a sentence, not a row of internals: what it is,
    // where it points, when it ingests, and when it was last read.
    const kindLabel = { git: "repository", web: "web pages", upload: "documents" };
    const triggerPhrase = {
      manual: "ingests when you press Ingest now",
      webhook: "ingests automatically on pushes",
      poll: "re-checked on a schedule",
    };
    // The trigger clause is a verb phrase inside a sentence, not a label. A
    // mode we have no phrase for drops its clause rather than dropping the
    // raw value into the middle of one -- "poll; last read 3 days ago" is not
    // a sentence. What is left is still true and still answers "when was this
    // last read", which is the half a reader actually came for.
    const connectorWhen = (c) => {
      const read = c.lastSynced ? `last read ${relTime(c.lastSynced)}` : "not read yet";
      const phrase = triggerPhrase[c.triggerMode];
      return phrase ? `${phrase}; ${read}.` : `${read[0].toUpperCase()}${read.slice(1)}.`;
    };
    // The health line is what the server actually knows: whether the source is
    // enabled, whether the last read failed, and when it happened. There is no
    // per-source unit count on the API, so the bar carries state rather than a
    // proportion -- full when a source is reading cleanly, empty when it is
    // paused, oxide when the last read failed.
    const SRC_ICONS = { git: iconRepo, web: iconWeb, upload: iconDoc };
    const connectorHealth = (c) => {
      if (!c.enabled) return { pct: 0, cls: "fill-soft", note: "Paused — skipped on ingest" };
      if (c.lastError) return { pct: 100, cls: "fill-err", note: "Last read failed" };
      if (!c.lastSynced) return { pct: 0, cls: "fill-soft", note: "Not read yet" };
      return { pct: 100, cls: "fill-ember", note: "Read cleanly" };
    };

    const connectorRow = (c) => {
      const h = connectorHealth(c);
      return `
      <div class="src-row" data-connector-row="${esc(c.id)}">
        <span class="src-icon kind-${esc(c.kind)}">${SRC_ICONS[c.kind] || iconDoc}</span>
        <div class="src-main">
          <div class="src-name">
            <strong>${esc(c.name)}</strong>
            <span class="chip">${esc(kindLabel[c.kind] || c.kind)}</span>
            ${c.enabled ? "" : `<span class="chip">paused</span>`}
          </div>
          <div class="src-where" title="${esc(connectorSummary(c))}">${esc(connectorSummary(c))}</div>
        </div>
        <div class="src-health">
          <div class="bar"><span class="${h.cls}" data-w="${h.pct}"></span></div>
          <div class="bar-note">${esc(h.note)}</div>
        </div>
        <span class="src-when" title="${esc(connectorWhen(c))}">${esc(
          c.lastSynced ? relTime(c.lastSynced) : "never")}</span>
        <div class="src-tools">
          <button class="sq-btn" data-conn-toggle="${esc(c.id)}" data-enabled="${c.enabled}"
            aria-label="${c.enabled ? "Pause" : "Resume"} ${esc(c.name)}" title="${c.enabled ? "pause" : "resume"}">
            ${c.enabled ? iconPause : iconPlay}</button>
          <button class="sq-btn rm" data-conn-delete="${esc(c.id)}"
            aria-label="Delete ${esc(c.name)}" title="delete">${iconTrash}</button>
        </div>
      </div>
      ${c.lastError ? `<div class="src-err">${esc(c.lastError)}</div>` : ""}`;
    };

    const fileRow = (f) => `
      <div class="src-row ${f.enabled === false ? "file-paused" : ""}" data-file-row="${esc(f.id)}">
        <span class="src-icon kind-upload">${iconDoc}</span>
        <div class="src-main">
          <div class="src-name">
            <strong>${esc(f.name || f.path.split("/").pop())}</strong>
            <span class="chip">${esc(humanBytes(f.size))}</span>
            ${f.enabled === false ? `<span class="chip">paused</span>` : ""}
          </div>
          <!-- The filename it arrived as, kept where the path already was:
               the label above is derived, and the one question it cannot
               answer is "which upload is this" when someone is looking for the
               copy they replaced. -->
          <div class="src-where" title="${esc(f.path)}">${esc(f.path)}</div>
        </div>
        <span class="src-when">${timeTag(f.updated)}</span>
        <div class="src-tools">
          <button class="sq-btn" data-file-toggle="${esc(f.id)}" data-enabled="${f.enabled !== false}"
            aria-label="${f.enabled === false ? "Resume" : "Pause"} ${esc(f.path)}"
            title="${f.enabled === false ? "resume" : "pause"}">
            ${f.enabled === false ? iconPlay : iconPause}</button>
          <button class="sq-btn rm" data-file-delete="${esc(f.id)}"
            aria-label="Delete ${esc(f.path)}" title="delete">${iconTrash}</button>
        </div>
      </div>`;

    // Spend is summed over the runs actually on screen rather than over a
    // window the API does not report, and the note says which -- a "7 days"
    // label over ten runs would be a number nothing measured.
    const spend = runs.reduce((n, r) => n + (Number(r.costUsd) || 0), 0);
    const stat = (label, value, note) => `<div class="stat">
      <div class="stat-label">${esc(label)}</div>
      <div class="stat-value">${esc(value)}</div>
      <div class="stat-note">${esc(note)}</div>
    </div>`;
    const liveSources = connectors.filter((c) => c.enabled).length;

    if (!view.done(`
      <div class="head-row">
        ${viewHead("Ingest", "ingest")}
        <div class="head-actions">
          ${canAdmin ? `<button class="btn quiet" id="sources-add">${iconPlus}Add source</button>` : ""}
          <button class="btn" id="sources-build" ${buildActive ? "disabled" : ""}
            title="${buildActive ? "An ingest is already queued or running" : "Ingest now"}">${iconIngest}Ingest now</button>
        </div>
      </div>
      <p class="head-note" id="next-build">${runningNow(runs)
        || esc(nextBuildLine(runs, connectors, state.sourcePollIntervalSeconds || 0))}</p>
      <div class="meta"><span class="hint" id="sources-build-note" role="status"></span></div>

      <div class="stats n4">
        ${stat("Pages", String(state.pages.length), "in this bench's wiki")}
        ${stat("Sources", String(canAdmin ? connectors.length : "—"),
          canAdmin ? `${liveSources} enabled` : "needs an owner to see")}
        ${stat("Documents", String(files.length), files.length ? "uploaded to this bench" : "none uploaded yet")}
        ${stat("Spend", `$${spend.toFixed(2)}`,
          runs.length ? `across the last ${runs.length} run${runs.length === 1 ? "" : "s"}` : "no runs yet")}
      </div>

      <div id="connector-note" class="hint" role="status"></div>

      <div class="sec-head">
        <div class="group-label">Sources</div>
        <span class="gloss">— what this bench reads</span>
      </div>
      <div class="card rows">
        ${canAdmin
          ? (connectors.map(connectorRow).join("") ||
             `<div class="empty card-empty">No sources connected yet. Add one with the button above.</div>`)
          : `<div class="empty card-empty">Managing sources needs an org owner or an
             instance admin. You can still add documents below if your role allows.</div>`}
      </div>

      <div class="sec-head">
        <div class="group-label">Documents</div>
        <span class="gloss">— uploaded straight from this browser${
          uploadConn && !uploadConn.enabled ? "; paused, so skipped on ingest" : ""}</span>
      </div>
      <div id="upload-progress" class="hint" role="status"></div>
      <div class="card rows" id="file-drop" aria-label="Uploaded documents; drop files to add more">
        ${filesErr ? `<div class="empty card-empty">${esc(filesErr)}</div>`
          : files.map(fileRow).join("") ||
            `<div class="empty card-empty">No documents yet. Add markdown, PDFs, Office
             files, or HTML with the button above, or drop files anywhere on this card.</div>`}
      </div>

      <div class="sec-head">
        <div class="group-label">Run timeline</div>
        <span class="gloss">— every run, what it cost, what it changed</span>
      </div>
      <div class="card timeline" id="run-feed">${renderRunsHTML(runs, activeUnits)}</div>`)) return;

    sizeBars($("main"));

    // Per-unit cost attribution, fetched lazily on first expand: where the
    // money went, costliest unit first, estimate beside actual.
    wireRunItems(document, ws);

    // The feed refreshes itself from here on: on a timer while a run moves,
    // and on demand when this page queues one.
    const refreshRuns = pollRuns(ws, view, connectors);

    // ---- actions ------------------------------------------------------------
    const buildNote = (msg, isErr) => {
      const n = $("sources-build-note");
      if (n) { n.textContent = msg; n.classList.toggle("error", Boolean(isErr)); }
    };
    const addBtn = $("sources-add");
    if (addBtn) addBtn.addEventListener("click", () => openSourceWizard({
      ws, credentials,
      hasUpload: connectors.some((c) => c.kind === "upload" && c.enabled),
      refresh: () => { if (view.current()) showSources(); },
    }));
    // Not once(): this button's disabled state means "a run is active", which
    // outlives the click, and once() hands the button back on the way out. It
    // is wired even when it starts disabled, because the poll re-enables it in
    // place when the run finishes.
    const buildBtn = $("sources-build");
    if (buildBtn) buildBtn.addEventListener("click", async () => {
      if (buildBtn.disabled) return;
      buildBtn.disabled = true;
      try {
        const res = await api(`/workspaces/${ws}/runs`, { method: "POST", body: {} });
        toast(res.created ? "Run queued" : "A run was already waiting — joined it");
        // The run appears in Recent runs below; no page bounce. The refresh
        // settles the button to match whatever the queue now says.
        await refreshRuns.now();
      } catch (err) {
        buildBtn.disabled = false;
        if (!err.handled) buildNote(err.message, true);
      }
    });

    // ---- connector actions --------------------------------------------------
    const connNote = (msg, isErr) => {
      const n = $("connector-note");
      if (n) { n.textContent = msg; n.classList.toggle("error", Boolean(isErr)); }
    };
    for (const b of document.querySelectorAll("[data-conn-toggle]")) {
      once(b, async () => {
        try {
          await api(`/workspaces/${ws}/connectors/${encodeURIComponent(b.dataset.connToggle)}`,
            { method: "PATCH", body: { enabled: b.dataset.enabled !== "true" } });
          showSources();
        } catch (err) {
          if (!err.handled) connNote(err.message, true);
        }
      });
    }
    for (const b of document.querySelectorAll("[data-conn-delete]")) {
      once(b, async () => {
        if (!armButton(b, "delete")) return;
        try {
          await api(`/workspaces/${ws}/connectors/${encodeURIComponent(b.dataset.connDelete)}`,
            { method: "DELETE" });
          toast("Source removed — its pages stay until a deletion is approved in Reviews");
          showSources();
        } catch (err) {
          if (!err.handled) connNote(err.message, true);
        }
      });
    }

    // ---- uploads ------------------------------------------------------------
    // The file list itself is the drop target: no standing dropzone chrome,
    // since the Add flow covers the click path. Dragging anything over the
    // list lights it up.
    const drop = $("file-drop");
    drop.addEventListener("dragover", (e) => {
      e.preventDefault();
      drop.classList.add("over");
    });
    drop.addEventListener("dragleave", () => drop.classList.remove("over"));
    drop.addEventListener("drop", (e) => {
      e.preventDefault();
      drop.classList.remove("over");
      uploadWorkspaceFiles(ws, e.dataTransfer.files, $("upload-progress"), () => {
        if (view.current()) showSources();
      });
    });

    for (const b of document.querySelectorAll("[data-file-toggle]")) {
      once(b, async () => {
        try {
          await api(`/workspaces/${ws}/files/${encodeURIComponent(b.dataset.fileToggle)}`,
            { method: "PATCH", body: { enabled: b.dataset.enabled !== "true" } });
          showSources();
        } catch (err) {
          if (!err.handled) {
            const n = $("upload-progress");
            if (n) { n.textContent = err.message; n.classList.add("error"); }
          }
        }
      });
    }
    for (const b of document.querySelectorAll("[data-file-delete]")) {
      once(b, async () => {
        if (!armButton(b, "delete")) return;
        try {
          await api(`/workspaces/${ws}/files/${encodeURIComponent(b.dataset.fileDelete)}`,
            { method: "DELETE" });
          toast("Document removed — derived pages await deletion review after the next build");
          showSources();
        } catch (err) {
          if (!err.handled) {
            const n = $("upload-progress");
            if (n) { n.textContent = err.message; n.classList.add("error"); }
          }
        }
      });
    }

    // Live-ish while something is moving. The poll touches the run feed and
    // the two lines that depend on it, never the whole view: re-rendering the
    // page would scroll the reader back to the top every five seconds, and
    // they are down here watching the run precisely because it is moving.
    if (buildActive) refreshRuns.soon();
  } catch (err) {
    if (!err.handled) view.done(banner(err));
  }
}

// ---- first run ---------------------------------------------------------------
// A bench with no pages has nothing to read, so the reading views are all
// dead ends: before this, a brand-new bench greeted its creator with "Run
// kiln build <path>" -- a CLI instruction to someone who just made the bench
// in a browser. This replaces that with the actual next step.

// benchProgress reads where the bench actually is, so the checklist reflects
// state rather than assuming a happy path.
async function benchProgress(ws) {
  const [connRes, filesRes, runsRes] = await Promise.allSettled([
    api(`/workspaces/${ws}/connectors`),
    api(`/workspaces/${ws}/files`),
    api(`/workspaces/${ws}/runs?limit=5`),
  ]);
  for (const r of [connRes, filesRes, runsRes]) {
    if (r.status === "rejected" && r.reason?.handled) return null;
  }
  const connectors = connRes.status === "fulfilled" ? connRes.value : [];
  const files = filesRes.status === "fulfilled" ? filesRes.value : [];
  const runs = runsRes.status === "fulfilled" ? runsRes.value : [];
  // An upload connector with no documents is plumbing, not a source: it
  // ingests nothing, so counting it would mark the step done prematurely.
  const realSources = connectors.filter(
    (c) => c.enabled && (c.kind !== "upload" || files.length > 0));
  return {
    canAdmin: connRes.status === "fulfilled",
    hasSource: realSources.length > 0,
    sourceCount: realSources.length,
    files,
    connectors,
    runs,
    active: runs.some((r) => r.status === "queued" || r.status === "running"),
    everRan: runs.some((r) => r.status !== "queued"),
  };
}

const stepIcon = (state) =>
  state === "done" ? `<span class="step-mark done" aria-hidden="true">✓</span>`
  : state === "now" ? `<span class="step-mark now" aria-hidden="true">→</span>`
  : `<span class="step-mark" aria-hidden="true">•</span>`;

// showGetStarted is the overview of a bench that has not been built yet: a
// checklist that knows which step you are on, with the button for that step
// right there rather than a page away.
async function showGetStarted() {
  const view = beginView("Get started", "overview");
  const ws = encodeURIComponent(state.workspace);
  try {
    const p = await benchProgress(ws);
    if (!p) return;

    const sourceState = p.hasSource ? "done" : "now";
    const ingestState = !p.hasSource ? "todo" : (p.active || p.everRan ? "done" : "now");
    const readState = "todo";

    const sourceDetail = p.hasSource
      ? `${p.sourceCount} source${p.sourceCount === 1 ? "" : "s"} connected`
      : "A repository, web pages, or documents you upload.";
    // The first ingest is the longest wait a bench ever imposes, and it is
    // watched from here on an otherwise empty page. Saying how far along it is
    // costs one number and answers the only question the reader has.
    const running = p.runs.find((r) => r.status === "running");
    // A run in motion is drawn rather than described here too, so the first
    // ingest -- the longest wait a bench ever imposes, watched from an
    // otherwise empty page -- shows the kiln alight instead of a sentence.
    const ingestDetail = p.active
      ? "This page becomes your wiki when it finishes."
      : p.everRan
        ? "Ingested. If no pages appeared, check Ingest for what the run reported."
        : "Reads every source and writes the pages. Only changed sources cost anything.";
    const ingestBar = running && running.unitsTotal
      ? `<div class="run-progress">
          <progress max="${running.unitsTotal}" value="${running.unitsDone || 0}"
            aria-label="Ingest progress: ${running.unitsDone || 0} of ${running.unitsTotal} units done"></progress>
        </div>`
      : "";

    if (!view.done(`<h1>Get started</h1>
      <p class="hint">A bench compiles a wiki from the sources you connect, and
      keeps it current as they change. Three steps, then it reads itself.</p>

      <ol class="steps">
        <li class="step done">
          ${stepIcon("done")}
          <div><strong>Bench created</strong>
          <span class="hint">${esc(state.workspace)}</span></div>
        </li>
        <li class="step ${sourceState}">
          ${stepIcon(sourceState)}
          <div><strong>Add a source</strong>
            <span class="hint">${esc(sourceDetail)}</span>
            ${p.canAdmin
              ? `<div class="meta"><button class="btn icon-btn" id="gs-add"
                   aria-label="Add a source" title="Add a source">${iconPlus}</button></div>`
              : `<div class="hint">Ask an owner of this bench's org to connect one.</div>`}
          </div>
        </li>
        <li class="step ${ingestState}">
          ${stepIcon(ingestState)}
          <div><strong>Ingest</strong>
            ${p.active ? runningNow(p.runs) : ""}
            <span class="hint">${esc(ingestDetail)}</span>
            ${ingestBar}
            ${p.hasSource && !p.active
              ? `<div class="meta"><button class="btn ${p.everRan ? "quiet" : ""} icon-btn" id="gs-ingest"
                   aria-label="Ingest now" title="Ingest now">${iconIngest}</button>
                 <span class="hint" id="gs-note" role="status"></span></div>`
              : ""}
          </div>
        </li>
        <li class="step ${readState}">
          ${stepIcon(readState)}
          <div><strong>Read the wiki</strong>
          <span class="hint">Pages appear in the sidebar, and this page becomes the overview.</span></div>
        </li>
      </ol>

      <details class="panel">
        <summary>What can a bench read?</summary>
        <div class="detail">
          <p><strong>A repository</strong> — an https remote or a directory the
          worker can reach. Code becomes pages per module, with an architecture
          overview across them.</p>
          <p><strong>Web pages</strong> — https addresses fetched and folded in,
          re-checked on a schedule.</p>
          <p><strong>Documents</strong> — PDFs, Office files, markdown, and HTML
          uploaded from your browser.</p>
          <p class="hint">All three fire into one wiki whose pages link across
          the boundary.</p>
        </div>
      </details>`)) return;

    const addBtn = $("gs-add");
    if (addBtn) addBtn.addEventListener("click", () => openSourceWizard({
      ws,
      credentials: [],
      hasUpload: p.connectors.some((c) => c.kind === "upload" && c.enabled),
      refresh: () => { if (view.current()) showGetStarted(); },
    }));

    const ingestBtn = $("gs-ingest");
    if (ingestBtn) once(ingestBtn, async () => {
      try {
        const res = await api(`/workspaces/${ws}/runs`, { method: "POST", body: {} });
        toast(res.created ? "Ingesting — pages appear as it finishes" : "A run was already waiting — joined it");
        showGetStarted();
      } catch (err) {
        if (err.handled) return;
        const n = $("gs-note");
        if (n) { n.textContent = err.message; n.classList.add("error"); }
      }
    });

    // While a run is moving, keep the checklist honest and pick up the pages
    // the moment they land -- the payoff should not need a manual refresh.
    if (p.active) {
      setTimeout(async () => {
        if (!view.current()) return;
        try {
          const pages = await api(`/workspaces/${ws}/pages`);
          if (pages.length) { location.reload(); return; }
        } catch { /* fall through to another tick */ }
        showGetStarted();
      }, 5000);
    }
  } catch (err) {
    if (!err.handled) view.done(banner(err));
  }
}
