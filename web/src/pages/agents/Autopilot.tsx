import { useEffect, useMemo, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import clsx from 'clsx'
import {
  AlertTriangle,
  CheckCircle2,
  ChevronDown,
  CirclePause,
  CircleSlash,
  FastForward,
  Hand,
  Hourglass,
  Loader,
  Moon,
  Plane,
  Play,
  Power,
  Settings,
  SlidersHorizontal,
  Square,
  Timer,
  type LucideIcon,
} from 'lucide-react'
import {
  autopilotQuery,
  useAutopilotStep,
  useSetAutopilot,
  useTurnOffAutopilot,
  useStartAutopilotSession,
} from '@/lib/queries'
import {
  openSessionBrief,
  stepBrief,
  stepLive,
  type AutopilotOpenSession,
  type AutopilotOverview,
  type AutopilotProject,
  type AutopilotStep,
  type RowBrief,
  type RowTone,
} from '@/lib/autopilot'
import { relativeTime, plural } from '@/lib/format'
import { AppLink } from '@/components/AppLink'
import { IconButton } from '@/components/ui/button'
import { Explainer } from '@/components/ui/help'
import { Switch } from '@/components/ui/input'
import { Badge, Empty, Section, Skeleton } from '@/components/ui/misc'
import { toastError } from '@/components/ui/toast'
import { Activity } from '@/components/autopilot/Activity'
import { Readings } from '@/components/autopilot/Readings'
import { SettingsDialog, SpendToggle } from '@/components/autopilot/SettingsDialog'
import { Summaries } from '@/components/autopilot/Summaries'
import type { HubTabProps } from './search'

/** The clock, ticking each second while something counts down on screen. */
export function useNow(active: boolean) {
  const [now, setNow] = useState(() => Date.now())
  useEffect(() => {
    if (!active) return
    setNow(Date.now())
    const t = window.setInterval(() => setNow(Date.now()), 1000)
    return () => window.clearInterval(t)
  }, [active])
  return now
}

export function AutopilotTab({ filters }: HubTabProps) {
  const { data, isLoading, error } = useQuery({ ...autopilotQuery(), refetchInterval: 20_000 })
  const project = filters.project
  const [settings, setSettings] = useState<string | null>(null)
  const projects = useMemo(
    () =>
      (data?.projects ?? [])
        .filter((p) => !project || p.project_key === project)
        .sort(
          (a, b) =>
            Number(b.enabled) - Number(a.enabled) ||
            Number(b.steps.some(stepLive)) - Number(a.steps.some(stepLive)) ||
            a.project_key.localeCompare(b.project_key),
        ),
    [data?.projects, project],
  )
  const ticking = projects.some((p) => p.steps.some((s) => s.state === 'due'))
  const now = useNow(ticking)
  if (isLoading || !data)
    return error ? (
      <Empty title="Couldn't load autopilot">{String(error)}</Empty>
    ) : (
      <div className="space-y-3">
        <Skeleton className="h-24" />
        <Skeleton className="h-64" />
      </div>
    )
  return (
    <div className="space-y-3">
      <div className="flex flex-wrap items-center justify-end gap-2">
        <SpendToggle data={data} />
        <IconButton
          label="Autopilot settings"
          variant="secondary"
          size="md"
          icon={<Settings className="size-4" />}
          onClick={() => setSettings('')}
        />
      </div>
      <Explainer id="autopilot" label="How autopilot works">
        <p>
          <b>Autopilot</b> works through the queue in sessions of its own. In a project you turned
          it on for, your runner starts Claude Code sessions in <b>tmux</b> on a machine where it is
          online, each on the next task; when one has finished its task, Waypoint waits a minute
          (the grace), then restarts it on the next. Your own sessions are left alone: one you
          started, or typed in, is never cleared or restarted, though the review its ship asked for
          still runs in it. <b>Start next task</b> hands one of yours to autopilot. Typing{' '}
          <b>keep</b> or <b>/keep</b> in an autopilot session during the grace keeps it, with no
          turn.
        </p>
        <p className="mt-1.5">
          <b>Limits</b> pause it, and it resumes by itself: the last hour’s spend at list price, the
          week’s quota running ahead of an even pace or past a ceiling, and the five-hour window
          past its ceiling. While a quota limit pauses it you can turn the quota limits off for a
          while; the hourly one still holds. <b>Merge wait</b> decides whether the next task waits
          for the session’s unmerged pull request: only a task that builds on it (auto), always, or
          never. Every finished task leaves a <b>summary</b> to read below.
        </p>
        <p className="mt-1.5">
          <b>Use what’s left</b> spends the week’s quota before it resets: over the week’s last
          hours it starts more sessions of its own, past each project’s sessions at once, while the
          week is on pace to end below 100%.
        </p>
      </Explainer>

      <Readings meter={data.meter} limits={data.effective} />
      <Activity />

      <div className="grid gap-4 xl:grid-cols-[minmax(0,1fr)_30rem] 2xl:grid-cols-[minmax(0,1fr)_36rem] xl:items-start">
        <Summaries project={project} unreviewed={data.unreviewed} className="order-2 xl:order-1" />
        <ProjectsSection
          projects={projects}
          onAnywhere={(data.projects ?? []).filter((p) => p.enabled).length}
          now={now}
          onSettings={setSettings}
          className="order-1 xl:order-2"
        />
      </div>
      <SettingsDialog
        open={settings !== null}
        onClose={() => setSettings(null)}
        data={data}
        scope={settings ?? ''}
        onScope={setSettings}
      />
    </div>
  )
}

function ProjectsSection({
  projects,
  onAnywhere,
  now,
  onSettings,
  className,
}: {
  projects: AutopilotProject[]
  /** projects with autopilot on, counting those a project filter hides */
  onAnywhere: number
  now: number
  onSettings: (scope: string) => void
  className?: string
}) {
  const on = projects.filter((p) => p.enabled).length
  const off = useTurnOffAutopilot()
  return (
    <Section
      title={
        <span className="inline-flex items-center gap-2">
          <span>Projects</span>
          <Badge
            color={on > 0 ? 'var(--wp-accent)' : undefined}
            title={`Autopilot is on in ${plural(on, 'project')}`}
          >
            {on}
          </Badge>
        </span>
      }
      className={className}
      actions={
        onAnywhere > 0 && (
          <IconButton
            size="xs"
            label={`Turn autopilot off in every project: on in ${plural(onAnywhere, 'project')} (sessions already started carry on)`}
            icon={<Power className="size-3.5" />}
            loading={off.isPending}
            onClick={() => off.mutate(undefined, { onError: toastError })}
          />
        )
      }
    >
      {projects.length === 0 ? (
        <Empty icon={<Plane />} title="No projects to run autopilot in">
          Autopilot needs a project you can edit.
        </Empty>
      ) : (
        <ul className="divide-y divide-line">
          {projects.map((p) => (
            <ProjectRow key={p.project_id} p={p} now={now} onSettings={onSettings} />
          ))}
        </ul>
      )}
    </Section>
  )
}

const TONE: Record<RowTone, { icon: LucideIcon; cls: string; spin?: boolean }> = {
  live: { icon: Timer, cls: 'text-accent' },
  wait: { icon: Hourglass, cls: 'text-muted' },
  pause: { icon: CirclePause, cls: 'text-warn' },
  run: { icon: Loader, cls: 'text-accent', spin: true },
  done: { icon: CheckCircle2, cls: 'text-success' },
  stop: { icon: CircleSlash, cls: 'text-subtle' },
  fail: { icon: AlertTriangle, cls: 'text-danger' },
  idle: { icon: Moon, cls: 'text-subtle' },
  you: { icon: Hand, cls: 'text-warn' },
}

function ToneIcon({ tone }: { tone: RowTone }) {
  const t = TONE[tone]
  return (
    <t.icon
      aria-hidden
      className={clsx('mt-0.5 size-3.5 shrink-0', t.cls, t.spin && 'animate-spin')}
    />
  )
}

/** The project's own page and the Agents list share this row; the gear opens the settings dialog. */
export function ProjectRow({
  p,
  now,
  onSettings,
}: {
  p: AutopilotProject
  now: number
  onSettings: (scope: string) => void
}) {
  const set = useSetAutopilot()
  const [recent, setRecent] = useState(false)
  const live = p.steps.filter((s) => stepLive(s) || s.state === 'starting')
  const ended = p.steps.filter((s) => !stepLive(s) && s.state !== 'starting')
  const sessions = p.sessions ?? []
  const own = Object.keys(p.limits).length
  return (
    <li className="px-3.5 py-2.5" data-testid={`autopilot-${p.project_key}`}>
      <div className="flex items-center gap-2">
        <div className="flex min-w-0 flex-1 items-baseline gap-1.5">
          <span className="font-mono text-xs text-muted">{p.project_key}</span>
          <span className={clsx('truncate text-[13px]', !p.enabled && 'text-muted')}>
            {p.project_name}
          </span>
          {p.enabled && p.criteria && (
            <Badge title={`Only tasks ${p.criteria}`} className="shrink-0">
              criteria
            </Badge>
          )}
          {p.enabled && p.pace && (
            <Badge title={`Pace ${p.pace}`} className="shrink-0">
              pace
            </Badge>
          )}
        </div>
        {p.enabled && (
          <span className="relative">
            <IconButton
              label={`Limits for ${p.project_key}${own ? ` (${plural(own, 'limit')} set here)` : ''}`}
              icon={<SlidersHorizontal className="size-3.5" />}
              onClick={() => onSettings(p.project_key)}
            />
            {own > 0 && (
              <span className="pointer-events-none absolute right-1 top-1 size-1.5 rounded-full bg-accent" />
            )}
          </span>
        )}
        <Switch
          checked={p.enabled}
          disabled={set.isPending}
          label={`Autopilot in ${p.project_key}`}
          onChange={(enabled) =>
            set.mutate(
              { project: p.project_key, enabled, limits: {}, merge: true },
              { onError: toastError },
            )
          }
        />
      </div>
      {p.enabled && (
        <div className="mt-1.5 space-y-1.5">
          {p.criteria && (
            <p className="sr-only" data-testid="autopilot-criteria">
              Only tasks {p.criteria}
              {p.pace ? ` · Pace ${p.pace}` : ''}
            </p>
          )}
          {!p.criteria && p.pace && (
            <p className="sr-only" data-testid="autopilot-criteria">
              Pace {p.pace}
            </p>
          )}
          {live.length === 0 && sessions.length === 0 && (
            <p className="text-xs text-subtle">
              No sessions of yours open here: autopilot starts the next task when one finishes a
              task.
            </p>
          )}
          {live.map((s) => (
            <StepRow key={s.id} s={s} now={now} />
          ))}
          <Sessions list={sessions} />
          {ended.length > 0 && (
            <div>
              <button
                type="button"
                onClick={() => setRecent((v) => !v)}
                aria-expanded={recent}
                className="inline-flex flex-wrap items-center gap-1.5 text-xs text-subtle hover:text-fg"
              >
                <ChevronDown
                  className={clsx('size-3 transition-transform', !recent && '-rotate-90')}
                />
                {ended.length} earlier today
                {tally(ended, now).map(([tone, n]) => (
                  <span
                    key={tone}
                    className={clsx('inline-flex items-center gap-0.5', TONE[tone].cls)}
                  >
                    <ToneIcon tone={tone} />
                    {n}
                  </span>
                ))}
              </button>
              {recent && (
                <ul className="mt-1 space-y-1.5 opacity-80">
                  {ended.map((s) => (
                    <StepRow key={s.id} s={s} now={now} />
                  ))}
                </ul>
              )}
            </div>
          )}
        </div>
      )}
    </li>
  )
}

/** How many of each kind: the repeats a folded list shows in a glance. */
function tally(steps: AutopilotStep[], now: number): [RowTone, number][] {
  const m = new Map<RowTone, number>()
  for (const s of steps) {
    const t = stepBrief(s, now).tone
    m.set(t, (m.get(t) ?? 0) + 1)
  }
  return [...m.entries()]
}

const rowText = 'min-w-0 flex-1 text-[13px] leading-snug'

function StepRow({ s, now }: { s: AutopilotStep; now: number }) {
  const act = useAutopilotStep()
  const live = stepLive(s)
  const b = stepBrief(s, now)
  const sub = [
    s.session ? s.session.title || `Session ${s.session.id}` : 'A session of its own',
    s.session && s.session.machine,
    s.done && `after ${s.done.key}`,
    !live && relativeTime(s.updated_at),
  ].filter(Boolean)
  const can = s.session ? live || s.state === 'starting' : s.state === 'starting'
  return (
    <div
      className="flex items-start gap-2"
      data-testid="autopilot-step"
      data-state={s.state}
      title={b.full}
    >
      <ToneIcon tone={b.tone} />
      <div className={rowText}>
        <p
          className={clsx(
            'truncate',
            b.tone === 'fail' && 'text-danger',
            b.tone === 'pause' && 'text-warn',
          )}
        >
          {s.next && s.state !== 'due' && s.state !== 'started' ? (
            <AppLink to={`/issue/${s.next.key}`} className="hover:underline">
              {b.label}
            </AppLink>
          ) : (
            b.label
          )}
          {s.spend && (
            <Badge color="var(--wp-accent)" title="Started by use what’s left" className="ml-1.5">
              week’s end
            </Badge>
          )}
        </p>
        <p className="truncate text-xs text-subtle">
          {s.session ? (
            <AppLink to={s.session.url} className="hover:text-fg hover:underline">
              {sub[0]}
            </AppLink>
          ) : (
            sub[0]
          )}
          {sub.slice(1).map((x) => ` · ${x}`)}
        </p>
      </div>
      {can && (
        <div className="flex shrink-0 gap-0.5">
          {live && s.session && (
            <IconButton
              size="xs"
              label="Start now (skips the grace and any merge wait; limits still apply)"
              icon={<FastForward className="size-3.5" />}
              loading={act.isPending && act.variables?.action === 'now'}
              disabled={act.isPending}
              onClick={() => act.mutate({ id: s.id, action: 'now' }, { onError: toastError })}
            />
          )}
          <IconButton
            size="xs"
            label="Stop (leave the session as it is)"
            icon={<Square className="size-3.5" />}
            loading={act.isPending && act.variables?.action === 'stop'}
            disabled={act.isPending}
            onClick={() => act.mutate({ id: s.id, action: 'stop' }, { onError: toastError })}
          />
        </div>
      )}
    </div>
  )
}

/** Open sessions: one row each, but sessions in the same state fold into one line. */
function Sessions({ list }: { list: AutopilotOpenSession[] }) {
  const groups = new Map<string, AutopilotOpenSession[]>()
  for (const x of list) {
    const k = x.startable || x.task ? `own-${x.id}` : openSessionBrief(x).label
    groups.set(k, [...(groups.get(k) ?? []), x])
  }
  return (
    <>
      {[...groups.values()].map((g) =>
        g.length === 1 ? (
          <OpenSessionRow key={g[0].id} x={g[0]} />
        ) : (
          <FoldedSessions key={g[0].id} list={g} />
        ),
      )}
    </>
  )
}

function FoldedSessions({ list }: { list: AutopilotOpenSession[] }) {
  const [open, setOpen] = useState(false)
  const b = openSessionBrief(list[0])
  return (
    <div data-testid="autopilot-session-group">
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        aria-expanded={open}
        title={b.full}
        className="flex w-full items-center gap-2 text-left text-[13px] hover:text-fg"
      >
        <ToneIcon tone={b.tone} />
        <span className="min-w-0 flex-1 truncate">{b.label}</span>
        <Badge title={`${plural(list.length, 'session')} of yours`}>×{list.length}</Badge>
        <ChevronDown
          className={clsx('size-3 text-subtle transition-transform', !open && '-rotate-90')}
        />
      </button>
      {open && (
        <ul className="mt-1 space-y-1.5 pl-5">
          {list.map((x) => (
            <OpenSessionRow key={x.id} x={x} />
          ))}
        </ul>
      )}
    </div>
  )
}

function OpenSessionRow({ x }: { x: AutopilotOpenSession }) {
  const start = useStartAutopilotSession()
  const b: RowBrief = openSessionBrief(x)
  return (
    <div
      className="flex items-start gap-2"
      data-testid="autopilot-session"
      data-state={x.state}
      title={b.full}
    >
      <ToneIcon tone={b.tone} />
      <div className={rowText}>
        <p className={clsx('truncate', !x.startable && !x.task && 'text-muted')}>
          {x.task ? (
            <AppLink to={`/issue/${x.task.key}`} className="hover:underline">
              {b.label}
            </AppLink>
          ) : (
            b.label
          )}
          {x.task && <span className="text-muted"> {x.task.title}</span>}
        </p>
        <p className="truncate text-xs text-subtle">
          <AppLink to={x.url} className="hover:text-fg hover:underline">
            {x.title || `Session ${x.id}`}
          </AppLink>
          {` · ${x.machine} · ${relativeTime(x.last_seen_at)}`}
        </p>
      </div>
      {x.startable && (
        <IconButton
          size="xs"
          label="Start next task (autopilot clears this session and starts the next task in it; limits still apply)"
          icon={<Play className="size-3.5" />}
          loading={start.isPending}
          onClick={() => start.mutate(x.id, { onError: toastError })}
        />
      )}
    </div>
  )
}

/** One project's autopilot: the same row Agents → Autopilot lists, for the project's own page. */
export function ProjectAutopilot({ projectKey }: { projectKey: string }) {
  const { data } = useQuery({ ...autopilotQuery(), refetchInterval: 20_000 })
  const p = data?.projects.find((x) => x.project_key === projectKey)
  const now = useNow(!!p?.steps.some((s) => s.state === 'due'))
  const [scope, setScope] = useState<string | null>(null)
  if (!data || !p) return null
  return (
    <div className="overflow-hidden rounded-xl border border-line bg-panel">
      <ul>
        <ProjectRow p={p} now={now} onSettings={setScope} />
      </ul>
      <SettingsDialog
        open={scope !== null}
        onClose={() => setScope(null)}
        data={data as AutopilotOverview}
        scope={scope ?? ''}
        onScope={setScope}
      />
    </div>
  )
}
