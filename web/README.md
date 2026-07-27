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
  It also owns the normalisers the rest of the app reads through:
  `parseSchedule`, `parseRetention`, `parsePolicy`, `reductionOf`.
- `src/theme.tsx` — light / system / dark, persisted, applied as `data-theme`.
- `src/App.tsx` — auth gate: `GET /api/setup/status`, then `GET /api/me`,
  routing to Setup, Login, or the authenticated shell.
- `src/components/` — layout shell plus the shared primitives (buttons, cards,
  status pills, chips, segmented control, progress bars, arc gauge, sparkline,
  modal, toasts, confirm dialog, disclosure, definition list).
  - `Brand.tsx` — `BrandMark` / `BrandLockup`. The only identity in the app;
    there is no stock icon standing in for a logo anywhere.
  - `Identity.tsx` — `WorkloadIdentity` / `identityText`, the canonical
    `cluster / name (vmid) / node`.
  - `ScheduleEditor.tsx` — the whole scheduling control: cadence, time, days.
  - `RetentionStep.tsx` — keep-last plus the GFS disclosure and the live
    keep-vs-prune preview.
  - `PolicyStep.tsx` — the optional Advanced protection step and the
    `PolicySummary` chips shown on Review and on the job card.
  - `RestoreWizard.tsx` — Mode → Destination → Review.
  - `HelpersSection.tsx` — node helpers, keyed by (cluster, node).
  - `RunLive.tsx` — the live run card and the per-workload breakdown, shared by
    the Monitor page and the run detail modal.
- `src/pages/` — one file per console page.
- `src/lib/` — formatting helpers and the async/polling hooks.

Running jobs are polled every 2 seconds on the Dashboard, Backup Jobs, and
Monitor pages.

## Colour

Two ramps, and they mean different things. Getting this wrong is what made the
old panel read as generic, because "green means brand" and "green means
healthy" were the same token.

- **`accent-*` is the brand.** An iris/indigo. It is spent on selection, the
  primary action, focus rings, the mark, and "in flight". It is never used to
  say that something is well.
- **`ok-* / warn-* / fail-*` are state, and nothing else.** Green means
  *verified healthy*, amber means *at risk*, red means *failed*. No link, no
  button, no brand surface may borrow them.
- A run that is *running* carries the brand tone, not green: activity is not
  evidence of health.
- `PillTone` is named for meaning — `ok | warn | fail | brand | neutral` — so a
  call site cannot quietly pick a pigment.

## Themes

Light, Dark and System, defaulting to System and persisted in `localStorage`
under `proxback.theme`. `data-theme="light|dark"` on the root element
re-points every ramp variable, so a utility like `text-slate-400` keeps meaning
"secondary text" in either theme and no component branches on the theme.

- Every stop below 500 inverts; 500 and 600 stay saturated because they are
  fills (progress bars, dots, the primary button) that must read on either
  ground.
- Anything that is *not* a ramp member needs its own variable. `--pb-scrim` is
  the dialog scrim: derived from the slate ramp it would invert into a white
  veil over a white page.
- Never hard-code a colour. Chart fills and SVG strokes take
  `var(--color-*)` so they follow the switch too.
- The dim end of the slate ramp is re-pointed in *both* themes: Tailwind's
  stock `slate-500/600` sit at 3.9:1 and 2.5:1 on a near-black surface, which
  is decoration rather than text.
- Verify, do not assume: audit composited contrast in both themes after any
  colour change, including inside modals and wizards.

## Visual system

- **Sections, not stacks.** `SectionHeading` (eyebrow + rule to the right edge)
  separates blocks; tables and toolbars are preferred over nested translucent
  rounded boxes.
- **Elevation says importance.** `Card` takes `elevation`: `flat` for reference
  material and tables, `raised` (default) for the surfaces a page is built
  from, `feature` for the one card that answers the page's question.
- **Wells are recessed.** The `well` utility marks anything that sits *inside*
  a card: source lists, log tails, metric strips.
- **One control height.** Buttons, inputs, selects and icon buttons all use the
  `control-h` utility, so a filter row never staircases.
- **Type scale.** `text-micro` (10px) for eyebrows and unit suffixes,
  `text-meta` (11px) for chips, table meta and captions, `text-[13px]` for body
  rows, `text-xl` for page titles.

## House rules

- Every quantity — bytes, counts, durations, ratios, percentages — renders
  through `<Num>` (`font-mono tabular-nums`) so columns line up and polled
  values never reflow the row.
- **The console shows evidence, not opinion.** The dashboard verdict is the
  server's `GET /api/posture`; the UI renders `reasons[]` with their workload
  counts and never derives a verdict of its own. When posture cannot be
  evaluated it says so — it never produces a green light by omission.
- **Data reduction is defined once**, in `reductionOf`. Show the percentage
  always and the ratio *only when one exists*: a run that uploaded nothing is
  "100% avoided" and has no finite ratio, so `1.0×` must never appear.
- **Identity is `cluster / name (vmid) / node`** everywhere a workload is
  chosen or displayed — inventory, job sources, restore points, run sources.
  Two clusters can hold identically named guests and identically named nodes.
- **Verified means integrity verified**: the point was read back from the
  target and re-hashed. Restore testing is not implemented and is never
  implied.
- **Overwrite is never the default and never preselected.** The restore wizard
  defaults to restoring alongside on a server-suggested free VMID; overwrite is
  styled destructive throughout and blocked until the operator types the
  destination machine's current name.
- **No cron in the normal path.** Schedules are edited through
  `ScheduleEditor` and confirmed in one English sentence with the next fire
  time and the server's timezone. The cron field lives behind a closed
  `<details>` marked "Advanced" and is never the default.
- **Advanced disclosures over explanatory paragraphs.** Guidance that earns its
  place goes in `Hint` (one line); anything longer belongs inside a
  `Disclosure`. Defaults keep the simple case simple — a six-guest estate can
  ignore Advanced protection and GFS retention entirely.
- Empty states go through `EmptyState` and must be *state-specific*: say what
  this particular emptiness means and what to do about it.
- `<Chip>` is the one metadata chip shape. Chips stay slate; colour belongs to
  `StatusPill`, so no row carries more than one accent-coloured element.
- Icon-only destructive actions use `variant="dangerQuiet"` — slate until
  hover, then red — and always carry an `aria-label`.
- Color transitions are `transition-colors duration-150`; the global
  `prefers-reduced-motion` block in `index.css` mutes decorative motion.

## Terminology

Write for the operator, not for the engine.

| Say | Not |
| --- | --- |
| Workloads in this run | Objects in this session |
| Source data processed | Read |
| Transferred to target | Uploaded |
| Data reduction / avoided | Dedup |
| Backup and recovery for Proxmox VE | Backup & replication |

Chunks, manifests, garbage collection and API paths do not appear in default
(non-Advanced) UI copy.
