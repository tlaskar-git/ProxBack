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
  status pills, chips, segmented control, progress bars, arc gauge, sparkline,
  modal, toasts, confirm dialog, job wizard).
  - `ScheduleEditor.tsx` — the whole scheduling control: cadence, time, days.
  - `RunLive.tsx` — the live run card and the per-source breakdown, shared by
    the Monitor page and the run detail modal.
- `src/pages/` — one file per console page.
- `src/lib/` — formatting helpers and the async/polling hooks.

Running jobs are polled every 2 seconds on the Dashboard, Backup Jobs, and
Monitor pages.

## Visual system

- **Elevation says importance.** `Card` takes `elevation`: `flat` for reference
  material and tables, `raised` (default) for the surfaces a page is built
  from, `feature` for the one card that answers the page's question. The
  `elev-1/2/3` utilities each carry a 1px inner highlight along the top edge —
  that is what stops a dark card reading as a flat rectangle.
- **Wells are recessed.** The `well` utility (inset shadow, darker fill) marks
  anything that sits *inside* a card: source lists, log tails, metric strips.
- **Type scale.** `text-micro` (10px) for eyebrows and unit suffixes,
  `text-meta` (11px) for chips, table meta and captions, `text-[13px]` for body
  rows, `text-xl` for page titles. Group labels go through the `eyebrow`
  utility; large figures through `figure-lg` (mono, tabular, tightened).
- **Sections, not stacks.** `SectionHeading` (eyebrow + rule to the right edge)
  separates blocks on a page. Tables use `text-micro font-semibold uppercase`
  headers everywhere.
- **Colour is spent, not sprinkled.** Red/amber/green mean state and nothing
  else: the dashboard verdict cell, a failing run, a guest no enabled job
  covers. Everything else stays slate with the emerald accent.

## House rules

- Every quantity — bytes, counts, durations, ratios, percentages — renders
  through `<Num>` (`font-mono tabular-nums`) so columns line up and polled
  values never reflow the row.
- `<Chip>` is the one metadata chip shape (Proxmox tags, a job's tag filter,
  the VM tag filter row). Chips stay slate; color belongs to `StatusPill`, so
  no row carries more than one accent-colored element.
- Empty states go through the `EmptyState` primitive: icon, one-line
  explanation, one call to action, optional `hint` for the secondary route out.
- **No cron in the normal path.** Schedules are edited through
  `ScheduleEditor` — cadence tiles, a time control, weekday toggles, a
  month grid — and confirmed in one English sentence with the next fire time
  and the server's timezone. The cron field lives behind a closed `<details>`
  marked "Advanced" and is never the default. `Job.schedule` is the v0.4.0
  object; read it through `parseSchedule` and display the server's
  `scheduleLabel` verbatim when it is present.
- Icon-only destructive actions use `variant="dangerQuiet"` — slate until
  hover, then red.
- Color transitions are `transition-colors duration-150`; the global
  `prefers-reduced-motion` block in `index.css` mutes decorative motion.
