import { useState } from 'react'
import { useMutation } from '@tanstack/react-query'
import { Plus, Tag, Trash2 } from 'lucide-react'
import { api, enc } from '@/lib/api'
import { Button, IconButton } from '@/components/ui/button'
import { Confirm } from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Empty } from '@/components/ui/misc'
import { toastError } from '@/components/ui/toast'
import type { Label, Project } from '@/lib/types'
import {
  ColorPicker,
  InlineText,
  List,
  ReadOnlyNote,
  TabIntro,
  canEdit,
  randomSwatch,
  useRefreshProject,
} from './shared'

export function LabelsTab({ project }: { project: Project }) {
  const editable = canEdit(project)
  const refresh = useRefreshProject(project.key)
  const labels = [...(project.labels ?? [])].sort((a, b) => a.name.localeCompare(b.name))
  const [deleting, setDeleting] = useState<Label | null>(null)
  const [adding, setAdding] = useState(false)
  const [name, setName] = useState('')
  const [color, setColor] = useState(randomSwatch)

  const patch = useMutation({
    mutationFn: ({ id, body }: { id: number; body: { name?: string; color?: string } }) =>
      api.patch(`/labels/${id}`, body),
    onError: toastError,
    onSettled: refresh,
  })
  const del = useMutation({
    mutationFn: (id: number) => api.del(`/labels/${id}`),
    onError: toastError,
    onSettled: refresh,
  })
  const add = useMutation({
    mutationFn: () =>
      api.post<Label>(`/projects/${enc(project.key)}/labels`, { name: name.trim(), color }),
    onSuccess: () => {
      setName('')
      setColor(randomSwatch())
    },
    onError: toastError,
    onSettled: refresh,
  })

  return (
    <div className="space-y-5">
      <TabIntro title="Tags">
        For grouping and filtering, like “frontend” or “waiting”. Search with <b>label:name</b>.
      </TabIntro>
      {!editable && <ReadOnlyNote>Only owners and editors can change tags.</ReadOnlyNote>}

      {labels.length === 0 ? (
        <div className="rounded-xl border border-dashed border-line">
          <Empty icon={<Tag />} title="No tags yet">
            Typing a new tag on an issue creates it too.
          </Empty>
        </div>
      ) : (
        <List>
          {labels.map((l) => (
            <div key={l.id} className="flex items-center gap-2 px-3 py-1.5">
              <ColorPicker
                value={l.color}
                onChange={(c) => patch.mutate({ id: l.id, body: { color: c } })}
                disabled={!editable}
                label={`${l.name} color`}
                size={16}
              />
              <InlineText
                value={l.name}
                onSave={(n) => patch.mutate({ id: l.id, body: { name: n } })}
                disabled={!editable}
                ariaLabel="Label name"
                className="min-w-0 flex-1"
              />
              {editable && (
                <IconButton
                  label={`Delete ${l.name}`}
                  onClick={() => setDeleting(l)}
                  icon={<Trash2 className="size-3.5" />}
                />
              )}
            </div>
          ))}
        </List>
      )}

      {editable && adding && (
        <form
          className="flex items-center gap-2"
          onKeyDown={(e) => e.key === 'Escape' && !e.defaultPrevented && setAdding(false)}
          onSubmit={(e) => {
            e.preventDefault()
            if (name.trim()) add.mutate()
          }}
        >
          <ColorPicker value={color} onChange={setColor} size={24} label="New label color" />
          <Input
            value={name}
            onChange={(e) => setName(e.target.value)}
            autoFocus
            placeholder="Tag name"
            aria-label="New tag name"
            className="min-w-0 flex-1"
          />
          <Button type="submit" variant="primary" disabled={!name.trim()} loading={add.isPending}>
            Add
          </Button>
          <Button type="button" variant="ghost" onClick={() => setAdding(false)}>
            Cancel
          </Button>
        </form>
      )}

      {editable && !adding && (
        <Button variant="ghost" icon={<Plus className="size-4" />} onClick={() => setAdding(true)}>
          Add tag
        </Button>
      )}

      <Confirm
        open={!!deleting}
        onClose={() => setDeleting(null)}
        onConfirm={() => deleting && del.mutate(deleting.id)}
        title={`Delete tag “${deleting?.name}”?`}
        body="It's removed from every issue that has it. The issues themselves stay."
      />
    </div>
  )
}
