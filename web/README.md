# incus-backup web console

The management UI: Vue 3 + Vite + vue-router, TypeScript (`<script setup lang="ts">`), no state
library (small reactive stores), no chart or UI library, no runtime CDN. `npm run build` writes into
`../internal/server/webui/dist`, which the server binary embeds (`go:embed`) and serves on the web
listener next to `/api/v1`.

```
just web          # npm ci + build (do this before `just build` to ship the UI)
just web-dev      # Vite on :5173, /api proxied to http://127.0.0.1:8099 (demo server); IB_API=https://host:8443 overrides
npm run typecheck # vue-tsc
npm test          # vitest (unit tests, jsdom)
npm run e2e       # Playwright (see "Testing")
```

Node ≥ 22. The contract for every API call is `docs/api.md`; the role of each route is
`internal/server/mgmt/perms.go`.

## Layout

```
src/
  main.ts                 boot: fonts, tokens, theme, 401 handling, idle watch, error handler
  App.vue                 bare pages (sign-in, setup) or the AppShell
  router/routes.ts        EVERY page: name, path, meta (title, role, section, parent, apis, planned, file)
  router/nav.ts           sidebar groups (route names + icons)
  router/index.ts         guards: setup -> sign-in -> forced password change; canOpen(name)
  api/client.ts           fetch wrapper: JSON, CSRF header, ApiError, 401 hook, ?repository=, abort
  api/endpoints.ts        `api.*`: one typed function per route (planned routes under api.draft)
  api/types.ts            wire types for every route (exact Go JSON names); Draft* = planned routes
  api/perms.ts            role per route, mirrored from perms.go (perms.test.ts fails on drift)
  api/events.ts           SSE: one EventSource, reconnect with backoff, typed bus, `live` state
  api/cache.ts            useResource(): stale-while-revalidate for lists; invalidate(prefix)
  stores/                 session, repos (selected repository), instance, theme, toast
  components/             shared components (see /_styleguide in the running console)
  components/app/         the shell: Sidebar, TopBar, CommandPalette, Breadcrumbs, LiveIndicator, …
  components/charts/      AreaChart, BarChart, StackedBar, Sparkline (own SVG)
  components/form/        FormField, SelectInput, SwitchInput, TagInput, PasswordInput
  components/icons/       Icon.vue + the Lucide icons in use (import per icon)
  composables/            useNow, useClipboard/downloadText, useConfirm, useDismiss, useElementWidth
  lib/                    pure logic with unit tests: format, table, chart, status, meter, password, fuzzy
  views/<section>/        one file per page (section agents replace the placeholders)
  styles/tokens.css       design tokens (light + dark)
  styles/base.css         element defaults + .btn .input .panel .page-head …
e2e/                      Playwright: backend.ts starts the servers; real.spec.ts, demo.spec.ts
public/theme-init.js      sets data-theme before first paint (a file: the CSP forbids inline script)
```

## Building a section (for section agents)

1. **Find your route** in `src/router/routes.ts`. Every page already exists and points at a placeholder
   in `src/views/<section>/` (e.g. `hosts/HostList.vue`). The placeholder shows the route, its params,
   the required role and the API routes it is expected to use. Replace the file's content; keep the
   file name, route name, path and meta. Adding a page: add a route (with `meta.title`, `meta.role`,
   `meta.section` for the sidebar highlight, `meta.parent` for the breadcrumb, `meta.file`) and a view
   file; the router fails at start if the file is missing.
2. **Start with the page header**: `<header class="page-head"><div><h1>…</h1><p class="lede">…</p></div><div class="actions">…</div></header>`.
3. **Data**: call `api.*` from `@/api/endpoints` only (never `fetch`). For lists use
   `useResource(() => \`hosts:${repos.current}\`, (s) => api.hosts.list(s), { refreshOn: ['host'] })`:
   the key must include the repository for repository-scoped routes; `refreshOn` lists the SSE event
   types that should refetch it. After a mutation call `invalidate('hosts:')` (or `resource.refresh()`).
   Lists may be `null` when empty: use `list(x)`. Routes needing a repository get `?repository=` from
   the top-bar selector automatically; the shell already shows a notice when no repository exists.
   Planned routes (`api.draft.*`) may answer 404 until their backend lands: show an EmptyState that
   says so, not an error toast.
4. **Roles**: hide or disable what the user cannot do: `can(session.role, 'PUT /policies/{name}')`
   (from `@/api/perms`) or `hasRole('operator')` (from `@/stores/session`). The server enforces anyway;
   the UI must not offer buttons that will answer 403. Prefer hiding for whole sections, disabling
   with a tooltip ("Needs the operator role") for single actions on a page the user can see.
5. **Components**: use the shared ones (open `/_styleguide`): `DataTable` for every list (sorting,
   filter, columns, CSV, selection, loading/empty/error states, windowing above 300 rows),
   `StatusPill state="…"` for any server state word (mapping in `lib/status.ts`), `RelativeTime` /
   `AbsoluteTime` for every timestamp, `ByteSize`, `DurationText` (Go durations are nanoseconds: `:ns`),
   `KeyValue` on detail pages, `CopyField`/`CodeBlock` for tokens and commands, `Modal` (or `drawer`),
   `confirm({ …, danger: true, typeToConfirm: name })` before anything destructive, `Tabs` for
   sub-pages, `Stepper` for wizards, `Callout` for inline errors, `toast.success/error` for results.
   Formatting is in `lib/format.ts` (bytes, durations, times, `describeSchedule()`/`parseSchedule()` for
   the schedule language of `internal/schedule`). Put new reusable logic in `lib/` with a `*.test.ts`.
6. **Never** keep secrets anywhere but component memory: tokens shown once (`CopyField once`), and the
   browser-side decryption key on the snapshot browser lives in RAM only, is never sent to the server and
   never stored (no localStorage, no URL).

## Design tokens

All colours, sizes and radii are CSS variables in `styles/tokens.css` (light in `:root`, dark in
`:root[data-theme='dark']`). Never write a raw colour in a component.

* Neutrals: `--bg` (page), `--surface`, `--surface-2` (table header, subtle fill), `--surface-3`
  (hover), `--border`, `--border-strong`, text `--text`, `--text-2`, `--text-muted`, `--text-faint`.
* Accent (interaction only: primary buttons, links, focus, selection): `--accent`, `--accent-soft`,
  `--accent-text`, `--focus`.
* Status (state only, always with an icon or a word): `--ok|warn|critical|info|neutral` plus `-bg`,
  `-border`, `-text` variants.
* Charts: `--series-1..4` in fixed order (validated for colour-vision deficiency in both themes),
  `--series-other` for the fold-in; never status colours as series; one measure per chart (no dual axes).
* Type: `--font-sans` (IBM Plex Sans), `--font-mono` (IBM Plex Mono); `--fs-xs … --fs-3xl`; tabular
  numerals are on for tables and `.num`. Body text is 13.5 px: the console is dense on purpose.
* Space on a 4 px grid (`--s-1 … --s-10`), radii `--r-sm` 3 px, `--r` 5 px, `--r-lg` 7 px.
* Motion is short and respects `prefers-reduced-motion`; focus is always visible (`:focus-visible`).
* Layout works down to 360 px: the sidebar becomes a drawer under 900 px; no horizontal page scroll
  (tables scroll inside their own box).

## Copy rules (English only, no i18n layer)

* Plain, specific, active voice. "Sign in", not "Login"; "Add a host", not "Host creation".
* Errors say what went wrong and how to fix it: "The repository is read-only. Set it online on the
  repository page." The server's `{error}` text is shown as is; add the next step when you know it.
* Toast titles say what happened ("Policy saved"), error titles what failed ("Could not save the policy").
* Empty states say what is missing and give the one next action.
* Numbers with units (`1.5 GiB`, `3 min ago`), ISO dates (`2026-10-08 14:03`), times in the browser's
  zone (the tooltip says so and gives UTC).
* No emoji, no exclamation marks, no "Oops".

## Testing

* `npm test`: vitest unit tests for everything in `lib/`, the API client, SSE client, cache and the
  perms mirror (which reads `internal/server/mgmt/perms.go`).
* `npm run e2e`: `e2e/global-setup.ts` builds the UI, builds the REAL `incus-backup-server` with the UI
  embedded, starts it on a fresh PostgreSQL (binaries from the embedded-postgres cache in
  `~/.cache/incus-backup-embedded-postgres`) over HTTPS on 127.0.0.1:18443, and runs `real.spec.ts`
  (first-run setup, every section, theme, palette, 2FA enrolment with a TOTP computed in the test,
  sign-in with TOTP and recovery code, session revoked elsewhere, API tokens; fails on any CSP
  violation or console error). If `/tmp/ibwork/demo-bin/ibdemo` (or `IB_DEMO_BIN`) exists, it also
  starts the seeded demo server on 127.0.0.1:18199 for `demo.spec.ts` (roles, repository selector,
  palette with live data, 1500-row table, screenshots light/dark × 1440×900/390×844 into
  `e2e/screenshots/`). `--project real` skips the demo. Needs `go` on PATH.
  Env: `IB_E2E_SKIP_BUILD=1` (reuse dist), `IB_SERVER_BIN`, `IB_E2E_PORT`, `IB_E2E_DEMO_PORT`,
  `IB_CHROMIUM_ROOT` (unpacked Debian libraries for containers without Chromium's system libraries).
