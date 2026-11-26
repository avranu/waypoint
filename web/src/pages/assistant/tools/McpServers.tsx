import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import clsx from 'clsx'
import { FlaskConical, Lock, MoreHorizontal, Server, Trash2, X } from 'lucide-react'
import { enc } from '@/lib/api'
import { connectionsQuery, hermes, type Assistant } from '@/lib/assistant'
import { plural } from '@/lib/format'
import { Button, IconButton } from '@/components/ui/button'
import { Confirm } from '@/components/ui/dialog'
import { Switch } from '@/components/ui/input'
import { Badge, Empty, Section, Skeleton } from '@/components/ui/misc'
import { MenuList, Popover, usePopover } from '@/components/ui/popover'
import { toast, toastError } from '@/components/ui/toast'
import { PINNED_SERVERS, connFromUrl, gateAgentUrl } from './connections'
import { mcpServersQuery, type McpServer, type McpTest } from './hermes-mcp'

export function McpServersSection({ assistant }: { assistant: Assistant }) {
  const id = assistant.id
  const gate = gateAgentUrl(assistant)
  const qc = useQueryClient()
  const servers = useQuery(mcpServersQuery(id))
  const conns = useQuery(connectionsQuery(id))
  const [tests, setTests] = useState<Record<string, McpTest | 'running'>>({})
  const [remove, setRemove] = useState<McpServer | null>(null)
  const [turnOff, setTurnOff] = useState<McpServer | null>(null)
  const key = mcpServersQuery(id).queryKey

  const toggle = useMutation({
    mutationFn: ({ name, enabled }: { name: string; enabled: boolean }) =>
      hermes(id, 'PUT', `mcp/servers/${enc(name)}/enabled`, { enabled }),
    onMutate: ({ name, enabled }) => {
      qc.setQueryData(key, (d) =>
        d ? { servers: d.servers.map((s) => (s.name === name ? { ...s, enabled } : s)) } : d,
      )
    },
    onSuccess: (_, { name, enabled }) =>
      toast(`${name} ${enabled ? 'on' : 'off'} from the next conversation`),
    onError: toastError,
    onSettled: () => qc.invalidateQueries({ queryKey: key }),
  })
  const del = useMutation({
    mutationFn: (name: string) => hermes(id, 'DELETE', `mcp/servers/${enc(name)}`),
    onSuccess: (_, name) => toast(`Removed ${name}`),
    onError: toastError,
    onSettled: () => qc.invalidateQueries({ queryKey: key }),
  })
  const test = async (name: string) => {
    setTests((t) => ({ ...t, [name]: 'running' }))
    try {
      const r = await hermes<McpTest>(id, 'POST', `mcp/servers/${enc(name)}/test`)
      setTests((t) => ({ ...t, [name]: r }))
    } catch (e) {
      setTests((t) => ({
        ...t,
        [name]: { ok: false, error: e instanceof Error ? e.message : String(e), tools: [] },
      }))
    }
  }
  const setEnabled = (s: McpServer, on: boolean) => {
    if (!on && gateKind(gate, s)) setTurnOff(s)
    else toggle.mutate({ name: s.name, enabled: on })
  }

  const connNames = conns.data ? new Set(conns.data.map((c) => c.name)) : null
  const list = servers.data?.servers ?? []
  return (
    <Section
      title={
        <span className="flex items-center gap-2">
          <Server className="size-4 text-subtle" /> MCP servers
        </span>
      }
      helpId="assistant:mcp"
      help={`Where ${assistant.name}'s outside tools come from. Changes apply to new conversations.`}
    >
      {servers.isPending ? (
        <div className="space-y-3 p-4">
          <Skeleton className="h-12" />
          <Skeleton className="h-12" />
        </div>
      ) : servers.isError ? (
        <p className="p-4 text-[13px] text-danger">
          Couldn't load {assistant.name}'s MCP servers: {servers.error.message}
        </p>
      ) : list.length === 0 ? (
        <Empty icon={<Server />} title="No MCP servers">
          Add a connection above, then add it to {assistant.name}.
        </Empty>
      ) : (
        <ul className="divide-y divide-line">
          {list.map((s) => (
            <ServerRow
              key={s.name}
              agent={assistant.name}
              gate={gate}
              server={s}
              missingConn={(() => {
                const c = connFromUrl(gate, s.url)
                return !!c && !!connNames && !connNames.has(c)
              })()}
              test={tests[s.name]}
              onTest={() => test(s.name)}
              onCloseTest={() =>
                setTests((t) => {
                  const { [s.name]: _, ...rest } = t
                  return rest
                })
              }
              onEnabled={(on) => setEnabled(s, on)}
              onRemove={() => setRemove(s)}
            />
          ))}
        </ul>
      )}
      <Confirm
        open={!!remove}
        onClose={() => setRemove(null)}
        title={`Remove ${remove?.name ?? ''} from ${assistant.name}?`}
        body={
          remove && connFromUrl(gate, remove.url)
            ? 'New conversations lose its tools. The connection stays at the gate; add it back from there.'
            : 'New conversations lose its tools.'
        }
        confirmLabel="Remove"
        onConfirm={() => remove && del.mutate(remove.name)}
      />
      <Confirm
        open={!!turnOff}
        onClose={() => setTurnOff(null)}
        title={`Turn off ${turnOff?.name ?? ''}?`}
        body={
          turnOff && PINNED_SERVERS.has(turnOff.name)
            ? "New conversations lose its tools until you turn it back on. The assistant's next deploy turns it back on too."
            : 'New conversations lose its tools until you turn it back on.'
        }
        confirmLabel="Turn off"
        onConfirm={() => turnOff && toggle.mutate({ name: turnOff.name, enabled: false })}
      />
    </Section>
  )
}

/** How a server relates to the gate (at `gate`, as the agent reaches it), if it does. */
function gateKind(gate: string, s: McpServer): 'pinned' | 'conn' | 'gate' | null {
  if (PINNED_SERVERS.has(s.name)) return 'pinned'
  if (connFromUrl(gate, s.url)) return 'conn'
  if (gate && s.url?.startsWith(gate + '/')) return 'gate'
  return null
}

function ServerRow({
  agent,
  gate,
  server: s,
  missingConn,
  test,
  onTest,
  onCloseTest,
  onEnabled,
  onRemove,
}: {
  agent: string
  gate: string
  server: McpServer
  missingConn: boolean
  test?: McpTest | 'running'
  onTest: () => void
  onCloseTest: () => void
  onEnabled: (on: boolean) => void
  onRemove: () => void
}) {
  const menu = usePopover()
  const kind = gateKind(gate, s)
  const where =
    s.transport === 'stdio'
      ? [s.command, ...(s.args ?? [])].filter(Boolean).join(' ')
      : (s.url ?? '')
  const removable = kind !== 'pinned' && s.source !== 'plugin'
  const done = test && test !== 'running' ? test : null
  return (
    <li className="px-4 py-3">
      <div className="flex flex-col gap-1 sm:flex-row sm:items-start sm:gap-3">
        <div className="min-w-0 flex-1 space-y-0.5">
          <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
            <span className={clsx('font-mono text-[13px] font-medium', !s.enabled && 'text-muted')}>
              {s.name}
            </span>
            {kind === 'pinned' && (
              <Badge>
                <Lock className="size-3" /> Set by the deploy
              </Badge>
            )}
            {kind === 'conn' && <Badge color="var(--wp-accent)">Connection</Badge>}
            {kind === 'gate' && <Badge>Through the gate</Badge>}
            {missingConn && <Badge color="var(--wp-warn)">Not at the gate</Badge>}
            {s.source === 'plugin' && <Badge>Plugin {s.plugin}</Badge>}
            {s.tools && <Badge>{plural(s.tools.length, 'tool')} picked</Badge>}
            {done?.ok && (
              <span className="text-xs text-subtle">{plural(done.tools.length, 'tool')}</span>
            )}
          </div>
          <div className="break-all font-mono text-xs text-muted">
            <span className="text-subtle">{s.transport === 'stdio' ? 'stdio' : 'HTTP'} · </span>
            {where}
          </div>
        </div>
        <div className="-my-1 flex shrink-0 items-center justify-end gap-1 sm:my-0 sm:gap-2">
          <Button
            size="sm"
            variant="ghost"
            icon={<FlaskConical className="size-3.5" />}
            loading={test === 'running'}
            onClick={onTest}
            title="Connect and list its tools (can take up to 30 s)"
            className="h-9 sm:h-7"
          >
            Test
          </Button>
          <div className="flex size-9 items-center justify-center sm:size-auto">
            <Switch checked={s.enabled} onChange={onEnabled} label={`Use ${s.name}`} />
          </div>
          <IconButton
            ref={menu.anchor}
            label={`More for ${s.name}`}
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
                {
                  label: `Remove from ${agent}`,
                  icon: <Trash2 />,
                  danger: removable,
                  disabled: !removable,
                  hint: removable ? undefined : kind === 'pinned' ? 'pinned' : 'from a plugin',
                  onSelect: onRemove,
                },
              ]}
            />
          </Popover>
        </div>
      </div>
      {test === 'running' && (
        <p className="mt-2 text-xs text-subtle">Connecting… this can take up to 30 seconds.</p>
      )}
      {done && <TestResult result={done} onClose={onCloseTest} />}
    </li>
  )
}

function TestResult({ result: r, onClose }: { result: McpTest; onClose: () => void }) {
  return (
    <div className="relative mt-2 rounded-lg border border-line bg-sunken/60 px-3 py-2">
      <IconButton
        label="Close"
        size="xs"
        icon={<X className="size-3.5" />}
        onClick={onClose}
        className="absolute right-1.5 top-1.5"
      />
      {r.ok ? (
        <>
          <div className="pr-8 text-xs font-medium text-ok">
            Connected · {plural(r.tools.length, 'tool')}
            {r.prompts ? ` · ${plural(r.prompts, 'prompt')}` : ''}
            {r.resources ? ` · ${plural(r.resources, 'resource')}` : ''}
          </div>
          {r.tools.length > 0 && (
            <ul className="mt-1.5 max-h-64 space-y-1 overflow-y-auto">
              {r.tools.map((t) => (
                <li key={t.name} className="text-xs">
                  <span className="font-mono text-fg">{t.name}</span>
                  {t.description && (
                    <span className="text-subtle"> · {t.description.split('\n')[0]}</span>
                  )}
                </li>
              ))}
            </ul>
          )}
        </>
      ) : (
        <div className="break-words pr-8 text-xs text-danger">{r.error || "Couldn't connect"}</div>
      )}
    </div>
  )
}
