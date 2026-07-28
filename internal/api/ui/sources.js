"use strict";
// Sources: where a bench's material is configured. Connector management
// (owners and instance admins) and uploaded documents (any writing member)
// share the view because they answer the same question -- "what does this
// wiki read?" -- at two permission levels. Loaded before app.js; only defines
// functions, and resolves shared helpers (api, esc, beginView…) at call time.

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

// armButton is the shared two-step destructive confirm: first click arms,
// second within 4s commits. Returns true when the click should proceed.
function armButton(b, label) {
  if (b.dataset.armed === "true") return true;
  b.dataset.armed = "true";
  b.textContent = `confirm ${label}`;
  b.classList.add("danger");
  setTimeout(() => {
    if (b.isConnected) {
      b.dataset.armed = "false";
      b.textContent = label;
      b.classList.remove("danger");
    }
  }, 4000);
  return false;
}

async function showSources() {
  const view = beginView("Sources", "sources");
  const ws = encodeURIComponent(state.workspace);
  try {
    // Connectors and credentials are owner-gated; files are readable by
    // anyone who can read the wiki. Fetch all three and let a 403 downgrade
    // the connector half rather than blank the page.
    const [connRes, credRes, filesRes] = await Promise.allSettled([
      api(`/workspaces/${ws}/connectors`),
      api(`/workspaces/${ws}/credentials`),
      api(`/workspaces/${ws}/files`),
    ]);
    for (const r of [connRes, credRes, filesRes]) {
      if (r.status === "rejected" && r.reason?.handled) return;
    }
    const canAdmin = connRes.status === "fulfilled";
    const connectors = canAdmin ? connRes.value : [];
    const credentials = credRes.status === "fulfilled" ? credRes.value : [];
    const files = filesRes.status === "fulfilled" ? filesRes.value : [];
    const filesErr = filesRes.status === "rejected" ? filesRes.reason.message : "";

    const connectorRow = (c) => `
      <div class="review" data-connector-row="${esc(c.id)}">
        <div class="meta">
          <span class="chip kind-${esc(c.kind)}">${esc(c.kind)}</span>
          ${c.enabled ? "" : `<span class="chip">disabled</span>`}
          <span class="chip">${esc(c.triggerMode)}</span>
          ${c.lastSynced ? timeTag(c.lastSynced, "synced ") : `<span class="count">never synced</span>`}
        </div>
        <strong>${esc(c.name)}</strong>
        <span class="count mono">${esc(connectorSummary(c))}</span>
        ${c.lastError ? `<div class="detail hint error">${esc(c.lastError)}</div>` : ""}
        <div class="meta">
          <button class="btn quiet" data-conn-toggle="${esc(c.id)}" data-enabled="${c.enabled}">
            ${c.enabled ? "disable" : "enable"}</button>
          <button class="btn quiet" data-conn-delete="${esc(c.id)}">delete</button>
        </div>
      </div>`;

    const credOptions = credentials.map((cr) =>
      `<option value="${esc(cr.id)}">${esc(cr.kind)} · ${esc((cr.created || "").slice(0, 10))}</option>`).join("");

    const addForms = `
      <details class="panel">
        <summary>Add a repository</summary>
        <div class="detail">
          <label class="hint" for="git-name">Name</label>
          <input id="git-name" placeholder="code" autocomplete="off" spellcheck="false">
          <label class="hint" for="git-url">HTTPS remote (or a server path under the worker's permitted roots)</label>
          <input id="git-url" placeholder="https://github.com/org/repo.git" autocomplete="off" spellcheck="false">
          <label class="hint" for="git-cred">Credential for private remotes</label>
          <select id="git-cred" aria-label="Credential">
            <option value="">none (public)</option>${credOptions}
          </select>
          <input id="git-pat" type="password" placeholder="…or paste a new git PAT to seal"
                 autocomplete="off" spellcheck="false">
          <label class="hint" for="git-trigger">Build</label>
          <select id="git-trigger" aria-label="Trigger mode">
            <option value="manual" selected>manually</option>
            <option value="webhook">on webhook pushes</option>
            <option value="poll">on a poll schedule</option>
          </select>
          <button class="btn" id="git-add">Add repository</button>
          <span class="hint" id="git-note" role="status"></span>
        </div>
      </details>
      <details class="panel">
        <summary>Add web pages</summary>
        <div class="detail">
          <label class="hint" for="web-name">Name</label>
          <input id="web-name" placeholder="docs site" autocomplete="off" spellcheck="false">
          <label class="hint" for="web-urls">HTTPS addresses, one per line</label>
          <textarea id="web-urls" rows="4" placeholder="https://example.com/handbook"></textarea>
          <label class="hint" for="web-trigger">Refresh</label>
          <select id="web-trigger" aria-label="Trigger mode">
            <option value="manual">manually</option>
            <option value="poll" selected>on a poll schedule</option>
          </select>
          <button class="btn" id="web-add">Add web source</button>
          <span class="hint" id="web-note" role="status"></span>
        </div>
      </details>`;

    const hasUpload = connectors.some((c) => c.kind === "upload" && c.enabled);
    const uploadSetup = canAdmin && !hasUpload
      ? `<p class="hint">Documents need an upload source on this bench.
           <button class="btn quiet" id="upload-enable">Enable document uploads</button>
           <span class="hint" id="upload-enable-note" role="status"></span></p>`
      : "";

    const fileRow = (f) => `
      <div class="row" data-file-row="${esc(f.id)}">
        <span class="mono">${esc(f.path)}</span>
        <span>
          <span class="count">${esc(humanBytes(f.size))}</span>
          ${timeTag(f.updated)}
          <button class="btn quiet" data-file-delete="${esc(f.id)}">delete</button>
        </span>
      </div>`;

    if (!view.done(`<h1>Sources</h1>
      <p class="hint">What this bench reads: a repository, web pages, and uploaded
      documents all fire into one wiki. Changes take effect on the next build.</p>
      <div class="meta">
        <button class="btn" id="sources-build">Build now</button>
        <span class="hint" id="sources-build-note" role="status"></span>
      </div>

      <label class="group-label">Connected sources</label>
      <div id="connector-note" class="hint" role="status"></div>
      ${canAdmin
        ? (connectors.map(connectorRow).join("") ||
           `<div class="empty">No sources yet. Add a repository, web pages, or enable
            document uploads below.</div>`) + addForms
        : `<div class="empty">Managing sources needs an org owner or an instance
            admin. You can still add documents below if your role allows.</div>`}

      <label class="group-label">Documents</label>
      ${uploadSetup}
      <div class="dropzone" id="dropzone" role="button" tabindex="0"
           aria-label="Upload documents">
        Drop documents here, or <span class="linkish">choose files</span>
        <input id="file-input" type="file" multiple hidden>
      </div>
      <div id="upload-progress" class="hint" role="status"></div>
      <div id="file-list">
        ${filesErr ? `<div class="empty">${esc(filesErr)}</div>`
          : files.map(fileRow).join("") ||
            `<div class="empty">No documents yet. Markdown, PDF, Office files, and
             HTML are extracted and woven into the wiki alongside the other sources.</div>`}
      </div>`)) return;

    // ---- build now ----------------------------------------------------------
    const buildNote = (msg, isErr) => {
      const n = $("sources-build-note");
      if (n) { n.textContent = msg; n.classList.toggle("error", Boolean(isErr)); }
    };
    once($("sources-build"), async () => {
      try {
        const res = await api(`/workspaces/${ws}/runs`, { method: "POST", body: {} });
        toast(res.created ? "Run queued" : "A run was already waiting — joined it");
        location.hash = "#/runs";
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

    const addConnector = async (body, noteEl) => {
      try {
        await api(`/workspaces/${ws}/connectors`, { method: "POST", body });
        toast(`Added ${body.name}`);
        showSources();
        return true;
      } catch (err) {
        if (!err.handled) { noteEl.textContent = err.message; noteEl.classList.add("error"); }
        return false;
      }
    };
    const gitAdd = $("git-add");
    if (gitAdd) once(gitAdd, async () => {
      const note = $("git-note");
      const name = $("git-name").value.trim();
      const source = $("git-url").value.trim();
      if (!name || !source) {
        note.textContent = "name and remote (or path) are required";
        note.classList.add("error");
        return;
      }
      let credentialId = $("git-cred").value;
      const pat = $("git-pat").value.trim();
      if (pat) {
        // A pasted secret is sealed first; the connector then references the
        // sealed credential rather than ever carrying the plaintext.
        try {
          const cred = await api(`/workspaces/${ws}/credentials`,
            { method: "POST", body: { kind: "git_pat", secret: pat } });
          credentialId = cred.id;
          $("git-pat").value = "";
        } catch (err) {
          if (!err.handled) { note.textContent = err.message; note.classList.add("error"); }
          return;
        }
      }
      const config = source.startsWith("https://") ? { url: source } : { path: source };
      await addConnector({
        kind: "git", name, config, credentialId,
        triggerMode: $("git-trigger").value,
      }, note);
    });
    const webAdd = $("web-add");
    if (webAdd) once(webAdd, async () => {
      const note = $("web-note");
      const name = $("web-name").value.trim();
      const urls = $("web-urls").value.split("\n").map((u) => u.trim()).filter(Boolean);
      if (!name || !urls.length) {
        note.textContent = "name and at least one https address are required";
        note.classList.add("error");
        return;
      }
      await addConnector({
        kind: "web", name, config: { urls },
        triggerMode: $("web-trigger").value,
      }, note);
    });
    const uploadEnable = $("upload-enable");
    if (uploadEnable) once(uploadEnable, async () => {
      await addConnector({ kind: "upload", name: "documents", config: {} },
        $("upload-enable-note"));
    });

    // ---- uploads ------------------------------------------------------------
    const progress = (msg, isErr) => {
      const n = $("upload-progress");
      if (n) { n.textContent = msg; n.classList.toggle("error", Boolean(isErr)); }
    };
    let uploading = false;
    const uploadFiles = async (fileList) => {
      if (uploading || !fileList.length) return;
      uploading = true;
      let ok = 0, lastBuild = "";
      const failures = [];
      try {
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
      } finally {
        uploading = false;
      }
      if (failures.length) progress(failures.join(" · "), true);
      else progress("");
      if (ok) {
        toast(lastBuild === "queued"
          ? `${ok} document${ok === 1 ? "" : "s"} uploaded — build queued`
          : `${ok} document${ok === 1 ? "" : "s"} uploaded — changes pending until the next build`);
        if (view.current()) showSources();
      }
    };

    const zone = $("dropzone");
    const input = $("file-input");
    zone.addEventListener("click", () => input.click());
    zone.addEventListener("keydown", (e) => {
      if (e.key === "Enter" || e.key === " ") { e.preventDefault(); input.click(); }
    });
    input.addEventListener("change", () => {
      uploadFiles(input.files);
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
      uploadFiles(e.dataTransfer.files);
    });

    for (const b of document.querySelectorAll("[data-file-delete]")) {
      once(b, async () => {
        if (!armButton(b, "delete")) return;
        try {
          await api(`/workspaces/${ws}/files/${encodeURIComponent(b.dataset.fileDelete)}`,
            { method: "DELETE" });
          toast("Document removed — derived pages await deletion review after the next build");
          showSources();
        } catch (err) {
          if (!err.handled) progress(err.message, true);
        }
      });
    }
  } catch (err) {
    if (!err.handled) view.done(banner(err));
  }
}
