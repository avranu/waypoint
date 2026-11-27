import { useEffect, useRef, useState, type ReactNode } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import clsx from 'clsx'
import { Lock } from 'lucide-react'
import { hermes } from '@/lib/assistant'
import { Badge } from '@/components/ui/misc'
import { toast, toastError } from '@/components/ui/toast'

/**
 * Settings the assistant's deploy owns (its deploy config pins them): a
 * change to one here is put back at the next deploy. Keep the two lists the
 * same.
 */
export const PINNED = [
  'model.provider',
  'model.base_url',
  'terminal.backend',
  'terminal.docker_image',
  'terminal.docker_volumes',
  'terminal.docker_mount_cwd_to_workspace',
  'approvals.mode',
  'approvals.cron_mode',
  'approvals.single_query_mode',
  'approvals.unattended_mode',
  'security.allow_private_urls',
  'security.allow_lazy_installs',
  'updates.check',
  'auth.adopt_external_logins',
  'gateway.allow_all_users',
  'mcp_servers.gate',
  'mcp_servers.waypoint',
  'agent.disabled_toolsets',
  'cron.allow_agent_scheduling',
  'skills.inline_shell',
  'model_catalog.enabled',
  'tools.connectors.enabled',
  'nous.guest',
]

/** Whether the deploy owns the setting at `path` (or the whole map it is in). */
export const isPinned = (path: string) => PINNED.some((p) => path === p || path.startsWith(p + '.'))

export function PinnedBadge({ className }: { className?: string }) {
  return (
    <span title="The deploy puts this back to its own value" className={className}>
      <Badge>
        <Lock className="size-3" />
        Set by the deploy
      </Badge>
    </span>
  )
}

type Tree = Record<string, unknown>

export function getPath(obj: unknown, path: string): unknown {
  let cur = obj
  for (const k of path.split('.')) {
    if (!cur || typeof cur !== 'object' || !(k in (cur as Tree))) return undefined
    cur = (cur as Tree)[k]
  }
  return cur
}

/** `changes` (dot-path → value) as the nested object PUT /api/config merges. */
export function nest(changes: Record<string, unknown>): Tree {
  const out: Tree = {}
  for (const [path, v] of Object.entries(changes)) {
    const keys = path.split('.')
    let cur = out
    for (const k of keys.slice(0, -1)) {
      if (!cur[k] || typeof cur[k] !== 'object') cur[k] = {}
      cur = cur[k] as Tree
    }
    cur[keys[keys.length - 1]] = v
  }
  return out
}

/** A label and its value, as rows of a <dl>. */
export function Rows({ rows }: { rows: [ReactNode, ReactNode][] }) {
  return (
    <dl className="divide-y divide-line">
      {rows.map(([k, v], i) => (
        <div
          key={i}
          className="flex flex-col gap-0.5 px-4 py-2.5 sm:flex-row sm:items-center sm:gap-4"
        >
          <dt className="shrink-0 text-[13px] text-muted sm:w-44">{k}</dt>
          <dd className="min-w-0 flex-1 text-[13px]">{v}</dd>
        </div>
      ))}
    </dl>
  )
}

export interface ActionStatus {
  name: string
  running: boolean
  exit_code: number | null
  pid: number | null
  lines: string[]
}

/**
 * Restarting the gateway: start it, then follow its log until the process
 * exits. `phase` is idle, running, done or failed.
 */
export function useGatewayRestart(id: number) {
  const qc = useQueryClient()
  const [phase, setPhase] = useState<'idle' | 'running' | 'done' | 'failed'>('idle')
  const [lines, setLines] = useState<string[]>([])
  const alive = useRef(true)
  useEffect(() => {
    alive.current = true
    return () => {
      alive.current = false
    }
  }, [])
  const start = async () => {
    setPhase('running')
    setLines([])
    try {
      await hermes(id, 'POST', 'gateway/restart')
      for (let i = 0; i < 180 && alive.current; i++) {
        await new Promise((r) => setTimeout(r, 1000))
        const st = await hermes<ActionStatus>(
          id,
          'GET',
          'actions/gateway-restart/status',
          undefined,
          {
            lines: 200,
          },
        ).catch(() => null)
        if (!st || !alive.current) continue
        setLines(st.lines)
        if (!st.running && (i > 1 || st.exit_code !== null)) {
          const ok = !st.exit_code
          setPhase(ok ? 'done' : 'failed')
          if (ok) toast('Gateway restarted', { kind: 'success' })
          else toast(`The restart failed (exit ${st.exit_code})`, { kind: 'error' })
          break
        }
      }
    } catch (e) {
      setPhase('failed')
      toastError(e)
    } finally {
      qc.invalidateQueries({ queryKey: ['assistant', id, 'status'] })
      qc.invalidateQueries({ queryKey: ['assistant', id, 'messaging/platforms'] })
    }
  }
  return { phase, lines, start, busy: phase === 'running' }
}

/** The tail of a background action's log. */
export function LogTail({ lines, className }: { lines: string[]; className?: string }) {
  const ref = useRef<HTMLPreElement>(null)
  useEffect(() => {
    ref.current?.scrollTo({ top: ref.current.scrollHeight })
  }, [lines])
  return (
    <pre
      ref={ref}
      className={clsx(
        'max-h-60 overflow-auto whitespace-pre-wrap break-words bg-sunken px-4 py-2 font-mono text-[11px] leading-relaxed text-muted',
        className,
      )}
    >
      {lines.length ? lines.join('\n') : 'Starting…'}
    </pre>
  )
}

/** An error from Hermes, in place of what failed to load. */
export function LoadError({ error, what }: { error: unknown; what: string }) {
  return (
    <div className="px-4 py-6 text-center text-[13px] text-danger">
      Couldn't load {what}: {error instanceof Error ? error.message : String(error)}
    </div>
  )
}
