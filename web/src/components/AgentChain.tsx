import { ArrowDown, ArrowUp, Plus, X } from 'lucide-react'
import { Button, IconButton } from '@/components/ui/button'
import { Input, Select } from '@/components/ui/input'

export interface AgentModel {
  cli: string
  model?: string
}

export const AGENT_CLIS: { value: string; label: string; models: string[] }[] = [
  { value: 'claude', label: 'Claude Code', models: ['sonnet', 'opus', 'haiku'] },
  { value: 'codex', label: 'Codex', models: ['gpt-5-codex'] },
  { value: 'opencode', label: 'opencode', models: ['opencode/muse-spark-1.3-contributor-free'] },
  { value: 'cursor', label: 'Cursor', models: ['grok-4.7-high', 'grok-4.7-medium', 'auto'] },
]

/** What an audit's chain may name: the LLM gateway, which runs the
 * audit itself and picks the model (WP-617), and the CLIs on a runner. */
export const AUDIT_CLIS: typeof AGENT_CLIS = [
  { value: 'gateway', label: 'LLM gateway', models: ['audit', 'free'] },
  ...AGENT_CLIS,
]

export const cliLabel = (cli: string) => AUDIT_CLIS.find((c) => c.value === cli)?.label ?? cli

/** "opencode/muse-spark-1.3-contributor-free" → "muse-spark-1.3-contributor-free" */
const bare = (m: string) => m.slice(m.indexOf('/') + 1)

/** The chain as a phrase: "Claude Code sonnet → opencode muse-spark-1.3-contributor-free". */
export function agentsText(list: AgentModel[]): string {
  return list
    .map((a) => (a.model ? `${cliLabel(a.cli)} ${bare(a.model)}` : cliLabel(a.cli)))
    .join(' → ')
}

export function AgentChain({
  value,
  onChange,
  clis = AGENT_CLIS,
}: {
  value: AgentModel[]
  onChange: (v: AgentModel[]) => void
  /** The CLIs offered (default: those of a runner). */
  clis?: typeof AGENT_CLIS
}) {
  const move = (i: number, by: number) => {
    const list = [...value]
    const [m] = list.splice(i, 1)
    list.splice(i + by, 0, m)
    onChange(list)
  }
  const edit = (i: number, patch: Partial<AgentModel>) =>
    onChange(value.map((m, j) => (j === i ? { ...m, ...patch } : m)))
  const next = clis.find((c) => !value.some((v) => v.cli === c.value)) ?? clis[0]
  return (
    <div className="space-y-1" data-testid="agent-chain">
      {value.length > 0 && (
        <ol className="space-y-1">
          {value.map((m, i) => {
            const known = clis.find((c) => c.value === m.cli)
            const offered = known || !m.cli ? clis : [...clis, { value: m.cli, label: m.cli }]
            const list = `agent-models-${i}`
            return (
              <li
                key={i}
                className="flex flex-wrap items-center gap-1.5 rounded-lg border border-line bg-panel py-0.5 pl-2 pr-0.5"
              >
                <span className="w-4 shrink-0 text-right text-[11px] tabular-nums text-subtle">
                  {i + 1}
                </span>
                <Select
                  variant="inline"
                  value={m.cli}
                  onChange={(e) => edit(i, { cli: e.target.value, model: '' })}
                  aria-label={`CLI ${i + 1}`}
                >
                  {offered.map((c) => (
                    <option key={c.value} value={c.value}>
                      {c.label}
                    </option>
                  ))}
                </Select>
                <Input
                  list={list}
                  value={m.model ?? ''}
                  onChange={(e) => edit(i, { model: e.target.value })}
                  placeholder="the CLI’s own default model"
                  spellCheck={false}
                  className="!h-7 min-w-0 flex-1 basis-40 !px-2 font-mono !text-[12px]"
                  aria-label={`Model ${i + 1}`}
                />
                <datalist id={list}>
                  {(known?.models ?? []).map((x) => (
                    <option key={x} value={x} />
                  ))}
                </datalist>
                <span className="flex shrink-0">
                  <IconButton
                    label="Try earlier"
                    size="xs"
                    disabled={i === 0}
                    onClick={() => move(i, -1)}
                    icon={<ArrowUp className="size-3.5" />}
                  />
                  <IconButton
                    label="Try later"
                    size="xs"
                    disabled={i === value.length - 1}
                    onClick={() => move(i, 1)}
                    icon={<ArrowDown className="size-3.5" />}
                  />
                  <IconButton
                    label={`Remove ${cliLabel(m.cli)}`}
                    size="xs"
                    onClick={() => onChange(value.filter((_, j) => j !== i))}
                    icon={<X className="size-3.5" />}
                  />
                </span>
              </li>
            )
          })}
        </ol>
      )}
      {value.length < 6 && (
        <Button
          type="button"
          variant="ghost"
          size="xs"
          icon={<Plus className="size-3.5" />}
          onClick={() => onChange([...value, { cli: next.value, model: next.models[0] ?? '' }])}
        >
          {value.length === 0 ? 'Choose a CLI and model' : 'Add a fallback'}
        </Button>
      )}
    </div>
  )
}
