/**
 * Browsing the files inside a restore point.
 *
 * Recovering one file used to mean restoring a whole machine and then going
 * looking. This lists what is actually in a restore point and hands back a
 * single file, reading only the part of the backup that file occupies.
 *
 * Nothing here mutates a backup: everything is a read, and the download is a
 * plain link so the browser streams it to disk rather than holding a recovered
 * file in memory.
 */

import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import {
  ChevronRight,
  Download,
  File,
  FileText,
  Folder,
  FolderOpen,
  Home,
  Link2,
  Search,
  X,
} from 'lucide-react'
import { backupFileURL, browseBackup, errorMessage } from '../api'
import type { Backup, BackupEntry, ID } from '../api'
import { identityText } from './Identity'
import { Modal } from './Modal'
import { useToast } from './Toast'
import { Button, EmptyState, ErrorBlock, Num, SkeletonRows, StatusPill } from './ui'
import { cn } from '../lib/cn'
import { formatBytes, formatDateTime } from '../lib/format'
import { sourceVMID } from '../api'

/** Extensions worth a different icon — enough to scan a folder, not a taxonomy. */
const TEXTUAL = /\.(txt|log|md|conf|cfg|ini|ya?ml|json|xml|csv|sql|sh|ps1|bat|env)$/i

function EntryIcon({ entry }: { entry: BackupEntry }) {
  if (entry.dir) return <Folder className="size-4 shrink-0 text-accent-400" aria-hidden />
  if (entry.link) return <Link2 className="size-4 shrink-0 text-slate-500" aria-hidden />
  if (TEXTUAL.test(entry.name)) {
    return <FileText className="size-4 shrink-0 text-slate-500" aria-hidden />
  }
  return <File className="size-4 shrink-0 text-slate-500" aria-hidden />
}

/** The path split into clickable ancestors. */
function Breadcrumb({ path, onGo }: { path: string; onGo: (p: string) => void }) {
  const parts = path ? path.split('/').filter(Boolean) : []
  return (
    <nav className="flex flex-wrap items-center gap-0.5 text-[13px]" aria-label="Folder path">
      <button
        type="button"
        onClick={() => onGo('')}
        className="inline-flex items-center gap-1.5 rounded px-1.5 py-1 text-slate-400 transition-colors duration-150 hover:bg-slate-800/60 hover:text-slate-200"
      >
        <Home className="size-3.5" aria-hidden />
        Backup root
      </button>
      {parts.map((part, i) => {
        const upto = parts.slice(0, i + 1).join('/')
        const last = i === parts.length - 1
        return (
          <span key={upto} className="flex items-center gap-0.5">
            <ChevronRight className="size-3.5 shrink-0 text-slate-600" aria-hidden />
            <button
              type="button"
              onClick={() => onGo(upto)}
              aria-current={last ? 'page' : undefined}
              className={cn(
                'max-w-[16rem] truncate rounded px-1.5 py-1 transition-colors duration-150',
                last
                  ? 'font-medium text-slate-100'
                  : 'text-slate-400 hover:bg-slate-800/60 hover:text-slate-200',
              )}
            >
              {part}
            </button>
          </span>
        )
      })}
    </nav>
  )
}

export function FileBrowser({
  backup,
  onClose,
}: {
  /** The restore point to look inside; `null` keeps the dialog closed. */
  backup: Backup | null
  onClose: () => void
}) {
  const toast = useToast()
  const [path, setPath] = useState('')
  const [query, setQuery] = useState('')
  const [entries, setEntries] = useState<BackupEntry[]>([])
  const [truncated, setTruncated] = useState(false)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const searchRef = useRef<HTMLInputElement>(null)

  const backupId: ID | null = backup ? backup.id : null
  const searching = query.trim().length > 0

  /* Reset on every open: a browser that reopened onto the previous restore
     point's folder would be quietly showing another machine's files. */
  useEffect(() => {
    if (!backup) return
    setPath('')
    setQuery('')
    setEntries([])
    setTruncated(false)
    setError(null)
  }, [backup])

  const load = useCallback(
    async (dir: string, search: string, signal: AbortSignal) => {
      if (backupId === null) return
      setLoading(true)
      setError(null)
      try {
        const res = await browseBackup(backupId, {
          path: search ? undefined : dir,
          search: search || undefined,
          limit: 300,
        })
        if (signal.aborted) return
        setEntries(res.entries)
        setTruncated(res.truncated)
      } catch (err) {
        if (signal.aborted || (err instanceof DOMException && err.name === 'AbortError')) return
        setError(errorMessage(err))
        setEntries([])
      } finally {
        if (!signal.aborted) setLoading(false)
      }
    },
    [backupId],
  )

  /* Debounced, and cancelled on change: typing in the search box must not leave
     an earlier, slower response to land last and overwrite the current one. */
  useEffect(() => {
    if (backupId === null) return
    const ac = new AbortController()
    const delay = searching ? 250 : 0
    const timer = setTimeout(() => void load(path, query.trim(), ac.signal), delay)
    return () => {
      clearTimeout(timer)
      ac.abort()
    }
  }, [backupId, path, query, searching, load])

  const title = useMemo(() => {
    if (!backup) return ''
    return identityText({
      hostName: backup.hostName,
      name: backup.sourceName,
      vmid: backup.sourceKind === 'vm' ? sourceVMID(backup.sourceId) : null,
      node: backup.node,
    })
  }, [backup])

  const onDownload = (entry: BackupEntry) => {
    if (backupId === null) return
    // A plain navigation, so the browser streams it to disk and shows its own
    // download UI. Anything else would buffer a recovered file in the tab.
    window.location.href = backupFileURL(backupId, entry.path)
    toast.info(
      'Downloading ' + entry.name,
      `Only the part of the backup holding this file is read — ${formatBytes(entry.size)}.`,
    )
  }

  const open = (entry: BackupEntry) => {
    if (entry.dir) {
      setQuery('')
      setPath(entry.path)
      return
    }
    onDownload(entry)
  }

  return (
    <Modal
      open={backup !== null}
      onClose={onClose}
      title="Browse files"
      subtitle={
        backup
          ? `${title} · restore point from ${formatDateTime(backup.createdAt)}`
          : undefined
      }
      width="xl"
      footer={
        <div className="flex w-full items-center justify-between gap-3">
          <p className="text-meta text-slate-500">
            Downloading a file reads only the part of the backup it occupies — the restore point
            is not modified.
          </p>
          <Button onClick={onClose}>Close</Button>
        </div>
      }
    >
      <div className="flex flex-col gap-3">
        <div className="flex flex-wrap items-center justify-between gap-3">
          <Breadcrumb path={searching ? '' : path} onGo={setPath} />
          <div className="relative min-w-[13rem] flex-1 sm:max-w-xs">
            <Search
              className="pointer-events-none absolute top-1/2 left-2.5 size-3.5 -translate-y-1/2 text-slate-500"
              aria-hidden
            />
            <input
              ref={searchRef}
              type="search"
              value={query}
              onChange={(e) => setQuery(e.target.value)}
              placeholder="Search this restore point…"
              aria-label="Search files in this restore point"
              className="w-full rounded-lg border border-slate-800 bg-slate-950/60 py-1.5 pr-7 pl-8 text-[13px] text-slate-200 placeholder:text-slate-600 focus:border-accent-500/50 focus:outline-none"
            />
            {query ? (
              <button
                type="button"
                onClick={() => {
                  setQuery('')
                  searchRef.current?.focus()
                }}
                aria-label="Clear search"
                className="absolute top-1/2 right-1.5 -translate-y-1/2 rounded p-1 text-slate-500 hover:text-slate-300"
              >
                <X className="size-3.5" aria-hidden />
              </button>
            ) : null}
          </div>
        </div>

        {truncated ? (
          <p className="rounded-lg border border-warn-500/30 bg-warn-500/10 px-3 py-2 text-meta text-warn-200">
            This restore point holds more files than can be listed at once, so the listing is
            incomplete. Search still finds files that are not shown.
          </p>
        ) : null}

        <div className="min-h-[18rem] rounded-lg border border-slate-800 bg-slate-950/40">
          {loading && entries.length === 0 ? (
            <SkeletonRows count={7} />
          ) : error ? (
            <ErrorBlock
              message={error}
              onRetry={() => {
                const ac = new AbortController()
                void load(path, query.trim(), ac.signal)
              }}
            />
          ) : entries.length === 0 ? (
            <EmptyState
              className="border-0 bg-transparent"
              icon={<FolderOpen className="size-5" aria-hidden />}
              title={searching ? 'Nothing matched' : 'This folder is empty'}
              description={
                searching
                  ? 'No file in this restore point has that in its name.'
                  : 'The backup recorded this folder with nothing in it.'
              }
            />
          ) : (
            <ul className="divide-y divide-slate-800/70">
              {entries.map((entry) => (
                <li key={entry.path}>
                  <div className="flex items-center gap-3 px-3 py-2">
                    <button
                      type="button"
                      onClick={() => open(entry)}
                      className="flex min-w-0 flex-1 items-center gap-2.5 rounded text-left"
                      title={entry.path}
                    >
                      <EntryIcon entry={entry} />
                      <span className="min-w-0 flex-1">
                        <span className="block truncate text-[13px] text-slate-200">
                          {entry.name}
                        </span>
                        {/* In search results the folder is the useful part —
                            the name alone does not say which one it was in. */}
                        {searching ? (
                          <span className="mt-0.5 block truncate font-mono text-micro text-slate-600">
                            {entry.path}
                          </span>
                        ) : entry.link ? (
                          <span className="mt-0.5 block truncate font-mono text-micro text-slate-600">
                            → {entry.link}
                          </span>
                        ) : null}
                      </span>
                    </button>

                    {entry.dir ? (
                      <StatusPill tone="neutral" label="folder" />
                    ) : (
                      <>
                        <span className="shrink-0 text-meta text-slate-500">
                          <Num>{formatBytes(entry.size)}</Num>
                        </span>
                        <Button
                          size="sm"
                          icon={<Download className="size-3.5" aria-hidden />}
                          onClick={() => onDownload(entry)}
                          aria-label={`Download ${entry.name}`}
                        >
                          Download
                        </Button>
                      </>
                    )}
                  </div>
                </li>
              ))}
            </ul>
          )}
        </div>
      </div>
    </Modal>
  )
}
