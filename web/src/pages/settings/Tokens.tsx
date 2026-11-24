import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Bot, KeyRound, Plus, ShieldCheck, Trash2 } from 'lucide-react'
import { api } from '@/lib/api'
import { q } from '@/lib/queries'
import { agentLabel } from '@/lib/agents'
import { plural, relativeTime, shortDate } from '@/lib/format'
import type { APIToken } from '@/lib/types'
import { Button, IconButton } from '@/components/ui/button'
import { Confirm, Dialog } from '@/components/ui/dialog'
import { Field, Input, Switch } from '@/components/ui/input'
import { Badge, Empty, Section, Skeleton } from '@/components/ui/misc'
import { toastError } from '@/components/ui/toast'
import { CopyField, Warning } from './common'
import {
  AccessChoice,
  PermissionEditor,
  accessBody,
  accessOf,
  type Access,
} from './TokenPermissions'

/** Short summary of what a token may do. */
function accessLabel(t: APIToken) {
  const a = accessOf(t)
  return a === 'custom'
    ? plural(t.permissions!.length, 'permission')
    : a === 'read'
      ? 'read-only'
      : 'full access'
}

/** The Assistant preset, or nothing if the catalog isn't there. */
function useAssistantPreset() {
  const cat = useQuery(q.tokenPermissions())
  return cat.data?.presets.find((p) => p.key === 'assistant')?.permissions ?? []
}

export function TokensTab() {
  const qc = useQueryClient()
  const tokens = useQuery(q.tokens())
  const [creating, setCreating] = useState(false)
  const [revoking, setRevoking] = useState<APIToken | null>(null)
  const [editingId, setEditingId] = useState<number | null>(null)
  const revoke = useMutation({
    mutationFn: (id: number) => api.del(`/me/tokens/${id}`),
    onSettled: () => qc.invalidateQueries({ queryKey: ['tokens'] }),
    onError: toastError,
  })
  return (
    <>
      <Section
        title="API tokens"
        help={
          <>
            For scripts and AI agents, as you, over the{' '}
            <a
              href="/api/v1/help"
              target="_blank"
              rel="noreferrer"
              className="text-accent hover:underline"
            >
              REST API
            </a>{' '}
            or MCP. Send as <b>Authorization: Bearer …</b>. Limit a token to read-only, or to chosen
            permissions.
          </>
        }
        actions={
          <Button
            size="sm"
            variant="primary"
            icon={<Plus className="size-3.5" />}
            onClick={() => setCreating(true)}
          >
            New token
          </Button>
        }
      >
        {tokens.isPending ? (
          <div className="space-y-2 p-4">
            <Skeleton className="h-10" />
            <Skeleton className="h-10" />
          </div>
        ) : (tokens.data ?? []).length === 0 ? (
          <Empty icon={<KeyRound />} title="No tokens yet">
            Create one for each script or agent, so you can revoke them separately.
          </Empty>
        ) : (
          <ul className="divide-y divide-line">
            {tokens.data!.map((t) => {
              const expired = t.expires_at && new Date(t.expires_at) < new Date()
              return (
                <li key={t.id} className="flex items-center gap-3 px-4 py-3">
                  <KeyRound className="size-4 shrink-0 text-subtle" />
                  <div className="min-w-0 flex-1">
                    <div className="flex flex-wrap items-center gap-1.5">
                      <span className="truncate text-[13px] font-medium">{t.name}</span>
                      <code className="font-mono text-xs text-subtle">{t.prefix}…</code>
                      <Badge>{accessLabel(t)}</Badge>
                      {!!t.refusals?.length && (
                        <button type="button" onClick={() => setEditingId(t.id)}>
                          <Badge color="var(--wp-warn)">
                            {t.refusals.reduce((n, r) => n + r.count, 0)} refused
                          </Badge>
                        </button>
                      )}
                      {t.agent && (
                        <Badge>
                          <Bot className="mr-1 inline size-3" />
                          {agentLabel(t.agent)}
                        </Badge>
                      )}
                      {expired && <Badge color="var(--wp-danger)">expired</Badge>}
                    </div>
                    <div className="mt-0.5 text-xs text-subtle">
                      Created {relativeTime(t.created_at)} ·{' '}
                      {t.last_used_at ? `last used ${relativeTime(t.last_used_at)}` : 'never used'}
                      {t.expires_at && !expired && ` · expires ${shortDate(t.expires_at)}`}
                      {!t.expires_at && ' · never expires'}
                    </div>
                  </div>
                  <IconButton
                    label="Permissions"
                    onClick={() => setEditingId(t.id)}
                    icon={<ShieldCheck className="size-3.5" />}
                  />
                  <IconButton
                    label="Revoke"
                    onClick={() => setRevoking(t)}
                    icon={<Trash2 className="size-3.5" />}
                  />
                </li>
              )
            })}
          </ul>
        )}
      </Section>
      <CreateTokenDialog open={creating} onClose={() => setCreating(false)} />
      {editingId !== null && (
        <EditAccessDialog
          key={editingId}
          token={tokens.data?.find((t) => t.id === editingId)}
          onClose={() => setEditingId(null)}
        />
      )}
      <Confirm
        open={!!revoking}
        onClose={() => setRevoking(null)}
        title={`Revoke “${revoking?.name}”?`}
        body="Anything using this token will stop working immediately."
        confirmLabel="Revoke"
        onConfirm={() => revoking && revoke.mutate(revoking.id)}
      />
    </>
  )
}

export function CreateTokenDialog({ open, onClose }: { open: boolean; onClose: () => void }) {
  const qc = useQueryClient()
  const [name, setName] = useState('')
  const [access, setAccess] = useState<Access>('full')
  const [perms, setPerms] = useState<string[]>([])
  const assistant = useAssistantPreset()
  const [isAgent, setIsAgent] = useState(false)
  const [agent, setAgent] = useState('')
  const [days, setDays] = useState('')
  const [secret, setSecret] = useState<string | null>(null)
  const close = () => {
    onClose()
    setName('')
    setAccess('full')
    setPerms([])
    setIsAgent(false)
    setAgent('')
    setDays('')
    setSecret(null)
  }
  // Anything but blank must be a whole number of days, so a typo can't mint a token that never expires.
  const n = days.trim() ? Number(days) : null
  const badDays = n !== null && !(Number.isInteger(n) && n > 0 && n <= 3650)
  const create = useMutation({
    mutationFn: () =>
      api.post<{ token: string; info: APIToken }>('/me/tokens', {
        name: name.trim(),
        ...(access === 'custom'
          ? { permissions: perms, read_only: false }
          : { read_only: access === 'read' }),
        ...(isAgent ? { agent: agent.trim() } : {}),
        ...(n !== null ? { expires_in_days: n } : {}),
      }),
    onSuccess: (r) => {
      setSecret(r.token)
      qc.invalidateQueries({ queryKey: ['tokens'] })
    },
    onError: toastError,
  })
  return (
    <Dialog
      open={open}
      onClose={close}
      title={secret ? 'Token created' : 'New API token'}
      footer={
        secret ? (
          <Button variant="primary" onClick={close}>
            Done
          </Button>
        ) : (
          <>
            <Button variant="ghost" onClick={close}>
              Cancel
            </Button>
            <Button
              variant="primary"
              disabled={!name.trim() || badDays || (isAgent && !agent.trim())}
              loading={create.isPending}
              onClick={() => create.mutate()}
            >
              Create token
            </Button>
          </>
        )
      }
    >
      {secret ? (
        <div className="space-y-3 p-4">
          <Warning>
            Copy this token now. It won't be shown again — if you lose it, revoke it and make a new
            one.
          </Warning>
          <CopyField value={secret} />
        </div>
      ) : (
        <form
          className="space-y-4 p-4"
          onSubmit={(e) => {
            e.preventDefault()
            if (name.trim() && !badDays && !(isAgent && !agent.trim()) && !create.isPending)
              create.mutate()
          }}
        >
          <Field label="Name" hint="What will use it, e.g. “Claude Code on laptop”.">
            <Input
              autoFocus
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="Claude Code"
            />
          </Field>
          <AccessChoice
            value={access}
            onChange={(a) => {
              setAccess(a)
              if (a === 'custom' && !perms.length) setPerms(assistant)
            }}
          />
          {access === 'custom' && <PermissionEditor value={perms} onChange={setPerms} />}
          <div className="flex items-center justify-between gap-3">
            <div>
              <div className="text-[13px] font-medium">Agent token</div>
              <div className="text-xs text-subtle">
                For an AI agent working for you: what it does is marked as the agent's, and it can
                take issues meant for agents.
              </div>
            </div>
            <Switch checked={isAgent} onChange={setIsAgent} label="Agent token" />
          </div>
          {isAgent && (
            <Field
              label="Agent"
              hint="provider/model, e.g. anthropic/claude-opus-5-5. A request can name another with the X-Waypoint-Agent header."
            >
              <Input
                value={agent}
                onChange={(e) => setAgent(e.target.value)}
                placeholder="anthropic/claude-opus-5-5"
                spellCheck={false}
                autoComplete="off"
              />
            </Field>
          )}
          <Field
            label="Expires after (days)"
            hint="Leave blank for a token that never expires."
            error={badDays ? 'Use a whole number of days, 1 to 3650.' : undefined}
          >
            <Input
              type="number"
              min={1}
              inputMode="numeric"
              value={days}
              onChange={(e) => setDays(e.target.value)}
              placeholder="Never"
              className="sm:w-40"
            />
          </Field>
          <button type="submit" className="hidden" />
        </form>
      )}
    </Dialog>
  )
}

/** Change what an existing token may do, and see what it was refused. */
function EditAccessDialog({ token, onClose }: { token?: APIToken; onClose: () => void }) {
  const qc = useQueryClient()
  const assistant = useAssistantPreset()
  const [access, setAccess] = useState<Access>(token ? accessOf(token) : 'full')
  const [perms, setPerms] = useState<string[]>(token?.permissions ?? [])
  const save = useMutation({
    mutationFn: () => api.patch(`/me/tokens/${token!.id}`, accessBody(access, perms)),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['tokens'] })
      onClose()
    },
    onError: toastError,
  })
  const clear = useMutation({
    mutationFn: () => api.del(`/me/tokens/${token!.id}/refusals`),
    onSettled: () => qc.invalidateQueries({ queryKey: ['tokens'] }),
    onError: toastError,
  })
  const cat = useQuery(q.tokenPermissions())
  const label = (key: string) =>
    cat.data?.groups.flatMap((g) => g.permissions).find((p) => p.key === key)?.label ?? key
  const allow = (key: string) => {
    const base = access === 'custom' ? perms : assistant
    setAccess('custom')
    setPerms(base.includes(key) ? base : [...base, key])
  }
  const refusals = token?.refusals ?? []
  return (
    <Dialog
      open
      onClose={onClose}
      title={token ? `Permissions for “${token.name}”` : 'Permissions'}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button
            variant="primary"
            disabled={!token}
            loading={save.isPending}
            onClick={() => save.mutate()}
          >
            Save
          </Button>
        </>
      }
    >
      <div className="space-y-4 p-4">
        {refusals.length > 0 && (
          <div className="space-y-2 rounded-lg border border-warn/30 bg-warn/10 p-3">
            <div className="flex items-center justify-between gap-2">
              <div className="text-[13px] font-medium">Refused recently</div>
              <Button
                size="xs"
                variant="ghost"
                loading={clear.isPending}
                onClick={() => clear.mutate()}
              >
                Clear
              </Button>
            </div>
            <ul className="space-y-2">
              {refusals.map((r) => (
                <li key={r.permission} className="flex items-start gap-2">
                  <div className="min-w-0 flex-1 text-xs">
                    <div className="text-[13px]">
                      {label(r.permission)} · {r.count}× · {relativeTime(r.last_at)}
                    </div>
                    <div className="break-words font-mono text-subtle">{r.what}</div>
                  </div>
                  <Button
                    size="xs"
                    disabled={access === 'custom' && perms.includes(r.permission)}
                    onClick={() => allow(r.permission)}
                  >
                    {access === 'custom' && perms.includes(r.permission) ? 'Allowed' : 'Allow'}
                  </Button>
                </li>
              ))}
            </ul>
          </div>
        )}
        <AccessChoice
          value={access}
          onChange={(a) => {
            setAccess(a)
            if (a === 'custom' && !perms.length) setPerms(assistant)
          }}
        />
        {access === 'custom' && <PermissionEditor value={perms} onChange={setPerms} />}
      </div>
    </Dialog>
  )
}
