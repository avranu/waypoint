// Usage → Tools and Bash commands: what the sessions called, how often it
// failed or was refused, how much came back, and what is configured but
// never used.

import { useState } from 'react'
import { ChevronRight } from 'lucide-react'
import clsx from 'clsx'
import {
  bytesText,
  tok,
  type UsageOverview,
  type UsageTool,
  type UsageUnused,
} from '@/lib/usage-analytics'
import { Badge, Section } from '@/components/ui/misc'
import { SortTable, rate, type Col } from './shared'

const GROUPS: { id: UsageTool['group']; title: string; noun: string }[] = [
  { id: 'builtin', title: 'Built-in tools', noun: 'Tool' },
  { id: 'mcp', title: 'MCP servers', noun: 'Server' },
  { id: 'skill', title: 'Skills', noun: 'Skill' },
  { id: 'command', title: 'Slash commands', noun: 'Command' },
  { id: 'subagent', title: 'Subagents', noun: 'Type' },
]

function cols(noun: string, withAsked = false): Col<UsageTool>[] {
  const out: Col<UsageTool>[] = [
    {
      id: 'name',
      label: noun,
      asc: true,
      value: (r) => r.name.toLowerCase(),
      cell: (r) => (
        <div className="min-w-0">
          <div className="truncate font-medium" title={r.name}>
            {r.name}
          </div>
          {!!r.items?.length && (
            <div className="truncate text-xs text-subtle">
              {r.items.map((i) => `${i.name} ${i.calls}`).join(' · ')}
            </div>
          )}
        </div>
      ),
    },
    { id: 'calls', label: 'Calls', value: (r) => r.calls, bar: true, cell: (r) => tok(r.calls) },
    {
      id: 'errors',
      label: 'Errors',
      value: (r) => (r.calls ? r.errors / r.calls : 0),
      cell: (r) => rate(r.errors, r.calls),
    },
    {
      id: 'refused',
      label: 'Refused',
      value: (r) => r.refusals,
      wide: true,
      cell: (r) => (r.refusals ? String(r.refusals) : '–'),
    },
  ]
  if (withAsked)
    out.push({
      id: 'asked',
      label: 'Permission prompts',
      value: (r) => r.prompts,
      wide: true,
      cell: (r) => (r.prompts ? String(r.prompts) : '–'),
    })
  out.push({
    id: 'output',
    label: 'Output',
    value: (r) => r.bytes,
    wide: true,
    cell: (r) => (r.bytes ? bytesText(r.bytes) : '–'),
  })
  return out
}

export function ToolsSection({ o }: { o: UsageOverview }) {
  const byGroup = (g: string) => o.tools.filter((t) => t.group === g)
  const any = o.tools.length > 0
  return (
    <Section
      title="Tools"
      helpId="usage:tools"
      help={
        <p>
          Calls per tool, MCP server, skill, slash command and subagent type, with how often the
          call failed, was refused (by the auto-mode classifier, a hook, a rule or you) and how much
          text came back. Output size is what fills the context, since every later message carries
          it. Skills and commands count when they were run, whether by you or by the model.
        </p>
      }
    >
      {!any ? (
        <p className="px-4 py-8 text-center text-[13px] text-muted">
          No tool calls in this period.
        </p>
      ) : (
        <div className="divide-y divide-line">
          {GROUPS.map((g) => {
            const rows = byGroup(g.id)
            if (rows.length === 0) return null
            return (
              <div key={g.id}>
                <h4 className="px-3 pt-2.5 text-xs font-medium text-muted">{g.title}</h4>
                <SortTable
                  caption={g.title}
                  rows={rows}
                  rowKey={(r) => r.name}
                  sort="calls"
                  cols={cols(g.noun, g.id === 'builtin')}
                />
              </div>
            )
          })}
        </div>
      )}
      <Unused o={o} />
    </Section>
  )
}

const UNUSED_TITLES: Record<UsageUnused['kind'], string> = {
  mcp: 'MCP servers',
  skill: 'Skills',
  command: 'Slash commands',
  agent: 'Subagent definitions',
}

function Unused({ o }: { o: UsageOverview }) {
  const [open, setOpen] = useState(true)
  if (o.no_inventory)
    return (
      <p className="border-t border-line px-4 py-3 text-xs text-subtle">
        To see what is configured but never used, run{' '}
        <code className="rounded bg-sunken px-1">wp usage sync</code> on each machine: it reports
        the names of your MCP servers, skills, commands and agents.
      </p>
    )
  const kinds = (Object.keys(UNUSED_TITLES) as UsageUnused['kind'][]).filter((k) =>
    o.unused.some((u) => u.kind === k),
  )
  return (
    <div className="border-t border-line px-4 py-3">
      <button
        onClick={() => setOpen(!open)}
        aria-expanded={open}
        className="flex items-center gap-1 text-xs font-medium text-muted hover:text-fg"
      >
        <ChevronRight className={clsx('size-3.5 transition-transform', open && 'rotate-90')} />
        Configured but not used in this period
        <Badge className="ml-1">{o.unused.length}</Badge>
      </button>
      {open &&
        (kinds.length === 0 ? (
          <p className="mt-2 text-[13px] text-muted">Everything configured was used.</p>
        ) : (
          <dl className="mt-2 space-y-2">
            {kinds.map((k) => (
              <div key={k}>
                <dt className="text-xs text-subtle">{UNUSED_TITLES[k]}</dt>
                <dd className="mt-1 flex flex-wrap gap-1.5">
                  {o.unused
                    .filter((u) => u.kind === k)
                    .map((u) => (
                      <span
                        key={u.name + u.scope}
                        title={[u.scope, u.machine].filter(Boolean).join(' · ')}
                        className="max-w-full truncate rounded-md bg-sunken px-1.5 py-0.5 text-xs"
                      >
                        {u.name}
                      </span>
                    ))}
                </dd>
              </div>
            ))}
          </dl>
        ))}
    </div>
  )
}

export function BashSection({ o }: { o: UsageOverview }) {
  return (
    <Section
      title="Bash commands"
      helpId="usage:bash"
      help={
        <p>
          The <b>head</b> of each command: the program and, for tools like git, go, npm, make,
          docker, kubectl and wp, its subcommand. Arguments never leave the machine. A chain such as{' '}
          <code>cd x && git commit | tee log</code> counts each part once, and each part takes the
          whole call’s errors, refusals and output.
        </p>
      }
    >
      {o.bash.length === 0 ? (
        <p className="px-4 py-8 text-center text-[13px] text-muted">
          No Bash commands in this period.
        </p>
      ) : (
        <SortTable
          caption="Bash command heads"
          rows={o.bash}
          rowKey={(r) => r.name}
          sort="calls"
          cols={cols('Command', true)}
        />
      )}
      <p className="border-t border-line px-4 py-2 text-xs text-subtle">
        Asked: a person was prompted and said yes.
      </p>
    </Section>
  )
}
