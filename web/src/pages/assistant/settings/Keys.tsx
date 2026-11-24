// Values never come back, only a masked preview; the route that would reveal one (env/reveal) is refused on the way through Waypoint.

import { useMemo, useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { KeyRound, Plus, Search, Trash2 } from 'lucide-react'
import { hermes, hermesQuery } from '@/lib/assistant'
import { safeUrl } from '@/lib/format'
import { Button, IconButton } from '@/components/ui/button'
import { Confirm, Dialog } from '@/components/ui/dialog'
import { Field, Input } from '@/components/ui/input'
import { Empty, Section, Skeleton } from '@/components/ui/misc'
import { SegmentedControl } from '@/components/ui/segmented'
import { toast, toastError } from '@/components/ui/toast'
import { LoadError } from './common'

interface EnvVar {
  is_set: boolean
  redacted_value: string | null
  description: string
  url: string | null
  category: string
  is_password: boolean
  advanced: boolean
  channel_managed: boolean
  provider: string
  provider_label: string
  custom: boolean
}

const CATEGORIES: [string, string][] = [
  ['custom', 'Your own'],
  ['provider', 'Model providers'],
  ['tool', 'Tools'],
  ['skill', 'Skills'],
  ['setting', 'Settings'],
  ['messaging', 'Messaging'],
]

const NAME = /^[A-Za-z_][A-Za-z0-9_]*$/

export function KeysSection({ id, onConnections }: { id: number; onConnections: () => void }) {
  const env = useQuery(hermesQuery<Record<string, EnvVar>>(id, 'env', undefined, 30_000))
  const [show, setShow] = useState<'set' | 'all'>('set')
  const [text, setText] = useState('')
  const [adding, setAdding] = useState(false)
  const rows = useMemo(
    () => Object.entries(env.data ?? {}).filter(([, v]) => !v.channel_managed),
    [env.data],
  )
  const setCount = rows.filter(([, v]) => v.is_set).length
  const q = text.trim().toLowerCase()
  const shown = rows.filter(
    ([k, v]) =>
      (show === 'all' || q || v.is_set) &&
      (!q ||
        k.toLowerCase().includes(q) ||
        v.description.toLowerCase().includes(q) ||
        v.provider_label.toLowerCase().includes(q)),
  )
  const known = new Set(CATEGORIES.map(([c]) => c))
  const groups = [
    ...CATEGORIES,
    ...[...new Set(shown.map(([, v]) => v.category))]
      .filter((c) => !known.has(c))
      .map((c) => [c, c.charAt(0).toUpperCase() + c.slice(1)] as [string, string]),
  ]
    .map(([c, label]) => ({
      label,
      rows: shown
        .filter(([, v]) => v.category === c)
        .sort(
          ([ka, a], [kb, b]) =>
            Number(b.is_set) - Number(a.is_set) ||
            (a.provider_label || '~').localeCompare(b.provider_label || '~') ||
            Number(a.advanced) - Number(b.advanced) ||
            ka.localeCompare(kb),
        ),
    }))
    .filter((g) => g.rows.length)
  return (
    <>
      <div className="flex flex-wrap items-center justify-between gap-2">
        <h2 className="text-[15px] font-semibold">Keys</h2>
        <Button
          size="sm"
          variant="primary"
          icon={<Plus className="size-3.5" />}
          onClick={() => setAdding(true)}
        >
          Add key
        </Button>
      </div>
      <p className="text-[13px] text-muted">
        Credentials for services (GitHub, Google, …) belong in{' '}
        <button
          type="button"
          onClick={onConnections}
          className="font-medium text-accent hover:underline"
        >
          Tools → Connections
        </button>{' '}
        at the gate, not here.
      </p>
      <div className="flex flex-col gap-2 sm:flex-row sm:items-center">
        <div className="relative min-w-0 flex-1">
          <Search className="pointer-events-none absolute left-2.5 top-1/2 size-4 -translate-y-1/2 text-subtle" />
          <Input
            value={text}
            onChange={(e) => setText(e.target.value)}
            placeholder="Search keys…"
            aria-label="Search keys"
            className="pl-8"
          />
        </div>
        <SegmentedControl
          label="Show"
          value={show}
          onChange={setShow}
          textFrom="always"
          className="self-start sm:self-auto"
          options={[
            { value: 'set', label: `Set (${setCount})`, icon: null },
            { value: 'all', label: `All (${rows.length})`, icon: null },
          ]}
        />
      </div>
      {env.isPending ? (
        <Section>
          <div className="space-y-2 p-4">
            <Skeleton className="h-10" />
            <Skeleton className="h-10" />
            <Skeleton className="h-10" />
          </div>
        </Section>
      ) : env.isError ? (
        <Section>
          <LoadError error={env.error} what="the keys" />
        </Section>
      ) : groups.length === 0 ? (
        <Section>
          <Empty icon={<KeyRound />} title={q ? `No key matches “${text.trim()}”` : 'No keys set'}>
            {!q && (
              <button
                type="button"
                onClick={() => setShow('all')}
                className="font-medium text-accent hover:underline"
              >
                Show all {rows.length}
              </button>
            )}
          </Empty>
        </Section>
      ) : (
        groups.map((g) => (
          <Section key={g.label} title={`${g.label} · ${g.rows.length}`}>
            <ul className="divide-y divide-line">
              {g.rows.map(([k, v]) => (
                <KeyRow key={k} id={id} name={k} info={v} />
              ))}
            </ul>
          </Section>
        ))
      )}
      <AddKeyDialog
        id={id}
        open={adding}
        onClose={() => setAdding(false)}
        taken={rows.map(([k]) => k)}
      />
    </>
  )
}

function useSetKey(id: number) {
  const qc = useQueryClient()
  return async (key: string, value: string) => {
    await hermes(id, 'PUT', 'env', { key, value })
    qc.invalidateQueries({ queryKey: ['assistant', id, 'env'] })
  }
}

function KeyRow({ id, name, info }: { id: number; name: string; info: EnvVar }) {
  const qc = useQueryClient()
  const setKey = useSetKey(id)
  const [editing, setEditing] = useState(false)
  const [value, setValue] = useState('')
  const [busy, setBusy] = useState(false)
  const [removing, setRemoving] = useState(false)
  const link = safeUrl(info.url)
  const save = async () => {
    if (!value.trim()) return
    setBusy(true)
    try {
      await setKey(name, value)
      toast(`${name} saved`, { kind: 'success' })
      setEditing(false)
      setValue('')
    } catch (e) {
      toastError(e)
    } finally {
      setBusy(false)
    }
  }
  const remove = () =>
    hermes(id, 'DELETE', 'env', { key: name })
      .then(() => {
        toast(`${name} removed`)
        qc.invalidateQueries({ queryKey: ['assistant', id, 'env'] })
      })
      .catch(toastError)
  return (
    <li className="px-4 py-2.5">
      <div className="flex items-start gap-3">
        <div className="min-w-0 flex-1">
          <div className="flex flex-wrap items-center gap-x-2">
            <span className="break-all font-mono text-[13px] font-medium">{name}</span>
            {info.provider_label && (
              <span className="text-xs text-subtle">{info.provider_label}</span>
            )}
          </div>
          {(info.description || link) && (
            <div className="line-clamp-2 text-xs text-muted">
              {info.description}
              {link && (
                <>
                  {' '}
                  <a
                    href={link}
                    target="_blank"
                    rel="noreferrer noopener"
                    className="text-accent hover:underline"
                  >
                    Get one
                  </a>
                </>
              )}
            </div>
          )}
          {info.is_set && info.redacted_value && (
            <div className="mt-0.5 truncate font-mono text-[11px] text-subtle">
              {info.redacted_value}
            </div>
          )}
        </div>
        {!editing && (
          <div className="flex shrink-0 items-center gap-1">
            <Button
              size="sm"
              variant={info.is_set ? 'ghost' : 'secondary'}
              onClick={() => setEditing(true)}
            >
              {info.is_set ? 'Replace' : 'Set'}
            </Button>
            {info.is_set && (
              <IconButton
                label={`Remove ${name}`}
                onClick={() => setRemoving(true)}
                icon={<Trash2 className="size-3.5" />}
              />
            )}
          </div>
        )}
      </div>
      {editing && (
        <form
          className="mt-2 flex flex-col gap-2 sm:flex-row"
          onSubmit={(e) => {
            e.preventDefault()
            save()
          }}
        >
          <Input
            autoFocus
            type={info.is_password ? 'password' : 'text'}
            autoComplete="off"
            aria-label={`New value for ${name}`}
            placeholder={info.is_set ? 'New value' : 'Value'}
            value={value}
            onChange={(e) => setValue(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === 'Escape') {
                e.stopPropagation()
                setEditing(false)
                setValue('')
              }
            }}
            className="font-mono sm:flex-1"
          />
          <div className="flex gap-2">
            <Button
              variant="ghost"
              onClick={() => {
                setEditing(false)
                setValue('')
              }}
            >
              Cancel
            </Button>
            <Button type="submit" variant="primary" loading={busy} disabled={!value.trim()}>
              Save
            </Button>
          </div>
        </form>
      )}
      <Confirm
        open={removing}
        onClose={() => setRemoving(false)}
        title={`Remove ${name}?`}
        body="Hermes forgets it, along with copies of it in its config and credential pool."
        confirmLabel="Remove"
        onConfirm={remove}
      />
    </li>
  )
}

function AddKeyDialog({
  id,
  open,
  onClose,
  taken,
}: {
  id: number
  open: boolean
  onClose: () => void
  taken: string[]
}) {
  const setKey = useSetKey(id)
  const [name, setName] = useState('')
  const [value, setValue] = useState('')
  const [busy, setBusy] = useState(false)
  const close = () => {
    setName('')
    setValue('')
    onClose()
  }
  const n = name.trim()
  const error =
    n && !NAME.test(n)
      ? 'Letters, digits and _, not starting with a digit'
      : taken.includes(n)
        ? 'That key is listed already; set it there'
        : ''
  const save = async () => {
    if (!n || error || !value) return
    setBusy(true)
    try {
      await setKey(n, value)
      toast(`${n} added`, { kind: 'success' })
      close()
    } catch (e) {
      toastError(e)
    } finally {
      setBusy(false)
    }
  }
  return (
    <Dialog
      open={open}
      onClose={close}
      title="Add a key"
      footer={
        <>
          <Button variant="ghost" onClick={close}>
            Cancel
          </Button>
          <Button
            variant="primary"
            onClick={save}
            loading={busy}
            disabled={!n || !!error || !value}
          >
            Add
          </Button>
        </>
      }
    >
      <form
        className="space-y-4 p-4"
        onSubmit={(e) => {
          e.preventDefault()
          save()
        }}
      >
        <Field label="Name" error={error}>
          <Input
            autoFocus
            value={name}
            onChange={(e) => setName(e.target.value.toUpperCase().replace(/[\s-]/g, '_'))}
            placeholder="MY_SERVICE_TOKEN"
            className="font-mono"
            autoComplete="off"
          />
        </Field>
        <Field label="Value">
          <Input
            type="password"
            value={value}
            onChange={(e) => setValue(e.target.value)}
            autoComplete="new-password"
            className="font-mono"
          />
        </Field>
        <button type="submit" hidden />
      </form>
    </Dialog>
  )
}
