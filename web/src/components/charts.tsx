// Colors are CSS variables only, so light and dark mode both work.

import {
  useEffect,
  useLayoutEffect,
  useRef,
  useState,
  type CSSProperties,
  type KeyboardEvent,
  type PointerEvent,
  type ReactNode,
} from 'react'
import clsx from 'clsx'

/**
 * Validated categorical slots (blue, orange, aqua, yellow), stepped separately
 * for light and dark surfaces. Apply `vizClass` on (or above) a chart root.
 */
export const vizClass =
  '[--viz-1:#2a78d6] [--viz-2:#eb6834] [--viz-3:#1baf7a] [--viz-4:#eda100] dark:[--viz-1:#3987e5] dark:[--viz-2:#d95926] dark:[--viz-3:#199e70] dark:[--viz-4:#c98500]'

export const viz = {
  blue: 'var(--viz-1)',
  orange: 'var(--viz-2)',
  aqua: 'var(--viz-3)',
  yellow: 'var(--viz-4)',
  /** De-emphasised reference series (ideal, scope, committed, backlog). */
  neutral: 'var(--wp-subtle)',
}

/** 1,234 · 12.5 */
export function fmtNum(v: number | null | undefined, digits = 1): string {
  if (v === null || v === undefined || Number.isNaN(v)) return '–'
  return v.toLocaleString(undefined, { maximumFractionDigits: Number.isInteger(v) ? 0 : digits })
}

/** Round-number ticks from 0 to at least max. */
export function niceTicks(max: number, count = 4, integer = false): number[] {
  if (!(max > 0)) max = integer ? 4 : 1
  const raw = max / count
  const pow = Math.pow(10, Math.floor(Math.log10(raw)))
  let step = [1, 2, 2.5, 5, 10].map((m) => m * pow).find((s) => s >= raw) ?? 10 * pow
  if (integer) step = Math.max(1, Math.ceil(step))
  const top = Math.ceil(max / step - 1e-9) * step
  const out: number[] = []
  for (let v = 0; v <= top + step / 2; v += step) out.push(Math.round(v * 1000) / 1000)
  return out
}

/** Width of an element, kept current with a ResizeObserver. */
export function useWidth<T extends HTMLElement>() {
  const ref = useRef<T>(null)
  const [w, setW] = useState(0)
  useLayoutEffect(() => {
    const el = ref.current
    if (!el) return
    setW(Math.floor(el.getBoundingClientRect().width))
    const ro = new ResizeObserver((entries) => setW(Math.floor(entries[0].contentRect.width)))
    ro.observe(el)
    return () => ro.disconnect()
  }, [])
  return [ref, w] as const
}

function barPath(x: number, y: number, w: number, h: number, r = 4): string {
  if (h <= 0 || w <= 0) return ''
  r = Math.min(r, w / 2, h)
  return `M${x},${y + h}V${y + r}Q${x},${y} ${x + r},${y}H${x + w - r}Q${x + w},${y} ${x + w},${y + r}V${y + h}Z`
}

function linePath(pts: ([number, number] | null)[]): string {
  let d = ''
  let pen = false
  for (const p of pts) {
    if (!p) {
      pen = false
      continue
    }
    d += `${pen ? 'L' : 'M'}${p[0].toFixed(1)},${p[1].toFixed(1)}`
    pen = true
  }
  return d
}

export type KeyKind = 'line' | 'dash' | 'box'

export interface LegendItem {
  name: string
  color: string
  kind?: KeyKind
  /** Optional current value shown after the name. */
  value?: string
}

function Key({ color, kind = 'box' }: { color: string; kind?: KeyKind }) {
  if (kind === 'box')
    return (
      <span
        aria-hidden
        className="inline-block size-2.5 shrink-0 rounded-[3px]"
        style={{ background: color }}
      />
    )
  return (
    <svg aria-hidden width="14" height="4" className="shrink-0">
      <line
        x1="1"
        y1="2"
        x2="13"
        y2="2"
        stroke={color}
        strokeWidth="2"
        strokeLinecap="round"
        strokeDasharray={kind === 'dash' ? '3 3' : undefined}
      />
    </svg>
  )
}

export function Legend({ items, className }: { items: LegendItem[]; className?: string }) {
  return (
    <ul
      className={clsx(
        'flex flex-wrap items-center gap-x-4 gap-y-1 text-xs text-muted',
        vizClass,
        className,
      )}
    >
      {items.map((it) => (
        <li key={it.name} className="flex items-center gap-1.5">
          <Key color={it.color} kind={it.kind} />
          <span>{it.name}</span>
          {it.value !== undefined && (
            <span className="font-medium tabular-nums text-fg">{it.value}</span>
          )}
        </li>
      ))}
    </ul>
  )
}

export interface TipRow {
  name: string
  color: string
  value: string
  kind?: KeyKind
}

function TooltipBox({
  title,
  rows,
  style,
}: {
  title: string
  rows: TipRow[]
  style: CSSProperties
}) {
  return (
    <div
      role="status"
      style={style}
      className="pointer-events-none absolute z-10 min-w-28 max-w-[220px] rounded-lg border border-line bg-raised px-2.5 py-2 text-xs shadow-pop"
    >
      <div className="mb-1 truncate font-medium text-muted">{title}</div>
      <ul className="space-y-0.5">
        {rows.map((r) => (
          <li key={r.name} className="flex items-center gap-1.5 whitespace-nowrap">
            <Key color={r.color} kind={r.kind === 'dash' ? 'dash' : 'line'} />
            <span className="font-semibold tabular-nums text-fg">{r.value}</span>
            <span className="truncate text-subtle">{r.name}</span>
          </li>
        ))}
      </ul>
    </div>
  )
}

export interface Scale {
  /** Plot width/height (inside the axes). */
  w: number
  h: number
  x: (i: number) => number
  y: (v: number) => number
  /** Band width (band mode) or the spacing between points. */
  bw: number
  active: number | null
}

/** A moment drawn across the plot: a dashed rule at an index. */
export interface PlotMark {
  i: number
  label: string
}

interface PlotProps {
  ariaLabel: string
  /** Dashed vertical rules (changes, releases) behind the data. */
  marks?: PlotMark[]
  /** Short x label per index. */
  labels: string[]
  /** Tooltip heading per index (defaults to the label). */
  titles?: string[]
  mode: 'point' | 'band'
  yMax: number
  integer?: boolean
  /** Y axis unit, shown above the axis. */
  unit?: string
  height?: number
  /** No axes or grid (sparklines). */
  compact?: boolean
  tooltip: (i: number) => TipRow[]
  children: (s: Scale) => ReactNode
  className?: string
}

/** Axes, grid, hit-testing and tooltip shared by every chart. */
export function Plot({
  ariaLabel,
  marks,
  labels,
  titles,
  mode,
  yMax,
  integer,
  unit,
  height = 200,
  compact,
  tooltip,
  children,
  className,
}: PlotProps) {
  const [ref, width] = useWidth<HTMLDivElement>()
  const [active, setActive] = useState<number | null>(null)
  const n = labels.length

  useEffect(() => {
    if (active === null) return
    const onDown = (e: globalThis.PointerEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) setActive(null)
    }
    document.addEventListener('pointerdown', onDown)
    return () => document.removeEventListener('pointerdown', onDown)
  }, [active, ref])
  useEffect(() => {
    if (active !== null && active >= n) setActive(null)
  }, [active, n])

  const ticks = compact ? [0, yMax > 0 ? yMax : 1] : niceTicks(yMax, height < 160 ? 3 : 4, integer)
  const top = ticks[ticks.length - 1]
  const tickW = compact ? 0 : Math.max(...ticks.map((t) => fmtNum(t, 3).length)) * 6.6 + 8
  const m = compact ? { t: 2, r: 0, b: 0, l: 0 } : { t: 8, r: 10, b: 22, l: Math.ceil(tickW) }
  const w = Math.max(0, width - m.l - m.r)
  const h = Math.max(0, height - m.t - m.b)
  const bw = mode === 'band' ? w / Math.max(1, n) : n > 1 ? w / (n - 1) : w
  const x = (i: number) => (mode === 'band' ? (i + 0.5) * bw : n > 1 ? i * bw : w / 2)
  const y = (v: number) => h - (Math.max(0, v) / top) * h
  const indexAt = (px: number) => {
    const i = mode === 'band' ? Math.floor(px / bw) : n > 1 ? Math.round(px / bw) : 0
    return Math.max(0, Math.min(n - 1, i))
  }

  const onPointer = (e: PointerEvent<SVGRectElement>) => {
    if (n === 0) return
    const r = e.currentTarget.getBoundingClientRect()
    setActive(indexAt(e.clientX - r.left))
  }
  const onKey = (e: KeyboardEvent<HTMLDivElement>) => {
    if (n === 0) return
    const cur = active ?? n - 1
    let next: number | null = null
    if (e.key === 'ArrowRight') next = Math.min(n - 1, cur + 1)
    else if (e.key === 'ArrowLeft') next = Math.max(0, cur - 1)
    else if (e.key === 'Home') next = 0
    else if (e.key === 'End') next = n - 1
    else if (e.key === 'Escape') setActive(null)
    if (next !== null) {
      e.preventDefault()
      setActive(next)
    }
  }

  const step = Math.max(1, Math.ceil(n / Math.max(2, Math.floor(w / 62))))
  const scale: Scale = { w, h, x, y, bw, active }
  let tip: ReactNode = null
  if (active !== null && active < n && width > 0) {
    const px = m.l + x(active)
    const style: CSSProperties =
      px < width / 2
        ? { left: px + (mode === 'band' ? bw / 2 : 0) + 8, top: m.t }
        : { right: width - px + (mode === 'band' ? bw / 2 : 0) + 8, top: m.t }
    tip = (
      <TooltipBox
        title={titles?.[active] ?? labels[active]}
        rows={[
          ...tooltip(active),
          ...(marks ?? [])
            .filter((mk) => mk.i === active)
            .map((mk) => ({
              name: mk.label,
              color: 'var(--wp-subtle)',
              kind: 'dash' as const,
              value: '',
            })),
        ]}
        style={style}
      />
    )
  }

  return (
    <div className={clsx(vizClass, className)}>
      {unit && !compact && <div className="mb-1 text-[11px] text-subtle">{unit}</div>}
      <div
        ref={ref}
        tabIndex={0}
        role="group"
        aria-roledescription="chart"
        aria-label={ariaLabel + ' — use arrow keys to read values'}
        onKeyDown={onKey}
        onFocus={(e) =>
          active === null && e.currentTarget.matches(':focus-visible') && setActive(n - 1)
        }
        onBlur={() => setActive(null)}
        className="relative select-none rounded-md"
        style={{ height }}
      >
        {width > 0 && n > 0 && (
          <svg width={width} height={height} className="block overflow-visible" aria-hidden>
            <g transform={`translate(${m.l},${m.t})`}>
              {!compact &&
                ticks.map((t) => (
                  <g key={t}>
                    <line
                      x1={0}
                      x2={w}
                      y1={y(t)}
                      y2={y(t)}
                      stroke={t === 0 ? 'var(--wp-line-strong)' : 'var(--wp-line)'}
                      strokeWidth={1}
                      shapeRendering="crispEdges"
                    />
                    <text
                      x={-6}
                      y={y(t)}
                      dy="0.32em"
                      textAnchor="end"
                      fontSize={11}
                      fill="var(--wp-subtle)"
                      style={{ fontVariantNumeric: 'tabular-nums' }}
                    >
                      {fmtNum(t, 3)}
                    </text>
                  </g>
                ))}
              {marks?.map((mk) =>
                mk.i < 0 || mk.i >= n ? null : (
                  <g key={`${mk.i}:${mk.label}`}>
                    <title>{mk.label}</title>
                    <line
                      x1={x(mk.i)}
                      x2={x(mk.i)}
                      y1={0}
                      y2={h}
                      stroke="var(--wp-subtle)"
                      strokeWidth={1}
                      strokeDasharray="2 3"
                      shapeRendering="crispEdges"
                    />
                    <path
                      d={`M${x(mk.i) - 4},0H${x(mk.i) + 4}L${x(mk.i)},5Z`}
                      fill="var(--wp-subtle)"
                    />
                  </g>
                ),
              )}
              {mode === 'band' && active !== null && (
                <rect
                  x={x(active) - bw / 2}
                  y={0}
                  width={bw}
                  height={h}
                  fill="var(--wp-hover)"
                  rx={4}
                />
              )}
              {children(scale)}
              {mode === 'point' && active !== null && (
                <line
                  x1={x(active)}
                  x2={x(active)}
                  y1={0}
                  y2={h}
                  stroke="var(--wp-muted)"
                  strokeOpacity={0.5}
                  strokeWidth={1}
                  shapeRendering="crispEdges"
                />
              )}
              {!compact &&
                labels.map((l, i) => {
                  if ((n - 1 - i) % step !== 0) return null
                  const xi = x(i)
                  const anchor =
                    mode === 'point' && xi < 28
                      ? 'start'
                      : mode === 'point' && xi > w - 28
                        ? 'end'
                        : 'middle'
                  return (
                    <text
                      key={i}
                      x={xi}
                      y={h + 16}
                      textAnchor={anchor}
                      fontSize={11}
                      fill="var(--wp-subtle)"
                    >
                      {l}
                    </text>
                  )
                })}
              <rect
                x={mode === 'point' && n > 1 ? -bw / 2 : 0}
                y={-m.t}
                width={mode === 'point' && n > 1 ? w + bw : w}
                height={h + m.t + m.b}
                fill="transparent"
                style={{ touchAction: 'pan-y' }}
                onPointerDown={onPointer}
                onPointerMove={onPointer}
                onPointerLeave={(e) => e.pointerType !== 'touch' && setActive(null)}
              />
            </g>
          </svg>
        )}
        {tip}
      </div>
    </div>
  )
}

export interface LineSeries {
  name: string
  color: string
  values: (number | null)[]
  /** Reference line (ideal, target): dashed, no markers. */
  dashed?: boolean
  /** 10% wash under the line. */
  area?: boolean
}

interface CommonProps {
  ariaLabel: string
  marks?: PlotMark[]
  labels: string[]
  titles?: string[]
  unit?: string
  height?: number
  integer?: boolean
  format?: (v: number) => string
}

/** One or more lines on a shared y axis, with a crosshair tooltip. */
export function LineChart({
  series,
  format = (v) => fmtNum(v),
  ...p
}: CommonProps & { series: LineSeries[] }) {
  const yMax = Math.max(
    0,
    ...series.flatMap((s) => s.values.filter((v): v is number => v !== null)),
  )
  return (
    <Plot
      {...p}
      mode="point"
      yMax={yMax}
      tooltip={(i) =>
        [...series].reverse().map((s) => ({
          name: s.name,
          color: s.color,
          kind: s.dashed ? 'dash' : 'line',
          value: s.values[i] === null ? '–' : format(s.values[i]!),
        }))
      }
    >
      {({ x, y, h, active }) => (
        <>
          {series.map((s) => {
            const pts = s.values.map((v, i) =>
              v === null ? null : ([x(i), y(v)] as [number, number]),
            )
            const d = linePath(pts)
            let area = ''
            if (s.area) {
              let run: [number, number][] = []
              const flush = () => {
                if (run.length > 1)
                  area += `${linePath(run)}L${run[run.length - 1][0]},${h}L${run[0][0]},${h}Z`
                run = []
              }
              for (const pt of pts) {
                if (pt) run.push(pt)
                else flush()
              }
              flush()
            }
            const lastI = s.values.reduce<number>((acc, v, i) => (v === null ? acc : i), -1)
            return (
              <g key={s.name}>
                {area && <path d={area} fill={s.color} fillOpacity={0.1} />}
                <path
                  d={d}
                  fill="none"
                  stroke={s.color}
                  strokeWidth={s.dashed ? 1.5 : 2}
                  strokeDasharray={s.dashed ? '4 4' : undefined}
                  strokeLinecap="round"
                  strokeLinejoin="round"
                />
                {!s.dashed && lastI >= 0 && active === null && (
                  <circle
                    cx={x(lastI)}
                    cy={y(s.values[lastI]!)}
                    r={4}
                    fill={s.color}
                    stroke="var(--wp-panel)"
                    strokeWidth={2}
                  />
                )}
              </g>
            )
          })}
          {active !== null &&
            series.map((s) =>
              s.values[active] === null || s.values[active] === undefined ? null : (
                <circle
                  key={s.name}
                  cx={x(active)}
                  cy={y(s.values[active]!)}
                  r={4}
                  fill={s.color}
                  stroke="var(--wp-panel)"
                  strokeWidth={2}
                />
              ),
            )}
        </>
      )}
    </Plot>
  )
}

export interface BarSeries {
  name: string
  color: string
  values: number[]
}

/** Grouped columns, one group per label. */
export function BarChart({
  series,
  format = (v) => fmtNum(v),
  ...p
}: CommonProps & { series: BarSeries[] }) {
  const yMax = Math.max(0, ...series.flatMap((s) => s.values))
  const k = series.length
  return (
    <Plot
      {...p}
      mode="band"
      yMax={yMax}
      tooltip={(i) =>
        series.map((s) => ({ name: s.name, color: s.color, value: format(s.values[i] ?? 0) }))
      }
    >
      {({ x, y, h, bw }) => {
        const gap = 2
        const barW = Math.max(2, Math.min(24, (bw * 0.72 - gap * (k - 1)) / k))
        const groupW = barW * k + gap * (k - 1)
        return series.map((s, j) => (
          <g key={s.name}>
            {s.values.map((v, i) => {
              const bx = x(i) - groupW / 2 + j * (barW + gap)
              return <path key={i} d={barPath(bx, y(v), barW, h - y(v))} fill={s.color} />
            })}
          </g>
        ))
      }}
    </Plot>
  )
}

/** Stacked areas; series are listed bottom to top. */
export function StackedAreaChart({
  series,
  format = (v) => fmtNum(v),
  ...p
}: CommonProps & { series: BarSeries[] }) {
  const n = p.labels.length
  const cum: number[][] = []
  series.forEach((s, j) =>
    cum.push(Array.from({ length: n }, (_, i) => (j ? cum[j - 1][i] : 0) + (s.values[i] ?? 0))),
  )
  const yMax = Math.max(0, ...(cum[cum.length - 1] ?? [0]))
  return (
    <Plot
      {...p}
      mode="point"
      yMax={yMax}
      tooltip={(i) =>
        [...series]
          .reverse()
          .map((s) => ({ name: s.name, color: s.color, value: format(s.values[i] ?? 0) }))
      }
    >
      {({ x, y }) => {
        const xs = Array.from({ length: n }, (_, i) => x(i))
        const edge = (j: number) =>
          xs.map((xi, i) => [xi, y(j < 0 ? 0 : cum[j][i])] as [number, number])
        return (
          <>
            {series.map((s, j) => {
              const topPts = edge(j)
              const bottom = edge(j - 1).reverse()
              const d = `${linePath(topPts)}${bottom.map(([a, b]) => `L${a.toFixed(1)},${b.toFixed(1)}`).join('')}Z`
              return (
                <path
                  key={s.name}
                  d={d}
                  fill={`color-mix(in oklab, ${s.color} 78%, var(--wp-panel))`}
                />
              )
            })}
            {series.slice(0, -1).map((s, j) => (
              <path
                key={s.name}
                d={linePath(edge(j))}
                fill="none"
                stroke="var(--wp-panel)"
                strokeWidth={2}
                strokeLinejoin="round"
              />
            ))}
          </>
        )
      }}
    </Plot>
  )
}

/** A tiny single-series column chart with tooltips and no axes. */
export function Sparkbars({
  values,
  labels,
  color = viz.blue,
  height = 44,
  format = (v) => fmtNum(v),
  name,
  ariaLabel,
  className,
}: {
  values: number[]
  labels: string[]
  color?: string
  height?: number
  format?: (v: number) => string
  name: string
  ariaLabel: string
  className?: string
}) {
  const yMax = Math.max(0, ...values)
  return (
    <Plot
      ariaLabel={ariaLabel}
      labels={labels}
      mode="band"
      yMax={yMax}
      compact
      height={height}
      className={className}
      tooltip={(i) => [{ name, color, value: format(values[i] ?? 0) }]}
    >
      {({ x, y, h, bw }) => {
        const barW = Math.max(2, Math.min(16, bw - 3))
        return values.map((v, i) =>
          v > 0 ? (
            <path key={i} d={barPath(x(i) - barW / 2, y(v), barW, h - y(v), 3)} fill={color} />
          ) : (
            <rect
              key={i}
              x={x(i) - barW / 2}
              y={h - 1}
              width={barW}
              height={1}
              fill="var(--wp-line-strong)"
            />
          ),
        )
      }}
    </Plot>
  )
}

export interface HBarItem {
  key: string
  label: string
  /** Secondary text before the label (e.g. an issue key). */
  prefix?: string
  value: number
  display: string
  onClick?: () => void
}

/** Ranked horizontal bars in HTML, so long labels truncate gracefully. */
export function HBarList({
  items,
  color = viz.blue,
  ariaLabel,
}: {
  items: HBarItem[]
  color?: string
  ariaLabel: string
}) {
  const max = Math.max(0, ...items.map((i) => i.value))
  return (
    <ul aria-label={ariaLabel} className={clsx('space-y-2', vizClass)}>
      {items.map((it) => {
        const pct = max > 0 ? (it.value / max) * 100 : 0
        const label = (
          <>
            {it.prefix && <span className="mr-1.5 text-subtle">{it.prefix}</span>}
            {it.label}
          </>
        )
        return (
          <li key={it.key} title={`${it.prefix ? it.prefix + ' ' : ''}${it.label}: ${it.display}`}>
            <div className="mb-1 flex items-baseline gap-3 text-[13px]">
              {it.onClick ? (
                <button
                  onClick={it.onClick}
                  className="min-w-0 flex-1 truncate text-left hover:underline"
                >
                  {label}
                </button>
              ) : (
                <span className="min-w-0 flex-1 truncate">{label}</span>
              )}
              <span className="shrink-0 font-medium tabular-nums">{it.display}</span>
            </div>
            <div className="h-1.5 rounded-full bg-sunken">
              <div
                className="h-full rounded-full"
                style={{ width: `max(${pct}%, 3px)`, background: color }}
              />
            </div>
          </li>
        )
      })}
    </ul>
  )
}

/** Collapsed table twin of a chart, for exact values and screen readers. */
export function DataTable({
  columns,
  rows,
  className,
}: {
  columns: string[]
  rows: (string | number)[][]
  className?: string
}) {
  return (
    <details className={clsx('group text-xs', className)}>
      <summary className="cursor-pointer select-none text-subtle hover:text-fg">Data table</summary>
      <div className="mt-2 max-h-64 overflow-auto rounded-md border border-line">
        <table className="w-full tabular-nums">
          <thead className="sticky top-0 bg-sunken text-muted">
            <tr>
              {columns.map((c, i) => (
                <th
                  key={c}
                  className={clsx('px-2 py-1.5 font-medium', i ? 'text-right' : 'text-left')}
                >
                  {c}
                </th>
              ))}
            </tr>
          </thead>
          <tbody>
            {rows.map((r, ri) => (
              <tr key={ri} className="border-t border-line">
                {r.map((c, i) => (
                  <td
                    key={i}
                    className={clsx('px-2 py-1', i ? 'text-right' : 'text-left text-muted')}
                  >
                    {typeof c === 'number' ? fmtNum(c) : c}
                  </td>
                ))}
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </details>
  )
}
