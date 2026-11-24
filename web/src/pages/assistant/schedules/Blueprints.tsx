import { useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { Wand2 } from 'lucide-react'
import { hermes } from '@/lib/assistant'
import { Button } from '@/components/ui/button'
import { Dialog } from '@/components/ui/dialog'
import { Field, Input, Select } from '@/components/ui/input'
import { Section, Skeleton } from '@/components/ui/misc'
import { toast } from '@/components/ui/toast'
import { blueprintsQuery, jobsQuery, targetsQuery, type Blueprint, type CronJob } from './api'
import { deliverLabel } from './JobRow'

export function Blueprints({
  assistantId,
  onCreated,
}: {
  assistantId: number
  onCreated: (job: CronJob) => void
}) {
  const bp = useQuery(blueprintsQuery(assistantId))
  const [open, setOpen] = useState<Blueprint | null>(null)
  const [all, setAll] = useState(false)
  const list = bp.data?.blueprints ?? []
  const shown = all ? list : list.slice(0, 6)
  if (bp.isSuccess && list.length === 0) return null
  return (
    <Section
      title="Start from a blueprint"
      actions={
        list.length > 6 && (
          <Button size="xs" variant="ghost" onClick={() => setAll((a) => !a)}>
            {all ? 'Fewer' : `All ${list.length}`}
          </Button>
        )
      }
    >
      {bp.isPending ? (
        <div className="grid gap-2 p-3 sm:grid-cols-2 lg:grid-cols-3">
          {[0, 1, 2].map((i) => (
            <Skeleton key={i} className="h-16" />
          ))}
        </div>
      ) : bp.isError ? (
        <p className="p-4 text-[13px] text-danger">Couldn't load blueprints: {bp.error.message}</p>
      ) : (
        <ul className="grid gap-2 p-3 sm:grid-cols-2 lg:grid-cols-3">
          {shown.map((b) => (
            <li key={b.key}>
              <button
                onClick={() => setOpen(b)}
                title={b.description}
                className="flex h-full w-full items-start gap-2.5 rounded-lg border border-line px-3 py-2.5 text-left transition-colors hover:border-line-strong hover:bg-hover"
              >
                <Wand2 className="mt-0.5 size-4 shrink-0 text-accent" />
                <span className="min-w-0">
                  <span className="block truncate text-[13px] font-medium">{b.title}</span>
                  <span className="line-clamp-2 text-xs text-muted">{b.description}</span>
                </span>
              </button>
            </li>
          ))}
        </ul>
      )}
      {open && (
        <BlueprintDialog
          assistantId={assistantId}
          blueprint={open}
          onClose={() => setOpen(null)}
          onCreated={onCreated}
        />
      )}
    </Section>
  )
}

const RECURRENCE: Record<string, string> = {
  everyday: 'Every day',
  weekdays: 'Weekdays',
  weekends: 'Weekends',
}

function BlueprintDialog({
  assistantId,
  blueprint: b,
  onClose,
  onCreated,
}: {
  assistantId: number
  blueprint: Blueprint
  onClose: () => void
  onCreated: (job: CronJob) => void
}) {
  const qc = useQueryClient()
  const targets = useQuery(targetsQuery(assistantId)).data?.targets ?? []
  const names = new Map(targets.map((t) => [t.id, t.name]))
  const noHome = !targets.some((t) => t.id !== 'local' && !t.id.startsWith('bot-chat'))
  const [values, setValues] = useState<Record<string, string>>(() =>
    Object.fromEntries(
      b.fields.map((f) => [
        f.name,
        f.name === 'deliver' && f.default === 'origin' && noHome && f.options.includes('local')
          ? 'local'
          : (f.default ?? ''),
      ]),
    ),
  )
  const [error, setError] = useState('')
  const [saving, setSaving] = useState(false)
  const missing = b.fields.find((f) => !f.optional && !values[f.name]?.trim())
  const save = async () => {
    if (saving) return
    if (missing) return setError(`Fill in “${missing.label}”`)
    setSaving(true)
    setError('')
    try {
      const job = await hermes<CronJob>(assistantId, 'POST', 'cron/blueprints/instantiate', {
        blueprint: b.key,
        values,
      })
      qc.setQueryData<CronJob[]>(jobsQuery(assistantId).queryKey, (l) => (l ? [...l, job] : l))
      toast(`Scheduled “${job.name}”`, { kind: 'success' })
      onCreated(job)
      onClose()
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
      setSaving(false)
    }
  }
  const optionLabel = (field: string, o: string) =>
    field === 'deliver'
      ? deliverLabel(o, names)
      : (RECURRENCE[o] ?? o.charAt(0).toUpperCase() + o.slice(1))
  return (
    <Dialog
      open
      onClose={onClose}
      title={b.title}
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
            Schedule it
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
      >
        <p className="text-[13px] text-muted">{b.description}</p>
        {b.fields.map((f, i) => {
          const v = values[f.name] ?? ''
          const set = (x: string) => setValues((s) => ({ ...s, [f.name]: x }))
          const help = ['text', 'time'].includes(f.type) || f.name === 'deliver' ? '' : f.help
          return (
            <Field key={f.name} label={f.label} note={help || undefined}>
              {f.options.length > 0 ? (
                <Select value={v} onChange={(e) => set(e.target.value)}>
                  {f.options.map((o) => (
                    <option key={o} value={o}>
                      {optionLabel(f.name, o)}
                    </option>
                  ))}
                </Select>
              ) : f.type === 'time' ? (
                <Input
                  type="time"
                  value={v}
                  onChange={(e) => set(e.target.value)}
                  className="w-32"
                />
              ) : (
                <Input
                  autoFocus={i === 0}
                  value={v}
                  onChange={(e) => set(e.target.value)}
                  placeholder={f.help || undefined}
                />
              )}
            </Field>
          )
        })}
        <button type="submit" hidden />
      </form>
    </Dialog>
  )
}
