// Agents → Audit → Refused calls (WP-232, ADR115): tool calls Claude Code's
// auto-mode classifier refused. They are logged here, not filed as
// requests: the agent usually works on another way, and asks for what it
// still needs itself. A person can turn repeated ones into an allow rule
// (WP-233, WP-ADR114): the rules that would have let them through are
// ranked at the top, and each call offers its own; a pick is added to the
// repo's .claude/settings.json by pull request.

import { useMemo, useState } from 'react'
import { Link } from '@tanstack/react-router'
import { useInfiniteQuery } from '@tanstack/react-query'
import { GitPullRequest, HardDrive, ShieldCheck, ShieldX } from 'lucide-react'
import { dateTime, relativeTime } from '@/lib/format'
import { agentLabel } from '@/lib/agents'
import {
  refusalsQuery,
  useAllowRefused,
  type RuleSuggestion,
  type ToolRefusal,
} from '@/lib/requests'
import { pickedRule, RuleChoice, ruleOwn } from '@/components/RuleChoice'
import { Button } from '@/components/ui/button'
import { Badge, Empty, Skeleton } from '@/components/ui/misc'
import { Explainer } from '@/components/ui/help'
import type { HubTabProps } from './search'

export function RefusedCalls({ filters }: HubTabProps) {
  const list = useInfiniteQuery(
    refusalsQuery({
      project: filters.project || undefined,
      agent: filters.agent || undefined,
      days: filters.days,
    }),
  )
  const rows = useMemo(() => list.data?.pages.flatMap((p) => p.refusals) ?? [], [list.data])
  const c = list.data?.pages[0]?.counts
  const rules = list.data?.pages[0]?.rules ?? []
  const tools = Object.entries(c?.tools ?? {}).sort((a, b) => b[1] - a[1])
  const narrowed = !!(filters.agent || filters.project)

  return (
    <div className="space-y-4">
      <Explainer id="agents:refused" label="Why these aren't requests">
        Tool calls Claude Code’s auto-mode classifier refused, newest first. The agent usually works
        on another way and files a request itself for what it still needs, so a refusal waits on
        nobody: it is recorded here, as a <b>Refused</b> line on the session’s timeline, and on a
        request the agent files within the hour after it. <b>Ran later</b>: the same command then
        ran in that session, because someone allowed it in the terminal. To stop a call being
        refused again, add an allow rule: <b>Allow rules</b> ranks the narrow rules that would have
        let the period’s repeated refusals through, and each call offers its own. The rule is added
        to the repo’s <code>.claude/settings.json</code> by a pull request that merges once its
        checks pass. You see refusals in your own sessions and in projects you edit; admins see all.
      </Explainer>

      {c && c.refusals > 0 && (
        <p className="text-[13px] text-muted">
          Last {filters.days} days: <b className="font-medium text-fg">{c.calls}</b> refused{' '}
          {c.calls === 1 ? 'call' : 'calls'}
          {c.calls !== c.refusals && <> ({c.refusals} different)</>} in {c.sessions}{' '}
          {c.sessions === 1 ? 'session' : 'sessions'}
          {c.ran > 0 && <>, {c.ran} ran later</>}
          {tools.length > 1 && <> · {tools.map(([t, n]) => `${t} ${n}`).join(', ')}</>}.
        </p>
      )}

      {rules.length > 0 && <SuggestedRules rules={rules} />}

      {list.isLoading ? (
        <div className="space-y-2">
          <Skeleton className="h-4 w-24" />
          <Skeleton className="h-40" />
        </div>
      ) : list.isError ? (
        <div className="rounded-xl border border-danger/40 bg-danger/5 px-4 py-3 text-[13px] text-danger">
          Couldn’t load refused calls: {(list.error as Error).message}
        </div>
      ) : rows.length === 0 ? (
        <div className="rounded-xl border border-dashed border-line">
          <Empty icon={<ShieldX />} title={`No refused calls in the last ${filters.days} days`}>
            {narrowed
              ? 'Nothing matches these filters. Try every agent and project, or a longer period.'
              : 'Calls the auto-mode classifier refuses appear here as agents’ hooks report them.'}
          </Empty>
        </div>
      ) : (
        <>
          <ul className="overflow-hidden rounded-xl border border-line bg-panel">
            {rows.map((x) => (
              <RefusalRow key={x.id} x={x} />
            ))}
          </ul>
          {list.hasNextPage && (
            <div className="flex justify-center">
              <Button
                size="sm"
                loading={list.isFetchingNextPage}
                onClick={() => list.fetchNextPage()}
              >
                Load older
              </Button>
            </div>
          )}
        </>
      )}
    </div>
  )
}

/** The rules that would have let the period's repeated refusals through, most useful first. */
function SuggestedRules({ rules }: { rules: RuleSuggestion[] }) {
  const allow = useAllowRefused()
  return (
    <section
      aria-label="Allow rules"
      className="overflow-hidden rounded-xl border border-line bg-panel"
    >
      <h2 className="flex items-center gap-1.5 border-b border-line px-3 py-2 text-xs font-medium text-muted sm:px-4">
        <ShieldCheck className="size-3.5" />
        Allow rules that would have let these through
      </h2>
      <ul>
        {rules.map((g) => (
          <li
            key={`${g.repo} ${g.rule}`}
            className="flex flex-wrap items-center gap-x-3 gap-y-1.5 border-b border-line px-3 py-2.5 last:border-b-0 sm:px-4"
          >
            <div className="min-w-0 flex-1 basis-56">
              <code className="font-mono text-[12px] break-all">{g.rule}</code>
              <p className="text-xs text-muted">From now on: {g.covers}.</p>
              <p className="text-xs text-subtle">
                {g.calls} refused {g.calls === 1 ? 'call' : 'calls'} in {g.sessions}{' '}
                {g.sessions === 1 ? 'session' : 'sessions'}
                {g.ran > 0 && <>, {g.ran} ran later</>} · {g.repo}
              </p>
            </div>
            {g.added ? (
              <RuleLink rule={g.rule} pr={g.pr} />
            ) : (
              <Button
                size="sm"
                icon={<ShieldCheck className="size-3.5" />}
                loading={
                  allow.isPending &&
                  allow.variables?.id === g.refusal &&
                  allow.variables.rule === g.rule
                }
                disabled={allow.isPending}
                onClick={() => allow.mutate({ id: g.refusal, rule: g.rule })}
              >
                Add rule
              </Button>
            )}
          </li>
        ))}
      </ul>
    </section>
  )
}

function RuleLink({ rule, pr }: { rule: string; pr?: string }) {
  return pr ? (
    <a
      href={pr}
      target="_blank"
      rel="noreferrer noopener"
      className="inline-flex items-center gap-1 text-xs font-medium text-muted hover:text-fg hover:underline"
      title={`The pull request adding ${rule}`}
    >
      <GitPullRequest className="size-3.5" />
      Rule added
    </a>
  ) : (
    <span className="text-xs text-muted">Already allowed</span>
  )
}

/** A refused call's allow rule: the one picked, or a choice of them. */
function RowRule({ x }: { x: ToolRefusal }) {
  const allow = useAllowRefused()
  const [open, setOpen] = useState(false)
  const [rule, setRule] = useState(x.rules?.[0]?.rule ?? ruleOwn)
  const [own, setOwn] = useState('')
  if (x.rule)
    return (
      <div className="mt-1.5 flex flex-wrap items-center gap-x-2 gap-y-1 text-xs">
        <span className="text-muted">Allowed from now on:</span>
        <code className="font-mono break-all">{x.rule}</code>
        <RuleLink rule={x.rule} pr={x.rule_pr} />
      </div>
    )
  const reason = x.rule_reason && (
    <p className="mt-1 text-xs text-danger [overflow-wrap:anywhere]">{x.rule_reason}</p>
  )
  if (!x.rules?.length || !x.rule_repo) return reason || null
  const picked = pickedRule(rule, own)
  return (
    <div className="mt-1.5">
      {reason}
      {open ? (
        <div className="mt-1 space-y-2">
          <RuleChoice
            rules={x.rules}
            repo={x.rule_repo}
            tool={x.tool}
            value={rule}
            own={own}
            onChange={setRule}
            onOwn={setOwn}
          />
          <div className="flex gap-2">
            <Button
              size="sm"
              variant="primary"
              icon={<ShieldCheck className="size-3.5" />}
              loading={allow.isPending}
              disabled={!picked}
              onClick={() => picked && allow.mutate({ id: x.id, rule: picked })}
            >
              Add rule
            </Button>
            <Button size="sm" variant="ghost" onClick={() => setOpen(false)}>
              Cancel
            </Button>
          </div>
        </div>
      ) : (
        <Button
          size="xs"
          variant="ghost"
          className="-ml-2"
          icon={<ShieldCheck className="size-3.5" />}
          onClick={() => setOpen(true)}
        >
          Allow from now on…
        </Button>
      )}
    </div>
  )
}

/** One refused call: what, why, where, and an allow rule. Also used on a request's page. */
export function RefusalRow({ x, compact }: { x: ToolRefusal; compact?: boolean }) {
  const s = x.session
  return (
    <li className="border-b border-line px-3 py-3 last:border-b-0 sm:px-4">
      <div className="flex items-start gap-2">
        <Badge className="mt-px shrink-0 font-mono">{x.tool}</Badge>
        <p className="min-w-0 flex-1 text-[13px] leading-5 text-muted [overflow-wrap:anywhere]">
          {x.repeats > 1 && <span>refused {x.repeats} times</span>}
          {x.ran_at && (
            <span className="text-warn" title={dateTime(x.ran_at)}>
              {x.repeats > 1 && ' · '}ran later
            </span>
          )}
        </p>
        <time
          dateTime={x.at}
          title={dateTime(x.at)}
          className="shrink-0 pt-0.5 text-xs text-subtle tabular-nums"
        >
          {relativeTime(x.at)}
        </time>
      </div>
      {x.command && (
        <pre className="mt-1.5 max-h-24 overflow-auto whitespace-pre-wrap break-all rounded-md bg-sunken px-2 py-1.5 font-mono text-xs">
          {x.command}
        </pre>
      )}
      {x.reason && (
        <p
          className="mt-1 line-clamp-3 text-xs text-muted [overflow-wrap:anywhere]"
          title={x.reason}
        >
          {x.reason}
        </p>
      )}
      <RowRule x={x} />
      {!compact && (
        <div className="mt-1 flex flex-wrap items-center gap-x-2 gap-y-0.5 text-xs text-subtle">
          {x.agent && <span>{agentLabel(x.agent)}</span>}
          {s && (
            <Link
              to="/agents/sessions/$id"
              params={{ id: String(s.id) }}
              className="flex min-w-0 items-center gap-1 hover:text-fg hover:underline"
              title="The agent session it was refused in"
            >
              <HardDrive className="size-3 shrink-0" />
              <span className="truncate">
                {s.machine}
                {s.repo && ` · ${s.repo}`}
              </span>
            </Link>
          )}
          {x.issue && (
            <Link
              to="/issue/$key"
              params={{ key: x.issue.key }}
              className="hover:text-fg hover:underline"
              title={x.issue.title}
            >
              {x.issue.key}
            </Link>
          )}
          {x.request && (
            <Link
              to="/requests/$code"
              params={{ code: x.request }}
              className="hover:text-fg hover:underline"
              title="Filed as a permission request, before refusals were logged"
            >
              {x.request}
            </Link>
          )}
        </div>
      )}
    </li>
  )
}

/** On a request: what the classifier refused in its session in the hour before it was filed. */
export function RefusedBefore({ list }: { list?: ToolRefusal[] }) {
  if (!list?.length) return null
  return (
    <section className="mt-5" aria-label="Refused just before this was asked">
      <h2 className="mb-1.5 flex items-center gap-1.5 text-xs font-medium text-muted">
        <ShieldX className="size-3.5" />
        Refused by auto mode in this session, in the hour before this was asked
      </h2>
      <ul className="overflow-hidden rounded-xl border border-line bg-panel">
        {list.map((x) => (
          <RefusalRow key={x.id} x={x} compact />
        ))}
      </ul>
    </section>
  )
}
