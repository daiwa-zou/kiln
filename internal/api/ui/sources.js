"use strict";
// Sources: where a bench's material is configured. Connector management
// (owners and instance admins) and uploaded documents (any writing member)
// share the view because they answer the same question -- "what does this
// wiki read?" -- at two permission levels. One Add button opens a guided
// flow; the page itself stays a status surface. Loaded before app.js; only
// defines functions, and resolves shared helpers (api, esc, beginView,
// openOverlay…) at call time.

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

// nextBuildLine answers the question every visitor to this page has: when
// does the wiki next catch up with its sources? Priority order matters -- an
// actual run in motion beats any schedule.
function nextBuildLine(runs, connectors, pollSeconds) {
  if (runs.some((r) => r.status === "running")) {
    return "A build is running now.";
  }
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

// uploadWorkspaceFiles pushes a FileList one request at a time, reporting
// progress into progressEl. Shared by the page dropzone and the wizard's.
// Calls onDone(okCount, lastBuildState) when at least one file landed.
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
    onDone?.(ok, lastBuild);
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
      await uploadWorkspaceFiles(ctx.ws, batch, $("upload-progress-wizard"), () => {
        changed = true;
        staged = [];
        renderStaged();
        // The page behind the overlay catches up immediately; the wizard
        // stays open for another batch, or the X closes it.
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

async function showSources() {
  const view = beginView("Ingestion", "sources");
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

    // sec renders one collapsible section. Open is the default; a collapsed
    // choice is read back at render time so re-renders (including the
    // active-run refresh) respect it.
    const sec = (name, labelHTML, bodyHTML) => `
      <details class="sec" data-sec="${name}" ${lsGet(`kiln.ingest.${name}`, true) ? "open" : ""}>
        <summary class="group-label">${labelHTML}</summary>
        ${bodyHTML}
      </details>`;

    // Each source reads as a sentence, not a row of internals: what it is,
    // where it points, when it ingests, and when it was last read.
    const kindLabel = { git: "repository", web: "web pages", upload: "documents" };
    const triggerPhrase = {
      manual: "ingests when you press Ingest now",
      webhook: "ingests automatically on pushes",
      poll: "re-checked on a schedule",
    };
    const connectorRow = (c) => `
      <div class="review" data-connector-row="${esc(c.id)}">
        <div class="meta">
          <span class="chip kind-${esc(c.kind)}">${esc(kindLabel[c.kind] || c.kind)}</span>
          ${c.enabled ? "" : `<span class="chip">paused</span>`}
        </div>
        <strong>${esc(c.name)}</strong>
        <span class="count mono">${esc(connectorSummary(c))}</span>
        <div class="detail">${esc(triggerPhrase[c.triggerMode] || c.triggerMode)}; ${c.lastSynced ? `last read ${esc(relTime(c.lastSynced))}` : "not read yet"}.</div>
        ${c.lastError ? `<div class="detail hint error">${esc(c.lastError)}</div>` : ""}
        <div class="meta">
          <button class="btn quiet icon-btn" data-conn-toggle="${esc(c.id)}" data-enabled="${c.enabled}"
            aria-label="${c.enabled ? "Pause" : "Resume"} ${esc(c.name)}" title="${c.enabled ? "pause" : "resume"}">
            ${c.enabled ? iconPause : iconPlay}</button>
          <button class="btn quiet icon-btn" data-conn-delete="${esc(c.id)}"
            aria-label="Delete ${esc(c.name)}" title="delete">${iconTrash}</button>
        </div>
      </div>`;

    const fileRow = (f) => `
      <div class="row ${f.enabled === false ? "file-paused" : ""}" data-file-row="${esc(f.id)}">
        <span class="mono">${esc(f.path)}
          ${f.enabled === false ? `<span class="chip">paused</span>` : ""}</span>
        <span>
          <span class="count">${esc(humanBytes(f.size))}</span>
          ${timeTag(f.updated)}
          <button class="btn quiet icon-btn" data-file-toggle="${esc(f.id)}" data-enabled="${f.enabled !== false}"
            aria-label="${f.enabled === false ? "Resume" : "Pause"} ${esc(f.path)}"
            title="${f.enabled === false ? "resume" : "pause"}">
            ${f.enabled === false ? iconPlay : iconPause}</button>
          <button class="btn quiet icon-btn" data-file-delete="${esc(f.id)}"
            aria-label="Delete ${esc(f.path)}" title="delete">${iconTrash}</button>
        </span>
      </div>`;

    const money = (v) => `$${Number(v || 0).toFixed(2)}`;
    // Trigger values are queue internals; runs started by hand carry no
    // chip at all (that is the normal case), automatic ones say why.
    const triggerLabel = {
      webhook: "repo push",
      upload: "documents changed",
      poll: "scheduled check",
      continuation: "finishing the previous run",
    };
    // Each run reads as one sentence about what happened, not a ledger row.
    const runOutcome = (r) => {
      if (r.status === "queued") return "Waiting to start";
      if (r.status === "running") return "Ingesting now…";
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
    const runRow = (r) => `
      <div class="row" title="${esc(r.created)}">
        <span><span class="run-dot run-${esc(r.status)}" aria-hidden="true"></span>${esc(runOutcome(r))}</span>
        <span>
          ${r.trigger && r.trigger !== "manual" ? `<span class="chip">${esc(triggerLabel[r.trigger] || r.trigger)}</span>` : ""}
          ${r.ref ? `<span class="count mono">${esc(r.ref)}</span>` : ""}
          ${r.costUsd > 0 ? `<span class="count">${money(r.costUsd)}</span>` : ""}
        </span>
      </div>
      ${r.error ? `<div class="detail hint error">${esc(r.error)}</div>` : ""}
      ${r.costUsd > 0 ? `<details class="run-units" data-run-items="${esc(r.id)}">
        <summary>cost by unit</summary>
        <div class="detail">loading…</div>
      </details>` : ""}`;
    // Runs group under day headers, newest first; the repeated time chips go.
    const runGroups = [];
    for (const r of runs) {
      const label = relTime(r.created);
      if (!runGroups.length || runGroups[runGroups.length - 1].label !== label) {
        runGroups.push({ label, runs: [] });
      }
      runGroups[runGroups.length - 1].runs.push(r);
    }
    const runsHTML = runGroups.map((g) => `
      <div class="run-day">${esc(g.label)}</div>
      <div class="review run-list">${g.runs.map(runRow).join("")}</div>`).join("");

    if (!view.done(`<h1>Ingestion</h1>
      <p class="hint">What this bench reads and when it read it: a repository,
      web pages, and uploaded documents all fire into one wiki. Pausing a source
      skips it without touching the pages it already produced.</p>
      <div class="meta sources-actions">
        ${canAdmin ? `<button class="btn" id="sources-add">+ Add source</button>` : ""}
        <button class="btn ${canAdmin ? "quiet" : ""}" id="sources-build" ${buildActive ? "disabled" : ""}>
          ${buildActive ? "Ingestion is queued or running" : "Ingest now"}</button>
        <span class="hint" id="sources-build-note" role="status"></span>
      </div>
      <p class="hint" id="next-build">${esc(nextBuildLine(runs, connectors, state.sourcePollIntervalSeconds || 0))}</p>

      <div id="connector-note" class="hint" role="status"></div>
      ${canAdmin
        ? sec("repos", "Repositories",
            connectors.filter((c) => c.kind === "git").map(connectorRow).join("") ||
              `<div class="empty">No repository connected yet. Add one with + Add source.</div>`) +
          sec("web", "Web pages",
            connectors.filter((c) => c.kind === "web").map(connectorRow).join("") ||
              `<div class="empty">No web pages connected yet. Add some with + Add source.</div>`)
        : sec("repos", "Repositories &amp; web pages",
            `<div class="empty">Managing sources needs an org owner or an instance
             admin. You can still add documents below if your role allows.</div>`)}

      ${sec("docs",
        `Documents${uploadConn && !uploadConn.enabled ? ` <span class="chip">paused — skipped on ingest</span>` : ""}`,
        `<div id="upload-progress" class="hint" role="status"></div>
         <div class="review" id="file-drop" aria-label="Uploaded documents; drop files to add more">
           ${filesErr ? `<div class="empty">${esc(filesErr)}</div>`
             : files.map(fileRow).join("") ||
               `<div class="empty">No documents yet. Add markdown, PDFs, Office
                files, or HTML with + Add source, or drop files anywhere on this card.</div>`}
         </div>`)}

      ${sec("runs", "Recent runs",
        `<p class="hint">Each ingest regenerates only the pages whose sources
         changed; an unchanged bench incurs no cost. Runs execute one at a time
         per bench.</p>
         ${runsHTML ||
           `<div class="empty">No runs yet. Press Ingest now, or build from the
            CLI with <span class="mono">kiln build</span>.</div>`}`)}`)) return;

    // Collapsed/expanded choices persist across renders and visits -- the
    // 5-second active-run refresh must not spring sections back open.
    for (const d of document.querySelectorAll("details.sec")) {
      d.addEventListener("toggle", () =>
        lsSet(`kiln.ingest.${d.dataset.sec}`, d.open));
    }

    // Per-unit cost attribution, fetched lazily on first expand: where the
    // money went, costliest unit first, estimate beside actual.
    for (const d of document.querySelectorAll("[data-run-items]")) {
      d.addEventListener("toggle", async () => {
        if (!d.open || d.dataset.loaded) return;
        d.dataset.loaded = "true";
        const box = d.querySelector(".detail");
        try {
          const items = await api(`/workspaces/${ws}/runs/${encodeURIComponent(d.dataset.runItems)}/items`);
          box.innerHTML = items.map((it) => `<div class="row">
              <span class="mono">${esc(it.key)}${it.status !== "succeeded" ? ` <span class="chip run-failed">${esc(it.status)}</span>` : ""}</span>
              <span class="count">${money(it.costUsd)}${it.estCostUsd ? ` (est ${money(it.estCostUsd)})` : ""}</span>
            </div>`).join("") || "no unit records";
        } catch (err) {
          if (!err.handled) box.textContent = err.message;
        }
      });
    }

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
    const buildBtn = $("sources-build");
    if (buildBtn && !buildBtn.disabled) once(buildBtn, async () => {
      try {
        const res = await api(`/workspaces/${ws}/runs`, { method: "POST", body: {} });
        toast(res.created ? "Run queued" : "A run was already waiting — joined it");
        // The run appears in Recent runs below; no page bounce.
        showSources();
      } catch (err) {
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

    // Live-ish while something is moving: the next-ingest line and the run
    // cards refresh together on a short leash, guarded by the nav token so
    // leaving the view stops the poll.
    if (buildActive) setTimeout(() => { if (view.current()) showSources(); }, 5000);
  } catch (err) {
    if (!err.handled) view.done(banner(err));
  }
}
