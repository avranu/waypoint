import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { MailPlus, Trash2, UserPlus } from 'lucide-react'
import { api } from '@/lib/api'
import { q } from '@/lib/queries'
import { dateTime, relativeTime } from '@/lib/format'
import type { PendingInvite, UserLink } from '@/lib/types'
import { Button, IconButton } from '@/components/ui/button'
import { Confirm, Dialog } from '@/components/ui/dialog'
import { Field, Input, Switch } from '@/components/ui/input'
import { Badge, Empty, LoadError, Section, Skeleton } from '@/components/ui/misc'
import { toastError } from '@/components/ui/toast'
import { CopyField, Help, Warning } from '../settings/common'

export function InvitesTab() {
  const qc = useQueryClient()
  const invites = useQuery(q.invites())
  const [creating, setCreating] = useState(false)
  const [revoking, setRevoking] = useState<PendingInvite | null>(null)
  const revoke = useMutation({
    mutationFn: (id: string) => api.del(`/admin/invites/${encodeURIComponent(id)}`),
    onSettled: () => qc.invalidateQueries({ queryKey: ['invites'] }),
    onError: toastError,
  })
  const list = invites.data ?? []
  return (
    <>
      <Section
        title="Invites"
        help={
          <>
            There's no public sign-up: people join through a one-time link that you send them
            yourself (Waypoint doesn't send email).
          </>
        }
        actions={
          <Button
            size="sm"
            variant="primary"
            icon={<UserPlus className="size-3.5" />}
            onClick={() => setCreating(true)}
          >
            Invite someone
          </Button>
        }
      >
        {invites.isPending ? (
          <div className="p-4">
            <Skeleton className="h-10" />
          </div>
        ) : invites.isError ? (
          <LoadError what="Invites" error={invites.error} />
        ) : list.length === 0 ? (
          <Empty icon={<MailPlus />} title="No pending invites" />
        ) : (
          <ul className="divide-y divide-line">
            {list.map((i) => (
              <li key={i.id} className="flex items-center gap-3 px-4 py-3">
                <MailPlus className="size-4 shrink-0 text-subtle" />
                <div className="min-w-0 flex-1">
                  <div className="flex flex-wrap items-center gap-1.5">
                    <span className="truncate text-[13px] font-medium">
                      {i.email || 'Anyone with the link'}
                    </span>
                    {i.is_admin && <Badge color="var(--wp-accent)">admin</Badge>}
                  </div>
                  <div className="mt-0.5 text-xs text-subtle">
                    Created {relativeTime(i.created_at)} · expires {relativeTime(i.expires_at)}
                  </div>
                </div>
                <IconButton
                  label="Revoke invite"
                  onClick={() => setRevoking(i)}
                  icon={<Trash2 className="size-3.5" />}
                />
              </li>
            ))}
          </ul>
        )}
      </Section>
      <InviteDialog open={creating} onClose={() => setCreating(false)} />
      <Confirm
        open={!!revoking}
        onClose={() => setRevoking(null)}
        title="Revoke this invite?"
        body="The link stops working. You can always send a new one."
        confirmLabel="Revoke"
        onConfirm={() => revoking && revoke.mutate(revoking.id)}
      />
    </>
  )
}

function InviteDialog({ open, onClose }: { open: boolean; onClose: () => void }) {
  const qc = useQueryClient()
  const [email, setEmail] = useState('')
  const [admin, setAdmin] = useState(false)
  const [days, setDays] = useState('7')
  const [link, setLink] = useState<UserLink | null>(null)
  const close = () => {
    onClose()
    setEmail('')
    setAdmin(false)
    setDays('7')
    setLink(null)
  }
  const create = useMutation({
    mutationFn: () =>
      api.post<UserLink>('/admin/invites', {
        email: email.trim(),
        is_admin: admin,
        expires_in_days: Math.max(1, parseInt(days, 10) || 7),
      }),
    onSuccess: (l) => {
      setLink(l)
      qc.invalidateQueries({ queryKey: ['invites'] })
    },
    onError: toastError,
  })
  return (
    <Dialog
      open={open}
      onClose={close}
      title={link ? 'Invite link' : 'Invite someone'}
      footer={
        link ? (
          <Button variant="primary" onClick={close}>
            Done
          </Button>
        ) : (
          <>
            <Button variant="ghost" onClick={close}>
              Cancel
            </Button>
            <Button variant="primary" loading={create.isPending} onClick={() => create.mutate()}>
              Create link
            </Button>
          </>
        )
      }
    >
      {link ? (
        <div className="space-y-3 p-4">
          <Warning>
            Copy this link now — it's only shown once. Send it to{' '}
            {link.email || 'the person you are inviting'}.
          </Warning>
          <CopyField value={link.url ?? ''} />
          <Help className="text-xs">
            They'll pick a username and password. The link works once and expires{' '}
            {dateTime(link.expires_at)}.
          </Help>
        </div>
      ) : (
        <form
          className="space-y-4 p-4"
          onSubmit={(e) => {
            e.preventDefault()
            create.mutate()
          }}
        >
          <Field
            label="Email (optional)"
            hint="If set, the account will use this address. Leave blank to let them enter their own."
          >
            <Input
              autoFocus
              type="email"
              value={email}
              onChange={(e) => setEmail(e.target.value)}
              placeholder="friend@example.com"
            />
          </Field>
          <div className="flex items-center justify-between gap-3">
            <div>
              <div className="text-[13px] font-medium">Make them an admin</div>
              <div className="text-xs text-subtle">
                Admins manage people, forges and global webhooks.
              </div>
            </div>
            <Switch checked={admin} onChange={setAdmin} label="Admin" />
          </div>
          <Field label="Link expires after (days)">
            <Input
              type="number"
              min={1}
              inputMode="numeric"
              value={days}
              onChange={(e) => setDays(e.target.value)}
              className="sm:w-32"
            />
          </Field>
          <button type="submit" className="hidden" />
        </form>
      )}
    </Dialog>
  )
}
