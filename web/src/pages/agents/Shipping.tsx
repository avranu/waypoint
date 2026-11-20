// A pull request's way to merge (WP-D21): whether it is ready and why not,
// its checks, the review gate, and the bot's findings with their states and
// history; merge now, or merge when ready.

import { useState, type ReactNode } from 'react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import clsx from 'clsx'
import {
  CircleCheck,
  CircleX,
  GitMerge,
  LoaderCircle,
  RefreshCw,
  ShieldAlert,
  ShieldCheck,
  Timer,
  Undo2,
} from 'lucide-react'
import { relativeTime, safeUrl } from '@/lib/format'
import {
  checkPull,
  disarmPull,
  dismissFinding,
  mergePull,
  reopenFinding,
  type FindingEvent,
  type MergeStyle,
  type PRCheck,
  type PRDetail,
  type PRFinding,
  type PRShipping,
} from '@/lib/pulls'
import { Button } from '@/components/ui/button'
import { Confirm } from '@/components/ui/dialog'
import { Select } from '@/components/ui/input'
import { Badge } from '@/components/ui/misc'
import { toast, toastError } from '@/components/ui/toast'
import { FindingMarks, SEVERITY } from '../project-settings/Bots'

const FINDING_STATE: Record<PRFinding['state'], { label: string; color?: string }> = {
  open: { label: 'Open' },
  fixed: { label: 'Fixed', color: 'var(--wp-ok)' },
  withdrawn: { label: 'Withdrawn', color: 'var(--wp-subtle)' },
  dismissed: { label: 'Dismissed', color: 'var(--wp-subtle)' },
  resolved: { label: 'Resolved', color: 'var(--wp-ok)' },
  dropped: { label: 'Dropped', color: 'var(--wp-subtle)' },
}

const STYLE_LABEL: Record<string, string> = {
  '': 'The repo’s default',
  merge: 'Merge commit',
  squash: 'Squash',
  rebase: 'Rebase',
}

/** "Fixed in abc1234 · checked by muse-spark-1.3 · the comparison is now ==" */
function eventLine(e: FindingEvent) {
  const sha = e.sha ? e.sha.slice(0, 7) : ''
  const by = e.by || 'someone'
  let what: string
  switch (e.state) {
    case 'fixed':
      what = `Fixed${sha ? ` in ${sha}` : ''}, checked by ${by}`
      break
    case 'resolved':
      what = `Resolved on the forge by ${by}`
      break
    case 'dismissed':
      what = e.kind === 'forge' ? `Dismissed with 👎 by ${by}` : `Dismissed by ${by}`
      break
    case 'withdrawn':
      what = 'Withdrawn by the bot after a reply'
      break
    default:
      what =
        e.old === 'open' && e.state === 'open'
          ? 'The bot agreed it is minor'
          : `Reopened${e.by ? ` by ${by}` : ''}`
  }
  return e.note ? `${what}: ${e.note}` : what
}

function CheckIcon({ state }: { state: PRCheck['state'] }) {
  if (state === 'success') return <CircleCheck className="size-4 shrink-0 text-ok" />
  if (state === 'failed') return <CircleX className="size-4 shrink-0 text-danger" />
  return <LoaderCircle className="size-4 shrink-0 animate-spin text-subtle" />
}

export function Shipping({ pr }: { pr: PRDetail }) {
  const sh = pr.shipping
  const live = pr.state === 'open' || pr.state === 'draft'
  if (!sh) return null
  const canAct = sh.can_merge
  const hasAnything = sh.checks.length > 0 || sh.gate.enabled || sh.findings.length > 0 || live
  if (!hasAnything) return null
  return (
    <section aria-label="Merging" className="space-y-4">
      <div className="flex items-center gap-2">
        <h3 className="text-[13px] font-semibold">{live ? 'Merging' : 'Checks and findings'}</h3>
        {live && canAct && <CheckNow id={pr.id} />}
      </div>
      {live && <Readiness pr={pr} sh={sh} />}
      <Checks sh={sh} />
      <Gate sh={sh} />
      <Findings sh={sh} canAct={canAct} live={live} />
    </section>
  )
}

function useRefresh() {
  const qc = useQueryClient()
  return () => qc.invalidateQueries({ queryKey: ['pulls'] })
}

function CheckNow({ id }: { id: number }) {
  const refresh = useRefresh()
  const check = useMutation({
    mutationFn: () => checkPull(id),
    onError: toastError,
    onSettled: refresh,
  })
  return (
    <Button
      size="xs"
      variant="ghost"
      className="ml-auto"
      loading={check.isPending}
      icon={<RefreshCw className="size-3" />}
      onClick={() => check.mutate()}
      title="Read its threads and checks from the forge now"
    >
      Check now
    </Button>
  )
}

function Readiness({ pr, sh }: { pr: PRDetail; sh: PRShipping }) {
  const refresh = useRefresh()
  const [style, setStyle] = useState<MergeStyle>('')
  const [confirming, setConfirming] = useState(false)
  const merge = useMutation({
    mutationFn: (whenReady: boolean) => mergePull(pr.id, { style, when_ready: whenReady }),
    onSuccess: (r) =>
      toast(r.merged ? 'Merged' : 'It will merge as soon as it is ready', { kind: 'success' }),
    onError: toastError,
    onSettled: refresh,
  })
  const disarm = useMutation({
    mutationFn: () => disarmPull(pr.id),
    onError: toastError,
    onSettled: refresh,
  })
  const ready = sh.readiness.ready
  const armed = sh.armed
  const mayMerge = sh.can_merge
  return (
    <div
      className={clsx(
        'rounded-xl border p-3',
        ready ? 'border-ok/40 bg-ok/5' : 'border-line bg-sunken/40',
      )}
    >
      <div className="flex items-start gap-2 text-[13px]">
        {ready ? (
          <CircleCheck className="mt-0.5 size-4 shrink-0 text-ok" />
        ) : (
          <Timer className="mt-0.5 size-4 shrink-0 text-subtle" />
        )}
        <div className="min-w-0 flex-1">
          <div className="font-medium">
            {ready ? 'Ready to merge' : 'Not ready to merge'}
            {sh.stage && <span className="ml-2 text-xs font-normal text-subtle">{sh.stage}</span>}
          </div>
          {!ready && (
            <ul className="mt-1 list-disc space-y-0.5 pl-4 text-muted">
              {sh.readiness.reasons.map((r) => (
                <li key={r}>{r}</li>
              ))}
            </ul>
          )}
        </div>
      </div>

      {armed && (
        <div className="mt-2 flex flex-wrap items-center gap-x-2 gap-y-1 border-t border-line pt-2 text-[13px]">
          <GitMerge className="size-4 shrink-0 text-accent" />
          <span className="min-w-0 flex-1">
            Merges when ready
            {armed.style
              ? ` (${STYLE_LABEL[armed.style]?.toLowerCase() ?? armed.style})`
              : ''},{' '}
            {armed.label
              ? 'set by the automerge label'
              : armed.setting
                ? `set by the repo’s Merge automatically${armed.scope ? ` (${armed.scope})` : ''}`
                : `set by ${armed.by || 'someone'}`}
            {armed.agent && !armed.label ? ` for ${armed.agent}` : ''}{' '}
            <span className="text-xs text-subtle">{relativeTime(armed.at)}</span>
            {armed.note && <span className="mt-0.5 block text-xs text-warn">{armed.note}</span>}
          </span>
          {mayMerge && (
            <Button
              size="xs"
              variant="ghost"
              loading={disarm.isPending}
              onClick={() => disarm.mutate()}
            >
              Turn off
            </Button>
          )}
        </div>
      )}

      {mayMerge && (
        <div className="mt-3 flex flex-wrap items-center gap-2">
          <Select
            value={style}
            onChange={(e) => setStyle(e.target.value as MergeStyle)}
            aria-label="How to merge"
            className="w-44"
          >
            {Object.entries(STYLE_LABEL).map(([v, l]) => (
              <option key={v} value={v}>
                {l}
              </option>
            ))}
          </Select>
          <Button
            size="sm"
            variant="primary"
            disabled={!ready && !sh.readiness.only_waiting}
            loading={merge.isPending && merge.variables === false}
            icon={<GitMerge className="size-3.5" />}
            onClick={() => setConfirming(true)}
            title={
              ready
                ? undefined
                : sh.readiness.only_waiting
                  ? 'Merge without waiting for reviews to settle'
                  : 'Not ready yet'
            }
          >
            Merge now
          </Button>
          {!armed && (
            <Button
              size="sm"
              variant="secondary"
              loading={merge.isPending && merge.variables === true}
              icon={<Timer className="size-3.5" />}
              onClick={() => merge.mutate(true)}
              title="Merge it the moment it is ready, on the head checked then"
            >
              Merge when ready
            </Button>
          )}
        </div>
      )}
      <Confirm
        open={confirming}
        onClose={() => setConfirming(false)}
        onConfirm={() => merge.mutate(false)}
        title="Merge this pull request?"
        body={
          <>
            It merges into <b>{pr.base_ref || 'its base'}</b> ({STYLE_LABEL[style].toLowerCase()})
            and its branch is deleted.
          </>
        }
        confirmLabel="Merge"
        danger={false}
      />
    </div>
  )
}

function Checks({ sh }: { sh: PRShipping }) {
  if (sh.checks.length === 0) {
    return sh.ci === 'none' ? (
      <p className="text-[13px] text-subtle">This repo reports no checks.</p>
    ) : null
  }
  const failed = sh.checks.filter((c) => c.state === 'failed').length
  const pending = sh.checks.filter((c) => c.state === 'pending').length
  const passed = sh.checks.length - failed - pending
  const summary = [
    passed ? `${passed} passed` : '',
    failed ? `${failed} failed` : '',
    pending ? `${pending} running` : '',
  ]
    .filter(Boolean)
    .join(' · ')
  return (
    <details className="group" open={failed > 0 || pending > 0}>
      <summary className="flex cursor-pointer select-none items-center gap-2 text-[13px] hover:text-fg">
        <span className="font-medium">Checks</span>
        <span className={clsx('text-xs', failed ? 'text-danger' : 'text-muted')}>{summary}</span>
      </summary>
      <ul className="mt-1.5 divide-y divide-line overflow-hidden rounded-xl border border-line">
        {sh.checks.map((c) => (
          <li key={c.name} className="flex items-center gap-2 px-3 py-1.5 text-[13px]">
            <CheckIcon state={c.state} />
            {c.url ? (
              <a
                href={safeUrl(c.url)}
                target="_blank"
                rel="noreferrer"
                className="min-w-0 flex-1 truncate font-mono text-xs hover:underline"
                title={c.description || c.name}
              >
                {c.name}
              </a>
            ) : (
              <span className="min-w-0 flex-1 truncate font-mono text-xs">{c.name}</span>
            )}
            {c.description && (
              <span className="hidden truncate text-xs text-subtle sm:block">{c.description}</span>
            )}
          </li>
        ))}
      </ul>
    </details>
  )
}

function Gate({ sh }: { sh: PRShipping }) {
  const g = sh.gate
  if (!g.enabled) return null
  const state =
    g.state === 'success'
      ? {
          label: 'Passing',
          color: 'var(--wp-ok)',
          icon: <ShieldCheck className="size-4 text-ok" />,
        }
      : g.state === 'failure'
        ? {
            label: 'Blocking',
            color: 'var(--wp-danger)',
            icon: <ShieldAlert className="size-4 text-danger" />,
          }
        : {
            label: g.state === 'pending' ? 'Reviewing' : 'Not run yet',
            color: undefined,
            icon: <LoaderCircle className="size-4 animate-spin text-subtle" />,
          }
  return (
    <div className="flex flex-wrap items-center gap-x-2 gap-y-1 text-[13px]">
      {state.icon}
      <span className="font-medium">Review gate</span>
      <Badge color={state.color}>{state.label}</Badge>
      <span className="min-w-0 flex-1 text-muted">
        {g.note || `Findings ${SEVERITY[g.severity]?.badge ?? ''} and above block merging.`}
      </span>
    </div>
  )
}

function Findings({ sh, canAct, live }: { sh: PRShipping; canAct: boolean; live: boolean }) {
  const refresh = useRefresh()
  const decide = useMutation({
    mutationFn: ({ id, reopen }: { id: number; reopen: boolean }) =>
      reopen ? reopenFinding(id) : dismissFinding(id),
    onError: toastError,
    onSettled: refresh,
  })
  const list = sh.findings.filter((f) => f.placement !== 'withheld' || f.state === 'open')
  if (list.length === 0) return null
  const open = list.filter((f) => f.state === 'open').length
  const blocking = (f: PRFinding) =>
    f.state === 'open' &&
    sh.gate.enabled &&
    (['critical', 'high', 'medium', 'low'] as const).indexOf(f.severity) <=
      (['critical', 'high', 'medium', 'low'] as const).indexOf(sh.gate.severity)
  return (
    <div>
      <h4 className="mb-1.5 text-[13px] font-medium">
        Findings{' '}
        <span className="font-normal text-muted">
          {open} open of {list.length}
        </span>
      </h4>
      <ul className="divide-y divide-line overflow-hidden rounded-xl border border-line">
        {list.map((f) => (
          <li key={f.id} className="px-3 py-2 text-[13px]">
            <div className="flex items-start gap-2">
              <Badge color={SEVERITY[f.severity]?.color} className="mt-px shrink-0">
                {SEVERITY[f.severity]?.badge ?? f.severity}
              </Badge>
              <span className={clsx('min-w-0 flex-1', f.state !== 'open' && 'text-muted')}>
                <span className="break-all font-mono text-xs text-muted">
                  {f.path}
                  {f.line ? `:${f.line}` : ''}
                </span>{' '}
                {f.claim}
                <FindingMarks f={f} />
              </span>
              <FindingState f={f} blocking={blocking(f)} />
              {canAct && live && (f.state === 'open' || f.state === 'dismissed') && (
                <Button
                  size="xs"
                  variant="ghost"
                  className="shrink-0"
                  icon={f.state === 'open' ? undefined : <Undo2 className="size-3" />}
                  loading={decide.isPending && decide.variables?.id === f.id}
                  onClick={() => decide.mutate({ id: f.id, reopen: f.state !== 'open' })}
                  title={
                    f.state === 'open'
                      ? 'It is wrong or not worth fixing: stop it blocking'
                      : 'Count it again'
                  }
                >
                  {f.state === 'open' ? 'Dismiss' : 'Reopen'}
                </Button>
              )}
            </div>
            {f.history && f.history.length > 0 && (
              <ul className="mt-1 space-y-0.5 pl-9 text-xs text-subtle">
                {f.history.map((e, n) => (
                  <li key={n}>
                    {eventLine(e)} <span title={e.at}>{relativeTime(e.at)}</span>
                  </li>
                ))}
              </ul>
            )}
          </li>
        ))}
      </ul>
    </div>
  )
}

function FindingState({ f, blocking }: { f: PRFinding; blocking: boolean }): ReactNode {
  const s = FINDING_STATE[f.state] ?? { label: f.state }
  if (f.state === 'open')
    return blocking ? (
      <Badge color="var(--wp-danger)" className="shrink-0">
        Blocks
      </Badge>
    ) : (
      <Badge className="shrink-0">Open</Badge>
    )
  return (
    <Badge color={s.color} className="shrink-0">
      {s.label}
    </Badge>
  )
}
