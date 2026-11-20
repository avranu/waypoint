import type { Modifier } from '@dnd-kit/core'

/** Keep dragged rows in their column (@dnd-kit/modifiers isn't installed). */
export const restrictToVerticalAxis: Modifier = ({ transform }) => ({ ...transform, x: 0 })
