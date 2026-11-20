// Small building blocks shared by the Settings and Admin pages.

import type { ReactNode } from 'react'
import clsx from 'clsx'
import { Copy } from 'lucide-react'
import { IconButton } from '@/components/ui/button'
import { Tip } from '@/components/ui/help'
import { toast, toastError } from '@/components/ui/toast'
import { copyText } from '@/lib/clipboard'

export function copy(text: string, what = 'Copied') {
  copyText(text).then(() => toast(what), toastError)
}

/** A read-only value with a copy button (URLs, secrets, commands). */
export function CopyField({
  value,
  mono = true,
  multiline,
  className,
}: {
  value: string
  mono?: boolean
  multiline?: boolean
  className?: string
}) {
  return (
    <div
      className={clsx(
        'flex items-start gap-1 rounded-lg border border-line bg-sunken py-1 pl-3 pr-1',
        className,
      )}
    >
      {multiline ? (
        <pre
          className={clsx(
            'min-w-0 flex-1 overflow-x-auto whitespace-pre py-1 text-xs leading-relaxed',
            mono && 'font-mono',
          )}
        >
          {value}
        </pre>
      ) : (
        <input
          readOnly
          value={value}
          onFocus={(e) => e.target.select()}
          className={clsx(
            'h-7 min-w-0 flex-1 bg-transparent text-xs outline-none',
            mono && 'font-mono',
          )}
        />
      )}
      <IconButton label="Copy" onClick={() => copy(value)} icon={<Copy className="size-3.5" />} />
    </div>
  )
}

export interface TabDef<T extends string> {
  id: T
  label: string
  icon: ReactNode
}

/** Tabs that are a vertical list on wide screens and a scrolling row below that. */
export function TabNav<T extends string>({
  tabs,
  active,
  onSelect,
}: {
  tabs: TabDef<T>[]
  active: T
  onSelect: (t: T) => void
}) {
  return (
    <nav
      aria-label="Sections"
      className="-mx-4 flex shrink-0 gap-1 overflow-x-auto px-4 pb-1 lg:sticky lg:top-4 lg:mx-0 lg:w-48 lg:flex-col lg:self-start lg:overflow-visible lg:px-0"
    >
      {tabs.map((t) => (
        <button
          key={t.id}
          type="button"
          aria-current={t.id === active ? 'page' : undefined}
          onClick={() => onSelect(t.id)}
          className={clsx(
            'flex h-8 shrink-0 items-center gap-2 rounded-lg px-2.5 text-[13px] transition-colors [&>svg]:size-4 [&>svg]:shrink-0',
            t.id === active
              ? 'bg-accent-soft font-medium text-fg [&>svg]:text-accent'
              : 'text-muted hover:bg-hover hover:text-fg',
          )}
        >
          {t.icon}
          {t.label}
        </button>
      ))}
    </nav>
  )
}

/** Two-column page layout: tab nav + content. */
export function TabbedLayout({ nav, children }: { nav: ReactNode; children: ReactNode }) {
  return (
    <div className="flex flex-col gap-4 px-4 sm:px-6 lg:flex-row lg:gap-8">
      {nav}
      <div className="min-w-0 flex-1 space-y-4">{children}</div>
    </div>
  )
}

/** Muted explanatory paragraph. */
export function Help({ children, className }: { children: ReactNode; className?: string }) {
  return <p className={clsx('text-[13px] leading-relaxed text-muted', className)}>{children}</p>
}

/** A yellow-ish callout for one-time secrets. */
export function Warning({ children }: { children: ReactNode }) {
  return (
    <div className="rounded-lg border border-warn/30 bg-warn/10 px-3 py-2 text-[13px] text-fg">
      {children}
    </div>
  )
}

/** Numbered steps. */
export function Steps({ items }: { items: ReactNode[] }) {
  return (
    <ol className="space-y-1.5 text-[13px] text-muted">
      {items.map((s, i) => (
        <li key={i} className="flex gap-2.5">
          <span className="mt-px flex size-5 shrink-0 items-center justify-center rounded-full bg-sunken text-[11px] font-semibold text-fg">
            {i + 1}
          </span>
          <span className="min-w-0 flex-1">{s}</span>
        </li>
      ))}
    </ol>
  )
}

/** A checkbox row with a label and short description. */
export function CheckRow({
  checked,
  onChange,
  label,
  hint,
  disabled,
}: {
  checked: boolean
  onChange: (v: boolean) => void
  label: ReactNode
  hint?: ReactNode
  disabled?: boolean
}) {
  return (
    <div
      className={clsx(
        'flex items-center gap-1 px-4 hover:bg-hover',
        disabled && 'pointer-events-none opacity-60',
      )}
    >
      <label className="flex min-w-0 cursor-pointer items-center gap-3 py-2.5">
        <input
          type="checkbox"
          checked={checked}
          disabled={disabled}
          onChange={(e) => onChange(e.target.checked)}
          className="size-4 shrink-0 accent-[var(--wp-accent)]"
        />
        <span className="min-w-0 text-[13px] font-medium">{label}</span>
      </label>
      {hint && <Tip>{hint}</Tip>}
    </div>
  )
}

export const AVATAR_TYPES = ['image/png', 'image/jpeg', 'image/gif', 'image/webp']

/** Client-side check before uploading an avatar; returns an error message or null. */
export function avatarProblem(f: File): string | null {
  if (!AVATAR_TYPES.includes(f.type)) return 'Pick a PNG, JPEG, GIF or WebP image.'
  if (f.size > 2 * 1024 * 1024) return 'Images must be under 2 MB.'
  return null
}
