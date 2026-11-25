import { useEffect, useMemo, useState, type Dispatch, type SetStateAction } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import clsx from 'clsx'
import { FileCode2, ListTree, Search, Undo2, X } from 'lucide-react'
import { hermes, hermesQuery } from '@/lib/assistant'
import { plural } from '@/lib/format'
import { Diff } from '@/components/Diff'
import { Button, IconButton } from '@/components/ui/button'
import { Dialog } from '@/components/ui/dialog'
import { Tip } from '@/components/ui/help'
import { Input, Select, Switch, Textarea, inputClass } from '@/components/ui/input'
import { Section, Skeleton } from '@/components/ui/misc'
import { Picker, Popover, usePopover } from '@/components/ui/popover'
import { SegmentedControl } from '@/components/ui/segmented'
import { toast, toastError } from '@/components/ui/toast'
import { Warning } from '../../settings/common'
import { getPath, isPinned, LoadError, nest, PINNED, PinnedBadge } from './common'
import { unifiedDiff } from './yaml-diff'

interface FieldDef {
  type: 'string' | 'number' | 'boolean' | 'list' | 'object' | 'select'
  description?: string
  category?: string
  options?: string[]
  searchable?: boolean
}

interface Schema {
  fields: Record<string, FieldDef>
  category_order: string[]
}

type Draft = Record<string, unknown>

const CATEGORY_NAMES: Record<string, string> = {
  tts: 'Text to speech',
  stt: 'Speech to text',
  lsp: 'LSP',
  moa: 'Mixture of agents',
  mcp: 'MCP',
  x_search: 'X search',
}

const catName = (c: string) =>
  CATEGORY_NAMES[c] ?? (c.charAt(0).toUpperCase() + c.slice(1)).replace(/_/g, ' ')

const human = (s: string) => {
  const t = s.replace(/_/g, ' ')
  return t.charAt(0).toUpperCase() + t.slice(1)
}

/** The schema's description, unless it only restates the key ("Agent → Max Turns"). */
function describe(key: string, d?: string) {
  if (!d || d.includes('→')) return ''
  const last = key.split('.').pop() ?? key
  return d.toLowerCase() === last.replace(/_/g, ' ').toLowerCase() ? '' : d
}

const same = (a: unknown, b: unknown) => JSON.stringify(a ?? null) === JSON.stringify(b ?? null)

export function ConfigSection({
  id,
  draft,
  setDraft,
}: {
  id: number
  draft: Draft
  setDraft: Dispatch<SetStateAction<Draft>>
}) {
  const [mode, setMode] = useState<'form' | 'yaml'>('form')
  const changes = Object.keys(draft).length
  return (
    <>
      <div className="flex flex-wrap items-center justify-between gap-2">
        <h2 className="text-[15px] font-semibold">Configuration</h2>
        <SegmentedControl
          label="Edit as"
          value={mode}
          onChange={setMode}
          textFrom="always"
          options={[
            { value: 'form', label: 'Form', icon: <ListTree className="size-3.5" /> },
            { value: 'yaml', label: 'YAML', icon: <FileCode2 className="size-3.5" /> },
          ]}
        />
      </div>
      {mode === 'form' ? (
        <ConfigForm id={id} draft={draft} setDraft={setDraft} />
      ) : (
        <>
          {changes > 0 && (
            <Warning>
              {plural(changes, 'unsaved change')} in the form {changes === 1 ? "isn't" : "aren't"}{' '}
              in this text. Save or discard {changes === 1 ? 'it' : 'them'} there first.
            </Warning>
          )}
          <YamlEditor id={id} />
        </>
      )}
    </>
  )
}

function ConfigForm({
  id,
  draft,
  setDraft,
}: {
  id: number
  draft: Draft
  setDraft: Dispatch<SetStateAction<Draft>>
}) {
  const qc = useQueryClient()
  const schema = useQuery(hermesQuery<Schema>(id, 'config/schema', undefined, 10 * 60_000))
  const config = useQuery(hermesQuery<Record<string, unknown>>(id, 'config', undefined, 30_000))
  const [text, setText] = useState('')
  const [cat, setCat] = useState('')
  const [saving, setSaving] = useState(false)
  const fields = useMemo(() => Object.entries(schema.data?.fields ?? {}), [schema.data])
  const categories = useMemo(() => {
    const all = [...new Set(fields.map(([, f]) => f.category || 'general'))]
    const order = schema.data?.category_order ?? []
    return [
      ...order.filter((c) => all.includes(c)),
      ...all.filter((c) => !order.includes(c)).sort(),
    ]
  }, [fields, schema.data])
  const active = cat && categories.includes(cat) ? cat : (categories[0] ?? '')
  const q = text.trim().toLowerCase()
  const shown = useMemo(
    () =>
      q
        ? fields.filter(
            ([k, f]) =>
              k.toLowerCase().includes(q) ||
              k.replace(/_/g, ' ').toLowerCase().includes(q) ||
              (f.description ?? '').toLowerCase().includes(q),
          )
        : fields.filter(([, f]) => (f.category || 'general') === active),
    [fields, q, active],
  )
  const changedIn = (c: string) =>
    Object.keys(draft).filter((k) => (schema.data?.fields[k]?.category || 'general') === c).length

  const original = (k: string) => getPath(config.data, k)
  const change = (k: string, v: unknown) =>
    setDraft((d) => {
      const next = { ...d }
      if (same(v, original(k))) delete next[k]
      else next[k] = v
      return next
    })
  const count = Object.keys(draft).length
  const save = async () => {
    setSaving(true)
    try {
      await hermes(id, 'PUT', 'config', { config: nest(draft) })
      setDraft({})
      toast(`Saved ${plural(count, 'setting')}`, { kind: 'success' })
      ;['config', 'config/raw', 'model/info', 'model/auxiliary'].forEach((p) =>
        qc.invalidateQueries({ queryKey: ['assistant', id, p] }),
      )
    } catch (e) {
      toastError(e)
    } finally {
      setSaving(false)
    }
  }

  if (schema.isError || config.isError)
    return (
      <Section>
        <LoadError error={schema.error ?? config.error} what="the configuration" />
      </Section>
    )
  if (!schema.data || !config.data)
    return (
      <Section>
        <div className="space-y-3 p-4">
          <Skeleton className="h-9" />
          <Skeleton className="h-14" />
          <Skeleton className="h-14" />
          <Skeleton className="h-14" />
        </div>
      </Section>
    )
  const groups: [string, [string, FieldDef][]][] = q
    ? categories
        .map(
          (c) =>
            [c, shown.filter(([, f]) => (f.category || 'general') === c)] as [
              string,
              [string, FieldDef][],
            ],
        )
        .filter(([, l]) => l.length)
    : [[active, shown]]
  return (
    <div>
      <Section>
        <div className="flex flex-col gap-2 border-b border-line p-3 sm:flex-row">
          <div className="relative min-w-0 flex-1">
            <Search className="pointer-events-none absolute left-2.5 top-1/2 size-4 -translate-y-1/2 text-subtle" />
            <Input
              value={text}
              onChange={(e) => setText(e.target.value)}
              placeholder={`Search ${fields.length} settings…`}
              aria-label="Search settings"
              className="pl-8 pr-8"
            />
            {text && (
              <IconButton
                label="Clear search"
                onClick={() => setText('')}
                className="absolute right-1 top-1/2 -translate-y-1/2"
                icon={<X className="size-3.5" />}
              />
            )}
          </div>
          {!q && (
            <div className="md:hidden">
              <Select aria-label="Category" value={active} onChange={(e) => setCat(e.target.value)}>
                {categories.map((c) => (
                  <option key={c} value={c}>
                    {catName(c) + (changedIn(c) ? ` · ${changedIn(c)} changed` : '')}
                  </option>
                ))}
              </Select>
            </div>
          )}
        </div>
        <div className="md:flex">
          {!q && (
            <nav
              aria-label="Categories"
              className="hidden max-h-[70vh] w-44 shrink-0 overflow-y-auto border-r border-line p-2 md:block"
            >
              {categories.map((c) => (
                <button
                  key={c}
                  type="button"
                  aria-current={c === active ? 'true' : undefined}
                  onClick={() => setCat(c)}
                  className={clsx(
                    'flex h-7 w-full items-center gap-2 rounded-md px-2 text-left text-[13px]',
                    c === active
                      ? 'bg-accent-soft font-medium text-fg'
                      : 'text-muted hover:bg-hover hover:text-fg',
                  )}
                >
                  <span className="min-w-0 flex-1 truncate">{catName(c)}</span>
                  {changedIn(c) > 0 && (
                    <span
                      className="size-1.5 shrink-0 rounded-full bg-accent"
                      aria-label="changed"
                    />
                  )}
                </button>
              ))}
            </nav>
          )}
          <div className="min-w-0 flex-1">
            {groups.length === 0 && (
              <div className="px-4 py-10 text-center text-[13px] text-subtle">
                No setting matches “{text.trim()}”
              </div>
            )}
            {groups.map(([c, list]) => (
              <div key={c}>
                {q && (
                  <div className="border-b border-line bg-sunken/50 px-4 py-1.5 text-[11px] font-semibold uppercase tracking-wide text-subtle">
                    {catName(c)}
                  </div>
                )}
                <ul className="divide-y divide-line">
                  {list.map(([k, f]) => (
                    <FieldRow
                      key={k}
                      name={k}
                      field={f}
                      value={k in draft ? draft[k] : original(k)}
                      changed={k in draft}
                      onChange={(v) => change(k, v)}
                      onUndo={() => change(k, original(k))}
                    />
                  ))}
                </ul>
              </div>
            ))}
          </div>
        </div>
      </Section>
      {count > 0 && (
        <div className="sticky bottom-0 z-10 -mx-1 mt-3 flex items-center gap-2 rounded-xl border border-line bg-raised px-3 py-2 shadow-pop">
          <span className="min-w-0 flex-1 truncate text-[13px]">
            {plural(count, 'unsaved change')}
          </span>
          <Button variant="ghost" size="sm" onClick={() => setDraft({})} disabled={saving}>
            Discard
          </Button>
          <Button variant="primary" size="sm" onClick={save} loading={saving}>
            Save
          </Button>
        </div>
      )}
    </div>
  )
}

function FieldRow({
  name,
  field,
  value,
  changed,
  onChange,
  onUndo,
}: {
  name: string
  field: FieldDef
  value: unknown
  changed: boolean
  onChange: (v: unknown) => void
  onUndo: () => void
}) {
  const pinned = isPinned(name)
  const desc = describe(name, field.description)
  const last = name.split('.').pop() ?? name
  const wide = field.type === 'list' || field.type === 'object'
  return (
    <li
      className={clsx(
        'flex flex-col gap-2 border-l-2 px-4 py-2.5',
        changed ? 'border-l-accent bg-accent-soft/40' : 'border-l-transparent',
        !wide && 'sm:flex-row sm:items-center sm:gap-4',
      )}
    >
      <div className="min-w-0 flex-1">
        <div className="flex flex-wrap items-center gap-1.5">
          <span className="text-[13px] font-medium">{human(last)}</span>
          {pinned && <PinnedBadge />}
          {changed && (
            <IconButton
              size="xs"
              label="Undo this change"
              onClick={onUndo}
              icon={<Undo2 className="size-3.5" />}
            />
          )}
        </div>
        {desc && <div className="text-xs text-muted">{desc}</div>}
        <div className="truncate font-mono text-[11px] text-subtle">{name}</div>
      </div>
      <div
        className={clsx(
          'shrink-0',
          !wide && 'sm:w-64',
          field.type === 'boolean' && 'sm:flex sm:justify-end',
        )}
      >
        <FieldControl
          name={name}
          field={field}
          value={value}
          onChange={onChange}
          disabled={pinned}
        />
      </div>
    </li>
  )
}

function FieldControl({
  name,
  field,
  value,
  onChange,
  disabled,
}: {
  name: string
  field: FieldDef
  value: unknown
  onChange: (v: unknown) => void
  disabled: boolean
}) {
  const label = human(name.split('.').pop() ?? name)
  switch (field.type) {
    case 'boolean':
      return <Switch checked={!!value} onChange={onChange} label={label} disabled={disabled} />
    case 'number':
      return (
        <Input
          type="number"
          inputMode="decimal"
          aria-label={label}
          disabled={disabled}
          value={value === null || value === undefined ? '' : String(value)}
          onChange={(e) => onChange(e.target.value === '' ? null : Number(e.target.value))}
        />
      )
    case 'select': {
      const opts = field.options ?? []
      const cur = value === null || value === undefined ? '' : String(value)
      const all = opts.includes(cur) ? opts : [cur, ...opts]
      if (field.searchable || all.length > 30)
        return (
          <SearchSelect
            label={label}
            value={cur}
            options={all}
            onChange={onChange}
            disabled={disabled}
          />
        )
      return (
        <Select
          aria-label={label}
          value={cur}
          disabled={disabled}
          onChange={(e) => onChange(e.target.value)}
        >
          {all.map((o) => (
            <option key={o} value={o}>
              {o === '' ? '(none)' : o}
            </option>
          ))}
        </Select>
      )
    }
    case 'list':
    case 'object':
      return <StructuredField label={label} value={value} onChange={onChange} disabled={disabled} />
    default:
      return (
        <Input
          aria-label={label}
          disabled={disabled}
          value={value === null || value === undefined ? '' : String(value)}
          onChange={(e) => {
            const t = e.target.value
            onChange(
              typeof value === 'number' && t.trim() !== '' && !isNaN(Number(t)) ? Number(t) : t,
            )
          }}
        />
      )
  }
}

/** A select with many options (time zones): a searchable list in a popover. */
function SearchSelect({
  label,
  value,
  options,
  onChange,
  disabled,
}: {
  label: string
  value: string
  options: string[]
  onChange: (v: string) => void
  disabled: boolean
}) {
  const pop = usePopover()
  const items = useMemo(
    () => options.map((o) => ({ value: o, label: o === '' ? '(none)' : o })),
    [options],
  )
  return (
    <>
      <button
        ref={pop.anchor}
        type="button"
        aria-label={label}
        aria-haspopup="listbox"
        aria-expanded={pop.open}
        disabled={disabled}
        onClick={pop.toggle}
        className={clsx(inputClass, 'flex h-9 items-center text-left')}
      >
        <span className={clsx('min-w-0 flex-1 truncate', !value && 'text-subtle')}>
          {value || '(none)'}
        </span>
      </button>
      <Popover open={pop.open} onClose={pop.close} anchor={pop.anchor}>
        <Picker items={items} selected={[value]} onPick={(v) => onChange(v)} onClose={pop.close} />
      </Popover>
    </>
  )
}

/**
 * A list (one item a line, when its items are plain) or anything else as
 * JSON. The text is kept as typed; the value changes only when it reads.
 */
function StructuredField({
  label,
  value,
  onChange,
  disabled,
}: {
  label: string
  value: unknown
  onChange: (v: unknown) => void
  disabled: boolean
}) {
  const plain =
    Array.isArray(value) && value.every((x) => typeof x === 'string' || typeof x === 'number')
  const format = (v: unknown) =>
    plain ? (v as unknown[]).join('\n') : v === undefined ? '' : JSON.stringify(v, null, 2)
  const [text, setText] = useState(() => format(value))
  const [error, setError] = useState('')
  const read = (t: string): unknown =>
    plain
      ? t
          .split('\n')
          .map((s) => s.trim())
          .filter(Boolean)
      : t.trim() === ''
        ? null
        : JSON.parse(t)
  useEffect(() => {
    let mine: unknown
    try {
      mine = read(text)
    } catch {
      mine = Symbol()
    }
    if (!same(mine, value)) {
      setText(format(value))
      setError('')
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [value])
  return (
    <div>
      <Textarea
        aria-label={label}
        disabled={disabled}
        spellCheck={false}
        rows={Math.min(8, Math.max(2, text.split('\n').length))}
        value={text}
        placeholder={plain || Array.isArray(value) ? 'One a line' : undefined}
        className="font-mono text-xs"
        onChange={(e) => {
          setText(e.target.value)
          try {
            onChange(read(e.target.value))
            setError('')
          } catch {
            setError('Not valid JSON yet')
          }
        }}
      />
      {error && <div className="mt-1 text-xs text-danger">{error}</div>}
    </div>
  )
}

function YamlEditor({ id }: { id: number }) {
  const qc = useQueryClient()
  const raw = useQuery({
    ...hermesQuery<{ yaml: string; path: string }>(id, 'config/raw', undefined, 0),
    refetchOnWindowFocus: false,
  })
  const [text, setText] = useState<string | null>(null)
  const [review, setReview] = useState<{ latest: string } | null>(null)
  const [saving, setSaving] = useState(false)
  const base = raw.data?.yaml ?? ''
  const value = text ?? base
  const dirty = text !== null && text !== base
  const open = async () => {
    try {
      const latest = await hermes<{ yaml: string }>(id, 'GET', 'config/raw')
      setReview({ latest: latest.yaml })
    } catch (e) {
      toastError(e)
    }
  }
  const save = async () => {
    setSaving(true)
    try {
      await hermes(id, 'PUT', 'config/raw', { yaml_text: value })
      setReview(null)
      toast('Saved config.yaml', { kind: 'success' })
      await qc.invalidateQueries({ queryKey: ['assistant', id, 'config/raw'] })
      setText(null)
      ;['config', 'model/info', 'model/auxiliary'].forEach((p) =>
        qc.invalidateQueries({ queryKey: ['assistant', id, p] }),
      )
    } catch (e) {
      toastError(e)
    } finally {
      setSaving(false)
    }
  }
  const diff = useMemo(() => (review ? unifiedDiff(review.latest, value) : ''), [review, value])
  return (
    <Section
      title={
        <span title={raw.data?.path} className="font-mono text-xs font-normal text-muted">
          <span className="sm:hidden">config.yaml</span>
          <span className="hidden sm:inline">{raw.data?.path ?? 'config.yaml'}</span>
        </span>
      }
      actions={
        <div className="flex items-center gap-1.5">
          {dirty && (
            <Button size="sm" variant="ghost" onClick={() => setText(null)}>
              Discard
            </Button>
          )}
          <Button size="sm" variant="primary" disabled={!dirty} onClick={open}>
            Review and save
          </Button>
        </div>
      }
    >
      {raw.isPending ? (
        <div className="p-4">
          <Skeleton className="h-80" />
        </div>
      ) : raw.isError ? (
        <LoadError error={raw.error} what="config.yaml" />
      ) : (
        <>
          <div className="flex items-center gap-1 border-b border-line px-4 py-2 text-xs text-subtle">
            Saving replaces the whole file. The deploy puts back the settings it pins.
            <Tip label="Settings the deploy pins">
              <span className="font-mono">{PINNED.join(', ')}</span>
            </Tip>
          </div>
          <textarea
            aria-label="config.yaml"
            spellCheck={false}
            autoCapitalize="off"
            autoCorrect="off"
            wrap="off"
            value={value}
            onChange={(e) => setText(e.target.value)}
            rows={Math.min(40, Math.max(16, value.split('\n').length + 1))}
            className="block w-full resize-y rounded-b-xl bg-panel px-4 py-3 font-mono text-xs leading-relaxed text-fg outline-none focus:bg-sunken/40"
          />
        </>
      )}
      <Dialog
        open={!!review}
        onClose={() => setReview(null)}
        wide
        title="Save config.yaml?"
        footer={
          <>
            <Button variant="ghost" onClick={() => setReview(null)}>
              Keep editing
            </Button>
            <Button variant="primary" onClick={save} loading={saving} disabled={!diff}>
              Save
            </Button>
          </>
        }
      >
        <div className="space-y-3 p-4">
          {review && review.latest !== base && (
            <Warning>
              It changed since you opened it. Saving also undoes those changes, shown below as
              removed.
            </Warning>
          )}
          <div className="overflow-hidden rounded-lg border border-line">
            <Diff diff={diff} className="max-h-[55vh] overflow-y-auto" />
          </div>
        </div>
      </Dialog>
    </Section>
  )
}
