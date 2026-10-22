import { fileURLToPath } from "node:url";
import { resolve } from "node:path";
import { defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";

const here = fileURLToPath(new URL(".", import.meta.url));

// runar-sdk is bundled straight from the local Rúnar checkout's TypeScript
// sources (README.md, "runar-sdk from the local checkout"): no build step, no
// copy. RUNAR_DIR overrides where that checkout is; the default is a sibling
// of this checkout's parent (~/Work/bsv/runar next to ~/Work/agent-env/skein-amm).
// tsconfig.json's `paths` mirror these aliases for the type checker.
const runarDir = resolve(here, process.env.RUNAR_DIR ?? "../../../../bsv/runar");

// Build output (`npm run build` -> ../../www/) is the app's `www/`, committed:
// the tree a skein serves under /<app>/ (nothing is built on the skein). The
// 1Color pages are copied into www/color/ afterwards
// (../../scripts/color-pages.sh), so a build empties www/ and the whole
// sequence is ../../scripts/www.sh. Relative base so the pages work under
// /<app>/ on any instance, and under a host's "/@<handle>/<app>/" dev form.
export default defineConfig({
  base: "./",
  plugins: [react()],
  resolve: {
    alias: [
      { find: /^runar-sdk$/, replacement: resolve(runarDir, "packages/runar-sdk/src/index.ts") },
      // runar-ir-schema's barrel without its node-only Ajv validators (see the file).
      { find: /^runar-ir-schema$/, replacement: resolve(here, "src/pool/runar-ir-schema.ts") },
      { find: /^@runar-src\//, replacement: resolve(runarDir, "packages") + "/" },
      // @1sat/actions lazily imports xdelta3-wasm (OrdFS patches, unused
      // here), whose package.json names a "module" file that is not in the
      // package (dist/xdelta3.esm.js; the file is dist/xdelta3-wasm.esm.js).
      // Point at the file that exists so the bundler can resolve it.
      { find: /^xdelta3-wasm$/, replacement: resolve(here, "node_modules/xdelta3-wasm/dist/xdelta3-wasm.esm.js") },
    ],
    // runar-sdk's sources import @bsv/sdk from outside this package: resolve
    // it from here, one copy (the @bopen-io/sdk 4 alias, package.json), so the
    // checkout needs no node_modules.
    dedupe: ["@bsv/sdk", "cbor2", "@ipld/dag-cbor", "cborg"],
  },
  build: {
    outDir: resolve(here, "../../www"),
    emptyOutDir: true,
    assetsDir: "assets",
  },
  test: {
    environment: "node",
    globals: true,
  },
});
