import { useState, type ReactNode, type RefObject } from 'react'
import { useQuery } from '@tanstack/react-query'
import { ArrowDown, ArrowUp, Coins, Plus, X } from 'lucide-react'
import { aiq, type AIModel, type AIProvider } from '@/lib/ai'
import { CLI_LABELS, CLI_SIGN_IN, REVIEW_CLIS, cliReadiness, runnersQuery } from '@/lib/runners'
import type { ChainModel } from '@/lib/types'
import { AGENT_CLIS, cliLabel } from '@/components/AgentChain'
import { Button, IconButton, ToggleButton } from '@/components/ui/button'
import { Input, Select } from '@/components/ui/input'
import { Badge, Skeleton } from '@/components/ui/misc'
import { MenuList, Picker, Popover, usePopover } from '@/components/ui/popover'

export const PATTERN_HINT = '* picks the newest model of that name the provider offers.'

/** "opencode/muse-spark-1.3-contributor-free" → "muse-spark-1.3-contributor-free" */
export const shortModel = (m: string) => m.slice(m.indexOf('/') + 1)

/** One entry as a phrase: "muse-spark-*", or "Claude Code sonnet (runner)". */
const entryText = (m: ChainModel) =>
  m.cli
    ? `${cliLabel(m.cli)}${m.model ? ' ' + shortModel(m.model) : ''} (runner)`
    : shortModel(m.model)

/** Models in order: "muse-spark-* → nemotron-*-ultra-free". */
export function chainText(models: ChainModel[]): string {
  return models.map(entryText).join(' → ')
}

/**
 * An ordered, editable list of models. With `runners`, an entry may also be a
 * coding agent (Claude Code, Codex, Cursor, opencode) on the runner of
 * whoever saves the rule (WP-D14): review and reply rules only.
 */
export function ModelChain({
  value,
  onChange,
  addLabel = 'Add a model',
  start = 1,
  runners = false,
}: {
  value: ChainModel[]
  onChange: (v: ChainModel[]) => void
  addLabel?: string
  /** the number of the first entry, when something comes before the list */
  start?: number
  /** entries may also name a coding agent on the owner's runner */
  runners?: boolean
}) {
  const providers = useQuery(aiq.providers()).data ?? []
  const mine = useQuery({ ...runnersQuery(), enabled: runners && value.some((m) => m.cli) })
  const ready = cliReadiness(mine.data ?? [])
  const agentMenu = usePopover()
  const edit = (i: number, patch: Partial<ChainModel>) =>
    onChange(value.map((m, j) => (j === i ? { ...m, ...patch } : m)))
  const unready = mine.isSuccess
    ? [...new Set(value.flatMap((m) => (m.cli && ready[m.cli]?.state !== 'ready' ? [m.cli] : [])))]
    : []
  const move = (i: number, by: number) => {
    const list = [...value]
    const [m] = list.splice(i, 1)
    list.splice(i + by, 0, m)
    onChange(list)
  }
  return (
    <div className="space-y-1">
      {value.length > 0 && (
        <ol className="space-y-1">
          {value.map((m, i) => (
            <li
              key={m.cli ? `${i}-${m.cli}` : `${i}-${m.provider_id}-${m.model}`}
              className="flex flex-wrap items-center gap-2 rounded-lg border border-line bg-panel py-0.5 pl-2 pr-0.5"
            >
              <span className="w-4 shrink-0 text-right text-[11px] tabular-nums text-subtle">
                {i + start}
              </span>
              {m.cli ? (
                <>
                  <span className="shrink-0 text-xs font-medium">{cliLabel(m.cli)}</span>
                  <Badge>my runner</Badge>
                  <Input
                    list={`runner-models-${i}`}
                    value={m.model}
                    onChange={(e) => edit(i, { model: e.target.value })}
                    placeholder="the CLI’s own default model"
                    spellCheck={false}
                    className="!h-7 min-w-0 flex-1 basis-32 !px-2 font-mono !text-[12px]"
                    aria-label={`Model of ${cliLabel(m.cli)}`}
                  />
                  <datalist id={`runner-models-${i}`}>
                    {(AGENT_CLIS.find((c) => c.value === m.cli)?.models ?? []).map((x) => (
                      <option key={x} value={x} />
                    ))}
                  </datalist>
                </>
              ) : (
                <span className="min-w-0 flex-1 truncate font-mono text-xs" title={m.model}>
                  {m.model}
                </span>
              )}
              {!m.cli && (
                <ToggleButton
                  label={`Paid: ${m.model}`}
                  hint="costs money, so it is asked on its own, only after the models before it failed"
                  pressed={!!m.paid}
                  onPressedChange={(paid) => edit(i, { paid: paid || undefined })}
                  icon={<Coins className="size-3.5" />}
                  text={m.paid ? 'paid' : undefined}
                  className="!h-6"
                />
              )}
              {!m.cli && providers.length > 1 && (
                <span className="hidden shrink-0 text-[11px] text-subtle sm:inline">
                  {providers.find((p) => p.id === m.provider_id)?.name ?? 'removed provider'}
                </span>
              )}
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
                  label={`Remove ${m.cli ? cliLabel(m.cli) : m.model}`}
                  size="xs"
                  onClick={() => onChange(value.filter((_, j) => j !== i))}
                  icon={<X className="size-3.5" />}
                />
              </span>
            </li>
          ))}
        </ol>
      )}
      <div className="flex flex-wrap items-center gap-x-1">
        <ModelChooser
          providers={providers}
          label={addLabel}
          onPick={(m) => m && onChange([...value, m])}
          trigger={(props) => (
            <Button {...props} variant="ghost" size="xs" icon={<Plus className="size-3.5" />}>
              {addLabel}
            </Button>
          )}
        />
        {runners && (
          <>
            <Button
              ref={agentMenu.anchor}
              variant="ghost"
              size="xs"
              icon={<Plus className="size-3.5" />}
              onClick={agentMenu.toggle}
              aria-label="Add a coding agent on my runner"
            >
              Coding agent on my runner
            </Button>
            <Popover
              open={agentMenu.open}
              onClose={agentMenu.close}
              anchor={agentMenu.anchor}
              align="start"
            >
              <MenuList
                onClose={agentMenu.close}
                items={REVIEW_CLIS.map((cli) => ({
                  label: CLI_LABELS[cli],
                  onSelect: () => onChange([...value, { provider_id: 0, model: '', cli }]),
                }))}
              />
            </Popover>
          </>
        )}
      </div>
      {unready.length > 0 && (
        <p className="text-[11px] leading-snug text-warn" data-testid="runner-note">
          None of your runners has {unready.map((c) => CLI_LABELS[c] ?? c).join(' or ')} ready:
          connect one under Settings → Runners
          {unready.length === 1 && CLI_SIGN_IN[unready[0]]
            ? ` and sign in with ${CLI_SIGN_IN[unready[0]]}`
            : ''}
          . Until then this step is skipped and the next one is tried.
        </p>
      )}
    </div>
  )
}

/** A popover to pick one model from any provider, or type a family pattern. */
export function ModelChooser({
  providers,
  label,
  onPick,
  trigger,
  selected,
  allowOff,
  initialProvider,
}: {
  providers: AIProvider[]
  label: string
  onPick: (m: ChainModel | null) => void
  trigger: (props: {
    ref: RefObject<HTMLButtonElement | null>
    onClick: () => void
    'aria-label': string
  }) => ReactNode
  selected?: string
  allowOff?: boolean
  initialProvider?: number | null
}) {
  const pop = usePopover()
  const [pid, setPid] = useState<number>(initialProvider || providers[0]?.id || 0)
  const provider = pid || providers[0]?.id || 0
  const models = useQuery({ ...aiq.models(provider), enabled: pop.open && provider > 0 })
  const items = [
    ...(allowOff ? [{ value: '' as string, label: 'Off', hint: undefined }] : []),
    ...(models.data ?? []).map((m: AIModel) => ({
      value: m.id,
      label: m.name,
      keywords: m.id + (m.free ? ' free' : ''),
      hint: m.free ? <Badge color="var(--wp-ok)">free</Badge> : undefined,
    })),
  ]
  const pick = (model: string) => {
    const listed = models.data?.find((m) => m.id === model)
    onPick(
      model
        ? { provider_id: provider, model, ...(listed && !listed.free ? { paid: true } : {}) }
        : null,
    )
    pop.close()
  }
  return (
    <>
      {trigger({
        ref: pop.anchor,
        onClick: pop.toggle,
        'aria-label': label,
      })}
      <Popover open={pop.open} onClose={pop.close} anchor={pop.anchor} align="start" width={320}>
        {providers.length === 0 ? (
          <div className="p-3 text-xs text-subtle">Add an AI provider under Admin → AI first.</div>
        ) : (
          <div className="flex flex-col">
            {providers.length > 1 && (
              <div className="border-b border-line p-1.5">
                <Select
                  aria-label="Provider"
                  value={provider}
                  onChange={(e) => setPid(Number(e.target.value))}
                  className="!h-8 text-xs"
                >
                  {providers.map((p) => (
                    <option key={p.id} value={p.id}>
                      {p.name}
                    </option>
                  ))}
                </Select>
              </div>
            )}
            {models.isPending ? (
              <div className="p-3">
                <Skeleton className="h-8" />
              </div>
            ) : (
              <>
                {models.isError && (
                  <div className="px-3 pt-2 text-xs text-danger">
                    {(models.error as Error).message}
                  </div>
                )}
                <Picker
                  items={items}
                  selected={selected !== undefined ? [selected] : []}
                  placeholder="Search, or type a pattern like opencode/muse-spark-*"
                  onClose={pop.close}
                  onPick={(v) => pick(v)}
                  onCreate={(text) => pick(text)}
                  createLabel={(text) => `Use “${text}”`}
                />
                <p className="border-t border-line px-3 py-2 text-[11px] text-subtle">
                  {PATTERN_HINT}
                </p>
              </>
            )}
          </div>
        )}
      </Popover>
    </>
  )
}
