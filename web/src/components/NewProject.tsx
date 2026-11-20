import { useEffect, useState } from 'react'
import { useNavigate } from '@tanstack/react-router'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import clsx from 'clsx'
import { Columns3, Repeat } from 'lucide-react'
import { api } from '@/lib/api'
import { Dialog } from '@/components/ui/dialog'
import { Button } from '@/components/ui/button'
import { Field, Input } from '@/components/ui/input'
import { toastError } from '@/components/ui/toast'
import { ProjectIcon } from '@/components/issue/icons'
import { EmojiPicker } from '@/components/EmojiPicker'
import { PROJECT_COLORS } from '@/lib/theme'
import type { Project } from '@/lib/types'

function suggestKey(name: string): string {
  const words = name.split(/[^A-Za-z0-9]+/).filter(Boolean)
  let k =
    words.length >= 2
      ? words
          .map((w) => w[0])
          .join('')
          .slice(0, 5)
      : (words[0] ?? '').slice(0, 4)
  k = k.toUpperCase().replace(/^[^A-Z]+/, '')
  return k.length >= 2 ? k : (k + 'PRJ').slice(0, 3)
}

/** Creating a project asks only for a name; everything else has defaults. */
export function NewProjectDialog({ open, onClose }: { open: boolean; onClose: () => void }) {
  const qc = useQueryClient()
  const navigate = useNavigate()
  const [name, setName] = useState('')
  const [key, setKey] = useState('')
  const [keyEdited, setKeyEdited] = useState(false)
  // Empty until picked: Waypoint (an AI model, when one is set up) chooses the
  // color, icon and background of a project that arrives without them.
  const [color, setColor] = useState('')
  const [icon, setIcon] = useState('')
  const [mode, setMode] = useState<'kanban' | 'sprints'>('kanban')
  useEffect(() => {
    if (!keyEdited) setKey(suggestKey(name))
  }, [name, keyEdited])
  useEffect(() => {
    if (open) {
      setName('')
      setKeyEdited(false)
      setIcon('')
      setColor('')
    }
  }, [open])
  const create = useMutation({
    mutationFn: () =>
      api.post<Project>('/projects', {
        name,
        key: keyEdited ? key : undefined,
        // Left out when not picked, so an AI model can style the project.
        color: color || undefined,
        icon: icon || undefined,
        settings: { mode },
      }),
    onSuccess: (p) => {
      qc.invalidateQueries({ queryKey: ['projects'] })
      qc.setQueryData(['project', p.key], p)
      onClose()
      navigate({ to: '/p/$key/board', params: { key: p.key } })
    },
    onError: toastError,
  })
  return (
    <Dialog
      open={open}
      onClose={onClose}
      title="New project"
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button
            variant="primary"
            disabled={!name.trim()}
            loading={create.isPending}
            onClick={() => create.mutate()}
          >
            Create project
          </Button>
        </>
      }
    >
      <form
        className="space-y-4 p-4"
        onSubmit={(e) => {
          e.preventDefault()
          if (name.trim()) create.mutate()
        }}
      >
        <div className="flex items-end gap-3">
          <ProjectIcon
            icon={icon}
            color={color || PROJECT_COLORS[0]}
            name={name || '?'}
            size={40}
          />
          <Field label="Name" className="flex-1">
            <Input
              autoFocus
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="Home renovation"
            />
          </Field>
        </div>
        <div className="grid grid-cols-[1fr_auto] gap-3">
          <Field label="Key" hint={`Issues will be ${key || 'KEY'}-1, ${key || 'KEY'}-2…`}>
            <Input
              value={key}
              onChange={(e) => {
                setKeyEdited(true)
                setKey(
                  e.target.value
                    .toUpperCase()
                    .replace(/[^A-Z0-9]/g, '')
                    .slice(0, 10),
                )
              }}
              className="font-mono uppercase"
            />
          </Field>
          <div className="space-y-1.5">
            <div className="text-[13px] font-medium leading-5">Icon</div>
            <EmojiPicker value={icon} onChange={setIcon} />
          </div>
        </div>
        <div>
          <div className="mb-1.5 text-[13px] font-medium">
            Color{' '}
            <span className="font-normal text-muted">· optional, we pick one if you don't</span>
          </div>
          <div className="flex flex-wrap gap-2">
            {PROJECT_COLORS.map((c) => (
              <button
                key={c}
                type="button"
                aria-label={c}
                onClick={() => setColor(c)}
                className={clsx(
                  'size-7 rounded-full transition-transform',
                  color === c && 'scale-110 ring-2 ring-fg ring-offset-2 ring-offset-raised',
                )}
                style={{ background: c }}
              />
            ))}
          </div>
        </div>
        <div>
          <div className="mb-1.5 text-[13px] font-medium">How will you work?</div>
          <div className="grid grid-cols-2 gap-2">
            {(
              [
                ['kanban', Columns3, 'Kanban', 'Continuous flow.'],
                ['sprints', Repeat, 'Sprints', 'Time-boxed, with velocity.'],
              ] as const
            ).map(([m, Icon, label, desc]) => (
              <button
                key={m}
                type="button"
                onClick={() => setMode(m)}
                className={clsx(
                  'rounded-xl border p-3 text-left transition-colors',
                  mode === m ? 'border-accent bg-accent-soft' : 'border-line hover:bg-hover',
                )}
              >
                <Icon className="mb-1.5 size-4 text-accent" />
                <div className="text-[13px] font-medium">{label}</div>
                <div className="text-xs text-muted">{desc}</div>
              </button>
            ))}
          </div>
          <p className="mt-2 text-xs text-subtle">You can switch any time.</p>
        </div>
      </form>
    </Dialog>
  )
}
