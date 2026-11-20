import { useEffect, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import clsx from 'clsx'
import {
  History,
  MoreHorizontal,
  Pencil,
  Plus,
  Send,
  Trash2,
  Webhook as WebhookIcon,
} from 'lucide-react'
import { api } from '@/lib/api'
import { q } from '@/lib/queries'
import type { Webhook } from '@/lib/types'
import { Button, IconButton } from '@/components/ui/button'
import { Confirm, Dialog } from '@/components/ui/dialog'
import { Field, Input, Switch } from '@/components/ui/input'
import { Badge, Empty, Section, Skeleton } from '@/components/ui/misc'
import { MenuList, Popover, usePopover } from '@/components/ui/popover'
import { toast, toastError } from '@/components/ui/toast'
import { WebhookDeliveries } from '@/components/WebhookDeliveries'

const EVENT_PRESETS: [string, string][] = [
  ['*', 'Everything'],
  ['issue.*', 'All issue events'],
  ['issue.created', 'Created'],
  ['issue.status_changed', 'Status changed'],
  ['issue.assigned', 'Assigned'],
  ['issue.commented', 'Commented'],
  ['issue.deleted', 'Deleted'],
  ['sprint.*', 'Sprints'],
  ['external.*', 'Pull requests'],
]

export function WebhooksTab() {
  const qc = useQueryClient()
  const hooks = useQuery(q.globalWebhooks())
  const [editing, setEditing] = useState<Webhook | 'new' | null>(null)
  const [deleting, setDeleting] = useState<Webhook | null>(null)
  const [history, setHistory] = useState<Webhook | null>(null)
  const refresh = () => qc.invalidateQueries({ queryKey: ['webhooks', ''] })
  const toggle = useMutation({
    mutationFn: (h: Webhook) => api.patch(`/webhooks/${h.id}`, { enabled: !h.enabled }),
    onMutate: (h) =>
      qc.setQueryData<Webhook[]>(['webhooks', ''], (l) =>
        l?.map((x) => (x.id === h.id ? { ...x, enabled: !h.enabled } : x)),
      ),
    onSettled: refresh,
    onError: toastError,
  })
  const test = useMutation({
    mutationFn: (h: Webhook) =>
      api.post<{ ok: boolean; status: number; error?: string }>(`/webhooks/${h.id}/test`),
    onSuccess: (r) =>
      r.ok
        ? toast(`Delivered (HTTP ${r.status})`)
        : toast(r.error || `Failed (HTTP ${r.status})`, { kind: 'error' }),
    onSettled: refresh,
    onError: toastError,
  })
  const del = useMutation({
    mutationFn: (id: number) => api.del(`/webhooks/${id}`),
    onSettled: refresh,
    onError: toastError,
  })
  const list = hooks.data ?? []
  return (
    <>
      <Section
        title="Global webhooks"
        help={
          <>
            Events from <b>every project</b>, as JSON POSTs (projects can add their own). For{' '}
            <b>Kestra</b>, paste a Webhook trigger's URL. With a secret, requests carry{' '}
            <b>X-Waypoint-Signature</b>, an HMAC of the body.
          </>
        }
        actions={
          <Button
            size="sm"
            variant="primary"
            icon={<Plus className="size-3.5" />}
            onClick={() => setEditing('new')}
          >
            Add webhook
          </Button>
        }
      >
        {hooks.isPending ? (
          <div className="p-4">
            <Skeleton className="h-12" />
          </div>
        ) : list.length === 0 ? (
          <Empty icon={<WebhookIcon />} title="No global webhooks" />
        ) : (
          <ul className="divide-y divide-line">
            {list.map((h) => (
              <HookRow
                key={h.id}
                hook={h}
                testing={test.isPending && test.variables?.id === h.id}
                onToggle={() => toggle.mutate(h)}
                onTest={() => test.mutate(h)}
                onHistory={() => setHistory(h)}
                onEdit={() => setEditing(h)}
                onDelete={() => setDeleting(h)}
              />
            ))}
          </ul>
        )}
      </Section>
      <WebhookDialog hook={editing} onClose={() => setEditing(null)} onSaved={refresh} />
      <WebhookDeliveries hook={history} onClose={() => setHistory(null)} />
      <Confirm
        open={!!deleting}
        onClose={() => setDeleting(null)}
        title={`Delete “${deleting?.name}”?`}
        body="Events stop being sent to this URL."
        onConfirm={() => deleting && del.mutate(deleting.id)}
      />
    </>
  )
}

function HookRow({
  hook: h,
  testing,
  onToggle,
  onTest,
  onHistory,
  onEdit,
  onDelete,
}: {
  hook: Webhook
  testing: boolean
  onToggle: () => void
  onTest: () => void
  onHistory: () => void
  onEdit: () => void
  onDelete: () => void
}) {
  const m = usePopover()
  const failing =
    h.last_error || (h.last_status !== null && (h.last_status < 200 || h.last_status >= 300))
  return (
    <li className="flex items-start gap-3 px-4 py-3">
      <span
        className={clsx(
          'mt-1.5 size-2 shrink-0 rounded-full',
          !h.enabled
            ? 'bg-line-strong'
            : failing
              ? 'bg-danger'
              : h.last_status
                ? 'bg-ok'
                : 'bg-subtle',
        )}
      />
      <div className="min-w-0 flex-1">
        <div className="flex flex-wrap items-center gap-1.5">
          <span className={clsx('truncate text-[13px] font-medium', !h.enabled && 'text-muted')}>
            {h.name}
          </span>
          {h.has_secret && <Badge>signed</Badge>}
          {h.last_status !== null && (
            <Badge color={failing ? 'var(--wp-danger)' : 'var(--wp-ok)'}>
              HTTP {h.last_status}
            </Badge>
          )}
        </div>
        <div className="mt-0.5 truncate font-mono text-xs text-subtle">{h.url}</div>
        <div className="mt-0.5 truncate text-xs text-subtle">
          Events: <span className="font-mono">{h.events}</span>
        </div>
        {h.last_error && (
          <div className="mt-1 line-clamp-2 text-xs text-danger">{h.last_error}</div>
        )}
      </div>
      <Switch checked={h.enabled} onChange={onToggle} label={h.enabled ? 'Disable' : 'Enable'} />
      <IconButton
        ref={m.anchor}
        label={`Actions for ${h.name}`}
        onClick={m.toggle}
        icon={<MoreHorizontal className="size-4" />}
        className="-mt-1"
      />
      <Popover open={m.open} onClose={m.close} anchor={m.anchor} align="end">
        <MenuList
          onClose={m.close}
          items={[
            {
              label: testing ? 'Sending…' : 'Send test ping',
              icon: <Send />,
              disabled: testing,
              onSelect: onTest,
            },
            { label: 'Deliveries', icon: <History />, onSelect: onHistory },
            { label: 'Edit', icon: <Pencil />, onSelect: onEdit },
            { separator: true, label: '' },
            { label: 'Delete…', icon: <Trash2 />, danger: true, onSelect: onDelete },
          ]}
        />
      </Popover>
    </li>
  )
}

function WebhookDialog({
  hook,
  onClose,
  onSaved,
}: {
  hook: Webhook | 'new' | null
  onClose: () => void
  onSaved: () => void
}) {
  const existing = hook && hook !== 'new' ? hook : null
  const [name, setName] = useState('')
  const [url, setUrl] = useState('')
  const [secret, setSecret] = useState('')
  const [events, setEvents] = useState('*')
  useEffect(() => {
    if (!hook) return
    setName(existing?.name ?? '')
    setUrl(existing?.url ?? '')
    setSecret('')
    setEvents(existing?.events ?? '*')
  }, [hook]) // eslint-disable-line react-hooks/exhaustive-deps
  const selected = events
    .split(',')
    .map((s) => s.trim())
    .filter(Boolean)
  const togglePreset = (ev: string) => {
    if (ev === '*') return setEvents('*')
    const rest = selected.filter((s) => s !== '*')
    setEvents((rest.includes(ev) ? rest.filter((s) => s !== ev) : [...rest, ev]).join(',') || '*')
  }
  const save = useMutation({
    mutationFn: () => {
      const body: Record<string, string> = {
        name: name.trim(),
        url: url.trim(),
        events: events.trim() || '*',
      }
      if (existing) {
        if (secret.trim()) body.secret = secret.trim()
        return api.patch<Webhook>(`/webhooks/${existing.id}`, body)
      }
      body.secret = secret.trim()
      return api.post<Webhook>('/admin/webhooks', body)
    },
    onSuccess: () => {
      onSaved()
      onClose()
    },
    onError: toastError,
  })
  const validURL = /^https?:\/\/\S+$/.test(url.trim())
  return (
    <Dialog
      open={!!hook}
      onClose={onClose}
      title={existing ? 'Edit webhook' : 'Add a global webhook'}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button
            variant="primary"
            disabled={!validURL}
            loading={save.isPending}
            onClick={() => save.mutate()}
          >
            Save
          </Button>
        </>
      }
    >
      <form
        className="space-y-4 p-4"
        onSubmit={(e) => {
          e.preventDefault()
          if (validURL) save.mutate()
        }}
      >
        <Field label="Name" hint="Defaults to the URL.">
          <Input
            autoFocus
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder="Kestra"
          />
        </Field>
        <Field label="URL">
          <Input
            type="url"
            value={url}
            onChange={(e) => setUrl(e.target.value)}
            placeholder="https://kestra.example.com/api/v1/executions/webhook/…"
            className="font-mono text-xs"
          />
        </Field>
        <Field
          label="Secret"
          hint={
            existing
              ? existing.has_secret
                ? 'A secret is set. Leave blank to keep it.'
                : 'Optional. Used to sign each request.'
              : 'Optional. Used to sign each request.'
          }
        >
          <Input
            value={secret}
            onChange={(e) => setSecret(e.target.value)}
            autoComplete="off"
            className="font-mono"
          />
        </Field>
        <Field
          label="Events"
          hint="Comma separated. Use * for everything, or a prefix like issue.*"
        >
          <Input
            value={events}
            onChange={(e) => setEvents(e.target.value)}
            className="font-mono text-xs"
          />
        </Field>
        <div className="-mt-2 flex flex-wrap gap-1.5">
          {EVENT_PRESETS.map(([ev, label]) => (
            <button
              key={ev}
              type="button"
              onClick={() => togglePreset(ev)}
              title={ev}
              className={clsx(
                'rounded-full border px-2.5 py-1 text-xs transition-colors',
                selected.includes(ev)
                  ? 'border-accent bg-accent-soft text-fg'
                  : 'border-line text-muted hover:text-fg',
              )}
            >
              {label}
            </button>
          ))}
        </div>
        <button type="submit" className="hidden" />
      </form>
    </Dialog>
  )
}
