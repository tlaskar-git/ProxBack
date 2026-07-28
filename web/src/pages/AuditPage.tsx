/**
 * Audit.
 *
 * Roles without attribution are theatre, so every mutation is recorded: who
 * attempted what, against which object, and whether it was allowed. Admin-only,
 * newest first.
 *
 * A **denied** attempt is the reason this page exists — somebody tried
 * something their role does not permit — so denials are rendered distinctly
 * rather than as another quiet row. It stays a dense, scannable table: this is
 * read by eye, looking for the one line that does not belong.
 */

import { useCallback, useMemo, useState } from 'react'
import { Ban, Filter, RefreshCw, ScrollText, TriangleAlert, X } from 'lucide-react'
import { auditObjectText, listAudit } from '../api'
import type { AuditEntry, AuditResult } from '../api'
import { cn } from '../lib/cn'
import {
  Button,
  Card,
  CardHeader,
  EmptyState,
  ErrorBlock,
  Field,
  Hint,
  Input,
  Num,
  PageHeader,
  Select,
  SkeletonRows,
  StatusPill,
} from '../components/ui'
import type { PillTone } from '../components/ui'
import { useAsync } from '../lib/useAsync'
import { formatDateTime, formatRelative } from '../lib/format'

/** How many entries to ask for. The server keeps the newest 50,000. */
const LIMITS = [100, 250, 500, 1000, 5000] as const
const DEFAULT_LIMIT = 250

/**
 * `denied` is red because it is the finding, not the state of a machine — the
 * one place in the console where red does not mean "a backup failed". `error`
 * is amber: the attempt was permitted and did not complete.
 */
function toneForResult(result: AuditResult): PillTone {
  switch (result) {
    case 'ok':
      return 'ok'
    case 'denied':
      return 'fail'
    case 'error':
      return 'warn'
    default:
      return 'neutral'
  }
}

/** Splits `user.create` into something readable without losing the vocabulary. */
function actionText(action: string): string {
  return action ? action.replace(/[._]/g, ' · ') : '—'
}

function AuditRow({ entry }: { entry: AuditEntry }) {
  const denied = entry.result === 'denied'
  const errored = entry.result === 'error'

  return (
    <tr
      className={cn(
        'align-top transition-colors duration-150',
        denied
          ? // A denial gets a tinted row and a hard left edge, so it is visible
            // while scrolling past a hundred routine entries.
            'bg-fail-500/[0.07] hover:bg-fail-500/[0.12]'
          : 'hover:bg-slate-800/30',
      )}
    >
      <td
        className={cn(
          'py-2 pr-3 pl-5 whitespace-nowrap',
          denied && 'border-l-2 border-fail-500 pl-[18px]',
        )}
      >
        <Num
          className={cn('text-[13px]', denied ? 'text-fail-200' : 'text-slate-300')}
          title={formatDateTime(entry.at)}
        >
          {formatRelative(entry.at)}
        </Num>
      </td>
      <td className="px-3 py-2 whitespace-nowrap">
        <span className={cn('text-[13px]', denied ? 'font-medium text-fail-200' : 'text-slate-200')}>
          {entry.actor || '—'}
        </span>
      </td>
      <td className="px-3 py-2 whitespace-nowrap">
        <span className="font-mono text-meta text-slate-300">{actionText(entry.action)}</span>
      </td>
      <td className="max-w-[18rem] px-3 py-2">
        <span className="block truncate text-[13px] text-slate-400" title={auditObjectText(entry)}>
          {auditObjectText(entry)}
        </span>
      </td>
      <td className="px-3 py-2 whitespace-nowrap">
        <span className="inline-flex items-center gap-1.5">
          {denied ? <Ban className="size-3.5 shrink-0 text-fail-400" aria-hidden /> : null}
          {errored ? (
            <TriangleAlert className="size-3.5 shrink-0 text-warn-400" aria-hidden />
          ) : null}
          <StatusPill tone={toneForResult(entry.result)} label={entry.result || 'unknown'} />
        </span>
      </td>
      <td className="px-3 py-2 whitespace-nowrap">
        <Num className="text-meta text-slate-500">{entry.sourceIP || '—'}</Num>
      </td>
      <td className="max-w-[20rem] py-2 pr-5 pl-3">
        {entry.detail ? (
          <span className="block truncate text-meta text-slate-500" title={entry.detail}>
            {entry.detail}
          </span>
        ) : null}
      </td>
    </tr>
  )
}

export function AuditPage() {
  /** Applied filters — the inputs are staged and committed, not live-polled. */
  const [action, setAction] = useState('')
  const [actor, setActor] = useState('')
  const [limit, setLimit] = useState<number>(DEFAULT_LIMIT)
  const [actionDraft, setActionDraft] = useState('')
  const [actorDraft, setActorDraft] = useState('')

  const loader = useCallback(
    (): Promise<AuditEntry[]> => listAudit({ limit, action: action || undefined, actor: actor || undefined }),
    [limit, action, actor],
  )
  const { data, loading, error, reload } = useAsync(loader)

  const entries = useMemo(() => data ?? [], [data])
  const denied = entries.filter((entry) => entry.result === 'denied').length

  /**
   * Suggestions come from what this server has actually recorded rather than
   * from a hard-coded vocabulary, so a filter never offers an action the server
   * has never heard of.
   */
  const actionOptions = useMemo(
    () => [...new Set(entries.map((entry) => entry.action).filter(Boolean))].sort(),
    [entries],
  )
  const actorOptions = useMemo(
    () => [...new Set(entries.map((entry) => entry.actor).filter(Boolean))].sort(),
    [entries],
  )

  const filtered = action !== '' || actor !== ''

  const apply = () => {
    setAction(actionDraft.trim())
    setActor(actorDraft.trim())
  }

  const clear = () => {
    setActionDraft('')
    setActorDraft('')
    setAction('')
    setActor('')
  }

  return (
    <>
      <PageHeader
        title="Audit"
        description="Every sign-in and every change, newest first: who attempted it, what it touched, and whether it was allowed."
        actions={
          <Button
            icon={<RefreshCw className="size-4" aria-hidden />}
            onClick={() => void reload()}
            loading={loading}
          >
            Refresh
          </Button>
        }
      />

      <Card elevation="flat" className="mb-4">
        <div className="flex flex-wrap items-end gap-3 px-5 py-4">
          <Field label="Action" className="w-full sm:w-52">
            {({ id }) => (
              <>
                <Input
                  id={id}
                  list="audit-actions"
                  value={actionDraft}
                  placeholder="user.create"
                  spellCheck={false}
                  onChange={(event) => setActionDraft(event.target.value)}
                  onKeyDown={(event) => {
                    if (event.key === 'Enter') apply()
                  }}
                />
                <datalist id="audit-actions">
                  {actionOptions.map((option) => (
                    <option key={option} value={option} />
                  ))}
                </datalist>
              </>
            )}
          </Field>

          <Field label="Actor" className="w-full sm:w-52">
            {({ id }) => (
              <>
                <Input
                  id={id}
                  list="audit-actors"
                  value={actorDraft}
                  placeholder="jrivera"
                  spellCheck={false}
                  onChange={(event) => setActorDraft(event.target.value)}
                  onKeyDown={(event) => {
                    if (event.key === 'Enter') apply()
                  }}
                />
                <datalist id="audit-actors">
                  {actorOptions.map((option) => (
                    <option key={option} value={option} />
                  ))}
                </datalist>
              </>
            )}
          </Field>

          <Field label="Entries" className="w-full sm:w-32">
            {({ id }) => (
              <Select
                id={id}
                value={String(limit)}
                onChange={(event) => setLimit(Number(event.target.value))}
              >
                {LIMITS.map((option) => (
                  <option key={option} value={option}>
                    Newest {option}
                  </option>
                ))}
              </Select>
            )}
          </Field>

          <div className="flex items-center gap-2">
            <Button icon={<Filter className="size-4" aria-hidden />} onClick={apply}>
              Apply
            </Button>
            {filtered ? (
              <Button
                variant="ghost"
                icon={<X className="size-4" aria-hidden />}
                onClick={clear}
                aria-label="Clear the action and actor filters"
              >
                Clear
              </Button>
            ) : null}
          </div>
        </div>
      </Card>

      {loading && !data ? (
        <Card>
          <SkeletonRows count={8} />
        </Card>
      ) : error && !data ? (
        <ErrorBlock message={error} onRetry={() => void reload()} />
      ) : entries.length === 0 ? (
        <EmptyState
          icon={<ScrollText className="size-5" aria-hidden />}
          title={filtered ? 'Nothing matches these filters' : 'Nothing recorded yet'}
          description={
            filtered
              ? 'No entry matches this action and actor. Clear the filters to see the whole trail.'
              : 'The trail fills as people sign in and change things. An empty trail on a running server means nothing has been changed since it was installed.'
          }
          action={
            filtered ? (
              <Button variant="primary" onClick={clear}>
                Clear filters
              </Button>
            ) : (
              <Button variant="primary" onClick={() => void reload()}>
                Check again
              </Button>
            )
          }
        />
      ) : (
        <Card elevation="flat">
          <CardHeader
            title="Activity"
            subtitle={
              denied > 0
                ? `${entries.length} entries · ${denied} refused because of the actor's role`
                : `${entries.length} entries · none refused`
            }
          />
          <div className="overflow-x-auto">
            <table className="w-full min-w-[64rem]">
              <thead>
                <tr className="border-b border-slate-800 text-left text-micro font-semibold tracking-wide text-slate-500 uppercase">
                  <th className="py-2 pr-3 pl-5 font-medium">When</th>
                  <th className="px-3 py-2 font-medium">Actor</th>
                  <th className="px-3 py-2 font-medium">Action</th>
                  <th className="px-3 py-2 font-medium">Object</th>
                  <th className="px-3 py-2 font-medium">Result</th>
                  <th className="px-3 py-2 font-medium">Source IP</th>
                  <th className="py-2 pr-5 pl-3 font-medium">Detail</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-slate-800/70">
                {entries.map((entry) => (
                  <AuditRow key={String(entry.id)} entry={entry} />
                ))}
              </tbody>
            </table>
          </div>
          <div className="border-t border-slate-800/80 px-5 py-2.5">
            <Hint>
              The trail is append-only and keeps the newest <Num>50,000</Num> entries. Secrets are
              never recorded.
            </Hint>
          </div>
        </Card>
      )}
    </>
  )
}
