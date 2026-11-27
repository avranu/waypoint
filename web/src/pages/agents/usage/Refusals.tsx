import { useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { ShieldAlert } from 'lucide-react'
import { dateTime, shortDate } from '@/lib/format'
import {
  usageRefusalsQuery,
  type UsageFilters,
  type UsageOverview,
  type UsageRefusalRow,
} from '@/lib/usage-analytics'
import { Button } from '@/components/ui/button'
import { toastError } from '@/components/ui/toast'
import { Badge, Section } from '@/components/ui/misc'
import { HBarList, Sparkbars, viz } from '@/components/charts'
import { CopyButton } from './shared'

const SOURCES: Record<string, string> = {
  classifier: 'classifier',
  hook: 'hook',
  safety_check: 'safety check',
  rule: 'rule',
  user: 'you',
}

export function RefusalsSection({ o, filters }: { o: UsageOverview; filters: UsageFilters }) {
  const r = o.refusals
  return (
    <Section
      title="Permission refusals"
      helpId="usage:refusals"
      help={
        <p>
          Tool calls that did not run. <b>Classifier</b> is Claude Code’s auto-mode classifier (the
          bracketed category is its reason), <b>hook</b> is one of your hooks, <b>safety check</b>{' '}
          is built in, <b>rule</b> a permission rule, and <b>you</b> a prompt you declined. Hooks
          and the safety check can’t be overridden by an allow rule, so none is suggested for them.
          The first 300 characters of the input are kept (with obvious secrets hidden) so you can
          see what was refused; they stay on this server, visible to you and admins only.
        </p>
      }
    >
      {r.total === 0 ? (
        <p className="px-4 py-8 text-center text-[13px] text-muted">
          Nothing was refused in this period.
        </p>
      ) : (
        <div className="divide-y divide-line">
          <div className="grid gap-x-6 gap-y-4 p-4 md:grid-cols-2">
            <div>
              <h4 className="mb-2 text-xs font-medium text-muted">By reason · {r.total} in all</h4>
              <HBarList
                ariaLabel="Refusals by reason"
                color={viz.orange}
                items={r.reasons.slice(0, 8).map((x) => ({
                  key: x.reason + x.source,
                  label: x.reason || 'No reason given',
                  prefix: SOURCES[x.source] ?? x.source,
                  value: x.count,
                  display: String(x.count),
                }))}
              />
            </div>
            <div>
              <h4 className="mb-1 text-xs font-medium text-muted">Per day</h4>
              <Sparkbars
                ariaLabel="Refusals per day"
                name="Refusals"
                color={viz.orange}
                values={r.daily.map((d) => d.count)}
                labels={r.daily.map((d) => shortDate(d.date))}
                height={64}
              />
            </div>
          </div>
          <Commands o={o} />
          <Recent o={o} filters={filters} />
        </div>
      )}
    </Section>
  )
}

/** Bash heads show as written; other tools by name (MCP ones without the prefix). */
function toolLabel(tool: string, detail: string) {
  if (tool === 'Bash') return detail || 'Bash'
  if (tool.startsWith('mcp__')) return tool.slice(5).replace('__', ' · ')
  return tool
}

function Commands({ o }: { o: UsageOverview }) {
  const cmds = o.refusals.commands.slice(0, 8)
  return (
    <div className="p-4">
      <h4 className="mb-2 text-xs font-medium text-muted">Most refused</h4>
      <ul className="divide-y divide-line">
        {cmds.map((c) => (
          <li
            key={c.tool + c.detail}
            className="flex flex-wrap items-center gap-x-3 gap-y-1 py-1.5 text-[13px]"
          >
            <span
              className="min-w-0 flex-1 truncate font-medium"
              title={toolLabel(c.tool, c.detail)}
            >
              {toolLabel(c.tool, c.detail)}
            </span>
            <span className="tabular-nums text-muted">{c.count}×</span>
            <span className="hidden max-w-[14rem] truncate text-xs text-subtle sm:inline">
              {c.reasons.join(', ')}
            </span>
            {c.rule ? (
              <CopyButton text={ruleSnippet(c.rule)} label="Copy allow rule" />
            ) : (
              <span className="text-xs text-subtle">no rule can allow it</span>
            )}
          </li>
        ))}
      </ul>
    </div>
  )
}

/** The settings.json to merge in for an allow rule. */
export function ruleSnippet(rule: string) {
  return JSON.stringify({ permissions: { allow: [rule] } }, null, 2)
}

function Recent({ o, filters }: { o: UsageOverview; filters: UsageFilters }) {
  const qc = useQueryClient()
  const [extra, setExtra] = useState<UsageRefusalRow[]>([])
  const [loading, setLoading] = useState(false)
  const rows = [...o.refusals.recent, ...extra]
  const loadMore = async () => {
    setLoading(true)
    try {
      const page = await qc.fetchQuery(usageRefusalsQuery(filters, rows[rows.length - 1].id))
      setExtra((e) => [...e, ...page])
    } catch (e) {
      toastError(e)
    } finally {
      setLoading(false)
    }
  }
  return (
    <div className="p-4">
      <h4 className="mb-2 text-xs font-medium text-muted">Latest</h4>
      <ul className="space-y-2">
        {rows.map((x) => (
          <li key={x.id} className="rounded-lg border border-line px-3 py-2 text-[13px]">
            <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
              <ShieldAlert className="size-3.5 shrink-0 text-warn" />
              <span className="font-medium">{x.reason || 'Refused'}</span>
              <Badge>{SOURCES[x.source] ?? x.source}</Badge>
              <span className="text-xs text-subtle">
                {toolLabel(x.tool, x.detail)} · {dateTime(x.at)}
                {x.repo ? ` · ${x.repo}` : ''}
              </span>
            </div>
            {x.excerpt && (
              <pre className="mt-1.5 max-h-24 overflow-auto whitespace-pre-wrap break-all rounded-md bg-sunken px-2 py-1.5 font-mono text-xs text-muted">
                {x.excerpt}
              </pre>
            )}
          </li>
        ))}
      </ul>
      {o.refusals.total > rows.length && (
        <div className="mt-2">
          <Button size="xs" variant="ghost" loading={loading} onClick={loadMore}>
            Show older
          </Button>
        </div>
      )}
    </div>
  )
}
