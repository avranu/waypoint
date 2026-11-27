import { useMemo, useState, type ReactNode } from 'react'
import clsx from 'clsx'
import { ArrowDown, ArrowUp, Check, Copy } from 'lucide-react'
import { copyText } from '@/lib/clipboard'
import { Button } from '@/components/ui/button'
import { toastError } from '@/components/ui/toast'
import { fmtNum, viz, vizClass } from '@/components/charts'

export function Tile({
  label,
  value,
  sub,
  title,
}: {
  label: string
  value: ReactNode
  sub?: ReactNode
  title?: string
}) {
  return (
    <div className="min-w-0 rounded-xl border border-line bg-panel px-3 py-2.5" title={title}>
      <dt className="truncate text-xs text-muted">{label}</dt>
      <dd className="truncate text-xl font-semibold tabular-nums tracking-tight">{value}</dd>
      {sub && <dd className="truncate text-xs text-subtle">{sub}</dd>}
    </div>
  )
}

/** Copies text and says so on the button for a moment. */
export function CopyButton({
  text,
  label = 'Copy',
  size = 'xs',
}: {
  text: string
  label?: string
  size?: 'xs' | 'sm'
}) {
  const [done, setDone] = useState(false)
  return (
    <Button
      size={size}
      variant="ghost"
      icon={done ? <Check className="size-3.5" /> : <Copy className="size-3.5" />}
      onClick={() =>
        copyText(text)
          .then(() => {
            setDone(true)
            window.setTimeout(() => setDone(false), 1500)
          })
          .catch(toastError)
      }
    >
      {done ? 'Copied' : label}
    </Button>
  )
}

/** A row of exclusive choices, small. */
export function Segmented<T extends string>({
  value,
  onChange,
  options,
  label,
}: {
  value: T
  onChange: (v: T) => void
  options: { id: T; label: string }[]
  label: string
}) {
  return (
    <div
      role="radiogroup"
      aria-label={label}
      className="no-scrollbar flex h-7 max-w-full items-center overflow-x-auto rounded-lg border border-line p-0.5"
    >
      {options.map((o) => (
        <button
          key={o.id}
          role="radio"
          aria-checked={value === o.id}
          onClick={() => onChange(o.id)}
          className={clsx(
            'h-full shrink-0 rounded-md px-2.5 text-xs',
            value === o.id ? 'bg-accent-soft font-medium text-fg' : 'text-muted hover:text-fg',
          )}
        >
          {o.label}
        </button>
      ))}
    </div>
  )
}

export interface Col<T> {
  id: string
  label: string
  /** The value sorted on; numbers get right-aligned columns. */
  value: (r: T) => number | string
  /** What is shown (defaults to the value). */
  cell?: (r: T) => ReactNode
  /** Draw a bar behind the value, scaled to the column's largest. */
  bar?: boolean
  /** Hidden below the sm breakpoint, to keep the table readable at 390px. */
  wide?: boolean
  /** Ascending first (names). */
  asc?: boolean
}

/** A sortable table; numeric columns can carry an inline bar. */
export function SortTable<T>({
  rows,
  cols,
  rowKey,
  sort: initial,
  onRow,
  caption,
  color = viz.blue,
}: {
  rows: T[]
  cols: Col<T>[]
  rowKey: (r: T) => string
  sort: string
  onRow?: (r: T) => void
  caption: string
  color?: string
}) {
  const [sort, setSort] = useState({ id: initial, dir: -1 })
  const col = cols.find((c) => c.id === sort.id) ?? cols[0]
  const sorted = useMemo(() => {
    const out = [...rows]
    out.sort((a, b) => {
      const x = col.value(a)
      const y = col.value(b)
      const c =
        typeof x === 'number' && typeof y === 'number' ? x - y : String(x).localeCompare(String(y))
      return c * sort.dir
    })
    return out
  }, [rows, col, sort.dir])
  const max = useMemo(() => {
    const m: Record<string, number> = {}
    for (const c of cols)
      if (c.bar) m[c.id] = Math.max(0, ...rows.map((r) => Number(c.value(r)) || 0))
    return m
  }, [rows, cols])
  return (
    <div className={clsx('overflow-x-auto', vizClass)}>
      <table className="w-full table-fixed text-[13px]">
        <caption className="sr-only">{caption}</caption>
        <thead>
          <tr className="border-b border-line text-xs text-muted">
            {cols.map((c, i) => {
              const num = typeof c.value(rows[0] ?? ({} as T)) === 'number' || c.bar
              const active = c.id === col.id
              return (
                <th
                  key={c.id}
                  scope="col"
                  aria-sort={active ? (sort.dir > 0 ? 'ascending' : 'descending') : 'none'}
                  className={clsx(
                    'px-3 py-1.5 font-medium',
                    num && i > 0 ? 'text-right' : 'text-left',
                    i === 0 && 'w-[40%] sm:w-[34%]',
                    c.wide && 'hidden sm:table-cell',
                  )}
                >
                  <button
                    onClick={() =>
                      setSort(
                        active ? { id: c.id, dir: -sort.dir } : { id: c.id, dir: c.asc ? 1 : -1 },
                      )
                    }
                    className={clsx(
                      'inline-flex items-center gap-0.5 hover:text-fg',
                      active && 'text-fg',
                    )}
                  >
                    {c.label}
                    {active &&
                      (sort.dir > 0 ? (
                        <ArrowUp className="size-3" />
                      ) : (
                        <ArrowDown className="size-3" />
                      ))}
                  </button>
                </th>
              )
            })}
          </tr>
        </thead>
        <tbody>
          {sorted.map((r) => (
            <tr
              key={rowKey(r)}
              onClick={onRow ? () => onRow(r) : undefined}
              className={clsx(
                'border-b border-line last:border-0',
                onRow && 'cursor-pointer hover:bg-hover',
              )}
            >
              {cols.map((c, i) => {
                const v = c.value(r)
                const num = typeof v === 'number'
                const pct = c.bar && max[c.id] > 0 ? (Number(v) / max[c.id]) * 100 : 0
                return (
                  <td
                    key={c.id}
                    className={clsx(
                      'px-3 py-1.5',
                      num && i > 0 && 'text-right tabular-nums',
                      c.wide && 'hidden sm:table-cell',
                      i === 0 && 'max-w-[16rem]',
                    )}
                  >
                    {c.bar ? (
                      <div className="relative ml-auto min-w-20">
                        <div className="relative z-10 text-right">
                          {c.cell ? c.cell(r) : fmtNum(Number(v))}
                        </div>
                        <div className="mt-0.5 h-1 rounded-full bg-sunken">
                          <div
                            className="h-full rounded-full"
                            style={{
                              width: `${Math.max(pct, pct > 0 ? 3 : 0)}%`,
                              background: color,
                            }}
                          />
                        </div>
                      </div>
                    ) : c.cell ? (
                      c.cell(r)
                    ) : (
                      String(v)
                    )}
                  </td>
                )
              })}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

export function rate(n: number, of: number) {
  if (of <= 0) return '–'
  const p = (n / of) * 100
  return p === 0 ? '0%' : p < 1 ? '<1%' : `${Math.round(p)}%`
}
