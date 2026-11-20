// Hermes's built-in toolsets, on and off. A toolset in the assistant's
// agent.disabled_toolsets stays off whatever its switch says (and the
// assistant's deploy pins that list), so it shows as off and locked.

import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import clsx from 'clsx'
import { Wrench } from 'lucide-react'
import { enc } from '@/lib/api'
import { hermes, type Assistant } from '@/lib/assistant'
import { Switch } from '@/components/ui/input'
import { Badge, Section, Skeleton } from '@/components/ui/misc'
import { toast, toastError } from '@/components/ui/toast'
import { configQuery, toolsetsQuery, type Toolset } from './hermes-mcp'

export function ToolsetsSection({ assistant }: { assistant: Assistant }) {
  const id = assistant.id
  const qc = useQueryClient()
  const sets = useQuery(toolsetsQuery(id))
  const config = useQuery(configQuery(id))
  const off = new Set(config.data?.agent?.disabled_toolsets ?? [])
  const key = toolsetsQuery(id).queryKey
  const toggle = useMutation({
    mutationFn: ({ name, enabled }: { name: string; enabled: boolean }) =>
      hermes<{ post_setup_started?: string | null }>(id, 'PUT', `tools/toolsets/${enc(name)}`, {
        enabled,
      }),
    onMutate: ({ name, enabled }) => {
      qc.setQueryData(key, (d) =>
        d?.map((t) => (t.name === name ? { ...t, enabled, available: enabled } : t)),
      )
    },
    onSuccess: (r) => {
      if (r?.post_setup_started) toast('Installing what it needs in the background')
    },
    onError: toastError,
    onSettled: () => qc.invalidateQueries({ queryKey: key }),
  })
  const list = sets.data ?? []
  const on = list.filter((t) => t.enabled && !off.has(t.name)).length
  return (
    <Section
      title={
        <span className="flex items-center gap-2">
          <Wrench className="size-4 text-subtle" /> Toolsets
          {sets.data && (
            <span className="font-normal text-subtle">
              {on} of {list.length} on
            </span>
          )}
        </span>
      }
      helpId="assistant:toolsets"
      help={`${assistant.name}'s built-in tools. Changes apply to new conversations.`}
    >
      {sets.isPending ? (
        <div className="grid gap-px p-4 sm:grid-cols-2">
          {Array.from({ length: 6 }, (_, i) => (
            <Skeleton key={i} className="m-1 h-10" />
          ))}
        </div>
      ) : sets.isError ? (
        <p className="p-4 text-[13px] text-danger">
          Couldn't load the toolsets: {sets.error.message}
        </p>
      ) : (
        // Each row draws its bottom border; the last row's tucks under the panel's.
        <div className="overflow-hidden rounded-b-xl">
          <ul className="-mb-px grid sm:grid-cols-2">
            {list.map((t) => (
              <ToolsetRow
                key={t.name}
                t={t}
                locked={off.has(t.name)}
                onChange={(enabled) => toggle.mutate({ name: t.name, enabled })}
              />
            ))}
          </ul>
        </div>
      )}
    </Section>
  )
}

function ToolsetRow({
  t,
  locked,
  onChange,
}: {
  t: Toolset
  locked: boolean
  onChange: (on: boolean) => void
}) {
  const on = t.enabled && !locked
  return (
    <li className="flex items-start gap-3 border-b border-line px-4 py-2.5 sm:odd:border-r">
      <div className="min-w-0 flex-1">
        <div className="flex flex-wrap items-center gap-x-2 gap-y-0.5">
          <span className={clsx('text-[13px] font-medium', !on && 'text-muted')}>{t.label}</span>
          {locked && (
            <Badge>
              <span title="agent.disabled_toolsets in the assistant's config, pinned by its deploy">
                Off in config
              </span>
            </Badge>
          )}
          {!t.configured && !locked && <Badge color="var(--wp-warn)">Needs keys</Badge>}
        </div>
        <div
          className="mt-0.5 line-clamp-2 break-words font-mono text-[11px] leading-4 text-subtle"
          title={t.tools.join(', ')}
        >
          {t.tools.length ? t.tools.join(' · ') : t.description}
        </div>
      </div>
      <div className="flex h-9 shrink-0 items-center sm:h-5">
        <Switch checked={on} onChange={onChange} disabled={locked} label={t.label} />
      </div>
    </li>
  )
}
