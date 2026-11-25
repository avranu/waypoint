import { useMemo, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import {
  CircleCheck,
  Hourglass,
  ListPlus,
  MessageSquareText,
  Play,
  Sparkles,
  X,
} from 'lucide-react'
import { api } from '@/lib/api'
import { catalogQuery, runActive, type Catalog } from '@/lib/automations'
import { q } from '@/lib/queries'
import type { AgentSession, FollowUp } from '@/lib/sessions'
import type { Action, Project } from '@/lib/types'
import { Button } from '@/components/ui/button'
import { Confirm, Dialog } from '@/components/ui/dialog'
import { Section, Skeleton } from '@/components/ui/misc'
import { toast, toastError } from '@/components/ui/toast'
import {
  addStep,
  defaultAction,
  draftFromRule,
  draftProblems,
  dropOn,
  flatten,
  mapSteps,
  maxSteps,
  removeStep,
  reorder,
  stepIDs,
  toRule,
  wrapStep,
  type DStep,
} from '../project-settings/automation-draft'
import type { FieldEnv } from '../project-settings/automation-fields'
import {
  AddStep,
  StepsProvider,
  StepsSection,
  type StepsApi,
} from '../project-settings/AutomationSteps'
import { RunnerNote } from '../project-settings/AutomationEditor'
import { RunBadge, StepsView } from '../project-settings/AutomationRuns'
import { describeAction } from '../project-settings/rules'

const TRIGGER = 'session_finished'

/** The steps most people queue, offered first. */
const QUICK: { type: string; label: string; icon: typeof Sparkles }[] = [
  { type: 'start_session', label: 'Start a fresh agent', icon: Sparkles },
  { type: 'tell_agent', label: 'Tell it what’s next', icon: MessageSquareText },
]

export function FollowUpPanel({ session: s }: { session: AgentSession }) {
  const qc = useQueryClient()
  const { data: fu } = useQuery(q.followUp(s.id))
  const cat = useQuery(catalogQuery()).data
  const project = useQuery({ ...q.project(s.project?.key ?? ''), enabled: !!s.project }).data
  const [editing, setEditing] = useState<Action[] | null>(null)
  const [clearing, setClearing] = useState(false)
  const refresh = () => {
    qc.invalidateQueries({ queryKey: ['followUp', s.id] })
    qc.invalidateQueries({ queryKey: ['session', s.id] })
    qc.invalidateQueries({ queryKey: ['sessions'] })
  }
  const runNow = useMutation({
    mutationFn: () => api.post<FollowUp>(`/sessions/${s.id}/follow-up/run`),
    onSuccess: (f) => {
      qc.setQueryData(q.followUp(s.id).queryKey, f)
      refresh()
      toast('Running the steps now')
    },
    onError: toastError,
  })
  const clear = useMutation({
    mutationFn: () => api.del(`/sessions/${s.id}/follow-up`),
    onSuccess: () => {
      refresh()
      setClearing(false)
    },
    onError: toastError,
  })
  const cancel = useMutation({
    mutationFn: (run: number) => api.post(`/automation-runs/${run}/cancel`),
    onSuccess: refresh,
    onError: toastError,
  })

  const start = (types: string[]) => {
    if (!cat) return
    setEditing(
      types.flatMap((t) => {
        const def = cat.actions.find((a) => a.name === t)
        return def ? [defaultAction(def)] : []
      }),
    )
  }
  const statuses = project?.statuses ?? []
  const run = fu?.run
  const active = !!run && runActive(run.status)

  return (
    <Section
      title="When it finishes"
      help={
        <>
          Steps that run once this agent <b>truly</b> finishes: not while it still runs a background
          task, waits on a wakeup or cron it set, a permission prompt or a question, or a request it
          filed. They run once, as you, on your runners. To start over with another model, queue{' '}
          <i>Start a fresh agent</i>: your runner on {s.machine} starts a new Claude Code session in
          the same folder, which you open with <code>claude attach</code>.
        </>
      }
      actions={
        fu &&
        fu.state !== 'none' && (
          <div className="flex items-center gap-1">
            {fu.state === 'armed' && (
              <Button
                size="xs"
                variant="ghost"
                icon={<Play className="size-3.5" />}
                loading={runNow.isPending}
                onClick={() => runNow.mutate()}
              >
                Run now
              </Button>
            )}
            {active && run ? (
              <Button
                size="xs"
                variant="ghost"
                icon={<X className="size-3.5" />}
                loading={cancel.isPending}
                onClick={() => cancel.mutate(run.id)}
              >
                Cancel run
              </Button>
            ) : (
              <>
                <Button size="xs" variant="ghost" onClick={() => setEditing(fu.steps)}>
                  {fu.state === 'armed' ? 'Edit' : 'Queue again'}
                </Button>
                <Button size="xs" variant="ghost" onClick={() => setClearing(true)}>
                  Clear
                </Button>
              </>
            )}
          </div>
        )
      }
    >
      <div className="space-y-3 px-4 py-3" data-testid="follow-up">
        {!fu ? (
          <Skeleton className="h-10" />
        ) : fu.state === 'none' ? (
          <>
            <p className="text-[13px] text-muted">
              Queue what happens once this agent is done, so you don’t have to wait for it.
            </p>
            <div className="flex flex-wrap gap-2">
              {QUICK.map((x) => (
                <Button
                  key={x.type}
                  size="sm"
                  icon={<x.icon className="size-3.5" />}
                  disabled={!cat}
                  onClick={() => start([x.type])}
                >
                  {x.label}
                </Button>
              ))}
              <Button
                size="sm"
                variant="ghost"
                icon={<ListPlus className="size-3.5" />}
                disabled={!cat}
                onClick={() => setEditing([])}
              >
                Other steps…
              </Button>
            </div>
          </>
        ) : fu.state === 'armed' ? (
          <>
            <p className="flex items-start gap-1.5 text-[13px] text-muted">
              <Hourglass className="mt-0.5 size-3.5 shrink-0 text-warn" />
              <span>{fu.held || 'Waits for the agent to finish.'}</span>
            </p>
            <ol className="list-decimal space-y-1 pl-5 text-[13px]">
              {fu.steps.map((a, i) => (
                <li key={i}>{describeAction(a, statuses, cat)}</li>
              ))}
            </ol>
          </>
        ) : run ? (
          <>
            <div className="flex flex-wrap items-center gap-2 text-[13px] text-muted">
              <RunBadge status={run.status} />
              <span>{run.summary}</span>
              {run.error && !run.steps?.length && <span className="text-danger">{run.error}</span>}
            </div>
            {run.steps && run.steps.length > 0 && <StepsView steps={run.steps} rule={run.rule} />}
          </>
        ) : (
          <p className="flex items-center gap-1.5 text-[13px] text-muted">
            <CircleCheck className="size-3.5 text-ok" /> Ran.
          </p>
        )}
      </div>
      {editing && cat && (
        <FollowUpEditor
          session={s}
          cat={cat}
          project={project}
          initial={editing}
          onClose={() => setEditing(null)}
          onSaved={(f) => {
            qc.setQueryData(q.followUp(s.id).queryKey, f)
            refresh()
            setEditing(null)
            if (f.state === 'running') toast('The agent had already finished: the steps run now')
          }}
        />
      )}
      <Confirm
        open={clearing}
        onClose={() => setClearing(false)}
        onConfirm={() => clear.mutate()}
        title="Clear the queued steps?"
        body={active ? 'Its run is cancelled too.' : undefined}
        confirmLabel="Clear"
      />
    </Section>
  )
}

function FollowUpEditor({
  session: s,
  cat,
  project,
  initial,
  onClose,
  onSaved,
}: {
  session: AgentSession
  cat: Catalog
  project?: Project
  initial: Action[]
  onClose: () => void
  onSaved: (f: FollowUp) => void
}) {
  const [d, setD] = useState(() =>
    draftFromRule('', true, { trigger: { event: TRIGGER }, actions: initial }),
  )
  const [tried, setTried] = useState(false)
  const [error, setError] = useState('')
  const problems = useMemo(() => draftProblems(d, cat), [d, cat])
  const ids = useMemo(() => stepIDs(d.steps), [d.steps])
  const statuses = useMemo(
    () => [...(project?.statuses ?? [])].sort((a, b) => a.position - b.position),
    [project?.statuses],
  )
  const env: FieldEnv = {
    catalog: cat,
    project,
    statuses,
    stepNames: [],
    timeZone: d.timeZone,
    openQueryHelp: () => {},
  }
  const api_: StepsApi = {
    cat,
    env,
    trigger: cat.triggers.find((t) => t.name === TRIGGER),
    steps: d.steps,
    inputs: d.inputs,
    ids,
    problems: problems.steps,
    tried,
    change: (id, fn) => setD((x) => ({ ...x, steps: mapSteps(x.steps, id, fn) })),
    remove: (id) => setD((x) => ({ ...x, steps: removeStep(x.steps, id) })),
    move: (id, by) => setD((x) => ({ ...x, steps: reorder(x.steps, id, (i) => i + by) })),
    dropOn: (a, o) => setD((x) => ({ ...x, steps: dropOn(x.steps, a, o) })),
    add: (parent, type) => {
      const def = cat.actions.find((a) => a.name === type)
      if (def)
        setD((x) => ({ ...x, steps: addStep(x.steps, parent, wrapStep(defaultAction(def))) }))
    },
  }
  const save = useMutation({
    mutationFn: () =>
      api.put<FollowUp>(`/sessions/${s.id}/follow-up`, { steps: toRule(d, cat).actions }),
    onSuccess: onSaved,
    onError: (e) => {
      setError(e instanceof Error ? e.message : 'Couldn’t save')
      toastError(e)
    },
  })
  const submit = () => {
    setTried(true)
    if (d.steps.length > 0 && !problems.any) save.mutate()
  }
  const onRunner = flatten(d.steps).some(
    (st: DStep) => cat.actions.find((a) => a.name === st.a.type)?.runner,
  )
  return (
    <Dialog
      open
      onClose={onClose}
      wide
      title="When this agent finishes"
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button variant="primary" loading={save.isPending} onClick={submit}>
            Queue
          </Button>
        </>
      }
    >
      <form
        className="space-y-3 p-4"
        onSubmit={(e) => {
          e.preventDefault()
          submit()
        }}
      >
        <p className="text-[13px] text-muted">
          Runs once, when the agent on <b>{s.machine}</b> has truly finished: no background task,
          wakeup or open request left. <i>Start a fresh agent</i> is <code>/clear</code>, a new
          model and a prompt, in a new session in {s.cwd ? <code>{s.cwd}</code> : 'its folder'}.
        </p>
        <StepsProvider value={api_}>
          <StepsSection />
          {d.steps.length === 0 && tried && (
            <p className="text-xs text-danger">Add at least one step.</p>
          )}
          {d.steps.length < maxSteps && (
            <div className="flex flex-wrap items-center gap-2 sm:pl-16">
              {d.steps.length === 0 &&
                QUICK.map((x) => (
                  <Button
                    key={x.type}
                    size="xs"
                    icon={<x.icon className="size-3.5" />}
                    onClick={() => api_.add(null, x.type)}
                  >
                    {x.label}
                  </Button>
                ))}
              <AddStep first={d.steps.length === 0} onAdd={(type) => api_.add(null, type)} />
            </div>
          )}
        </StepsProvider>
        {onRunner && <RunnerNote automation={null} />}
        {error && (
          <p role="alert" className="rounded-lg bg-danger/10 px-3 py-2 text-[13px] text-danger">
            {error}
          </p>
        )}
      </form>
    </Dialog>
  )
}
