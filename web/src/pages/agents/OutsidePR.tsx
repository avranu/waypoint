// Untrusted text: unseen by agents, automations and models until a person approves it; shown as plain text, never rendered, loaded or linked.

import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { ExternalLink, Globe } from 'lucide-react'
import { decideOutsidePR, outsidePRQuery, type OutsidePRText } from '@/lib/pulls'
import { dateTime, relativeTime, safeUrl } from '@/lib/format'
import { Button } from '@/components/ui/button'
import { toast, toastError } from '@/components/ui/toast'

const WHAT: Record<OutsidePRText['kind'], string> = {
  pull: 'The pull request',
  inline: 'Line comment',
  review: 'Review',
  comment: 'Comment',
}

export function OutsidePR({ prID }: { prID: number }) {
  const qc = useQueryClient()
  const held = useQuery(outsidePRQuery(prID))
  const decide = useMutation({
    mutationFn: (v: { id: number; approve: boolean }) => decideOutsidePR(v.id, v.approve),
    onSuccess: (_, v) =>
      toast(v.approve ? 'Approved: agents and bots can read it now' : 'Discarded: it stays out'),
    onError: toastError,
    onSettled: () => {
      qc.invalidateQueries({ queryKey: ['pulls'] })
      qc.invalidateQueries({ queryKey: ['waiting'] })
    },
  })
  const list = held.data ?? []
  if (!list.length) return null
  return (
    <section aria-label="Text from outside">
      <h3 className="mb-1.5 flex items-center gap-1.5 text-xs font-semibold uppercase tracking-wide text-subtle">
        <Globe className="size-3.5" aria-hidden />
        Text from outside · {list.length}
      </h3>
      <p className="mb-2 text-[13px] text-muted">
        Written on the forge by people who aren't Waypoint people. No agent, bot or model reads it
        until you approve it; approving the pull request covers its code at this head.
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
                by <span className="font-mono">{o.author || 'someone'}</span>
              </span>
              {o.kind === 'pull' && o.head_sha && (
                <span className="font-mono text-xs text-subtle">at {o.head_sha.slice(0, 7)}</span>
              )}
              {o.path && (
                <span className="font-mono text-xs text-subtle [overflow-wrap:anywhere]">
                  {o.path}
                  {o.line ? `:${o.line}` : ''}
                </span>
              )}
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
                  on the forge
                </a>
              )}
            </div>
            {o.kind === 'pull' && o.title && (
              <p className="mt-1.5 text-[14px] font-medium [overflow-wrap:anywhere]">{o.title}</p>
            )}
            <p className="mt-1 whitespace-pre-wrap text-[13px] [overflow-wrap:anywhere]">
              {o.body || <span className="text-subtle">(no description)</span>}
            </p>
            <div className="mt-2 flex gap-2">
              <Button
                size="sm"
                variant="primary"
                loading={
                  decide.isPending && decide.variables?.id === o.id && decide.variables.approve
                }
                onClick={() => decide.mutate({ id: o.id, approve: true })}
              >
                Approve
              </Button>
              {o.kind !== 'pull' && (
                <Button
                  size="sm"
                  variant="ghost"
                  loading={
                    decide.isPending && decide.variables?.id === o.id && !decide.variables.approve
                  }
                  onClick={() => decide.mutate({ id: o.id, approve: false })}
                >
                  Discard
                </Button>
              )}
            </div>
          </li>
        ))}
      </ul>
    </section>
  )
}
