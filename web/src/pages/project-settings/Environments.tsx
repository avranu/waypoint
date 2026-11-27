import { useState } from 'react'
import { Link } from '@tanstack/react-router'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { api, enc } from '@/lib/api'
import { invalidateProject } from '@/lib/queries'
import { Button } from '@/components/ui/button'
import { Field, Input } from '@/components/ui/input'
import { Section } from '@/components/ui/misc'
import { toast, toastError } from '@/components/ui/toast'
import type { Project } from '@/lib/types'
import { ReadOnlyNote, TabIntro, isOwner } from './shared'

export function EnvironmentsTab({ project }: { project: Project }) {
  const qc = useQueryClient()
  const saved = project.settings.dev_env_url ?? ''
  const [pattern, setPattern] = useState(saved)
  const trimmed = pattern.trim()
  const bad =
    trimmed !== '' && (!trimmed.includes('{name}') || !/^https?:\/\/[^\s/]+/i.test(trimmed))
  const save = useMutation({
    mutationFn: () =>
      api.patch<Project>(`/projects/${enc(project.key)}`, { settings: { dev_env_url: trimmed } }),
    onSuccess: (p) => {
      qc.setQueryData(['project', p.key], p)
      invalidateProject(qc, project.key)
      toast('Settings saved', { kind: 'success' })
    },
    onError: toastError,
  })
  const owner = isOwner(project)
  return (
    <div>
      <TabIntro title="Environments">
        Where the project’s code runs is listed on its{' '}
        <Link
          to="/p/$key/environments"
          params={{ key: project.key }}
          className="text-accent hover:underline"
        >
          Environments tab
        </Link>
        . Dev environments come and go: a cluster or CI registers one with{' '}
        <code className="font-mono text-xs">wp env up NAME</code> and removes it with{' '}
        <code className="font-mono text-xs">wp env down NAME</code>.
      </TabIntro>
      {!owner && <ReadOnlyNote />}
      <Section title="Dev environment address">
        <form
          className="space-y-3 p-4"
          onSubmit={(e) => {
            e.preventDefault()
            if (owner && !bad && trimmed !== saved) save.mutate()
          }}
        >
          <Field
            label="Address pattern"
            error={bad ? 'Start with https:// and include {name}' : undefined}
            note={
              <>
                {'{name}'} stands for the environment’s name. With{' '}
                <code className="font-mono">https://{'{name}'}.dev.example.com</code>,{' '}
                <code className="font-mono">wp env up a23x42s</code> checks{' '}
                <code className="font-mono">https://a23x42s.dev.example.com</code>.
              </>
            }
          >
            <Input
              type="url"
              inputMode="url"
              value={pattern}
              disabled={!owner}
              onChange={(e) => setPattern(e.target.value)}
              placeholder="https://{name}.dev.example.com"
              autoCapitalize="off"
            />
          </Field>
          {owner && (
            <div className="flex justify-end">
              <Button
                type="submit"
                variant="primary"
                loading={save.isPending}
                disabled={bad || trimmed === saved}
              >
                Save
              </Button>
            </div>
          )}
        </form>
      </Section>
    </div>
  )
}
