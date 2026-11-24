import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { RotateCcw } from 'lucide-react'
import { invalidateProject, q } from '@/lib/queries'
import {
  samePolicy,
  saveProjectStallPolicy,
  saveStallPolicy,
  stallPolicyError,
  type StallPolicy,
} from '@/lib/stalled-jobs'
import { Button } from '@/components/ui/button'
import { Field, Input, Switch } from '@/components/ui/input'
import { Section, Skeleton } from '@/components/ui/misc'
import { SavedMark, useAutosave, useSent } from '@/components/ui/autosave'
import { toastError } from '@/components/ui/toast'

const Code = ({ children }: { children: string }) => <code className="font-mono">{children}</code>

const help = (
  <>
    When an agent’s background job (a build, a test run, a wait on CI) has printed nothing for a
    while, its session is told, so a stuck job doesn’t hold up the task. While the session’s prompt
    cache is warm it hears at once, or in place of a keep-warm ping that was due anyway; once the
    cache is cold the notice waits for its next turn, unless the job stays stuck past the wake time.
    Unless the times are fixed, Waypoint lengthens the stall time for kinds of jobs that are often
    quiet and fine, and stops waking cold sessions when those wakes aren’t acted on (
    <Code>wp usage stalled</Code> shows the figures).
  </>
)

/** Admin → AI: the times for every project. */
export function StalledJobsSection() {
  const qc = useQueryClient()
  const pol = useQuery(q.stallPolicy())
  const save = useMutation({
    mutationFn: (v: StallPolicy | null) => saveStallPolicy(v),
    onSuccess: (r) => {
      qc.setQueryData(q.stallPolicy().queryKey, r)
      void qc.invalidateQueries({ queryKey: ['stalled-jobs', 'policy'] })
    },
    onError: toastError,
  })
  return (
    <Section title="Stalled background jobs" help={help} className="mt-4">
      {pol.isPending ? (
        <div className="p-4">
          <Skeleton className="h-40" />
        </div>
      ) : !pol.data ? null : (
        <StallForm
          current={pol.data.policy}
          fallback={pol.data.default}
          custom={pol.data.custom}
          resetLabel="Use the default"
          onSave={(v) => save.mutateAsync(v)}
        />
      )}
    </Section>
  )
}

/** A project's settings: its own times, over every project's. */
export function ProjectStalledJobsSection({
  projectKey,
  canEdit,
}: {
  projectKey: string
  canEdit: boolean
}) {
  const qc = useQueryClient()
  const pol = useQuery(q.projectStallPolicy(projectKey))
  const save = useMutation({
    mutationFn: (v: StallPolicy | null) => saveProjectStallPolicy(projectKey, v),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: q.projectStallPolicy(projectKey).queryKey })
      invalidateProject(qc, projectKey)
    },
    onError: toastError,
  })
  const d = pol.data
  return (
    <Section
      title="Stalled background jobs"
      help={<>{help} Unchanged, this project uses the times for every project (Admin → AI).</>}
    >
      {pol.isPending ? (
        <div className="p-4">
          <Skeleton className="h-40" />
        </div>
      ) : !d ? null : (
        <StallForm
          current={d.policy}
          fallback={d.inherited ?? d.default}
          custom={d.custom}
          resetLabel="Use every project’s"
          disabled={!canEdit}
          onSave={(v) => save.mutateAsync(v)}
        />
      )}
    </Section>
  )
}

type Minutes = 'min_age' | 'quiet' | 'skip_timeout' | 'early' | 'cold_wake'
type Draft = Omit<StallPolicy, Minutes> & Record<Minutes, string>

const toDraft = (p: StallPolicy): Draft => ({
  ...p,
  min_age: String(p.min_age),
  quiet: String(p.quiet),
  skip_timeout: String(p.skip_timeout),
  early: String(p.early),
  cold_wake: String(p.cold_wake),
})

/** The same text for the same times, whatever order the fields came in. */
const canon = (p: StallPolicy | null) =>
  p === null
    ? 'null'
    : JSON.stringify([p.off, p.min_age, p.quiet, p.skip_timeout, p.early, p.cold_wake, p.fixed])

const num = (s: string) => (s.trim() === '' ? NaN : Number(s))

const fromDraft = (d: Draft): StallPolicy => ({
  off: d.off,
  fixed: d.fixed,
  min_age: num(d.min_age),
  quiet: num(d.quiet),
  skip_timeout: num(d.skip_timeout),
  early: num(d.early),
  cold_wake: num(d.cold_wake),
})

const FIELDS: { key: Minutes; label: string; note: string }[] = [
  {
    key: 'quiet',
    label: 'Stall after',
    note: 'Minutes without output before a job counts as stuck.',
  },
  { key: 'min_age', label: 'Jobs at least', note: 'Minutes old; younger jobs are left alone.' },
  {
    key: 'early',
    label: 'Early, at a keep-warm ping',
    note: 'Minutes quiet to tell the session in place of a ping that was due. 0: never early.',
  },
  {
    key: 'cold_wake',
    label: 'Wake a cold session after',
    note: 'Minutes without output; until then it hears at its next turn. 0: never woken.',
  },
  {
    key: 'skip_timeout',
    label: 'Skip timeouts under',
    note: 'Minutes; a job under a shorter timeout ends by itself. 0: skip none.',
  },
]

/**
 * current is what applies; fallback is what applies without the times saved
 * here (the default, or every project's), so times equal to it are saved as
 * none. No <form>: a project's settings already sit in one.
 */
function StallForm({
  current,
  fallback,
  custom,
  resetLabel,
  disabled,
  onSave,
}: {
  current: StallPolicy
  fallback: StallPolicy
  custom: boolean
  resetLabel: string
  disabled?: boolean
  onSave: (v: StallPolicy | null) => Promise<unknown>
}) {
  const [v, setV] = useState<Draft>(() => toDraft(current))
  const [seen, setSeen] = useState(canon(current))
  const sent = useSent()
  if (seen !== canon(current)) {
    setSeen(canon(current))
    if (!sent.mine(canon(current))) setV(toDraft(current))
  }
  const policy = fromDraft(v)
  const error = stallPolicyError(policy)
  const store = (p: StallPolicy) => (samePolicy(p, fallback) ? null : p)
  const next = store(policy)
  const changed = !samePolicy(policy, current)
  const { done: saved } = useAutosave(
    canon(next),
    canon(store(current)),
    !error && !disabled,
    900,
    () => {
      sent.add(canon(policy))
      return onSave(next)
    },
  )
  const set = (k: keyof Draft, val: string | boolean) => setV((d) => ({ ...d, [k]: val }))
  return (
    <div className="space-y-4 px-4 py-3">
      <Row
        label="Tell sessions about stalled jobs"
        note="Off, nobody is told; runners still report the jobs, so the figures are kept."
      >
        <Switch
          checked={!v.off}
          onChange={(on) =>
            setV((d) => {
              const n = { ...d, off: !on }
              return !on && stallPolicyError(fromDraft(n))
                ? { ...toDraft(current), off: true, fixed: d.fixed }
                : n
            })
          }
          label="Tell sessions about stalled jobs"
          disabled={disabled}
        />
      </Row>
      {!v.off && (
        <>
          <div className="grid gap-4 sm:grid-cols-2">
            {FIELDS.map((f) => (
              <Field key={f.key} label={f.label} note={f.note}>
                <Input
                  type="number"
                  inputMode="numeric"
                  min={0}
                  max={1440}
                  step={1}
                  value={v[f.key]}
                  disabled={disabled}
                  onChange={(e) => set(f.key, e.target.value)}
                />
              </Field>
            ))}
          </div>
          {error && (
            <p role="alert" className="text-xs text-danger">
              {error}
            </p>
          )}
          <Row
            label="Fixed times"
            note="Use these times as they are: the evidence doesn’t lengthen the stall time or stop cold wakes."
          >
            <Switch
              checked={v.fixed}
              onChange={(on) => set('fixed', on)}
              label="Fixed times"
              disabled={disabled}
            />
          </Row>
        </>
      )}
      {!disabled && (
        <div className="flex flex-wrap items-center gap-2">
          <SavedMark show={saved} />
          {changed ? (
            <Button
              type="button"
              size="sm"
              variant="ghost"
              icon={<RotateCcw className="size-3.5" />}
              onClick={() => setV(toDraft(current))}
            >
              Undo
            </Button>
          ) : (
            custom && (
              <Button
                type="button"
                size="sm"
                variant="ghost"
                icon={<RotateCcw className="size-3.5" />}
                onClick={() => setV(toDraft(fallback))}
              >
                {resetLabel}
              </Button>
            )
          )}
        </div>
      )}
    </div>
  )
}

function Row({
  label,
  note,
  children,
}: {
  label: string
  note: string
  children: React.ReactNode
}) {
  return (
    <div className="flex items-start justify-between gap-4">
      <div>
        <div className="text-[13px] font-medium leading-5">{label}</div>
        <p className="text-xs text-subtle">{note}</p>
      </div>
      {children}
    </div>
  )
}
