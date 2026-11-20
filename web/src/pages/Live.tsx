// What's live (WP-389): can I try it yet? Each deployed environment says
// whether it has everything that merged, and recent work is grouped by where
// it stands: stuck, merged but not deployed, on its way, being worked on,
// live. The server places each item (internal/core/delivery.go).

import { useEffect, useState, type ReactNode } from 'react'
import { Link } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import clsx from 'clsx'
import { GitPullRequest, Loader2, Rocket } from 'lucide-react'
import { deliveryQuery } from '@/lib/queries'
import { STAGES, type DeliveryEnv, type DeliveryItem } from '@/lib/delivery'
import { NAME_FONT, ago, shortSha } from '@/lib/environments'
import { StageIcon, runsText } from '@/components/delivery'
import { plural } from '@/lib/format'
import { useTick } from '@/lib/hooks'
import { Button } from '@/components/ui/button'
import { Empty, PageHeader, Section, Skeleton } from '@/components/ui/misc'

const LIVE_SHOWN = 8

export function LivePage() {
  const query = useQuery(deliveryQuery())
  useTick(30_000)
  const [allLive, setAllLive] = useState(false)
  useEffect(() => {
    document.title = "What's live · Waypoint"
  }, [])
  const d = query.data
  const waiting = d?.items.filter((it) => it.stage === 'merged').length ?? 0
  return (
    <div className="flex min-h-0 flex-1 flex-col overflow-y-auto pb-16">
      <div className="mx-auto w-full max-w-5xl">
        <PageHeader
          icon={<Rocket className="size-5 text-accent" />}
          title="What’s live"
          subtitle={
            <span className="inline-flex items-center gap-1.5">
              {d
                ? waiting
                  ? `${plural(waiting, 'merged change')} not deployed yet`
                  : 'Everything merged is deployed'
                : 'Recent work and where it runs'}
              {query.isFetching && <Loader2 className="size-3 animate-spin" aria-label="Reading" />}
            </span>
          }
        />
        <div className="space-y-4 px-4 pb-6 sm:px-6">
          {query.isPending ? (
            <div className="space-y-3">
              <Skeleton className="h-20" />
              <Skeleton className="h-40" />
            </div>
          ) : !d ? (
            <Empty icon={<Rocket />} title="Couldn’t load what’s live" />
          ) : (
            <>
              <Section
                title="Deployments"
                help="Each production and staging environment, and whether it runs everything merged into its branch in the last week."
              >
                {d.environments.length === 0 ? (
                  <Empty icon={<Rocket />} title="No production or staging environment">
                    Add one in a project’s Environments tab, or have CI record deploys with{' '}
                    <code className="font-mono text-xs">wp env deploy</code>, to see when merged
                    work goes live.
                  </Empty>
                ) : (
                  <ul className="divide-y divide-line">
                    {d.environments.map((e) => (
                      <EnvLine key={e.id} env={e} />
                    ))}
                  </ul>
                )}
              </Section>
              {d.items.length === 0 && (
                <Empty icon={<GitPullRequest />} title="No recent work">
                  Pull requests from the last week and tasks agents are on show up here.
                </Empty>
              )}
              {STAGES.map(({ stage, title, hint }) => {
                const list = d.items.filter((it) => it.stage === stage)
                if (!list.length) return null
                const cut = stage === 'live' && !allLive && list.length > LIVE_SHOWN
                return (
                  <Section
                    key={stage}
                    title={
                      <span className="inline-flex items-center gap-2">
                        <StageIcon stage={stage} decorative />
                        {title}
                        <span className="font-normal text-muted">{list.length}</span>
                      </span>
                    }
                    actions={<span className="hidden text-xs text-muted sm:inline">{hint}</span>}
                  >
                    <ul className="divide-y divide-line">
                      {(cut ? list.slice(0, LIVE_SHOWN) : list).map((it, n) => (
                        <ItemLine key={it.pr?.id ?? `t${it.tasks[0]?.id ?? n}`} it={it} />
                      ))}
                    </ul>
                    {cut && (
                      <div className="border-t border-line px-4 py-2">
                        <Button variant="ghost" size="sm" onClick={() => setAllLive(true)}>
                          Show all {list.length}
                        </Button>
                      </div>
                    )}
                  </Section>
                )
              })}
            </>
          )}
        </div>
      </div>
    </div>
  )
}

function EnvLine({ env: e }: { env: DeliveryEnv }) {
  let state: ReactNode
  if (e.deploying)
    state = (
      <span className="inline-flex items-center gap-1 text-accent">
        <Loader2 className="size-3.5 animate-spin" /> Deploying
      </span>
    )
  else if (!e.commit) state = <span className="text-muted">Nothing deployed yet</span>
  else if (e.missing > 0)
    state = (
      <span className="font-medium text-warn">{plural(e.missing, 'merged change')} not in it</span>
    )
  else if (e.unknown > 0)
    state = <span className="text-muted">Can’t tell ({e.unknown} unknown)</span>
  else state = <span className="font-medium text-ok">Up to date</span>
  return (
    <li className="flex flex-wrap items-center gap-x-3 gap-y-1 px-4 py-2.5 text-[13px]">
      <Link
        to="/p/$key/environments/$name"
        params={{ key: e.project, name: e.name }}
        className={clsx(NAME_FONT, 'min-w-0 font-medium hover:underline')}
      >
        <span className="text-muted">{e.project} · </span>
        {e.name}
      </Link>
      {state}
      <span className="ml-auto text-xs text-muted">
        {e.commit && (
          <>
            runs {e.version || `${e.branch ? e.branch + '@' : ''}${shortSha(e.commit)}`}
            {e.deployed_at && <> · deployed {ago(e.deployed_at)}</>}
          </>
        )}
      </span>
    </li>
  )
}

function ItemLine({ it }: { it: DeliveryItem }) {
  const task = it.tasks[0]
  const title = task?.title ?? it.pr?.title ?? ''
  const when =
    it.stage === 'merged' && it.pr?.merged_at
      ? `merged ${ago(it.pr.merged_at)}`
      : it.stage === 'live'
        ? ''
        : ago(it.at)
  return (
    <li className="flex items-start gap-3 px-4 py-2.5 text-[13px]">
      <StageIcon stage={it.stage} className="mt-0.5" />
      <div className="min-w-0 flex-1">
        <div className="flex flex-wrap items-baseline gap-x-2">
          {task && (
            <Link
              to="/issue/$key"
              params={{ key: task.key }}
              className="shrink-0 font-mono text-xs text-muted hover:underline"
            >
              {task.key}
            </Link>
          )}
          {task ? (
            <Link
              to="/issue/$key"
              params={{ key: task.key }}
              className="min-w-0 font-medium hover:underline"
            >
              {title}
            </Link>
          ) : (
            <span className="min-w-0 font-medium">{title}</span>
          )}
          {it.tasks.length > 1 && (
            <span className="text-xs text-muted">+{it.tasks.length - 1} more</span>
          )}
        </div>
        <div className="mt-0.5 flex flex-wrap items-center gap-x-2 text-xs text-muted">
          {it.pr && (
            <Link
              to="/agents"
              search={{ tab: 'pulls', pr: it.pr.id }}
              className="font-mono hover:underline"
            >
              {it.pr.repo}#{it.pr.number}
            </Link>
          )}
          <span className={clsx(it.stage === 'stuck' && 'text-danger')}>{runsText(it)}</span>
          {it.agent && <span className="text-subtle">· {it.agent}</span>}
        </div>
      </div>
      {when && <span className="shrink-0 text-xs text-subtle">{when}</span>}
    </li>
  )
}
