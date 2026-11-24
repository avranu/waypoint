import { useEffect, useId, useRef, type ReactNode } from 'react'
import { createPortal } from 'react-dom'
import clsx from 'clsx'
import { X } from 'lucide-react'
import { IconButton } from './button'
import { useEscapeLayer } from './layers'

/** A centered modal; full screen on phones. */
export function Dialog({
  open,
  onClose,
  title,
  children,
  footer,
  className,
  wide,
}: {
  open: boolean
  onClose: () => void
  title?: ReactNode
  children: ReactNode
  footer?: ReactNode
  className?: string
  wide?: boolean
}) {
  const ref = useRef<HTMLDivElement>(null)
  const titleId = useId()
  useEscapeLayer(open, onClose)
  useEffect(() => {
    if (!open) return
    const prev = document.activeElement as HTMLElement | null
    if (!ref.current?.contains(document.activeElement))
      (focusables(ref.current)[0] ?? ref.current)?.focus({ preventScroll: true })
    return () => prev?.focus?.()
  }, [open])
  // Portaled popovers bubble through React to here, so only act on this dialog's own DOM.
  const trap = (e: React.KeyboardEvent) => {
    if (e.key !== 'Tab' || !ref.current?.contains(e.target as Node)) return
    const list = focusables(ref.current)
    if (!list.length) return
    const first = list[0]
    const last = list[list.length - 1]
    if (e.shiftKey && document.activeElement === first) {
      e.preventDefault()
      last.focus()
    } else if (!e.shiftKey && document.activeElement === last) {
      e.preventDefault()
      first.focus()
    }
  }
  if (!open) return null
  return createPortal(
    <div className="fixed inset-0 z-50 flex items-end justify-center sm:items-start sm:p-4 sm:pt-[10vh]">
      <div className="absolute inset-0 bg-black/40 backdrop-blur-[1px]" onClick={onClose} />
      <div
        ref={ref}
        role="dialog"
        aria-modal
        aria-labelledby={title ? titleId : undefined}
        tabIndex={-1}
        onKeyDown={trap}
        className={clsx(
          'animate-up pb-safe relative flex max-h-[92vh] outline-none w-full flex-col overflow-hidden rounded-t-2xl border border-line bg-raised shadow-pop sm:rounded-2xl',
          wide ? 'sm:max-w-3xl' : 'sm:max-w-lg',
          className,
        )}
      >
        {title && (
          <div className="flex items-center justify-between gap-2 border-b border-line px-4 py-3">
            <h2 id={titleId} className="text-[15px] font-semibold">
              {title}
            </h2>
            <IconButton label="Close" onClick={onClose} icon={<X className="size-4" />} />
          </div>
        )}
        <div className="flex-1 overflow-auto">{children}</div>
        {footer && (
          <div className="flex items-center justify-end gap-2 border-t border-line px-4 py-3">
            {footer}
          </div>
        )}
      </div>
    </div>,
    document.body,
  )
}

/** Confirm a destructive action. */
export function Confirm({
  open,
  onClose,
  onConfirm,
  title,
  body,
  confirmLabel = 'Delete',
  cancelLabel = 'Cancel',
  danger = true,
}: {
  open: boolean
  onClose: () => void
  onConfirm: () => void
  title: string
  body?: ReactNode
  confirmLabel?: string
  cancelLabel?: string
  danger?: boolean
}) {
  return (
    <Dialog
      open={open}
      onClose={onClose}
      title={title}
      footer={
        <>
          <button
            className="h-9 rounded-lg px-3.5 text-sm text-muted hover:bg-hover"
            onClick={onClose}
          >
            {cancelLabel}
          </button>
          <button
            autoFocus
            className={clsx(
              'h-9 rounded-lg px-3.5 text-sm font-medium text-white',
              danger ? 'bg-danger' : 'bg-accent',
            )}
            onClick={() => {
              onConfirm()
              onClose()
            }}
          >
            {confirmLabel}
          </button>
        </>
      }
    >
      {body && <div className="px-4 py-4 text-sm text-muted">{body}</div>}
    </Dialog>
  )
}

function focusables(root: HTMLElement | null): HTMLElement[] {
  if (!root) return []
  const sel =
    'a[href], button:not(:disabled), input:not(:disabled), textarea:not(:disabled), select:not(:disabled), [tabindex]:not([tabindex="-1"])'
  return Array.from(root.querySelectorAll<HTMLElement>(sel)).filter(
    (el) => el.offsetParent !== null || el === document.activeElement,
  )
}
