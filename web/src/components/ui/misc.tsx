import type { ReactNode } from 'react'
import clsx from 'clsx'
import { hashColor, initials } from '@/lib/format'
import { HelpButton, HelpPanel, useHelp } from './help'
import type { UserRef } from '@/lib/types'

export function Avatar({
  user,
  size = 24,
  className,
  ring,
}: {
  user?: UserRef | null
  size?: number
  className?: string
  ring?: boolean
}) {
  const style = { width: size, height: size, fontSize: Math.max(9, size * 0.4) }
  if (!user) {
    return (
      <span
        data-avatar=""
        style={style}
        className={clsx(
          'inline-block shrink-0 rounded-full border border-dashed border-line-strong',
          className,
        )}
      />
    )
  }
  const name = user.display_name || user.username
  if (user.avatar_url) {
    return (
      <img
        data-avatar=""
        src={user.avatar_url}
        alt={name}
        title={name}
        loading="lazy"
        style={style}
        className={clsx(
          'inline-block shrink-0 rounded-full object-cover',
          ring && 'ring-2 ring-panel',
          className,
        )}
      />
    )
  }
  return (
    <span
      data-avatar=""
      title={name}
      style={{ ...style, background: hashColor(user.username) }}
      className={clsx(
        'inline-flex shrink-0 select-none items-center justify-center rounded-full font-semibold text-white',
        ring && 'ring-2 ring-panel',
        className,
      )}
    >
      {initials(name)}
    </span>
  )
}

export function AvatarStack({
  users,
  size = 22,
  max = 3,
}: {
  users: UserRef[]
  size?: number
  max?: number
}) {
  if (users.length === 0) return null
  const shown = users.slice(0, max)
  return (
    <span className="flex -space-x-1.5">
      {shown.map((u) => (
        <Avatar key={u.id} user={u} size={size} ring />
      ))}
      {users.length > max && (
        <span
          style={{ width: size, height: size }}
          className="inline-flex items-center justify-center rounded-full bg-sunken text-[10px] text-muted ring-2 ring-panel"
        >
          +{users.length - max}
        </span>
      )}
    </span>
  )
}

export function Kbd({ children }: { children: ReactNode }) {
  return (
    <kbd className="inline-flex h-5 min-w-5 items-center justify-center rounded border border-line bg-sunken px-1 font-sans text-[11px] text-muted">
      {children}
    </kbd>
  )
}

export function Badge({
  children,
  color,
  title,
  icon,
  className,
}: {
  children: ReactNode
  color?: string
  /** The tooltip, and the accessible name when the badge is only a number or icon. */
  title?: string
  /** The badge holds only an icon (no text), so its title names it. */
  icon?: boolean
  className?: string
}) {
  // A bare count says nothing read aloud; its tooltip does. A plain span's
  // aria-label isn't read, so the badge is named as an image.
  const named =
    !!title &&
    (icon || typeof children === 'number' || /^[\d×+\-\u2212.,%\s]+$/.test(String(children)))
  return (
    <span
      title={title}
      role={named ? 'img' : undefined}
      aria-label={named ? title : undefined}
      className={clsx(
        'inline-flex h-5 items-center gap-1 rounded-md px-1.5 text-[11px] font-medium tabular-nums',
        !color && 'bg-sunken text-muted',
        title && 'cursor-help',
        className,
      )}
      style={
        color
          ? {
              background: `color-mix(in oklab, ${color} 16%, transparent)`,
              color: `color-mix(in oklab, ${color} 80%, var(--wp-fg))`,
            }
          : undefined
      }
    >
      {children}
    </span>
  )
}

export function Progress({
  value,
  max,
  className,
  color,
}: {
  value: number
  max: number
  className?: string
  color?: string
}) {
  const pct = max > 0 ? Math.min(100, (value / max) * 100) : 0
  return (
    <div
      role="progressbar"
      aria-valuemin={0}
      aria-valuemax={max}
      aria-valuenow={value}
      className={clsx('h-1.5 overflow-hidden rounded-full bg-sunken', className)}
    >
      <div
        className="h-full rounded-full transition-[width]"
        style={{ width: pct + '%', background: color ?? 'var(--wp-ok)' }}
      />
    </div>
  )
}

export function Empty({
  icon,
  title,
  children,
  action,
}: {
  icon?: ReactNode
  title: string
  children?: ReactNode
  action?: ReactNode
}) {
  return (
    <div className="flex flex-col items-center justify-center gap-1.5 px-6 py-8 text-center">
      {icon && <div className="text-subtle [&>svg]:size-5">{icon}</div>}
      <div className="text-[13px] text-muted">{title}</div>
      {children && <div className="max-w-sm text-xs text-subtle">{children}</div>}
      {action && <div className="mt-1">{action}</div>}
    </div>
  )
}

/** What a list shows when its request failed, instead of an empty claim. */
export function LoadError({ what, error }: { what: string; error: unknown }) {
  return (
    <p role="alert" className="px-4 py-6 text-center text-[13px] text-danger">
      {what} didn’t load{error instanceof Error ? `: ${error.message}` : ''}.
    </p>
  )
}

/**
 * A panel with an optional title row. `help` is an explainer under the
 * title that the person can fold into a (?) beside it.
 */
export function Section({
  title,
  children,
  actions,
  help,
  helpId,
  className,
}: {
  title?: ReactNode
  children: ReactNode
  actions?: ReactNode
  help?: ReactNode
  /** remembers the fold; defaults to the title */
  helpId?: string
  className?: string
}) {
  const h = useHelp(helpId ?? `section:${typeof title === 'string' ? title.toLowerCase() : ''}`)
  return (
    <section className={clsx('rounded-xl border border-line bg-panel', className)}>
      {(title || actions) && (
        <div className="flex items-center justify-between gap-2 border-b border-line px-4 py-2.5">
          <div className="flex min-w-0 items-center gap-1">
            <h3 className="text-[13px] font-semibold">{title}</h3>
            {help && <HelpButton help={h} label={typeof title === 'string' ? title : 'this'} />}
          </div>
          {actions}
        </div>
      )}
      {help && (
        <HelpPanel help={h} bare className="border-b border-line">
          {help}
        </HelpPanel>
      )}
      {children}
    </section>
  )
}

export function Skeleton({ className }: { className?: string }) {
  return <div aria-hidden className={clsx('animate-pulse rounded-md bg-sunken', className)} />
}

export function PageHeader({
  title,
  subtitle,
  actions,
  icon,
}: {
  title: ReactNode
  subtitle?: ReactNode
  actions?: ReactNode
  icon?: ReactNode
}) {
  return (
    <div className="flex flex-wrap items-center justify-between gap-3 px-4 pb-3 pt-4 sm:px-6 sm:pt-5">
      <div className="flex min-w-0 items-center gap-3">
        {icon}
        <div className="min-w-0">
          <h1 className="truncate text-lg font-semibold tracking-tight">{title}</h1>
          {subtitle && <div className="truncate text-[13px] text-muted">{subtitle}</div>}
        </div>
      </div>
      {actions && <div className="flex items-center gap-2">{actions}</div>}
    </div>
  )
}
