# ProxBack web control panel

React + Vite + TypeScript + Tailwind SPA for the ProxBack backup server.

```bash
npm install
npm run dev        # http://localhost:5173, proxies /api and /downloads to :8443
npm run build      # type-checks, then emits dist/
npm run typecheck  # tsc -b only
npm run lint       # oxlint
```

`npm run build` writes to `dist/`; the Go build copies that directory into
`internal/api/webdist` so the server can serve it through `go:embed`.

## Layout

- `src/api.ts` — the only place that talks to the network. One exported
  interface per entity and one function per endpoint in the PLAN.md REST
  contract. Every call sends `credentials: 'include'` and throws `ApiError`
  (carrying the server's `{"error"}` message plus the HTTP status) on failure.
- `src/App.tsx` — auth gate: `GET /api/setup/status`, then `GET /api/me`,
  routing to Setup, Login, or the authenticated shell.
- `src/components/` — layout shell plus the shared primitives (buttons, cards,
  status pills, chips, segmented control, progress bars, modal, toasts, confirm
  dialog, job wizard).
- `src/pages/` — one file per console page.
- `src/lib/` — formatting helpers and the async/polling hooks.

Running jobs are polled every 2 seconds on the Dashboard, Backup Jobs, and
Monitor pages.

## House rules

- Every quantity — bytes, counts, durations, ratios, percentages — renders
  through `<Num>` (`font-mono tabular-nums`) so columns line up and polled
  values never reflow the row.
- `<Chip>` is the one metadata chip shape (Proxmox tags, a job's tag filter,
  the VM tag filter row). Chips stay slate; color belongs to `StatusPill`, so
  no row carries more than one accent-colored element.
- Empty states go through the `EmptyState` primitive: icon, one-line
  explanation, one call to action.
- Icon-only destructive actions use `variant="dangerQuiet"` — slate until
  hover, then red.
- Color transitions are `transition-colors duration-150`; the global
  `prefers-reduced-motion` block in `index.css` mutes decorative motion.
