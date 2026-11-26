import { useNavigate } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import { BookOpenCheck, TriangleAlert } from 'lucide-react'
import { knowledgeOverviewQuery, type AvoidedWord, type UsedItem } from '@/lib/usage'
import { itemHref } from '@/lib/knowledge'
import { dateTime, plural, relativeTime } from '@/lib/format'
import { agentLabel } from '@/lib/agents'
import { Empty, Section, Skeleton } from '@/components/ui/misc'
import { HBarList, fmtNum } from '@/components/charts'
import { ItemIcon } from '@/components/Links'
import { SourceLink, UseRow, times } from '@/components/Usage'
import { Who } from '@/components/knowledge'
import type { HubTabProps } from './search'

export function KnowledgeTab({ filters }: HubTabProps) {
  const ov = useQuery(knowledgeOverviewQuery(filters))
  const navigate = useNavigate()
  const period = `the last ${filters.days} days`
  if (ov.isLoading)
    return (
      <div className="space-y-4">
        <Skeleton className="h-20" />
        <div className="grid gap-4 lg:grid-cols-2">
          <Skeleton className="h-64" />
          <Skeleton className="h-64" />
        </div>
      </div>
    )
  if (ov.error || !ov.data)
    return <p className="text-[13px] text-danger">Couldn’t load knowledge use.</p>
  const d = ov.data
  const t = d.totals
  const who = filters.agent ? agentLabel(filters.agent) : 'people and agents'
  return (
    <div className="space-y-4">
      <Section
        title={`In ${period}`}
        helpId="agents:knowledge"
        help={
          <p>
            When the glossary, pages and design notes are used. A <b>lookup</b> is a tool reading
            one: <code>wp define</code>, <code>wp page show</code>, <code>wp spec</code>, MCP or the
            API (pages people open in the browser aren’t counted). A <b>mention</b> is text that
            names one: a [[link]], a page code like WP-D11, a term or its alias, counted once per
            issue, comment, page or pull request. A <b>word to avoid</b> is a glossary entry’s
            rejected name written where its term belongs.
          </p>
        }
      >
        <dl className="grid grid-cols-2 gap-4 p-4 sm:grid-cols-4">
          <Stat label="Lookups" value={t.lookups} />
          <Stat label="Places naming them" value={t.mentions} />
          <Stat label="Words to avoid written" value={t.avoided} warn={t.avoided > 0} />
          <Stat label="Items used" value={t.items_used} />
        </dl>
      </Section>

      {t.items_used === 0 && d.unused.length === 0 ? (
        <div className="rounded-xl border border-dashed border-line">
          <Empty icon={<BookOpenCheck />} title={`No knowledge used in ${period}`}>
            {filters.agent
              ? `${agentLabel(filters.agent)} didn’t look anything up or name a term, page or design note.`
              : 'When agents run wp define, wp page show or wp spec, or anyone writes a term, a [[link]] or a page code like WP-D11, it shows here.'}
          </Empty>
        </div>
      ) : (
        <>
          {d.avoided.length > 0 && (
            <Section title="Words to avoid that were written">
              <ul>
                {d.avoided.map((a, i) => (
                  <AvoidedRow key={i} a={a} />
                ))}
              </ul>
            </Section>
          )}
          <div className="grid gap-4 lg:grid-cols-2">
            <Section title="Most used">
              <div className="p-4">
                {d.top.length === 0 ? (
                  <p className="text-[13px] text-subtle">
                    Nothing used by {who} in {period}.
                  </p>
                ) : (
                  <HBarList
                    ariaLabel="Most used glossary entries, pages and design notes"
                    items={d.top.map((u) => ({
                      key: `${u.item.type}:${u.item.id}`,
                      prefix: prefix(u),
                      label: u.item.title,
                      value: u.lookups + u.mentions + u.avoided,
                      display: summarize(u),
                      onClick: () => {
                        const href = itemHref(u.item)
                        if (href) navigate({ to: href })
                      },
                    }))}
                  />
                )}
              </div>
            </Section>
            <Section title="What agents read">
              {d.lookups.length === 0 ? (
                <p className="p-4 text-[13px] text-subtle">
                  No agent looked anything up in {period}. Agents read terms with{' '}
                  <code>wp define</code>, plans with <code>wp page show</code> and features with{' '}
                  <code>wp spec</code>.
                </p>
              ) : (
                <ol>
                  {d.lookups.map((u, i) => (
                    <UseRow key={i} use={u} item={u.item} />
                  ))}
                </ol>
              )}
            </Section>
          </div>
          {d.unused.length > 0 && (
            <Section title={`Not used in ${period}`}>
              <ul className="grid sm:grid-cols-2">
                {d.unused.map((u) => (
                  <li
                    key={`${u.item.type}:${u.item.id}`}
                    className="flex min-w-0 items-center gap-2 border-b border-line px-4 py-2 text-[13px]"
                  >
                    <ItemIcon item={u.item} />
                    <span className="min-w-0 flex-1 truncate">
                      <SourceLink item={u.item} />
                    </span>
                    <span
                      className="shrink-0 text-xs text-subtle"
                      title={u.last_used_at ? dateTime(u.last_used_at) : undefined}
                    >
                      {u.last_used_at ? `last ${relativeTime(u.last_used_at)}` : 'never used'}
                    </span>
                  </li>
                ))}
              </ul>
              {d.unused_total > d.unused.length && (
                <p className="px-4 py-2 text-xs text-subtle">
                  and {d.unused_total - d.unused.length} more. Unused items may be out of date, or
                  missing from agents’ instructions.
                </p>
              )}
            </Section>
          )}
        </>
      )}
    </div>
  )
}

function Stat({ label, value, warn }: { label: string; value: number; warn?: boolean }) {
  return (
    <div className="min-w-0">
      <dt className="truncate text-xs text-muted">{label}</dt>
      <dd className={warn ? 'text-xl font-semibold text-warn' : 'text-xl font-semibold'}>
        {fmtNum(value)}
      </dd>
    </div>
  )
}

function prefix(u: UsedItem) {
  if (u.item.type === 'page') return u.item.key || 'Page'
  return u.item.type === 'glossary' ? 'Term' : 'Design'
}

function summarize(u: UsedItem) {
  const parts = []
  if (u.lookups) parts.push(plural(u.lookups, 'lookup'))
  if (u.mentions) parts.push(plural(u.mentions, 'place'))
  if (u.avoided) parts.push(`${u.avoided} avoided`)
  return parts.join(' · ')
}

function AvoidedRow({ a }: { a: AvoidedWord }) {
  const l = a.latest
  return (
    <li className="flex items-start gap-2 border-b border-line px-4 py-2.5 text-[13px] last:border-b-0">
      <TriangleAlert className="mt-0.5 size-3.5 shrink-0 text-warn" aria-hidden />
      <div className="min-w-0 flex-1">
        <div>
          <b className="font-medium text-warn">“{a.word}”</b>{' '}
          <span className="text-muted">written {times(a.count)} where the glossary says</span>{' '}
          <SourceLink item={a.entry} />
        </div>
        <div className="mt-0.5 text-xs text-subtle">
          Last by {l.user || l.agent ? <Who user={l.user} agent={l.agent} /> : 'a forge'}
          {l.source && (
            <>
              {' '}
              in {l.comment && 'a comment on '}
              <SourceLink item={l.source} />
            </>
          )}
        </div>
      </div>
      <time className="shrink-0 text-xs text-subtle" title={dateTime(l.at)}>
        {relativeTime(l.at)}
      </time>
    </li>
  )
}
