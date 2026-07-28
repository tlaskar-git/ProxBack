/**
 * How a storage target is named and recognised across the console.
 *
 * v0.6.0 gave targets two kinds, and an operator choosing a destination has to
 * be able to tell at a glance whether the data lands on a disk in the building
 * or in a bucket somewhere else. So the kind travels with the target everywhere
 * it is displayed — the targets page, the job wizard, restore — rather than
 * only on the page where it was created.
 */

import { Cloud, HardDrive } from 'lucide-react'
import type { LucideIcon } from 'lucide-react'
import { capacityOf, TARGET_KIND_LABEL, TARGET_KIND_SHORT } from '../api'
import type { Target, TargetKind } from '../api'
import { formatBytes } from '../lib/format'
import { Chip, Num } from './ui'

export const TARGET_KIND_ICON: Record<TargetKind, LucideIcon> = {
  filesystem: HardDrive,
  s3: Cloud,
}

/** Kind badge. Slate like every other metadata chip — colour is state's job. */
export function TargetKindChip({ kind }: { kind: TargetKind }) {
  const Icon = TARGET_KIND_ICON[kind]
  return (
    <Chip
      mono={false}
      icon={<Icon className="size-3" aria-hidden />}
      title={TARGET_KIND_LABEL[kind]}
    >
      {TARGET_KIND_SHORT[kind]}
    </Chip>
  )
}

/**
 * Free space on a target, for a chooser. Renders nothing for anything without
 * capacity: a bucket is elastic and reports none, and a NAS is not, so the
 * figure belongs next to the choice rather than three pages away.
 */
export function TargetCapacityLine({ target }: { target: Target }) {
  const capacity = capacityOf(target)
  if (!capacity) return null
  return (
    <span
      className={
        capacity.low
          ? 'block text-meta text-warn-300'
          : 'block text-meta text-slate-500'
      }
    >
      <Num>{formatBytes(capacity.freeBytes)}</Num> free of{' '}
      <Num>{formatBytes(capacity.totalBytes)}</Num>
      {capacity.low ? ' — running low' : ''}
    </span>
  )
}
