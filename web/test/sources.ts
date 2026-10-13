// Loads the legacy internal/api/ui/sources.js into the test's jsdom window and
// hands back the pure ingest-status helpers, so the Ingest page's rules (the
// Documents summary, the row chips, the reason line, the attention line, the
// connector bar and the in-place refresh after a run) are checked against the
// code that runs in production.
//
// sources.js is a classic script that only declares functions and constants
// and resolves app.js's helpers (esc, relTime, api, …) at call time, so it is
// evaluated as one function body with the helpers the tested functions reach
// for as parameters, and its top-level declarations become locals the body
// can return. Nothing in it runs at load.
import { readFileSync } from "node:fs";
import { join } from "node:path";

export const SOURCES_JS = join(import.meta.dirname, "../../internal/api/ui/sources.js");

export interface FileIngest {
  status?: "read" | "waiting" | "skipped";
  pages?: number;
  readAt?: string;
  class?: string;
  reason?: string;
}

export interface FileRow {
  id?: string;
  name?: string;
  path?: string;
  enabled?: boolean;
  ingest?: FileIngest;
}

export interface Connector {
  enabled: boolean;
  lastError?: string;
  lastSynced?: string;
}

export interface Run {
  status: string;
}

export interface Health {
  pct: number;
  cls: string;
  note: string;
}

export interface Summary {
  read: number;
  waiting: number;
  notRead: number;
  paused: number;
}

// What the view hands patchIngestStatus: the queue state, the upload
// connector when the caller can see one, and the row renderer it owns.
export interface PatchContext {
  buildActive: boolean;
  upload?: Connector;
  rows: (files: FileRow[]) => string;
}

export interface Sources {
  plural(n: number, word: string): string;
  ingestSummary(files: FileRow[]): Summary;
  ingestSummaryText(n: Summary): string;
  ingestChip(f: FileRow, nextCheck?: string): string;
  ingestReason(f: FileRow): string;
  attentionLine(files: FileRow[]): string;
  connectorHealth(c: Connector, summary?: Summary | null): Health;
  runsActive(runs: Run[]): boolean;
  docsNoteHTML(files: FileRow[], buildActive: boolean): string;
  docsGlossHTML(files: FileRow[], uploadPaused: boolean): string;
  patchIngestStatus(root: ParentNode, files: FileRow[], ctx: PatchContext): void;
}

// The app.js helpers the exported functions call.
export interface Helpers {
  esc: (s: string | number | null | undefined) => string;
  relTime: (iso: string) => string;
}

const EXPORTS = ["plural", "ingestSummary", "ingestSummaryText", "ingestChip",
  "ingestReason", "attentionLine", "connectorHealth", "runsActive", "docsNoteHTML", "docsGlossHTML",
  "patchIngestStatus"] as const;

// esc as app.js defines it (app.js:587), copied rather than evaluated out of
// app.js: the loader in legacy.ts returns the characterized functions only,
// and what the tests assert is this rule, not a reference to it.
export const esc: Helpers["esc"] = (s) => String(s ?? "").replace(/[&<>"']/g, (c) =>
  ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c] ?? c);

// relTime's output is a chip title; the tests only need to see where it went.
export const relTime: Helpers["relTime"] = (iso) => `rel(${iso})`;

export const loadSources = (helpers: Helpers = { esc, relTime }): Sources =>
  evaluateSources(readFileSync(SOURCES_JS, "utf8"), helpers);

export function evaluateSources(src: string, helpers: Helpers): Sources {
  // eslint-disable-next-line @typescript-eslint/no-implied-eval -- evaluating sources.js is this loader's purpose
  const body = new Function("esc", "relTime", `${src}\nreturn { ${EXPORTS.join(", ")} };`) as
    (esc: Helpers["esc"], relTime: Helpers["relTime"]) => Sources;
  return body(helpers.esc, helpers.relTime);
}
