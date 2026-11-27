import { useMemo } from 'react'
import { createPortal } from 'react-dom'
import clsx from 'clsx'
import { Avatar } from '@/components/ui/misc'
import { PARK } from '@/lib/richinput'
import type { UserRef } from '@/lib/types'

/**
 * "@partial" right before the caret. Usernames start with a letter or digit
 * (as markdown.ts's TOKEN has it), so "@.", "@-" and the like open nothing; a
 * bare "@" does.
 */
export const MENTION_RE = new RegExp(`(^|[\\s(${PARK}])@((?:[a-zA-Z0-9][\\w.-]{0,38})?)$`)

/** Up to six people whose username or a word of whose name starts with `text`. */
export function matchUsers(users: UserRef[], text: string | undefined): UserRef[] {
  if (text === undefined) return []
  const t = text.toLowerCase()
  return users
    .filter(
      (u) =>
        u.username.toLowerCase().startsWith(t) ||
        u.display_name
          .toLowerCase()
          .split(/\s+/)
          .some((w) => w.startsWith(t)),
    )
    .slice(0, 6)
}

export function useMentionMatches(users: UserRef[], text: string | undefined) {
  return useMemo(() => matchUsers(users, text), [users, text])
}

export function MentionList({
  users,
  active,
  pos,
  onHover,
  onPick,
}: {
  users: UserRef[]
  active: number
  pos: { top: number; left: number } | null
  onHover: (i: number) => void
  onPick: (u: UserRef) => void
}) {
  return createPortal(
    <div
      role="listbox"
      aria-label="Mention someone"
      style={{ top: pos?.top ?? -9999, left: pos?.left ?? -9999 }}
      className="animate-pop fixed z-[70] w-60 overflow-hidden rounded-xl border border-line bg-raised p-1 shadow-pop"
      onMouseDown={(e) => e.preventDefault()}
    >
      {users.map((u, i) => (
        <button
          key={u.id}
          role="option"
          aria-selected={i === active}
          onMouseEnter={() => onHover(i)}
          onClick={() => onPick(u)}
          className={clsx(
            'flex h-8 w-full items-center gap-2 rounded-lg px-2 text-left text-[13px]',
            i === active && 'bg-hover',
          )}
        >
          <Avatar user={u} size={18} />
          <span className="truncate">{u.display_name}</span>
          <span className="ml-auto shrink-0 text-xs text-subtle">@{u.username}</span>
        </button>
      ))}
    </div>,
    document.body,
  )
}
