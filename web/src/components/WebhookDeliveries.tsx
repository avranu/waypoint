// A webhook's delivery log: what was sent, what came back, and a way to
// send one again. Shared by project and global webhooks.

import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import clsx from 'clsx'
import { ChevronRight, RotateCcw } from 'lucide-react'
import { api } from '@/lib/api'
import { q } from '@/lib/queries'
import { dateTime, relativeTime } from '@/lib/format'
import type { Webhook, WebhookDelivery } from '@/lib/types'
import { Dialog } from '@/components/ui/dialog'
import { IconButton } from '@/components/ui/button'
import { Empty, Skeleton } from '@/components/ui/misc'
import { toast, toastError } from '@/components/ui/toast'

const ok = (d: WebhookDelivery) => !d.error && d.status !== null && d.status < 300

export function WebhookDeliveries({
  hook,
  onClose,
}: {
  hook: Webhook | null
  onClose: () => void
}) {
  return (
    <Dialog open={!!hook} onClose={onClose} title={hook ? `Deliveries · ${hook.name}` : ''} wide>
      {hook && <DeliveryList hook={hook} />}
    </Dialog>
  )
}

function DeliveryList({ hook }: { hook: Webhook }) {
  const qc = useQueryClient()
  const log = useQuery(q.webhookDeliveries(hook.id))
  const [open, setOpen] = useState<number | null>(null)
  const replay = useMutation({
    mutationFn: (d: WebhookDelivery) =>
      api.post<WebhookDelivery>(`/webhooks/${hook.id}/deliveries/${d.id}/replay`),
    onSuccess: (r) =>
      ok(r)
        ? toast(`Sent again (HTTP ${r.status})`, { kind: 'success' })
        : toast(r.error || `Failed (HTTP ${r.status})`, { kind: 'error' }),
    onError: toastError,
    onSettled: () => {
      qc.invalidateQueries({ queryKey: ['webhookDeliveries', hook.id] })
      qc.invalidateQueries({ queryKey: ['webhooks'] })
    },
  })
  if (log.isPending)
    return (
      <div className="space-y-2 p-4">
        <Skeleton className="h-9" />
        <Skeleton className="h-9" />
      </div>
    )
  const list = log.data ?? []
  if (list.length === 0)
    return <Empty title="Nothing sent yet">Deliveries from the last 14 days show here.</Empty>
  return (
    <ul className="divide-y divide-line" aria-label="Deliveries">
      {list.map((d) => {
        const expanded = open === d.id
        return (
          <li key={d.id}>
            <div className="flex items-center gap-2 px-4 py-2">
              <button
                type="button"
                onClick={() => setOpen(expanded ? null : d.id)}
                aria-expanded={expanded}
                className="flex min-w-0 flex-1 items-center gap-2 text-left"
              >
                <ChevronRight
                  className={clsx(
                    'size-3.5 shrink-0 text-subtle transition-transform',
                    expanded && 'rotate-90',
                  )}
                />
                <span
                  className={clsx('size-2 shrink-0 rounded-full', ok(d) ? 'bg-ok' : 'bg-danger')}
                />
                <span className="min-w-0 flex-1">
                  <span className="block truncate font-mono text-[13px]">{d.event}</span>
                  <span className="block truncate text-xs text-subtle">
                    <span className={ok(d) ? 'text-ok' : 'text-danger'}>
                      {d.status ? `HTTP ${d.status}` : 'No answer'}
                    </span>
                    {' · '}
                    {d.duration_ms} ms
                    {d.attempts > 1 && ` · ${d.attempts} tries`}
                    {d.replay_of && ' · sent again'}
                  </span>
                </span>
                <time
                  dateTime={d.created_at}
                  title={dateTime(d.created_at)}
                  className="shrink-0 text-xs text-subtle"
                >
                  {relativeTime(d.created_at)}
                </time>
              </button>
              <IconButton
                label={d.replayable ? 'Send again' : 'Too big to keep, so it can’t be sent again'}
                size="xs"
                disabled={!d.replayable || replay.isPending}
                onClick={() => replay.mutate(d)}
                icon={
                  <RotateCcw
                    className={clsx(
                      'size-3.5',
                      replay.isPending && replay.variables?.id === d.id && 'animate-spin',
                    )}
                  />
                }
              />
            </div>
            {expanded && (
              <div className="space-y-1.5 px-4 pb-3 pl-10 text-xs">
                {d.error && d.error !== `HTTP ${d.status}` && (
                  <div className="text-danger">{d.error}</div>
                )}
                <div className="text-subtle">
                  Delivery <code className="font-mono text-muted">{d.delivery}</code>
                </div>
                {d.response ? (
                  <pre className="max-h-40 overflow-auto whitespace-pre-wrap break-all rounded-lg bg-sunken p-2 font-mono text-[11px] text-muted">
                    {d.response}
                  </pre>
                ) : (
                  <div className="text-subtle">No response body.</div>
                )}
              </div>
            )}
          </li>
        )
      })}
    </ul>
  )
}
