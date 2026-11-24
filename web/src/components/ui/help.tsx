import { useEffect, useRef, useState, type ReactNode } from 'react'
import { useQuery } from '@tanstack/react-query'
import clsx from 'clsx'
import { CircleHelp, X } from 'lucide-react'
import { q, useSavePrefs } from '@/lib/queries'
import { Popover, usePopover } from './popover'

/** Whether the explainer `id` is open (folded until asked for), and a way to fold or unfold it. */
export function useHelp(id: string) {
  const me = useQuery(q.me()).data
  const save = useSavePrefs()
  const open = me?.prefs.open_help ?? []
  const shown = open.includes(id)
  const set = (show: boolean) => {
    const next = open.filter((h) => h !== id)
    if (show) next.push(id)
    save.mutate({ open_help: next })
  }
  return { id, shown, show: () => set(true), hide: () => set(false) }
}

export type HelpState = ReturnType<typeof useHelp>

const FOLD_MS = 200

/**
 * The details of an explainer, with a button that folds them away. The
 * panel shrinks toward where its (?) appears (`origin`), then is gone.
 */
export function HelpPanel({
  help,
  children,
  origin = 'top left',
  bare,
  className,
}: {
  help: HelpState
  children: ReactNode
  origin?: string
  /** No box of its own (it already sits in a panel). */
  bare?: boolean
  className?: string
}) {
  const [folding, setFolding] = useState(false)
  const timer = useRef<number | undefined>(undefined)
  useEffect(() => () => window.clearTimeout(timer.current), [])
  if (!help.shown) return null
  const fold = () => {
    setFolding(true)
    timer.current = window.setTimeout(() => {
      help.hide()
      setFolding(false)
    }, FOLD_MS)
  }
  return (
    <div
      className={clsx(
        'grid transition-[grid-template-rows,opacity,transform] duration-200 ease-out motion-reduce:transition-none',
        folding ? 'grid-rows-[0fr] scale-90 opacity-0' : 'grid-rows-[1fr]',
        className,
      )}
      style={{ transformOrigin: origin }}
    >
      <div className="min-h-0 overflow-hidden">
        <div
          role="note"
          className={clsx(
            'relative text-[13px] leading-relaxed text-muted [&_b]:font-medium [&_b]:text-fg',
            bare ? 'py-3 pl-4 pr-11' : 'rounded-lg border border-line bg-sunken/70 py-2 pl-3 pr-9',
          )}
        >
          {children}
          <button
            type="button"
            onClick={fold}
            aria-label="Hide this help"
            title="Hide (the ? brings it back)"
            className={clsx(
              'absolute flex size-6 items-center justify-center rounded-md text-subtle hover:bg-hover hover:text-fg',
              bare ? 'right-2.5 top-2.5' : 'right-1.5 top-1.5',
            )}
          >
            <X className="size-3.5" />
          </button>
        </div>
      </div>
    </div>
  )
}

/** The (?) a folded explainer leaves behind; it opens the details again. */
export function HelpButton({ help, label }: { help: HelpState; label: string }) {
  if (help.shown) return null
  return (
    <button
      type="button"
      onClick={help.show}
      aria-label={`Show help: ${label}`}
      title={`About ${label.toLowerCase()}`}
      className="wp-pop-in inline-flex size-6 shrink-0 items-center justify-center rounded-md text-subtle hover:bg-hover hover:text-fg"
    >
      <CircleHelp className="size-4" />
    </button>
  )
}

/**
 * An explainer on its own: the details, or once folded a (?) with an
 * optional short label (e.g. "How categories work").
 */
export function Explainer({
  id,
  label,
  children,
  className,
}: {
  id: string
  label: string
  children: ReactNode
  className?: string
}) {
  const help = useHelp(id)
  if (!help.shown)
    return (
      <div className={clsx('flex items-center gap-1 text-xs text-subtle', className)}>
        <HelpButton help={help} label={label} />
        <button type="button" onClick={help.show} className="hover:text-fg">
          {label}
        </button>
      </div>
    )
  return (
    <HelpPanel help={help} className={className}>
      {children}
    </HelpPanel>
  )
}

/**
 * A hint behind a small (?): it shows on hover or keyboard focus, and on a
 * tap on touch screens (where it opens as a sheet).
 */
export function Tip({
  children,
  label = 'More info',
  className,
}: {
  children: ReactNode
  label?: string
  className?: string
}) {
  const pop = usePopover()
  const hoverTimer = useRef<number | undefined>(undefined)
  useEffect(() => () => window.clearTimeout(hoverTimer.current), [])
  const openSoon = () => {
    window.clearTimeout(hoverTimer.current)
    hoverTimer.current = window.setTimeout(() => pop.setOpen(true), 120)
  }
  const closeSoon = () => {
    window.clearTimeout(hoverTimer.current)
    hoverTimer.current = window.setTimeout(() => pop.setOpen(false), 120)
  }
  return (
    <>
      <button
        ref={pop.anchor}
        type="button"
        aria-label={label}
        aria-expanded={pop.open}
        onClick={(e) => {
          // Inside a <label>, a click would otherwise focus its input.
          e.preventDefault()
          e.stopPropagation()
          pop.toggle()
        }}
        onPointerEnter={(e) => e.pointerType === 'mouse' && openSoon()}
        onPointerLeave={(e) => e.pointerType === 'mouse' && closeSoon()}
        onFocus={(e) => e.currentTarget.matches(':focus-visible') && pop.setOpen(true)}
        onBlur={() => pop.setOpen(false)}
        className={clsx(
          'inline-flex size-4 shrink-0 items-center justify-center rounded-full align-[-2px] text-subtle hover:text-fg focus-visible:text-fg',
          className,
        )}
      >
        <CircleHelp className="size-3.5" />
      </button>
      <Popover open={pop.open} onClose={pop.close} anchor={pop.anchor} className="sm:max-w-[280px]">
        <div
          role="tooltip"
          onPointerEnter={(e) =>
            e.pointerType === 'mouse' && window.clearTimeout(hoverTimer.current)
          }
          onPointerLeave={(e) => e.pointerType === 'mouse' && closeSoon()}
          className="px-2.5 py-1.5 text-xs leading-relaxed text-muted [&_b]:font-medium [&_b]:text-fg"
        >
          {children}
        </div>
      </Popover>
    </>
  )
}
