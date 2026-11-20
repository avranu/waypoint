// Agent runs on the issue page, and starting a page from an issue.

import { useState } from 'react'
import { useNavigate } from '@tanstack/react-router'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { Bot, FileText } from 'lucide-react'
import { api } from '@/lib/api'
import { agentLabel } from '@/lib/agents'
import { clock, duration, relativeTime } from '@/lib/format'
import { useTick } from '@/lib/hooks'
import type { AgentRun, IssueDetail, Page } from '@/lib/types'
import { toastError } from '@/components/ui/toast'

const fmtTokens = (n: number) => (n >= 10_000 ? `${Math.round(n / 1000)}k` : n.toLocaleString())

const RUNS_MAX = 100

/** Who worked on this issue as an agent, for how long, at what cost. */
export function AgentRuns({ runs }: { runs: AgentRun[] }) {
  const [all, setAll] = useState(false)
  useTick(
    1000,
    runs.some((r) => !r.ended_at),
  )
  // A running run's time counts up from its start.
  const secs = (r: AgentRun) =>
    r.ended_at ? r.seconds : Math.max(r.seconds, (Date.now() - Date.parse(r.started_at)) / 1000)
  const total = runs.reduce(
    (t, r) => ({
      s: t.s + secs(r),
      i: t.i + r.input_tokens,
      o: t.o + r.output_tokens,
      c: t.c + r.cost_usd,
    }),
    { s: 0, i: 0, o: 0, c: 0 },
  )
  // The server sends the latest 100 runs (agentwork.go), so a full list may be more.
  const capped = runs.length >= RUNS_MAX
  const shown = all ? runs : runs.slice(0, 5)
  return (
    <div className="overflow-hidden rounded-lg border border-line">
      <div className="flex flex-wrap items-center gap-x-4 gap-y-1 border-b border-line bg-sunken/60 px-3 py-2 text-xs text-muted">
        <span>
          <b className="font-medium text-fg">{capped ? `${runs.length}+` : runs.length}</b>{' '}
          {runs.length === 1 ? 'run' : 'runs'}
        </span>
        <span title={capped ? `Counted over the latest ${RUNS_MAX} runs` : undefined}>
          <b className="font-medium text-fg">{duration(total.s)}</b> agent time
          {capped && ` (latest ${RUNS_MAX})`}
        </span>
        {(total.i > 0 || total.o > 0) && (
          <span>
            <b className="font-medium text-fg">{fmtTokens(total.i)}</b> in ·{' '}
            <b className="font-medium text-fg">{fmtTokens(total.o)}</b> out
          </span>
        )}
        {total.c > 0 && (
          <span>
            <b className="font-medium text-fg">${total.c.toFixed(2)}</b>
          </span>
        )}
      </div>
      <ul className="divide-y divide-line">
        {shown.map((r) => (
          <li key={r.id} className="flex items-center gap-2.5 px-3 py-2 text-[13px]">
            <Bot
              className={
                r.ended_at
                  ? 'size-3.5 text-subtle'
                  : 'size-3.5 animate-pulse text-accent motion-reduce:animate-none'
              }
            />
            <span
              className="min-w-0 flex-1 truncate"
              title={r.model ? `${r.agent} on ${r.model}` : r.agent}
            >
              {agentLabel(r.agent)}
              {r.model && agentLabel(r.model) !== agentLabel(r.agent) && (
                <span className="text-subtle"> ({agentLabel(r.model)})</span>
              )}
              {r.step && <span className="text-subtle"> · {r.step}</span>}
            </span>
            <span
              className="shrink-0 text-xs text-subtle"
              title={new Date(r.started_at).toLocaleString()}
            >
              {r.ended_at ? (r.outcome || 'ended') + ' ' + relativeTime(r.ended_at) : 'working now'}
            </span>
            <span
              className={
                r.ended_at
                  ? 'w-14 shrink-0 text-right text-xs tabular-nums text-muted'
                  : 'w-14 shrink-0 text-right text-xs font-medium tabular-nums text-accent'
              }
            >
              {r.ended_at ? duration(r.seconds) : clock(secs(r))}
            </span>
            <span className="hidden w-24 shrink-0 text-right text-xs tabular-nums text-subtle sm:inline">
              {r.input_tokens + r.output_tokens > 0
                ? `${fmtTokens(r.input_tokens)}/${fmtTokens(r.output_tokens)}`
                : '–'}
            </span>
          </li>
        ))}
      </ul>
      {runs.length > 5 && (
        <button
          onClick={() => setAll(!all)}
          className="w-full border-t border-line py-1.5 text-xs text-subtle hover:text-fg"
        >
          {all ? 'Show fewer' : `Show all ${runs.length}`}
        </button>
      )}
    </div>
  )
}

/** "New page from this issue": a plan page linked to the issue, opened for writing. */
export function NewPageFromIssue({ issue }: { issue: IssueDetail }) {
  const qc = useQueryClient()
  const navigate = useNavigate()
  const create = useMutation({
    mutationFn: () =>
      api.post<Page>('/pages', {
        title: issue.title,
        category: 'plan',
        project: issue.project_key,
        issues: [issue.key],
        body: '',
      }),
    onSuccess: (p) => {
      qc.invalidateQueries({ queryKey: ['issue', issue.key] })
      qc.invalidateQueries({ queryKey: ['pages'] })
      qc.invalidateQueries({ queryKey: ['links'] })
      navigate({ to: '/pages/$id', params: { id: String(p.id) } })
    },
    onError: toastError,
  })
  return (
    <button
      onClick={() => create.mutate()}
      disabled={create.isPending}
      className="inline-flex items-center gap-1 text-xs text-subtle hover:text-fg"
    >
      <FileText className="size-3.5" /> New page
    </button>
  )
}
