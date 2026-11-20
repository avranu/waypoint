// Who chose the project's icon, color and background (WP-65, WP-108), with the
// two ways to change it: Pick another has Waypoint choose again (no AI), Reset
// to default clears it.

import { useState } from 'react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { RotateCcw, Shuffle } from 'lucide-react'
import { api, enc } from '@/lib/api'
import { invalidateProject } from '@/lib/queries'
import { Button } from '@/components/ui/button'
import { Dialog } from '@/components/ui/dialog'
import { Section } from '@/components/ui/misc'
import { toast, toastError } from '@/components/ui/toast'
import { ProjectIcon } from '@/components/issue/icons'
import type { Project } from '@/lib/types'

export function LookSection({
  project,
  owner,
  dirty,
}: {
  project: Project
  owner: boolean
  /** the form has unsaved edits, which a new look would replace */
  dirty: boolean
}) {
  const qc = useQueryClient()
  const [confirm, setConfirm] = useState(false)
  const done = (p: Project, message: string) => {
    qc.setQueryData(['project', p.key], p)
    invalidateProject(qc, p.key)
    qc.invalidateQueries({ queryKey: ['projects'] })
    toast(message, { kind: 'success' })
  }
  const regenerate = useMutation({
    mutationFn: () => api.post<Project>(`/projects/${enc(project.key)}/look/regenerate`),
    onSuccess: (p) => {
      setConfirm(false)
      done(p, 'Picked another look')
    },
    onError: (e) => {
      setConfirm(false)
      toastError(e)
    },
  })
  const reset = useMutation({
    mutationFn: () => api.post<Project>(`/projects/${enc(project.key)}/look/reset`),
    onSuccess: (p) => done(p, 'Back to the default look'),
    onError: toastError,
  })
  const busy = regenerate.isPending || reset.isPending
  const who = {
    auto: 'Picked automatically so this project stands apart from the others.',
    generated: 'Picked automatically so this project stands apart from the others.',
    custom: 'Chosen by you. Waypoint never replaces it unless you press Pick another.',
    plain: 'Default look. Waypoint leaves it alone until you press Pick another.',
  }[project.look]
  return (
    <Section title="Look">
      <div className="flex flex-wrap items-center gap-3 p-4">
        <ProjectIcon icon={project.icon} color={project.color} name={project.name} size={40} />
        <div className="min-w-0 flex-1 basis-48 text-[13px]">
          <div className="font-medium">Icon, color and background</div>
          <div data-testid="look-state" className="text-xs text-muted">
            {who}
          </div>
          {dirty && (
            <div className="text-xs text-warn">Save or discard your changes below first.</div>
          )}
        </div>
        {owner && (
          <div className="flex shrink-0 gap-2">
            <Button
              type="button"
              size="sm"
              icon={<Shuffle className="size-3.5" />}
              loading={regenerate.isPending && !confirm}
              disabled={busy || dirty}
              onClick={() => (project.look === 'custom' ? setConfirm(true) : regenerate.mutate())}
            >
              Pick another
            </Button>
            <Button
              type="button"
              size="sm"
              icon={<RotateCcw className="size-3.5" />}
              loading={reset.isPending}
              disabled={busy || dirty}
              onClick={() => reset.mutate()}
            >
              Reset to default
            </Button>
          </div>
        )}
      </div>
      <Dialog
        open={confirm}
        onClose={() => setConfirm(false)}
        title="Replace your look?"
        footer={
          <>
            <Button type="button" variant="ghost" onClick={() => setConfirm(false)}>
              Cancel
            </Button>
            <Button
              type="button"
              variant="primary"
              loading={regenerate.isPending}
              onClick={() => regenerate.mutate()}
            >
              Pick another
            </Button>
          </>
        }
      >
        <p className="p-4 text-[13px] text-muted">
          You chose this icon, color and background yourself. Waypoint picks new ones and they
          replace yours.
        </p>
      </Dialog>
    </Section>
  )
}
