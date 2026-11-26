import { useMemo, useRef, useState, type ReactNode } from 'react'
import { useQuery } from '@tanstack/react-query'
import clsx from 'clsx'
import { Undo2 } from 'lucide-react'
import { q } from '@/lib/queries'
import {
  MAX_CRITERIA_QUERY,
  MAX_REVIEW_PROMPT,
  MAX_REVIEW_TEXT,
  MERGE_LABEL,
  REVIEW_SKIP_LABEL,
  MIN_PRIORITY_LABEL,
  type AutopilotLimits,
  type MergePolicy,
  type ReviewSkip,
  type Priority,
} from '@/lib/autopilot'
import { Button } from '@/components/ui/button'
import { SavedMark, useAutosave, useSent } from '@/components/ui/autosave'
import { Tip } from '@/components/ui/help'
import { Field, Input, Select, Textarea } from '@/components/ui/input'
import { toastError } from '@/components/ui/toast'

/** A save that failed: say so, and let the form keep what was typed. */
export const saveFailed = (e: unknown) => {
  toastError(e)
  throw e
}

type Tri = '' | 'on' | 'off'

interface LimitsDraft {
  max_hourly_usd: string
  week_ahead: string
  week_max: string
  week_end_hours: string
  five_hour_max: string
  merge: '' | MergePolicy
  grace_seconds: string
  deferred: Tri
  review: Tri
  review_prompt: string
  review_skip: '' | ReviewSkip
  review_text: string
  /**
   * 'on': the brief is set to Waypoint's built-in one (saved as an empty
   * brief, which overrides a custom one inherited), where review_text is
   * empty. '' inherits.
   */
  review_builtin: '' | 'on'
  /** '' inherits */
  review_model: string
  /** '' inherits; 'any' sets no minimum */
  min_priority: '' | 'any' | Priority
  due_within_days: string
  approved: Tri
  planned: Tri
  query: string
  per_hour: string
  per_day: string
  spacing_minutes: string
  at_once: string
  merge_wait_hours: string
  hours: string
  days: string
}

type NumKey =
  | 'max_hourly_usd'
  | 'week_ahead'
  | 'week_max'
  | 'week_end_hours'
  | 'five_hour_max'
  | 'grace_seconds'
  | 'due_within_days'
  | 'per_hour'
  | 'per_day'
  | 'spacing_minutes'
  | 'at_once'
  | 'merge_wait_hours'

const tri = (b: boolean | undefined): Tri => (b === undefined ? '' : b ? 'on' : 'off')

const draftOf = (l: AutopilotLimits): LimitsDraft => ({
  max_hourly_usd: l.max_hourly_usd?.toString() ?? '',
  week_ahead: l.week_ahead?.toString() ?? '',
  week_max: l.week_max?.toString() ?? '',
  week_end_hours: l.week_end_hours?.toString() ?? '',
  five_hour_max: l.five_hour_max?.toString() ?? '',
  merge: l.merge ?? '',
  grace_seconds: l.grace_seconds?.toString() ?? '',
  deferred: tri(l.deferred),
  review: tri(l.review),
  review_prompt: l.review_prompt ?? '',
  review_skip: l.review_skip ?? '',
  review_text: l.review_text ?? '',
  review_builtin: l.review_text === '' ? 'on' : '',
  review_model: l.review_model ?? '',
  min_priority: l.min_priority === undefined ? '' : l.min_priority || 'any',
  due_within_days: l.due_within_days?.toString() ?? '',
  approved: tri(l.approved),
  planned: tri(l.planned),
  query: l.query ?? '',
  per_hour: l.per_hour?.toString() ?? '',
  per_day: l.per_day?.toString() ?? '',
  spacing_minutes: l.spacing_minutes?.toString() ?? '',
  at_once: l.at_once?.toString() ?? '',
  merge_wait_hours: l.merge_wait_hours?.toString() ?? '',
  hours: l.hours === undefined ? '' : l.hours || 'any',
  days: l.days === undefined ? '' : l.days || 'all',
})

/**
 * How long a text is as the server counts it: in characters (code points),
 * where .length counts an emoji or a rare CJK character as two.
 */
const chars = (s: string) => {
  let n = 0
  for (const _ of s) n++
  return n
}

/** The limits a draft sets, or the first thing wrong with it. */
function limitsOf(d: LimitsDraft): { limits: AutopilotLimits; error?: string } {
  const limits: AutopilotLimits = {}
  const num = (key: NumKey, name: string, max: number, whole = false, min = 0) => {
    const raw = d[key].trim()
    if (raw === '') return undefined
    const n = Number(raw)
    if (!Number.isFinite(n) || n < min || n > max || (whole && !Number.isInteger(n)))
      return `${name} must be ${whole ? 'a whole number ' : ''}from ${min} to ${max}`
    limits[key] = n
    return undefined
  }
  const error =
    num('max_hourly_usd', 'Hourly spend', 100000) ??
    num('week_ahead', 'Points ahead', 100) ??
    num('week_max', 'Week ceiling', 100) ??
    num('week_end_hours', 'Week’s end', 168) ??
    num('five_hour_max', 'Five-hour ceiling', 100) ??
    num('grace_seconds', 'Grace', 3600, true) ??
    num('due_within_days', 'Due within', 365, true) ??
    num('per_hour', 'Tasks an hour', 60, true, 1) ??
    num('per_day', 'Tasks a day', 500, true) ??
    num('spacing_minutes', 'Spacing', 1440, true) ??
    num('at_once', 'Sessions at once', 20, true) ??
    num('merge_wait_hours', 'Longest merge wait', 168, true, 1)
  if (d.merge) limits.merge = d.merge
  if (d.deferred) limits.deferred = d.deferred === 'on'
  if (d.review) limits.review = d.review === 'on'
  if (d.review_skip) limits.review_skip = d.review_skip
  if (d.review_model) limits.review_model = d.review_model
  if (d.approved) limits.approved = d.approved === 'on'
  if (d.planned) limits.planned = d.planned === 'on'
  if (d.min_priority) limits.min_priority = d.min_priority === 'any' ? '' : d.min_priority
  const prompt = d.review_prompt.trim()
  if (prompt) limits.review_prompt = prompt
  const brief = d.review_text.trim()
  if (brief) limits.review_text = brief
  else if (d.review_builtin) limits.review_text = ''
  const query = d.query.trim()
  if (query) limits.query = query
  const hours = d.hours.trim()
  if (hours) limits.hours = hours
  const days = d.days.trim()
  if (days) limits.days = days
  const textError =
    chars(prompt) > MAX_REVIEW_PROMPT
      ? `The review prompt must be at most ${MAX_REVIEW_PROMPT} characters`
      : chars(brief) > MAX_REVIEW_TEXT
        ? `The reviewer’s brief must be at most ${MAX_REVIEW_TEXT} characters`
        : query.length > MAX_CRITERIA_QUERY
          ? `The query must be at most ${MAX_CRITERIA_QUERY} characters`
          : /\b(order|sort):/i.test(query)
            ? 'The query picks tasks, not their order: drop order: (autopilot takes them in wp next’s order)'
            : hours && hours !== 'any' && !/^\d{1,2}(:\d\d)?\s*-\s*\d{1,2}(:\d\d)?$/.test(hours)
              ? 'Hours must be like 09:00-17:00 (22:00-06:00 runs overnight)'
              : undefined
  return { limits, error: error ?? textError }
}

/** What a review_model value is called, as the server lists them. */
function reviewerLabel(
  value: string | undefined,
  o: { modes: { value: string; label: string }[] } | undefined,
): string {
  const v = value || 'auto'
  return (
    o?.modes.find((m) => m.value === v)?.label.toLowerCase() ?? (v === 'auto' ? 'automatic' : v)
  )
}

const knownReviewer = (
  value: string,
  o:
    | { modes: { value: string }[]; gateway: { value: string }[]; runners: { value: string }[] }
    | undefined,
) => !!o && [...o.modes, ...o.gateway, ...o.runners].some((m) => m.value === value)

/**
 * The limits as fields. Empty ones inherit, and say what they inherit as
 * their placeholder. Changes save by themselves once they settle (selects at
 * once, typing after a pause): `onSave` gets exactly the set ones, which
 * replace the row's; `onReset` clears them all.
 */
export function LimitsForm({
  value,
  inherit,
  onSave,
  onReset,
  saving,
  idPrefix,
}: {
  value: AutopilotLimits
  inherit: AutopilotLimits
  /** answers the limits as the server keeps them */
  onSave: (l: AutopilotLimits) => Promise<AutopilotLimits | undefined>
  onReset?: () => void
  saving: boolean
  idPrefix: string
}) {
  const initial = useMemo(() => draftOf(value), [value])
  const [draft, setDraft] = useState(initial)
  const [seen, setSeen] = useState(JSON.stringify(initial))
  const sent = useSent()
  if (seen !== JSON.stringify(initial)) {
    const before = JSON.parse(seen) as LimitsDraft
    setSeen(JSON.stringify(initial))
    if (!sent.mine(JSON.stringify(limitsOf(initial).limits)))
      setDraft((d) => {
        const next = { ...d }
        for (const k of Object.keys(initial) as (keyof LimitsDraft)[])
          if (d[k] === before[k]) Object.assign(next, { [k]: initial[k] })
        return next
      })
  }
  const { limits, error } = limitsOf(draft)
  const limitsKey = JSON.stringify(limits)
  const instant = useRef(false)
  const { done, flush } = useAutosave(
    limitsKey,
    JSON.stringify(limitsOf(initial).limits),
    !error,
    instant.current ? 0 : null,
    () => {
      sent.add(limitsKey)
      const was = draft
      return onSave(limits).then((back) => {
        if (!back) return
        const kept = draftOf(back)
        sent.add(JSON.stringify(limitsOf(kept).limits))
        setDraft((d) => {
          const next = { ...d }
          for (const k of Object.keys(kept) as (keyof LimitsDraft)[])
            if (d[k] === was[k]) Object.assign(next, { [k]: kept[k] })
          return next
        })
      })
    },
  )
  const reviewers = useQuery(q.reviewModels()).data
  const builtin = draft.review_builtin === 'on'
  const shownBrief = (!builtin && inherit.review_text) || reviewers?.default_text || ''
  const set = (k: keyof LimitsDraft) => (e: { target: { value: string } }) => {
    instant.current = e.target instanceof HTMLSelectElement
    setDraft((d) => ({ ...d, [k]: e.target.value }))
  }
  const ph = (n: number | undefined) => (n === undefined ? '' : String(n))
  const field = (k: NumKey, label: string, hint: ReactNode, step: string) => (
    <Field label={label} hint={hint}>
      <Input
        id={`${idPrefix}-${k}`}
        type="number"
        inputMode="decimal"
        min={k === 'per_hour' || k === 'merge_wait_hours' ? 1 : 0}
        step={step}
        value={draft[k]}
        placeholder={ph(inherit[k])}
        onChange={set(k)}
        className="tabular-nums"
      />
    </Field>
  )
  const text = (
    k: 'query' | 'hours' | 'days',
    label: string,
    hint: ReactNode,
    empty: string,
    className?: string,
  ) => (
    <Field label={label} hint={hint} className={className}>
      <Input
        id={`${idPrefix}-${k}`}
        value={draft[k]}
        maxLength={k === 'query' ? MAX_CRITERIA_QUERY + 50 : 40}
        placeholder={inherit[k] || empty}
        onChange={set(k)}
        spellCheck={false}
        autoComplete="off"
        className={clsx(k === 'query' && 'font-mono text-xs')}
      />
    </Field>
  )
  const onOff = (
    k: 'deferred' | 'review' | 'approved' | 'planned',
    label: string,
    hint: ReactNode,
  ) => (
    <Field label={label} hint={hint}>
      <Select id={`${idPrefix}-${k}`} value={draft[k]} onChange={set(k)} aria-label={label}>
        <option value="">{`Inherit (${inherit[k] ? 'on' : 'off'})`}</option>
        <option value="on">On</option>
        <option value="off">Off</option>
      </Select>
    </Field>
  )
  return (
    <form
      className="space-y-3"
      onBlur={flush}
      onKeyDown={(e) => e.key === 'Enter' && e.target instanceof HTMLInputElement && flush()}
      onSubmit={(e) => e.preventDefault()}
    >
      <div className="grid grid-cols-2 gap-3">
        {field(
          'max_hourly_usd',
          'Hourly spend ($)',
          'Pause when the last hour’s spend, at list price across all your sessions, reaches this.',
          '1',
        )}
        {field(
          'grace_seconds',
          'Grace (seconds)',
          'How long after a session finishes before it is restarted. Typing keep or /keep in it during that time keeps it, without a turn.',
          '1',
        )}
        {field(
          'week_ahead',
          'Week: points ahead',
          'Pause when the week’s quota use runs more than this many points ahead of an even pace.',
          '1',
        )}
        {field(
          'week_max',
          'Week: pause at %',
          'Pause when the week’s quota use passes this, whatever the pace.',
          '1',
        )}
        {field(
          'week_end_hours',
          'Week’s end (hours)',
          'No ceilings (100%) in the week’s last this many hours, rising to it over as many hours before: what is left at the reset goes unused.',
          '1',
        )}
        {field(
          'five_hour_max',
          '5-hour: pause at %',
          'Pause when the five-hour window’s use passes this, until it resets.',
          '1',
        )}
        <Field
          label="Merge wait"
          hint="Whether the next task waits for the session’s unmerged pull request. Auto: only a task that builds on it waits, unrelated ones go first. Always, with a long wait: the next task starts once the last one merged."
        >
          <Select
            id={`${idPrefix}-merge`}
            value={draft.merge}
            onChange={set('merge')}
            aria-label="Merge wait"
          >
            <option value="">{`Inherit (${MERGE_LABEL[inherit.merge ?? 'auto'].toLowerCase()})`}</option>
            {(Object.keys(MERGE_LABEL) as MergePolicy[]).map((m) => (
              <option key={m} value={m}>
                {MERGE_LABEL[m]}
              </option>
            ))}
          </Select>
        </Field>
        {field(
          'merge_wait_hours',
          'Longest merge wait (hours)',
          'How long a task waits for a merge at most, then starts anyway. Start now skips the wait.',
          '1',
        )}
        {onOff(
          'deferred',
          'Offer deferred tasks at ship',
          'When an Opus or Fable agent ships and filed other tasks meanwhile, Waypoint lists them once, before the ship or after it, so the agent can do the fitting ones.',
        )}
        {onOff(
          'review',
          'Adversarial review',
          'Before a finished task’s pull request opens, compact the session and have it review its own change and fix what it finds, whether autopilot is on or off. Its report goes into the task’s work log, counted. Only for tasks with code to ship.',
        )}
      </div>
      {(draft.review === 'on' || (draft.review === '' && inherit.review)) && (
        <>
          <Field
            label="Low-risk changes"
            hint="Whether a small change that only touches docs or comments skips the review. Risky changes are always reviewed, and one in five that could skip is reviewed anyway, to check the rules. wp worklog triage shows how it’s doing."
          >
            <Select
              id={`${idPrefix}-review_skip`}
              value={draft.review_skip}
              onChange={set('review_skip')}
              aria-label="Low-risk changes"
            >
              <option value="">{`Inherit (${REVIEW_SKIP_LABEL[inherit.review_skip ?? 'on'].toLowerCase()})`}</option>
              {(Object.keys(REVIEW_SKIP_LABEL) as ReviewSkip[]).map((m) => (
                <option key={m} value={m}>
                  {REVIEW_SKIP_LABEL[m]}
                </option>
              ))}
            </Select>
          </Field>
          <Field
            label="Review prompt"
            hint="What the review asks the agent. Empty inherits the one shown in grey."
          >
            <Textarea
              id={`${idPrefix}-review_prompt`}
              rows={2}
              maxLength={MAX_REVIEW_PROMPT + 100}
              value={draft.review_prompt}
              placeholder={inherit.review_prompt ?? ''}
              onChange={set('review_prompt')}
            />
          </Field>
          <Field
            label="Reviewer"
            hint="Who reviews each round. Automatic: the LLM gateway’s audit model, held to a bar by the change’s stakes. The same session: it reviews its own change with an adversarial-review subagent and reports what that found to Waypoint, which checks and scores the findings and asks for another round like any other reviewer’s. A model: that model reviews; if it fails, the round falls back to automatic. Any other cli/model: wp autopilot set --review-model."
          >
            <Select
              id={`${idPrefix}-review_model`}
              value={draft.review_model}
              onChange={set('review_model')}
              aria-label="Reviewer"
            >
              <option value="">{`Inherit (${reviewerLabel(inherit.review_model, reviewers)})`}</option>
              {reviewers?.modes.map((o) => (
                <option key={o.value} value={o.value}>
                  {o.label}
                </option>
              ))}
              {!!reviewers?.gateway.length && (
                <optgroup label="Behind the LLM gateway">
                  {reviewers.gateway.map((o) => (
                    <option key={o.value} value={o.value}>
                      {o.note ? `${o.label} (${o.note})` : o.label}
                    </option>
                  ))}
                </optgroup>
              )}
              {!!reviewers?.runners.length && (
                <optgroup label="On your runners">
                  {reviewers.runners.map((o) => (
                    <option key={o.value} value={o.value}>
                      {o.label}
                    </option>
                  ))}
                </optgroup>
              )}
              {draft.review_model && !knownReviewer(draft.review_model, reviewers) && (
                <option value={draft.review_model}>{draft.review_model}</option>
              )}
            </Select>
          </Field>
          <Field
            label="Reviewer’s brief"
            hint="How every round’s reviewer is told to review, like a skill: it goes ahead of the fixed rules and output form. Empty inherits the one shown in grey."
            error={
              chars(draft.review_text.trim()) > MAX_REVIEW_TEXT
                ? `${chars(draft.review_text.trim()).toLocaleString()} characters: the most is ${MAX_REVIEW_TEXT.toLocaleString()}.`
                : undefined
            }
            note={
              draft.review_text ? (
                <span className="tabular-nums">
                  {`${chars(draft.review_text.trim()).toLocaleString()} of ${MAX_REVIEW_TEXT.toLocaleString()} characters`}
                </span>
              ) : (
                <span className="flex flex-wrap items-center gap-x-2 gap-y-1">
                  {builtin && <span>Set to Waypoint’s built-in brief, shown in grey.</span>}
                  <Button
                    size="sm"
                    variant="ghost"
                    onClick={() => setDraft((d) => ({ ...d, review_text: shownBrief }))}
                    disabled={!shownBrief}
                  >
                    Start from it
                  </Button>
                  {builtin ? (
                    <Button
                      size="sm"
                      variant="ghost"
                      onClick={() => {
                        instant.current = true
                        setDraft((d) => ({ ...d, review_builtin: '' }))
                      }}
                    >
                      Inherit instead
                    </Button>
                  ) : (
                    !!inherit.review_text && (
                      <Button
                        size="sm"
                        variant="ghost"
                        onClick={() => {
                          instant.current = true
                          setDraft((d) => ({ ...d, review_builtin: 'on' }))
                        }}
                      >
                        Use the built-in
                      </Button>
                    )
                  )}
                </span>
              )
            }
          >
            <Textarea
              id={`${idPrefix}-review_text`}
              rows={8}
              autoGrow
              maxLength={2 * MAX_REVIEW_TEXT + 100}
              value={draft.review_text}
              placeholder={shownBrief}
              onChange={set('review_text')}
              className="text-xs"
            />
          </Field>
        </>
      )}
      <fieldset className="space-y-2 border-t border-line pt-3">
        <legend className="mb-1 flex items-center gap-1 text-xs font-medium text-muted">
          Which tasks
          <Tip label="About which tasks">
            Autopilot starts only ready tasks that match every criterion set here, in wp next’s
            order. Nothing else changes: wp next and the queue still offer every task.
          </Tip>
        </legend>
        <div className="grid grid-cols-2 gap-3">
          <Field
            label="Priority"
            hint="Only tasks of this priority or above. Tasks with no priority never match a minimum."
          >
            <Select
              id={`${idPrefix}-min_priority`}
              value={draft.min_priority}
              onChange={set('min_priority')}
              aria-label="Priority"
            >
              <option value="">{`Inherit (${inherit.min_priority ? MIN_PRIORITY_LABEL[inherit.min_priority].toLowerCase() : 'any'})`}</option>
              <option value="any">Any priority</option>
              {(Object.keys(MIN_PRIORITY_LABEL) as Priority[]).map((p) => (
                <option key={p} value={p}>
                  {MIN_PRIORITY_LABEL[p]}
                </option>
              ))}
            </Select>
          </Field>
          {field(
            'due_within_days',
            'Due within (days)',
            'Only tasks due within this many days, overdue ones included. Tasks with no due date never match.',
            '1',
          )}
          {onOff(
            'approved',
            'Approved only',
            'Only tasks a person wrote, or whose agent-written text a person reviewed (is:approved).',
          )}
          {onOff(
            'planned',
            'Planned only',
            'Only tasks a published plan names, or that a free model has prepared (is:planned).',
          )}
          {text(
            'query',
            'Query',
            'A search the tasks must also match, as in the search bar: label:backend, under:WP-3, "login page". Empty inherits.',
            'label:backend -label:risky',
            'col-span-2',
          )}
        </div>
      </fieldset>
      <fieldset className="space-y-2 border-t border-line pt-3">
        <legend className="mb-1 flex items-center gap-1 text-xs font-medium text-muted">
          Pace
          <Tip label="About pace">
            How often autopilot starts a task, counted in each project, in your time zone. Start and
            Start now skip the hours, the spacing and tasks at once; the counts still hold.
          </Tip>
        </legend>
        <div className="grid grid-cols-2 gap-3">
          {field(
            'per_hour',
            'Tasks an hour',
            'At most this many starts in an hour (1-60). It guards against a loop, so it can’t be off.',
            '1',
          )}
          {field('per_day', 'Tasks a day', 'At most this many starts in a calendar day.', '1')}
          {field(
            'spacing_minutes',
            'Spacing (minutes)',
            'At least this long between one start and the next.',
            '1',
          )}
          {field(
            'at_once',
            'Sessions at once',
            'Autopilot starts sessions of its own, up to this many in the project at a time. Your own sessions count while at work on a task, until it ships. 0 starts none.',
            '1',
          )}
          {text(
            'hours',
            'Hours',
            'Only start tasks between these times, like 09:00-17:00; 22:00-06:00 runs overnight. "any" overrides an inherited window.',
            'any time',
          )}
          {text(
            'days',
            'Days',
            'Only start tasks on these days, like mon-fri or sat,sun. "all" overrides inherited days.',
            'every day',
          )}
        </div>
      </fieldset>
      {error && <p className="text-xs text-danger">{error}</p>}
      <div className="flex flex-wrap items-center gap-2">
        {onReset && (
          <Button
            size="sm"
            variant="ghost"
            onClick={() => {
              setDraft(draftOf({}))
              onReset()
            }}
            disabled={saving || Object.keys(value).length === 0}
            title="Clear every override here so it inherits again"
          >
            <Undo2 className="size-3.5" />
            Reset
          </Button>
        )}
        <SavedMark show={done} />
        <span className="ml-auto inline-flex items-center gap-1 text-xs text-subtle">
          <Tip label="About empty fields">
            An empty field inherits the value shown in grey. <b>0 turns a limit off.</b>
          </Tip>
        </span>
      </div>
    </form>
  )
}
