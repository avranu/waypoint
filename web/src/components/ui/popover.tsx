import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useId,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
  type RefObject,
} from 'react'
import { createPortal } from 'react-dom'
import clsx from 'clsx'
import { Check, Plus, Search } from 'lucide-react'
import { useMediaQuery } from '@/lib/hooks'
import { useEscapeLayer } from './layers'
import { useHoverCard } from './hovercard'

interface PopoverProps {
  open: boolean
  onClose: () => void
  anchor: RefObject<HTMLElement | null>
  children: ReactNode
  className?: string
  align?: 'start' | 'end'
  width?: number
}

const PopoverDepth = createContext(0)

export function Popover({
  open,
  onClose,
  anchor,
  children,
  className,
  align = 'start',
  width,
}: PopoverProps) {
  const ref = useRef<HTMLDivElement>(null)
  const [pos, setPos] = useState<{ top: number; left: number; maxH: number } | null>(null)
  const mobile = useMediaQuery('(max-width: 639px)')
  const depth = useContext(PopoverDepth)
  useEscapeLayer(open, () => {
    onClose()
    anchor.current?.focus()
  })

  const place = useCallback(() => {
    const a = anchor.current?.getBoundingClientRect()
    const el = ref.current
    if (!a || !el) return
    const w = el.offsetWidth
    const h = el.offsetHeight
    const vw = window.innerWidth
    const vh = window.innerHeight
    let left = align === 'end' ? a.right - w : a.left
    left = Math.max(8, Math.min(left, vw - w - 8))
    const below = vh - a.bottom - 8
    const above = a.top - 8
    let top: number
    let maxH: number
    if (below >= Math.min(h, 320) || below >= above) {
      top = a.bottom + 4
      maxH = below - 4
    } else {
      maxH = above - 4
      top = Math.max(8, a.top - 4 - Math.min(h, maxH))
    }
    setPos({ top, left, maxH })
  }, [anchor, align])

  useLayoutEffect(() => {
    if (!open || mobile) return
    place()
    const ro = new ResizeObserver(place)
    if (ref.current) ro.observe(ref.current)
    window.addEventListener('resize', place)
    window.addEventListener('scroll', place, true)
    return () => {
      ro.disconnect()
      window.removeEventListener('resize', place)
      window.removeEventListener('scroll', place, true)
    }
  }, [open, mobile, place])

  useEffect(() => {
    if (!open) return
    const onDown = (e: PointerEvent) => {
      const t = e.target as Node
      if (ref.current?.contains(t) || anchor.current?.contains(t)) return
      if ((t as HTMLElement).closest?.('[data-popover]')) {
        const level = Number(
          (t as HTMLElement).closest('[data-popover]')?.getAttribute('data-popover'),
        )
        if (level > depth) return
      }
      onClose()
    }
    document.addEventListener('pointerdown', onDown, true)
    return () => document.removeEventListener('pointerdown', onDown, true)
  }, [open, onClose, anchor, depth])

  if (!open) return null
  if (mobile) {
    return createPortal(
      <PopoverDepth.Provider value={depth + 1}>
        <div className="fixed inset-0 z-[60] bg-black/40" onClick={onClose} />
        <div
          ref={ref}
          data-popover={depth + 1}
          className={clsx(
            'animate-up pb-safe fixed inset-x-0 bottom-0 z-[61] max-h-[75vh] overflow-auto rounded-t-2xl border-t border-line bg-raised p-2 shadow-pop',
            className,
          )}
        >
          <div className="mx-auto mb-2 h-1 w-10 rounded-full bg-line-strong" />
          {children}
        </div>
      </PopoverDepth.Provider>,
      document.body,
    )
  }
  return createPortal(
    <PopoverDepth.Provider value={depth + 1}>
      <div
        ref={ref}
        data-popover={depth + 1}
        style={{ top: pos?.top ?? -9999, left: pos?.left ?? -9999, maxHeight: pos?.maxH, width }}
        className={clsx(
          'animate-pop fixed z-[61] min-w-[180px] overflow-auto rounded-xl border border-line bg-raised p-1 shadow-pop',
          className,
        )}
      >
        {children}
      </div>
    </PopoverDepth.Provider>,
    document.body,
  )
}

/** A button that toggles a popover. */
/** initial: open on mount (a field picked from a "More" menu opens its picker at once). */
export function usePopover(initial = false) {
  const anchor = useRef<HTMLButtonElement>(null)
  const [open, setOpen] = useState(initial)
  return { anchor, open, setOpen, toggle: () => setOpen((o) => !o), close: () => setOpen(false) }
}

export interface MenuItem {
  label: ReactNode
  icon?: ReactNode
  onSelect?: () => void
  danger?: boolean
  disabled?: boolean
  hint?: ReactNode
  separator?: boolean
  /** a choice among alternatives: shows a tick when it is the current one */
  checked?: boolean
}

export function MenuList({ items, onClose }: { items: MenuItem[]; onClose: () => void }) {
  const ref = useRef<HTMLDivElement>(null)
  const mobile = useMediaQuery('(max-width: 639px)')
  const enabled = () =>
    Array.from(
      ref.current?.querySelectorAll<HTMLButtonElement>('[role^=menuitem]:not(:disabled)') ?? [],
    )
  useEffect(() => {
    if (!mobile && !ref.current?.contains(document.activeElement))
      enabled()[0]?.focus({ preventScroll: true })
  }, [mobile])
  const onKeyDown = (e: React.KeyboardEvent) => {
    const list = enabled()
    const i = list.indexOf(document.activeElement as HTMLButtonElement)
    const to =
      e.key === 'ArrowDown'
        ? i + 1
        : e.key === 'ArrowUp'
          ? i - 1
          : e.key === 'Home'
            ? 0
            : e.key === 'End'
              ? list.length - 1
              : null
    if (to === null || !list.length) return
    e.preventDefault()
    list[(to + list.length) % list.length].focus()
  }
  return (
    <div ref={ref} role="menu" className="flex flex-col" onKeyDown={onKeyDown}>
      {items.map((it, i) =>
        it.separator ? (
          <div key={i} className="my-1 h-px bg-line" />
        ) : (
          <button
            key={i}
            role={it.checked === undefined ? 'menuitem' : 'menuitemradio'}
            aria-checked={it.checked}
            disabled={it.disabled}
            onClick={() => {
              it.onSelect?.()
              onClose()
            }}
            className={clsx(
              'flex h-9 items-center gap-2.5 rounded-lg px-2.5 text-left text-[13px] outline-none hover:bg-hover focus-visible:bg-hover disabled:opacity-40 sm:h-8',
              it.danger ? 'text-danger' : 'text-fg',
            )}
          >
            {it.icon && (
              <span className="flex size-4 items-center justify-center text-muted [&>svg]:size-4">
                {it.icon}
              </span>
            )}
            <span className="flex-1 truncate">{it.label}</span>
            {it.hint && <span className="text-xs text-subtle">{it.hint}</span>}
            {it.checked && <Check className="size-4 text-accent" />}
          </button>
        ),
      )}
    </div>
  )
}

export interface PickItem<V> {
  value: V
  label: string
  icon?: ReactNode
  hint?: ReactNode
  keywords?: string
  /** Not a search result (such as "Remove parent"): with onSearch, still filtered by the text, so Enter never picks it by default. */
  fixed?: boolean
  /** details shown in a card when the pointer rests on the row */
  peek?: () => ReactNode
}

interface PickerProps<V> {
  items: PickItem<V>[]
  selected: V[]
  multi?: boolean
  onPick: (v: V, on: boolean) => void
  placeholder?: string
  onCreate?: (text: string) => void
  createLabel?: (text: string) => string
  onClose?: () => void
  autoFocus?: boolean
  /** The items come from a search on the server: report the text instead of filtering by it. */
  onSearch?: (text: string) => void
  /** Room for long labels, such as issue titles. */
  wide?: boolean
  /** The items are still for earlier text: Enter waits for the answer, so it never picks a stale row. */
  searching?: boolean
}

/** A filterable list with keyboard navigation. */
export function Picker<V extends string | number | null>({
  items,
  selected,
  multi,
  onPick,
  placeholder = 'Search…',
  onCreate,
  createLabel,
  onClose,
  autoFocus = true,
  onSearch,
  wide,
  searching,
}: PickerProps<V>) {
  const [text, setText] = useState('')
  const hover = useHoverCard<PickItem<V>>()
  const [active, setActive] = useState(0)
  const listRef = useRef<HTMLDivElement>(null)
  const mobile = useMediaQuery('(max-width: 639px)')
  const filtered = useMemo(() => {
    const t = text.trim().toLowerCase()
    if (!t) return items
    return items.filter(
      (i) =>
        (onSearch && !i.fixed) || (i.label + ' ' + (i.keywords ?? '')).toLowerCase().includes(t),
    )
  }, [items, text, onSearch])
  const canCreate =
    onCreate &&
    text.trim() &&
    !items.some((i) => i.label.toLowerCase() === text.trim().toLowerCase())
  const total = filtered.length + (canCreate ? 1 : 0)
  const enterLater = useRef(false)
  const listId = useId()
  useEffect(() => {
    setActive(0)
    enterLater.current = false
  }, [text])
  useEffect(() => {
    listRef.current?.querySelector(`[data-idx="${active}"]`)?.scrollIntoView({ block: 'nearest' })
  }, [active])
  const choose = (idx: number) => {
    if (idx < 0 || idx >= total) return
    if (idx < filtered.length) {
      const it = filtered[idx]
      const on = !selected.includes(it.value)
      onPick(it.value, multi ? on : true)
      if (!multi) onClose?.()
    } else if (canCreate) {
      onCreate!(text.trim())
      setText('')
      if (!multi) onClose?.()
    }
  }
  useEffect(() => {
    if (searching || !enterLater.current) return
    enterLater.current = false
    choose(active)
  })
  return (
    <div className={clsx('flex w-full flex-col', wide ? 'sm:w-[30rem]' : 'sm:w-64')}>
      <div className="flex items-center gap-2 border-b border-line px-2 pb-1.5 pt-0.5">
        <Search className="size-3.5 text-subtle" />
        <input
          role="combobox"
          aria-expanded
          aria-controls={listId}
          aria-activedescendant={total > 0 ? `${listId}-${active}` : undefined}
          aria-autocomplete="list"
          autoFocus={autoFocus && !mobile}
          value={text}
          onChange={(e) => {
            setText(e.target.value)
            onSearch?.(e.target.value)
          }}
          placeholder={placeholder}
          className="h-8 w-full bg-transparent text-[13px] outline-none placeholder:text-subtle"
          onKeyDown={(e) => {
            if (e.key === 'ArrowDown') {
              e.preventDefault()
              setActive((a) => Math.min(a + 1, Math.max(total - 1, 0)))
            } else if (e.key === 'ArrowUp') {
              e.preventDefault()
              setActive((a) => Math.max(a - 1, 0))
            } else if (e.key === 'Enter') {
              e.preventDefault()
              if (searching) enterLater.current = true
              else choose(active)
            }
          }}
        />
      </div>
      <div
        ref={listRef}
        id={listId}
        role="listbox"
        aria-multiselectable={multi || undefined}
        className={clsx(
          'max-h-72 overflow-auto py-1 transition-opacity',
          searching && 'opacity-60',
        )}
      >
        {filtered.map((it, idx) => {
          const on = selected.includes(it.value)
          return (
            <button
              key={String(it.value)}
              id={`${listId}-${idx}`}
              role="option"
              aria-selected={on}
              data-idx={idx}
              onMouseEnter={(e) => {
                setActive(idx)
                if (it.peek) hover.trigger(it).onMouseEnter?.(e)
              }}
              onMouseLeave={it.peek ? hover.trigger(it).onMouseLeave : undefined}
              onClick={() => choose(idx)}
              className={clsx(
                'flex h-9 w-full items-center gap-2.5 rounded-lg px-2.5 text-left text-[13px] sm:h-8',
                idx === active && 'bg-hover',
              )}
            >
              {it.icon && (
                <span className="flex size-5 shrink-0 items-center justify-center">{it.icon}</span>
              )}
              <span className="flex-1 truncate">{it.label}</span>
              {it.hint && <span className="text-xs text-subtle">{it.hint}</span>}
              <Check className={clsx('size-3.5 text-accent', on ? 'opacity-100' : 'opacity-0')} />
            </button>
          )
        })}
        {canCreate && (
          <button
            id={`${listId}-${filtered.length}`}
            role="option"
            aria-selected={false}
            data-idx={filtered.length}
            onMouseEnter={() => setActive(filtered.length)}
            onClick={() => choose(filtered.length)}
            className={clsx(
              'flex h-8 w-full items-center gap-2.5 rounded-lg px-2.5 text-left text-[13px] text-accent',
              active === filtered.length && 'bg-hover',
            )}
          >
            <Plus className="size-4" />{' '}
            {createLabel ? createLabel(text.trim()) : `Create “${text.trim()}”`}
          </button>
        )}
        {total === 0 && (
          <div className="px-3 py-4 text-center text-xs text-subtle">Nothing matches</div>
        )}
      </div>
      {hover.render((it) => it.peek?.())}
    </div>
  )
}
