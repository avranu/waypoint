// Admin → Assistants (WP-ADR1): connect a person's Hermes agent through
// its gate, change how Waypoint reaches it, or disconnect it. This is the
// only place its secrets are typed; the API never sends them back.

import { useState } from 'react'
import { Link } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import { BotMessageSquare, MoreHorizontal, Pencil, Plus, Trash2 } from 'lucide-react'
import {
  assistantsQuery,
  health,
  HEALTH_COLOR,
  HEALTH_LABEL,
  useAssistantMutations,
  type Assistant,
  type AssistantInput,
  type AssistantSite,
} from '@/lib/assistant'
import { q } from '@/lib/queries'
import { relativeTime } from '@/lib/format'
import { Button, IconButton } from '@/components/ui/button'
import { Confirm, Dialog } from '@/components/ui/dialog'
import { Field, Input, Select, Textarea } from '@/components/ui/input'
import { Avatar, Empty, Section, Skeleton } from '@/components/ui/misc'
import { MenuList, Popover, usePopover } from '@/components/ui/popover'
import { toast, toastError } from '@/components/ui/toast'
import { TestSteps } from '../assistant/settings/Connection'

/** One or more PEM certificates, checked without a backtracking pattern. */
function looksLikePEM(text: string): boolean {
  const blocks = text.trim().split('-----END CERTIFICATE-----')
  if (blocks.pop()?.trim() !== '' || !blocks.length) return false
  return blocks.every((b) => {
    const [before, body] = b.split('-----BEGIN CERTIFICATE-----')
    return before.trim() === '' && body !== undefined && /^[A-Za-z0-9+/=\s]+$/.test(body)
  })
}

export function AssistantsTab() {
  const list = useQuery(assistantsQuery())
  const [editing, setEditing] = useState<Assistant | 'new' | null>(null)
  const [deleting, setDeleting] = useState<Assistant | null>(null)
  const items = list.data ?? []
  return (
    <>
      <Section
        title="Assistants"
        help="A person's own Hermes agent, reached through the door of its gate. Its owner chats with it and runs it from the Assistant page."
        actions={
          <Button
            size="sm"
            variant="primary"
            icon={<Plus className="size-3.5" />}
            onClick={() => setEditing('new')}
          >
            Connect an assistant
          </Button>
        }
      >
        {list.isPending ? (
          <div className="p-4">
            <Skeleton className="h-14" />
          </div>
        ) : list.isError ? (
          <p className="px-4 py-6 text-center text-[13px] text-danger">
            {list.error instanceof Error ? list.error.message : String(list.error)}
          </p>
        ) : items.length === 0 ? (
          <Empty icon={<BotMessageSquare />} title="No assistants connected" />
        ) : (
          <ul className="divide-y divide-line">
            {items.map((a) => (
              <AssistantRow
                key={a.id}
                a={a}
                onEdit={() => setEditing(a)}
                onDelete={() => setDeleting(a)}
              />
            ))}
          </ul>
        )}
      </Section>
      {editing && (
        <AssistantDialog
          assistant={editing === 'new' ? null : editing}
          site={items[0]?.site}
          onClose={() => setEditing(null)}
        />
      )}
      {deleting && <DeleteConfirm a={deleting} onClose={() => setDeleting(null)} />}
    </>
  )
}

function AssistantRow({
  a,
  onEdit,
  onDelete,
}: {
  a: Assistant
  onEdit: () => void
  onDelete: () => void
}) {
  const m = usePopover()
  const h = health(a)
  const checked = a.state.checked_at && !a.state.checked_at.startsWith('0001')
  return (
    <li className="flex items-center gap-3 px-4 py-3">
      <span
        aria-hidden
        className="size-2.5 shrink-0 rounded-full"
        style={{ background: HEALTH_COLOR[h] }}
      />
      <div className="min-w-0 flex-1">
        <div className="flex flex-wrap items-center gap-x-2">
          {a.mine ? (
            <Link
              to="/assistant/$id"
              params={{ id: String(a.id) }}
              className="truncate text-[13px] font-medium hover:underline"
            >
              {a.name}
            </Link>
          ) : (
            <span className="truncate text-[13px] font-medium">{a.name}</span>
          )}
          <span
            className="text-xs"
            style={{ color: h === 'ok' ? 'var(--wp-ok)' : HEALTH_COLOR[h] }}
          >
            {HEALTH_LABEL[h]}
          </span>
          {checked && (
            <span className="text-xs text-subtle">{relativeTime(a.state.checked_at)}</span>
          )}
        </div>
        <div className="mt-0.5 flex min-w-0 items-center gap-2 text-xs text-muted">
          <span className="flex shrink-0 items-center gap-1.5">
            <Avatar user={a.owner} size={16} />
            {a.owner.display_name || a.owner.username}
          </span>
          <span className="truncate font-mono text-subtle">{a.url}</span>
        </div>
        {a.state.error && h === 'down' && (
          <div className="mt-0.5 truncate text-xs text-danger">{a.state.error}</div>
        )}
      </div>
      <IconButton
        ref={m.anchor}
        label={`Actions for ${a.name}`}
        onClick={m.toggle}
        icon={<MoreHorizontal className="size-4" />}
      />
      <Popover open={m.open} onClose={m.close} anchor={m.anchor} align="end">
        <MenuList
          onClose={m.close}
          items={[
            { label: 'Edit', icon: <Pencil />, onSelect: onEdit },
            { separator: true, label: '' },
            { label: 'Disconnect…', icon: <Trash2 />, danger: true, onSelect: onDelete },
          ]}
        />
      </Popover>
    </li>
  )
}

function DeleteConfirm({ a, onClose }: { a: Assistant; onClose: () => void }) {
  const m = useAssistantMutations(a.id)
  return (
    <Confirm
      open
      onClose={onClose}
      title={`Disconnect ${a.name}?`}
      body={`Waypoint forgets its door and login, and ${a.owner.display_name || a.owner.username} can't reach it from here any more. The agent itself keeps running.`}
      confirmLabel="Disconnect"
      onConfirm={() =>
        m.remove.mutate(undefined, {
          onSuccess: () => toast(`${a.name} disconnected`),
          onError: toastError,
        })
      }
    />
  )
}

/** Connect (assistant null) or change one. Secrets left blank stay as they are. */
function AssistantDialog({
  assistant: a,
  site,
  onClose,
}: {
  assistant: Assistant | null
  /** The site's gate settings, when an assistant has said them. */
  site?: AssistantSite
  onClose: () => void
}) {
  const me = useQuery(q.me()).data
  const users = useQuery(q.users()).data ?? []
  const m = useAssistantMutations(a?.id)
  const [name, setName] = useState(a?.name ?? '')
  const [owner, setOwner] = useState<number>(a?.owner.id ?? me?.id ?? 0)
  const [url, setUrl] = useState(a?.url ?? '')
  const [ca, setCa] = useState('')
  const [clearCa, setClearCa] = useState(false)
  const [key, setKey] = useState('')
  const [keyHeader, setKeyHeader] = useState(a?.key_header ?? '')
  const [agentUrl, setAgentUrl] = useState(a?.agent_url ?? '')
  const [username, setUsername] = useState(a?.username ?? 'waypoint')
  const [password, setPassword] = useState('')
  const [token, setToken] = useState<number>(a?.token_id ?? 0)
  const [tried, setTried] = useState(false)
  const [saved, setSaved] = useState<Assistant | null>(null)
  const ownerIsMe = !!me && owner === me.id
  const tokens = useQuery({ ...q.tokens(), enabled: ownerIsMe }).data ?? []
  const creating = !a
  const busy = m.create.isPending || m.update.isPending

  const pem = ca.trim()
  const errors = {
    name: !name.trim() ? 'Give it a name' : name.trim().length > 60 ? 'At most 60 characters' : '',
    url: !/^https:\/\/[^\s/]+/.test(url.trim()) ? 'An https:// address' : '',
    ca:
      pem && !looksLikePEM(pem)
        ? 'Paste the PEM: -----BEGIN CERTIFICATE----- … -----END CERTIFICATE-----'
        : '',
    key: creating && !key.trim() ? 'Required' : '',
    keyHeader:
      keyHeader.trim() && !/^[A-Za-z0-9][A-Za-z0-9-]{0,63}$/.test(keyHeader.trim())
        ? 'A header name: letters, digits and dashes'
        : '',
    agentUrl:
      agentUrl.trim() && !/^https?:\/\/[^\s/@?#]+(\/[^\s?#]*)?$/.test(agentUrl.trim())
        ? 'An http:// or https:// address'
        : '',
    password: creating && !password ? 'Required' : '',
  }
  const invalid = Object.values(errors).some(Boolean)
  const err = (k: keyof typeof errors) => (tried ? errors[k] : '') || undefined

  const submit = () => {
    setTried(true)
    if (invalid || busy) return
    const body: AssistantInput = {
      name: name.trim(),
      owner_id: owner || undefined,
      url: url.trim(),
      key_header: keyHeader.trim(),
      agent_url: agentUrl.trim(),
      username: username.trim() || 'waypoint',
    }
    if (pem) body.ca_pem = pem
    else if (clearCa) body.ca_pem = ''
    if (key.trim()) body.key = key.trim()
    if (password) body.password = password
    // Only the owner's own tokens fit; moving it to someone else drops it.
    if (ownerIsMe) {
      if (creating ? token : token !== (a?.token_id ?? 0)) body.token_id = token
    } else if (a?.token_id && owner !== a.owner.id) body.token_id = 0
    const opts = {
      onSuccess: (r: Assistant) => {
        toast(creating ? `${r.name} connected` : `${r.name} saved`, { kind: 'success' })
        setSaved(r)
      },
      onError: toastError,
    }
    if (creating) m.create.mutate(body, opts)
    else m.update.mutate(body, opts)
  }

  if (saved)
    return (
      <Dialog
        open
        onClose={onClose}
        title={saved.name}
        footer={
          <>
            {saved.mine && (
              <Link
                to="/assistant/$id"
                params={{ id: String(saved.id) }}
                className="inline-flex h-9 items-center rounded-lg px-3.5 text-sm font-medium text-accent hover:bg-hover"
              >
                Open
              </Link>
            )}
            <Button variant="primary" onClick={onClose}>
              Done
            </Button>
          </>
        }
      >
        <div className="p-4">
          <TestSteps id={saved.id} auto />
        </div>
      </Dialog>
    )

  return (
    <Dialog
      open
      onClose={onClose}
      title={creating ? 'Connect an assistant' : `Edit ${a.name}`}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button variant="primary" onClick={submit} loading={busy}>
            {creating ? 'Connect' : 'Save'}
          </Button>
        </>
      }
    >
      <form
        className="space-y-4 p-4"
        autoComplete="off"
        onSubmit={(e) => {
          e.preventDefault()
          submit()
        }}
      >
        <div className="grid gap-4 sm:grid-cols-2">
          <Field label="Name" error={err('name')}>
            <Input
              autoFocus
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="Assistant"
            />
          </Field>
          <Field label="Owner">
            <Select value={String(owner)} onChange={(e) => setOwner(Number(e.target.value))}>
              {users.length === 0 && me && (
                <option value={String(me.id)}>{me.display_name || me.username}</option>
              )}
              {users.map((u) => (
                <option key={u.id} value={String(u.id)}>
                  {(u.display_name || u.username) + (u.id === me?.id ? ' (you)' : '')}
                </option>
              ))}
            </Select>
          </Field>
        </div>
        <Field
          label="Door"
          hint="The gate's address for Waypoint, from its deploy."
          error={err('url')}
        >
          <Input
            value={url}
            onChange={(e) => setUrl(e.target.value)}
            placeholder="https://gate.example:9443"
            className="font-mono"
            inputMode="url"
            spellCheck={false}
          />
        </Field>
        <Field
          label="CA certificate"
          hint="The CA certificate the gate printed (PEM): the only one Waypoint trusts for this door."
          error={err('ca')}
          note={
            a?.has_ca && !pem ? (
              clearCa ? (
                <>
                  The certificate is removed on save.{' '}
                  <button
                    type="button"
                    className="text-accent hover:underline"
                    onClick={() => setClearCa(false)}
                  >
                    Keep it
                  </button>
                </>
              ) : (
                <>
                  One is set.{' '}
                  <button
                    type="button"
                    className="text-accent hover:underline"
                    onClick={() => setClearCa(true)}
                  >
                    Remove it
                  </button>
                </>
              )
            ) : undefined
          }
        >
          <Textarea
            value={ca}
            onChange={(e) => setCa(e.target.value)}
            rows={4}
            spellCheck={false}
            placeholder={a?.has_ca ? 'unchanged' : '-----BEGIN CERTIFICATE-----'}
            className="font-mono text-xs"
          />
        </Field>
        <Field label="Control key" hint="The door's key." error={err('key')}>
          <Input
            type="password"
            autoComplete="new-password"
            value={key}
            onChange={(e) => setKey(e.target.value)}
            placeholder={creating ? '' : 'unchanged'}
            className="font-mono"
          />
        </Field>
        <div className="grid gap-4 sm:grid-cols-2">
          <Field
            label="Key header"
            hint="The header the door reads its key from. Empty: the site's."
            error={err('keyHeader')}
          >
            <Input
              value={keyHeader}
              onChange={(e) => setKeyHeader(e.target.value)}
              placeholder={a?.site.key_header ?? site?.key_header ?? 'X-Gate-Key'}
              className="font-mono"
              autoComplete="off"
              spellCheck={false}
            />
          </Field>
          <Field
            label="Gate, as the agent reaches it"
            hint="Where the gate serves connections on the agent's network. Empty: the site's."
            error={err('agentUrl')}
          >
            <Input
              value={agentUrl}
              onChange={(e) => setAgentUrl(e.target.value)}
              placeholder={a?.site.agent_url || site?.agent_url || 'http://gate:8080'}
              className="font-mono"
              inputMode="url"
              spellCheck={false}
            />
          </Field>
        </div>
        <div className="grid gap-4 sm:grid-cols-2">
          <Field label="Hermes username">
            <Input
              value={username}
              onChange={(e) => setUsername(e.target.value)}
              autoComplete="off"
              spellCheck={false}
            />
          </Field>
          <Field label="Hermes password" error={err('password')}>
            <Input
              type="password"
              autoComplete="new-password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              placeholder={creating ? '' : 'unchanged'}
            />
          </Field>
        </div>
        {ownerIsMe ? (
          <Field
            label="The agent's Waypoint token"
            hint="One of your API tokens, for the agent to use Waypoint as you. Its permissions are set under Settings → Tokens."
          >
            <Select value={String(token)} onChange={(e) => setToken(Number(e.target.value))}>
              <option value="0">None</option>
              {tokens.map((t) => (
                <option key={t.id} value={String(t.id)}>
                  {t.name + (t.agent ? ` · ${t.agent}` : '') + ` (${t.prefix}…)`}
                </option>
              ))}
            </Select>
          </Field>
        ) : (
          a?.token_id &&
          owner === a.owner.id && (
            <Field label="The agent's Waypoint token">
              <div className="text-[13px] text-muted">{a.token || `#${a.token_id}`}</div>
            </Field>
          )
        )}
        <button type="submit" hidden />
      </form>
    </Dialog>
  )
}
