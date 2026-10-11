// @vitest-environment node
// The sink ban is only as good as the forms it catches, so it is tested the
// way it would be bypassed: test/eslint-fixtures/bypass.tsx writes each form
// once, marked BAD, and this lints it under the real eslint.config.js.
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { ESLint } from "eslint";
import { expect, test } from "vitest";

const SINK_RULES = new Set(["no-restricted-syntax", "no-eval", "no-new-func", "no-script-url", "no-restricted-imports"]);
const root = join(import.meta.dirname, "..");
const fixture = join(root, "test/eslint-fixtures/bypass.tsx");

// sinkLines lints source under a path and returns, by line, the sink rules
// that fired, plus the lines the source marks BAD.
async function sinkLines(eslint: ESLint, source: string, filePath: string) {
  const [result] = await eslint.lintText(source, { filePath });
  if (!result) throw new Error(`eslint returned no result for ${filePath}`);
  expect(result.fatalErrorCount, JSON.stringify(result.messages.filter((m) => m.fatal))).toBe(0);
  const flagged = new Map<number, string[]>();
  for (const m of result.messages) {
    if (m.severity !== 2 || !m.ruleId || !SINK_RULES.has(m.ruleId)) continue;
    flagged.set(m.line, [...(flagged.get(m.line) ?? []), m.ruleId]);
  }
  const lines = source.split("\n");
  const bad = lines.flatMap((l, i) => (/\/\/ BAD\b/.test(l) ? [i + 1] : []));
  const show = (n: number) => `${String(n)}: ${lines[n - 1] ?? ""}`;
  return {
    bad,
    missed: bad.filter((n) => !flagged.has(n)).map(show),
    extra: [...flagged.keys()].filter((n) => !bad.includes(n)).map(show),
  };
}

test("every sink form in the bypass fixture is reported, and nothing else is", async () => {
  // ignore:false: `eslint .` skips the fixtures; this run must not.
  const eslint = new ESLint({ cwd: root, ignore: false });
  const { bad, missed, extra } = await sinkLines(eslint, readFileSync(fixture, "utf8"), fixture);
  expect(bad.length).toBeGreaterThan(45);
  expect(missed, "sink forms the ban does not catch").toEqual([]);
  expect(extra, "lines flagged that the fixture says are fine").toEqual([]);
});

test("Prose.tsx is the one file that may use dangerouslySetInnerHTML, and only that", async () => {
  const eslint = new ESLint({ cwd: root });
  const prose = join(root, "src/components/Prose.tsx");
  const [ok] = await eslint.lintFiles([prose]);
  expect(ok?.messages.filter((m) => m.severity === 2)).toEqual([]);

  // Every other sink stays banned inside Prose: the extra lines are linted as
  // if they were part of Prose.tsx itself (nothing is written to disk).
  const extraSrc = readFileSync(join(root, "test/eslint-fixtures/prose-extra.tsx"), "utf8");
  const inProse = await sinkLines(eslint, `${readFileSync(prose, "utf8")}${extraSrc}`, prose);
  expect(inProse.bad).toHaveLength(4);
  expect(inProse.missed, "sinks Prose's exemption lets through").toEqual([]);
  expect(inProse.extra).toEqual([]);

  // The same file under any other name is an error. Linted as a fixture
  // path, which carries src/'s sink rules without needing a tsconfig entry.
  const [elsewhere] = await new ESLint({ cwd: root, ignore: false }).lintText(readFileSync(prose, "utf8"), {
    filePath: join(root, "test/eslint-fixtures/NotProse.tsx"),
  });
  expect(elsewhere?.messages.some((m) => m.ruleId === "no-restricted-syntax")).toBe(true);
});
