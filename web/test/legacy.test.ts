// Characterization of the legacy renderer: every stored fixture output must
// still be what app.js produces. A failure here means app.js changed
// behaviour; if that was the intent, `npm run fixtures` and review the diff.
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { afterAll, beforeAll, describe, expect, test, vi } from "vitest";
import { BENCH, SUITES, setBench, type FixtureFile } from "./characterize";
import { evaluateLegacy, loadLegacy } from "./legacy";

beforeAll(() => { vi.useFakeTimers({ now: new Date(BENCH.now), toFake: ["Date"] }); });
afterAll(() => { vi.useRealTimers(); });

const L = loadLegacy();

const readFixture = (name: string) =>
  JSON.parse(readFileSync(join(import.meta.dirname, "fixtures", `${name}.json`), "utf8")) as FixtureFile;

for (const [name, suite] of Object.entries(SUITES)) {
  describe(name, () => {
    const file = readFixture(name);

    test("fixture inputs match fixtures/cases.ts (regenerate with npm run fixtures)", () => {
      expect(file.bench).toEqual(BENCH);
      expect(file.cases.map((c) => Object.fromEntries(Object.entries(c).filter(([k]) => k !== "output"))))
        .toEqual(suite.inputs);
    });

    for (const { output, ...input } of file.cases) {
      const label = typeof input.name === "string" ? input.name : JSON.stringify(input);
      test(label, () => {
        setBench(L, file.bench);
        expect(suite.run(L, input, file.bench)).toEqual(output);
      });
    }
  });
}

test("the loader refuses an app.js that no longer ends in boot()", () => {
  expect(() => evaluateLegacy("const x = 1;\nstart();\n")).toThrow(/boot\(\);/);
});
