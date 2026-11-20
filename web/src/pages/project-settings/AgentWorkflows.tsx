// Workflows: the ordered steps an issue moves through (plan → implement →
// review…), each optionally for a particular agent. Shared ones are
// available in every project; admins edit those, owners edit their own.

import { useEffect, useMemo, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import {
  ArrowDown,
  ArrowUp,
  Bot,
  Pencil,
  Plus,
  Trash2,
  Workflow as WorkflowIcon,
  X,
} from 'lucide-react'
import { api } from '@/lib/api'
import { q } from '@/lib/queries'
import { AGENT_SUGGESTIONS, workflowPath, workflowsQuery } from '@/lib/agents'
import { Button, IconButton } from '@/components/ui/button'
import { Confirm, Dialog } from '@/components/ui/dialog'
import { Field, Input, Select, Textarea } from '@/components/ui/input'
import { Badge, Empty, LoadError } from '@/components/ui/misc'
import { toastError } from '@/components/ui/toast'
import type { Project, Workflow, WorkflowStep } from '@/lib/types'
import { List, ReadOnlyNote, TabIntro } from './shared'

export function AgentWorkflowsTab({ project }: { project: Project }) {
  const qc = useQueryClient()
  const admin = !!useQuery(q.me()).data?.is_admin
  const owner = project.role === 'owner' || admin
  const workflows = useQuery(workflowsQuery(project.key))
  const [editing, setEditing] = useState<Workflow | 'new' | 'new-shared' | null>(null)
  const [deleting, setDeleting] = useState<Workflow | null>(null)
  const refresh = () => qc.invalidateQueries({ queryKey: ['workflows'] })
  const del = useMutation({
    mutationFn: (id: number) => api.del(workflowPath(id)),
    onSettled: refresh,
    onError: toastError,
  })
  const list = workflows.data ?? []
  const mayEdit = (w: Workflow) => (w.project_id === null ? admin : owner)
  return (
    <div className="space-y-5">
      <TabIntro
        title="Workflows"
        actions={
          owner && (
            <Button
              size="sm"
              variant="primary"
              icon={<Plus className="size-3.5" />}
              onClick={() => setEditing('new')}
            >
              New workflow
            </Button>
          )
        }
      >
        Steps an issue goes through, like plan → implement → review. A step can be limited to
        certain agents, or to a <b>different</b> agent than the step before, so work gets a second
        pair of eyes. Agents move issues on as they finish each step.
      </TabIntro>
      {!owner && <ReadOnlyNote>Only project owners can change workflows.</ReadOnlyNote>}
      {workflows.isError ? (
        <LoadError what="Workflows" error={workflows.error} />
      ) : list.length === 0 ? (
        !workflows.isPending && <Empty icon={<WorkflowIcon />} title="No workflows yet" />
      ) : (
        <List>
          {list.map((w) => (
            <li key={w.id} className="flex items-center gap-3 px-4 py-3">
              <WorkflowIcon className="size-4 shrink-0 text-muted" />
              <div className="min-w-0 flex-1">
                <div className="flex items-center gap-2 text-[13px] font-medium">
                  <span className="truncate">{w.name}</span>
                  {w.project_id === null && <Badge>Shared</Badge>}
                </div>
                <div className="truncate text-xs text-subtle">
                  {w.steps.map((s) => s.name).join(' → ') || 'No steps'}
                </div>
              </div>
              {mayEdit(w) && (
                <>
                  <IconButton
                    label={`Edit ${w.name}`}
                    onClick={() => setEditing(w)}
                    icon={<Pencil className="size-3.5" />}
                  />
                  <IconButton
                    label={`Delete ${w.name}`}
                    onClick={() => setDeleting(w)}
                    icon={<Trash2 className="size-3.5" />}
                  />
                </>
              )}
            </li>
          ))}
        </List>
      )}
      {admin && (
        <Button
          size="sm"
          variant="ghost"
          icon={<Plus className="size-3.5" />}
          onClick={() => setEditing('new-shared')}
        >
          New shared workflow (every project)
        </Button>
      )}
      <WorkflowDialog
        project={project}
        workflow={editing}
        onClose={() => setEditing(null)}
        onSaved={refresh}
      />
      <Confirm
        open={!!deleting}
        onClose={() => setDeleting(null)}
        title={`Delete the ${deleting?.name} workflow?`}
        body="Issues on it keep their current step name but leave the workflow."
        confirmLabel="Delete"
        onConfirm={() => deleting && del.mutate(deleting.id)}
      />
    </div>
  )
}

const blankStep = (): WorkflowStep => ({ name: '', agent: '', instructions: '' })

function WorkflowDialog({
  project,
  workflow,
  onClose,
  onSaved,
}: {
  project: Project
  workflow: Workflow | 'new' | 'new-shared' | null
  onClose: () => void
  onSaved: () => void
}) {
  const existing = workflow && typeof workflow === 'object' ? workflow : null
  const [name, setName] = useState('')
  const [steps, setSteps] = useState<WorkflowStep[]>([])
  useEffect(() => {
    if (!workflow) return
    setName(existing?.name ?? '')
    setSteps(existing?.steps.map((s) => ({ ...s })) ?? [blankStep(), blankStep()])
  }, [workflow]) // eslint-disable-line react-hooks/exhaustive-deps
  const save = useMutation({
    mutationFn: () => {
      const body = { name: name.trim(), steps: steps.filter((s) => s.name.trim()) }
      return existing
        ? api.patch(workflowPath(existing.id), body)
        : api.post('/workflows', { ...body, project: workflow === 'new' ? project.key : '' })
    },
    onSuccess: () => {
      onSaved()
      onClose()
    },
    onError: toastError,
  })
  const tokens = useQuery({ ...q.tokens(), enabled: !!workflow }).data
  const suggestions = useMemo(
    () => [
      ...new Set([
        'any',
        ...AGENT_SUGGESTIONS,
        ...(tokens ?? []).flatMap((t) => (t.agent ? [t.agent] : [])),
      ]),
    ],
    [tokens],
  )
  const set = (i: number, patch: Partial<WorkflowStep>) =>
    setSteps((all) => all.map((s, j) => (j === i ? { ...s, ...patch } : s)))
  const move = (i: number, d: -1 | 1) =>
    setSteps((all) => {
      const next = [...all]
      ;[next[i], next[i + d]] = [next[i + d], next[i]]
      return next
    })
  const shared = workflow === 'new-shared' || existing?.project_id === null
  const valid = !!name.trim() && steps.some((s) => s.name.trim())
  return (
    <Dialog
      open={!!workflow}
      onClose={onClose}
      title={existing ? `Edit ${existing.name}` : shared ? 'New shared workflow' : 'New workflow'}
      wide
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button
            variant="primary"
            disabled={!valid}
            loading={save.isPending}
            onClick={() => save.mutate()}
          >
            Save workflow
          </Button>
        </>
      }
    >
      <form
        className="space-y-4 p-4"
        onSubmit={(e) => {
          e.preventDefault()
          if (valid) save.mutate()
        }}
      >
        {shared && (
          <p className="rounded-lg bg-sunken px-3 py-2 text-xs text-muted">
            Shared: available in every project. Changes apply everywhere.
          </p>
        )}
        <Field label="Name">
          <Input
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder="Agent pipeline"
            autoFocus
          />
        </Field>
        <div>
          <div className="mb-1.5 text-[13px] font-medium">Steps</div>
          <ol className="space-y-2">
            {steps.map((s, i) => (
              <li key={i} className="rounded-lg border border-line p-3">
                <div className="flex items-center gap-2">
                  <span className="flex size-5 shrink-0 items-center justify-center rounded-full bg-sunken text-[11px] font-semibold text-muted">
                    {i + 1}
                  </span>
                  <Input
                    value={s.name}
                    onChange={(e) => set(i, { name: e.target.value })}
                    placeholder={['Plan', 'Implement', 'Review', 'Test'][i] ?? 'Step name'}
                    aria-label={`Step ${i + 1} name`}
                    className="flex-1"
                  />
                  <IconButton
                    label="Move up"
                    size="xs"
                    disabled={i === 0}
                    onClick={() => move(i, -1)}
                    icon={<ArrowUp className="size-3.5" />}
                  />
                  <IconButton
                    label="Move down"
                    size="xs"
                    disabled={i === steps.length - 1}
                    onClick={() => move(i, 1)}
                    icon={<ArrowDown className="size-3.5" />}
                  />
                  <IconButton
                    label={`Remove step ${i + 1}`}
                    size="xs"
                    onClick={() => setSteps((all) => all.filter((_, j) => j !== i))}
                    icon={<X className="size-3.5" />}
                  />
                </div>
                <div className="mt-2 grid gap-2 sm:grid-cols-[17rem_1fr] sm:pl-7">
                  <StepAgent
                    step={i + 1}
                    value={s.agent ?? ''}
                    onChange={(agent) => set(i, { agent })}
                  />
                  <Textarea
                    value={s.instructions ?? ''}
                    onChange={(e) => set(i, { instructions: e.target.value })}
                    rows={2}
                    autoGrow
                    className="resize-none overflow-hidden"
                    placeholder="What to do in this step, for the agent or person taking it"
                    aria-label={`Step ${i + 1} instructions`}
                  />
                </div>
              </li>
            ))}
          </ol>
          <datalist id="wf-agents">
            {suggestions.map((a) => (
              <option key={a} value={a} />
            ))}
          </datalist>
          <Button
            size="sm"
            variant="ghost"
            className="mt-2"
            icon={<Plus className="size-3.5" />}
            onClick={() => setSteps((all) => [...all, blankStep()])}
          >
            Add step
          </Button>
        </div>
      </form>
    </Dialog>
  )
}

/** Who takes a step: the issue's own agent, a different or the same one as
 * the step before, or agents matching a spec (a family or model). */
const RELATIVE = ['', 'different', 'same']

function StepAgent({
  step,
  value,
  onChange,
}: {
  step: number
  value: string
  onChange: (v: string) => void
}) {
  // Emptying the spec field shouldn't snap it shut mid-edit.
  const [typing, setTyping] = useState(false)
  const v = value.trim().toLowerCase()
  const specific = typing || !RELATIVE.includes(v)
  return (
    <div className="flex flex-col gap-2 self-start">
      <Select
        value={specific ? 'specific' : v}
        onChange={(e) => {
          setTyping(false)
          onChange(e.target.value === 'specific' ? 'any' : e.target.value)
        }}
        aria-label={`Step ${step} agent`}
        icon={<Bot className="size-3.5 shrink-0 text-subtle" />}
      >
        <option value="">Issue's agent</option>
        <option value="different">A different agent than before</option>
        <option value="same">The same agent as before</option>
        <option value="specific">Specific agents…</option>
      </Select>
      {specific && (
        <Input
          value={value}
          onChange={(e) => {
            setTyping(e.target.value.trim() === '')
            onChange(e.target.value)
          }}
          onBlur={() => setTyping(false)}
          placeholder="any, claude, haiku 5.5…"
          aria-label={`Step ${step} agent spec`}
          list="wf-agents"
        />
      )}
    </div>
  )
}
