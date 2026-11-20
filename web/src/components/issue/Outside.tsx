// Text from outside (WP-279): comments, and new text for what a person
// approved, that a forge user who isn't a Waypoint person wrote. It waits
// here, unseen by agents and models, until a person approves or discards it.
// Shown as plain text: nothing in it renders, loads or links by itself.

import { useQuery } from '@tanstack/react-query'
import { ExternalLink, Globe } from 'lucide-react'
import { q, useDecideOutside } from '@/lib/queries'
import { dateTime, relativeTime, safeUrl } from '@/lib/format'
import type { Issue, OutsideText } from '@/lib/types'
import { Button } from '@/components/ui/button'
import { toast, toastError } from '@/components/ui/toast'

const WHAT: Record<OutsideText['kind'], string> = {
  comment: 'Comment',
  comment_edit: 'Edit to a comment',
  issue_edit: 'New title and description',
}

export function OutsideTexts({ issue, editable }: { issue: Issue; editable: boolean }) {
  const held = useQuery(q.outside(issue.key))
  const decide = useDecideOutside(issue.key, issue.project_key)
  const list = held.data ?? []
  if (!list.length) return null
  const act = (o: OutsideText, approve: boolean) =>
    decide.mutate(
      { id: o.id, approve },
      {
        onSuccess: () =>
          toast(approve ? 'Approved: agents can read it now' : 'Discarded: it stays out'),
        onError: toastError,
      },
    )
  return (
    <section aria-label="Text from outside">
      <h3 className="mb-1.5 flex items-center gap-1.5 text-xs font-semibold uppercase tracking-wide text-subtle">
        <Globe className="size-3.5" aria-hidden />
        Text from outside · {list.length}
      </h3>
      <p className="mb-2 text-[13px] text-muted">
        Written on a forge by people who aren't Waypoint people. No agent or model sees it until you
        approve it.
      </p>
      <ul className="space-y-2">
        {list.map((o) => (
          <li
            key={o.id}
            className="rounded-lg border border-dashed px-3 py-2"
            style={{ borderColor: 'color-mix(in oklab, var(--wp-warn) 55%, transparent)' }}
          >
            <div className="flex flex-wrap items-center gap-x-2 gap-y-1 text-[13px]">
              <span className="font-medium">{WHAT[o.kind]}</span>
              <span className="text-muted">
                by <span className="font-mono">{o.author || 'someone'}</span> · {o.via}
              </span>
              <span className="text-subtle" title={dateTime(o.updated_at)}>
                {relativeTime(o.updated_at)}
              </span>
              {safeUrl(o.url) && (
                <a
                  href={safeUrl(o.url)}
                  target="_blank"
                  rel="noreferrer noopener"
                  className="inline-flex items-center gap-1 text-subtle hover:text-fg"
                >
                  <ExternalLink className="size-3" aria-hidden />
                  on {o.forge}
                </a>
              )}
            </div>
            {o.kind === 'issue_edit' && o.title && (
              <p className="mt-1.5 text-[14px] font-medium [overflow-wrap:anywhere]">{o.title}</p>
            )}
            <p className="mt-1 whitespace-pre-wrap text-[13px] [overflow-wrap:anywhere]">
              {o.body || <span className="text-subtle">(empty)</span>}
            </p>
            {o.kind === 'comment_edit' && o.was && (
              <details className="mt-1 text-[13px] text-muted">
                <summary className="cursor-pointer">Approved text it replaces</summary>
                <p className="mt-1 whitespace-pre-wrap [overflow-wrap:anywhere]">{o.was}</p>
              </details>
            )}
            {editable && (
              <div className="mt-2 flex gap-2">
                <Button
                  size="sm"
                  variant="primary"
                  loading={
                    decide.isPending && decide.variables?.id === o.id && decide.variables.approve
                  }
                  onClick={() => act(o, true)}
                >
                  Approve
                </Button>
                <Button
                  size="sm"
                  variant="ghost"
                  loading={
                    decide.isPending && decide.variables?.id === o.id && !decide.variables.approve
                  }
                  onClick={() => act(o, false)}
                >
                  Discard
                </Button>
              </div>
            )}
          </li>
        ))}
      </ul>
    </section>
  )
}
