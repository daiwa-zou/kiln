// The fixture generator, run by `npm run fixtures` and skipped otherwise.
// It is a vitest file only so it runs in the same jsdom environment as the
// tests that read its output. Regenerate after a deliberate change to the
// legacy renderer or to fixtures/cases.ts, and review the JSON diff: it is
// the behaviour change.
import { writeFileSync } from "node:fs";
import { join } from "node:path";
import { afterAll, beforeAll, test, vi } from "vitest";
import { BENCH, SUITES, setBench, type FixtureFile } from "./characterize";
import { loadLegacy } from "./legacy";

beforeAll(() => { vi.useFakeTimers({ now: new Date(BENCH.now), toFake: ["Date"] }); });
afterAll(() => { vi.useRealTimers(); });

test.runIf(process.env.KILN_FIXTURES === "write")("write characterization fixtures", () => {
  const L = loadLegacy();
  for (const [name, suite] of Object.entries(SUITES)) {
    const file: FixtureFile = {
      generatedFrom: "internal/api/ui/app.js",
      bench: BENCH,
      cases: suite.inputs.map((input) => {
        setBench(L, BENCH);
        return { ...input, output: suite.run(L, input, BENCH) };
      }),
    };
    writeFileSync(join(import.meta.dirname, "fixtures", `${name}.json`), `${JSON.stringify(file, null, 2)}\n`);
  }
});
