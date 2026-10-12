// Behavioural tests for the Ingest page's per-document status (ST-U-2),
// run against the real sources.js: the Documents summary adds up, the
// connector bar never says "Read cleanly" while a document is unread, the
// chips and the reason line say the agreed words and escape the member's
// text, and the attention line names up to three files and lists beyond.
// The Go pins in internal/api/ui_ingest_status_test.go hold the source
// shape; these hold what the functions do.
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
    // The five-file case from the qa audit: it used to read 7 for 5 documents.
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
    expect(n.waiting + n.notRead).toBe(1);
  });

  test("Marcus's bench: 25 read, one waiting, one not read", () => {
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

describe("connectorHealth", () => {
  const clean = { enabled: true, lastSynced: "2026-10-09T10:00:00Z" };

  test("says Read cleanly only with nothing unread", () => {
    expect(S.connectorHealth(clean, 0)).toEqual({ pct: 100, cls: "fill-ember", note: "Read cleanly" });
    expect(S.connectorHealth(clean)).toEqual({ pct: 100, cls: "fill-ember", note: "Read cleanly" });
  });

  test("never says Read cleanly while a document is unread, and softens the fill", () => {
    for (const unread of [1, 2, 26]) {
      const h = S.connectorHealth(clean, unread);
      expect(h.note).not.toContain("Read cleanly");
      expect(h).toEqual({ pct: 100, cls: "fill-soft", note: "Some documents are unread; see below." });
    }
  });

  test("paused, failed and never-read sources keep their own notes whatever the count", () => {
    expect(S.connectorHealth({ enabled: false, lastSynced: clean.lastSynced }, 3).note).toBe("Paused — skipped on ingest");
    expect(S.connectorHealth({ ...clean, lastError: "boom" }, 3)).toEqual({ pct: 100, cls: "fill-err", note: "Last read failed" });
    expect(S.connectorHealth({ enabled: true }, 3)).toEqual({ pct: 0, cls: "fill-soft", note: "Not read yet" });
  });
});

describe("ingestChip", () => {
  test("read: quiet page count, readAt in the title", () => {
    const six = parse(S.ingestChip(read("a.pdf", 6))).firstElementChild;
    expect(six?.className).toBe("chip");
    expect(six?.textContent).toBe("6 pages");
    expect(six?.getAttribute("title")).toBe("Read rel(2026-10-09T10:00:00Z)");
    expect(parse(S.ingestChip(read("b.pdf", 1))).textContent).toBe("1 page");
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

  test("one file: a sentence naming it, and the reason stays on the row", () => {
    const el = parse(S.attentionLine([read("a.pdf", 6), skipped("Are You Paying Too Much.pdf")]));
    const p = el.firstElementChild;
    expect(p?.tagName).toBe("P");
    expect(p?.classList.contains("ingest-attention")).toBe(true);
    expect(p?.textContent).toBe("1 document was not read: Are You Paying Too Much.pdf");
    expect(p?.querySelector(".att-name")?.textContent).toBe("Are You Paying Too Much.pdf");
    expect(el.textContent).not.toContain("No text found");
  });

  test("up to three files ride in the sentence", () => {
    const el = parse(S.attentionLine([skipped("a.pdf"), skipped("b.pdf"), skipped("c.pdf")]));
    expect(el.firstElementChild?.tagName).toBe("P");
    expect(el.textContent).toBe("3 documents were not read: a.pdf, b.pdf, c.pdf");
    expect(el.querySelector("ul")).toBeNull();
  });

  test("more than three go beneath the sentence as a list", () => {
    const names = ["a.pdf", "b.pdf", "c.pdf", "d.pdf", "e.pdf"];
    const el = parse(S.attentionLine(names.map((n) => skipped(n))));
    expect(el.firstElementChild?.tagName).toBe("DIV");
    expect(el.firstElementChild?.classList.contains("ingest-attention")).toBe(true);
    expect(el.firstElementChild?.firstChild?.textContent).toBe("5 documents were not read");
    expect([...el.querySelectorAll("li")].map((li) => li.textContent)).toEqual(names);
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

describe("plural", () => {
  test("singular only at exactly one", () => {
    expect(S.plural(1, "page")).toBe("1 page");
    expect(S.plural(6, "page")).toBe("6 pages");
    expect(S.plural(0, "document")).toBe("0 documents");
  });
});
