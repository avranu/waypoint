// A unified diff, as git and patch write it: line numbers, additions and
// removals, one hunk after another. Used for change requests (WP-D16); it
// takes the diff of one file, or a whole patch.

import { useMemo, useState } from 'react'
import clsx from 'clsx'
import { ChevronRight } from 'lucide-react'

export interface DiffLine {
  /** '+' added, '-' removed, ' ' context, '@' hunk header, '\\' a note */
  kind: '+' | '-' | ' ' | '@' | '\\'
  text: string
  old?: number
  new?: number
}

const HUNK = /^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@/

/** Reads a unified diff into lines; the ---/+++ file headers are left out. */
export function parseDiff(diff: string): DiffLine[] {
  const out: DiffLine[] = []
  let o = 0
  let n = 0
  let inHunk = false
  for (const raw of diff.split('\n')) {
    const m = HUNK.exec(raw)
    if (m) {
      o = Number(m[1])
      n = Number(m[2])
      inHunk = true
      out.push({ kind: '@', text: raw })
      continue
    }
    if (!inHunk) continue // file headers, "diff --git", "index …"
    const c = raw[0]
    if (c === '+') out.push({ kind: '+', text: raw.slice(1), new: n++ })
    else if (c === '-') out.push({ kind: '-', text: raw.slice(1), old: o++ })
    else if (c === '\\') out.push({ kind: '\\', text: raw.slice(1).trim() })
    else if (c === ' ') out.push({ kind: ' ', text: raw.slice(1), old: o++, new: n++ })
    // a final empty line after the last newline is not a context line
  }
  return out
}

const rowTone: Record<DiffLine['kind'], string> = {
  '+': 'bg-ok/10',
  '-': 'bg-danger/10',
  ' ': '',
  '@': 'bg-sunken text-subtle',
  '\\': 'text-subtle italic',
}

const markTone: Record<DiffLine['kind'], string> = {
  '+': 'text-ok',
  '-': 'text-danger',
  ' ': 'text-transparent',
  '@': 'text-transparent',
  '\\': 'text-transparent',
}

/** The lines of a diff; scrolls sideways rather than wrapping, so columns stay aligned. */
export function Diff({ diff, className }: { diff: string; className?: string }) {
  const lines = useMemo(() => parseDiff(diff), [diff])
  if (lines.length === 0) return <div className="px-3 py-2 text-xs text-subtle">No changes.</div>
  return (
    <div className={clsx('overflow-x-auto', className)}>
      <table className="w-full min-w-max border-collapse font-mono text-[12px] leading-5">
        <tbody>
          {lines.map((l, i) => (
            <tr key={i} className={rowTone[l.kind]}>
              {l.kind === '@' ? (
                <td colSpan={3} className="px-3 py-0.5">
                  {l.text}
                </td>
              ) : (
                <>
                  <td
                    aria-hidden
                    className="w-10 select-none whitespace-nowrap border-r border-line/60 px-1.5 text-right text-subtle tabular-nums"
                  >
                    {l.kind === '+' ? '' : l.old}
                  </td>
                  <td
                    aria-hidden
                    className="w-10 select-none whitespace-nowrap border-r border-line/60 px-1.5 text-right text-subtle tabular-nums"
                  >
                    {l.kind === '-' ? '' : l.new}
                  </td>
                  <td className="whitespace-pre pr-3">
                    <span
                      aria-hidden
                      className={clsx('inline-block w-4 text-center', markTone[l.kind])}
                    >
                      {l.kind === ' ' || l.kind === '\\' ? '' : l.kind}
                    </span>
                    <span className="sr-only">
                      {l.kind === '+' ? 'added: ' : l.kind === '-' ? 'removed: ' : ''}
                    </span>
                    {l.text || ' '}
                  </td>
                </>
              )}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

/** One file of a change: its path and counts, folding its diff. */
export function DiffFile({
  path,
  additions,
  deletions,
  diff,
  status,
  defaultOpen = true,
}: {
  path: string
  additions: number
  deletions: number
  diff: string
  /** a new file, a deleted one, or (default) a modified one */
  status?: 'added' | 'deleted' | 'modified'
  defaultOpen?: boolean
}) {
  const [open, setOpen] = useState(defaultOpen)
  const mark = status === 'added' ? 'A' : status === 'deleted' ? 'D' : 'M'
  return (
    <div className="overflow-hidden rounded-lg border border-line bg-panel">
      <button
        type="button"
        aria-expanded={open}
        onClick={() => setOpen(!open)}
        className="flex w-full items-center gap-2 px-3 py-2 text-left hover:bg-hover"
      >
        <ChevronRight
          className={clsx('size-4 shrink-0 text-subtle transition-transform', open && 'rotate-90')}
        />
        <span
          title={status ?? 'modified'}
          className={clsx(
            'flex size-5 shrink-0 items-center justify-center rounded text-[11px] font-semibold',
            status === 'added' && 'bg-ok/15 text-ok',
            status === 'deleted' && 'bg-danger/15 text-danger',
            !status || status === 'modified' ? 'bg-sunken text-muted' : '',
          )}
        >
          {mark}
        </span>
        <span className="min-w-0 flex-1 truncate font-mono text-[12.5px]">{path}</span>
        <span className="shrink-0 text-xs tabular-nums">
          <span className="text-ok">+{additions}</span>{' '}
          <span className="text-danger">−{deletions}</span>
        </span>
      </button>
      {open && (
        <div className="border-t border-line">
          <Diff diff={diff} />
        </div>
      )}
    </div>
  )
}
