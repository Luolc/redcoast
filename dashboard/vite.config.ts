import tailwindcss from "@tailwindcss/vite";
import react from "@vitejs/plugin-react";
import type { Plugin } from "vite";
import { defineConfig } from "vitest/config";
import { sampleDashboard } from "./fixtures/sample.ts";

// `pnpm dev` only: made-up data at ./dashboard.json, so the build never
// carries it.
function sample(): Plugin {
  return {
    name: "sample-data",
    apply: "serve",
    configureServer(server) {
      server.middlewares.use("/dashboard.json", (_req, res) => {
        res.setHeader("Content-Type", "application/json");
        res.end(JSON.stringify(sampleDashboard(new Date())));
      });
    },
  };
}

// The build is one index.html with the script and the styles inline, so a
// deployment carries a single file that the install script checks by sha256.
function singleFile(): Plugin {
  return {
    name: "single-file",
    apply: "build",
    enforce: "post",
    generateBundle(_options, bundle) {
      const page = bundle["index.html"];
      if (page?.type !== "asset" || typeof page.source !== "string") {
        throw new Error("the bundle has no index.html");
      }
      let html = page.source;
      for (const [name, output] of Object.entries(bundle)) {
        if (output === page) continue;
        const tag = new RegExp(
          `<(script|link)[^>]*"\\./${name.replaceAll(".", "\\.")}"[^>]*>(</script>)?`,
        );
        if (!tag.test(html))
          throw new Error(`index.html does not reference ${name}`);
        // A replacer function: the inlined code may contain $ patterns.
        if (output.type === "chunk") {
          const code = output.code.replaceAll("</script", "<\\/script");
          html = html.replace(
            tag,
            () => `<script type="module">${code}</script>`,
          );
        } else if (name.endsWith(".css") && typeof output.source === "string") {
          const css = output.source;
          html = html.replace(tag, () => `<style>${css}</style>`);
        } else {
          throw new Error(`cannot inline ${name}`);
        }
        Reflect.deleteProperty(bundle, name);
      }
      page.source = html;
    },
  };
}

export default defineConfig({
  // Relative asset paths: the gateway serves the page next to dashboard.json.
  base: "./",
  plugins: [react(), tailwindcss(), sample(), singleFile()],
  // The commit the page is built from, which build.sh passes in; empty in
  // `pnpm dev` and the tests.
  define: {
    __DASHBOARD_COMMIT__: JSON.stringify(process.env["DASHBOARD_COMMIT"] ?? ""),
  },
  test: {
    // A zone that is neither of the two the page shows, so a test that
    // leans on the machine's local time fails everywhere alike.
    env: { TZ: "Asia/Shanghai" },
  },
});
