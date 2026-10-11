import { readFileSync, writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { defineConfig, type Plugin } from "vite";

// The build writes straight into the Go embed: internal/api/ui/dist is
// committed, so `go build` never needs Node, and CI rebuilds it and fails on
// any diff. Everything here serves that diff: no sourcemaps (absolute paths),
// no inlined assets (a data: URI would need a looser CSP), content-hashed
// names under assets/ (the Go side marks only those immutable).
//
// Expected warnings, all about the legacy files index.html still names:
//   - `<script src="/ui/sources.js">` and `/ui/app.js` "can't be bundled
//     without type=module": deliberate, they stay classic scripts served by
//     Go until their views are ported.
//   - `/ui/style.css` "doesn't exist at build time": same, Go serves it.

// modulesManifest writes web/modules.txt: every npm package that ends up in
// the bundle, as name@version, one per line and sorted. It sits outside the
// embed and is diff-checked in CI, so a dependency bump that changes what the
// browser runs shows up in review even when the bundle diff is unreadable.
function modulesManifest(): Plugin {
  return {
    name: "kiln-modules-manifest",
    apply: "build",
    generateBundle(_options, bundle) {
      const seen = new Set<string>();
      for (const chunk of Object.values(bundle)) {
        if (chunk.type !== "chunk") continue;
        for (const id of chunk.moduleIds) {
          const m = /[\\/]node_modules[\\/]((?:@[^\\/]+[\\/])?[^\\/]+)[\\/]/.exec(id);
          if (!m?.[1]) continue;
          const name = m[1].replace(/\\/g, "/");
          const pkg = JSON.parse(readFileSync(new URL(`./node_modules/${name}/package.json`, import.meta.url), "utf8")) as { version: string };
          seen.add(`${name}@${pkg.version}`);
        }
      }
      writeFileSync(fileURLToPath(new URL("./modules.txt", import.meta.url)), [...seen].sort().map((s) => `${s}\n`).join(""));
    },
  };
}

export default defineConfig({
  base: "/ui/",
  publicDir: false,
  // JSX without @preact/preset-vite: Vite 8's oxc transform does the
  // automatic runtime on its own, and the preset is one more dependency.
  oxc: { jsx: { runtime: "automatic", importSource: "preact" } },
  plugins: [modulesManifest()],
  build: {
    outDir: "../internal/api/ui/dist",
    emptyOutDir: true,
    sourcemap: false,
    assetsInlineLimit: 0,
    target: "es2022",
    modulePreload: { polyfill: false },
    rolldownOptions: {
      output: {
        entryFileNames: "assets/[name]-[hash].js",
        chunkFileNames: "assets/[name]-[hash].js",
        assetFileNames: "assets/[name]-[hash][extname]",
      },
    },
  },
});
