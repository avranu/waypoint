import { useRef, type KeyboardEvent, type ReactNode } from 'react'
import clsx from 'clsx'

export interface SegmentOption<T extends string> {
  value: T
  /** short visible word; always the accessible name */
  label: string
  icon: ReactNode
  /** what picking it does; shown in the tooltip */
  hint?: string
}

/**
 * A small row of exclusive choices, each one click: an icon and a short label.
 * Below `textFrom` only the icons show (the label stays as tooltip and accessible
 * name). `value` may be null for "nothing chosen yet" (a bulk action).
 */
export function SegmentedControl<T extends string>({
  label,
  value,
  onChange,
  options,
  textFrom = 'sm',
  className,
}: {
  label: string
  value: T | null
  onChange: (v: T) => void
  options: SegmentOption<T>[]
  /**
   * Where every label shows next to its icon (default sm); below it only the chosen
   * one does, the others keep their words as tooltip and accessible name.
   * 'selected': only the chosen one, at any width (narrow spots).
   */
  textFrom?: 'sm' | 'md' | 'always' | 'selected'
  className?: string
}) {
  const ref = useRef<HTMLDivElement>(null)
  // Arrow keys move between the choices, like any radio group.
  const onKeyDown = (e: KeyboardEvent<HTMLDivElement>) => {
    const step =
      e.key === 'ArrowRight' || e.key === 'ArrowDown'
        ? 1
        : e.key === 'ArrowLeft' || e.key === 'ArrowUp'
          ? -1
          : 0
    if (!step) return
    e.preventDefault()
    const at = options.findIndex((o) => o.value === value)
    const next = options[(Math.max(at, 0) + step + options.length) % options.length]
    onChange(next.value)
    ref.current?.querySelector<HTMLButtonElement>(`[data-value="${next.value}"]`)?.focus()
  }
  return (
    <div
      ref={ref}
      role="radiogroup"
      aria-label={label}
      onKeyDown={onKeyDown}
      className={clsx(
        'inline-flex h-7 shrink-0 items-center rounded-md bg-sunken p-0.5',
        className,
      )}
    >
      {options.map((o) => {
        const on = o.value === value
        return (
          <button
            key={o.value}
            type="button"
            role="radio"
            aria-checked={on}
            aria-label={o.label}
            data-value={o.value}
            tabIndex={on || (value === null && o === options[0]) ? 0 : -1}
            title={o.hint ? `${o.label}: ${o.hint}` : o.label}
            onClick={() => !on && onChange(o.value)}
            className={clsx(
              'inline-flex h-6 min-w-7 select-none items-center justify-center gap-1 rounded px-1.5 text-xs font-medium transition-colors',
              on ? 'bg-panel text-fg shadow-card' : 'text-subtle hover:text-fg',
            )}
          >
            {o.icon}
            <span
              className={clsx(
                // The chosen one always says what it is; the rest collapse to icons.
                !on && textFrom === 'sm' && 'hidden sm:inline',
                !on && textFrom === 'md' && 'hidden md:inline',
                !on && textFrom === 'selected' && 'hidden',
              )}
            >
              {o.label}
            </span>
          </button>
        )
      })}
    </div>
  )
}
