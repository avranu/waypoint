// Hermes's messaging channels: each platform's state, on or off, its
// settings (secrets masked; type a new one to replace it) and a test.

import { useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import clsx from 'clsx'
import { ChevronRight, MessageCircle, RotateCw } from 'lucide-react'
import { hermes, hermesQuery } from '@/lib/assistant'
import { safeUrl } from '@/lib/format'
import { Button } from '@/components/ui/button'
import { Field, Input, Switch } from '@/components/ui/input'
import { Badge, Empty, Section, Skeleton } from '@/components/ui/misc'
import { toast, toastError } from '@/components/ui/toast'
import { LoadError, LogTail, useGatewayRestart } from './common'

interface EnvVar {
  key: string
  required: boolean
  is_set: boolean
  redacted_value: string | null
  value: string | null
  description: string
  prompt: string
  url: string | null
  is_password: boolean
  is_list: boolean
  advanced: boolean
}

interface Platform {
  id: string
  name: string
  description: string
  enabled: boolean
  configured: boolean
  state: string
  error_message: string | null
  env_vars: EnvVar[]
}

const STATE: Record<string, [string, string]> = {
  connected: ['Connected', 'var(--wp-ok)'],
  disabled: ['Off', ''],
  not_configured: ['Not set up', ''],
  pending_restart: ['Restart to apply', 'var(--wp-warn)'],
  gateway_stopped: ['Gateway stopped', 'var(--wp-warn)'],
  startup_failed: ['Failed to start', 'var(--wp-danger)'],
  disconnected: ['Disconnected', 'var(--wp-danger)'],
  fatal: ['Failed', 'var(--wp-danger)'],
}

export function StateBadge({ state }: { state: string }) {
  const [label, color] = STATE[state] ?? [state.replace(/_/g, ' '), 'var(--wp-subtle)']
  return <Badge color={color || undefined}>{label}</Badge>
}

export function ChannelsSection({ id }: { id: number }) {
  const list = useQuery(
    hermesQuery<{ platforms: Platform[] }>(id, 'messaging/platforms', undefined, 15_000),
  )
  const [open, setOpen] = useState('')
  const restart = useGatewayRestart(id)
  const platforms = [...(list.data?.platforms ?? [])].sort(
    (a, b) =>
      Number(b.enabled) - Number(a.enabled) ||
      Number(b.configured) - Number(a.configured) ||
      a.name.localeCompare(b.name),
  )
  const waiting = platforms.some((p) => p.state === 'pending_restart')
  return (
    <>
      <div>
        <h2 className="text-[15px] font-semibold">Channels</h2>
        <p className="text-[13px] text-muted">
          Most of these need internet access, which the agent doesn't have.
        </p>
      </div>
      {(waiting || restart.phase !== 'idle') && (
        <Section
          title={restart.busy ? 'Restarting the gateway…' : 'Changes wait for a gateway restart'}
          actions={
            <Button
              size="sm"
              variant="primary"
              icon={<RotateCw className="size-3.5" />}
              loading={restart.busy}
              onClick={restart.start}
            >
              Restart gateway
            </Button>
          }
        >
          {restart.phase !== 'idle' && <LogTail lines={restart.lines} className="rounded-b-xl" />}
        </Section>
      )}
      <Section>
        {list.isPending ? (
          <div className="space-y-2 p-4">
            <Skeleton className="h-10" />
            <Skeleton className="h-10" />
            <Skeleton className="h-10" />
          </div>
        ) : list.isError ? (
          <LoadError error={list.error} what="the channels" />
        ) : platforms.length === 0 ? (
          <Empty icon={<MessageCircle />} title="No channels" />
        ) : (
          <ul className="divide-y divide-line">
            {platforms.map((p) => (
              <PlatformRow
                key={p.id}
                id={id}
                p={p}
                open={open === p.id}
                onToggle={() => setOpen(open === p.id ? '' : p.id)}
              />
            ))}
          </ul>
        )}
      </Section>
    </>
  )
}

function PlatformRow({
  id,
  p,
  open,
  onToggle,
}: {
  id: number
  p: Platform
  open: boolean
  onToggle: () => void
}) {
  const qc = useQueryClient()
  const [enabled, setEnabled] = useState<boolean | null>(null)
  const [test, setTest] = useState<{ ok: boolean; message: string } | null>(null)
  const [testing, setTesting] = useState(false)
  const refresh = () => qc.invalidateQueries({ queryKey: ['assistant', id, 'messaging/platforms'] })
  const on = enabled ?? p.enabled
  const toggle = (v: boolean) => {
    setEnabled(v)
    hermes(id, 'PUT', `messaging/platforms/${encodeURIComponent(p.id)}`, { enabled: v })
      .then(() => toast(`${p.name} ${v ? 'on' : 'off'}`))
      .catch((e) => {
        setEnabled(null)
        toastError(e)
      })
      .finally(() => refresh().then(() => setEnabled(null)))
  }
  const runTest = () => {
    setTesting(true)
    hermes<{ ok: boolean; message: string; state: string }>(
      id,
      'POST',
      `messaging/platforms/${encodeURIComponent(p.id)}/test`,
    )
      .then((r) => setTest({ ok: r.ok, message: r.message || (r.ok ? 'It works' : r.state) }))
      .catch((e) => setTest({ ok: false, message: e instanceof Error ? e.message : String(e) }))
      .finally(() => setTesting(false))
  }
  return (
    <li>
      <div className="flex items-center gap-2 px-2 py-1.5 sm:px-3">
        <button
          type="button"
          aria-expanded={open}
          onClick={onToggle}
          className="flex min-h-11 min-w-0 flex-1 items-center gap-2 rounded-lg px-2 text-left hover:bg-hover"
        >
          <ChevronRight
            className={clsx(
              'size-4 shrink-0 text-subtle transition-transform',
              open && 'rotate-90',
            )}
          />
          <span className="min-w-0 flex-1">
            <span className="block truncate text-[13px] font-medium">{p.name}</span>
            {!open && p.error_message && (
              <span className="block truncate text-xs text-danger">{p.error_message}</span>
            )}
          </span>
          <StateBadge state={p.state} />
        </button>
        <Switch checked={on} onChange={toggle} label={`${p.name} on`} />
      </div>
      {open && (
        <div className="space-y-4 border-t border-line bg-sunken/40 px-4 py-3 sm:pl-11">
          {p.description && <p className="text-[13px] text-muted">{p.description}</p>}
          {p.error_message && <p className="text-[13px] text-danger">{p.error_message}</p>}
          {p.env_vars.length > 0 && <EnvForm id={id} platform={p} onSaved={refresh} />}
          <div className="flex flex-wrap items-center gap-2">
            <Button size="sm" onClick={runTest} loading={testing}>
              Test
            </Button>
            {test && (
              <span className={clsx('min-w-0 text-[13px]', test.ok ? 'text-ok' : 'text-danger')}>
                {test.message}
              </span>
            )}
          </div>
        </div>
      )}
    </li>
  )
}

/** A platform's settings: type a value to set or replace it; blank keeps it. */
function EnvForm({
  id,
  platform,
  onSaved,
}: {
  id: number
  platform: Platform
  onSaved: () => void
}) {
  const [values, setValues] = useState<Record<string, string>>({})
  const [busy, setBusy] = useState(false)
  const [advanced, setAdvanced] = useState(false)
  const changed = Object.entries(values).filter(([, v]) => v.trim() !== '')
  const vars = platform.env_vars.filter((v) => advanced || !v.advanced || v.is_set)
  const hidden = platform.env_vars.length - vars.length
  const save = async (clear?: string) => {
    setBusy(true)
    try {
      await hermes(id, 'PUT', `messaging/platforms/${encodeURIComponent(platform.id)}`, {
        env: Object.fromEntries(changed),
        clear_env: clear ? [clear] : [],
      })
      setValues({})
      toast(clear ? `${clear} cleared` : `${platform.name} saved`, { kind: 'success' })
      onSaved()
    } catch (e) {
      toastError(e)
    } finally {
      setBusy(false)
    }
  }
  return (
    <form
      className="space-y-3"
      onSubmit={(e) => {
        e.preventDefault()
        if (changed.length) save()
      }}
    >
      {vars.map((v) => {
        const link = safeUrl(v.url)
        const current = v.is_list ? v.value : v.redacted_value
        return (
          <Field
            key={v.key}
            label={
              <span className="flex flex-wrap items-baseline gap-x-2">
                {v.prompt || v.key}
                {v.required && <span className="text-xs font-normal text-subtle">required</span>}
              </span>
            }
            note={
              <>
                <span className="font-mono">{v.key}</span>
                {v.description && ` · ${v.description}`}
                {link && (
                  <>
                    {' '}
                    <a
                      href={link}
                      target="_blank"
                      rel="noreferrer noopener"
                      className="text-accent hover:underline"
                    >
                      Where to find it
                    </a>
                  </>
                )}
              </>
            }
          >
            <div className="flex gap-2">
              <Input
                type={v.is_password ? 'password' : 'text'}
                autoComplete="off"
                value={values[v.key] ?? ''}
                placeholder={v.is_set ? (current ?? 'set') + ' (unchanged)' : ''}
                onChange={(e) => setValues((s) => ({ ...s, [v.key]: e.target.value }))}
                className="font-mono sm:flex-1"
              />
              {v.is_set && !v.required && (
                <Button variant="ghost" disabled={busy} onClick={() => save(v.key)}>
                  Clear
                </Button>
              )}
            </div>
          </Field>
        )
      })}
      <div className="flex flex-wrap items-center gap-2">
        <Button type="submit" size="sm" variant="primary" loading={busy} disabled={!changed.length}>
          Save
        </Button>
        {hidden > 0 && (
          <button
            type="button"
            onClick={() => setAdvanced(true)}
            className="text-[13px] text-muted hover:text-fg"
          >
            {hidden} more {hidden === 1 ? 'setting' : 'settings'}
          </button>
        )}
      </div>
    </form>
  )
}
