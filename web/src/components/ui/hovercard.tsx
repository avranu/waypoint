// A card of details beside a row the pointer rests on (WP-610). It waits
// before showing, so sweeping the pointer across a list doesn't flash cards;
// once one is up, moving to the next row swaps it sooner. Only on devices
// that hover.

import { useCallback, useEffect, useLayoutEffect, useRef, useState, type ReactNode } from 'react'
import { createPortal } from 'react-dom'
import { useMediaQuery } from '@/lib/hooks'

const SHOW_MS = 650
const SWAP_MS = 250
const HIDE_MS = 120
const GAP = 8

interface HoverTrigger {
  onMouseEnter?: (e: React.MouseEvent<HTMLElement>) => void
  onMouseLeave?: () => void
}

interface Shown<T> {
  item: T
  rect: DOMRect
}

/** Hover cards for the rows of one list: spread trigger(item) on each row and put render(…) once. */
export function useHoverCard<T>() {
  const canHover = useMediaQuery('(hover: hover) and (pointer: fine)')
  const [shown, setShown] = useState<Shown<T> | null>(null)
  const timer = useRef<number | undefined>(undefined)
  const visible = useRef(false)
  visible.current = shown !== null

  const clear = () => window.clearTimeout(timer.current)
  const hide = useCallback(() => {
    window.clearTimeout(timer.current)
    setShown(null)
  }, [])
  // A scroll moves the card's row away, but the row the pointer entered
  // (which may have scrolled the list into view) still gets its card.
  const drop = useCallback(() => setShown(null), [])
  useEffect(() => () => window.clearTimeout(timer.current), [])
  // Typing, clicking or scrolling means the person moved on.
  useEffect(() => {
    if (!shown) return
    window.addEventListener('keydown', hide, true)
    window.addEventListener('pointerdown', hide, true)
    window.addEventListener('scroll', drop, true)
    return () => {
      window.removeEventListener('keydown', hide, true)
      window.removeEventListener('pointerdown', hide, true)
      window.removeEventListener('scroll', drop, true)
    }
  }, [shown, hide, drop])

  const trigger = (item: T): HoverTrigger =>
    canHover
      ? {
          onMouseEnter: (e: React.MouseEvent<HTMLElement>) => {
            const el = e.currentTarget
            clear()
            timer.current = window.setTimeout(
              () => el.isConnected && setShown({ item, rect: el.getBoundingClientRect() }),
              visible.current ? SWAP_MS : SHOW_MS,
            )
          },
          onMouseLeave: () => {
            clear()
            timer.current = window.setTimeout(() => setShown(null), HIDE_MS)
          },
        }
      : {}

  const render = (content: (item: T) => ReactNode) =>
    shown ? <Card rect={shown.rect}>{content(shown.item)}</Card> : null

  return { trigger, render, hide }
}

function Card({ rect, children }: { rect: DOMRect; children: ReactNode }) {
  const ref = useRef<HTMLDivElement>(null)
  const [pos, setPos] = useState<{ top: number; left: number } | null>(null)
  useLayoutEffect(() => {
    const el = ref.current
    if (!el) return
    const w = el.offsetWidth
    const h = el.offsetHeight
    const vw = window.innerWidth
    const vh = window.innerHeight
    // Beside the row, so the list stays readable: right if it fits, else
    // left, else below or above it.
    let left: number
    let top = Math.max(GAP, Math.min(rect.top, vh - h - GAP))
    if (rect.right + GAP + w <= vw - GAP) left = rect.right + GAP
    else if (rect.left - GAP - w >= GAP) left = rect.left - GAP - w
    else {
      left = Math.max(GAP, Math.min(rect.left, vw - w - GAP))
      top =
        rect.bottom + GAP + h <= vh - GAP ? rect.bottom + GAP : Math.max(GAP, rect.top - GAP - h)
    }
    setPos({ top, left })
  }, [rect])
  return createPortal(
    <div
      ref={ref}
      role="tooltip"
      style={{ top: pos?.top ?? -9999, left: pos?.left ?? -9999 }}
      className="animate-pop pointer-events-none fixed z-[70] w-80 max-w-[calc(100vw-16px)] rounded-xl border border-line bg-raised p-3 text-[13px] shadow-pop"
    >
      {children}
    </div>,
    document.body,
  )
}
