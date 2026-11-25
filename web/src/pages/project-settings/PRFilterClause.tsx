import { useEffect, useState, type ReactNode } from 'react'
import clsx from 'clsx'
import { Plus, X } from 'lucide-react'
import { IconButton } from '@/components/ui/button'
import { Input, Select } from '@/components/ui/input'
import { MenuList, Popover, usePopover } from '@/components/ui/popover'
import { Tip } from '@/components/ui/help'
import type { PRFilter } from '@/lib/types'
import { Clause, inlineInput } from './automation-fields'

type ListKey = 'authors' | 'skip_authors' | 'labels' | 'paths' | 'skip_paths' | 'repos'
type CondKey = ListKey | 'size'

const parseList = (v: string) =>
  v
    .split(',')
    .map((x) => x.trim())
    .filter(Boolean)
const sameList = (a: string[], b: string[]) =>
  a.length === b.length && a.every((x, i) => x === b[i])

/** A comma separated list that keeps what is typed (spaces, a trailing comma)
 * while the filter gets the cleaned list. */
function ListInput({
  value,
  onChange,
  className,
  ...rest
}: {
  value?: string[]
  onChange: (v: string[]) => void
  className?: string
  placeholder?: string
  'aria-label': string
  autoFocus?: boolean
}) {
  const [text, setText] = useState(() => (value ?? []).join(', '))
  useEffect(() => {
    if (!sameList(parseList(text), value ?? [])) setText((value ?? []).join(', '))
  }, [value]) // eslint-disable-line react-hooks/exhaustive-deps
  return (
    <Input
      {...rest}
      value={text}
      onChange={(e) => {
        setText(e.target.value)
        onChange(parseList(e.target.value))
      }}
      className={clsx(inlineInput, 'min-w-0', className)}
    />
  )
}

/** One condition on one line: what it says, its inputs (which narrow on a
 * phone), its help and a way to drop it. */
function Cond({
  label,
  name,
  tip,
  onRemove,
  children,
}: {
  label: ReactNode
  name: string
  tip?: ReactNode
  onRemove: () => void
  children: ReactNode
}) {
  return (
    <span className="inline-flex max-w-full min-w-0 items-center gap-1.5 rounded-lg border border-line bg-panel py-0.5 pr-0.5 pl-2 whitespace-nowrap">
      {label}
      {children}
      <span className="inline-flex shrink-0 items-center gap-1">
        {tip}
        <IconButton
          label={`Remove “${name}”`}
          size="xs"
          onClick={onRemove}
          icon={<X className="size-3.5" />}
        />
      </span>
    </span>
  )
}

const CONDS: { key: CondKey; menu: string; except?: boolean }[] = [
  { key: 'authors', menu: 'By certain authors' },
  { key: 'labels', menu: 'With labels' },
  { key: 'paths', menu: 'Touching certain files' },
  { key: 'size', menu: 'Of a size' },
  { key: 'repos', menu: 'In certain repositories' },
  { key: 'skip_authors', menu: 'Except by certain authors', except: true },
  { key: 'skip_paths', menu: 'Except when it only touches certain files', except: true },
]

export function PRFilterClause({
  filter: f,
  event,
  set,
}: {
  filter: PRFilter
  event: string
  set: (p: Partial<PRFilter>) => void
}) {
  const [opened, setOpened] = useState<CondKey[]>([])
  const pop = usePopover()
  const has = (k: CondKey) =>
    opened.includes(k) ||
    (k === 'size' ? !!(f.min_changed_lines || f.max_changed_lines) : !!f[k]?.length)
  const add = (k: CondKey) => setOpened((o) => [...o, k])
  const remove = (k: CondKey) => {
    setOpened((o) => o.filter((x) => x !== k))
    set(k === 'size' ? { min_changed_lines: undefined, max_changed_lines: undefined } : { [k]: [] })
  }
  const fresh = (k: CondKey) => opened.includes(k) && !(k !== 'size' && f[k]?.length)
  const num = (v: string) => (v === '' ? undefined : Math.max(0, Math.round(Number(v))))
  const updated = event === 'pr_updated'
  const named = has('authors')
  const only = CONDS.filter((c) => !c.except && has(c.key))
  const except = CONDS.filter((c) => c.except && has(c.key))
  const left = CONDS.filter((c) => !has(c.key))

  const list = (k: ListKey, placeholder: string, mono: boolean, width: string) => (
    <ListInput
      value={f[k]}
      onChange={(v) => set({ [k]: v })}
      placeholder={placeholder}
      aria-label={CONDS.find((c) => c.key === k)!.menu}
      autoFocus={fresh(k)}
      className={clsx(width, mono && 'font-mono !text-[12px]')}
    />
  )
  const cond = (k: CondKey): ReactNode => {
    const name = CONDS.find((c) => c.key === k)!.menu
    const box = (label: ReactNode, body: ReactNode, tip?: ReactNode) => (
      <Cond key={k} label={label} name={name} tip={tip} onRemove={() => remove(k)}>
        {body}
      </Cond>
    )
    switch (k) {
      case 'authors':
        return box(
          'by',
          list('authors', 'ann, renovate*', false, '!w-40'),
          <Tip>
            Forge usernames, comma separated; <b>*</b> stands for anything, so <b>*[bot]</b> is
            every GitHub app. Naming authors lets their bots in too.
          </Tip>,
        )
      case 'labels':
        return box(
          'labelled',
          list('labels', 'backend', false, '!w-36'),
          <Tip>It needs every one of these labels.</Tip>,
        )
      case 'paths':
        return box(
          'touching',
          list('paths', 'web/**', true, '!w-40'),
          <Tip>
            Path patterns, comma separated: it must change at least one matching file.
            <b> web/**</b> is everything under web.
          </Tip>,
        )
      case 'size':
        return box(
          'size',
          <>
            <Input
              type="number"
              min={0}
              value={f.min_changed_lines ?? ''}
              onChange={(e) => set({ min_changed_lines: num(e.target.value) })}
              placeholder="0"
              aria-label="At least this many changed lines"
              autoFocus={fresh('size')}
              className={clsx(inlineInput, 'min-w-12 !w-16')}
            />
            –
            <Input
              type="number"
              min={0}
              value={f.max_changed_lines ?? ''}
              onChange={(e) => set({ max_changed_lines: num(e.target.value) })}
              placeholder="any"
              aria-label="At most this many changed lines"
              className={clsx(inlineInput, 'min-w-12 !w-16')}
            />
            lines
          </>,
          <Tip>
            Changed lines (added plus deleted), at least and at most. Leave a side empty for no
            limit.
          </Tip>,
        )
      case 'repos':
        return box(
          'in',
          list('repos', 'owner/app', true, '!w-40'),
          <Tip>
            Repositories attached to this project, comma separated; <b>owner/*</b> works too.
          </Tip>,
        )
      case 'skip_authors':
        return box('by', list('skip_authors', 'renovate*', false, '!w-36'))
      case 'skip_paths':
        return box(
          'only touching',
          list('skip_paths', 'docs/**, *.md', true, '!w-40'),
          <Tip>
            Leave it out when every file it changes matches one of these, like a docs-only change.
          </Tip>,
        )
    }
  }

  return (
    <>
      <Clause word="for">
        {updated && (
          <Select
            variant="inline"
            value={f.drafts ? 'all' : 'ready'}
            onChange={(e) => set({ drafts: e.target.value === 'all' })}
            aria-label="Drafts"
          >
            <option value="ready">ready pull requests</option>
            <option value="all">pull requests and drafts</option>
          </Select>
        )}
        {updated && !named && (
          <Select
            variant="inline"
            value={f.bots ? 'all' : 'people'}
            onChange={(e) => set({ bots: e.target.value === 'all' })}
            aria-label="Authors"
          >
            <option value="people">by people</option>
            <option value="all">by anyone, bots too</option>
          </Select>
        )}
        into
        <ListInput
          value={f.bases}
          onChange={(v) => set({ bases: v })}
          placeholder="any branch"
          className="!w-36 font-mono !text-[12px]"
          aria-label="Target branches"
        />
        <Tip>
          Branch patterns, comma separated: <b>main</b>, <b>release/*</b>. Empty is any branch.
        </Tip>
      </Clause>
      {only.length > 0 && <Clause word="only if">{only.map((c) => cond(c.key))}</Clause>}
      <Clause word="except">
        labelled
        <ListInput
          value={f.skip_labels}
          onChange={(v) => set({ skip_labels: v })}
          placeholder="no labels"
          className="!w-40"
          aria-label="Skip labels"
        />
        or from
        <ListInput
          value={f.skip_heads}
          onChange={(v) => set({ skip_heads: v })}
          placeholder="no branches"
          className="!w-40 font-mono !text-[12px]"
          aria-label="Skip head branches"
        />
        <Tip>
          Leave out pull requests with these labels (like <b>dependencies</b>) or from these
          branches (like <b>release-please--*</b>), comma separated.
        </Tip>
        {except.map((c) => cond(c.key))}
      </Clause>
      {left.length > 0 && (
        <div className="sm:pl-16">
          <button
            ref={pop.anchor}
            type="button"
            onClick={pop.toggle}
            aria-haspopup="menu"
            aria-expanded={pop.open}
            className="inline-flex items-center gap-1 text-xs text-accent hover:underline"
          >
            <Plus className="size-3" />
            More conditions…
          </button>
          <Popover open={pop.open} onClose={pop.close} anchor={pop.anchor}>
            <MenuList
              items={left.map((c) => ({ label: c.menu, onSelect: () => add(c.key) }))}
              onClose={pop.close}
            />
          </Popover>
        </div>
      )}
    </>
  )
}
