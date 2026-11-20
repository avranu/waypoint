import { useSyncExternalStore } from 'react'
import { Link } from '@tanstack/react-router'
import clsx from 'clsx'
import { X } from 'lucide-react'

interface Toast {
  id: number
  text: string
  kind: 'info' | 'error' | 'success'
  href?: string
  action?: { label: string; run: () => void }
}

let toasts: Toast[] = []
let nextId = 1
const listeners = new Set<() => void>()
const emit = () => listeners.forEach((l) => l())

export function toast(
  text: string,
  opts: { kind?: Toast['kind']; href?: string; action?: Toast['action']; ms?: number } = {},
) {
  const t: Toast = {
    id: nextId++,
    text,
    kind: opts.kind ?? 'info',
    href: opts.href,
    action: opts.action,
  }
  toasts = [...toasts.slice(-3), t]
  emit()
  setTimeout(() => dismiss(t.id), opts.ms ?? (t.kind === 'error' ? 6000 : 4000))
}

export const toastError = (e: unknown) =>
  toast(e instanceof Error ? e.message : String(e), { kind: 'error' })

function dismiss(id: number) {
  toasts = toasts.filter((t) => t.id !== id)
  emit()
}

export function Toaster() {
  const list = useSyncExternalStore(
    (cb) => {
      listeners.add(cb)
      return () => listeners.delete(cb)
    },
    () => toasts,
  )
  return (
    <div
      role="status"
      aria-live="polite"
      className="pointer-events-none fixed inset-x-0 bottom-20 z-[70] flex flex-col items-center gap-2 px-4 sm:bottom-6 sm:items-end sm:px-6"
    >
      {list.map((t) => (
        <div
          key={t.id}
          className={clsx(
            'animate-up pointer-events-auto flex max-w-sm items-center gap-3 rounded-xl border px-3.5 py-2.5 text-[13px] shadow-pop',
            t.kind === 'error'
              ? 'border-danger/30 bg-raised text-danger'
              : 'border-line bg-raised text-fg',
          )}
        >
          {t.href ? (
            <Link to={t.href} className="flex-1 hover:underline" onClick={() => dismiss(t.id)}>
              {t.text}
            </Link>
          ) : (
            <span className="flex-1">{t.text}</span>
          )}
          {t.action && (
            <button
              className="font-medium text-accent"
              onClick={() => {
                t.action!.run()
                dismiss(t.id)
              }}
            >
              {t.action.label}
            </button>
          )}
          <button
            aria-label="Dismiss"
            className="text-subtle hover:text-fg"
            onClick={() => dismiss(t.id)}
          >
            <X className="size-3.5" />
          </button>
        </div>
      ))}
    </div>
  )
}
