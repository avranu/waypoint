// Agents page, Kit tab (WP-ADR2 §4): the agent kit's plugins and what each
// holds, where each goes, the notes sessions start with, and which
// checkouts are behind or drifted. Admins roll a sync out to their runners.

import { useState } from 'react'
import { Link } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import { CircleAlert, Package, RefreshCw, Rocket } from 'lucide-react'
import { q } from '@/lib/queries'
import {
  KIT_PARTS,
  shortSHA,
  useReadKit,
  useRolloutKit,
  type KitCheckout,
  type KitInfo,
  type KitPlugin,
  type KitRollout,
  type KitTarget,
} from '@/lib/kit'
import { JOB_STATES, runnerJobQuery } from '@/lib/runners'
import { dateTime, plural, relativeTime, safeUrl } from '@/lib/format'
import { Button, IconButton } from '@/components/ui/button'
import { Confirm } from '@/components/ui/dialog'
import { Badge, Empty, Section, Skeleton } from '@/components/ui/misc'
import type { HubTabProps } from './search'

export function KitTab(_: HubTabProps) {
  const kit = useQuery(q.kit())
  const checkouts = useQuery(q.kitCheckouts())
  const [confirming, setConfirming] = useState(false)
  const rollout = useRolloutKit()
  const read = useReadKit()
  if (kit.isPending)
    return (
      <div className="space-y-4">
        <Skeleton className="h-16" />
        <div className="grid gap-4 sm:grid-cols-2">
          <Skeleton className="h-40" />
          <Skeleton className="h-40" />
        </div>
      </div>
    )
  if (kit.error || !kit.data)
    return <p className="text-[13px] text-danger">Couldn’t load the kit.</p>
  const k = kit.data
  if (!k.configured)
    return (
      <div className="rounded-xl border border-dashed border-line">
        <Empty icon={<Package />} title="No agent kit yet.">
          {k.can_manage ? (
            <>
              Pick its repo in{' '}
              <Link to="/admin" search={{ tab: 'forges' }} className="text-accent hover:underline">
                Admin → Forges
              </Link>
              .
            </>
          ) : (
            'An admin picks its repo.'
          )}
        </Empty>
      </div>
    )
  const plugins = k.kit?.plugins ?? []
  const notes = k.kit?.notes ?? []
  return (
    <div className="space-y-4">
      <Section
        title="Agent kit"
        helpId="agents:kit"
        help={
          <p>
            Shared Claude Code plugins (skills, agents, commands, hooks, MCP servers, tools) from a
            marketplace repo. <code>kit.toml</code> says which repos get each plugin and which notes
            sessions start with. <code>wp kit sync</code> makes a checkout match (it runs at setup,
            upgrade and session start); <code>wp kit add</code> shares a skill through a pull
            request.
          </p>
        }
        actions={
          k.can_manage && (
            <div className="flex items-center gap-1">
              <IconButton
                label="Read the kit again now"
                onClick={() => read.mutate()}
                disabled={read.isPending}
              >
                <RefreshCw className={read.isPending ? 'size-3.5 animate-spin' : 'size-3.5'} />
              </IconButton>
              <Button
                size="sm"
                variant="primary"
                icon={<Rocket className="size-3.5" />}
                loading={rollout.isPending}
                onClick={() => setConfirming(true)}
                disabled={!k.kit}
              >
                Roll out now
              </Button>
            </div>
          )
        }
      >
        <KitSummary k={k} />
        {rollout.data && <RolloutStatus rows={rollout.data} />}
      </Section>

      {plugins.length > 0 && (
        <div className="grid gap-4 sm:grid-cols-2" aria-label="Plugins">
          {plugins.map((p) => (
            <PluginCard key={p.name} p={p} reach={k.reach[p.name] ?? []} />
          ))}
        </div>
      )}

      {notes.length > 0 && (
        <Section title="Notes">
          <ul>
            {notes.map((n, i) => (
              <li key={i} className="border-b border-line px-4 py-2.5 text-[13px] last:border-b-0">
                <div>{n.text}</div>
                <div className="mt-0.5 text-xs text-subtle">{targetText(n)}</div>
              </li>
            ))}
          </ul>
        </Section>
      )}

      <Section title="Checkouts">
        {checkouts.isPending ? (
          <div className="p-4">
            <Skeleton className="h-10" />
          </div>
        ) : !checkouts.data?.length ? (
          <p className="px-4 py-3 text-[13px] text-subtle">
            No checkout has reported yet: run <code>wp kit sync</code> in one.
          </p>
        ) : (
          <Checkouts rows={checkouts.data} current={k.commit ?? ''} />
        )}
      </Section>

      <Confirm
        open={confirming}
        onClose={() => setConfirming(false)}
        title="Roll the kit out now?"
        body="Each of your online runners runs wp kit sync --all over the checkouts in its folders. Plugins load in the next session there."
        confirmLabel="Roll out"
        danger={false}
        onConfirm={() => rollout.mutate()}
      />
    </div>
  )
}

/** Where the kit comes from, its commit, the last read and what went wrong. */
function KitSummary({ k }: { k: KitInfo }) {
  const url = safeUrl(k.repo_url ?? '')
  return (
    <div className="space-y-2 px-4 py-3 text-[13px]">
      <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
        {url ? (
          <a href={url} target="_blank" rel="noreferrer" className="font-medium hover:underline">
            {k.repo}
          </a>
        ) : (
          <span className="font-medium">{k.repo}</span>
        )}
        {k.commit && (
          <span className="font-mono text-xs text-muted" title={k.commit}>
            {k.branch ? `${k.branch}@` : ''}
            {shortSHA(k.commit)}
          </span>
        )}
        {k.read_at && (
          <span className="text-xs text-subtle" title={dateTime(k.read_at)}>
            read {relativeTime(k.read_at)}
          </span>
        )}
      </div>
      {!k.kit && k.errors.length === 0 && (
        <p className="text-xs text-subtle">Not read yet; it is read on a push to it and hourly.</p>
      )}
      {k.errors.length > 0 && (
        <div className="rounded-lg border border-danger/40 bg-danger/5 px-3 py-2" role="alert">
          <div className="flex items-center gap-1.5 font-medium text-danger">
            <CircleAlert className="size-3.5" aria-hidden />
            {k.read_sha ? `${shortSHA(k.read_sha)} has problems` : 'Couldn’t read the kit'}
            {k.kit && k.commit ? `; still using ${shortSHA(k.commit)}` : ''}
          </div>
          <ul className="mt-1 list-disc space-y-0.5 pl-5 text-xs">
            {k.errors.map((e, i) => (
              <li key={i}>{e}</li>
            ))}
          </ul>
        </div>
      )}
    </div>
  )
}

function PluginCard({ p, reach }: { p: KitPlugin; reach: string[] }) {
  const parts = KIT_PARTS.filter((x) => p[x.key].length > 0)
  return (
    <section
      className="flex min-w-0 flex-col rounded-xl border border-line bg-panel"
      aria-label={`Plugin ${p.name}`}
    >
      <div className="flex items-baseline gap-2 px-4 pt-3">
        <h3 className="truncate text-[13px] font-semibold">{p.name}</h3>
        {p.version && <span className="text-xs text-subtle">{p.version}</span>}
      </div>
      {p.description && <p className="px-4 pt-0.5 text-[13px] text-muted">{p.description}</p>}
      {parts.length > 0 && (
        <dl className="space-y-1 px-4 pt-2 text-xs">
          {parts.map((x) => (
            <div key={x.key} className="flex min-w-0 gap-2">
              <dt className="shrink-0 text-subtle">{plural(p[x.key].length, x.one, x.many)}</dt>
              <dd className="min-w-0 truncate">
                {p[x.key].map((c, i) => (
                  <span key={c.name} title={c.description || undefined}>
                    {i > 0 && ', '}
                    {c.name}
                  </span>
                ))}
              </dd>
            </div>
          ))}
        </dl>
      )}
      <div className="mt-auto px-4 pb-3 pt-2 text-xs text-subtle">
        {!p.targeted ? (
          'Not sent anywhere: kit.toml doesn’t name it.'
        ) : (
          <>
            <span>{reachText(reach)}</span>
            {(p.target.when?.length || p.target.machines?.length) && (
              <span> · {conditions(p.target)}</span>
            )}
          </>
        )}
      </div>
    </section>
  )
}

function reachText(reach: string[]) {
  if (reach.length === 0) return 'No attached repo matches.'
  const shown = reach.slice(0, 3).join(', ')
  return reach.length > 3 ? `${shown} and ${reach.length - 3} more` : shown
}

function conditions(t: KitTarget) {
  const out: string[] = []
  for (const w of t.when ?? []) out.push(`with ${w.replace(/^exists:/, '')}`)
  if (t.machines?.length) out.push(`on ${t.machines.join(', ')}`)
  return out.join(', ')
}

function targetText(t: KitTarget) {
  const repos = t.repos?.length ? t.repos.join(', ') : 'every repo'
  const c = conditions(t)
  return c ? `${repos} · ${c}` : repos
}

/** Each checkout's last report: behind, drifted or failed ones first. */
function Checkouts({ rows, current }: { rows: KitCheckout[]; current: string }) {
  const many = new Set(rows.map((r) => r.user.id)).size > 1
  return (
    <ul>
      {rows.map((c) => {
        const state = c.error ? 'failed' : c.behind ? 'behind' : null
        return (
          <li
            key={`${c.user.id}:${c.machine}:${c.path}`}
            className="flex flex-col gap-1 border-b border-line px-4 py-2.5 text-[13px] last:border-b-0 sm:flex-row sm:items-start sm:gap-3"
          >
            <div className="min-w-0 flex-1">
              <div className="flex flex-wrap items-baseline gap-x-2">
                <span className="font-medium">{c.machine}</span>
                <span className="min-w-0 truncate font-mono text-xs text-muted" title={c.path}>
                  {c.path}
                </span>
              </div>
              <div className="text-xs text-subtle">
                {c.repo || 'unknown repo'}
                {many && ` · ${c.user.display_name}`}
                {c.plugins.length > 0 && ` · ${c.plugins.map((p) => p.split('@')[0]).join(', ')}`}
              </div>
              {c.drift.map((d, i) => (
                <div key={i} className="text-xs text-warn">
                  Drift: {d.plugin && <b className="font-medium">{d.plugin} </b>}
                  {d.what}
                </div>
              ))}
              {c.error && <div className="text-xs text-danger">{c.error}</div>}
            </div>
            <div className="flex shrink-0 items-center gap-2 text-xs">
              <span className="font-mono text-muted" title={c.kit_commit || undefined}>
                {shortSHA(c.kit_commit) || '—'}
              </span>
              {state === 'behind' && (
                <Badge color="var(--wp-warn)">behind {current ? shortSHA(current) : ''}</Badge>
              )}
              {state === 'failed' && <Badge color="var(--wp-danger)">failed</Badge>}
              <time className="text-subtle" title={dateTime(c.reported_at)}>
                {relativeTime(c.reported_at)}
              </time>
            </div>
          </li>
        )
      })}
    </ul>
  )
}

/** What Roll out queued, one line per runner, following each job. */
function RolloutStatus({ rows }: { rows: KitRollout[] }) {
  return (
    <ul className="border-t border-line" aria-label="Roll out">
      {rows.map((r) => (
        <li
          key={r.runner_id}
          className="flex items-center gap-2 border-b border-line px-4 py-2 text-[13px] last:border-b-0"
        >
          <span className="min-w-0 flex-1 truncate font-medium">{r.runner}</span>
          {r.job_id ? (
            <JobState id={r.job_id} />
          ) : (
            <span className="truncate text-xs text-subtle">skipped: {r.skipped}</span>
          )}
        </li>
      ))}
    </ul>
  )
}

function JobState({ id }: { id: number }) {
  const job = useQuery(runnerJobQuery(id)).data
  const s = job ? JOB_STATES[job.state] : JOB_STATES.queued
  return (
    <Link
      to="/runner-jobs/$id"
      params={{ id: String(id) }}
      className="text-xs hover:underline"
      style={s.color ? { color: s.color } : undefined}
    >
      {s.label}
    </Link>
  )
}
