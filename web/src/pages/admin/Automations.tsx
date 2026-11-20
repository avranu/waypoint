import { useEffect, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { HardDrive } from 'lucide-react'
import { api } from '@/lib/api'
import {
  adminSystemQuery,
  bytesText,
  type ResourceLimits,
  type ResourceStatus,
} from '@/lib/automations'
import { Button } from '@/components/ui/button'
import { Field, Input } from '@/components/ui/input'
import { Badge, Section, Skeleton } from '@/components/ui/misc'
import { toast, toastError } from '@/components/ui/toast'
import { AutomationsPanel } from '../project-settings/Automations'

/** Admin → Automations: rules that belong to no project, and the limits the system triggers watch. */
export function AutomationsAdminTab() {
  return (
    <div className="space-y-8">
      <AutomationsPanel />
      <SystemLimits />
    </div>
  )
}

const FIELDS: { key: keyof ResourceLimits; resource: string; label: string; hint: string }[] = [
  {
    key: 'disk_free_mb',
    resource: 'disk',
    label: 'Free disk space below (MB)',
    hint: 'On the disk with the database. Fires “disk, database or memory low” once when it drops below, and at most once an hour.',
  },
  {
    key: 'database_mb',
    resource: 'database',
    label: 'Database larger than (MB)',
    hint: 'The size of the database file.',
  },
  {
    key: 'memory_free_mb',
    resource: 'memory',
    label: 'Available memory below (MB)',
    hint: 'What the machine reports as available.',
  },
]

function reading(r: ResourceStatus): string {
  if (r.resource === 'database') return `${bytesText(r.bytes)} used`
  return r.total
    ? `${bytesText(r.bytes)} free of ${bytesText(r.total)}`
    : `${bytesText(r.bytes)} free`
}

function SystemLimits() {
  const qc = useQueryClient()
  const sys = useQuery(adminSystemQuery())
  const [form, setForm] = useState<Record<keyof ResourceLimits, string>>({
    disk_free_mb: '',
    database_mb: '',
    memory_free_mb: '',
  })
  const limits = sys.data?.limits
  useEffect(() => {
    if (limits)
      setForm({
        disk_free_mb: String(limits.disk_free_mb),
        database_mb: String(limits.database_mb),
        memory_free_mb: String(limits.memory_free_mb),
      })
  }, [limits?.disk_free_mb, limits?.database_mb, limits?.memory_free_mb]) // eslint-disable-line react-hooks/exhaustive-deps
  const save = useMutation({
    mutationFn: () =>
      api.put<ResourceLimits>('/admin/system-limits', {
        disk_free_mb: Math.max(0, Math.round(Number(form.disk_free_mb) || 0)),
        database_mb: Math.max(0, Math.round(Number(form.database_mb) || 0)),
        memory_free_mb: Math.max(0, Math.round(Number(form.memory_free_mb) || 0)),
      }),
    onSuccess: () => {
      toast('Limits saved', { kind: 'success' })
      qc.invalidateQueries({ queryKey: ['admin', 'system'] })
    },
    onError: toastError,
  })
  const dirty = !!limits && FIELDS.some((f) => Number(form[f.key] || 0) !== limits[f.key])
  return (
    <Section
      title="System limits"
      help="Admin automations can start when the disk, the database or memory crosses a limit below. It’s checked every minute; crossing fires once, and not again within the hour. Zero turns a check off."
    >
      {sys.isLoading ? (
        <div className="space-y-2 p-4">
          <Skeleton className="h-10" />
          <Skeleton className="h-10" />
        </div>
      ) : (
        <div className="space-y-4 p-4">
          <ul className="divide-y divide-line rounded-lg border border-line">
            {(sys.data?.resources ?? []).map((r) => (
              <li key={r.resource} className="flex items-center gap-3 px-3 py-2">
                <HardDrive className="size-4 shrink-0 text-subtle" />
                <span className="min-w-0 flex-1">
                  <span className="block text-[13px] font-medium">{r.label}</span>
                  <span className="block text-xs text-muted">{reading(r)}</span>
                </span>
                {r.low ? (
                  <Badge color="var(--wp-danger)">Low</Badge>
                ) : r.limit_mb > 0 ? (
                  <span className="text-xs text-subtle">
                    alerts {r.resource === 'database' ? 'above' : 'below'}{' '}
                    {bytesText(r.limit_mb * 1024 * 1024)}
                  </span>
                ) : (
                  <span className="text-xs text-subtle">not checked</span>
                )}
              </li>
            ))}
          </ul>
          <form
            className="grid gap-3 sm:grid-cols-3"
            onSubmit={(e) => {
              e.preventDefault()
              save.mutate()
            }}
          >
            {FIELDS.map((f) => (
              <Field key={f.key} label={f.label} hint={f.hint}>
                <Input
                  type="number"
                  min={0}
                  value={form[f.key]}
                  onChange={(e) => setForm((x) => ({ ...x, [f.key]: e.target.value }))}
                />
              </Field>
            ))}
            <div className="flex items-center justify-between gap-3 sm:col-span-3">
              <span className="text-xs text-subtle">0 turns a check off.</span>
              <Button type="submit" variant="primary" loading={save.isPending} disabled={!dirty}>
                Save limits
              </Button>
            </div>
          </form>
        </div>
      )}
    </Section>
  )
}
