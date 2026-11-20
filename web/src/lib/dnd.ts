import { useRef } from 'react'
import {
  closestCenter,
  pointerWithin,
  rectIntersection,
  type CollisionDetection,
} from '@dnd-kit/core'

/**
 * Collision detection for multi-container lists: trust what's under the
 * pointer first, so moving an item (which reflows the containers) doesn't
 * flip the target back and forth.
 */
export const pointerFirst: CollisionDetection = (args) => {
  const within = pointerWithin(args)
  if (within.length) {
    // Prefer an item over its container when both are under the pointer.
    const items = within.filter(
      (c) =>
        !String(c.id).startsWith('col-') &&
        !String(c.id).startsWith('cell|') &&
        !String(c.id).startsWith('sprint-') &&
        c.id !== 'backlog',
    )
    if (items.length)
      return closestCenter({
        ...args,
        droppableContainers: args.droppableContainers.filter((d) =>
          items.some((i) => i.id === d.id),
        ),
      })
    return within
  }
  return rectIntersection(args)
}

/** Allows one cross-container move per animation frame. */
export function useMoveThrottle() {
  const busy = useRef(false)
  return (f: () => void) => {
    if (busy.current) return
    busy.current = true
    f()
    requestAnimationFrame(() => {
      busy.current = false
    })
  }
}
