import { useEffect, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { FileStack, Pencil, Plus, Trash2 } from 'lucide-react'
import { api, enc } from '@/lib/api'
import { templatesQuery, type IssueTemplate } from '@/lib/templates'
import { Button, IconButton } from '@/components/ui/button'
import { Confirm, Dialog } from '@/components/ui/dialog'
import { Field, Input, Textarea } from '@/components/ui/input'
import { MarkdownField } from '@/components/editor/MarkdownField'
import { Badge, Empty, LoadError } from '@/components/ui/misc'
import { toastError } from '@/components/ui/toast'
import { LabelPicker, PointsPicker, TypePicker } from '@/components/issue/pickers'
import { TypeIcon } from '@/components/issue/icons'
import type { IssueType, Project } from '@/lib/types'
import { List, ReadOnlyNote, TabIntro, canEdit } from './shared'

export function TemplatesTab({ project }: { project: Project }) {
  const editable = canEdit(project)
  const qc = useQueryClient()
  const templates = useQuery(templatesQuery(project.key))
  const [editing, setEditing] = useState<IssueTemplate | 'new' | null>(null)
  const [deleting, setDeleting] = useState<IssueTemplate | null>(null)
  const del = useMutation({
    mutationFn: (id: number) => api.del(`/projects/${enc(project.key)}/templates/${id}`),
    onSettled: () => qc.invalidateQueries({ queryKey: ['templates', project.key] }),
    onError: toastError,
  })
  const list = templates.data ?? []
  return (
    <div className="space-y-5">
      <TabIntro
        title="Issue templates"
        actions={
          editable && (
            <Button
              size="sm"
              variant="primary"
              icon={<Plus className="size-3.5" />}
              onClick={() => setEditing('new')}
            >
              New template
            </Button>
          )
        }
      >
        Starting points for issues you make often, like a bug report with its checklist and
        sub-tasks. Pick one when creating an issue, or have a scheduled automation make it.
      </TabIntro>
      {!editable && <ReadOnlyNote>Only owners and editors can change templates.</ReadOnlyNote>}
      {templates.isError ? (
        <LoadError what="Templates" error={templates.error} />
      ) : list.length === 0 ? (
        !templates.isPending && <Empty icon={<FileStack />} title="No templates yet" />
      ) : (
        <List>
          {list.map((t) => (
            <li key={t.id} className="flex items-center gap-3 px-4 py-3">
              <TypeIcon type={(t.type || 'task') as IssueType} />
              <div className="min-w-0 flex-1">
                <div className="truncate text-[13px] font-medium">{t.name}</div>
                <div className="truncate text-xs text-subtle">
                  {[
                    t.title && `“${t.title}”`,
                    t.subtasks.length && `${t.subtasks.length} sub-tasks`,
                    t.points != null && `${t.points} pts`,
                  ]
                    .filter(Boolean)
                    .join(' · ') || 'Empty template'}
                </div>
              </div>
              {t.labels.slice(0, 3).map((l) => (
                <Badge key={l} className="max-sm:hidden">
                  {l}
                </Badge>
              ))}
              {editable && (
                <>
                  <IconButton
                    label={`Edit ${t.name}`}
                    onClick={() => setEditing(t)}
                    icon={<Pencil className="size-3.5" />}
                  />
                  <IconButton
                    label={`Delete ${t.name}`}
                    onClick={() => setDeleting(t)}
                    icon={<Trash2 className="size-3.5" />}
                  />
                </>
              )}
            </li>
          ))}
        </List>
      )}
      <TemplateDialog project={project} template={editing} onClose={() => setEditing(null)} />
      <Confirm
        open={!!deleting}
        onClose={() => setDeleting(null)}
        title={`Delete the ${deleting?.name} template?`}
        body="Issues already created from it are unchanged. Automations that use it will fail until you pick another."
        confirmLabel="Delete"
        onConfirm={() => deleting && del.mutate(deleting.id)}
      />
    </div>
  )
}

function TemplateDialog({
  project,
  template,
  onClose,
}: {
  project: Project
  template: IssueTemplate | 'new' | null
  onClose: () => void
}) {
  const qc = useQueryClient()
  const existing = template && template !== 'new' ? template : null
  const [name, setName] = useState('')
  const [type, setType] = useState<IssueType>('task')
  const [title, setTitle] = useState('')
  const [description, setDescription] = useState('')
  const [labels, setLabels] = useState<string[]>([])
  const [points, setPoints] = useState<number | null>(null)
  const [subtasks, setSubtasks] = useState('')
  useEffect(() => {
    if (!template) return
    setName(existing?.name ?? '')
    setType(((existing?.type || 'task') as IssueType) ?? 'task')
    setTitle(existing?.title ?? '')
    setDescription(existing?.description ?? '')
    setLabels(existing?.labels ?? [])
    setPoints(existing?.points ?? null)
    setSubtasks((existing?.subtasks ?? []).join('\n'))
  }, [template]) // eslint-disable-line react-hooks/exhaustive-deps
  const save = useMutation({
    mutationFn: () => {
      const body = {
        name: name.trim(),
        type,
        title: title.trim(),
        description,
        labels,
        points,
        subtasks: subtasks
          .split('\n')
          .map((s) => s.trim())
          .filter(Boolean),
      }
      return existing
        ? api.patch(`/projects/${enc(project.key)}/templates/${existing.id}`, body)
        : api.post(`/projects/${enc(project.key)}/templates`, body)
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['templates', project.key] })
      onClose()
    },
    onError: toastError,
  })
  const known = project.labels ?? []
  return (
    <Dialog
      open={!!template}
      onClose={onClose}
      title={existing ? `Edit ${existing.name}` : 'New issue template'}
      wide
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button
            variant="primary"
            disabled={!name.trim()}
            loading={save.isPending}
            onClick={() => save.mutate()}
          >
            Save template
          </Button>
        </>
      }
    >
      <form
        className="space-y-4 p-4"
        onSubmit={(e) => {
          e.preventDefault()
          if (name.trim()) save.mutate()
        }}
      >
        <Field label="Template name">
          <Input
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder="Bug report"
            autoFocus
          />
        </Field>
        <div className="flex flex-wrap items-center gap-2">
          <TypePicker value={type} onChange={setType} />
          <PointsPicker value={points} onChange={setPoints} />
          <LabelPicker
            labels={known}
            value={labels.map(
              (n, i) =>
                known.find((l) => l.name === n) ?? { id: -1 - i, name: n, color: '#94a3b8' },
            )}
            onChange={setLabels}
          />
        </div>
        <Field label="Title" hint="Prefilled; people can change it.">
          <Input value={title} onChange={(e) => setTitle(e.target.value)} placeholder="Bug: " />
        </Field>
        <Field label="Description" hint="Markdown; checklists (- [ ] step) work well.">
          <MarkdownField
            value={description}
            onValueChange={setDescription}
            rows={6}
            ariaLabel="Description"
            placeholder={'## Steps to reproduce\n\n## Expected\n\n## Actual'}
          />
        </Field>
        <Field label="Sub-tasks" hint="One per line.">
          <Textarea
            value={subtasks}
            onChange={(e) => setSubtasks(e.target.value)}
            rows={4}
            placeholder={'Reproduce\nFix\nAdd a test'}
          />
        </Field>
      </form>
    </Dialog>
  )
}
