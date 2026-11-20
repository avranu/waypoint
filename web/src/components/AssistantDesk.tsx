// Your assistant on the home page (WP-ADR1): how it is, a box to hand it
// something, and what it has been talking about.

import { useState } from 'react'
import { Link, useNavigate } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import { ArrowUp, Sparkles } from 'lucide-react'
import {
  assistantsQuery,
  health,
  HEALTH_COLOR,
  HEALTH_LABEL,
  iso,
  sessionsQuery,
  sessionTitle,
  sourceLabel,
  type Assistant,
} from '@/lib/assistant'
import { relativeTime } from '@/lib/format'

export function AssistantDesk() {
  const mine = (useQuery(assistantsQuery()).data ?? []).filter((a) => a.mine)
  if (!mine.length) return null
  return (
    <>
      {mine.map((a) => (
        <Desk key={a.id} a={a} />
      ))}
    </>
  )
}

function Desk({ a }: { a: Assistant }) {
  const navigate = useNavigate()
  const [text, setText] = useState('')
  const h = health(a)
  const usable = a.state.reachable && !a.state.frozen
  const recent =
    useQuery({ ...sessionsQuery(a.id, { limit: 4 }), enabled: usable }).data?.sessions ?? []
  const ask = () => {
    const t = text.trim()
    if (!t) return
    navigate({ to: '/assistant/$id', params: { id: String(a.id) }, search: { ask: t, send: '1' } })
  }
  return (
    <section className="overflow-hidden rounded-xl border border-line bg-panel">
      <div className="flex items-center gap-2 px-3 pt-2.5">
        <Sparkles className="size-4 text-accent" />
        <Link
          to="/assistant/$id"
          params={{ id: String(a.id) }}
          className="text-[13px] font-semibold hover:underline"
        >
          {a.name}
        </Link>
        <span className="flex items-center gap-1 text-xs text-muted">
          <span className="size-1.5 rounded-full" style={{ background: HEALTH_COLOR[h] }} />
          {a.state.pending.length ? `${a.state.pending.length} waiting on Signal` : HEALTH_LABEL[h]}
        </span>
      </div>
      <form
        className="flex items-center gap-2 px-3 py-2"
        onSubmit={(e) => {
          e.preventDefault()
          ask()
        }}
      >
        <input
          value={text}
          disabled={!usable}
          onChange={(e) => setText(e.target.value)}
          placeholder={usable ? `Ask ${a.name}…` : HEALTH_LABEL[h]}
          aria-label={`Ask ${a.name}`}
          className="h-8 min-w-0 flex-1 bg-transparent text-sm outline-none placeholder:text-subtle"
        />
        <button
          type="submit"
          disabled={!text.trim()}
          aria-label="Send"
          className="flex size-7 items-center justify-center rounded-full bg-accent text-accent-fg disabled:opacity-40"
        >
          <ArrowUp className="size-4" />
        </button>
      </form>
      {recent.length > 0 && (
        <ul className="border-t border-line">
          {recent.map((s) => (
            <li key={s.id}>
              <Link
                to="/assistant/$id"
                params={{ id: String(a.id) }}
                search={{ s: s.id }}
                className="flex items-center gap-2 px-3 py-1.5 text-[13px] hover:bg-hover"
              >
                {s.is_active ? (
                  <span
                    className="size-1.5 shrink-0 rounded-full bg-ok"
                    aria-label="Active lately"
                  />
                ) : (
                  <span className="size-1.5 shrink-0" />
                )}
                <span className="min-w-0 flex-1 truncate">{sessionTitle(s)}</span>
                <span className="shrink-0 text-xs text-subtle">
                  {s.source !== 'waypoint' && `${sourceLabel(s.source)} · `}
                  {relativeTime(iso(s.last_active || s.started_at))}
                </span>
              </Link>
            </li>
          ))}
        </ul>
      )}
    </section>
  )
}
