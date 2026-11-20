// Flat config (ESLint 9/10). Lives with the app; run from web/ via `npm run lint`.
// Also runnable from the repo root (pre-commit does this):
//   eslint -c web/eslint.config.mjs <files>
// so this file must stay CWD-agnostic: no root-relative `files:` patterns,
// only `**`-scoped ones. Prettier owns formatting; eslint-config-prettier
// disables the style rules that would fight it.
//
// DORMANT as of 2026-10-12: typescript-eslint v8 hard-errors on this repo's
// TypeScript 7 (`typescript-eslint does not support TS 7.0`), so `npm run
// lint` cannot execute until upstream supports TS >= 7.1
// (https://github.com/typescript-eslint/typescript-eslint/issues/10940)
// or the repo moves back to a supported TS. The config is kept correct and
// ready; CI and pre-commit do NOT run it until then (see README Tooling).
// Type safety is covered meanwhile by `tsc --noEmit` (strict).
import js from '@eslint/js'
import tseslint from 'typescript-eslint'
import reactHooks from 'eslint-plugin-react-hooks'
import reactRefresh from 'eslint-plugin-react-refresh'
import prettierConfig from 'eslint-config-prettier'
import globals from 'globals'

export default [
  {
    ignores: ['**/dist/**', '**/node_modules/**', '**/coverage/**'],
  },
  js.configs.recommended,
  ...tseslint.configs.recommended,
  reactHooks.configs.flat.recommended,
  {
    plugins: { 'react-refresh': reactRefresh },
    rules: { 'react-refresh/only-export-components': 'warn' },
  },
  prettierConfig,
  {
    languageOptions: {
      globals: { ...globals.browser },
    },
  },
  // Config files and tooling run under Node, not the browser.
  {
    files: ['**/*.config.{js,mjs,cjs,ts}'],
    languageOptions: {
      globals: { ...globals.node },
    },
  },
]
