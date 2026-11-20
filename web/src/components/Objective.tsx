// Objectives (WP-927): a task worked in rounds until a check says it is met
// ("until this runs in under a second"), by an amount every round ("twice
// as fast", never met for good), or as a loop with no check. A round ends
// where the task would close; the objective decides whether it does.

import { useEffect, useRef, useState } from 'react'
import { ExternalLink, Repeat, Target } from 'lucide-react'
import clsx from 'clsx'
import type { Objective, ObjectiveInput, ObjectiveRound } from '@/lib/types'
import { useObjective } from '@/lib/queries'
import { agentLabel } from '@/lib/agents'
import { dateTime, relativeTime, safeUrl } from '@/lib/format'
import { Button } from '@/components/ui/button'
import { Dialog } from '@/components/ui/dialog'
import { Field, Input, Select } from '@/components/ui/input'
import { toast, toastError } from '@/components/ui/toast'

const OUTCOME: Record<ObjectiveRound['outcome'], string> = {
  met: 'met',
  progress: 'better, not met',
  none: 'no better',
  unmeasured: 'ended without a check',
  loop: 'done',
}

function heading(o: Objective): string {
  switch (o.state) {
    case 'proposed':
      return 'Proposed objective'
    case 'met':
      return 'Objective met'
    case 'ended':
      return 'Objective ended'
    default:
      return `Objective · round ${o.round}${o.max_rounds ? ` of ${o.max_rounds}` : ''}`
  }
}

/** The server's sentence with its `commands` set as code. */
function Sentence({ text }: { text: string }) {
  return (
    <>
      {text.split('`').map((part, i) =>
        i % 2 ? (
          <code key={i} className="rounded bg-sunken px-1 font-mono text-[12px]">
            {part}
          </code>
        ) : (
          part
        ),
      )}
    </>
  )
}

/** Seconds as the duration the server reads back: 86400 → "1d", 5400 → "90m". */
export function everyText(seconds: number): string {
  if (!seconds) return '0'
  if (seconds % 86400 === 0) return `${seconds / 86400}d`
  if (seconds % 3600 === 0) return `${seconds / 3600}h`
  return `${Math.max(1, Math.round(seconds / 60))}m`
}

/** Says a task's objective and where its rounds stand, with what a person may do about it. */
export function ObjectiveBanner({
  objective: o,
  editable,
  onChange,
  className,
}: {
  objective: Objective
  editable: boolean
  onChange: () => void
  className?: string
}) {
  const act = useObjective(o.key)
  const proposed = o.state === 'proposed'
  const active = o.state === 'active'
  const tone = proposed ? 'var(--wp-warn)' : o.state === 'met' ? 'var(--wp-ok)' : 'var(--wp-accent)'
  const Icon = o.kind === 'loop' ? Repeat : Target
  const waits = active && o.next_at && new Date(o.next_at).getTime() > Date.now()
  const last = active && o.last?.round === o.round ? o.last : undefined
  return (
    <div
      role="note"
      aria-label="Objective"
      className={clsx(
        'rounded-lg border px-3 py-2 text-[13px]',
        proposed && 'border-dashed',
        className,
      )}
      style={{
        borderColor: `color-mix(in oklab, ${tone} 45%, transparent)`,
        background: `color-mix(in oklab, ${tone} 6%, transparent)`,
      }}
    >
      <div className="flex flex-wrap items-start gap-x-3 gap-y-2">
        <Icon className="mt-0.5 size-4 shrink-0" style={{ color: tone }} aria-hidden />
        <div className="min-w-0 flex-1 basis-56 space-y-1">
          <p className="[overflow-wrap:anywhere]">
            <span className="font-medium">{heading(o)}:</span> <Sentence text={o.summary} />.
          </p>
          {proposed && (
            <p className="text-muted">
              {o.set_agent ? agentLabel(o.set_agent) : 'An agent'} proposed it. Its command isn’t
              run and the task doesn’t repeat until you confirm it.
            </p>
          )}
          {last && (
            <p className="text-muted">
              This round’s last check, {relativeTime(last.at)}: {last.line};{' '}
              {last.met ? 'met' : 'not met'}.
            </p>
          )}
          {waits && (
            <p className="text-muted">
              Agents aren’t handed round {o.round} before {dateTime(o.next_at!)}
              {o.stalled
                ? `: ${o.stalled} round${o.stalled === 1 ? '' : 's'} in a row moved nothing`
                : ''}
              .
            </p>
          )}
          {active && o.pace === 'spare' && (
            <p className="text-muted">Runs only on quota that would go unused.</p>
          )}
          {o.rounds.length > 0 && (
            <details className="text-muted">
              <summary className="cursor-pointer">
                {o.rounds.length === 1
                  ? '1 finished round'
                  : `Last ${o.rounds.length} finished rounds`}
              </summary>
              <ul className="mt-1 space-y-0.5">
                {o.rounds.map((r) => (
                  <li key={r.round} className="[overflow-wrap:anywhere]">
                    Round {r.round}: {OUTCOME[r.outcome] ?? r.outcome}
                    {r.check ? ` (${r.check.line})` : ''}
                    <span className="text-subtle" title={dateTime(r.ended_at)}>
                      {' '}
                      · {relativeTime(r.ended_at)}
                    </span>
                    {safeUrl(r.pr_url) && (
                      <a
                        href={safeUrl(r.pr_url)}
                        target="_blank"
                        rel="noreferrer noopener"
                        className="ml-1.5 inline-flex items-center gap-1 text-subtle hover:text-fg"
                      >
                        <ExternalLink className="size-3" aria-hidden />
                        pull request
                      </a>
                    )}
                  </li>
                ))}
              </ul>
            </details>
          )}
        </div>
        {editable && (
          <div className="flex shrink-0 gap-2">
            {proposed && (
              <Button
                size="sm"
                variant="primary"
                loading={act.isPending && !!act.variables && 'confirm' in act.variables}
                onClick={() =>
                  act.mutate(
                    { confirm: o.rev },
                    {
                      onSuccess: () => toast(`${o.key}’s objective is in effect`),
                      onError: toastError,
                    },
                  )
                }
              >
                Confirm
              </Button>
            )}
            <Button size="sm" onClick={onChange}>
              Change…
            </Button>
            <Button
              size="sm"
              variant="ghost"
              loading={act.isPending && !!act.variables && 'clear' in act.variables}
              onClick={() =>
                act.mutate(
                  { clear: true },
                  {
                    onSuccess: () => toast(`${o.key} has no objective now: it’s an ordinary task`),
                    onError: toastError,
                  },
                )
              }
            >
              {proposed ? 'Discard' : 'Remove'}
            </Button>
          </div>
        )}
      </div>
    </div>
  )
}

/** Gives a task an objective, or changes the one it has. */
export function ObjectiveDialog({
  open,
  onClose,
  issueKey,
  objective: o,
}: {
  open: boolean
  onClose: () => void
  issueKey: string
  objective?: Objective
}) {
  const save = useObjective(issueKey)
  const [check, setCheck] = useState('')
  const [until, setUntil] = useState('')
  const [dir, setDir] = useState('')
  const [setup, setSetup] = useState('')
  const [pace, setPace] = useState<Objective['pace']>('queue')
  const [every, setEvery] = useState('')
  const [rounds, setRounds] = useState('')
  // Filled in as the dialog opens; a refetch of the objective while it is
  // open (an agent's round finishing) mustn't wipe what is being typed.
  const latest = useRef(o)
  latest.current = o
  useEffect(() => {
    if (!open) return
    const o = latest.current
    setCheck(o?.check ?? '')
    setUntil(o?.until ?? '')
    setDir(o?.dir ?? '')
    setSetup(o?.setup ?? '')
    setPace(o?.pace ?? 'queue')
    setEvery(o ? everyText(o.every) : '')
    setRounds(o?.max_rounds ? String(o.max_rounds) : '')
  }, [open])
  const loop = !check.trim()
  const submit = () => {
    // A loop has no check, so nothing that goes with one.
    const set: ObjectiveInput = {
      check: check.trim(),
      until: loop ? '' : until.trim(),
      dir: loop ? '' : dir.trim(),
      setup: loop ? '' : setup.trim(),
      pace,
      max_rounds: Math.max(0, parseInt(rounds, 10) || 0),
    }
    // Blank, or left as it was, keeps the kind's own gap: a day for one
    // that never ends, none for one with a fixed target.
    if (every.trim() && !(o && every.trim() === everyText(o.every))) set.every = every.trim()
    save.mutate(
      { set },
      {
        onSuccess: () => {
          toast(o ? `Changed ${issueKey}’s objective` : `${issueKey} has an objective`)
          onClose()
        },
        onError: toastError,
      },
    )
  }
  return (
    <Dialog
      open={open}
      onClose={onClose}
      title={o ? `Change ${issueKey}’s objective` : `Give ${issueKey} an objective`}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button variant="primary" onClick={submit} loading={save.isPending}>
            {o ? 'Save' : 'Set objective'}
          </Button>
        </>
      }
    >
      <form
        className="space-y-4 p-4"
        onSubmit={(e) => {
          e.preventDefault()
          submit()
        }}
      >
        <p className="text-[13px] text-muted">
          Agents work the task in rounds. Each round ends with a pull request; the task opens again
          for the next until the check says the objective is met, or you close it.
        </p>
        <Field
          label="Check command"
          hint="Shell, run in the task’s repository by the agent working the round and again before it ships. Blank: a loop with no check, one useful step a round, until you close the task."
        >
          <Input
            autoFocus
            className="font-mono"
            value={check}
            onChange={(e) => setCheck(e.target.value)}
            placeholder="go test ./internal/report -run TestSlow"
          />
        </Field>
        <Field
          label="Until"
          hint="‘< 1s’ or ‘>= 80%’: a fixed target; the task closes once the check reads it (a time is how long the command takes, a number is the last one it prints). ‘2x’, ‘-10%’ or ‘+5%’: that much better than where each round started, so it is never met for good. ‘pass’: the command exits 0."
          note={
            loop ? 'No check: a loop. Nothing is measured and it never ends by itself.' : undefined
          }
        >
          <Input
            value={loop ? '' : until}
            disabled={loop}
            onChange={(e) => setUntil(e.target.value)}
            placeholder="< 1s"
          />
        </Field>
        <div className="grid gap-4 sm:grid-cols-2">
          <Field label="Folder" hint="Where in the repository the command runs. Blank: its root.">
            <Input
              className="font-mono"
              value={dir}
              disabled={loop}
              onChange={(e) => setDir(e.target.value)}
              placeholder="web"
            />
          </Field>
          <Field
            label="Setup"
            hint="Shell run once before a timed check, and not timed: a build, say."
          >
            <Input
              className="font-mono"
              value={setup}
              disabled={loop}
              onChange={(e) => setSetup(e.target.value)}
              placeholder="make build"
            />
          </Field>
          <Field
            label="Pace"
            hint="In the queue: rounds are handed out like any task. Spare quota only: a round starts only when the week’s quota would otherwise go unused, so it never takes from other work."
          >
            <Select value={pace} onChange={(e) => setPace(e.target.value as Objective['pace'])}>
              <option value="queue">In the queue</option>
              <option value="spare">Spare quota only</option>
            </Select>
          </Field>
          <Field
            label="Between rounds"
            hint="The least time from one round’s end to the next’s start: 0, 90m, 24h, 3d. Blank: a day for one that never ends by itself, none for a fixed target. Rounds that move nothing wait longer by themselves."
          >
            <Input value={every} onChange={(e) => setEvery(e.target.value)} placeholder="24h" />
          </Field>
          <Field label="Most rounds" hint="It ends after this many rounds. Blank: no limit.">
            <Input
              type="number"
              min={0}
              value={rounds}
              onChange={(e) => setRounds(e.target.value)}
              placeholder="No limit"
            />
          </Field>
        </div>
      </form>
    </Dialog>
  )
}
