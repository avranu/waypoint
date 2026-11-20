// An issue's conversations with your assistant (WP-ADR1): the ones linked
// to it (started from here, by an automation, or linked from the chat) and
// a way to start one about it.

import { Link } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import { MessagesSquare, Sparkles } from 'lucide-react'
import { assistantsQuery, issueAssistantLinksQuery } from '@/lib/assistant'
import { relativeTime } from '@/lib/format'

export function AssistantChats({ issue }: { issue: { key: string; title: string } }) {
  const mine = (useQuery(assistantsQuery()).data ?? []).filter((a) => a.mine)
  const links =
    useQuery({ ...issueAssistantLinksQuery(issue.key), enabled: mine.length > 0 }).data ?? []
  if (!mine.length) return null
  const ask = mine.map((a) => (
    <Link
      key={a.id}
      to="/assistant/$id"
      params={{ id: String(a.id) }}
      search={{ ask: `About ${issue.key} (“${issue.title}”): `, issue: issue.key }}
      className="inline-flex h-6 items-center gap-1 rounded-md px-1.5 text-xs font-medium text-muted hover:bg-hover hover:text-fg"
    >
      <Sparkles className="size-3.5" />
      Ask {a.name}
    </Link>
  ))
  // No conversations yet: just the button, not a section.
  if (!links.length) return <div className="flex flex-wrap gap-1">{ask}</div>
  return (
    <section>
      <div className="mb-1.5 flex items-center justify-between">
        <h3 className="text-xs font-semibold uppercase tracking-wide text-subtle">Assistant</h3>
        <div className="flex gap-1">{ask}</div>
      </div>
      {links.length > 0 && (
        <ul className="space-y-0.5">
          {links.map((l) => (
            <li key={l.id}>
              <Link
                to="/assistant/$id"
                params={{ id: String(l.assistant_id) }}
                search={{ s: l.session_id }}
                className="flex items-center gap-2 rounded-md px-1.5 py-1 text-[13px] hover:bg-hover"
              >
                <MessagesSquare className="size-3.5 shrink-0 text-subtle" />
                <span className="min-w-0 flex-1 truncate">{l.title || 'Conversation'}</span>
                <span className="shrink-0 text-xs text-subtle">
                  {mine.length > 1 && `${l.assistant} · `}
                  {relativeTime(l.created_at)}
                </span>
              </Link>
            </li>
          ))}
        </ul>
      )}
    </section>
  )
}
