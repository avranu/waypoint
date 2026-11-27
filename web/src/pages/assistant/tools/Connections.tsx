import { useEffect, useRef, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import clsx from 'clsx'
import {
  Check,
  Clock,
  Globe,
  KeyRound,
  MoreHorizontal,
  Pencil,
  Plug,
  Plus,
  RotateCw,
  ShieldCheck,
  ShieldX,
  Trash2,
  X,
} from 'lucide-react'
import { api, enc } from '@/lib/api'
import {
  approvalsQuery,
  connectionsQuery,
  hermes,
  iso,
  type Assistant,
  type GateApproval,
  type GateConnection,
} from '@/lib/assistant'
import { relativeTime } from '@/lib/format'
import { useLocal } from '@/lib/hooks'
import { Button, IconButton } from '@/components/ui/button'
import { Confirm } from '@/components/ui/dialog'
import { Badge, Empty, Section, Skeleton } from '@/components/ui/misc'
import { MenuList, Popover, usePopover } from '@/components/ui/popover'
import { toast, toastError } from '@/components/ui/toast'
import { ConnectionDialog, RotateDialog } from './ConnectionForm'
import { approvalConn, connMcpUrl, gateAgentUrl, latestApprovals, policyWords } from './connections'
import { mcpServersQuery, type McpServer } from './hermes-mcp'

const hasPending = (d: { pending: GateApproval[] | null } | undefined) =>
  (d?.pending ?? []).some((a) => approvalConn(a) !== null)

export function ConnectionsSection({ assistant }: { assistant: Assistant }) {
  const id = assistant.id
  const gate = gateAgentUrl(assistant)
  const qc = useQueryClient()
  const conns = useQuery(connectionsQuery(id))
  const servers = useQuery(mcpServersQuery(id))
  const approvals = useQuery({
    ...approvalsQuery(id),
    refetchInterval: (q) => (hasPending(q.state.data) ? 5000 : false),
  })
  const [dialog, setDialog] = useState<{ edit?: GateConnection } | null>(null)
  const [rotate, setRotate] = useState<GateConnection | null>(null)
  const [remove, setRemove] = useState<GateConnection | null>(null)
  const [dismissed, setDismissed] = useLocal<string[]>(`assistant:${id}:conn-dismissed`, [])

  const link = useMutation({
    mutationFn: (name: string) =>
      hermes<McpServer>(id, 'POST', 'mcp/servers', { name, url: connMcpUrl(gate, name) }),
    onSuccess: (_, name) => {
      qc.invalidateQueries({ queryKey: mcpServersQuery(id).queryKey })
      toast(`Added ${name} to ${assistant.name}; new conversations can use it`, { kind: 'success' })
    },
    onError: toastError,
  })

  const latest = latestApprovals(approvals.data)

  const seen = useRef<Set<string> | null>(null)
  useEffect(() => {
    if (!approvals.data) return
    const now = new Set(
      (approvals.data.pending ?? []).filter((a) => approvalConn(a)).map((a) => a.code),
    )
    const before = seen.current
    seen.current = now
    if (!before) return
    const answered = [...before].filter((c) => !now.has(c))
    if (!answered.length) return
    qc.invalidateQueries({ queryKey: connectionsQuery(id).queryKey })
    for (const code of answered) {
      const a = (approvals.data.recent ?? []).find((x) => x.code === code)
      const name = a && approvalConn(a)
      if (!a || !name) continue
      if (a.status === 'approved' || a.status === 'done')
        toast(`Approved: ${name} is saved at the gate`, { kind: 'success' })
      else if (a.status === 'denied') toast(`Declined on Signal: ${name}`, { kind: 'error' })
      else if (a.status === 'expired') toast(`Not approved in time: ${name}`, { kind: 'error' })
    }
  }, [approvals.data, id, qc])

  const list = conns.data ?? []
  const byName = new Map(list.map((c) => [c.name, c]))
  const linked = new Map<string, McpServer>()
  for (const s of servers.data?.servers ?? []) {
    const c = gate ? list.find((c) => s.url === connMcpUrl(gate, c.name)) : undefined
    if (c) linked.set(c.name, s)
  }

  const outcome = (name: string): GateApproval | undefined => {
    const a = latest.get(name)
    if (!a) return undefined
    if (a.status === 'pending') return a
    if (!['denied', 'expired', 'failed'].includes(a.status) || dismissed.includes(a.code))
      return undefined
    const c = byName.get(name)
    const at = a.decided ?? a.created
    if (!c) return Date.now() / 1000 - at < 7 * 86400 ? a : undefined
    return at > (c.updated ?? 0) ? a : undefined
  }
  const names = [
    ...new Set([...list.map((c) => c.name), ...[...latest.keys()].filter((n) => outcome(n))]),
  ].sort()

  return (
    <Section
      title={
        <span className="flex items-center gap-2">
          <Plug className="size-4 text-subtle" /> Connections
        </span>
      }
      helpId="assistant:connections"
      help={`Services ${assistant.name} reaches through the gate. The gate holds their credentials; ${assistant.name} never sees them.`}
      actions={
        list.length > 0 || names.length > 0 ? (
          <Button size="sm" icon={<Plus className="size-3.5" />} onClick={() => setDialog({})}>
            Add
          </Button>
        ) : undefined
      }
    >
      {conns.isPending ? (
        <div className="space-y-3 p-4">
          <Skeleton className="h-14" />
          <Skeleton className="h-14" />
        </div>
      ) : conns.isError ? (
        <p className="p-4 text-[13px] text-danger">
          Couldn't load the gate's connections: {conns.error.message}
        </p>
      ) : names.length === 0 ? (
        <Empty
          icon={<Plug />}
          title="No connections yet"
          action={
            <Button
              variant="primary"
              icon={<Plus className="size-4" />}
              onClick={() => setDialog({})}
            >
              Add a connection
            </Button>
          }
        >
          Give {assistant.name} a service. Anything that widens its reach waits for your approval on
          Signal.
        </Empty>
      ) : (
        <ul className="divide-y divide-line">
          {names.map((n) => (
            <ConnectionRow
              key={n}
              agent={assistant.name}
              name={n}
              conn={byName.get(n)}
              approval={outcome(n)}
              server={linked.get(n)}
              serversLoaded={servers.isSuccess && !!gate}
              linking={link.isPending && link.variables === n}
              onLink={() => link.mutate(n)}
              onEdit={(c) => setDialog({ edit: c })}
              onRotate={setRotate}
              onRemove={setRemove}
              onDismiss={(code) => setDismissed([...dismissed.slice(-50), code])}
            />
          ))}
        </ul>
      )}
      {!gate && list.length > 0 && (
        <p className="border-t border-line px-4 py-2.5 text-xs text-muted">
          Waypoint doesn't know the gate's address on {assistant.name}'s network, so it can't add
          these to {assistant.name} for you. An admin sets it in Admin → Assistants.
        </p>
      )}
      <ConnectionDialog
        assistantId={id}
        open={dialog !== null}
        existing={dialog?.edit}
        taken={(n) => byName.has(n) || latest.get(n)?.status === 'pending'}
        onClose={() => setDialog(null)}
      />
      {rotate && (
        <RotateDialog assistantId={id} conn={rotate} open onClose={() => setRotate(null)} />
      )}
      <RemoveConnection
        assistantId={id}
        agent={assistant.name}
        conn={remove}
        server={remove ? linked.get(remove.name) : undefined}
        onClose={() => setRemove(null)}
        onRemoved={(name) => {
          const code = latest.get(name)?.code
          if (code) setDismissed([...dismissed.slice(-50), code])
        }}
      />
    </Section>
  )
}

function ConnectionRow({
  agent,
  name,
  conn,
  approval,
  server,
  serversLoaded,
  linking,
  onLink,
  onEdit,
  onRotate,
  onRemove,
  onDismiss,
}: {
  agent: string
  name: string
  conn?: GateConnection
  approval?: GateApproval
  server?: McpServer
  serversLoaded: boolean
  linking: boolean
  onLink: () => void
  onEdit: (c: GateConnection) => void
  onRotate: (c: GateConnection) => void
  onRemove: (c: GateConnection) => void
  onDismiss: (code: string) => void
}) {
  const menu = usePopover()
  const headers = conn?.header_names ?? []
  const Icon = conn?.kind === 'mcp' ? Plug : Globe
  return (
    <li className="flex items-start gap-3 px-4 py-3">
      <div
        className={clsx(
          'mt-0.5 flex size-8 shrink-0 items-center justify-center rounded-lg',
          conn || approval?.status !== 'pending' ? 'bg-sunken text-muted' : 'bg-warn/10 text-warn',
        )}
      >
        {conn ? (
          <Icon className="size-4" />
        ) : approval?.status === 'pending' ? (
          <Clock className="size-4" />
        ) : (
          <Plug className="size-4" />
        )}
      </div>
      <div className="min-w-0 flex-1 space-y-1">
        <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
          <span className="font-mono text-[13px] font-medium">{name}</span>
          {conn && <Badge>{conn.kind === 'mcp' ? 'MCP server' : 'HTTP API'}</Badge>}
          {server && (
            <Badge color={server.enabled ? 'var(--wp-ok)' : undefined}>
              <Check className="size-3" />
              {server.enabled ? `In ${agent}` : `In ${agent} · off`}
            </Badge>
          )}
        </div>
        {conn && (
          <>
            <div className="break-all font-mono text-xs text-muted">{conn.url}</div>
            {conn.description && (
              <p className="whitespace-pre-line break-words text-[13px] text-muted">
                {conn.description}
              </p>
            )}
            <div className="flex items-start gap-1.5 text-[13px]">
              <ShieldCheck className="mt-0.5 size-3.5 shrink-0 text-subtle" />
              <span className="min-w-0 break-words">{policyWords(conn)}</span>
            </div>
            {headers.length > 0 && (
              <div className="flex flex-wrap items-center gap-1">
                <KeyRound className="size-3.5 text-subtle" aria-label="Headers the gate adds" />
                {headers.map((h) => (
                  <span
                    key={h}
                    className="rounded bg-sunken px-1.5 py-0.5 font-mono text-[11px] text-muted"
                  >
                    {h}
                  </span>
                ))}
              </div>
            )}
          </>
        )}
        {approval && <Outcome approval={approval} hasConn={!!conn} onDismiss={onDismiss} />}
        {conn && serversLoaded && !server && (
          <div className="flex flex-wrap items-center gap-2 pt-1">
            <span className="text-xs text-muted">{agent} doesn't use it yet.</span>
            <Button
              size="sm"
              variant="secondary"
              icon={<Plus className="size-3.5" />}
              loading={linking}
              onClick={onLink}
            >
              Add to {agent}
            </Button>
          </div>
        )}
      </div>
      {conn && (
        <>
          <IconButton
            ref={menu.anchor}
            label={`Actions for ${name}`}
            size="md"
            icon={<MoreHorizontal className="size-4" />}
            onClick={menu.toggle}
            aria-haspopup="menu"
            aria-expanded={menu.open}
          />
          <Popover open={menu.open} onClose={menu.close} anchor={menu.anchor} align="end">
            <MenuList
              onClose={menu.close}
              items={[
                { label: 'Edit', icon: <Pencil />, onSelect: () => onEdit(conn) },
                ...(headers.length
                  ? [
                      {
                        label: 'New credentials',
                        icon: <RotateCw />,
                        onSelect: () => onRotate(conn),
                      },
                    ]
                  : []),
                { separator: true, label: '' },
                { label: 'Remove', icon: <Trash2 />, danger: true, onSelect: () => onRemove(conn) },
              ]}
            />
          </Popover>
        </>
      )}
    </li>
  )
}

function Outcome({
  approval: a,
  hasConn,
  onDismiss,
}: {
  approval: GateApproval
  hasConn: boolean
  onDismiss: (code: string) => void
}) {
  const when = relativeTime(iso(a.decided ?? a.created))
  if (a.status === 'pending')
    return (
      <div className="rounded-lg border border-warn/30 bg-warn/5 px-2.5 py-2 text-[13px]">
        <div className="flex items-center gap-1.5 font-medium">
          <Clock className="size-3.5 shrink-0 text-warn" />
          <span>
            Waiting for your approval on Signal ·{' '}
            <span className="font-mono tracking-wide">{a.code}</span>
          </span>
        </div>
        {a.summary && (
          <p className="mt-1 break-words text-xs text-muted">
            {hasConn ? 'Asked: ' : ''}
            {a.summary}
          </p>
        )}
      </div>
    )
  const text =
    a.status === 'denied'
      ? hasConn
        ? 'You declined the change on Signal'
        : 'You declined it on Signal'
      : a.status === 'expired'
        ? 'Not approved in time'
        : `Approved, but the gate couldn't save it${a.result ? `: ${a.result}` : ''}`
  return (
    <div className="flex items-start gap-1.5 rounded-lg bg-danger/5 px-2.5 py-1.5 text-[13px]">
      <ShieldX className="mt-0.5 size-3.5 shrink-0 text-danger" />
      <span className="min-w-0 flex-1 break-words">
        {text} · <span className="font-mono">{a.code}</span>
        <span className="text-subtle"> · {when}</span>
      </span>
      <button
        type="button"
        aria-label="Dismiss"
        title="Dismiss"
        onClick={() => onDismiss(a.code)}
        className="-my-1 -mr-1.5 flex size-7 shrink-0 items-center justify-center rounded-md text-subtle hover:bg-hover hover:text-fg"
      >
        <X className="size-3.5" />
      </button>
    </div>
  )
}

function RemoveConnection({
  assistantId: id,
  agent,
  conn,
  server,
  onClose,
  onRemoved,
}: {
  assistantId: number
  agent: string
  conn: GateConnection | null
  server?: McpServer
  onClose: () => void
  onRemoved: (name: string) => void
}) {
  const qc = useQueryClient()
  const remove = useMutation({
    mutationFn: async ({ name, server }: { name: string; server?: string }) => {
      await api.del(`/assistants/${id}/gate/connections/${enc(name)}`)
      if (server) await hermes(id, 'DELETE', `mcp/servers/${enc(server)}`)
    },
    onSuccess: (_, { name }) => {
      onRemoved(name)
      toast(`Removed ${name}`)
    },
    onError: toastError,
    onSettled: () => {
      qc.invalidateQueries({ queryKey: connectionsQuery(id).queryKey })
      qc.invalidateQueries({ queryKey: mcpServersQuery(id).queryKey })
    },
  })
  return (
    <Confirm
      open={!!conn}
      onClose={onClose}
      title={`Remove ${conn?.name ?? ''}?`}
      body={
        <>
          {agent} loses it at once and the gate deletes its credentials
          {server ? <>, and it leaves {agent}'s MCP servers</> : null}. Adding it back asks you on
          Signal again.
        </>
      }
      confirmLabel="Remove"
      onConfirm={() => conn && remove.mutate({ name: conn.name, server: server?.name })}
    />
  )
}
