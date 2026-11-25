import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Archive, Code2, Eye, Pencil, Sparkles } from 'lucide-react'
import type { Assistant } from '@/lib/assistant'
import { plural } from '@/lib/format'
import { Button, IconButton } from '@/components/ui/button'
import { Confirm } from '@/components/ui/dialog'
import { Switch } from '@/components/ui/input'
import { Badge, Empty, Skeleton } from '@/components/ui/misc'
import { SegmentedControl } from '@/components/ui/segmented'
import { toast, toastError } from '@/components/ui/toast'
import { EditorActions, MarkdownEditor, SavedMark, useDirty, useSavedFlash } from '../memory/editor'
import { SkillDoc } from './SkillDoc'
import {
  categoryLabel,
  PROVENANCE,
  skillContentQuery,
  skillsQuery,
  splitFrontmatter,
  useSkillMutations,
} from './data'

export function SkillPage({
  assistant,
  name,
  back,
  onArchived,
}: {
  assistant: Assistant
  name: string
  back: React.ReactNode
  onArchived: () => void
}) {
  const id = assistant.id
  const list = useQuery(skillsQuery(id))
  const doc = useQuery(skillContentQuery(id, name))
  const m = useSkillMutations(id)
  const [view, setView] = useState<'preview' | 'raw'>('preview')
  const [editing, setEditing] = useState(false)
  const [draft, setDraft] = useState('')
  const [error, setError] = useState<string | null>(null)
  const [saved, flash] = useSavedFlash()
  const [archiving, setArchiving] = useState(false)
  const skill = list.data?.find((s) => s.name === name)
  const content = doc.data?.content ?? ''
  const isDirty = editing && draft !== content
  useDirty('skill', isDirty)
  const known = new Set(list.data?.map((s) => s.name))
  const fm = splitFrontmatter(content)
  const description = skill?.description || fm.description

  const save = () => {
    if (!isDirty || m.save.isPending) return
    setError(null)
    m.save.mutate(
      { name, content: draft },
      {
        onSuccess: () => {
          setEditing(false)
          flash()
        },
        onError: (e) => setError(e.message),
      },
    )
  }

  if (doc.isError)
    return (
      <div>
        {back}
        <Empty icon={<Sparkles />} title={`No skill named “${name}”`}>
          {doc.error.message}
        </Empty>
      </div>
    )
  const p = skill && PROVENANCE[skill.provenance]
  return (
    <div className="space-y-4">
      {back}
      <header className="flex items-start gap-3">
        <div className="min-w-0 flex-1 space-y-1">
          <h2 className="break-words text-lg font-semibold tracking-tight">{name}</h2>
          {description ? (
            <p className="text-[13px] text-muted">{description}</p>
          ) : (
            !doc.data && <Skeleton className="h-4 w-64 max-w-full" />
          )}
          {skill && p && (
            <div className="flex flex-wrap items-center gap-1.5 pt-1">
              <Badge>{categoryLabel(skill.category)}</Badge>
              <span title={p.hint}>
                <Badge color={p.color}>
                  {skill.provenance === 'agent' && <Sparkles className="size-3" />}
                  {p.label}
                </Badge>
              </span>
              {skill.usage > 0 && (
                <span className="text-xs text-subtle">used {plural(skill.usage, 'time')}</span>
              )}
            </div>
          )}
        </div>
        {skill && (
          <label className="flex h-11 shrink-0 cursor-pointer items-center gap-2 text-[13px] text-muted">
            <span className="hidden sm:inline">{skill.enabled ? 'On' : 'Off'}</span>
            <Switch
              label={`Let ${assistant.name} use ${name}`}
              checked={skill.enabled}
              onChange={(enabled) => m.toggle.mutate({ name, enabled }, { onError: toastError })}
            />
          </label>
        )}
      </header>

      <section className="rounded-xl border border-line bg-panel">
        <div className="flex items-center justify-between gap-2 border-b border-line px-3 py-2">
          {editing ? (
            <span className="px-1 text-[13px] font-semibold">Editing SKILL.md</span>
          ) : (
            <SegmentedControl
              label="Show"
              value={view}
              onChange={setView}
              textFrom="always"
              options={[
                { value: 'preview', label: 'Preview', icon: <Eye className="size-3" /> },
                { value: 'raw', label: 'SKILL.md', icon: <Code2 className="size-3" /> },
              ]}
            />
          )}
          <div className="flex items-center gap-2">
            <SavedMark show={saved} />
            {!editing && (
              <Button
                size="sm"
                icon={<Pencil className="size-3.5" />}
                disabled={!doc.data}
                onClick={() => {
                  setDraft(content)
                  setError(null)
                  setEditing(true)
                }}
              >
                Edit
              </Button>
            )}
            {!editing && skill?.provenance === 'agent' && (
              <IconButton
                label="Archive"
                icon={<Archive className="size-4" />}
                loading={m.archive.isPending}
                onClick={() => setArchiving(true)}
              />
            )}
          </div>
        </div>
        <div className="p-4">
          {doc.isPending ? (
            <div className="space-y-2">
              <Skeleton className="h-4 w-1/3" />
              <Skeleton className="h-4 w-full" />
              <Skeleton className="h-4 w-5/6" />
              <Skeleton className="h-4 w-2/3" />
            </div>
          ) : editing ? (
            <div className="space-y-3">
              <MarkdownEditor
                label={`${name} SKILL.md`}
                value={draft}
                onChange={setDraft}
                onSave={save}
                autoFocus
                rows={16}
              />
              <EditorActions
                onCancel={() => setEditing(false)}
                onSave={save}
                saving={m.save.isPending}
                dirty={isDirty}
                error={error}
              />
            </div>
          ) : view === 'raw' ? (
            <pre className="overflow-x-auto whitespace-pre-wrap break-words font-mono text-[12.5px] leading-relaxed text-fg">
              {content}
            </pre>
          ) : (
            <SkillDoc assistantId={id} content={content} known={known} />
          )}
        </div>
        {doc.data?.path && !editing && (
          <div className="truncate border-t border-line px-4 py-2 font-mono text-[11px] text-subtle">
            {doc.data.path}
          </div>
        )}
      </section>
      <Confirm
        open={archiving}
        onClose={() => setArchiving(false)}
        title={`Archive ${name}?`}
        body={`${assistant.name} stops seeing it. It stays in Hermes's archive, where \`hermes curator restore ${name}\` brings it back.`}
        confirmLabel="Archive"
        onConfirm={() =>
          m.archive.mutate(name, {
            onSuccess: () => {
              toast(`${name} archived`)
              onArchived()
            },
            onError: toastError,
          })
        }
      />
    </div>
  )
}
