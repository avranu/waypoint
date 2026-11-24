import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { RotateCcw } from 'lucide-react'
import { invalidateProject, q } from '@/lib/queries'
import {
  promptNamesTask,
  renderAutopilotDone,
  renderAutopilotPrompt,
  saveAutopilotText,
  saveProjectAutopilotText,
  type AutopilotText,
} from '@/lib/autopilot-text'
import { Button } from '@/components/ui/button'
import { Field, Textarea } from '@/components/ui/input'
import { Section, Skeleton } from '@/components/ui/misc'
import { SavedMark, useAutosave, useSent } from '@/components/ui/autosave'
import { toastError } from '@/components/ui/toast'

const Code = ({ children }: { children: string }) => <code className="font-mono">{children}</code>

/** Admin → AI: the wording for every project. */
export function AutopilotTextSection() {
  const qc = useQueryClient()
  const text = useQuery(q.autopilotText())
  const save = useMutation({
    mutationFn: (v: { prompt: string; done: string }) => saveAutopilotText(v),
    onSuccess: (r) => {
      qc.setQueryData(q.autopilotText().queryKey, r)
      void qc.invalidateQueries({ queryKey: ['autopilot', 'text'] })
    },
    onError: toastError,
  })
  return (
    <Section
      title="Autopilot’s wording"
      help={
        <>
          What autopilot says to a session: the prompt it pastes when it starts the next task there,
          and the note under the turn that finished one. A project’s owners can word their own under
          its settings.
        </>
      }
      className="mt-4"
    >
      {text.isPending ? (
        <div className="p-4">
          <Skeleton className="h-40" />
        </div>
      ) : !text.data ? null : (
        <WordingForm
          current={{ prompt: text.data.prompt, done: text.data.done }}
          fallback={{ prompt: text.data.default_prompt, done: text.data.default_done }}
          custom={text.data.custom_prompt || text.data.custom_done}
          resetLabel="Use the default"
          onSave={(v) => save.mutateAsync(v)}
        />
      )}
    </Section>
  )
}

/** A project's settings: its own wording, over every project's. */
export function ProjectAutopilotTextSection({
  projectKey,
  canEdit,
}: {
  projectKey: string
  canEdit: boolean
}) {
  const qc = useQueryClient()
  const text = useQuery(q.projectAutopilotText(projectKey))
  const save = useMutation({
    mutationFn: (v: { prompt: string; done: string }) => saveProjectAutopilotText(projectKey, v),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: q.projectAutopilotText(projectKey).queryKey })
      invalidateProject(qc, projectKey)
    },
    onError: toastError,
  })
  const d: AutopilotText | undefined = text.data
  return (
    <Section
      title="Autopilot’s wording"
      help={
        <>
          What autopilot says to sessions in this project: the prompt it pastes when it starts the
          next task, and the note under the turn that finished one. Left empty, this project uses
          the wording for every project (Admin → AI).
        </>
      }
    >
      {text.isPending ? (
        <div className="p-4">
          <Skeleton className="h-40" />
        </div>
      ) : !d ? null : (
        <WordingForm
          current={{ prompt: d.prompt, done: d.done }}
          fallback={{
            prompt: d.inherited_prompt ?? d.default_prompt,
            done: d.inherited_done ?? d.default_done,
          }}
          custom={d.custom_prompt || d.custom_done}
          resetLabel="Use every project’s"
          disabled={!canEdit}
          onSave={(v) => save.mutateAsync(v)}
        />
      )}
    </Section>
  )
}

interface Wording {
  prompt: string
  done: string
}

/**
 * The two templates. current is what applies; fallback is what applies
 * without the wording saved here (the default, or every project's), so
 * text equal to it, or left empty, is saved as none. No <form>: a project's
 * settings already sit in one.
 */
function WordingForm({
  current,
  fallback,
  custom,
  resetLabel,
  disabled,
  onSave,
}: {
  current: Wording
  fallback: Wording
  custom: boolean
  resetLabel: string
  disabled?: boolean
  onSave: (v: Wording) => Promise<unknown>
}) {
  const [v, setV] = useState<Wording>(current)
  const [seen, setSeen] = useState(JSON.stringify(current))
  const sent = useSent()
  const prompt = v.prompt.trim() || fallback.prompt
  const done = v.done.trim() || fallback.done
  const changed = prompt !== current.prompt || done !== current.done
  if (seen !== JSON.stringify(current)) {
    setSeen(JSON.stringify(current))
    if (!sent.mine(JSON.stringify(current))) setV(current)
  }
  const promptError = promptNamesTask(v.prompt)
    ? undefined
    : 'Name the task with {key} or {next}, or the session isn’t told what to do.'
  const store = (p: string, d: string): Wording => ({
    prompt: p === fallback.prompt ? '' : p,
    done: d === fallback.done ? '' : d,
  })
  const next = store(prompt, done)
  const savedAs = store(current.prompt, current.done)
  const { done: saved } = useAutosave(
    JSON.stringify(next),
    JSON.stringify(savedAs),
    !promptError && !disabled,
    900,
    () => {
      sent.add(JSON.stringify({ prompt, done }))
      return onSave(next)
    },
  )
  return (
    <div className="space-y-4 px-4 py-3">
      <Field
        label="Prompt when it starts a task"
        error={promptError}
        note={
          <>
            <Code>{'{key}'}</Code> is the task, <Code>{'{title}'}</Code> its title,{' '}
            <Code>{'{done}'}</Code> the task the session finished before it, and{' '}
            <Code>{'{next}'}</Code> what <Code>wp next</Code> answered: the task, how to finish it
            and where to start.
          </>
        }
      >
        <Textarea
          value={v.prompt}
          rows={6}
          autoGrow
          maxLength={4000}
          disabled={disabled}
          className="font-mono text-xs"
          onChange={(e) => setV((w) => ({ ...w, prompt: e.target.value }))}
          placeholder={fallback.prompt}
        />
      </Field>
      <Preview label="What the session sees">{renderAutopilotPrompt(prompt)}</Preview>
      <Field
        label="Note when a task is done"
        note={
          <>
            <Code>{'{key}'}</Code> is the task, <Code>{'{status}'}</Code> when the next one starts
            or what it waits for.
          </>
        }
      >
        <Textarea
          value={v.done}
          rows={2}
          autoGrow
          maxLength={4000}
          disabled={disabled}
          className="font-mono text-xs"
          onChange={(e) => setV((w) => ({ ...w, done: e.target.value }))}
          placeholder={fallback.done}
        />
      </Field>
      <Preview label="Under the turn">{renderAutopilotDone(done)}</Preview>
      {!disabled && (
        <div className="flex flex-wrap items-center gap-2">
          <SavedMark show={saved} />
          {changed ? (
            <Button
              type="button"
              size="sm"
              variant="ghost"
              icon={<RotateCcw className="size-3.5" />}
              onClick={() => setV(current)}
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
                onClick={() => onSave({ prompt: '', done: '' })}
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

function Preview({ label, children }: { label: string; children: string }) {
  return (
    <div className="rounded-lg border border-line bg-sunken px-3 py-2">
      <div className="mb-1 text-[11px] font-semibold uppercase tracking-wide text-subtle">
        {label}
      </div>
      <pre className="whitespace-pre-wrap font-mono text-xs text-fg [overflow-wrap:anywhere]">
        {children}
      </pre>
    </div>
  )
}
