import { useState } from 'react'
import { useNavigate } from '@tanstack/react-router'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { ChevronDown, UserMinus, UserPlus } from 'lucide-react'
import { api, enc } from '@/lib/api'
import { q } from '@/lib/queries'
import { Button, IconButton } from '@/components/ui/button'
import { Confirm } from '@/components/ui/dialog'
import { Select } from '@/components/ui/input'
import { Avatar, Badge } from '@/components/ui/misc'
import { Picker, Popover, usePopover } from '@/components/ui/popover'
import { toast, toastError } from '@/components/ui/toast'
import type { Member, Project, UserRef } from '@/lib/types'
import { List, TabIntro, isOwner, useRefreshProject } from './shared'

type EditRole = 'owner' | 'editor' | 'viewer'

const ROLES: { role: EditRole; label: string; help: string }[] = [
  {
    role: 'owner',
    label: 'Owner',
    help: 'Everything, including settings, members, automations and deleting the project.',
  },
  { role: 'editor', label: 'Editor', help: 'Create and edit issues, sprints and labels.' },
  { role: 'viewer', label: 'Viewer', help: 'Can see and follow everything, but not change it.' },
]

export function MembersTab({ project }: { project: Project }) {
  const owner = isOwner(project)
  const refresh = useRefreshProject(project.key)
  const me = useQuery(q.me()).data
  const members = [...(project.members ?? [])].sort(
    (a, b) =>
      ROLES.findIndex((r) => r.role === a.role) - ROLES.findIndex((r) => r.role === b.role) ||
      a.user.display_name.localeCompare(b.user.display_name),
  )
  const [removing, setRemoving] = useState<Member | null>(null)
  const [demoting, setDemoting] = useState<EditRole | null>(null)

  const setRole = useMutation({
    mutationFn: ({ username, role }: { username: string; role: EditRole }) =>
      api.put(`/projects/${enc(project.key)}/members/${enc(username)}`, { role }),
    onError: toastError,
    onSettled: refresh,
  })
  const qc = useQueryClient()
  const navigate = useNavigate()
  const remove = useMutation({
    mutationFn: (username: string) =>
      api.del(`/projects/${enc(project.key)}/members/${enc(username)}`),
    onSuccess: (_d, username) => {
      if (username === me?.username) {
        qc.invalidateQueries({ queryKey: ['projects'] })
        toast(`You left ${project.name}`)
        navigate({ to: '/' })
      }
    },
    onError: toastError,
    onSettled: refresh,
  })

  const leaving = removing?.user.username === me?.username
  return (
    <div className="space-y-5">
      <TabIntro title="Members">
        <ul className="space-y-0.5">
          {ROLES.map((r) => (
            <li key={r.role}>
              <b>{r.label}</b>: {r.help}
            </li>
          ))}
        </ul>
        <p className="mt-1.5">
          To involve someone in one issue only, use <b>Share</b> on that issue.
        </p>
      </TabIntro>

      {owner && <AddMember project={project} />}

      <List>
        {members.map((m) => {
          const self = m.user.username === me?.username
          return (
            <div key={m.user.id} className="flex items-center gap-3 px-3 py-2">
              <Avatar user={m.user} size={30} />
              <div className="min-w-0 flex-1">
                <div className="flex items-center gap-1.5 truncate text-[13px] font-medium">
                  {m.user.display_name || m.user.username}
                  {self && <Badge>You</Badge>}
                </div>
                <div className="truncate text-xs text-subtle">@{m.user.username}</div>
              </div>
              {owner ? (
                <Select
                  value={m.role}
                  onChange={(e) => {
                    const role = e.target.value as EditRole
                    if (self && m.role === 'owner' && role !== 'owner') setDemoting(role)
                    else setRole.mutate({ username: m.user.username, role })
                  }}
                  className="h-8 w-28 text-[13px]"
                  aria-label={`Role of ${m.user.display_name}`}
                >
                  {ROLES.map((r) => (
                    <option key={r.role} value={r.role}>
                      {r.label}
                    </option>
                  ))}
                </Select>
              ) : (
                <span className="text-xs capitalize text-muted">{m.role}</span>
              )}
              {(owner || self) && (
                <IconButton
                  label={self ? 'Leave project' : `Remove ${m.user.display_name}`}
                  onClick={() => setRemoving(m)}
                  icon={<UserMinus className="size-3.5" />}
                />
              )}
            </div>
          )
        })}
      </List>

      <Confirm
        open={!!demoting}
        onClose={() => setDemoting(null)}
        onConfirm={() =>
          demoting && me && setRole.mutate({ username: me.username, role: demoting })
        }
        title={`Stop being an owner of ${project.name}?`}
        body="You won't be able to manage members, settings or automations unless another owner makes you an owner again."
        confirmLabel={`Become ${demoting ?? ''}`}
      />
      <Confirm
        open={!!removing}
        onClose={() => setRemoving(null)}
        onConfirm={() => removing && remove.mutate(removing.user.username)}
        title={leaving ? `Leave ${project.name}?` : `Remove ${removing?.user.display_name}?`}
        body={
          leaving
            ? "You'll lose access unless someone adds you back."
            : 'They lose access to this project. Their issues, comments and time entries stay.'
        }
        confirmLabel={leaving ? 'Leave' : 'Remove'}
      />
    </div>
  )
}

function AddMember({ project }: { project: Project }) {
  const refresh = useRefreshProject(project.key)
  const users = useQuery(q.users()).data ?? []
  const memberIds = new Set((project.members ?? []).map((m) => m.user.id))
  const candidates = users.filter((u) => !memberIds.has(u.id))
  const [user, setUser] = useState<UserRef | null>(null)
  const [role, setRole] = useState<EditRole>('editor')
  const pop = usePopover()
  const add = useMutation({
    mutationFn: () =>
      api.put(`/projects/${enc(project.key)}/members/${enc(user!.username)}`, { role }),
    onSuccess: () => {
      toast(`Added ${user!.display_name || user!.username}`, { kind: 'success' })
      setUser(null)
    },
    onError: toastError,
    onSettled: refresh,
  })
  if (users.length > 0 && candidates.length === 0) {
    return (
      <p className="text-xs text-subtle">
        Everyone here is already a member. Admins invite more in Admin → Invites.
      </p>
    )
  }
  return (
    <div className="flex flex-wrap items-center gap-2 rounded-xl border border-line bg-panel p-3">
      <UserPlus className="hidden size-4 text-muted sm:block" />
      <button
        ref={pop.anchor}
        type="button"
        onClick={pop.toggle}
        className="flex h-9 min-w-0 flex-1 basis-48 items-center gap-2 rounded-lg border border-line bg-panel px-3 text-left text-sm hover:bg-hover"
      >
        {user ? (
          <>
            <Avatar user={user} size={20} />
            <span className="truncate">{user.display_name || user.username}</span>
          </>
        ) : (
          <span className="text-subtle">Choose a person…</span>
        )}
        <ChevronDown className="ml-auto size-4 shrink-0 text-subtle" />
      </button>
      <Popover open={pop.open} onClose={pop.close} anchor={pop.anchor}>
        <Picker
          items={candidates.map((u) => ({
            value: u.id,
            label: u.display_name || u.username,
            hint: '@' + u.username,
            keywords: u.username,
            icon: <Avatar user={u} size={20} />,
          }))}
          selected={user ? [user.id] : []}
          onPick={(id) => setUser(candidates.find((u) => u.id === id) ?? null)}
          onClose={pop.close}
          placeholder="Search people…"
        />
      </Popover>
      <Select
        value={role}
        onChange={(e) => setRole(e.target.value as EditRole)}
        className="w-28"
        aria-label="Role"
      >
        {ROLES.map((r) => (
          <option key={r.role} value={r.role}>
            {r.label}
          </option>
        ))}
      </Select>
      <Button
        variant="primary"
        disabled={!user}
        loading={add.isPending}
        onClick={() => add.mutate()}
      >
        Add
      </Button>
    </div>
  )
}
