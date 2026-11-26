import { useMemo, useState } from 'react'
import { useNavigate } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import { Check, CircleHelp, Copy, Plus, Server, Terminal, Trash2, X } from 'lucide-react'
import clsx from 'clsx'
import { relativeTime } from '@/lib/format'
import { q } from '@/lib/queries'
import {
  CLI_LABELS,
  CLI_SIGN_IN,
  connectCommand,
  platform,
  runnerJobsQuery,
  runnersQuery,
  useRemoveRunner,
  useRunCommand,
  type Runner,
  type RunnerCapacity,
  type RunnerCLI,
  type RunnerJob,
} from '@/lib/runners'
import { Button, IconButton } from '@/components/ui/button'
import { Confirm, Dialog } from '@/components/ui/dialog'
import { Field, Input, Select } from '@/components/ui/input'
import { Badge, Empty, Section, Skeleton } from '@/components/ui/misc'
import { JobLine } from '@/components/Runners'
import { copy, Help } from './common'
import { SandboxMachinesSection } from './SandboxMachines'

/**
 * A person's runners: the machines where `wp runner` does work for them.
 * `all` is the admin's view of everyone's.
 */
export function RunnersTab({ all = false }: { all?: boolean }) {
  const runners = useQuery(runnersQuery(all))
  const me = useQuery(q.me()).data
  const jobs = useQuery(runnerJobsQuery({ limit: 100, all }))
  const [connecting, setConnecting] = useState(false)
  const [removing, setRemoving] = useState<Runner | null>(null)
  const [running, setRunning] = useState<Runner | null>(null)
  const remove = useRemoveRunner()
  const byRunner = useMemo(() => {
    const m = new Map<number, RunnerJob[]>()
    for (const j of jobs.data ?? []) {
      if (j.runner_id == null) continue
      m.set(j.runner_id, [...(m.get(j.runner_id) ?? []), j])
    }
    return m
  }, [jobs.data])
  const waiting = (jobs.data ?? []).filter((j) => j.state === 'queued')
  const jobsCapped = (jobs.data?.length ?? 0) >= 100
  const list = runners.data ?? []
  return (
    <>
      <Section
        title={all ? "Everyone's runners" : 'Runners'}
        helpId={all ? 'admin:runners' : 'settings:runners'}
        help={
          all ? (
            <>
              Every runner on this Waypoint. You see that jobs exist, not what they run. Remove one
              that is gone; the person it belongs to connects a new one from Settings → Runners.
            </>
          ) : (
            <>
              A runner is <b>wp</b> on a computer of yours. Waypoint can then run a command, apply a
              change or start a coding agent there, as you. It connects out to Waypoint, so it works
              behind a firewall.
            </>
          )
        }
        actions={
          !all && (
            <Button
              size="sm"
              variant={list.length === 0 ? 'primary' : 'secondary'}
              icon={<Plus className="size-3.5" />}
              onClick={() => setConnecting((c) => !c)}
            >
              Connect a runner
            </Button>
          )
        }
      >
        {runners.isPending ? (
          <div className="space-y-2 p-4">
            <Skeleton className="h-16" />
            <Skeleton className="h-16" />
          </div>
        ) : runners.isError ? (
          <p className="p-4 text-[13px] text-danger">
            Couldn't load runners: {runners.error.message}
          </p>
        ) : list.length === 0 ? (
          <Empty icon={<Server />} title="No runners yet">
            {all
              ? 'Nobody has connected one.'
              : 'Connect a computer of yours and Waypoint can run work on it.'}
          </Empty>
        ) : (
          <ul className="divide-y divide-line">
            {list.map((r) => (
              <RunnerRow
                key={r.id}
                runner={r}
                jobs={byRunner.get(r.id) ?? []}
                showOwner={all}
                onRun={() => setRunning(r)}
                onRemove={() => setRemoving(r)}
              />
            ))}
          </ul>
        )}
      </Section>
      {!all && (connecting || (runners.isSuccess && list.length === 0)) && <ConnectPanel />}
      {waiting.length > 0 && (
        <Section title={`Waiting for a runner (${waiting.length}${jobsCapped ? '+' : ''})`}>
          <ul className="space-y-0.5 px-4 py-2.5 text-xs">
            {waiting.slice(0, 10).map((j) => (
              <li key={j.id}>
                <JobLine job={j} />
              </li>
            ))}
            {waiting.length > 10 && (
              <li className="px-1.5 pt-1 text-subtle">and {waiting.length - 10} more</li>
            )}
          </ul>
        </Section>
      )}
      <RunCommandDialog runner={running} onClose={() => setRunning(null)} />
      <Confirm
        open={!!removing}
        onClose={() => setRemoving(null)}
        title={`Remove “${removing?.name}”?`}
        body={
          <>
            Jobs waiting for it are cancelled, and the ones it is running fail. The runner stops
            itself, but if it is installed it starts again at login: run{' '}
            <code className="font-mono text-xs">wp runner uninstall</code> on that computer to
            remove it for good.
          </>
        }
        confirmLabel="Remove"
        onConfirm={() => removing && remove.mutate(removing.id)}
      />
      {me && <SandboxMachinesSection all={all} meId={me.id} />}
    </>
  )
}

export function ConnectPanel() {
  return (
    <Section title="Connect a runner">
      <div className="space-y-3 p-4">
        <p className="text-[13px] text-muted">On the computer you want to connect, run:</p>
        <CommandBox command={connectCommand(window.location.origin)} />
        <Help className="!text-xs">
          This installs <b>wp</b>, asks you to approve a sign-in here, adds the Claude Code hooks if
          Claude Code is there, and keeps the runner running from then on, updating itself when
          Waypoint does. It shows what it will do first, with the folders Waypoint may work in: the
          ones where Claude Code has worked on that computer. To choose them, end the command with{' '}
          <code className="font-mono">-s -- --allow ~/projects</code>; with none, only coding agents
          on a fresh checkout can run. Running it again is safe.
        </Help>
        <ul className="list-disc space-y-1.5 pl-5 text-xs leading-relaxed text-muted">
          <li>
            A runner can run shell commands and change files in the folders you allow, and start
            Claude Code, Codex, Cursor or opencode as you, with the sign-ins already on that
            computer. Waypoint never sees those.
          </li>
          <li>
            Only you, or a rule you own, can send work to your runners. Nothing else runs on them.
          </li>
          <li>
            On a Linux server also run{' '}
            <code className="font-mono">sudo loginctl enable-linger $USER</code>, so it keeps
            running when you log out.
          </li>
        </ul>
      </div>
    </Section>
  )
}

/** A command to copy; it wraps rather than scrolling sideways. */
function CommandBox({ command }: { command: string }) {
  return (
    <div className="flex items-start gap-1 rounded-lg border border-line bg-sunken py-1.5 pl-3 pr-1">
      <code className="min-w-0 flex-1 break-words py-0.5 font-mono text-xs leading-relaxed">
        {command}
      </code>
      <IconButton label="Copy" onClick={() => copy(command)} icon={<Copy className="size-3.5" />} />
    </div>
  )
}

function RunnerRow({
  runner: r,
  jobs,
  showOwner,
  onRun,
  onRemove,
}: {
  runner: Runner
  jobs: RunnerJob[]
  showOwner: boolean
  onRun: () => void
  onRemove: () => void
}) {
  const [more, setMore] = useState(false)
  const shown = more ? jobs.slice(0, 10) : jobs.slice(0, 3)
  return (
    <li className="px-4 py-3.5" data-testid="runner">
      <div className="flex items-start gap-3">
        <span
          role="img"
          aria-label={r.online ? 'Online' : 'Offline'}
          className={clsx(
            'mt-[7px] size-2.5 shrink-0 rounded-full',
            r.online
              ? 'bg-ok shadow-[0_0_0_3px_color-mix(in_oklab,var(--wp-ok)_25%,transparent)]'
              : 'bg-subtle/50',
          )}
        />
        <div className="min-w-0 flex-1">
          <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
            <span className="truncate text-sm font-medium">{r.name}</span>
            {r.labels.map((l) => (
              <Badge key={l}>{l}</Badge>
            ))}
          </div>
          <div className="mt-0.5 text-xs text-subtle">
            {r.online
              ? 'Online'
              : r.last_seen_at
                ? `Offline, last seen ${relativeTime(r.last_seen_at)}`
                : 'Never connected'}
            {r.running > 0 && ` · ${r.running} running`}
            {' · '}
            {platform(r)}
            {r.version && ` · wp ${r.version}`}
            {showOwner && ` · ${r.owner.display_name}`}
          </div>
        </div>
        <div className="flex shrink-0 items-center gap-1">
          {!showOwner && r.online && r.allowed_dirs.length > 0 && (
            <Button size="sm" icon={<Terminal className="size-3.5" />} onClick={onRun}>
              <span className="max-sm:sr-only">Run a command</span>
            </Button>
          )}
          <IconButton
            label={`Remove ${r.name}`}
            onClick={onRemove}
            icon={<Trash2 className="size-3.5" />}
          />
        </div>
      </div>
      <dl className="mt-3 grid gap-x-4 gap-y-2.5 pl-[22px] text-xs sm:grid-cols-[8rem_1fr]">
        <dt className="text-subtle">Allowed folders</dt>
        <dd className="min-w-0">
          {r.allowed_dirs.length === 0 ? (
            <span className="text-muted">
              None, so only coding agents on a fresh checkout run here. Start the runner with{' '}
              <code className="font-mono">--allow DIR</code> to allow commands and changes.
            </span>
          ) : (
            <ul className="flex flex-wrap gap-1.5">
              {r.allowed_dirs.map((d) => (
                <li
                  key={d}
                  className="max-w-full truncate rounded-md bg-sunken px-1.5 py-0.5 font-mono text-[11px] text-muted"
                  title={d}
                >
                  {d}
                </li>
              ))}
            </ul>
          )}
        </dd>
        <dt className="text-subtle">Coding agents</dt>
        <dd className="min-w-0">
          {r.clis.length === 0 ? (
            <span className="text-muted">None found on this computer.</span>
          ) : (
            <ul className="flex flex-wrap gap-x-4 gap-y-1">
              {r.clis.map((c) => (
                <li key={c.name}>
                  <CLIState cli={c} />
                </li>
              ))}
            </ul>
          )}
        </dd>
        {r.online && r.capacity && r.capacity.level !== '' && (
          <>
            <dt className="text-subtle">Jobs at once</dt>
            <dd className="min-w-0" data-testid="runner-capacity">
              <Capacity c={r.capacity} />
            </dd>
          </>
        )}
        {jobs.length > 0 && (
          <>
            <dt className="text-subtle">Recent jobs</dt>
            <dd className="min-w-0">
              <ul className="space-y-0.5">
                {shown.map((j) => (
                  <li key={j.id}>
                    <JobLine job={j} />
                  </li>
                ))}
              </ul>
              {jobs.length > 3 && (
                <button
                  type="button"
                  onClick={() => setMore((m) => !m)}
                  className="mt-1 text-xs text-accent hover:underline"
                >
                  {more ? 'Show fewer' : `Show ${Math.min(jobs.length, 10) - 3} more`}
                </button>
              )}
            </dd>
          </>
        )}
      </dl>
    </li>
  )
}

const LEVEL_DOT = {
  calm: 'bg-ok',
  busy: 'bg-warn',
  overloaded: 'bg-danger',
} as const

/** How many jobs the runner takes now and why, and when its machine is usually busy (WP-366). */
function Capacity({ c }: { c: RunnerCapacity }) {
  if (c.level === '') return null
  const now =
    c.level === 'overloaded'
      ? c.max_light
        ? 'One heavy job at a time'
        : 'One job at a time'
      : `Up to ${c.limit} heavy ${c.limit === 1 ? 'job' : 'jobs'} of ${c.max}`
  const busy = hourRanges(c.busy_at ?? [])
  return (
    <div className="space-y-0.5">
      <div className="flex flex-wrap items-center gap-x-1.5">
        <span
          aria-hidden
          className={clsx('inline-block size-2 shrink-0 rounded-full', LEVEL_DOT[c.level])}
        />
        <span>{now}</span>
        <span className="text-muted">
          · {c.level}
          {c.reason && `: ${c.reason}`}
        </span>
      </div>
      {busy && <div className="text-muted">Usually busy {busy}</div>}
    </div>
  )
}

/** Consecutive hour starts as local ranges: "03:00–05:00, 13:00–14:00". */
function hourRanges(hours: string[]): string {
  const t = hours.map((h) => new Date(h).getTime()).sort((a, b) => a - b)
  const fmt = (ms: number) =>
    new Date(ms).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', hourCycle: 'h23' })
  const out: string[] = []
  for (let i = 0; i < t.length;) {
    let j = i
    while (j + 1 < t.length && t[j + 1] - t[j] === 3_600_000) j++
    out.push(`${fmt(t[i])}–${fmt(t[j] + 3_600_000)}`)
    i = j + 1
  }
  return out.join(', ')
}

/** A coding-agent CLI and whether it is signed in. */
function CLIState({ cli: c }: { cli: RunnerCLI }) {
  const name = CLI_LABELS[c.name] ?? c.name
  const state =
    c.signed_in === true ? 'signed in' : c.signed_in === false ? 'signed out' : 'sign-in unknown'
  return (
    <span
      className="inline-flex items-center gap-1 text-muted"
      title={
        c.signed_in === false
          ? `Run ${CLI_SIGN_IN[c.name] ?? 'its login command'} on that computer`
          : c.detail
      }
    >
      {c.signed_in === true ? (
        <Check className="size-3.5 text-ok" />
      ) : c.signed_in === false ? (
        <X className="size-3.5 text-danger" />
      ) : (
        <CircleHelp className="size-3.5 text-subtle" />
      )}
      <span className="text-fg">{name}</span>
      {c.version && <span className="text-subtle">{c.version}</span>}
      <span className={c.signed_in === false ? 'text-danger' : 'text-subtle'}>· {state}</span>
    </span>
  )
}

/** A person's click: run one command on a runner of theirs. */
function RunCommandDialog({ runner, onClose }: { runner: Runner | null; onClose: () => void }) {
  const navigate = useNavigate()
  const run = useRunCommand()
  const [dir, setDir] = useState('')
  const [command, setCommand] = useState('')
  const close = () => {
    onClose()
    setCommand('')
    setDir('')
  }
  const folder = dir || runner?.allowed_dirs[0] || ''
  const go = () => {
    if (!runner || !command.trim() || !folder || run.isPending) return
    run.mutate(
      { runner: runner.id, dir: folder, command: command.trim() },
      {
        onSuccess: (j) => {
          close()
          navigate({ to: '/runner-jobs/$id', params: { id: String(j.id) } })
        },
      },
    )
  }
  return (
    <Dialog
      open={!!runner}
      onClose={close}
      title={`Run a command on ${runner?.name ?? ''}`}
      footer={
        <>
          <Button variant="ghost" onClick={close}>
            Cancel
          </Button>
          <Button variant="primary" disabled={!command.trim()} loading={run.isPending} onClick={go}>
            Run
          </Button>
        </>
      }
    >
      <form
        className="space-y-4 p-4"
        onSubmit={(e) => {
          e.preventDefault()
          go()
        }}
      >
        <Field label="Folder">
          {runner && runner.allowed_dirs.length > 1 ? (
            <Select value={folder} onChange={(e) => setDir(e.target.value)}>
              {runner.allowed_dirs.map((d) => (
                <option key={d} value={d}>
                  {d}
                </option>
              ))}
            </Select>
          ) : (
            <div className="rounded-lg border border-line bg-sunken px-3 py-2 font-mono text-xs">
              {folder}
            </div>
          )}
        </Field>
        <Field
          label="Command"
          note="Runs as you, in that folder, through the shell. You'll see its output live."
        >
          <Input
            autoFocus
            value={command}
            onChange={(e) => setCommand(e.target.value)}
            placeholder="git status"
            className="font-mono"
            spellCheck={false}
            autoComplete="off"
          />
        </Field>
        <button type="submit" className="hidden" />
      </form>
    </Dialog>
  )
}
