import { useRef, useState, type ReactNode } from 'react'
import clsx from 'clsx'
import { Check, Lock } from 'lucide-react'
import { useQueryClient } from '@tanstack/react-query'
import { invalidateProject } from '@/lib/queries'
import { PROJECT_COLORS } from '@/lib/theme'
import { Popover, usePopover } from '@/components/ui/popover'
import type { Project } from '@/lib/types'
import { HelpButton, HelpPanel, useHelp } from '@/components/ui/help'

/** Colors offered for statuses and labels. */
export const SWATCHES = [...PROJECT_COLORS, '#a855f7', '#3b82f6', '#94a3b8', '#78716c']

export const randomSwatch = () => SWATCHES[Math.floor(Math.random() * SWATCHES.length)]

export const isOwner = (p: Project) => p.role === 'owner'
export const canEdit = (p: Project) => p.role === 'owner' || p.role === 'editor'

/**
 * A tab's title. Its children are an explainer: shown until the person
 * folds it away, after which a (?) by the title brings it back.
 */
export function TabIntro({
  title,
  children,
  actions,
}: {
  title: string
  children?: ReactNode
  actions?: ReactNode
}) {
  const help = useHelp(`settings:${title.toLowerCase()}`)
  return (
    <div className="mb-4 space-y-2">
      <div className="flex min-h-8 flex-wrap items-center justify-between gap-3">
        <div className="flex min-w-0 items-center gap-1">
          <h2 className="text-[15px] font-semibold tracking-tight">{title}</h2>
          {children && <HelpButton help={help} label={title} />}
        </div>
        {actions && <div className="flex shrink-0 items-center gap-2">{actions}</div>}
      </div>
      {children && <HelpPanel help={help}>{children}</HelpPanel>}
    </div>
  )
}

/** Shown to people who can see but not change a tab. */
export function ReadOnlyNote({
  children = 'Only project owners can change these settings.',
}: {
  children?: ReactNode
}) {
  return (
    <div className="mb-4 flex items-center gap-2 rounded-lg border border-line bg-sunken px-3 py-2 text-xs text-muted">
      <Lock className="size-3.5 shrink-0" />
      {children}
    </div>
  )
}

/** A swatch button that opens a small palette. */
export function ColorPicker({
  value,
  onChange,
  disabled,
  label = 'Color',
  size = 20,
}: {
  value: string
  onChange: (c: string) => void
  disabled?: boolean
  label?: string
  size?: number
}) {
  const pop = usePopover()
  const [custom, setCustom] = useState(value || '#6366f1')
  return (
    <>
      <button
        ref={pop.anchor}
        type="button"
        aria-label={label}
        title={label}
        disabled={disabled}
        onClick={pop.toggle}
        className="inline-flex shrink-0 items-center justify-center rounded-full ring-offset-2 ring-offset-panel transition-shadow hover:ring-2 hover:ring-line-strong disabled:pointer-events-none"
        style={{ width: size, height: size, background: value || 'var(--wp-line-strong)' }}
      />
      <Popover open={pop.open} onClose={pop.close} anchor={pop.anchor}>
        <div className="p-2">
          <div className="grid grid-cols-7 gap-1.5">
            {SWATCHES.map((c) => (
              <button
                key={c}
                type="button"
                aria-label={c}
                onClick={() => {
                  onChange(c)
                  pop.close()
                }}
                className="flex size-7 items-center justify-center rounded-full sm:size-6"
                style={{ background: c }}
              >
                {c.toLowerCase() === value.toLowerCase() && (
                  <Check className="size-3.5 text-white" strokeWidth={3} />
                )}
              </button>
            ))}
          </div>
          <label className="mt-2 flex items-center gap-2 border-t border-line pt-2 text-xs text-muted">
            <input
              type="color"
              value={custom}
              onChange={(e) => setCustom(e.target.value)}
              ref={(el) => {
                if (el) el.onchange = () => el.value !== value && onChange(el.value)
              }}
              className="size-6 cursor-pointer rounded border-0 bg-transparent p-0"
            />
            Custom color
          </label>
        </div>
      </Popover>
    </>
  )
}

/** A text field that saves on blur or Enter and reverts on Escape. */
export function InlineText({
  value,
  onSave,
  disabled,
  className,
  placeholder,
  ariaLabel,
}: {
  value: string
  onSave: (v: string) => void
  disabled?: boolean
  className?: string
  placeholder?: string
  ariaLabel?: string
}) {
  const [text, setText] = useState(value)
  const [prev, setPrev] = useState(value)
  if (prev !== value) {
    setPrev(value)
    setText(value)
  }
  const cancelled = useRef(false)
  const commit = () => {
    const t = text.trim()
    if (cancelled.current || !t) {
      cancelled.current = false
      return setText(value)
    }
    if (t !== value) onSave(t)
  }
  return (
    <input
      value={text}
      disabled={disabled}
      aria-label={ariaLabel}
      placeholder={placeholder}
      onChange={(e) => setText(e.target.value)}
      onBlur={commit}
      onKeyDown={(e) => {
        if (e.key === 'Enter') e.currentTarget.blur()
        if (e.key === 'Escape') {
          cancelled.current = true
          e.currentTarget.blur()
        }
      }}
      className={clsx(
        'h-8 min-w-0 rounded-md border border-transparent bg-transparent px-2 text-[13px] outline-none transition-colors hover:border-line focus:border-accent focus:bg-panel disabled:hover:border-transparent',
        className,
      )}
    />
  )
}

/** Refetch the project and everything that shows its statuses/labels/members. */
export function useRefreshProject(key: string) {
  const qc = useQueryClient()
  return () => invalidateProject(qc, key)
}

/** A bordered list container with dividers between rows. */
export function List({ children, className }: { children: ReactNode; className?: string }) {
  return (
    <div
      className={clsx(
        'divide-y divide-line overflow-hidden rounded-xl border border-line bg-panel',
        className,
      )}
    >
      {children}
    </div>
  )
}
