import { useEffect, useRef, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import {
  Ban,
  CheckCircle2,
  ImageUp,
  KeyRound,
  MoreHorizontal,
  Pencil,
  Shield,
  ShieldOff,
  Trash2,
  Users,
} from 'lucide-react'
import { api } from '@/lib/api'
import { q } from '@/lib/queries'
import { dateTime, shortDate } from '@/lib/format'
import type { User, UserLink } from '@/lib/types'
import { Button, IconButton } from '@/components/ui/button'
import { Confirm, Dialog } from '@/components/ui/dialog'
import { Field, Input } from '@/components/ui/input'
import { Avatar, Badge, Empty, Section, Skeleton } from '@/components/ui/misc'
import { MenuList, Popover, usePopover } from '@/components/ui/popover'
import { toast, toastError } from '@/components/ui/toast'
import { avatarProblem, CopyField, Help } from '../settings/common'

interface UserPatch {
  display_name?: string
  email?: string
  username?: string
  is_admin?: boolean
  disabled?: boolean
}

function useInvalidateUsers() {
  const qc = useQueryClient()
  return (u?: User) => {
    qc.invalidateQueries({ queryKey: ['adminUsers'] })
    qc.invalidateQueries({ queryKey: ['users'] })
    const me = qc.getQueryData<User>(['me'])
    if (u && me && u.id === me.id) qc.setQueryData(['me'], u)
  }
}

export function UsersTab({ me }: { me: User }) {
  const qc = useQueryClient()
  const users = useQuery(q.adminUsers())
  const done = useInvalidateUsers()
  const [editing, setEditing] = useState<User | null>(null)
  const [deleting, setDeleting] = useState<User | null>(null)
  const [link, setLink] = useState<UserLink | null>(null)
  const fileFor = useRef<User | null>(null)
  const file = useRef<HTMLInputElement>(null)

  const patch = useMutation({
    mutationFn: ({ id, p }: { id: number; p: UserPatch }) =>
      api.patch<User>(`/admin/users/${id}`, p),
    onSuccess: (u) => done(u),
    onError: toastError,
  })
  const del = useMutation({
    mutationFn: (id: number) => api.del(`/admin/users/${id}`),
    onSuccess: () => {
      done()
      toast('User deleted')
    },
    onError: toastError,
  })
  const reset = useMutation({
    mutationFn: (id: number) => api.post<UserLink>(`/admin/users/${id}/reset-link`),
    onSuccess: setLink,
    onError: toastError,
  })
  const avatar = useMutation({
    mutationFn: ({ id, f }: { id: number; f: File }) => api.put(`/admin/users/${id}/avatar`, f),
    onSuccess: (_d, v) => {
      done()
      if (v.id === me.id) qc.invalidateQueries({ queryKey: ['me'] })
      toast('Avatar updated')
    },
    onError: toastError,
  })

  const list = users.data ?? []
  return (
    <>
      <Section
        title={users.data ? `People (${list.length})` : 'People'}
        helpId="admin:people"
        help={
          <>
            Disabled people can't sign in, but what they did stays. To add someone, send an invite.
          </>
        }
      >
        {users.isPending ? (
          <div className="space-y-2 p-4">
            <Skeleton className="h-12" />
            <Skeleton className="h-12" />
          </div>
        ) : list.length === 0 ? (
          <Empty icon={<Users />} title="No users" />
        ) : (
          <ul className="divide-y divide-line">
            {list.map((u) => (
              <UserRow
                key={u.id}
                user={u}
                self={u.id === me.id}
                onEdit={() => setEditing(u)}
                onPatch={(p) => patch.mutate({ id: u.id, p })}
                onReset={() => reset.mutate(u.id)}
                onAvatar={() => {
                  fileFor.current = u
                  file.current?.click()
                }}
                onDelete={() => setDeleting(u)}
              />
            ))}
          </ul>
        )}
      </Section>
      <input
        ref={file}
        type="file"
        accept="image/png,image/jpeg,image/gif,image/webp"
        className="hidden"
        onChange={(e) => {
          const f = e.target.files?.[0]
          e.target.value = ''
          const u = fileFor.current
          if (!f || !u) return
          const bad = avatarProblem(f)
          if (bad) toast(bad, { kind: 'error' })
          else avatar.mutate({ id: u.id, f })
        }}
      />
      <EditUserDialog user={editing} onClose={() => setEditing(null)} onSaved={done} />
      <Confirm
        open={!!deleting}
        onClose={() => setDeleting(null)}
        title={`Delete ${deleting?.display_name}?`}
        body="Their account, API tokens and time entries are removed for good. Issues and comments they wrote stay. Disabling is usually the better choice."
        onConfirm={() => deleting && del.mutate(deleting.id)}
      />
      <Dialog
        open={!!link}
        onClose={() => setLink(null)}
        title="Password reset link"
        footer={
          <Button variant="primary" onClick={() => setLink(null)}>
            Done
          </Button>
        }
      >
        {link && (
          <div className="space-y-3 p-4">
            <Help>
              Send this link to <b className="font-medium text-fg">@{link.username}</b> yourself —
              Waypoint doesn't send email. It lets them choose a new password, works once, and
              expires {dateTime(link.expires_at)}.
            </Help>
            <CopyField value={link.url ?? ''} />
          </div>
        )}
      </Dialog>
    </>
  )
}

function UserRow({
  user: u,
  self,
  onEdit,
  onPatch,
  onReset,
  onAvatar,
  onDelete,
}: {
  user: User
  self: boolean
  onEdit: () => void
  onPatch: (p: UserPatch) => void
  onReset: () => void
  onAvatar: () => void
  onDelete: () => void
}) {
  const m = usePopover()
  return (
    <li className="flex items-center gap-3 px-4 py-3">
      <Avatar user={u} size={36} className={u.disabled ? 'opacity-50 grayscale' : undefined} />
      <div className="min-w-0 flex-1">
        <div className="flex flex-wrap items-center gap-1.5">
          <span className="truncate text-[13px] font-medium">{u.display_name}</span>
          <span className="text-xs text-subtle">@{u.username}</span>
          {self && <Badge>you</Badge>}
          {u.is_admin && <Badge color="var(--wp-accent)">admin</Badge>}
          {u.disabled && <Badge color="var(--wp-danger)">disabled</Badge>}
        </div>
        <div className="mt-0.5 truncate text-xs text-subtle">
          {u.email} <span className="hidden sm:inline">· joined {shortDate(u.created_at)}</span>
        </div>
      </div>
      <IconButton
        ref={m.anchor}
        label={`Actions for ${u.display_name}`}
        onClick={m.toggle}
        icon={<MoreHorizontal className="size-4" />}
      />
      <Popover open={m.open} onClose={m.close} anchor={m.anchor} align="end">
        <MenuList
          onClose={m.close}
          items={[
            { label: 'Edit details', icon: <Pencil />, onSelect: onEdit },
            { label: 'Upload avatar', icon: <ImageUp />, onSelect: onAvatar },
            { label: 'Password reset link', icon: <KeyRound />, onSelect: onReset },
            { separator: true, label: '' },
            u.is_admin
              ? {
                  label: 'Remove admin',
                  icon: <ShieldOff />,
                  disabled: self,
                  hint: self ? "can't demote yourself" : undefined,
                  onSelect: () => onPatch({ is_admin: false }),
                }
              : {
                  label: 'Make admin',
                  icon: <Shield />,
                  onSelect: () => onPatch({ is_admin: true }),
                },
            u.disabled
              ? {
                  label: 'Enable',
                  icon: <CheckCircle2 />,
                  onSelect: () => onPatch({ disabled: false }),
                }
              : {
                  label: 'Disable',
                  icon: <Ban />,
                  disabled: self,
                  onSelect: () => onPatch({ disabled: true }),
                },
            {
              label: 'Delete…',
              icon: <Trash2 />,
              danger: true,
              disabled: self,
              onSelect: onDelete,
            },
          ]}
        />
      </Popover>
    </li>
  )
}

function EditUserDialog({
  user,
  onClose,
  onSaved,
}: {
  user: User | null
  onClose: () => void
  onSaved: (u: User) => void
}) {
  const [name, setName] = useState('')
  const [email, setEmail] = useState('')
  const [username, setUsername] = useState('')
  useEffect(() => {
    if (user) {
      setName(user.display_name)
      setEmail(user.email)
      setUsername(user.username)
    }
  }, [user])
  const save = useMutation({
    mutationFn: () => {
      const p: UserPatch = {}
      if (name.trim() !== user!.display_name) p.display_name = name.trim()
      if (email.trim() !== user!.email) p.email = email.trim()
      if (username.trim() !== user!.username) p.username = username.trim()
      return api.patch<User>(`/admin/users/${user!.id}`, p)
    },
    onSuccess: (u) => {
      onSaved(u)
      onClose()
    },
    onError: toastError,
  })
  return (
    <Dialog
      open={!!user}
      onClose={onClose}
      title="Edit person"
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button
            variant="primary"
            disabled={!name.trim() || !email.trim() || !username.trim()}
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
          save.mutate()
        }}
      >
        <Field label="Display name">
          <Input autoFocus value={name} onChange={(e) => setName(e.target.value)} />
        </Field>
        <Field
          label="Username"
          hint="Used for @mentions and signing in. Changing it breaks old @mentions."
        >
          <Input
            value={username}
            onChange={(e) => setUsername(e.target.value)}
            autoCapitalize="off"
            autoCorrect="off"
            spellCheck={false}
          />
        </Field>
        <Field label="Email">
          <Input type="email" value={email} onChange={(e) => setEmail(e.target.value)} />
        </Field>
        <button type="submit" className="hidden" />
      </form>
    </Dialog>
  )
}
