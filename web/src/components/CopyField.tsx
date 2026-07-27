import { useState } from 'react'
import { Check, Copy } from 'lucide-react'
import { cn } from '../lib/cn'
import { useToast } from './Toast'
import { IconButton } from './ui'

async function copyText(text: string): Promise<boolean> {
  try {
    if (navigator.clipboard?.writeText) {
      await navigator.clipboard.writeText(text)
      return true
    }
  } catch {
    /* fall through to the textarea fallback */
  }
  try {
    const area = document.createElement('textarea')
    area.value = text
    area.setAttribute('readonly', '')
    area.style.position = 'fixed'
    area.style.opacity = '0'
    document.body.appendChild(area)
    area.select()
    const ok = document.execCommand('copy')
    document.body.removeChild(area)
    return ok
  } catch {
    return false
  }
}

export function CopyButton({
  value,
  label = 'Copy',
  className,
}: {
  value: string
  label?: string
  className?: string
}) {
  const toast = useToast()
  const [copied, setCopied] = useState(false)

  const onCopy = async () => {
    const ok = await copyText(value)
    if (ok) {
      setCopied(true)
      window.setTimeout(() => setCopied(false), 1600)
      toast.success(`${label} copied to the clipboard.`)
    } else {
      toast.error('Could not copy', 'Select the text and copy it manually.')
    }
  }

  return (
    <IconButton
      variant="secondary"
      aria-label={label}
      title={label}
      onClick={() => void onCopy()}
      className={className}
    >
      {copied ? (
        <Check className="size-4 text-ok-400" aria-hidden />
      ) : (
        <Copy className="size-4" aria-hidden />
      )}
    </IconButton>
  )
}

/** A monospace, copyable block — used for install commands and tokens. */
export function CopyField({
  value,
  label,
  caption,
  copyLabel,
  className,
}: {
  value: string
  label?: string
  caption?: string
  copyLabel?: string
  className?: string
}) {
  return (
    <div className={cn('space-y-1.5', className)}>
      {label ? (
        <p className="text-xs font-medium tracking-wide text-slate-400">{label}</p>
      ) : null}
      <div className="flex items-start gap-2 rounded-lg border border-slate-800 bg-slate-950/70 p-2.5">
        <code className="min-w-0 flex-1 overflow-x-auto py-1 font-mono text-xs leading-relaxed whitespace-pre text-slate-300">
          {value}
        </code>
        <CopyButton value={value} label={copyLabel ?? label ?? 'Command'} />
      </div>
      {caption ? <p className="text-xs text-slate-500">{caption}</p> : null}
    </div>
  )
}
