import { Maximize2 } from 'lucide-react'
import { IconButton } from '@/components/ui/button'
import { openCreateIssue } from '@/components/CreateIssue'
import type { IssueInput } from '@/lib/queries'

/** Opens the new-issue dialog from a quick-add box with what's typed (WP-125). */
export const DETAILS_HINT = '⌘↵'

/** True for the key that opens details from a quick-add box: Ctrl or ⌘ with Enter. */
export const isDetailsKey = (e: React.KeyboardEvent) =>
  e.key === 'Enter' && (e.metaKey || e.ctrlKey)

export function openDetails(
  add: { details: (x?: Partial<IssueInput>) => Partial<IssueInput> },
  extra?: Partial<IssueInput>,
  after?: () => void,
) {
  openCreateIssue(add.details(extra))
  after?.()
}

export function QuickAddDetailsButton({ onOpen }: { onOpen: () => void }) {
  return (
    <IconButton
      type="button"
      size="xs"
      label={`Add details (${DETAILS_HINT})`}
      icon={<Maximize2 className="size-3.5" />}
      // Keeps focus in the box, so a blur doesn't close it before the click lands.
      onMouseDown={(e) => e.preventDefault()}
      onClick={onOpen}
    />
  )
}
