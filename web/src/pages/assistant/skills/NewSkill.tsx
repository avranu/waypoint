import { useId, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import clsx from 'clsx'
import type { Assistant } from '@/lib/assistant'
import { Field, Input } from '@/components/ui/input'
import { toast } from '@/components/ui/toast'
import { EditorActions, MarkdownEditor, releaseDirty, useDirty } from '../memory/editor'
import {
  BODY_TEMPLATE,
  categoryLabel,
  composeSkill,
  DESCRIPTION_LIMIT,
  NAME_RE,
  skillsQuery,
  useSkillMutations,
} from './data'

export function NewSkill({
  assistant,
  back,
  onCancel,
  onCreated,
}: {
  assistant: Assistant
  back: React.ReactNode
  onCancel: () => void
  onCreated: (name: string) => void
}) {
  const id = assistant.id
  const list = useQuery(skillsQuery(id)).data
  const m = useSkillMutations(id)
  const [name, setName] = useState('')
  const [category, setCategory] = useState('')
  const [description, setDescription] = useState('')
  const [body, setBody] = useState(BODY_TEMPLATE)
  const [tried, setTried] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const categories = [...new Set(list?.map((s) => s.category).filter(Boolean) as string[])].sort()
  const listId = useId()
  const dirty = !!(name || category || description || body !== BODY_TEMPLATE)
  useDirty('new-skill', dirty)

  const nameError = !name
    ? 'A name is needed.'
    : name.length > 64
      ? 'At most 64 characters.'
      : !NAME_RE.test(name)
        ? 'Lowercase letters, digits, and . _ - only; start with a letter or digit.'
        : list?.some((s) => s.name === name)
          ? 'There is already a skill with this name.'
          : null
  const categoryError =
    category && !NAME_RE.test(category) ? 'Lowercase letters, digits, and . _ - only.' : null
  const descLen = description.trim().length
  const descError = !descLen
    ? 'Say when to use it.'
    : descLen > DESCRIPTION_LIMIT
      ? `At most ${DESCRIPTION_LIMIT} characters.`
      : null
  const bodyError = !body.trim() ? 'Write the instructions.' : null
  const content = composeSkill({
    name,
    description,
    author: assistant.owner.display_name || assistant.owner.username,
    body,
  })

  const create = () => {
    setTried(true)
    setError(null)
    if (nameError || categoryError || descError || bodyError || m.create.isPending) return
    m.create.mutate(
      { name, category: category || undefined, content },
      {
        onSuccess: () => {
          toast(`Skill ${name} created`, { kind: 'success' })
          releaseDirty('new-skill')
          onCreated(name)
        },
        onError: (e) => setError(e.message),
      },
    )
  }

  return (
    <div className="space-y-4">
      {back}
      <h2 className="text-lg font-semibold tracking-tight">New skill</h2>
      <div className="space-y-4 rounded-xl border border-line bg-panel p-4">
        <div className="grid gap-4 sm:grid-cols-2">
          <Field label="Name" error={tried ? (nameError ?? undefined) : undefined}>
            <Input
              value={name}
              autoFocus
              autoCapitalize="none"
              autoCorrect="off"
              spellCheck={false}
              placeholder="weekly-review"
              className="font-mono"
              onChange={(e) => setName(e.target.value.toLowerCase().replace(/\s+/g, '-'))}
            />
          </Field>
          <Field label="Category" error={categoryError ?? undefined}>
            <Input
              value={category}
              list={listId}
              autoCapitalize="none"
              autoCorrect="off"
              spellCheck={false}
              placeholder="productivity"
              className="font-mono"
              onChange={(e) => setCategory(e.target.value.toLowerCase().replace(/\s+/g, '-'))}
            />
            <datalist id={listId}>
              {categories.map((c) => (
                <option key={c} value={c}>
                  {categoryLabel(c)}
                </option>
              ))}
            </datalist>
          </Field>
        </div>
        <Field
          label="When to use it"
          error={tried ? (descError ?? undefined) : undefined}
          note={
            <span className={clsx(descLen > DESCRIPTION_LIMIT && 'text-danger')}>
              {descLen} / {DESCRIPTION_LIMIT} · {assistant.name} picks skills by this line.
            </span>
          }
        >
          <Input
            value={description}
            placeholder="Review the week's issues every Friday."
            onChange={(e) => setDescription(e.target.value)}
          />
        </Field>
        <div className="space-y-1.5">
          <div className="text-[13px] font-medium">Instructions</div>
          <MarkdownEditor label="Instructions" value={body} onChange={setBody} onSave={create} />
          {tried && bodyError && <p className="text-xs text-danger">{bodyError}</p>}
        </div>
        <EditorActions
          onCancel={onCancel}
          onSave={create}
          saving={m.create.isPending}
          dirty
          error={error}
          saveLabel="Create skill"
        />
      </div>
    </div>
  )
}
