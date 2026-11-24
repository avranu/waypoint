// Only a person signed in to the Waypoint UI changes a project's sandbox: no token, agent or automation can.

import { useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { ShieldAlert } from 'lucide-react'
import { q } from '@/lib/queries'
import {
  levelLoosens,
  SANDBOX_LEVELS,
  saveProjectSandbox,
  type SandboxLevel,
  type SandboxPolicy,
} from '@/lib/sandbox'
import { relativeTime } from '@/lib/format'
import type { Project } from '@/lib/types'
import { Button } from '@/components/ui/button'
import { Field, Select, Textarea } from '@/components/ui/input'
import { LoadError, Section, Skeleton } from '@/components/ui/misc'
import { toast } from '@/components/ui/toast'
import { SandboxLog, sandboxError, useReauth } from '@/components/Sandbox'

export function SandboxTab({ project }: { project: Project }) {
  const policy = useQuery(q.projectSandbox(project.key))
  return (
    <div className="space-y-4">
      {policy.isPending ? (
        <Skeleton className="h-48" />
      ) : policy.data ? (
        <PolicyForm key={policy.data.updated_at ?? ''} policy={policy.data} />
      ) : policy.isError ? (
        <LoadError what="The sandbox policy" error={policy.error} />
      ) : null}
      <Section title="Changes">
        <SandboxLog
          filter={{ project: project.key }}
          canEdit={() => !!policy.data?.can_edit}
          empty="No changes yet: this project uses the defaults."
        />
      </Section>
    </div>
  )
}

function PolicyForm({ policy }: { policy: SandboxPolicy }) {
  const qc = useQueryClient()
  const { guard, dialog } = useReauth()
  const [level, setLevel] = useState<SandboxLevel>(policy.level)
  const [interactive, setInteractive] = useState<SandboxLevel>(policy.interactive_level)
  const [reason, setReason] = useState('')
  const [busy, setBusy] = useState(false)
  const changed = level !== policy.level || interactive !== policy.interactive_level
  const loosens =
    levelLoosens(policy.level, level) || levelLoosens(policy.interactive_level, interactive)
  const ro = !policy.can_edit
  const save = async () => {
    setBusy(true)
    try {
      const input = {
        ...(level !== policy.level && { level }),
        ...(interactive !== policy.interactive_level && { interactive_level: interactive }),
        reason: reason.trim(),
      }
      const r = await guard(() => saveProjectSandbox(policy.project, input))
      qc.setQueryData(q.projectSandbox(policy.project).queryKey, r)
      void qc.invalidateQueries({ queryKey: ['sandbox', 'log'] })
      setReason('')
      toast(loosens ? 'Sandbox loosened; admins are told' : 'Sandbox saved')
    } catch (e) {
      sandboxError(e)
    } finally {
      setBusy(false)
    }
  }
  return (
    <Section
      title="Sandbox"
      helpId="project:sandbox"
      help={
        <>
          How strongly agents working on this project are walled off from the rest of the machine.
          Only a project owner changes it, here in the browser: no token, agent or automation can.
          Loosening asks for your password and a reason, and every change is logged and can be
          reverted.
        </>
      }
    >
      <form
        className="space-y-4 px-4 py-3"
        onSubmit={(e) => {
          e.preventDefault()
          if (changed && (!loosens || reason.trim())) void save()
        }}
      >
        <div className="flex items-start gap-2 rounded-lg border border-line bg-sunken px-3 py-2 text-xs text-muted">
          <ShieldAlert className="mt-px size-3.5 shrink-0 text-subtle" />
          <span>
            Waypoint records these levels but doesn’t enforce them yet: agents still run as the
            runner’s person. Enforcement comes with the sandbox helper.
          </span>
        </div>
        <LevelField
          label="Runner jobs"
          unset="The machine’s default"
          value={level}
          onChange={setLevel}
          disabled={ro}
        />
        <LevelField
          label="Interactive sessions"
          unset="Same as runner jobs"
          value={interactive}
          onChange={setInteractive}
          disabled={ro}
        />
        {loosens && (
          <Field
            label="Why loosen it?"
            note="Kept in the sandbox log; admins and the project’s owners are told."
          >
            <Textarea
              rows={2}
              maxLength={500}
              value={reason}
              onChange={(e) => setReason(e.target.value)}
              placeholder="The build needs the Docker socket"
            />
          </Field>
        )}
        {ro ? (
          <p className="text-xs text-subtle">Only the project’s owners change this.</p>
        ) : (
          <div className="flex flex-wrap items-center gap-2">
            <Button
              type="submit"
              size="sm"
              variant={loosens ? 'danger' : 'primary'}
              disabled={!changed || (loosens && !reason.trim())}
              loading={busy}
            >
              {loosens ? 'Loosen sandbox' : 'Save'}
            </Button>
            {changed && (
              <Button
                type="button"
                size="sm"
                variant="ghost"
                onClick={() => {
                  setLevel(policy.level)
                  setInteractive(policy.interactive_level)
                  setReason('')
                }}
              >
                Undo
              </Button>
            )}
          </div>
        )}
        {policy.updated_by && (
          <p className="text-xs text-subtle">
            Last changed by @{policy.updated_by.username} {relativeTime(policy.updated_at)}
          </p>
        )}
      </form>
      {dialog}
    </Section>
  )
}

function LevelField({
  label,
  unset,
  value,
  onChange,
  disabled,
}: {
  label: string
  unset: string
  value: SandboxLevel
  onChange: (v: SandboxLevel) => void
  disabled: boolean
}) {
  const note = SANDBOX_LEVELS.find((l) => l.value === value)?.note
  return (
    <Field label={label} note={note ?? 'Unset: follows the default.'}>
      <Select
        value={value}
        disabled={disabled}
        onChange={(e) => onChange(e.target.value as SandboxLevel)}
      >
        <option value="">{unset}</option>
        {SANDBOX_LEVELS.map((l) => (
          <option key={l.value} value={l.value}>
            {l.label}
          </option>
        ))}
      </Select>
    </Field>
  )
}
