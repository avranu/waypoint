import { useEffect, useRef } from 'react'

const stack: number[] = []
let next = 1

/**
 * Escape calls onEscape on the topmost open layer only. If onEscape returns
 * false the key also reaches the focused element (so a field can cancel its
 * own edit); otherwise it stops here.
 */
export function useEscapeLayer(open: boolean, onEscape: () => void | boolean) {
  const cb = useRef(onEscape)
  cb.current = onEscape
  useEffect(() => {
    if (!open) return
    const id = next++
    stack.push(id)
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== 'Escape' || e.defaultPrevented || stack[stack.length - 1] !== id) return
      if (cb.current() === false) return
      e.preventDefault()
      e.stopPropagation()
    }
    document.addEventListener('keydown', onKey, true)
    return () => {
      document.removeEventListener('keydown', onKey, true)
      const i = stack.indexOf(id)
      if (i >= 0) stack.splice(i, 1)
    }
  }, [open])
}
