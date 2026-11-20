// Runners → Sandbox (WP-418, WP-ADR28): each machine's default sandbox level
// and its kill switch, for the runner's owner and admins, and the log of
// changes. Pausing loosens, so it asks for the password and a reason. The
// helper's machine users can be removed here, and work it left half done
// finished (WP-448).

import { useState } from 'react'
import { Link } from '@tanstack/react-router'
import { useMutation, useQuery } from '@tanstack/react-query'
import clsx from 'clsx'
import { q } from '@/lib/queries'
import { relativeTime } from '@/lib/format'
import {
  levelLoosens,
  requestSandboxOp,
  SANDBOX_LEVELS,
  saveSandboxMachine,
  startSandboxDrill,
  useSandboxRefresh,
  type SandboxLevel,
  type SandboxDrill,
  type SandboxDrillCase,
  type SandboxMachine,
  type SandboxMachineInput,
  type SandboxOp,
  type SandboxUser,
} from '@/lib/sandbox'
import { Button } from '@/components/ui/button'
import { Confirm } from '@/components/ui/dialog'
import { Select, Switch } from '@/components/ui/input'
import { Badge, Section, Skeleton } from '@/components/ui/misc'
import { toast } from '@/components/ui/toast'
import { ReasonDialog, SandboxLog, sandboxError, useReauth } from '@/components/Sandbox'

/** `all`: everyone's machines (admins); else only the person's own. */
export function SandboxMachinesSection({ all, meId }: { all: boolean; meId: number }) {
  const machines = useQuery(q.sandboxMachines())
  const key = useQuery(q.sandboxKey())
  const refresh = useSandboxRefresh()
  const { guard, dialog } = useReauth()
  const [asking, setAsking] = useState<{ m: SandboxMachine; input: SandboxMachineInput } | null>(
    null,
  )
  const save = useMutation({
    mutationFn: ({ id, input }: { id: number; input: SandboxMachineInput }) =>
      guard(() => saveSandboxMachine(id, input)),
    onSuccess: (m, { input }) =>
      toast(
        input.paused === true
          ? `Sandboxing paused on ${m.runner.name}`
          : input.paused === false
            ? `Sandboxing back on for ${m.runner.name}`
            : 'Default level saved',
      ),
    onError: sandboxError,
    onSettled: refresh,
  })
  const list = (machines.data ?? []).filter((m) => all || m.runner.owner.id === meId)
  const change = (m: SandboxMachine, input: SandboxMachineInput) => {
    const loosens =
      input.paused === true ||
      (input.default_level !== undefined && levelLoosens(m.default_level, input.default_level))
    if (loosens) setAsking({ m, input })
    else save.mutate({ id: m.runner.id, input })
  }
  const ids = new Set(list.filter((m) => m.can_edit).map((m) => m.runner.id))
  if (!machines.isPending && list.length === 0) return null
  return (
    <>
      <Section
        className="mt-4"
        title="Sandbox"
        helpId={all ? 'admin:sandbox' : 'settings:sandbox'}
        help={
          <>
            Each machine’s default sandbox level, for projects that don’t set their own, and its
            kill switch: pause sandboxing there when it gets in the way of urgent work. Jobs keep
            their own tokens either way. Pausing or loosening asks for your password and a reason,
            and admins are told. Waypoint doesn’t enforce levels yet. The sandbox helper, installed
            on a machine with <code className="font-mono">wp sandbox install</code>, makes a machine
            user for each repository its jobs work on; running the jobs as those users comes next.
            Where the helper is ready, an escape drill runs weekly and after each helper update:
            Contained sessions try to get out, and the owner and admins are told if one does.
            {key.data && (
              <>
                {' '}
                Helpers pin this server’s key,{' '}
                <code className="break-all font-mono">{key.data.fingerprint}</code>.
              </>
            )}
          </>
        }
      >
        {machines.isPending ? (
          <div className="p-4">
            <Skeleton className="h-16" />
          </div>
        ) : (
          <ul className="divide-y divide-line">
            {list.map((m) => (
              <li
                key={m.runner.id}
                className="flex flex-wrap items-center gap-x-4 gap-y-2 px-4 py-3"
              >
                <div className="min-w-0 flex-1 basis-40">
                  <div className="flex flex-wrap items-center gap-1.5 text-[13px] font-medium">
                    <span className="truncate">{m.runner.name}</span>
                    {m.paused && <Badge className="bg-danger/15 text-danger">paused</Badge>}
                  </div>
                  <div className="truncate text-xs text-subtle">
                    {all && `@${m.runner.owner.username} · `}
                    {m.runner.hostname}
                  </div>
                </div>
                <div className="w-44 shrink-0">
                  <Select
                    aria-label={`Default level on ${m.runner.name}`}
                    value={m.default_level}
                    disabled={!m.can_edit || save.isPending}
                    onChange={(e) => change(m, { default_level: e.target.value as SandboxLevel })}
                  >
                    <option value="">Default: not set</option>
                    {SANDBOX_LEVELS.map((l) => (
                      <option key={l.value} value={l.value}>
                        Default: {l.label}
                      </option>
                    ))}
                  </Select>
                </div>
                <label
                  className={clsx(
                    'flex shrink-0 items-center gap-2 text-[13px]',
                    m.paused ? 'text-danger' : 'text-muted',
                  )}
                >
                  <Switch
                    checked={m.paused}
                    disabled={!m.can_edit || save.isPending}
                    label={`Pause sandboxing on ${m.runner.name}`}
                    onChange={(paused) => change(m, { paused })}
                  />
                  Pause sandboxing
                </label>
                <HelperLine m={m} />
                <DrillLine m={m} />
              </li>
            ))}
          </ul>
        )}
      </Section>
      <Section className="mt-4" title="Sandbox changes">
        <SandboxLog
          filter={{}}
          showTarget
          canEdit={(c) => (c.scope === 'machine' ? ids.has(c.target_id) : all)}
        />
      </Section>
      <ReasonDialog
        open={!!asking}
        title={
          asking?.input.paused
            ? `Pause sandboxing on ${asking.m.runner.name}?`
            : `Loosen ${asking?.m.runner.name ?? ''}’s default?`
        }
        confirmLabel={asking?.input.paused ? 'Pause sandboxing' : 'Loosen'}
        needReason
        onClose={() => setAsking(null)}
        onConfirm={(reason) => {
          const a = asking!
          setAsking(null)
          save.mutate({ id: a.m.runner.id, input: { ...a.input, reason } })
        }}
      >
        {asking?.input.paused
          ? 'Agents on this machine run unsandboxed, as the runner’s person, until someone turns this back off.'
          : (SANDBOX_LEVELS.find((l) => l.value === asking?.input.default_level)?.note ??
            'Projects that don’t set a level get Waypoint’s default: VM where the machine has KVM, else Contained.')}
      </ReasonDialog>
      {dialog}
    </>
  )
}

/** How the machine's sandbox helper is, the machine users it made, and
 *  the actions people asked of it. */
function HelperLine({ m }: { m: SandboxMachine }) {
  const refresh = useSandboxRefresh()
  const [removing, setRemoving] = useState<SandboxUser | null>(null)
  const ask = useMutation({
    mutationFn: (input: { op: SandboxOp['op']; user?: string }) =>
      requestSandboxOp(m.runner.id, input),
    onSuccess: (o) =>
      toast(
        o.op === 'remove_user'
          ? `Asked ${m.runner.name} to remove ${o.user}`
          : `Asked ${m.runner.name} to finish its interrupted work`,
      ),
    onError: sandboxError,
    onSettled: refresh,
  })
  const h = m.helper
  if (!h || !h.installed)
    return (
      <div className="basis-full text-xs text-subtle">
        No sandbox helper: <code className="font-mono">wp sandbox install</code> on this machine
        adds one.
      </div>
    )
  if (!h.ready)
    return (
      <div className="basis-full text-xs text-warn">
        Sandbox helper: {h.problem}. <code className="font-mono">wp sandbox doctor</code> there says
        more.
      </div>
    )
  const users = h.status?.users ?? []
  const drift = h.status?.drift ?? []
  const pending = h.status?.pending ?? 0
  const removed = h.status?.removed ?? []
  const ops = m.ops ?? []
  const open = (o: SandboxOp) => o.state === 'waiting' || o.state === 'sent'
  const busy = (op: SandboxOp['op'], user?: string) =>
    ops.some((o) => open(o) && o.op === op && (user === undefined || o.user === user))
  const unlinked = users.filter((u) => m.users?.[u.name]?.linked === false).length
  return (
    <details className="group basis-full text-xs">
      <summary className="cursor-pointer text-muted marker:text-subtle">
        <span className="text-ok">Sandbox helper ready</span>
        {h.status && ` · ${h.status.version}`} ·{' '}
        {users.length === 1 ? '1 machine user' : `${users.length} machine users`}
        {unlinked > 0 && <span className="text-subtle"> · {unlinked} no project uses</span>}
        {(drift.length > 0 || pending > 0) && (
          <span className="text-warn"> · {drift.length + pending} to check</span>
        )}
        {ops.some(open) && <span className="text-accent"> · working</span>}
      </summary>
      <div className="mt-1.5 space-y-1 pl-3">
        {users.length === 0 && (
          <div className="text-subtle">
            None yet: one is made the first time a job works on a repository.
          </div>
        )}
        {users.map((u) => {
          const info = m.users?.[u.name]
          return (
            <div key={u.name} className="flex flex-wrap items-center gap-x-2 gap-y-0.5">
              <code className="font-mono text-fg">{u.name}</code>
              <span className="min-w-0 text-subtle">
                {u.kind === 'org' ? 'org' : 'repo'} {u.slug} · uid {u.uid}
                {u.last_used
                  ? ` · used ${relativeTime(new Date(u.last_used * 1000).toISOString())}`
                  : ''}
                {info?.last_job && (
                  <>
                    {' · '}
                    <Link
                      to="/runner-jobs/$id"
                      params={{ id: String(info.last_job.id) }}
                      className="text-muted underline decoration-line underline-offset-2 hover:text-fg"
                      title={info.last_job.title}
                    >
                      last job #{info.last_job.id}
                    </Link>
                  </>
                )}
                {info && !info.linked && <> · no project here uses it now</>}
                {u.present === false && (
                  <span className="text-warn"> · missing on the machine</span>
                )}
              </span>
              {m.can_edit &&
                (busy('remove_user', u.name) ? (
                  <span className="text-accent">removing…</span>
                ) : (
                  <Button
                    size="xs"
                    variant="ghost"
                    aria-label={`Remove ${u.name}`}
                    disabled={ask.isPending}
                    onClick={() => setRemoving(u)}
                  >
                    Remove
                  </Button>
                ))}
            </div>
          )
        })}
        {drift.map((d) => (
          <div key={d} className="text-warn">
            {d}
          </div>
        ))}
        {pending > 0 && (
          <div className="text-warn">
            {pending === 1 ? 'A removal was' : `${pending} removals were`} interrupted and
            {pending === 1 ? ' is' : ' are'} still to finish.
          </div>
        )}
        {m.can_edit && (drift.length > 0 || pending > 0) && (
          <div>
            <Button
              size="xs"
              variant="secondary"
              loading={busy('reconcile')}
              disabled={ask.isPending || busy('reconcile')}
              onClick={() => ask.mutate({ op: 'reconcile' })}
            >
              Finish interrupted work
            </Button>
          </div>
        )}
        {removed.length > 0 && (
          <div className="pt-1">
            <div className="text-muted">Removed</div>
            {removed.map((r) => (
              <div key={`${r.name}-${r.at}`} className="break-words text-subtle">
                <code className="font-mono">{r.name}</code> ({r.slug}) ·{' '}
                {relativeTime(new Date(r.at * 1000).toISOString())}
                {r.archive && (
                  <>
                    {' · its files wait in '}
                    <code className="break-all font-mono">{r.archive}</code>
                  </>
                )}
              </div>
            ))}
          </div>
        )}
        {ops.length > 0 && (
          <div className="pt-1">
            <div className="text-muted">Asked of the helper</div>
            {ops.map((o) => (
              <OpLine key={o.id} o={o} />
            ))}
          </div>
        )}
        {h.status && (
          <div className="text-subtle">
            Store {h.status.store.path}
            {h.status.store.fs ? ` (${h.status.store.fs})` : ''} ·{' '}
            {h.status.kvm ? 'KVM available' : 'no KVM'} · reported {relativeTime(h.at)}
          </div>
        )}
      </div>
      <Confirm
        open={!!removing}
        onClose={() => setRemoving(null)}
        onConfirm={() => removing && ask.mutate({ op: 'remove_user', user: removing.name })}
        title={`Remove ${removing?.name ?? ''} from ${m.runner.name}?`}
        confirmLabel="Remove"
        body={
          <>
            The helper deletes the account and moves its home, work and cache to the store’s trash,
            where they wait for someone to delete them. Its uid is never handed out again.
            {removing && m.users?.[removing.name]?.linked !== false && (
              <>
                {' '}
                A project here still works on {removing.slug}, so its next job there makes a new
                machine user.
              </>
            )}
          </>
        }
      />
    </details>
  )
}

/** The machine's latest escape drill (WP-473): what the Contained
 *  sessions tried, what got through, and a button to run it now. */
function DrillLine({ m }: { m: SandboxMachine }) {
  const refresh = useSandboxRefresh()
  const run = useMutation({
    mutationFn: () => startSandboxDrill(m.runner.id),
    onSuccess: () => toast(`Escape drill queued on ${m.runner.name}`),
    onError: sandboxError,
    onSettled: refresh,
  })
  const d = m.drill
  const blocked = m.drill_blocked
  // Before the first drill, say nothing until the helper is ready: the
  // helper line already says what's missing.
  if (!d && !m.helper?.ready) return null
  const busy = d?.state === 'queued' || d?.state === 'running'
  const button = m.can_edit && !blocked && (
    <Button
      size="xs"
      variant="secondary"
      loading={run.isPending}
      disabled={run.isPending || busy}
      onClick={() => run.mutate()}
    >
      Run drill
    </Button>
  )
  if (!d)
    return (
      <div className="flex basis-full flex-wrap items-center gap-2 text-xs text-subtle">
        {blocked
          ? `Escape drill: none yet. It can't run: ${blocked}.`
          : 'Escape drill: none yet. It runs on its own within the hour.'}
        {button}
      </div>
    )
  const cases = d.cases ?? []
  const wrong = cases.filter((c) => c.got !== c.want)
  return (
    <details className="group basis-full text-xs">
      <summary className="cursor-pointer text-muted marker:text-subtle">
        <DrillSummary d={d} />
      </summary>
      <div className="mt-1.5 space-y-1 pl-3">
        <div className="flex flex-wrap items-center gap-2 text-subtle">
          <span>
            {DRILL_REASON[d.reason] ?? d.reason}
            {d.started_by && ` by @${d.started_by.username}`} · {relativeTime(d.created_at)}
            {d.repo_user && (
              <>
                {' · as '}
                <code className="font-mono">{d.repo_user}</code>
              </>
            )}
            {d.helper_version && ` · helper ${d.helper_version}`}
            {d.job_id > 0 && (
              <>
                {' · '}
                <Link
                  to="/runner-jobs/$id"
                  params={{ id: String(d.job_id) }}
                  className="text-muted underline decoration-line underline-offset-2 hover:text-fg"
                >
                  job #{d.job_id}
                </Link>
              </>
            )}
          </span>
          {button}
        </div>
        {d.error && <div className="text-warn">{d.error}</div>}
        {blocked && <div className="text-subtle">The next drill can't run: {blocked}.</div>}
        {wrong.map((c) => (
          <DrillCaseLine key={`${c.group}-${c.name}`} c={c} />
        ))}
        {cases.length > wrong.length && (
          <details>
            <summary className="cursor-pointer text-muted marker:text-subtle">
              {cases.length - wrong.length} as they should be
            </summary>
            <div className="mt-1 space-y-0.5 pl-3">
              {cases
                .filter((c) => c.got === c.want)
                .map((c) => (
                  <DrillCaseLine key={`${c.group}-${c.name}`} c={c} />
                ))}
            </div>
          </details>
        )}
        {(d.not_covered ?? []).length > 0 && (
          <details>
            <summary className="cursor-pointer text-muted marker:text-subtle">
              Not tried yet
            </summary>
            <ul className="mt-1 list-disc space-y-0.5 pl-6 text-subtle">
              {d.not_covered!.map((n) => (
                <li key={n}>{n}</li>
              ))}
            </ul>
          </details>
        )}
      </div>
    </details>
  )
}

const DRILL_REASON: Record<string, string> = {
  first: 'First drill',
  weekly: 'Weekly',
  helper: 'After a helper update',
  retry: 'Again after one that didn’t run',
  person: 'Run',
}

function DrillSummary({ d }: { d: SandboxDrill }) {
  const when = relativeTime(d.finished_at ?? d.created_at)
  switch (d.state) {
    case 'passed':
      return (
        <>
          <span className="text-ok">Escape drill passed</span> · {when} · {d.passed} tries
        </>
      )
    case 'failed':
      return (
        <>
          <span className="text-danger">Escape drill failed</span> · {when}
          {d.escaped > 0 && ` · ${d.escaped} got through`}
          {d.failed_controls > 0 && ` · ${d.failed_controls} controls refused`}
        </>
      )
    case 'error':
      return (
        <>
          <span className="text-warn">Escape drill didn’t run</span> · {when}
        </>
      )
    default:
      return <span className="text-accent">Escape drill {d.state}…</span>
  }
}

function DrillCaseLine({ c }: { c: SandboxDrillCase }) {
  const ok = c.got === c.want
  return (
    <div className="break-words">
      <span className={ok ? 'text-subtle' : c.got === 'inconclusive' ? 'text-warn' : 'text-danger'}>
        {c.name}: {c.got}
        {!ok && `, should be ${c.want}`}
      </span>
      {c.detail && <span className="text-subtle"> · {c.detail}</span>}
    </div>
  )
}

const OP_STATE: Record<SandboxOp['state'], string> = {
  waiting: 'waiting for the runner',
  sent: 'the runner has it',
  done: 'done',
  failed: 'failed',
}

function OpLine({ o }: { o: SandboxOp }) {
  return (
    <div className="break-words text-subtle">
      {o.op === 'remove_user' ? (
        <>
          Remove <code className="font-mono">{o.user}</code>
        </>
      ) : (
        'Finish interrupted work'
      )}
      {o.requested_by && ` · @${o.requested_by.username}`} · {relativeTime(o.requested_at)} ·{' '}
      <span
        className={clsx(
          o.state === 'failed' ? 'text-danger' : o.state === 'done' ? 'text-ok' : 'text-accent',
        )}
      >
        {OP_STATE[o.state]}
        {o.error && `: ${o.error}`}
      </span>
    </div>
  )
}
