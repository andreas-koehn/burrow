import js from '@eslint/js'
import globals from 'globals'
import reactHooks from 'eslint-plugin-react-hooks'
import reactRefresh from 'eslint-plugin-react-refresh'
import tseslint from 'typescript-eslint'
import { defineConfig, globalIgnores } from 'eslint/config'

export default defineConfig([
  globalIgnores(['dist', 'playwright.config.ts', 'e2e/**']),
  {
    files: ['**/*.{ts,tsx}'],
    extends: [
      js.configs.recommended,
      tseslint.configs.recommended,
      reactHooks.configs.flat.recommended,
      reactRefresh.configs.vite,
    ],
    languageOptions: {
      globals: globals.browser,
    },
    rules: {
      // eslint-plugin-react-hooks v7 added NEW strict, on-by-default rules that
      // flag patterns we use deliberately throughout the dashboard. We downgrade
      // the noisy advisory ones to `warn` (NOT off) so `eslint` (which only fails
      // on errors) passes in CI while we keep the signal visible in editors.
      // The important hooks rules (rules-of-hooks, exhaustive-deps) stay errors.
      //
      //  - set-state-in-effect: ~14 call sites that seed local UI draft/selection
      //    state once a react-query result arrives (`if (data && !draft) setDraft(data)`).
      //    These are intentional "sync server state into local editable copy"
      //    effects, guarded so they run once; the rule's cascading-render concern
      //    does not apply here. Refactoring all of them under CI pressure is the
      //    riskier option, so they stay as advisory warnings.
      'react-hooks/set-state-in-effect': 'warn',
      //  - refs: Dialog.tsx assigns the latest `onOpenChange` to a ref during
      //    render (the documented "latest ref" pattern) to keep the focus-trap
      //    effect from re-firing on every parent re-render. Intentional and
      //    heavily commented in-place.
      'react-hooks/refs': 'warn',
      // react-refresh/only-export-components: CustomDomains.tsx co-exports a
      // couple of pure helpers (statusBadgeKind, STATUS_LABEL) next to its
      // component. This only affects Vite Fast Refresh DX in dev, never runtime
      // or correctness, so it is advisory rather than a CI-blocking error.
      'react-refresh/only-export-components': 'warn',
      // no-useless-assignment (from js.configs.recommended): Tunnels.tsx and
      // Services.tsx initialize `let cmp = 0` in a sort comparator before an
      // exhaustive if/else assigns it. The initializer is dead but harmless and
      // self-documenting; demoted to a warning to keep the comparators as-is
      // rather than risk touching sort logic / the built dist.
      'no-useless-assignment': 'warn',
      // Allow underscore-prefixed identifiers to be intentionally unused. The
      // mock handlers use `({ services: _services, ...v }) => v` and
      // `const { user_id: _u, ...wire }` to strip fields off response objects;
      // the leading underscore is the conventional "intentionally discarded"
      // marker.
      '@typescript-eslint/no-unused-vars': [
        'error',
        {
          argsIgnorePattern: '^_',
          varsIgnorePattern: '^_',
          destructuredArrayIgnorePattern: '^_',
          ignoreRestSiblings: true,
        },
      ],
    },
  },
  // Test files use `as any` extensively for mocking — suppress for those files only.
  {
    files: ['**/*.test.{ts,tsx}'],
    rules: {
      '@typescript-eslint/no-explicit-any': 'off',
    },
  },
])
