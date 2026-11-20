// Adding or changing one of the gate's connections, and giving one new
// credentials. The gate never hands header values back, so a change sends
// the whole header set again: the form asks for every value.

import { useId, useMemo, useState } from 'react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { Ban, Check, Globe, MessageSquareText, Plug, Plus, ShieldQuestion, X } from 'lucide-react'
import { api, enc } from '@/lib/api'
import { connectionsQuery, type GateConnection } from '@/lib/assistant'
import { Button, IconButton } from '@/components/ui/button'
import { Dialog } from '@/components/ui/dialog'
import { Field, Input, Textarea } from '@/components/ui/input'
import { SegmentedControl } from '@/components/ui/segmented'
import { toast, toastError } from '@/components/ui/toast'
import { Chips } from './Chips'
import {
  METHODS,
  globError,
  headerNameError,
  headerValueError,
  httpPolicy,
  keepOrder,
  mcpPolicy,
  methodRules,
  nameError,
  pathError,
  urlError,
  widens,
  type Method,
  type MethodRule,
} from './connections'

/** The gate's answer to a PUT: saved now, or waiting for the owner's approval. */
export interface PutResult {
  status: 'saved' | 'pending' | string
  code?: string
  message?: string
}

export function usePutConnection(assistantId: number) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: ({ name, body }: { name: string; body: Record<string, unknown> }) =>
      api.put<PutResult>(`/assistants/${assistantId}/gate/connections/${enc(name)}`, body),
    onSuccess: (r, { name }) => {
      qc.invalidateQueries({ queryKey: connectionsQuery(assistantId).queryKey })
      qc.invalidateQueries({ queryKey: ['assistant', assistantId, 'gate', 'approvals'] })
      // A pending save shows on its row (and Waypoint's own banner says so).
      if (r?.status !== 'pending') toast(`Saved ${name}`, { kind: 'success' })
    },
  })
}

interface HeaderRow {
  key: number
  name: string
  value: string
  /** one the connection already has (its name is fixed here) */
  kept: boolean
}

let rowKey = 0

const RULES: { value: MethodRule; label: string; icon: React.ReactNode; hint: string }[] = [
  {
    value: 'free',
    label: 'Free',
    icon: <Check className="size-3.5" />,
    hint: 'runs without asking',
  },
  {
    value: 'ask',
    label: 'Ask',
    icon: <ShieldQuestion className="size-3.5" />,
    hint: 'waits for you on Signal',
  },
  { value: 'refuse', label: 'Refuse', icon: <Ban className="size-3.5" />, hint: 'never runs' },
]

export function ConnectionDialog(props: {
  assistantId: number
  /** the connection being changed; absent when adding one */
  existing?: GateConnection
  taken: (name: string) => boolean
  open: boolean
  onClose: () => void
}) {
  // Mounted only while open, so each opening starts from the saved connection.
  return props.open ? <ConnectionForm {...props} /> : null
}

function ConnectionForm({
  assistantId,
  existing,
  taken,
  onClose,
}: {
  assistantId: number
  existing?: GateConnection
  taken: (name: string) => boolean
  onClose: () => void
}) {
  const put = usePutConnection(assistantId)
  const formId = useId()
  const [name, setName] = useState(existing?.name ?? '')
  const [kind, setKind] = useState<'mcp' | 'http'>(existing?.kind ?? 'http')
  const [url, setUrl] = useState(existing?.url ?? '')
  const [description, setDescription] = useState(existing?.description ?? '')
  const [headers, setHeaders] = useState<HeaderRow[]>(() =>
    existing
      ? (existing.header_names ?? []).map((n) => ({
          key: rowKey++,
          name: n,
          value: '',
          kept: true,
        }))
      : [{ key: rowKey++, name: 'Authorization', value: '', kept: false }],
  )
  const startMcp = mcpPolicy(existing?.kind === 'mcp' ? existing.policy : undefined)
  const [allow, setAllow] = useState(startMcp.allow)
  const [deny, setDeny] = useState(startMcp.deny)
  const [rules, setRules] = useState(() =>
    methodRules(existing?.kind === 'http' ? existing.policy : undefined),
  )
  const [paths, setPaths] = useState(() =>
    httpPolicy(existing?.kind === 'http' ? existing.policy : undefined).paths.filter(
      (p) => p !== '/',
    ),
  )
  const [tried, setTried] = useState(false)

  const errors = {
    name: existing ? undefined : nameError(name, taken),
    url: urlError(url),
    description: description.length > 2000 ? 'At most 2000 characters' : undefined,
  }
  const rowErrors = headers.map((h, i) => {
    const n = h.name.trim()
    if (!n && !h.value) return {}
    const dup = headers.findIndex((o) => o.name.trim().toLowerCase() === n.toLowerCase()) !== i
    return {
      name: !n ? 'Name it' : dup ? 'Already listed' : headerNameError(n),
      value: headerValueError(h.value),
    }
  })
  const filled = headers.filter((h) => h.name.trim() || h.value)
  const invalid =
    Object.values(errors).some(Boolean) ||
    rowErrors.some((e) => e.name || e.value) ||
    filled.length > 20

  const body = useMemo(() => {
    const prev = existing?.kind === kind ? existing.policy : undefined
    const policy: Record<string, string[]> =
      kind === 'mcp'
        ? {
            allow: keepOrder(allow, prev?.allow),
            deny: keepOrder(deny, prev?.deny),
          }
        : {
            allow_methods: keepOrder(
              METHODS.filter((m) => rules[m] === 'free'),
              prev?.allow_methods,
            ),
            approve_methods: keepOrder(
              METHODS.filter((m) => rules[m] === 'ask'),
              prev?.approve_methods,
            ),
            paths: keepOrder(paths.length ? paths : ['/'], prev?.paths),
          }
    const h: Record<string, string> = {}
    for (const r of headers) if (r.name.trim()) h[r.name.trim()] = r.value
    return { kind, url: url.trim(), description, headers: h, policy }
  }, [existing, kind, url, description, headers, allow, deny, rules, paths])
  const asks = widens(existing, body)

  const submit = (e: React.FormEvent) => {
    e.preventDefault()
    setTried(true)
    if (invalid) return
    put.mutate({ name: existing?.name ?? name, body }, { onSuccess: onClose, onError: toastError })
  }
  const show = (err: string | undefined, value: string) => (tried || value ? err : undefined)
  const setRow = (key: number, patch: Partial<HeaderRow>) =>
    setHeaders((hs) => hs.map((h) => (h.key === key ? { ...h, ...patch } : h)))

  return (
    <Dialog
      open
      onClose={onClose}
      wide
      title={existing ? `Edit ${existing.name}` : 'Add a connection'}
      footer={
        <>
          {asks && (
            <span className="mr-auto flex min-w-0 items-center gap-1.5 text-xs text-muted">
              <ShieldQuestion className="size-3.5 shrink-0 text-warn" />
              <span className="truncate">Waits for your approval on Signal</span>
            </span>
          )}
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button type="submit" form={formId} variant="primary" loading={put.isPending}>
            {asks ? 'Ask on Signal' : 'Save'}
          </Button>
        </>
      }
    >
      <form id={formId} onSubmit={submit} className="space-y-4 p-4" autoComplete="off">
        <div className="grid gap-4 sm:grid-cols-[1fr_auto]">
          {existing ? (
            <Field label="Name">
              <Input value={existing.name} readOnly disabled className="font-mono" />
            </Field>
          ) : (
            <Field label="Name" error={show(errors.name, name)}>
              <Input
                autoFocus
                value={name}
                onChange={(e) => setName(e.target.value)}
                placeholder="paperless"
                spellCheck={false}
                maxLength={40}
                aria-invalid={!!show(errors.name, name) || undefined}
                className="font-mono"
              />
            </Field>
          )}
          <div className="space-y-1.5">
            <span className="block text-[13px] font-medium leading-5">Kind</span>
            <SegmentedControl
              label="Kind"
              value={kind}
              onChange={setKind}
              textFrom="always"
              className="!h-9 [&>button]:!h-8 [&>button]:px-2.5"
              options={[
                {
                  value: 'http',
                  label: 'HTTP API',
                  icon: <Globe className="size-3.5" />,
                  hint: 'a plain HTTP API; the assistant gets one request tool',
                },
                {
                  value: 'mcp',
                  label: 'MCP server',
                  icon: <Plug className="size-3.5" />,
                  hint: "the service's own MCP server (streamable HTTP)",
                },
              ]}
            />
          </div>
        </div>
        <Field label="URL" error={show(errors.url, url)}>
          <Input
            value={url}
            onChange={(e) => setUrl(e.target.value)}
            placeholder={
              kind === 'mcp' ? 'https://service.example.com/mcp' : 'https://paperless.example.com'
            }
            spellCheck={false}
            inputMode="url"
            aria-invalid={!!show(errors.url, url) || undefined}
            className="font-mono"
          />
        </Field>
        <Field label="Description" error={errors.description}>
          <Textarea
            autoGrow
            rows={2}
            value={description}
            onChange={(e) => setDescription(e.target.value)}
            placeholder="Team documents"
          />
        </Field>

        <fieldset className="space-y-2">
          <legend className="text-[13px] font-medium leading-5">Headers the gate adds</legend>
          {existing && (existing.header_names ?? []).length > 0 && (
            <p className="text-xs text-subtle">
              Saved values are never shown: enter each again. Adding or removing a header asks you
              on Signal.
            </p>
          )}
          {headers.map((h, i) => (
            <div key={h.key} className="flex items-start gap-2">
              <div className="w-[38%] shrink-0 sm:w-48">
                <Input
                  aria-label="Header name"
                  value={h.name}
                  onChange={(e) => setRow(h.key, { name: e.target.value })}
                  readOnly={h.kept}
                  placeholder="Authorization"
                  spellCheck={false}
                  className="w-full font-mono text-[13px] read-only:bg-sunken"
                />
                {tried && rowErrors[i].name && (
                  <span className="mt-1 block text-xs text-danger">{rowErrors[i].name}</span>
                )}
              </div>
              <div className="min-w-0 flex-1">
                <Input
                  aria-label={`Value of ${h.name || 'header'}`}
                  type="password"
                  autoComplete="new-password"
                  value={h.value}
                  onChange={(e) => setRow(h.key, { value: e.target.value })}
                  placeholder={h.kept ? 'Enter again' : 'Token abc123…'}
                  className="w-full font-mono text-[13px]"
                />
                {tried && rowErrors[i].value && (
                  <span className="mt-1 block text-xs text-danger">{rowErrors[i].value}</span>
                )}
              </div>
              <IconButton
                label={`Remove ${h.name || 'header'}`}
                size="md"
                icon={<X className="size-4" />}
                onClick={() => setHeaders((hs) => hs.filter((x) => x.key !== h.key))}
              />
            </div>
          ))}
          <Button
            size="sm"
            variant="ghost"
            icon={<Plus className="size-3.5" />}
            disabled={headers.length >= 20}
            onClick={() =>
              setHeaders((hs) => [...hs, { key: rowKey++, name: '', value: '', kept: false }])
            }
          >
            Add header
          </Button>
        </fieldset>

        <fieldset className="space-y-3 border-t border-line pt-4">
          <legend className="sr-only">Policy</legend>
          {kind === 'mcp' ? (
            <>
              <Field label="Run freely" note="Tool names; * matches anything.">
                <Chips
                  label="Tools that run freely"
                  value={allow}
                  onChange={setAllow}
                  placeholder="get_*, list_*"
                  check={globError}
                />
              </Field>
              <Field label="Refuse">
                <Chips
                  label="Tools that are refused"
                  value={deny}
                  onChange={setDeny}
                  placeholder="*_delete"
                  check={globError}
                />
              </Field>
              <p className="flex items-center gap-1.5 text-xs text-muted">
                <MessageSquareText className="size-3.5 shrink-0 text-subtle" />
                Every other tool asks you on Signal, call by call.
              </p>
            </>
          ) : (
            <>
              <div className="space-y-1.5">
                <span className="block text-[13px] font-medium leading-5">Methods</span>
                <div className="grid gap-x-6 gap-y-1.5 sm:grid-cols-2">
                  {METHODS.map((m) => (
                    <div key={m} className="flex items-center justify-between gap-3">
                      <span className="font-mono text-xs text-muted">{m}</span>
                      <SegmentedControl
                        label={m}
                        value={rules[m]}
                        onChange={(v) => setRules((r) => ({ ...r, [m as Method]: v }))}
                        options={RULES}
                        textFrom="always"
                      />
                    </div>
                  ))}
                </div>
              </div>
              <Field label="Paths" note="Calls must sit under one of these. None means any path.">
                <Chips
                  label="Path prefixes"
                  value={paths}
                  onChange={(v) => setPaths(v.filter((p) => p !== '/'))}
                  placeholder="/api/documents"
                  check={pathError}
                />
              </Field>
            </>
          )}
        </fieldset>
      </form>
    </Dialog>
  )
}

/** New values for a connection's headers (a rotated token): saved at once. */
export function RotateDialog(props: {
  assistantId: number
  conn: GateConnection
  open: boolean
  onClose: () => void
}) {
  return props.open ? <RotateForm {...props} /> : null
}

function RotateForm({
  assistantId,
  conn,
  onClose,
}: {
  assistantId: number
  conn: GateConnection
  onClose: () => void
}) {
  const put = usePutConnection(assistantId)
  const formId = useId()
  const names = conn.header_names ?? []
  const [values, setValues] = useState<Record<string, string>>({})
  const [tried, setTried] = useState(false)
  const errs = Object.fromEntries(names.map((n) => [n, headerValueError(values[n] ?? '')]))
  const submit = (e: React.FormEvent) => {
    e.preventDefault()
    setTried(true)
    if (Object.values(errs).some(Boolean)) return
    put.mutate(
      {
        name: conn.name,
        body: {
          kind: conn.kind,
          url: conn.url,
          description: conn.description ?? '',
          policy: conn.policy,
          headers: Object.fromEntries(names.map((n) => [n, values[n] ?? ''])),
        },
      },
      { onSuccess: onClose, onError: toastError },
    )
  }
  return (
    <Dialog
      open
      onClose={onClose}
      title={`New credentials for ${conn.name}`}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button type="submit" form={formId} variant="primary" loading={put.isPending}>
            Save
          </Button>
        </>
      }
    >
      <form id={formId} onSubmit={submit} className="space-y-3 p-4" autoComplete="off">
        {names.map((n, i) => (
          <Field
            key={n}
            label={<span className="font-mono">{n}</span>}
            error={tried ? errs[n] : undefined}
          >
            <Input
              autoFocus={i === 0}
              type="password"
              autoComplete="new-password"
              value={values[n] ?? ''}
              onChange={(e) => setValues((v) => ({ ...v, [n]: e.target.value }))}
              className="font-mono text-[13px]"
            />
          </Field>
        ))}
        <p className="text-xs text-subtle">Takes effect at once; the old values are dropped.</p>
      </form>
    </Dialog>
  )
}
