import { useEffect, useRef, useState, useSyncExternalStore, type RefObject } from 'react'

export function useMediaQuery(query: string): boolean {
  return useSyncExternalStore(
    (cb) => {
      const m = matchMedia(query)
      m.addEventListener('change', cb)
      return () => m.removeEventListener('change', cb)
    },
    () => matchMedia(query).matches,
    () => false,
  )
}

export function useDebounced<T>(value: T, ms = 200): T {
  const [v, setV] = useState(value)
  useEffect(() => {
    const t = setTimeout(() => setV(value), ms)
    return () => clearTimeout(t)
  }, [value, ms])
  return v
}

/** Re-render every interval ms (for running timers and relative times). */
export function useTick(ms = 1000, enabled = true): number {
  const [n, setN] = useState(0)
  useEffect(() => {
    if (!enabled) return
    const t = setInterval(() => setN((x) => x + 1), ms)
    return () => clearInterval(t)
  }, [ms, enabled])
  return n
}

/** Read a useLocal value outside React (route loaders). */
export function readLocal<T>(key: string, fallback: T): T {
  try {
    const s = localStorage.getItem('wp:' + key)
    return s === null ? fallback : (JSON.parse(s) as T)
  } catch {
    return fallback
  }
}

/** A value persisted in localStorage (per browser convenience only). */
export function useLocal<T>(key: string, initial: T): [T, (v: T) => void] {
  const [v, setV] = useState<T>(() => {
    try {
      const s = localStorage.getItem('wp:' + key)
      return s === null ? initial : (JSON.parse(s) as T)
    } catch {
      return initial
    }
  })
  const set = (nv: T) => {
    setV(nv)
    try {
      localStorage.setItem('wp:' + key, JSON.stringify(nv))
    } catch {}
  }
  return [v, set]
}

/** Global keyboard shortcut, ignored while typing in a field. */
export function useHotkey(
  key: string,
  handler: (e: KeyboardEvent) => void,
  opts: { meta?: boolean; enabled?: boolean } = {},
) {
  useEffect(() => {
    if (opts.enabled === false) return
    const onKey = (e: KeyboardEvent) => {
      const t = e.target as HTMLElement
      const typing = t.isContentEditable || ['INPUT', 'TEXTAREA', 'SELECT'].includes(t.tagName)
      if (opts.meta) {
        if (!(e.metaKey || e.ctrlKey) || e.key.toLowerCase() !== key) return
      } else {
        if (typing || e.metaKey || e.ctrlKey || e.altKey || e.key !== key) return
      }
      e.preventDefault()
      handler(e)
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [key, handler, opts.meta, opts.enabled])
}

/** Enter or Space on a role=button element itself (not on a control inside it). */
export function activate(e: React.KeyboardEvent): boolean {
  if (e.target !== e.currentTarget || (e.key !== 'Enter' && e.key !== ' ')) return false
  e.preventDefault()
  return true
}

/**
 * Keyboard navigation over issue rows or cards (elements with data-nav-key)
 * inside ref: j/k or ↓/↑ move focus, h/l or ←/→ jump between columns
 * (data-nav-col), Enter or o opens, x toggles selection, Esc leaves.
 * Ignored while typing, and when focus is somewhere else on the page.
 */
export function useListNav(
  ref: RefObject<HTMLElement | null>,
  opts: { onOpen: (key: string) => void; onSelect?: (key: string) => void },
) {
  const cb = useRef(opts)
  cb.current = opts
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const root = ref.current
      if (!root || e.defaultPrevented || e.metaKey || e.ctrlKey || e.altKey) return
      const t = e.target as HTMLElement
      if (t.isContentEditable || ['INPUT', 'TEXTAREA', 'SELECT'].includes(t.tagName)) return
      const inside = root.contains(t)
      if (!inside && t !== document.body) return
      const cur = inside ? t.closest<HTMLElement>('[data-nav-key]') : null
      if (!cur && !['j', 'k'].includes(e.key)) return
      const items = [...root.querySelectorAll<HTMLElement>('[data-nav-key]')]
      if (!items.length) return
      const go = (el?: HTMLElement | null) => {
        if (!el) return
        e.preventDefault()
        el.focus({ preventScroll: true })
        el.scrollIntoView({ block: 'nearest', inline: 'nearest' })
      }
      const i = cur ? items.indexOf(cur) : -1
      switch (e.key) {
        case 'j':
        case 'ArrowDown':
          return go(items[Math.min(i + 1, items.length - 1)])
        case 'k':
        case 'ArrowUp':
          return go(items[Math.max(i - 1, 0)])
        case 'h':
        case 'l':
        case 'ArrowLeft':
        case 'ArrowRight': {
          const col = cur?.closest<HTMLElement>('[data-nav-col]')
          if (!cur || !col?.parentElement) return
          const cols = [...col.parentElement.children].filter((c) => c.hasAttribute('data-nav-col'))
          const step = e.key === 'h' || e.key === 'ArrowLeft' ? -1 : 1
          const row = [...col.querySelectorAll('[data-nav-key]')].indexOf(cur)
          for (let c = cols.indexOf(col) + step; c >= 0 && c < cols.length; c += step) {
            const cards = cols[c].querySelectorAll<HTMLElement>('[data-nav-key]')
            if (cards.length) return go(cards[Math.min(row, cards.length - 1)])
          }
          return
        }
        case 'o':
          e.preventDefault()
          return cb.current.onOpen(cur!.dataset.navKey!)
        case 'x':
          if (!cb.current.onSelect) return
          e.preventDefault()
          return cb.current.onSelect(cur!.dataset.navKey!)
        case 'Escape':
          cur?.blur()
          return
      }
    }
    document.addEventListener('keydown', onKey)
    return () => document.removeEventListener('keydown', onKey)
  }, [ref])
}

/**
 * True while the person scrolls down any vertical scroller on the page, false
 * again when they scroll up or near its top: a floating button hides out of
 * the way of what they're reading and comes back when they look for it.
 * Sideways scrolls (wide tables, charts) leave it as it was.
 */
export function useScrollingDown(reset?: unknown): boolean {
  const [state, setState] = useState({ reset, down: false })
  if (state.reset !== reset) setState({ reset, down: false })
  useEffect(() => {
    const last = new WeakMap<Element, number>()
    const on = (e: Event) => {
      const el = e.target instanceof Element ? e.target : document.scrollingElement
      if (!el) return
      const y = el.scrollTop
      // A scroller seen for the first time started at its top, near enough:
      // one jump (a keypress, a fling) may be its only scroll event.
      const prev = last.get(el) ?? 0
      last.set(el, y)
      if (Math.abs(y - prev) < 4) return
      const down = y > prev && y > 24
      setState((s) => (s.down === down ? s : { ...s, down }))
    }
    document.addEventListener('scroll', on, { capture: true, passive: true })
    return () => document.removeEventListener('scroll', on, { capture: true })
  }, [])
  return state.reset === reset && state.down
}
