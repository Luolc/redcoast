import js from "@eslint/js";
import eslintReact from "@eslint-react/eslint-plugin";
import vitest from "@vitest/eslint-plugin";
import betterTailwind from "eslint-plugin-better-tailwindcss";
import jsxA11y from "eslint-plugin-jsx-a11y-x";
import reactHooks from "eslint-plugin-react-hooks";
import { reactRefresh } from "eslint-plugin-react-refresh";
import { defineConfig, globalIgnores } from "eslint/config";
import globals from "globals";
import tseslint from "typescript-eslint";

export default defineConfig([
  globalIgnores(["dist"]),
  {
    linterOptions: { reportUnusedDisableDirectives: "error" },
  },
  {
    files: ["**/*.{ts,tsx}"],
    extends: [
      js.configs.recommended,
      tseslint.configs.strictTypeChecked,
      tseslint.configs.stylisticTypeChecked,
      reactHooks.configs.flat.recommended,
      eslintReact.configs["strict-type-checked"],
      eslintReact.configs["disable-conflict-eslint-plugin-react-hooks"],
      jsxA11y.configs.strict,
      reactRefresh.configs.vite(),
      betterTailwind.configs.correctness,
    ],
    languageOptions: {
      globals: globals.browser,
      parserOptions: {
        projectService: true,
        tsconfigRootDir: import.meta.dirname,
      },
    },
    settings: {
      "better-tailwindcss": { entryPoint: "src/index.css" },
    },
    rules: {
      // disable-conflict switches off every react-hooks rule that has an
      // @eslint-react twin, but these three have none, so they stay on.
      "react-hooks/globals": "error",
      "react-hooks/immutability": "error",
      "react-hooks/refs": "error",
      "@typescript-eslint/switch-exhaustiveness-check": "error",
      // Only real booleans in conditions: 0 and "" are falsy too, which
      // hides an empty-versus-missing mix-up.
      "@typescript-eslint/strict-boolean-expressions": [
        "error",
        { allowNumber: false, allowString: false },
      ],
      "@typescript-eslint/no-unsafe-type-assertion": "error",
    },
  },
  {
    // Run by Node, not the browser: the Vite config and the sample data.
    files: ["vite.config.ts", "fixtures/**"],
    languageOptions: { globals: globals.node },
  },
  {
    files: ["**/*.test.{ts,tsx}"],
    extends: [vitest.configs.recommended],
  },
]);
