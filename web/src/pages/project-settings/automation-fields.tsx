import { useMemo, useState, type ReactNode } from 'react'
import { useQuery } from '@tanstack/react-query'
import clsx from 'clsx'
import { CircleQuestionMark, Eye, EyeOff, Plus } from 'lucide-react'
import { IconButton } from '@/components/ui/button'
import { Field, Input, Select, Switch, Textarea } from '@/components/ui/input'
import { Popover, usePopover } from '@/components/ui/popover'
import { Tip } from '@/components/ui/help'
import { AgentChain, agentsText, type AgentModel } from '@/components/AgentChain'
import { ModelChain, chainText } from '@/components/ModelChain'
import {
  cronQuery,
  leadText,
  paramShown,
  phraseParts,
  type ActionDef,
  type Catalog,
  type ParamDef,
  type PhrasePart,
  type TriggerDef,
} from '@/lib/automations'
import { useDebounced } from '@/lib/hooks'
import { templatesQuery } from '@/lib/templates'
import { CATEGORIES, ISSUE_TYPES, type ChainModel, type Project, type Status } from '@/lib/types'
import { CATEGORY_LABEL, SET_FIELDS, WEEKDAYS } from './rules'

/** A short text or number field that sits in the sentence. */
export const inlineInput = '!h-7 !px-2 text-[13px]'

/** One line of the sentence: a lead word ("When", "then") and its parts. */
export function Clause({
  word,
  end,
  children,
}: {
  word: string
  end?: ReactNode
  children: ReactNode
}) {
  return (
    <div className="flex items-start gap-2">
      <span className="mt-1 w-14 shrink-0 text-[13px] font-semibold text-fg">{word}</span>
      <div className="flex min-w-0 flex-1 flex-wrap items-center gap-x-1.5 gap-y-2 text-[13px] text-muted">
        {children}
      </div>
      {end && <div className="-mr-1 flex shrink-0 items-center">{end}</div>}
    </div>
  )
}

export interface VarGroup {
  title: string
  items: { label: string; value: string }[]
}

/** A menu of values to put in a template: this issue, earlier steps, inputs … */
export function InsertVars({
  groups,
  onInsert,
}: {
  groups: VarGroup[]
  onInsert: (v: string) => void
}) {
  const pop = usePopover()
  return (
    <>
      <button
        ref={pop.anchor}
        type="button"
        onClick={pop.toggle}
        className="inline-flex items-center gap-1 text-xs text-accent hover:underline"
      >
        <Plus className="size-3" />
        Insert a value
      </button>
      <Popover open={pop.open} onClose={pop.close} anchor={pop.anchor} width={320}>
        <div className="max-h-80 overflow-auto p-1">
          {groups.map((g) => (
            <div key={g.title}>
              <div className="px-2 pb-0.5 pt-2 text-[11px] font-semibold uppercase tracking-wide text-subtle">
                {g.title}
              </div>
              {g.items.map((it) => (
                <button
                  key={it.value}
                  type="button"
                  onClick={() => {
                    onInsert(it.value)
                    pop.close()
                  }}
                  className="flex w-full items-baseline justify-between gap-3 rounded-lg px-2 py-1.5 text-left hover:bg-hover"
                >
                  <span className="text-[13px]">{it.label}</span>
                  <span className="truncate font-mono text-[11px] text-subtle">{it.value}</span>
                </button>
              ))}
            </div>
          ))}
        </div>
      </Popover>
    </>
  )
}

const lcFirst = (s: string) => s.charAt(0).toLowerCase() + s.slice(1)

/** A status name or category. */
export function StatusValue({
  value,
  onChange,
  statuses,
  anyLabel,
  label,
  variant = 'inline',
}: {
  value: string
  onChange: (v: string) => void
  statuses: Status[]
  anyLabel: string
  label: string
  variant?: 'inline' | 'field'
}) {
  const known =
    value === '' ||
    statuses.some((s) => s.name === value) ||
    (CATEGORIES as string[]).includes(value)
  return (
    <Select
      variant={variant}
      value={value}
      onChange={(e) => onChange(e.target.value)}
      aria-label={label}
    >
      <option value="">{anyLabel}</option>
      {!known && <option value={value}>{value}</option>}
      <optgroup label="Statuses">
        {statuses.map((s) => (
          <option key={s.id} value={s.name}>
            {s.name}
          </option>
        ))}
      </optgroup>
      <optgroup label="Categories">
        {CATEGORIES.map((c) => (
          <option key={c} value={c}>
            {`any ${CATEGORY_LABEL[c].toLowerCase()} status`}
          </option>
        ))}
      </optgroup>
    </Select>
  )
}

/** A workflow step: picked from the steps workflows have, or typed when there are none. */
export function StepValue({
  value,
  onChange,
  names,
  label,
}: {
  value: string
  onChange: (v: string) => void
  names: string[]
  label: string
}) {
  if (names.length === 0)
    return (
      <Input
        value={value}
        onChange={(e) => onChange(e.target.value)}
        placeholder="any step"
        className={clsx(inlineInput, '!w-32')}
        aria-label={label}
      />
    )
  return (
    <Select
      variant="inline"
      value={value}
      onChange={(e) => onChange(e.target.value)}
      aria-label={label}
    >
      <option value="">any step</option>
      {value && !names.includes(value) && <option value={value}>{value}</option>}
      {names.map((n) => (
        <option key={n} value={n}>
          {n}
        </option>
      ))}
    </Select>
  )
}

/** A person: the catalog's special choices (the assignees, the reporter …) and the project's members. */
export function UserValue({
  param,
  label,
  value,
  onChange,
  project,
  inline,
}: {
  param: ParamDef
  label?: string
  value: string
  onChange: (v: string) => void
  project?: Project
  inline?: boolean
}) {
  const special = param.options ?? []
  const members = project?.members ?? []
  if (!project)
    return (
      <>
        <Input
          list={`users-${param.name}`}
          value={value}
          onChange={(e) => onChange(e.target.value)}
          placeholder={special[0]?.value || 'username'}
          className={clsx(inline ? clsx(inlineInput, '!w-36') : '')}
          aria-label={label ?? param.label}
        />
        <datalist id={`users-${param.name}`}>
          {special
            .filter((s) => s.value)
            .map((s) => (
              <option key={s.value} value={s.value}>
                {s.label}
              </option>
            ))}
        </datalist>
      </>
    )
  const known =
    special.some((s) => s.value === value) || members.some((m) => m.user.username === value)
  return (
    <Select
      variant={inline ? 'inline' : 'field'}
      value={value}
      onChange={(e) => onChange(e.target.value)}
      aria-label={label ?? param.label}
    >
      {!known && <option value={value}>{value ? '@' + value : 'choose…'}</option>}
      {special.map((s) => (
        <option key={s.value} value={s.value}>
          {inline ? lcFirst(s.label) : s.label}
        </option>
      ))}
      {members.length > 0 && (
        <optgroup label="People">
          {members.map((m) => (
            <option key={m.user.id} value={m.user.username}>
              {m.user.display_name || m.user.username} (@{m.user.username})
            </option>
          ))}
        </optgroup>
      )}
    </Select>
  )
}

/** The project's issue templates, for create_issue. */
function TemplateSelect({
  projectKey,
  value,
  onChange,
}: {
  projectKey: string
  value: string
  onChange: (v: string) => void
}) {
  const templates = useQuery(templatesQuery(projectKey)).data ?? []
  return (
    <Select value={value} onChange={(e) => onChange(e.target.value)} aria-label="Template">
      <option value="">No template</option>
      {value && !templates.some((t) => t.name === value) && <option value={value}>{value}</option>}
      {templates.map((t) => (
        <option key={t.id} value={t.name}>
          {t.name}
        </option>
      ))}
    </Select>
  )
}

/** The next times a cron expression fires, in the rule's time zone. */
export function CronPreview({
  expr,
  tz: own,
  fallback,
}: {
  expr: string
  tz: string
  /** the viewer's zone, shown while the rule has none of its own */
  fallback?: string
}) {
  const tz = own || fallback || ''
  const e = useDebounced(expr.trim(), 400)
  const q = useQuery({ ...cronQuery(e, tz), enabled: !!e })
  if (!e)
    return (
      <p className="text-xs text-subtle">
        Five fields: minute hour day-of-month month day-of-week, like <code>0 9 * * 1-5</code> for
        weekdays at 9:00.
      </p>
    )
  if (q.isError) return <p className="text-xs text-danger">{(q.error as Error).message}</p>
  if (!q.data) return <p className="text-xs text-subtle">Checking…</p>
  return (
    <div className="text-xs text-muted" aria-live="polite">
      <span className="font-medium text-fg">Next runs</span>
      {` (${tz || 'UTC'})`}
      <ul className="mt-1 flex flex-wrap gap-x-3 gap-y-0.5">
        {q.data.next.map((t) => (
          <li key={t} className="tabular-nums">
            {new Date(t).toLocaleString([], {
              weekday: 'short',
              day: 'numeric',
              month: 'short',
              hour: '2-digit',
              minute: '2-digit',
              timeZone: tz || 'UTC',
            })}
          </li>
        ))}
      </ul>
      {!own && (
        <p className="mt-1 text-subtle">
          Shown in your time zone. A schedule runs in the project owner’s unless you set one under
          More options.
        </p>
      )}
    </div>
  )
}

const lit = (text: string): PhrasePart => ({ lit: text })
const par = (param: string): PhrasePart => ({ param })

/**
 * Triggers whose old sentences read better than their catalog phrase: the
 * words around their settings. `lead` replaces the words in the dropdown.
 */
const SENTENCE: Record<string, { lead?: string; parts: PhrasePart[] }> = {
  status_changed: { parts: [lit('from'), par('from'), lit('to'), par('to')] },
  step_changed: { parts: [lit('from'), par('from'), lit('to'), par('to')] },
  field_changed: { parts: [lit('—'), par('field')] },
  due_soon: { parts: [lit('within'), par('days'), lit('days')] },
  stale: { parts: [lit('for'), par('days'), lit('days')] },
  pr_command: {
    lead: 'someone comments on a pull request',
    parts: [lit('—'), par('command')],
  },
  agent_task: {
    lead: 'have a coding agent work on a task',
    parts: [par('agents'), lit('and may'), par('permission')],
  },
  fix_ci: { lead: 'have a coding agent fix the failing checks', parts: [par('agents')] },
  schedule: {
    parts: [
      par('every'),
      lit('on'),
      par('weekday'),
      lit('on day'),
      par('month_day'),
      par('cron'),
      lit('at'),
      par('at'),
    ],
  },
}

/** What the dropdown shows for a trigger or action. */
export function leadOf(def: TriggerDef | ActionDef): string {
  return SENTENCE[def.name]?.lead ?? leadText(def.phrase)
}

/** Labels that read better in a sentence than the catalog's. */
const OPTION_LABELS: Record<string, Record<string, string>> = {
  'agent_task.permission': {
    read: 'only read',
    edit: 'edit files',
    edit_run: 'edit files and run commands',
  },
  'schedule.every': {
    day: 'every day',
    weekday: 'every weekday',
    week: 'every week',
    month: 'every month',
    cron: 'a cron expression',
  },
}

/** Names people and tests know these settings by. */
const LABELS: Record<string, string> = {
  'schedule.every': 'Repeat',
  'schedule.at': 'Time',
  'schedule.weekday': 'Weekday',
  'status_changed.from': 'From status',
  'status_changed.to': 'To status',
  'due_soon.days': 'Days',
  'stale.days': 'Days',
  'comment.message': 'Comment',
  'pr_comment.message': 'Comment',
}

const paramLabel = (owner: string, p: ParamDef) => LABELS[`${owner}.${p.name}`] ?? p.label

const EMPTY_LABELS: Record<string, string> = {
  'field_changed.field': 'any field',
  'wait_for.on_timeout': 'fail the step',
}

const INLINE_TYPES = new Set([
  'text',
  'number',
  'duration',
  'select',
  'bool',
  'user',
  'status',
  'glob',
  'field',
  'time',
  'weekday',
  'cron',
  'runner',
  'repo',
  'query',
])
const INLINE_EXTRA = new Set(['repeat.until'])

/** Templates that are one line long. */
const ONE_LINE = new Set(['title', 'key', 'query'])

const inlineable = (owner: string, p: ParamDef) =>
  INLINE_TYPES.has(p.type) || INLINE_EXTRA.has(`${owner}.${p.name}`)

interface Slot {
  kind: 'lit' | 'param'
  text?: string
  p?: ParamDef
}

/** The sentence after the dropdown: the words and settings that sit in the line. */
function slots(def: TriggerDef | ActionDef): { slots: Slot[]; inLine: Set<string> } {
  const override = SENTENCE[def.name]
  let parts = override?.parts
  if (!parts) {
    parts = phraseParts(def.phrase)
    if (parts[0] && 'lit' in parts[0]) parts = parts.slice(1)
  }
  const out: Slot[] = []
  const inLine = new Set<string>()
  for (const part of parts) {
    if ('lit' in part) {
      const text = part.lit
        .replace(/^\s*\)/, '')
        .replace(/\(\s*$/, '')
        .trim()
      if (text) out.push({ kind: 'lit', text })
    } else {
      const p = def.params?.find((x) => x.name === part.param)
      if (!p) continue
      if (p.type === 'models' || p.type === 'agents') out.push({ kind: 'param', p })
      else if (inlineable(def.name, p)) {
        out.push({ kind: 'param', p })
        inLine.add(p.name)
      }
    }
  }
  const by = def.params?.find((p) => p.name === 'by')
  if (by && !inLine.has('by')) {
    out.push({ kind: 'lit', text: by.label.toLowerCase() }, { kind: 'param', p: by })
    inLine.add('by')
  }
  return { slots: out, inLine }
}

export interface FieldEnv {
  catalog: Catalog
  /** undefined: an admin automation */
  project?: Project
  statuses: Status[]
  stepNames: string[]
  timeZone: string
  openQueryHelp: () => void
}

type Vals = Record<string, unknown>
type SetFn = (name: string, v: unknown) => void

/** The words and settings after the dropdown, in the line. */
export function Sentence({
  def,
  vals,
  set,
  env,
}: {
  def: TriggerDef | ActionDef
  vals: Vals
  set: SetFn
  env: FieldEnv
}) {
  const { slots: list } = useMemo(() => slots(def), [def])
  const shown = (s: Slot) => s.kind === 'param' && s.p && paramShown(s.p, vals)
  const out: ReactNode[] = []
  list.forEach((s, i) => {
    if (s.kind === 'param') {
      if (!shown(s)) return
      out.push(
        s.p!.type === 'agents' ? (
          <span key={i} className="font-medium text-fg">
            {((vals[s.p!.name] as AgentModel[] | undefined)?.length ?? 0) > 0
              ? agentsText(vals[s.p!.name] as AgentModel[])
              : 'choose a model'}
          </span>
        ) : s.p!.type === 'models' ? (
          <span key={i} className="font-medium text-fg">
            {((vals[s.p!.name] as ChainModel[] | undefined)?.length ?? 0) > 0
              ? chainText(vals[s.p!.name] as ChainModel[])
              : 'the review models'}
          </span>
        ) : (
          <Control key={i} owner={def.name} p={s.p!} vals={vals} set={set} env={env} inline />
        ),
      )
      return
    }
    const next = list.slice(i + 1).find((x) => x.kind === 'param')
    const prev = list
      .slice(0, i)
      .reverse()
      .find((x) => x.kind === 'param')
    const owner = list[i + 1]?.kind === 'param' ? list[i + 1] : next ? undefined : prev
    if (owner && !shown(owner)) return
    out.push(<span key={i}>{s.text}</span>)
  })
  return <>{out}</>
}

const WIDE = new Set(['template', 'long', 'list', 'globs', 'json', 'models', 'agents', 'url'])

export function ParamBox({
  def,
  vals,
  set,
  env,
  vars,
  hide,
  extra,
}: {
  def: TriggerDef | ActionDef
  vals: Vals
  set: SetFn
  env: FieldEnv
  vars: VarGroup[]
  /** settings that don't show right now (Gotify's own server) */
  hide?: (p: ParamDef) => boolean
  extra?: ReactNode
}) {
  const [more, setMore] = useState(false)
  const { inLine } = useMemo(() => slots(def), [def])
  const rest = (def.params ?? []).filter(
    (p) => !inLine.has(p.name) && paramShown(p, vals) && !hide?.(p),
  )
  const basic = rest.filter((p) => !p.advanced)
  const advanced = rest.filter((p) => p.advanced)
  const hasAdvanced = advanced.some((p) => vals[p.name] !== undefined && vals[p.name] !== '')
  if (rest.length === 0 && !extra) return null
  const field = (p: ParamDef) => (
    <ParamField key={p.name} owner={def.name} p={p} vals={vals} set={set} env={env} vars={vars} />
  )
  return (
    <div className="mt-2 grid gap-3 rounded-lg border border-line bg-panel p-3 sm:ml-16 sm:grid-cols-2">
      {basic.map(field)}
      {advanced.length > 0 && (
        <div className="sm:col-span-2">
          <button
            type="button"
            aria-expanded={more || hasAdvanced}
            onClick={() => setMore((m) => !m)}
            className="text-xs text-accent hover:underline"
          >
            {more || hasAdvanced ? 'Fewer options' : 'More options'}
          </button>
        </div>
      )}
      {(more || hasAdvanced) && advanced.map(field)}
      {extra && <div className="sm:col-span-2">{extra}</div>}
    </div>
  )
}

function ParamField({
  owner,
  p,
  vals,
  set,
  env,
  vars,
}: {
  owner: string
  p: ParamDef
  vals: Vals
  set: SetFn
  env: FieldEnv
  vars: VarGroup[]
}) {
  const wide = WIDE.has(p.type) && !(owner === 'gotify' && p.name === 'url')
  const templated = p.type === 'template' || p.type === 'json'
  return (
    <Field
      label={paramLabel(owner, p)}
      hint={p.help}
      className={wide ? 'sm:col-span-2' : undefined}
      note={
        templated && vars.length > 0 ? (
          <InsertVars groups={vars} onInsert={(v) => set(p.name, String(vals[p.name] ?? '') + v)} />
        ) : undefined
      }
    >
      <Control owner={owner} p={p} vals={vals} set={set} env={env} />
    </Field>
  )
}

function Control({
  owner,
  p,
  vals,
  set,
  env,
  inline,
}: {
  owner: string
  p: ParamDef
  vals: Vals
  set: SetFn
  env: FieldEnv
  inline?: boolean
}) {
  const key = `${owner}.${p.name}`
  const label = paramLabel(owner, p)
  const raw = vals[p.name]
  const v = raw === undefined || raw === null ? p.default : raw
  const str = v === undefined || v === null ? '' : String(v)
  const variant = inline ? 'inline' : 'field'
  const w = (c: string) => (inline ? clsx(inlineInput, c) : '')
  const project = env.project
  const [reveal, setReveal] = useState(false)

  if ((key === 'add_label.value' || key === 'remove_label.value') && project) {
    const id = `labels-${project.key}`
    return (
      <>
        <Input
          list={id}
          value={str}
          onChange={(e) => set(p.name, e.target.value)}
          placeholder="needs-review"
          className={w('!w-36')}
          aria-label="Tag"
        />
        <datalist id={id}>
          {project.labels?.map((l) => (
            <option key={l.id} value={l.name} />
          ))}
        </datalist>
      </>
    )
  }
  if (key === 'add_to_sprint.value' && project) {
    const id = `sprints-${project.key}`
    return (
      <>
        <Input
          list={id}
          value={str}
          onChange={(e) => set(p.name, e.target.value)}
          className={w('!w-32')}
          aria-label="Sprint"
        />
        <datalist id={id}>
          {[
            'active',
            'next',
            'none',
            ...(project.sprints ?? []).filter((s) => s.state !== 'closed').map((s) => s.name),
          ].map((x) => (
            <option key={x} value={x} />
          ))}
        </datalist>
        <Tip>
          <b>active</b>, <b>next</b> (created if needed), a sprint’s name, or <b>none</b> to take it
          out of its sprint.
        </Tip>
      </>
    )
  }
  if (key === 'set_field.value') {
    const hint = SET_FIELDS.find((f) => f.value === vals.field)?.hint
    return (
      <>
        <Input
          value={str}
          onChange={(e) => set(p.name, e.target.value)}
          placeholder="nothing (clears it)"
          className={w('!w-40')}
          aria-label="Value"
        />
        {hint && <Tip>{hint}</Tip>}
      </>
    )
  }
  if (owner === 'step_changed' && (p.name === 'from' || p.name === 'to'))
    return (
      <StepValue
        value={str}
        onChange={(x) => set(p.name, x)}
        names={env.stepNames}
        label={p.name === 'from' ? 'From step' : 'To step'}
      />
    )
  if (key === 'pr_command.command')
    return (
      <Select
        variant={variant}
        value={str}
        onChange={(e) => set(p.name, e.target.value)}
        aria-label="Command"
      >
        <option value="review">review (or full review)</option>
        <option value="">any command</option>
      </Select>
    )
  if (key === 'create_issue.template' && project)
    return <TemplateSelect projectKey={project.key} value={str} onChange={(x) => set(p.name, x)} />
  if (key === 'create_issue.issue_type')
    return (
      <Select value={str} onChange={(e) => set(p.name, e.target.value)} aria-label="Type">
        <option value="">{vals.template ? 'From the template' : 'Task'}</option>
        {ISSUE_TYPES.map((it) => (
          <option key={it} value={it}>
            {it}
          </option>
        ))}
      </Select>
    )
  if (key === 'wait_for.event') {
    const kinds = [...new Set(env.catalog.triggers.map((t) => t.event).filter(Boolean))].sort()
    return (
      <>
        <Input
          list="wait-event-kinds"
          value={str}
          onChange={(e) => set(p.name, e.target.value)}
          placeholder="pr.ci"
          className={w('!w-32 font-mono !text-[12px]')}
          aria-label="Event"
        />
        <datalist id="wait-event-kinds">
          {kinds.map((k) => (
            <option key={k} value={k} />
          ))}
        </datalist>
      </>
    )
  }
  if (p.name === 'labels' && p.type === 'list')
    return (
      <Input
        value={(Array.isArray(raw) ? raw : []).join(', ')}
        onChange={(e) => set(p.name, e.target.value.split(','))}
        placeholder="planning, weekly"
        aria-label={label}
      />
    )
  if (p.name === 'month_day')
    return (
      <Select
        variant={variant}
        value={String(v ?? 1)}
        onChange={(e) => set(p.name, Number(e.target.value))}
        aria-label="Day of the month"
      >
        {Array.from({ length: 28 }, (_, i) => (
          <option key={i} value={i + 1}>
            {i + 1}
          </option>
        ))}
      </Select>
    )

  switch (p.type) {
    case 'select':
    case 'field': {
      const labels = OPTION_LABELS[key] ?? {}
      const opts = p.options ?? []
      const hasEmpty = opts.some((o) => o.value === '')
      return (
        <Select
          variant={variant}
          value={str}
          onChange={(e) => set(p.name, e.target.value)}
          aria-label={label}
        >
          {!hasEmpty && (!p.required || str === '') && (
            <option value="">
              {EMPTY_LABELS[key] ?? p.placeholder ?? (p.required ? 'choose…' : 'any')}
            </option>
          )}
          {str !== '' && !opts.some((o) => o.value === str) && <option value={str}>{str}</option>}
          {opts.map((o) => (
            <option key={o.value} value={o.value}>
              {inline ? (labels[o.value] ?? lcFirst(o.label)) : o.label}
            </option>
          ))}
        </Select>
      )
    }
    case 'weekday':
      return (
        <Select
          variant={variant}
          value={String(v ?? 1)}
          onChange={(e) => set(p.name, Number(e.target.value))}
          aria-label={label}
        >
          {WEEKDAYS.map((d, i) => (
            <option key={d} value={i}>
              {d}
            </option>
          ))}
        </Select>
      )
    case 'bool':
      return <Switch checked={!!v} onChange={(x) => set(p.name, x)} label={label} />
    case 'user':
      return (
        <UserValue
          param={p}
          label={inline ? 'Who' : label}
          value={str}
          onChange={(x) => set(p.name, x)}
          project={project}
          inline={inline}
        />
      )
    case 'status':
      return env.statuses.length || project ? (
        <StatusValue
          value={str}
          onChange={(x) => set(p.name, x)}
          statuses={env.statuses}
          anyLabel={
            owner === 'reopen_parent'
              ? 'in progress'
              : p.required || !isTrigger(env, owner)
                ? 'choose…'
                : 'any status'
          }
          label={label}
          variant={variant}
        />
      ) : (
        <Input
          value={str}
          onChange={(e) => set(p.name, e.target.value)}
          placeholder="done"
          className={w('!w-28')}
          aria-label={label}
        />
      )
    case 'models':
      return (
        <ModelChain
          value={(raw as ChainModel[] | undefined) ?? []}
          onChange={(x) => set(p.name, x)}
          runners={p.runners}
          addLabel={
            Array.isArray(raw) && raw.length
              ? 'Add a fallback'
              : p.name === 'models'
                ? 'Use other models'
                : 'Choose models'
          }
        />
      )
    case 'agents':
      return (
        <AgentChain
          value={(raw as AgentModel[] | undefined) ?? []}
          onChange={(x) => set(p.name, x)}
        />
      )
    case 'number':
      return (
        <Input
          type="number"
          min={p.min}
          max={p.max}
          value={v === undefined || v === null ? '' : String(v)}
          onChange={(e) => set(p.name, e.target.value === '' ? undefined : Number(e.target.value))}
          placeholder={p.placeholder}
          className={clsx(inline ? clsx(inlineInput, '!w-20') : 'w-28')}
          aria-label={label}
        />
      )
    case 'duration':
      return (
        <Input
          value={str}
          onChange={(e) => set(p.name, e.target.value)}
          placeholder={p.placeholder ?? '15m'}
          className={clsx(inline ? clsx(inlineInput, '!w-20') : 'w-28')}
          aria-label={label}
        />
      )
    case 'time':
      return (
        <Input
          type="time"
          value={str || '09:00'}
          onChange={(e) => set(p.name, e.target.value)}
          className={clsx(inline ? clsx(inlineInput, '!w-28') : 'w-32')}
          aria-label={label}
        />
      )
    case 'cron':
      return (
        <Input
          value={str}
          onChange={(e) => set(p.name, e.target.value)}
          placeholder="0 9 * * 1-5"
          spellCheck={false}
          className={clsx(inline ? clsx(inlineInput, '!w-40') : '', 'font-mono !text-[12px]')}
          aria-label={label}
        />
      )
    case 'query':
      return (
        <>
          <Input
            value={str}
            onChange={(e) => set(p.name, e.target.value)}
            placeholder={p.placeholder ?? 'type:bug priority:>=high'}
            spellCheck={false}
            className={clsx(
              inline ? clsx(inlineInput, 'min-w-0 flex-1 basis-40') : '',
              'font-mono !text-[12px]',
            )}
            aria-label={label}
          />
          <IconButton
            label="Query help"
            size="xs"
            onClick={env.openQueryHelp}
            icon={<CircleQuestionMark className="size-3.5" />}
          />
        </>
      )
    case 'secret':
      return (
        <div className="flex items-center gap-1">
          <Input
            type={reveal ? 'text' : 'password'}
            value={str}
            onChange={(e) => set(p.name, e.target.value)}
            autoComplete="off"
            aria-label={label}
          />
          <IconButton
            label={reveal ? 'Hide' : 'Show'}
            onClick={() => setReveal((r) => !r)}
            icon={reveal ? <EyeOff className="size-4" /> : <Eye className="size-4" />}
          />
        </div>
      )
    case 'url':
      return (
        <Input
          type="url"
          value={str}
          onChange={(e) => set(p.name, e.target.value)}
          placeholder={p.placeholder ?? 'https://'}
          aria-label={label}
        />
      )
    case 'long':
    case 'template':
    case 'json': {
      if (inline)
        return (
          <Input
            value={str}
            onChange={(e) => set(p.name, e.target.value)}
            placeholder={p.placeholder ?? '{{.Steps.check.OK}}'}
            spellCheck={false}
            className={clsx(inlineInput, 'min-w-0 flex-1 basis-40 font-mono !text-[12px]')}
            aria-label={label}
          />
        )
      if (p.type === 'template' && ONE_LINE.has(p.name))
        return (
          <Input
            value={str}
            onChange={(e) => set(p.name, e.target.value)}
            placeholder={p.placeholder}
            aria-label={label}
          />
        )
      return (
        <Textarea
          rows={p.type === 'long' ? 3 : 2}
          autoGrow
          value={str}
          onChange={(e) => set(p.name, e.target.value)}
          placeholder={p.placeholder}
          spellCheck={p.type === 'json' ? false : undefined}
          className={p.type === 'json' ? 'font-mono text-xs' : undefined}
          aria-label={label}
        />
      )
    }
    case 'list':
    case 'globs':
      return (
        <Textarea
          rows={3}
          autoGrow
          value={(Array.isArray(raw) ? raw : typeof raw === 'string' ? raw.split('\n') : []).join(
            '\n',
          )}
          onChange={(e) => set(p.name, e.target.value.split('\n'))}
          placeholder={p.placeholder ?? (p.type === 'globs' ? 'docs/**\n*.md' : undefined)}
          spellCheck={false}
          className={p.type === 'globs' ? 'font-mono text-xs' : undefined}
          aria-label={label}
        />
      )
    default:
      return (
        <Input
          value={str}
          onChange={(e) => set(p.name, e.target.value)}
          placeholder={p.placeholder}
          className={w(p.type === 'glob' ? '!w-36 font-mono !text-[12px]' : '!w-36')}
          aria-label={label}
        />
      )
  }
}

const isTrigger = (env: FieldEnv, owner: string) =>
  env.catalog.triggers.some((t) => t.name === owner)
