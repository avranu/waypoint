// Asks before leaving with text that would be lost (WP-68). Most typing is
// kept as a draft (lib/drafts.ts), so this only speaks up when storage
// refused one, or, for a refresh or a closed tab, while a save is still on
// its way to the server.

import { useBlocker } from '@tanstack/react-router'
import { hasInflight, hasUnsaved, useAnyUnsaved } from '@/lib/drafts'
import { Confirm } from '@/components/ui/dialog'

export function UnsavedGuard() {
  const any = useAnyUnsaved()
  const b = useBlocker({
    disabled: !any,
    // Moving within the page (opening an issue sheet) keeps what's typed.
    shouldBlockFn: ({ current, next }) => current.pathname !== next.pathname && hasUnsaved(),
    enableBeforeUnload: () => hasUnsaved() || hasInflight(),
    withResolver: true,
  })
  return (
    <Confirm
      open={b.status === 'blocked'}
      onClose={() => b.reset?.()}
      onConfirm={() => b.proceed?.()}
      title="Leave without saving?"
      body="You have unsaved text here that this browser couldn't keep as a draft. If you leave, it's lost."
      confirmLabel="Leave"
      cancelLabel="Stay"
    />
  )
}
