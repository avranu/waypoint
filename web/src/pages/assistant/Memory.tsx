// What the assistant remembers and who it is (WP-ADR1). Hermes keeps two
// small files it reads at the start of every conversation: its notes
// (MEMORY.md) and what it knows about you (USER.md), each a list of entries
// under a size limit. Each entry can be edited or deleted on its own, the
// whole file edited as text, or the file cleared. Its persona is the
// profile's SOUL.md.

import { useState, type ReactNode } from 'react'
import { useQuery } from '@tanstack/react-query'
import clsx from 'clsx'
import {
  Brain,
  Eraser,
  FileText,
  Pencil,
  Plus,
  Trash2,
  UserRound,
  VenetianMask,
} from 'lucide-react'
import { iso, type Assistant } from '@/lib/assistant'
import { relativeTime } from '@/lib/format'
import { Button, IconButton } from '@/components/ui/button'
import { Confirm } from '@/components/ui/dialog'
import { Badge, Progress, Section, Skeleton } from '@/components/ui/misc'
import { toast, toastError } from '@/components/ui/toast'
import { Markdown } from './Transcript'
import {
  configQuery,
  currentProfileQuery,
  ENTRY_DELIMITER,
  entryIds,
  fileUpdated,
  graphQuery,
  homeQuery,
  MEMORY_FILES,
  memoryChars,
  memoryPath,
  memoryStatusQuery,
  parseEntries,
  soulQuery,
  textFileQuery,
  useMemoryMutations,
  type LearningGraph,
  type MemoryTarget,
} from './memory/data'
import {
  EditorActions,
  LeaveGuard,
  MarkdownEditor,
  SavedMark,
  useDirty,
  useSavedFlash,
} from './memory/editor'

export function MemoryTab({ assistant }: { assistant: Assistant }) {
  const id = assistant.id
  const home = useQuery(homeQuery(id))
  const profile = useQuery(currentProfileQuery(id)).data ?? 'default'
  const config = useQuery(configQuery(id)).data?.memory
  const graph = useQuery(graphQuery(id)).data
  return (
    <div className="space-y-4">
      <LeaveGuard />
      <ProviderStrip assistant={assistant} />
      {home.isError ? (
        <ErrorPanel>Couldn't find where Hermes keeps its memory: {home.error.message}</ErrorPanel>
      ) : (
        <>
          <MemoryFile
            assistant={assistant}
            target="memory"
            title="Notes"
            icon={<Brain className="size-4 text-subtle" />}
            empty={`${assistant.name} writes down what it learns here.`}
            home={home.data}
            graph={graph}
            profile={profile}
            limit={config?.memory_char_limit ?? 2200}
            off={config?.memory_enabled === false}
          />
          <MemoryFile
            assistant={assistant}
            target="user"
            title="About you"
            icon={<UserRound className="size-4 text-subtle" />}
            empty={`${assistant.name} keeps what it learns about you here.`}
            home={home.data}
            graph={graph}
            profile={profile}
            limit={config?.user_char_limit ?? 1375}
            off={config?.user_profile_enabled === false}
          />
        </>
      )}
      <Persona assistant={assistant} profile={profile} />
    </div>
  )
}

function ErrorPanel({ children }: { children: ReactNode }) {
  return (
    <p role="alert" className="px-4 py-3 text-[13px] text-danger">
      {children}
    </p>
  )
}

function ProviderStrip({ assistant }: { assistant: Assistant }) {
  const status = useQuery(memoryStatusQuery(assistant.id))
  const active = status.data?.providers.find((p) => p.name === status.data?.active)
  return (
    <div className="flex flex-wrap items-center gap-x-3 gap-y-1 rounded-xl border border-line bg-panel px-4 py-2.5 text-[13px]">
      <span className="flex min-w-0 items-center gap-2">
        <Brain className="size-4 shrink-0 text-accent" />
        {status.isPending ? (
          <Skeleton className="h-4 w-32" />
        ) : status.isError ? (
          <span className="text-danger">Memory status unavailable: {status.error.message}</span>
        ) : (
          <>
            <span className="font-medium">Built-in memory</span>
            {status.data.active && (
              <>
                <span className="text-subtle">+</span>
                <span className="font-medium">{status.data.active}</span>
                {active && active.status !== 'ready' && (
                  <Badge color="var(--wp-warn)">{active.status.replace('_', ' ')}</Badge>
                )}
              </>
            )}
          </>
        )}
      </span>
      <span className="text-xs text-subtle sm:ml-auto">
        {assistant.name} reads changes from its next conversation.
      </span>
    </div>
  )
}

type Edit =
  { kind: 'file' } | { kind: 'add' } | { kind: 'entry'; index: number; node: string | null }

function MemoryFile({
  assistant,
  target,
  title,
  icon,
  empty,
  home,
  graph,
  profile,
  limit,
  off,
}: {
  assistant: Assistant
  target: MemoryTarget
  title: string
  icon: ReactNode
  empty: string
  home: string | undefined
  graph: LearningGraph | undefined
  profile: string
  limit: number
  off: boolean
}) {
  const id = assistant.id
  const path = home ? memoryPath(home, target) : ''
  const file = useQuery({ ...textFileQuery(id, path), enabled: !!home })
  const m = useMemoryMutations(id)
  const [edit, setEdit] = useState<Edit | null>(null)
  const [draft, setDraft] = useState('')
  const [base, setBase] = useState('')
  const [error, setError] = useState<string | null>(null)
  const [deleting, setDeleting] = useState<{ node: string; text: string } | null>(null)
  const [clearing, setClearing] = useState(false)
  const [conflict, setConflict] = useState(false)
  const [saved, flash] = useSavedFlash()
  const dirtyKey = `memory:${target}`
  const isDirty = edit !== null && draft !== base
  useDirty(dirtyKey, isDirty)

  const raw = file.data?.text ?? ''
  const entries = parseEntries(raw)
  const ids = entryIds(graph, target, entries)
  const draftEntries = edit?.kind === 'file' ? parseEntries(draft) : null
  const count = memoryChars(
    draftEntries ??
      (edit?.kind === 'add' && draft.trim()
        ? [...entries, draft.trim()]
        : edit?.kind === 'entry'
          ? entries.map((e, i) => (i === edit.index ? draft.trim() : e))
          : entries),
  )
  const updated = fileUpdated(graph, target)
  const saving = m.write.isPending || m.editEntry.isPending

  const start = (e: Edit, text: string) => {
    setEdit(e)
    setDraft(text)
    setBase(text)
    setError(null)
  }
  const done = () => {
    setEdit(null)
    setError(null)
    flash()
  }
  const fail = (e: unknown) => setError(e instanceof Error ? e.message : String(e))

  const writeFile = (content: string) =>
    m.write.mutate({ path, content }, { onSuccess: done, onError: fail })

  const save = async () => {
    if (!edit || saving || !isDirty) return
    setError(null)
    if (edit.kind === 'entry') {
      if (!draft.trim()) return setError('An empty note can’t be saved. Delete it instead.')
      if (!edit.node)
        return setError('This note changed since it was loaded. Cancel and try again.')
      m.editEntry.mutate(
        { id: edit.node, content: draft.trim() },
        { onSuccess: done, onError: fail },
      )
      return
    }
    try {
      const now = await m.fresh(path)
      if (edit.kind === 'add') {
        writeFile([...parseEntries(now), draft.trim()].join(ENTRY_DELIMITER))
      } else if (now !== base) {
        setConflict(true)
      } else {
        writeFile(draft)
      }
    } catch (e) {
      fail(e)
    }
  }

  const label = MEMORY_FILES[target].file
  const over = count > limit
  return (
    <Section
      title={
        <span className="flex items-center gap-2">
          {icon}
          {title}
          <span className="hidden font-mono text-[11px] font-normal text-subtle sm:inline">
            {label}
          </span>
          {off && (
            <span
              title={`${assistant.name} doesn't read or write this while it's off in its settings`}
            >
              <Badge>Off</Badge>
            </span>
          )}
        </span>
      }
      actions={
        <div className="flex items-center gap-1">
          <SavedMark show={saved} />
          {!edit && (
            <>
              <Button
                size="sm"
                variant="ghost"
                icon={<Plus className="size-3.5" />}
                disabled={!file.data}
                onClick={() => start({ kind: 'add' }, '')}
              >
                Add
              </Button>
              <IconButton
                label={`Edit ${label} as text`}
                icon={<FileText className="size-4" />}
                disabled={!file.data}
                onClick={() => start({ kind: 'file' }, raw)}
              />
              <IconButton
                label={`Clear ${title.toLowerCase()}`}
                icon={<Eraser className="size-4" />}
                disabled={!entries.length}
                onClick={() => setClearing(true)}
              />
            </>
          )}
        </div>
      }
    >
      <div className="flex items-center gap-3 border-b border-line px-4 py-2">
        <Progress
          value={count}
          max={limit}
          className="flex-1"
          color={
            over ? 'var(--wp-danger)' : count > limit * 0.9 ? 'var(--wp-warn)' : 'var(--wp-accent)'
          }
        />
        <span
          className={clsx('shrink-0 text-xs tabular-nums', over ? 'text-danger' : 'text-muted')}
          title="Characters, of the limit Hermes keeps it under"
        >
          {count.toLocaleString()} / {limit.toLocaleString()}
        </span>
        {updated && !edit && (
          <span className="hidden shrink-0 text-xs text-subtle sm:inline">
            {relativeTime(iso(updated))}
          </span>
        )}
      </div>
      {over && (
        <p className="border-b border-line px-4 py-2 text-xs text-danger">
          Over the limit: {assistant.name} can't add to it until it's shorter.
        </p>
      )}
      {file.isPending ? (
        <div className="space-y-2 p-4">
          <Skeleton className="h-4 w-3/4" />
          <Skeleton className="h-4 w-1/2" />
        </div>
      ) : file.isError ? (
        <ErrorPanel>
          Couldn't read {label}: {file.error.message}
        </ErrorPanel>
      ) : edit?.kind === 'file' ? (
        <div className="space-y-3 p-4">
          <MarkdownEditor
            label={label}
            value={draft}
            onChange={setDraft}
            onSave={save}
            autoFocus
            hint="A line with only § separates entries."
            preview={(t) => <EntryList entries={parseEntries(t)} />}
          />
          <EditorActions
            onCancel={() => setEdit(null)}
            onSave={save}
            saving={saving}
            dirty={isDirty}
            error={error}
          />
        </div>
      ) : (
        <>
          {entries.length === 0 && edit?.kind !== 'add' ? (
            <p className="px-4 py-6 text-center text-[13px] text-muted">{empty}</p>
          ) : (
            <ul className="divide-y divide-line">
              {entries.map((e, i) =>
                edit?.kind === 'entry' && edit.index === i ? (
                  <li key={i} className="space-y-3 px-4 py-3">
                    <MarkdownEditor
                      label={`${title}, entry ${i + 1}`}
                      value={draft}
                      onChange={setDraft}
                      onSave={save}
                      autoFocus
                      minHeight="min-h-[6rem]"
                    />
                    <EditorActions
                      onCancel={() => setEdit(null)}
                      onSave={save}
                      saving={saving}
                      dirty={isDirty}
                      error={error}
                    />
                  </li>
                ) : (
                  <li key={i} className="group flex items-start gap-2 py-2.5 pl-4 pr-2">
                    <Markdown text={e} className="min-w-0 flex-1 !text-[13px]" />
                    {!edit && (
                      <div className="-my-1 flex shrink-0 items-center opacity-100 transition-opacity sm:opacity-0 sm:focus-within:opacity-100 sm:group-hover:opacity-100">
                        <IconButton
                          label="Edit entry"
                          icon={<Pencil className="size-3.5" />}
                          onClick={() => start({ kind: 'entry', index: i, node: ids[i] }, e)}
                        />
                        <IconButton
                          label="Delete entry"
                          icon={<Trash2 className="size-3.5" />}
                          disabled={!ids[i]}
                          className="hover:text-danger"
                          onClick={() => ids[i] && setDeleting({ node: ids[i]!, text: e })}
                        />
                      </div>
                    )}
                  </li>
                ),
              )}
            </ul>
          )}
          {edit?.kind === 'add' && (
            <div
              className={clsx('space-y-3 px-4 py-3', entries.length > 0 && 'border-t border-line')}
            >
              <MarkdownEditor
                label={`New ${title.toLowerCase()} entry`}
                value={draft}
                onChange={setDraft}
                onSave={save}
                autoFocus
                minHeight="min-h-[6rem]"
                placeholder={
                  target === 'user' ? 'Something about you to remember…' : 'Something to remember…'
                }
              />
              <EditorActions
                onCancel={() => setEdit(null)}
                onSave={save}
                saving={saving}
                dirty={!!draft.trim()}
                error={error}
                saveLabel="Add"
              />
            </div>
          )}
        </>
      )}
      <Confirm
        open={!!deleting}
        onClose={() => setDeleting(null)}
        title="Delete this entry?"
        body={<span className="line-clamp-4 whitespace-pre-wrap">{deleting?.text}</span>}
        onConfirm={() =>
          deleting &&
          m.deleteEntry.mutate(deleting.node, {
            onSuccess: () => toast('Entry deleted'),
            onError: toastError,
          })
        }
      />
      <Confirm
        open={clearing}
        onClose={() => setClearing(false)}
        title={`Clear ${title.toLowerCase()}?`}
        body={`This deletes ${label} and everything ${assistant.name} has kept in it. It can't be undone.`}
        confirmLabel="Clear"
        onConfirm={() =>
          m.reset.mutate(
            { target, profile },
            { onSuccess: () => toast(`${title} cleared`), onError: toastError },
          )
        }
      />
      <Confirm
        open={conflict}
        onClose={() => setConflict(false)}
        title={`${label} changed while you were editing`}
        body={`${assistant.name} (or someone) saved it after you opened it. Saving now replaces their change with yours.`}
        confirmLabel="Save mine"
        onConfirm={() => writeFile(draft)}
      />
    </Section>
  )
}

/** Entries as they read: one block each, a rule between. */
function EntryList({ entries }: { entries: string[] }) {
  return (
    <ul className="divide-y divide-line">
      {entries.map((e, i) => (
        <li key={i} className="py-2 first:pt-0 last:pb-0">
          <Markdown text={e} className="!text-[13px]" />
        </li>
      ))}
    </ul>
  )
}

function Persona({ assistant, profile }: { assistant: Assistant; profile: string }) {
  const id = assistant.id
  const soul = useQuery(soulQuery(id, profile))
  const m = useMemoryMutations(id)
  const [editing, setEditing] = useState(false)
  const [draft, setDraft] = useState('')
  const [error, setError] = useState<string | null>(null)
  const [all, setAll] = useState(false)
  const [saved, flash] = useSavedFlash()
  const content = soul.data?.content ?? ''
  const isDirty = editing && draft !== content
  useDirty('memory:soul', isDirty)
  const long = content.length > 900 || content.split('\n').length > 14
  const save = () => {
    if (!isDirty || m.saveSoul.isPending) return
    setError(null)
    m.saveSoul.mutate(
      { profile, content: draft },
      {
        onSuccess: () => {
          setEditing(false)
          flash()
        },
        onError: (e) => setError(e.message),
      },
    )
  }
  return (
    <Section
      title={
        <span className="flex items-center gap-2">
          <VenetianMask className="size-4 text-subtle" />
          Persona
          <span className="hidden font-mono text-[11px] font-normal text-subtle sm:inline">
            SOUL.md
          </span>
        </span>
      }
      actions={
        <div className="flex items-center gap-1">
          <SavedMark show={saved} />
          {!editing && soul.data && (
            <Button
              size="sm"
              variant="ghost"
              icon={<Pencil className="size-3.5" />}
              onClick={() => {
                setDraft(content)
                setError(null)
                setEditing(true)
              }}
            >
              Edit
            </Button>
          )}
        </div>
      }
    >
      {soul.isPending ? (
        <div className="space-y-2 p-4">
          <Skeleton className="h-4 w-full" />
          <Skeleton className="h-4 w-5/6" />
          <Skeleton className="h-4 w-2/3" />
        </div>
      ) : soul.isError ? (
        <ErrorPanel>Couldn't read the persona: {soul.error.message}</ErrorPanel>
      ) : editing ? (
        <div className="space-y-3 p-4">
          <MarkdownEditor
            label="Persona"
            value={draft}
            onChange={setDraft}
            onSave={save}
            autoFocus
            placeholder={`Who ${assistant.name} is and how it talks…`}
          />
          <EditorActions
            onCancel={() => setEditing(false)}
            onSave={save}
            saving={m.saveSoul.isPending}
            dirty={isDirty}
            error={error}
          />
        </div>
      ) : !content.trim() ? (
        <p className="px-4 py-6 text-center text-[13px] text-muted">
          No persona: {assistant.name} uses Hermes's default.
        </p>
      ) : (
        <div className="px-4 py-3">
          <div className={clsx('relative', long && !all && 'max-h-64 overflow-hidden')}>
            <Markdown text={content} className="!text-[13px]" />
            {long && !all && (
              <div className="pointer-events-none absolute inset-x-0 bottom-0 h-16 bg-gradient-to-t from-panel" />
            )}
          </div>
          {long && (
            <button
              type="button"
              onClick={() => setAll(!all)}
              className="mt-1 h-8 text-[13px] font-medium text-accent hover:underline"
            >
              {all ? 'Show less' : 'Show all'}
            </button>
          )}
        </div>
      )}
    </Section>
  )
}
