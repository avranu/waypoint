// Activity → Logs: Hermes's own logs (agent, errors, gateway, …; filtered
// by level, component and text on Hermes's side) and the gate's services'
// logs (gatekeeper, bridge, squid, caddy, scraper; searched here). Refreshes
// every 5 seconds until paused, and follows the tail while scrolled to it.

import { useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react'
import { useQuery, keepPreviousData } from '@tanstack/react-query'
import clsx from 'clsx'
import { ArrowDownToLine, RefreshCw, ScrollText, Search, WrapText } from 'lucide-react'
import { GATE_LOGS, gateLogsQuery, hermesQuery, type Assistant } from '@/lib/assistant'
import { IconButton, ToggleButton } from '@/components/ui/button'
import { Input, Select } from '@/components/ui/input'
import { Skeleton } from '@/components/ui/misc'
import { CopyButton } from '@/pages/agents/usage/shared'
import { ErrorBox } from './shared'

const HERMES_FILES: { id: string; label: string }[] = [
  { id: 'agent', label: 'Agent' },
  { id: 'errors', label: 'Errors' },
  { id: 'gateway', label: 'Gateway' },
  { id: 'mcp', label: 'MCP' },
  { id: 'update', label: 'Update' },
  { id: 'handoff', label: 'Handoff' },
]

const GATE_LABEL: Record<string, string> = {
  gatekeeper: 'Gatekeeper',
  bridge: 'Model bridge',
  squid: 'Web proxy (squid)',
  caddy: 'Gate calls (caddy)',
  scraper: 'Page fetcher',
}

const LEVELS = ['ALL', 'DEBUG', 'INFO', 'WARNING', 'ERROR']
const COMPONENTS = ['all', 'agent', 'gateway', 'tools', 'cron', 'cli', 'gui']

/** A value that settles after the person stops typing. */
function useDebounced<T>(v: T, ms = 300): T {
  const [out, setOut] = useState(v)
  useEffect(() => {
    const t = window.setTimeout(() => setOut(v), ms)
    return () => window.clearTimeout(t)
  }, [v, ms])
  return out
}

function lineTone(l: string): string | undefined {
  if (/\b(ERROR|CRITICAL|FATAL|Traceback)\b|"level":\s*"error"/i.test(l)) return 'text-danger'
  if (/\bWARN(ING)?\b|"level":\s*"warn/i.test(l)) return 'text-warn'
  if (/\bDEBUG\b/.test(l)) return 'text-subtle'
  return undefined
}

export function LogsView({ assistant }: { assistant: Assistant }) {
  const id = assistant.id
  const [source, setSource] = useState('h:agent')
  const [level, setLevel] = useState('ALL')
  const [component, setComponent] = useState('all')
  const [search, setSearch] = useState('')
  const [lines, setLines] = useState(200)
  const [wrap, setWrap] = useState(true)
  const [auto, setAuto] = useState(true)
  const [follow, setFollow] = useState(true)
  const term = useDebounced(search.trim())
  const isHermes = source.startsWith('h:')
  const name = source.slice(2)
  const up = assistant.state.reachable && !assistant.state.frozen

  const hermes = useQuery({
    ...hermesQuery<{ file: string; lines: string[] }>(
      id,
      'logs',
      {
        file: name,
        lines: Math.min(lines, 500),
        level: level === 'ALL' ? undefined : level,
        component: component === 'all' ? undefined : component,
        search: term || undefined,
      },
      0,
    ),
    enabled: isHermes && up,
    refetchInterval: auto ? 5000 : false,
    placeholderData: keepPreviousData,
    retry: false,
  })
  const gate = useQuery({
    ...gateLogsQuery(id, name, lines),
    enabled: !isHermes,
    refetchInterval: auto ? 5000 : false,
    placeholderData: keepPreviousData,
    retry: false,
  })
  const q = isHermes ? hermes : gate

  const shown = useMemo(() => {
    const raw = (q.data?.lines ?? []).map((l) => l.replace(/\r?\n$/, ''))
    if (isHermes || !term) return raw
    const t = term.toLowerCase()
    return raw.filter((l) => l.toLowerCase().includes(t))
  }, [q.data, isHermes, term])

  // Follow the tail: stay at the bottom as lines arrive, while there.
  const box = useRef<HTMLDivElement>(null)
  useLayoutEffect(() => {
    if (follow && box.current) box.current.scrollTop = box.current.scrollHeight
  }, [shown, follow, wrap])

  const lineOptions = isHermes ? [100, 200, 500] : [200, 500, 1000, 2000]
  const updated = q.dataUpdatedAt
    ? new Date(q.dataUpdatedAt).toLocaleTimeString(undefined, {
        hour: 'numeric',
        minute: '2-digit',
        second: '2-digit',
      })
    : ''

  return (
    <div className="space-y-3">
      <div className="flex flex-wrap items-center gap-2">
        <Select
          aria-label="Log"
          value={source}
          onChange={(e) => {
            const v = e.target.value
            setSource(v)
            setFollow(true)
            if (v.startsWith('h:') && lines > 500) setLines(500)
          }}
          className="!w-auto min-w-40"
        >
          <optgroup label={assistant.name}>
            {HERMES_FILES.map((f) => (
              <option key={f.id} value={`h:${f.id}`}>
                {f.label}
              </option>
            ))}
          </optgroup>
          <optgroup label="Gate">
            {GATE_LOGS.map((g) => (
              <option key={g} value={`g:${g}`}>
                {GATE_LABEL[g] ?? g}
              </option>
            ))}
          </optgroup>
        </Select>
        {isHermes && (
          <>
            <Select
              aria-label="Lowest level"
              value={level}
              onChange={(e) => setLevel(e.target.value)}
              className="!w-auto"
            >
              {LEVELS.map((l) => (
                <option key={l} value={l}>
                  {l === 'ALL'
                    ? 'Every level'
                    : l === 'DEBUG'
                      ? 'Debug and up'
                      : l === 'INFO'
                        ? 'Info and up'
                        : l === 'WARNING'
                          ? 'Warnings and up'
                          : 'Errors'}
                </option>
              ))}
            </Select>
            <Select
              aria-label="Component"
              value={component}
              onChange={(e) => setComponent(e.target.value)}
              className="!w-auto"
            >
              {COMPONENTS.map((c) => (
                <option key={c} value={c}>
                  {c === 'all' ? 'Every component' : c.charAt(0).toUpperCase() + c.slice(1)}
                </option>
              ))}
            </Select>
          </>
        )}
        <div className="relative min-w-0 flex-1 basis-44">
          <Search className="pointer-events-none absolute left-2.5 top-1/2 size-3.5 -translate-y-1/2 text-subtle" />
          <Input
            type="search"
            aria-label="Search the log"
            placeholder="Search"
            value={search}
            onChange={(e) => setSearch(e.target.value)}
            className="pl-8"
          />
        </div>
      </div>

      <div className="flex flex-wrap items-center gap-1">
        <Select
          aria-label="Lines"
          value={String(lines)}
          onChange={(e) => setLines(Number(e.target.value))}
          variant="inline"
        >
          {lineOptions.map((n) => (
            <option key={n} value={n}>
              Last {n.toLocaleString()} lines
            </option>
          ))}
        </Select>
        <span className="ml-1 text-xs tabular-nums text-subtle">
          {shown.length.toLocaleString()} shown{updated && ` · ${updated}`}
        </span>
        <div className="ml-auto flex items-center gap-1">
          <ToggleButton
            label="Wrap lines"
            pressed={wrap}
            onPressedChange={setWrap}
            icon={<WrapText className="size-3.5" />}
          />
          <ToggleButton
            label="Follow the end"
            hint="Keep the newest lines in view"
            pressed={follow}
            onPressedChange={setFollow}
            icon={<ArrowDownToLine className="size-3.5" />}
          />
          <ToggleButton
            label="Refresh every 5 seconds"
            pressed={auto}
            onPressedChange={setAuto}
            icon={
              <RefreshCw className={clsx('size-3.5', auto && q.isFetching && 'animate-spin')} />
            }
            text="Live"
            textFrom="sm"
          />
          {!auto && (
            <IconButton
              label="Refresh now"
              icon={<RefreshCw className={clsx('size-4', q.isFetching && 'animate-spin')} />}
              onClick={() => q.refetch()}
            />
          )}
          <CopyButton text={shown.join('\n')} label="Copy all" />
        </div>
      </div>

      {isHermes && !up ? (
        <div className="rounded-xl border border-dashed border-line px-4 py-3.5 text-[13px] text-muted">
          {assistant.name} isn’t answering, so its own logs can’t be read. The gate’s logs still
          can.
        </div>
      ) : q.isPending ? (
        <Skeleton className="h-[60vh] min-h-64" />
      ) : q.isError && !q.data ? (
        <ErrorBox what="this log" error={q.error} />
      ) : (
        <div
          ref={box}
          role="log"
          aria-label="Log lines"
          tabIndex={0}
          onScroll={(e) => {
            const el = e.currentTarget
            const atEnd = el.scrollHeight - el.scrollTop - el.clientHeight < 24
            if (atEnd !== follow) setFollow(atEnd)
          }}
          className="h-[60vh] min-h-64 overflow-auto rounded-xl border border-line bg-sunken p-2.5 font-mono text-xs leading-relaxed"
        >
          {shown.length === 0 ? (
            <div className="flex h-full flex-col items-center justify-center gap-2 text-center font-sans text-[13px] text-muted">
              <ScrollText className="size-6 text-subtle" />
              {term || level !== 'ALL' || component !== 'all'
                ? 'No lines match.'
                : 'This log is empty.'}
            </div>
          ) : (
            shown.map((l, i) => (
              <div
                key={i}
                className={clsx(
                  wrap ? 'whitespace-pre-wrap break-all' : 'w-max whitespace-pre',
                  lineTone(l),
                )}
              >
                {l || ' '}
              </div>
            ))
          )}
        </div>
      )}
      {q.isError && q.data && (
        <p className="text-xs text-danger">
          Couldn’t refresh: {q.error instanceof Error ? q.error.message : String(q.error)}
        </p>
      )}
    </div>
  )
}
