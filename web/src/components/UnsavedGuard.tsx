import { useBlocker } from '@tanstack/react-router'
import { hasInflight, hasUnsaved, useAnyUnsaved } from '@/lib/drafts'
import { Confirm } from '@/components/ui/dialog'

export function UnsavedGuard() {
  const any = useAnyUnsaved()
  const b = useBlocker({
    disabled: !any,
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
