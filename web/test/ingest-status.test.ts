// Behavioural tests for the Ingest page's per-document status, run against
// the real sources.js: the Documents summary adds up, the connector bar never
// says "Read cleanly" while a document is unread, the chips and the reason
// line say the agreed words and escape the member's text, the attention line
// names up to three files and lists beyond, and the page is patched in place
// when a run settles. The Go pins in internal/api/ui_ingest_status_test.go
// hold the source shape; these hold what the functions do.
import { describe, expect, test } from "vitest";
import { loadSources, type FileRow } from "./sources";

const S = loadSources();

// parse renders one helper's HTML into jsdom so the assertions can read what
// a person would see (textContent) rather than only the markup.
const parse = (html: string): HTMLElement => {
  const el = document.createElement("div");
  el.innerHTML = html;
  return el;
};

const read = (name: string, pages: number, extra: Partial<FileRow> = {}): FileRow =>
  ({ name, path: `uploads/${name}`, ingest: { status: "read", pages, readAt: "2026-10-09T10:00:00Z" }, ...extra });
const waiting = (name: string, extra: Partial<FileRow> = {}): FileRow =>
  ({ name, path: `uploads/${name}`, ingest: { status: "waiting" }, ...extra });
const skipped = (name: string, extra: Partial<FileRow> = {}): FileRow => ({
  name, path: `uploads/${name}`,
  ingest: { status: "skipped", class: "no_text", reason: "No text found, probably a scan. Upload a text version." },
  ...extra,
});

describe("ingestSummary", () => {
  test("paused files are their own bucket; the buckets are disjoint and sum to the list", () => {
    // A paused file used to be counted in its status bucket as well, so five
    // documents summed to seven.
    const files = [
      read("a.md", 3),
      read("b.md", 2, { enabled: false }),
      waiting("c.pdf", { enabled: false }),
      skipped("scan.pdf"),
      skipped("old-scan.pdf", { enabled: false }),
    ];
    const n = S.ingestSummary(files);
    expect(n).toEqual({ read: 1, waiting: 0, notRead: 1, paused: 3 });
    expect(n.read + n.waiting + n.notRead + n.paused).toBe(files.length);
    expect(S.ingestSummaryText(n)).toBe("1 read · 1 not read · 3 paused");
    // The unread count the bar and the Ingest now link act on: a paused file
    // is never read and must not trigger either.
    expect(S.unreadCount(n)).toBe(1);
  });

  test("a bench with 25 read, one waiting, one not read", () => {
    const files = [
      ...Array.from({ length: 25 }, (_, i) => read(`doc-${String(i)}.pdf`, i + 1)),
      waiting("7 Questions.pdf"),
      skipped("scan.pdf"),
    ];
    const n = S.ingestSummary(files);
    expect(n).toEqual({ read: 25, waiting: 1, notRead: 1, paused: 0 });
    expect(S.ingestSummaryText(n)).toBe("25 read · 1 waiting · 1 not read");
  });

  test("zero parts are omitted; nothing counted is an empty line", () => {
    expect(S.ingestSummaryText(S.ingestSummary([]))).toBe("");
    expect(S.ingestSummaryText(S.ingestSummary([waiting("x.pdf")]))).toBe("1 waiting");
    // A row without an ingest member (an older API) counts nowhere.
    expect(S.ingestSummary([{ name: "y.md", path: "uploads/y.md" }]))
      .toEqual({ read: 0, waiting: 0, notRead: 0, paused: 0 });
  });
});

describe("unreadCount", () => {
  test("a waiting document is unread", () => {
    // One upload waiting for the next ingest must stop the bar from saying
    // Read cleanly, so waiting counts as unread on its own.
    expect(S.unreadCount(S.ingestSummary([read("a.md", 3), waiting("7 Questions.pdf")]))).toBe(1);
    expect(S.unreadCount({ read: 25, waiting: 1, notRead: 0, paused: 0 })).toBe(1);
  });

  test("a skipped document is unread", () => {
    expect(S.unreadCount(S.ingestSummary([read("a.md", 3), skipped("scan.pdf")]))).toBe(1);
    expect(S.unreadCount({ read: 25, waiting: 0, notRead: 1, paused: 0 })).toBe(1);
  });

  test("waiting and not read add; read and paused do not count", () => {
    expect(S.unreadCount({ read: 25, waiting: 1, notRead: 1, paused: 0 })).toBe(2);
    expect(S.unreadCount({ read: 25, waiting: 0, notRead: 0, paused: 0 })).toBe(0);
    expect(S.unreadCount(S.ingestSummary([
      read("a.md", 3),
      waiting("c.pdf", { enabled: false }),
      skipped("old-scan.pdf", { enabled: false }),
    ]))).toBe(0);
    expect(S.unreadCount({ read: 0, waiting: 0, notRead: 0, paused: 3 })).toBe(0);
  });
});

describe("connectorHealth", () => {
  const clean = { enabled: true, lastSynced: "2026-10-09T10:00:00Z" };

  test("says Read cleanly only with nothing unread", () => {
    expect(S.connectorHealth(clean, 0)).toEqual({ pct: 100, cls: "fill-ember", note: "Read cleanly" });
    expect(S.connectorHealth(clean)).toEqual({ pct: 100, cls: "fill-ember", note: "Read cleanly" });
  });

  test("never says Read cleanly while a document is unread, and softens the fill", () => {
    // "not read", the words the chips and the header use; the bar used to say
    // "unread", a fourth term for the one state.
    for (const unread of [1, 2, 26]) {
      const h = S.connectorHealth(clean, unread);
      expect(h.note).not.toContain("Read cleanly");
      expect(h).toEqual({ pct: 100, cls: "fill-soft", note: "Some documents were not read; see below." });
    }
  });

  test("paused, failed and never-read sources keep their own notes whatever the count", () => {
    expect(S.connectorHealth({ enabled: false, lastSynced: clean.lastSynced }, 3).note).toBe("Paused — skipped on ingest");
    expect(S.connectorHealth({ ...clean, lastError: "boom" }, 3)).toEqual({ pct: 100, cls: "fill-err", note: "Last read failed" });
    expect(S.connectorHealth({ enabled: true }, 3)).toEqual({ pct: 0, cls: "fill-soft", note: "Not read yet" });
  });
});

describe("ingestChip", () => {
  test("read: the word, then the page count; the last check in the title", () => {
    // Beside the size chip a bare "6 pages" reads as the PDF's own page
    // count, so the chip leads with the state. readAt is stamped by every
    // ingest that checks the file, not only the one that first read it.
    const six = parse(S.ingestChip(read("a.pdf", 6))).firstElementChild;
    expect(six?.className).toBe("chip");
    expect(six?.textContent).toBe("read · 6 pages");
    expect(six?.getAttribute("title")).toBe("Checked rel(2026-10-09T10:00:00Z)");
    expect(parse(S.ingestChip(read("b.pdf", 1))).textContent).toBe("read · 1 page");
    // No count is still read, without inventing "0 pages".
    expect(parse(S.ingestChip(read("c.pdf", 0))).textContent).toBe("read");
    const noStamp = parse(S.ingestChip({ name: "d.md", path: "uploads/d.md", ingest: { status: "read", pages: 2 } }));
    expect(noStamp.firstElementChild?.hasAttribute("title")).toBe(false);
  });

  test("waiting: the agreed words, the next check as the title when known", () => {
    const chip = parse(S.ingestChip(waiting("w.pdf"), "Next check in 10 minutes")).firstElementChild;
    expect(chip?.textContent).toBe("Waiting for the next ingest");
    expect(chip?.getAttribute("title")).toBe("Next check in 10 minutes");
    expect(parse(S.ingestChip(waiting("w.pdf"))).firstElementChild?.hasAttribute("title")).toBe(false);
  });

  test("skipped: an oxide Not read chip", () => {
    const chip = parse(S.ingestChip(skipped("s.pdf"))).firstElementChild;
    expect(chip?.textContent).toBe("Not read");
    expect(chip?.classList.contains("not-read")).toBe(true);
  });

  test("paused rows keep their paused chip and get none here, whatever the status", () => {
    expect(S.ingestChip(read("a.pdf", 6, { enabled: false }))).toBe("");
    expect(S.ingestChip(waiting("w.pdf", { enabled: false }))).toBe("");
    expect(S.ingestChip(skipped("s.pdf", { enabled: false }))).toBe("");
  });

  test("an unknown or missing status renders nothing", () => {
    expect(S.ingestChip({ name: "x", path: "uploads/x" })).toBe("");
  });
});

describe("ingestReason", () => {
  test("a skipped row carries the API sentence and its class, visible, not a hover", () => {
    const el = parse(S.ingestReason(skipped("scan.pdf"))).firstElementChild;
    expect(el?.classList.contains("src-reason")).toBe(true);
    expect(el?.getAttribute("data-class")).toBe("no_text");
    expect(el?.textContent).toBe("No text found, probably a scan. Upload a text version.");
    expect(el?.hasAttribute("title")).toBe(false);
  });

  test("the reason is text about a member's file and is escaped", () => {
    const html = S.ingestReason({
      name: "evil.pdf", path: "uploads/evil.pdf",
      ingest: { status: "skipped", class: `x" onmouseover="alert(1)`, reason: "<script>alert(1)</script> & more" },
    });
    expect(html).toContain("&lt;script&gt;alert(1)&lt;/script&gt; &amp; more");
    expect(html).not.toContain("<script>");
    const el = parse(html);
    expect(el.querySelector("script")).toBeNull();
    expect(el.firstElementChild?.getAttribute("data-class")).toBe(`x" onmouseover="alert(1)`);
    expect(el.firstElementChild?.hasAttribute("onmouseover")).toBe(false);
    expect(el.textContent).toBe("<script>alert(1)</script> & more");
  });

  test("read, waiting and paused rows have no reason line", () => {
    expect(S.ingestReason(read("a.pdf", 6))).toBe("");
    expect(S.ingestReason(waiting("w.pdf"))).toBe("");
    expect(S.ingestReason(skipped("s.pdf", { enabled: false }))).toBe("");
  });
});

describe("attentionLine", () => {
  test("nothing skipped, or only paused skips: no line", () => {
    expect(S.attentionLine([])).toBe("");
    expect(S.attentionLine([read("a.pdf", 6), waiting("w.pdf")])).toBe("");
    expect(S.attentionLine([skipped("s.pdf", { enabled: false })])).toBe("");
  });

  test("one file: a weighted sentence naming it, and the reason stays on the row", () => {
    const el = parse(S.attentionLine([read("a.pdf", 6), skipped("Are You Paying Too Much.pdf")]));
    const p = el.firstElementChild;
    expect(p?.tagName).toBe("P");
    expect(p?.classList.contains("ingest-attention")).toBe(true);
    expect(p?.textContent).toBe("1 document was not read: Are You Paying Too Much.pdf");
    // The lede is the part that must not read as one more grey hint.
    expect(p?.querySelector("strong")?.textContent).toBe("1 document was not read");
    expect(p?.querySelector(".att-name")?.textContent).toBe("Are You Paying Too Much.pdf");
    expect(el.textContent).not.toContain("No text found");
  });

  test("a name is a link to its row when the row has an id", () => {
    // The row can sit a long scroll below the line on a phone; the href is
    // the row's id, and the view's handler scrolls rather than navigating.
    const el = parse(S.attentionLine([skipped("scan.pdf", { id: "f-12" })]));
    const a = el.querySelector("a.att-name");
    expect(a?.getAttribute("href")).toBe("#file-f-12");
    expect(a?.getAttribute("data-file-jump")).toBe("f-12");
    expect(a?.textContent).toBe("scan.pdf");
    // Without an id there is nothing to point at, and the name stays text.
    expect(parse(S.attentionLine([skipped("scan.pdf")])).querySelector("a")).toBeNull();
    // The id is an attribute value and is escaped like any other.
    const hostile = parse(S.attentionLine([skipped("x.pdf", { id: `" onclick="alert(1)` })]));
    expect(hostile.querySelector("a")?.hasAttribute("onclick")).toBe(false);
  });

  test("up to three files ride in the sentence", () => {
    const el = parse(S.attentionLine([skipped("a.pdf"), skipped("b.pdf"), skipped("c.pdf")]));
    expect(el.firstElementChild?.tagName).toBe("P");
    expect(el.textContent).toBe("3 documents were not read: a.pdf, b.pdf, c.pdf");
    expect(el.querySelector("ul")).toBeNull();
  });

  test("more than three go beneath the sentence as a list", () => {
    const names = ["a.pdf", "b.pdf", "c.pdf", "d.pdf", "e.pdf"];
    const el = parse(S.attentionLine(names.map((n, i) => skipped(n, { id: `f${String(i)}` }))));
    expect(el.firstElementChild?.tagName).toBe("DIV");
    expect(el.firstElementChild?.classList.contains("ingest-attention")).toBe(true);
    expect(el.firstElementChild?.firstChild?.textContent).toBe("5 documents were not read");
    expect([...el.querySelectorAll("li")].map((li) => li.textContent)).toEqual(names);
    expect([...el.querySelectorAll("li a")].map((a) => a.getAttribute("href"))).toEqual(
      names.map((_, i) => `#file-f${String(i)}`));
  });

  test("names are escaped, and a row without a name falls back to the path's last segment", () => {
    const el = parse(S.attentionLine([
      skipped("<b>bold</b>.pdf"),
      { path: "uploads/deep/er/scan.pdf", ingest: { status: "skipped" } },
    ]));
    expect(el.querySelector("b")).toBeNull();
    expect([...el.querySelectorAll(".att-name")].map((s) => s.textContent)).toEqual(["<b>bold</b>.pdf", "scan.pdf"]);
  });
});

describe("runsActive", () => {
  test("a queued or running run is active; a settled feed is not", () => {
    expect(S.runsActive([{ status: "succeeded" }, { status: "queued" }])).toBe(true);
    expect(S.runsActive([{ status: "running" }])).toBe(true);
    expect(S.runsActive([{ status: "succeeded" }, { status: "no_changes" }, { status: "failed" }])).toBe(false);
    expect(S.runsActive([])).toBe(false);
  });
});

describe("docsNoteHTML", () => {
  test("the summary, then the way out when something is waiting", () => {
    const files = [read("a.pdf", 6), waiting("7 Questions.pdf")];
    const idle = parse(S.docsNoteHTML(files, false));
    expect(idle.textContent).toBe("1 read · 1 waiting · Ingest now");
    expect(idle.querySelector("button.linkish[data-ingest-now]")?.textContent).toBe("Ingest now");
    // Once a run is queued the link gives way to the fact, so the page cannot
    // say "waiting · Ingest now" under a header that says "Firing".
    const busy = parse(S.docsNoteHTML(files, true));
    expect(busy.textContent).toBe("1 read · 1 waiting · ingest in progress");
    expect(busy.querySelector("[data-ingest-now]")).toBeNull();
  });

  test("nothing waiting: the summary alone, whatever the queue is doing", () => {
    expect(parse(S.docsNoteHTML([read("a.pdf", 6), skipped("s.pdf")], false)).textContent).toBe("1 read · 1 not read");
    expect(parse(S.docsNoteHTML([read("a.pdf", 6)], true)).textContent).toBe("1 read");
    expect(S.docsNoteHTML([], false)).toBe("none uploaded yet");
    expect(S.docsNoteHTML([{ name: "y.md", path: "uploads/y.md" }], false)).toBe("uploaded to this bench");
  });
});

describe("docsGlossHTML", () => {
  test("count and summary first, the origin after a dash that cannot start a line", () => {
    const files = [read("a.pdf", 6), waiting("w.pdf")];
    const html = S.docsGlossHTML(files, false);
    expect(html).toBe("2 · 1 read · 1 waiting&nbsp;— uploaded straight from this browser");
    expect(parse(html).textContent).toBe("2 · 1 read · 1 waiting\u00a0— uploaded straight from this browser");
    expect(S.docsGlossHTML(files, true)).toContain("; paused, so skipped on ingest");
  });

  test("with nothing to summarize the gloss is the origin alone", () => {
    expect(S.docsGlossHTML([], false)).toBe("— uploaded straight from this browser");
  });
});

describe("patchIngestStatus", () => {
  // The Ingest view's skeleton: the parts the settle refresh patches, with
  // the ids the view renders them under, filled as the page would be with
  // one upload waiting for the next ingest.
  const upload = { enabled: true, lastSynced: "2026-10-09T10:00:00Z" };
  const rows = (files: FileRow[]) =>
    files.map((f) => `<div class="src-row" id="file-${f.id ?? ""}">${S.ingestChip(f)}${S.ingestReason(f)}</div>`).join("");
  const before = [read("a.pdf", 6, { id: "a" }), waiting("7 Questions.pdf", { id: "q" })];
  const page = (): HTMLElement => {
    const h = S.connectorHealth(upload, S.unreadCount(S.ingestSummary(before)));
    return parse(`
      <div class="stat" id="stat-documents">
        <div class="stat-value">${String(before.length)}</div>
        <div class="stat-note">${S.docsNoteHTML(before, false)}</div>
      </div>
      <div class="src-health" id="upload-health">
        <div class="bar"><span class="${h.cls}" data-w="${String(h.pct)}"></span></div>
        <div class="bar-note">${h.note}</div>
      </div>
      <span class="gloss" id="docs-gloss">${S.docsGlossHTML(before, false)}</span>
      <div id="ingest-attention">${S.attentionLine(before)}</div>
      <div id="file-drop">${rows(before)}</div>`);
  };
  const text = (root: ParentNode, sel: string) => root.querySelector(sel)?.textContent.replace(/\s+/g, " ").trim();
  const fillOf = (root: ParentNode) => root.querySelector("#upload-health .bar span")?.className;

  test("after the run that read the waiting file, every part says so without a reload", () => {
    const root = page();
    expect(text(root, ".stat-note")).toBe("1 read · 1 waiting · Ingest now");
    expect(text(root, ".bar-note")).toBe("Some documents were not read; see below.");
    expect(text(root, "#file-q")).toBe("Waiting for the next ingest");

    const after = [read("a.pdf", 6, { id: "a" }), read("7 Questions.pdf", 4, { id: "q" })];
    S.patchIngestStatus(root, after, { buildActive: false, upload, rows });

    expect(text(root, ".stat-value")).toBe("2");
    expect(text(root, ".stat-note")).toBe("2 read");
    expect(root.querySelector("[data-ingest-now]")).toBeNull();
    expect(text(root, "#docs-gloss")).toBe("2 · 2 read — uploaded straight from this browser");
    expect(root.querySelector("#ingest-attention")?.innerHTML).toBe("");
    expect(text(root, "#file-q")).toBe("read · 4 pages");
    const fill = root.querySelector("#upload-health .bar span");
    expect(fill?.className).toBe("fill-ember");
    expect(fill?.getAttribute("data-w")).toBe("100");
    expect(text(root, ".bar-note")).toBe("Read cleanly");
  });

  test("a run that skipped the file: the attention line, the oxide chip and the reason appear", () => {
    const root = page();
    const after = [read("a.pdf", 6, { id: "a" }), skipped("7 Questions.pdf", { id: "q" })];
    S.patchIngestStatus(root, after, { buildActive: false, upload, rows });

    expect(text(root, ".stat-note")).toBe("1 read · 1 not read");
    expect(text(root, "#ingest-attention")).toBe("1 document was not read: 7 Questions.pdf");
    expect(root.querySelector("#ingest-attention a")?.getAttribute("href")).toBe("#file-q");
    expect(root.querySelector("#file-q .chip.not-read")?.textContent).toBe("Not read");
    expect(text(root, "#file-q .src-reason")).toBe("No text found, probably a scan. Upload a text version.");
    expect(fillOf(root)).toBe("fill-soft");
    expect(text(root, ".bar-note")).toBe("Some documents were not read; see below.");
  });

  test("while a run is queued the note says so instead of offering the link", () => {
    const root = page();
    S.patchIngestStatus(root, before, { buildActive: true, upload, rows });
    expect(text(root, ".stat-note")).toBe("1 read · 1 waiting · ingest in progress");
    expect(root.querySelector("[data-ingest-now]")).toBeNull();
    // And the link comes back when the queue empties without the file read.
    S.patchIngestStatus(root, before, { buildActive: false, upload, rows });
    expect(root.querySelector("[data-ingest-now]")?.textContent).toBe("Ingest now");
  });

  test("a member's page has no bar, and the rest is still patched", () => {
    const root = page();
    root.querySelector("#upload-health")?.remove();
    const after = [read("a.pdf", 6, { id: "a" }), read("7 Questions.pdf", 4, { id: "q" })];
    S.patchIngestStatus(root, after, { buildActive: false, rows });
    expect(text(root, ".stat-note")).toBe("2 read");
    expect(text(root, "#file-q")).toBe("read · 4 pages");
  });
});

describe("plural", () => {
  test("singular only at exactly one", () => {
    expect(S.plural(1, "page")).toBe("1 page");
    expect(S.plural(6, "page")).toBe("6 pages");
    expect(S.plural(0, "document")).toBe("0 documents");
  });
});
