// The bridge between legacy route() and ported views, exercised end to end:
// the real src/main.ts installs window.kiln, the real app.js routes, and the
// real index.html shell is the DOM they share.
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { beforeAll, expect, test } from "vitest";
import type { ViewProps } from "../src/main";
import { loadLegacy, type Legacy } from "./legacy";

let L: Legacy;
let unmounted: string[] = [];

function Alpha({ hash, nav }: ViewProps) {
  return <p id="ported-a" data-nav={nav} ref={(el) => { if (!el) unmounted.push("alpha"); }}>alpha {hash}</p>;
}
function Beta({ hash }: ViewProps) {
  return <p id="ported-b" ref={(el) => { if (!el) unmounted.push("beta"); }}>beta {hash}</p>;
}
// A full-bleed layout, which <main> must notice from the markup.
function Gamma() {
  return <div class="reader" id="ported-g">gamma</div>;
}

beforeAll(async () => {
  const shell = readFileSync(join(import.meta.dirname, "../index.html"), "utf8");
  const body = /<body>([\s\S]*)<\/body>/.exec(shell)?.[1];
  if (!body) throw new Error("index.html has no <body>");
  // The shell without its scripts: this test loads them itself, in order.
  document.body.innerHTML = body.replace(/<script[\s\S]*?<\/script>/g, "");
  window.scrollTo = () => { /* jsdom has no layout */ };
  await import("../src/main");
  L = loadLegacy();
  window.kiln.registerView("alpha", Alpha, { title: "Alpha view", view: "log" });
  window.kiln.registerView("beta", Beta);
  window.kiln.registerView("gamma", Gamma, { activeSlug: (hash) => hash.split("/")[1] ?? null });
});

const main = () => {
  const m = document.getElementById("main");
  if (!m) throw new Error("no <main>");
  return m;
};
const go = (hash: string) => { location.hash = hash; L.route(); };

test("route() mounts a registered view into <main> with its hash and nav token", async () => {
  unmounted = [];
  go("#/alpha/some/where");
  const a = document.getElementById("ported-a");
  expect(a?.textContent).toBe("alpha alpha/some/where");
  expect(Number(a?.dataset.nav)).toBeGreaterThan(0);
  expect(main().getAttribute("aria-busy")).toBeNull();
  expect(main().dataset.view).toBe("log");
  // beginView's delayed skeleton (150ms) must not overwrite a mounted view.
  await new Promise((r) => setTimeout(r, 200));
  expect(document.getElementById("ported-a")).not.toBeNull();
});

test("the next route() unmounts the previous ported view first", () => {
  go("#/alpha");
  unmounted = [];
  go("#/beta");
  expect(unmounted).toEqual(["alpha"]);
  expect(document.getElementById("ported-a")).toBeNull();
  expect(document.getElementById("ported-b")?.textContent).toBe("beta beta");
  expect(main().children).toHaveLength(1);
});

test("the chrome presents a ported view by its registered title and nav entry", () => {
  go("#/alpha");
  expect(document.title).toBe("Alpha view · kiln");
  expect(document.querySelector('a[data-view="log"]')?.getAttribute("aria-current")).toBe("page");
  expect(document.querySelectorAll('a[aria-current="page"]')).toHaveLength(1);
  // Unset options fall back to the route name.
  go("#/beta");
  expect(document.title).toBe("beta · kiln");
  expect(document.querySelector('a[data-view="log"]')?.hasAttribute("aria-current")).toBe(false);
});

test("a ported view settles <main> like a legacy one: bleed from its markup, entrance class", () => {
  L.state.pages = [{ slug: "glaze", title: "Glaze", type: "concept" }];
  L.state.slugs = new Set(["glaze"]);
  go("#/gamma/glaze");
  expect(main().classList.contains("bleed")).toBe(true);
  expect(main().classList.contains("view-in")).toBe(true);
  // activeSlug reached the tree: the page is marked current there.
  expect(document.querySelector('#tree a[aria-current="page"]')?.textContent).toContain("Glaze");
  // A non-bleed view after a bleed one must not inherit the class.
  go("#/beta");
  expect(main().classList.contains("bleed")).toBe(false);
});

test("views is a Map, so prototype names never resolve to a view", () => {
  expect(window.kiln.views).toBeInstanceOf(Map);
  for (const name of ["constructor", "__proto__", "toString", "hasOwnProperty"]) {
    expect(window.kiln.views.get(name)).toBeUndefined();
  }
});

test("a route name can be registered once", () => {
  expect(() => { window.kiln.registerView("alpha", Beta); }).toThrow(/already registered/);
});

test("mount() returns an unmount that empties its element", () => {
  const el = document.createElement("div");
  const unmount = window.kiln.mount(el, Beta, { hash: "island", nav: 0 });
  expect(el.textContent).toBe("beta island");
  unmount();
  expect(el.childNodes).toHaveLength(0);
});
