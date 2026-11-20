// Links between anything (WP-D10): design notes, glossary terms, issues,
// pages, files and URLs, shown from both ends with direction-aware labels.
// One block for the glossary, design notes, pages and issues.

import { useEffect, useId, useMemo, useRef, useState, type ReactNode } from 'react'
import { Link } from '@tanstack/react-router'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import clsx from 'clsx'
import {
  BookA,
  Check,
  CircleDot,
  ExternalLink,
  FileCode,
  FileText,
  GitCommit,
  GitPullRequest,
  Lightbulb,
  Link2,
  Plus,
  Search,
  X,
} from 'lucide-react'
import { agentLabel } from '@/lib/agents'
import { useDebounced, useMediaQuery } from '@/lib/hooks'
import { safeUrl } from '@/lib/format'
import {
  DESIGN_KIND_LABEL,
  REL_LABEL,
  goalApi,
  invalidateKnowledge,
  itemHref,
  linkApi,
  linksQuery,
  searchAllQuery,
} from '@/lib/knowledge'
import type { Category, DesignKind, IssueType, KItem, KLink, LinkEnd, LinkRel } from '@/lib/types'
import { IconButton } from '@/components/ui/button'
import { Select } from '@/components/ui/input'
import { Popover, usePopover } from '@/components/ui/popover'
import { Skeleton } from '@/components/ui/misc'
import { toastError } from '@/components/ui/toast'
import { StatusIcon, TypeIcon } from '@/components/issue/icons'
import { BlockTitle } from './knowledge'

const GROUPS: { type: KItem['type']; label: string }[] = [
  { type: 'design', label: 'Design notes' },
  { type: 'glossary', label: 'Glossary' },
  { type: 'issue', label: 'Issues' },
  { type: 'page', label: 'Pages' },
  { type: 'file', label: 'Files' },
  { type: 'url', label: 'PRs, commits & links' },
]

/** Relations a person can choose; "mentions" come from [[wikilinks]]. */
const PICKABLE: LinkRel[] = [
  'relates',
  'implements',
  'serves',
  'depends_on',
  'supersedes',
  'tension',
]

export function ItemIcon({ item, className }: { item: KItem; className?: string }) {
  const c = clsx('size-3.5 shrink-0 text-subtle', className)
  switch (item.type) {
    case 'issue':
      return item.state ? (
        <StatusIcon category={item.state as Category} className={c} />
      ) : (
        <TypeIcon type={(item.kind ?? 'task') as IssueType} className={c} />
      )
    case 'page':
      return <FileText className={c} />
    case 'glossary':
      return <BookA className={c} />
    case 'design':
      return <Lightbulb className={c} />
    case 'file':
      return <FileCode className={c} />
  }
  if (item.kind === 'pull') return <GitPullRequest className={c} />
  if (item.kind === 'commit') return <GitCommit className={c} />
  if (item.kind === 'issue') return <CircleDot className={c} />
  return <Link2 className={c} />
}

/** An item's title as a link: in the app, out to the web, or a plain file path. */
export function ItemTitle({ item }: { item: KItem }) {
  const href = itemHref(item)
  const label = (
    <>
      {item.type === 'issue' && item.key && <span className="text-subtle">{item.key} </span>}
      <span
        className={clsx(
          item.type === 'file' && 'font-mono text-[12px]',
          item.type === 'issue' &&
            (item.state === 'done' || item.state === 'canceled') &&
            'text-muted line-through decoration-subtle',
        )}
      >
        {item.title}
      </span>
    </>
  )
  if (href)
    return (
      <Link to={href} className="min-w-0 flex-1 truncate hover:underline">
        {label}
      </Link>
    )
  const url = item.type === 'url' ? safeUrl(item.ref) : undefined
  if (url)
    return (
      <a
        href={url}
        target="_blank"
        rel="noopener noreferrer"
        className="inline-flex min-w-0 flex-1 items-center gap-1 hover:underline"
      >
        <span className="truncate">{label}</span>
        <ExternalLink className="size-3 shrink-0 text-subtle" />
      </a>
    )
  return (
    <span className="min-w-0 flex-1 truncate" title={item.ref}>
      {label}
    </span>
  )
}

export function Links({
  at,
  editable,
  title = 'Links',
  actions,
  hideEmpty,
  skipMentions,
}: {
  /** the item whose links these are */
  at: LinkEnd
  editable: boolean
  title?: string
  /** more buttons beside "Link" */
  actions?: ReactNode
  /** render nothing when there are no links and nothing can be added */
  hideEmpty?: boolean
  /** leave out links made from text; the issue page lists those on their own */
  skipMentions?: boolean
}) {
  const qc = useQueryClient()
  const links = useQuery(linksQuery(at))
  const pop = usePopover()
  const refresh = () => {
    qc.invalidateQueries({ queryKey: ['links'] })
    // Page↔issue links also show on the issue and the page.
    if (at.type === 'issue' || at.type === 'page') {
      qc.invalidateQueries({ queryKey: ['issue'] })
      qc.invalidateQueries({ queryKey: ['page'] })
    }
    qc.invalidateQueries({ queryKey: ['spec'] })
  }
  const remove = useMutation({
    mutationFn: (l: KLink) => linkApi.remove(at, l),
    onMutate: (l) => {
      // Drop it from the list straight away.
      qc.setQueryData<KLink[]>(linksQuery(at).queryKey, (old) =>
        old?.filter((x) => !(x.id === l.id && x.via === l.via && x.item.id === l.item.id)),
      )
    },
    onError: toastError,
    onSettled: refresh,
  })
  // An agent's link or Waypoint's guess becomes the person's (WP-ADR50).
  const confirm = useMutation({
    mutationFn: (l: KLink) => goalApi.confirm(l.id),
    onError: toastError,
    onSettled: () => {
      refresh()
      invalidateKnowledge(qc)
    },
  })
  const list = (links.data ?? []).filter((l) => !(skipMentions && l.auto && l.rel === 'mentions'))
  const groups = useMemo(
    () =>
      GROUPS.map((g) => ({ ...g, links: list.filter((l) => l.item.type === g.type) }))
        .filter((g) => g.links.length > 0)
        .map((g) => {
          // One kind of link across the group is said once, in its header.
          const rels = new Set(g.links.map((l) => relOf(l)))
          return { ...g, rel: rels.size === 1 ? [...rels][0] : undefined }
        }),
    [list],
  )
  if (hideEmpty && !editable && list.length === 0) return null
  return (
    <section>
      <BlockTitle
        actions={
          <div className="flex items-center gap-1">
            {actions}
            {editable && (
              <IconButton
                ref={pop.anchor}
                onClick={pop.toggle}
                label="Add a link"
                size="xs"
                icon={<Plus className="size-3.5" />}
              />
            )}
          </div>
        }
      >
        {title}
      </BlockTitle>
      {links.isPending ? (
        <Skeleton className="h-9" />
      ) : links.isError ? (
        <p className="text-[13px] text-danger">{links.error.message}</p>
      ) : list.length === 0 ? (
        <p className="text-[13px] text-subtle">Nothing linked.</p>
      ) : (
        <div className="overflow-hidden rounded-lg border border-line">
          {groups.map((g) => (
            <div key={g.type} className="border-b border-line last:border-b-0">
              <div className="bg-sunken/50 px-3 pb-0.5 pt-1.5 text-[11px] font-medium text-subtle">
                {g.label}
                {g.rel && <span className="font-normal"> · {g.rel}</span>}
              </div>
              <ul>
                {g.links.map((l) => (
                  <LinkRow
                    key={`${l.via ?? 'k'}:${l.id}:${l.item.type}:${l.item.id ?? l.item.ref}`}
                    link={l}
                    showRel={!g.rel}
                    onRemove={() => remove.mutate(l)}
                    onConfirm={() => confirm.mutate(l)}
                  />
                ))}
              </ul>
            </div>
          ))}
        </div>
      )}
      <Popover open={pop.open} onClose={pop.close} anchor={pop.anchor} align="end">
        <LinkPicker at={at} existing={list} onDone={refresh} />
      </Popover>
    </section>
  )
}

const relOf = (l: KLink) => REL_LABEL[l.rel]?.[l.direction] ?? l.rel

function LinkRow({
  link: l,
  showRel,
  onRemove,
  onConfirm,
}: {
  link: KLink
  showRel: boolean
  onRemove: () => void
  onConfirm: () => void
}) {
  const rel = relOf(l)
  const by =
    l.basis === 'inferred'
      ? 'Waypoint (a guess)'
      : l.created_agent
        ? `an agent (${agentLabel(l.created_agent)})`
        : l.created_by?.display_name
  const confirmed = l.confirmed_by && ` · confirmed by ${l.confirmed_by.display_name}`
  return (
    <li
      title={
        [
          l.note,
          l.auto && 'From a [[wikilink]] in the text; edit the text to change it',
          by && `Linked by ${by}${confirmed || ''}`,
        ]
          .filter(Boolean)
          .join(' · ') || undefined
      }
      className="group flex min-h-9 items-center gap-2 px-3 py-1.5 text-[13px]"
    >
      <ItemIcon item={l.item} />
      <ItemTitle item={l.item} />
      {l.item.type === 'design' && l.item.state === 'proposed' && (
        <span className="shrink-0 rounded bg-warn/15 px-1 text-[10px] font-medium text-warn">
          proposed
        </span>
      )}
      {l.item.type === 'design' && l.item.kind && (
        <span className="hidden shrink-0 text-[11px] text-subtle sm:inline">
          {DESIGN_KIND_LABEL[l.item.kind as DesignKind] ?? l.item.kind}
        </span>
      )}
      {showRel && <span className="shrink-0 text-[11px] text-subtle">{rel}</span>}
      {!l.auto && l.basis !== 'person' && !l.confirmed_by && (
        <span
          title={
            l.basis === 'inferred'
              ? 'Waypoint guessed this link; no person stands behind it yet'
              : 'An agent made this link; no person stands behind it yet'
          }
          className="shrink-0 rounded bg-sunken px-1 text-[10px] font-medium text-muted"
        >
          {l.basis === 'inferred'
            ? 'guess'
            : l.created_agent
              ? agentLabel(l.created_agent)
              : 'agent'}
        </span>
      )}
      {l.can_confirm && (
        <button
          onClick={onConfirm}
          title="Stand behind this link: it counts as yours"
          className="inline-flex shrink-0 items-center gap-0.5 rounded px-1 text-[11px] text-subtle hover:bg-hover hover:text-fg pointer-fine:opacity-0 pointer-fine:group-hover:opacity-100 pointer-fine:focus-visible:opacity-100"
        >
          <Check className="size-3" /> Confirm
        </button>
      )}
      {l.can_remove ? (
        <IconButton
          label={`Remove link to ${l.item.title}`}
          size="xs"
          onClick={onRemove}
          icon={<X className="size-3.5" />}
          className="opacity-0 focus-visible:opacity-100 group-hover:opacity-100 max-sm:opacity-100"
        />
      ) : (
        <span className="size-6 shrink-0" />
      )}
    </li>
  )
}

const KEY_RE = /^[A-Z][A-Z0-9]{1,9}-\d+$/i
const URL_RE = /^https?:\/\/\S+$/i
const FILE_RE = /^[^\s]+(\/[^\s]*|\.[A-Za-z0-9]{1,8}(:\d+(-\d+)?)?)$/

/** Search results offered per kind of item. */
const PER_KIND = 6

interface Choice {
  id: string
  to: LinkEnd
  item: KItem
  hint?: string
}

/** Search issues, design notes, terms and pages, or take a pasted URL, file path or key. */
function LinkPicker({
  at,
  existing,
  onDone,
}: {
  at: LinkEnd
  existing: KLink[]
  onDone: () => void
}) {
  const [text, setText] = useState('')
  const [rel, setRel] = useState<LinkRel>('relates')
  const [active, setActive] = useState(0)
  const box = useRef<HTMLInputElement>(null)
  const mobile = useMediaQuery('(max-width: 639px)')
  const t = text.trim()
  const debounced = useDebounced(t, 180)
  const isURL = URL_RE.test(t)
  const results = useQuery({
    // More than are shown: what's already linked is left out of the results.
    ...searchAllQuery({
      q: debounced,
      kinds: 'issues,design,glossary,pages',
      limit: PER_KIND + existing.length + 1,
    }),
    enabled: !!debounced && !URL_RE.test(debounced),
    placeholderData: (p) => p,
  })
  const add = useMutation({
    mutationFn: (c: Choice) => linkApi.add(at, c.to, rel),
    onSuccess: () => {
      // The picker stays for the next link; Esc closes it.
      setText('')
      setActive(0)
      box.current?.focus()
      onDone()
    },
    onError: toastError,
  })
  const self = (type: string, id?: number, key?: string) =>
    type === at.type && (at.type === 'issue' ? key === at.key : id === at.id)
  const linked = (it: KItem) =>
    existing.some(
      (l) =>
        l.direction === 'out' &&
        l.rel === rel &&
        l.item.type === it.type &&
        (it.id ? l.item.id === it.id : it.key ? l.item.key === it.key : l.item.ref === it.ref),
    )
  const choices = useMemo<Choice[]>(() => {
    const out: Choice[] = []
    if (isURL) {
      out.push({ id: 'url', to: { ref: t }, item: { type: 'url', ref: t, title: t }, hint: 'URL' })
      return out
    }
    if (KEY_RE.test(t) && !self('issue', undefined, t.toUpperCase()))
      out.push({
        id: 'key',
        to: { type: 'issue', key: t.toUpperCase() },
        item: { type: 'issue', key: t.toUpperCase(), title: '' },
        hint: 'issue',
      })
    if (FILE_RE.test(t) && !KEY_RE.test(t))
      out.push({
        id: 'file',
        to: { ref: 'file:' + t },
        item: { type: 'file', ref: t, title: t },
        hint: 'file',
      })
    if (debounced && results.data)
      for (const g of results.data.groups) {
        let shown = 0
        for (const h of g.hits) {
          if (g.kind === 'comment' || shown === PER_KIND || self(g.kind, h.id, h.key)) continue
          const item: KItem = {
            type: g.kind,
            id: h.id,
            key: h.key,
            title: h.title,
            state: g.kind === 'issue' ? h.state : undefined,
            project_key: h.project_key,
          }
          // A key or URL typed out still says "linked"; a search just moves on.
          if (linked(item)) continue
          if (out.some((o) => o.item.type === 'issue' && o.item.key === h.key && h.key)) continue
          shown++
          out.push({
            id: `${g.kind}:${h.id}`,
            to: g.kind === 'issue' ? { type: 'issue', key: h.key } : { type: g.kind, id: h.id },
            item,
            hint: [g.kind === 'design' ? h.sub : g.kind, h.project_key ?? 'global']
              .filter(Boolean)
              .join(' · '),
          })
        }
      }
    return out
  }, [t, isURL, debounced, results.data, rel, existing]) // eslint-disable-line react-hooks/exhaustive-deps
  const pick = (c?: Choice) => c && !add.isPending && add.mutate(c)
  // Until the search for what is typed answers, the rows are the last
  // search's: Enter waits for the answer rather than linking one of those.
  const settled = !t || isURL || (debounced === t && !results.isPlaceholderData)
  const listId = useId()
  const enterLater = useRef(false)
  useEffect(() => {
    if (!settled || !enterLater.current) return
    enterLater.current = false
    pick(choices[active])
  })
  return (
    <div className="flex w-full flex-col sm:w-80">
      <div className="flex items-center gap-2 border-b border-line px-2 pb-1.5 pt-0.5">
        <Search className="size-3.5 shrink-0 text-subtle" />
        <input
          ref={box}
          autoFocus={!mobile}
          value={text}
          onChange={(e) => ((enterLater.current = false), setText(e.target.value), setActive(0))}
          placeholder="Search, or paste a URL or file path…"
          aria-label="Find something to link"
          role="combobox"
          aria-expanded={choices.length > 0}
          aria-controls={listId}
          aria-autocomplete="list"
          aria-activedescendant={choices[active] ? `${listId}-${active}` : undefined}
          className="h-8 w-full bg-transparent text-[13px] outline-none placeholder:text-subtle"
          onKeyDown={(e) => {
            if (e.key === 'ArrowDown') {
              e.preventDefault()
              setActive((a) => Math.min(a + 1, Math.max(choices.length - 1, 0)))
            } else if (e.key === 'ArrowUp') {
              e.preventDefault()
              setActive((a) => Math.max(a - 1, 0))
            } else if (e.key === 'Enter') {
              e.preventDefault()
              if (settled) pick(choices[active])
              else enterLater.current = true
            }
          }}
        />
      </div>
      <div className="flex items-center gap-2 border-b border-line px-2 py-1.5 text-xs text-subtle">
        This item
        <Select
          variant="inline"
          aria-label="Relation"
          value={rel}
          onChange={(e) => setRel(e.target.value as LinkRel)}
        >
          {PICKABLE.map((r) => (
            <option key={r} value={r}>
              {REL_LABEL[r].out.toLowerCase()}
            </option>
          ))}
        </Select>
        …
      </div>
      <div id={listId} role="listbox" aria-label="Matches" className="max-h-72 overflow-auto py-1">
        {choices.map((c, i) => {
          const done = linked(c.item)
          return (
            <button
              key={c.id}
              id={`${listId}-${i}`}
              role="option"
              aria-selected={i === active}
              aria-disabled={done || undefined}
              tabIndex={-1}
              disabled={done}
              onMouseEnter={() => setActive(i)}
              onClick={() => pick(c)}
              className={clsx(
                'flex h-9 w-full items-center gap-2.5 rounded-lg px-2.5 text-left text-[13px] disabled:opacity-50 sm:h-8',
                i === active && 'bg-hover',
              )}
            >
              <ItemIcon item={c.item} />
              <span className="min-w-0 flex-1 truncate">
                {c.id === 'key' ? `Link ${c.item.key}` : null}
                {c.id === 'file' ? `Link file ${t}` : null}
                {c.id === 'url' ? `Link ${t}` : null}
                {!['key', 'file', 'url'].includes(c.id) && (
                  <>
                    {c.item.key && <span className="text-subtle">{c.item.key} </span>}
                    {c.item.title}
                  </>
                )}
              </span>
              <span className="shrink-0 text-[11px] text-subtle">{done ? 'linked' : c.hint}</span>
            </button>
          )
        })}
        {choices.length === 0 && (
          <div className="px-3 py-4 text-center text-xs text-subtle">
            {!t
              ? 'Find an issue, design note, term or page, or paste a PR link or a file path like internal/core/claim.go:40.'
              : results.isFetching
                ? 'Searching…'
                : 'Nothing matches.'}
          </div>
        )}
      </div>
    </div>
  )
}
