import { useMemo, useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import clsx from 'clsx'
import { CalendarClock, ChevronRight, Plus, X } from 'lucide-react'
import { hermes } from '@/lib/assistant'
import { useTick } from '@/lib/hooks'
import { Button } from '@/components/ui/button'
import { Dialog } from '@/components/ui/dialog'
import { Field, Input, Select, Textarea } from '@/components/ui/input'
import { Picker, Popover, usePopover } from '@/components/ui/popover'
import { toast } from '@/components/ui/toast'
import {
  jobPath,
  jobsQuery,
  modelInfoQuery,
  putJob,
  skillsQuery,
  targetsQuery,
  toolsetsQuery,
  type CronJob,
} from './api'
import {
  buildSchedule,
  builderFrom,
  defaultBuilder,
  describe,
  parseSchedule,
  upcoming,
  wallLabel,
  WEEK,
  zoneNote,
  type Builder,
  type Mode,
  type Zone,
} from './schedule'
import { deliverLabel } from './JobRow'

export function JobEditor({
  assistantId,
  job,
  zone,
  onClose,
  onSaved,
}: {
  assistantId: number
  /** absent: a new schedule */
  job?: CronJob
  zone: Zone
  onClose: () => void
  onSaved?: (job: CronJob) => void
}) {
  const qc = useQueryClient()
  const targets = useQuery(targetsQuery(assistantId)).data?.targets
  const modelInfo = useQuery(modelInfoQuery(assistantId)).data
  const initial = useMemo(
    () => (job ? builderFrom(job.schedule, job.schedule_display, zone) : defaultBuilder(zone)),
    [job, zone],
  )
  const [name, setName] = useState(job?.name ?? '')
  const [prompt, setPrompt] = useState(job?.prompt ?? '')
  const [builder, setBuilder] = useState<Builder>(initial)
  const [deliver, setDeliver] = useState(job?.deliver || 'local')
  const [skills, setSkills] = useState<string[]>(job?.skills ?? [])
  const [toolsets, setToolsets] = useState<string[] | null>(job?.enabled_toolsets ?? null)
  const [model, setModel] = useState(job?.model ?? '')
  const [more, setMore] = useState(
    !!(job && (job.skills.length || job.enabled_toolsets || job.model)),
  )
  const [error, setError] = useState('')
  const [saving, setSaving] = useState(false)

  const schedule = buildSchedule(builder)
  const scheduleChanged = !job || schedule !== buildSchedule(initial)
  const missing =
    !prompt.trim() && !skills.length ? 'Say what it should do' : !schedule ? 'Say when it runs' : ''

  const save = async () => {
    if (missing || saving) return setError(missing)
    setSaving(true)
    setError('')
    const fields = {
      name: name.trim(),
      prompt: prompt.trim(),
      deliver: deliver || 'local',
      skills,
      enabled_toolsets: toolsets && toolsets.length ? toolsets : null,
      model: model.trim() || null,
    }
    try {
      const saved = job
        ? await hermes<CronJob>(assistantId, 'PUT', jobPath(job.id), {
            updates: {
              ...fields,
              name: fields.name || job.name,
              ...(scheduleChanged ? { schedule } : {}),
            },
          })
        : await hermes<CronJob>(assistantId, 'POST', 'cron/jobs', { ...fields, schedule })
      if (job) putJob(qc, assistantId, saved)
      else
        qc.setQueryData<CronJob[]>(jobsQuery(assistantId).queryKey, (l) => (l ? [...l, saved] : l))
      toast(job ? `Saved “${saved.name}”` : `Scheduled “${saved.name}”`, { kind: 'success' })
      onSaved?.(saved)
      onClose()
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
      setSaving(false)
    }
  }

  const deliverOptions = targets ?? [
    { id: 'local', name: 'Local (save only)', home_target_set: true },
  ]
  const known = deliver.split(',').every((d) => deliverOptions.some((t) => t.id === d.trim()))

  return (
    <Dialog
      open
      wide
      onClose={onClose}
      title={job ? `Edit “${job.name}”` : 'New schedule'}
      footer={
        <>
          {error && (
            <p role="alert" className="mr-auto min-w-0 flex-1 text-[13px] text-danger">
              {error}
            </p>
          )}
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button variant="primary" loading={saving} onClick={save}>
            {job ? 'Save' : 'Schedule it'}
          </Button>
        </>
      }
    >
      <form
        className="space-y-4 p-4"
        onSubmit={(e) => {
          e.preventDefault()
          save()
        }}
        onKeyDown={(e) => {
          if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) {
            e.preventDefault()
            save()
          }
        }}
      >
        <Field label="What should it do?">
          <Textarea
            autoFocus={!job}
            autoGrow
            rows={4}
            value={prompt}
            onChange={(e) => setPrompt(e.target.value)}
            placeholder="Check my inbox and tell me about anything that needs a reply today."
            className="max-h-[40vh] min-h-24"
          />
        </Field>
        <ScheduleBuilder value={builder} onChange={setBuilder} zone={zone} />
        <div className="grid gap-4 sm:grid-cols-2">
          <Field label="Name">
            <Input
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder={prompt.trim() ? prompt.trim().slice(0, 50) : 'Morning briefing'}
            />
          </Field>
          <Field label="Send the result to">
            <Select value={deliver} onChange={(e) => setDeliver(e.target.value)}>
              {deliverOptions.map((t) => (
                <option key={t.id} value={t.id}>
                  {t.name}
                </option>
              ))}
              {!known && (
                <option value={deliver}>
                  {deliverLabel(deliver, new Map(deliverOptions.map((t) => [t.id, t.name])))}
                </option>
              )}
            </Select>
          </Field>
        </div>
        <div>
          <button
            type="button"
            aria-expanded={more}
            onClick={() => setMore((m) => !m)}
            className="-ml-1 flex h-8 items-center gap-1 rounded-md px-1 text-[13px] font-medium text-muted hover:text-fg"
          >
            <ChevronRight className={clsx('size-4 transition-transform', more && 'rotate-90')} />
            Skills, tools and model
            {!more && (skills.length > 0 || toolsets || model) && (
              <span className="ml-1 font-normal text-subtle">
                {[
                  skills.length && `${skills.length} skill${skills.length > 1 ? 's' : ''}`,
                  toolsets && `${toolsets.length} toolset${toolsets.length === 1 ? '' : 's'}`,
                  model,
                ]
                  .filter(Boolean)
                  .join(' · ')}
              </span>
            )}
          </button>
          {more && (
            <div className="mt-2 space-y-4 border-l-2 border-line pl-3 sm:pl-4">
              <SkillsField assistantId={assistantId} value={skills} onChange={setSkills} />
              <ToolsetsField assistantId={assistantId} value={toolsets} onChange={setToolsets} />
              <Field label="Model">
                <Input
                  value={model}
                  onChange={(e) => setModel(e.target.value)}
                  placeholder={modelInfo?.model ? `Default (${modelInfo.model})` : 'Default'}
                  className="font-mono text-[13px]"
                  spellCheck={false}
                  autoCapitalize="off"
                />
              </Field>
            </div>
          )}
        </div>
        <button type="submit" hidden />
      </form>
    </Dialog>
  )
}

const MODES: { value: Mode; label: string }[] = [
  { value: 'interval', label: 'Every…' },
  { value: 'daily', label: 'Daily' },
  { value: 'weekdays', label: 'Weekdays' },
  { value: 'weekly', label: 'Weekly' },
  { value: 'monthly', label: 'Monthly' },
  { value: 'once', label: 'Once' },
  { value: 'custom', label: 'Custom' },
]

const DAY_INITIAL = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat']

function ScheduleBuilder({
  value: b,
  onChange,
  zone,
}: {
  value: Builder
  onChange: (b: Builder) => void
  zone: Zone
}) {
  const set = (p: Partial<Builder>) => onChange({ ...b, ...p })
  const timeInput = (
    <Input
      type="time"
      aria-label="Time"
      required
      value={b.time}
      onChange={(e) => set({ time: e.target.value })}
      className="w-32"
    />
  )
  return (
    <div className="space-y-2.5">
      <div className="text-[13px] font-medium leading-5">When</div>
      <div role="radiogroup" aria-label="Repeats" className="flex flex-wrap gap-1.5">
        {MODES.map((m) => (
          <button
            key={m.value}
            type="button"
            role="radio"
            aria-checked={b.mode === m.value}
            onClick={() =>
              set({
                mode: m.value,
                ...(m.value === 'custom' && !b.raw ? { raw: buildSchedule(b) } : {}),
              })
            }
            className={clsx(
              'h-8 rounded-full border px-3 text-[13px] transition-colors max-sm:h-9',
              b.mode === m.value
                ? 'border-accent bg-accent-soft font-medium text-accent'
                : 'border-line bg-panel text-muted hover:bg-hover hover:text-fg',
            )}
          >
            {m.label}
          </button>
        ))}
      </div>
      <div className="flex flex-wrap items-center gap-2 text-[13px] text-muted">
        {b.mode === 'interval' && (
          <>
            <span>Every</span>
            <Input
              type="number"
              aria-label="How many"
              min={1}
              value={Number.isNaN(b.every) ? '' : b.every}
              onChange={(e) => set({ every: e.target.valueAsNumber })}
              className="w-20"
            />
            <div className="w-32">
              <Select
                aria-label="Unit"
                value={b.unit}
                onChange={(e) => set({ unit: e.target.value as Builder['unit'] })}
              >
                <option value="m">minutes</option>
                <option value="h">hours</option>
                <option value="d">days</option>
              </Select>
            </div>
          </>
        )}
        {(b.mode === 'daily' || b.mode === 'weekdays') && (
          <>
            <span>At</span>
            {timeInput}
          </>
        )}
        {b.mode === 'weekly' && (
          <>
            <div role="group" aria-label="Days" className="flex gap-1">
              {WEEK.map((d) => {
                const on = b.days.includes(d)
                return (
                  <button
                    key={d}
                    type="button"
                    aria-pressed={on}
                    aria-label={DAY_INITIAL[d]}
                    onClick={() =>
                      set({ days: on ? b.days.filter((x) => x !== d) : [...b.days, d] })
                    }
                    className={clsx(
                      'h-9 w-10 rounded-lg border text-xs font-medium transition-colors sm:w-11',
                      on
                        ? 'border-accent bg-accent text-accent-fg'
                        : 'border-line bg-panel text-muted hover:bg-hover',
                    )}
                  >
                    {DAY_INITIAL[d]}
                  </button>
                )
              })}
            </div>
            <span className="flex items-center gap-2">
              <span>at</span>
              {timeInput}
            </span>
          </>
        )}
        {b.mode === 'monthly' && (
          <>
            <span>On day</span>
            <Input
              type="number"
              aria-label="Day of the month"
              min={1}
              max={31}
              value={Number.isNaN(b.dom) ? '' : b.dom}
              onChange={(e) => set({ dom: e.target.valueAsNumber })}
              className="w-20"
            />
            <span>at</span>
            {timeInput}
          </>
        )}
        {b.mode === 'once' && (
          <Input
            type="datetime-local"
            aria-label="Date and time"
            value={b.at}
            onChange={(e) => set({ at: e.target.value })}
            className="w-auto max-w-full"
          />
        )}
        {b.mode === 'custom' && (
          <Input
            aria-label="Schedule"
            value={b.raw}
            onChange={(e) => set({ raw: e.target.value })}
            placeholder="0 9 * * 1-5 · every 2h · weekdays at 9am · in 30m"
            className="font-mono text-[13px]"
            spellCheck={false}
            autoCapitalize="off"
          />
        )}
      </div>
      <SchedulePreview text={buildSchedule(b)} zone={zone} />
    </div>
  )
}

/** What a schedule string means and when it next fires, as Hermes will read it. */
function SchedulePreview({ text, zone }: { text: string; zone: Zone }) {
  useTick(30_000)
  if (!text) return null
  const now = new Date()
  const p = parseSchedule(text, zone, now)
  const next = upcoming(p, zone, now, 3)
  const tz = zoneNote(zone, now)
  return (
    <div
      aria-live="polite"
      className={clsx(
        'flex items-start gap-2 rounded-lg px-3 py-2 text-[13px]',
        p.kind === 'error' ? 'bg-danger/10 text-danger' : 'bg-sunken',
      )}
    >
      <CalendarClock className="mt-0.5 size-4 shrink-0 opacity-70" />
      <div className="min-w-0">
        <div className="font-medium">
          {describe(p, zone)}
          {tz && p.kind !== 'error' && (
            <span className="font-normal text-subtle"> · {tz} time</span>
          )}
        </div>
        {next.length > 0 && p.kind !== 'once' && (
          <div className="text-muted">Next: {next.map((d) => wallLabel(d, zone)).join(' · ')}</div>
        )}
      </div>
    </div>
  )
}

function SkillsField({
  assistantId,
  value,
  onChange,
}: {
  assistantId: number
  value: string[]
  onChange: (v: string[]) => void
}) {
  const skills = useQuery(skillsQuery(assistantId))
  const pop = usePopover()
  const items = (skills.data ?? []).map((s) => ({
    value: s.name,
    label: s.name,
    hint: s.category.replace(/-/g, ' '),
    keywords: `${s.description} ${s.category}`,
  }))
  return (
    <ChipsField
      label="Skills"
      empty="None: the agent picks its own"
      chips={value}
      onRemove={(n) => onChange(value.filter((x) => x !== n))}
      add={
        <>
          <Button
            ref={pop.anchor}
            size="sm"
            variant="ghost"
            icon={<Plus className="size-3.5" />}
            onClick={pop.toggle}
            loading={skills.isPending && pop.open}
          >
            Add
          </Button>
          <Popover open={pop.open} onClose={pop.close} anchor={pop.anchor}>
            <Picker
              multi
              items={items}
              selected={value}
              placeholder="Find a skill…"
              onPick={(v, on) => onChange(on ? [...value, v] : value.filter((x) => x !== v))}
              onClose={pop.close}
            />
          </Popover>
        </>
      }
    />
  )
}

function ToolsetsField({
  assistantId,
  value,
  onChange,
}: {
  assistantId: number
  value: string[] | null
  onChange: (v: string[] | null) => void
}) {
  const sets = useQuery(toolsetsQuery(assistantId))
  const pop = usePopover()
  const label = (n: string) => sets.data?.find((s) => s.name === n)?.label ?? n
  const items = (sets.data ?? []).map((s) => ({
    value: s.name,
    label: s.label || s.name,
    hint: s.enabled ? undefined : 'off',
    keywords: `${s.name} ${s.description}`,
  }))
  const list = value ?? []
  return (
    <ChipsField
      label="Tools"
      empty="All the tools it normally has"
      chips={list}
      chipLabel={label}
      onRemove={(n) => {
        const next = list.filter((x) => x !== n)
        onChange(next.length ? next : null)
      }}
      add={
        <>
          <Button
            ref={pop.anchor}
            size="sm"
            variant="ghost"
            icon={<Plus className="size-3.5" />}
            onClick={pop.toggle}
          >
            {value ? 'Add' : 'Only some'}
          </Button>
          <Popover open={pop.open} onClose={pop.close} anchor={pop.anchor}>
            <Picker
              multi
              items={items}
              selected={list}
              placeholder="Find a toolset…"
              onPick={(v, on) => {
                const next = on ? [...list, v] : list.filter((x) => x !== v)
                onChange(next.length ? next : null)
              }}
              onClose={pop.close}
            />
          </Popover>
        </>
      }
    />
  )
}

function ChipsField({
  label,
  empty,
  chips,
  chipLabel = (s) => s,
  onRemove,
  add,
}: {
  label: string
  empty: string
  chips: string[]
  chipLabel?: (s: string) => string
  onRemove: (s: string) => void
  add: React.ReactNode
}) {
  return (
    <div role="group" aria-label={label} className="space-y-1.5">
      <div className="text-[13px] font-medium leading-5">{label}</div>
      <div className="flex flex-wrap items-center gap-1.5">
        {chips.length === 0 && <span className="text-[13px] text-subtle">{empty}</span>}
        {chips.map((c) => (
          <span
            key={c}
            className="inline-flex h-7 items-center gap-1 rounded-md bg-sunken pl-2 pr-0.5 text-[13px]"
          >
            {chipLabel(c)}
            <button
              type="button"
              aria-label={`Remove ${chipLabel(c)}`}
              onClick={() => onRemove(c)}
              className="flex size-6 items-center justify-center rounded text-subtle hover:bg-hover hover:text-fg"
            >
              <X className="size-3.5" />
            </button>
          </span>
        ))}
        {add}
      </div>
    </div>
  )
}
