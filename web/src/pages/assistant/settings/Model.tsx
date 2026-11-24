import { useId, useMemo, useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import clsx from 'clsx'
import { Check, Cpu, RefreshCw, RotateCcw, Search } from 'lucide-react'
import { hermes, hermesQuery } from '@/lib/assistant'
import { plural } from '@/lib/format'
import { Button, IconButton, Spinner } from '@/components/ui/button'
import { Confirm, Dialog } from '@/components/ui/dialog'
import { Badge, Section, Skeleton } from '@/components/ui/misc'
import { toast, toastError } from '@/components/ui/toast'
import { isPinned, LoadError, PinnedBadge } from './common'

interface ModelInfo {
  model: string
  provider: string
  auto_context_length: number
  config_context_length: number
  effective_context_length: number
  capabilities: {
    supports_tools?: boolean
    supports_vision?: boolean
    supports_reasoning?: boolean
    context_window?: number
    max_output_tokens?: number
  }
}

interface ProviderOption {
  slug: string
  name: string
  models?: string[]
  is_current?: boolean
  authenticated?: boolean
  warning?: string
  pricing?: Record<string, { input: string; output: string; free: boolean }>
}

interface ModelOptions {
  providers: ProviderOption[]
  model?: string
  provider?: string
}

interface AuxTask {
  task: string
  provider: string
  model: string
  label?: string
  hint?: string
  effective?: { provider: string; model: string }
}

interface SetResult {
  ok: boolean
  confirm_required?: boolean
  confirm_message?: string
  stale_aux?: { task: string }[]
}

/** The built-in auxiliary tasks, in plain words. Plugins label their own. */
const AUX: Record<string, [string, string]> = {
  vision: ['Vision', 'Looking at images'],
  compression: ['Compression', 'Shortening long conversations'],
  skills_hub: ['Skill search', 'Finding skills to use'],
  approval: ['Smart approval', 'Judging commands before they run'],
  mcp: ['MCP routing', 'Picking MCP tools'],
  title_generation: ['Titles', 'Naming conversations'],
  review: ['Review', 'The /review sub-agent'],
  voice_chat: ['Voice chat', 'Spoken replies'],
  triage_specifier: ['Triage', 'Fleshing out kanban tasks'],
  kanban_decomposer: ['Kanban', 'Breaking tasks down'],
  profile_describer: ['Profiles', 'Describing profiles'],
  curator: ['Curator', 'Reviewing how skills are used'],
}

const AUTO = 'auto'

/** 200000 → "200K". */
const tokens = (n: number) =>
  n >= 1_000_000
    ? `${+(n / 1_000_000).toFixed(1)}M`
    : n >= 1000
      ? `${Math.round(n / 1000)}K`
      : `${n}`

export function ModelSection({ id }: { id: number }) {
  const qc = useQueryClient()
  const info = useQuery(hermesQuery<ModelInfo>(id, 'model/info', undefined, 30_000))
  const aux = useQuery(hermesQuery<{ tasks: AuxTask[] }>(id, 'model/auxiliary', undefined, 30_000))
  const [picking, setPicking] = useState<'main' | AuxTask | null>(null)
  const [resetting, setResetting] = useState(false)
  const [confirm, setConfirm] = useState<{ message: string; run: () => void } | null>(null)
  const refresh = () =>
    ['model/info', 'model/auxiliary', 'model/options', 'config', 'config/raw'].forEach((p) =>
      qc.invalidateQueries({ queryKey: ['assistant', id, p] }),
    )

  /** POST model/set, asking first when Hermes wants a yes for a pricey model. */
  const assign = async (body: Record<string, unknown>, confirmed = false): Promise<boolean> => {
    const res = await hermes<SetResult>(id, 'POST', 'model/set', {
      ...body,
      confirm_expensive_model: confirmed,
    })
    if (res.confirm_required) {
      setConfirm({
        message: res.confirm_message || 'This model is expensive.',
        run: () =>
          assign(body, true)
            .then((ok) => ok && setPicking(null))
            .catch(toastError),
      })
      return false
    }
    refresh()
    if (body.scope === 'main') {
      const stale = res.stale_aux?.length ?? 0
      toast(
        `New conversations use ${body.model}` +
          (stale ? `; ${plural(stale, 'auxiliary task')} still name the old provider` : ''),
        { kind: 'success' },
      )
    } else toast('Saved', { kind: 'success' })
    return true
  }

  const pick = async (provider: string, model: string) => {
    if (!picking) return
    const body =
      picking === 'main'
        ? { scope: 'main', provider, model }
        : {
            scope: 'auxiliary',
            task: picking.task,
            provider,
            model: provider === AUTO ? '' : model,
          }
    try {
      if (await assign(body)) setPicking(null)
    } catch (e) {
      toastError(e)
    }
  }

  const m = info.data
  const tasks = aux.data?.tasks ?? []
  const pinned = tasks.filter((t) => t.provider && t.provider !== AUTO).length
  return (
    <>
      <Section title="Main model">
        {info.isPending ? (
          <div className="p-4">
            <Skeleton className="h-16" />
          </div>
        ) : info.isError ? (
          <LoadError error={info.error} what="the model" />
        ) : (
          m && (
            <div className="flex flex-col gap-3 p-4 sm:flex-row sm:items-center">
              <span className="flex size-10 shrink-0 items-center justify-center rounded-xl bg-accent-soft text-accent">
                <Cpu className="size-5" />
              </span>
              <div className="min-w-0 flex-1">
                <div className="truncate font-mono text-[15px] font-semibold">{m.model || '—'}</div>
                <div className="mt-1 flex flex-wrap items-center gap-1.5 text-[13px] text-muted">
                  <span>{m.provider}</span>
                  {isPinned('model.provider') && <PinnedBadge />}
                  {m.effective_context_length > 0 && (
                    <Badge>{tokens(m.effective_context_length)} context</Badge>
                  )}
                  {m.capabilities.supports_tools && <Badge>tools</Badge>}
                  {m.capabilities.supports_vision && <Badge>vision</Badge>}
                  {m.capabilities.supports_reasoning && <Badge>reasoning</Badge>}
                </div>
              </div>
              <Button className="self-start sm:self-auto" onClick={() => setPicking('main')}>
                Change model
              </Button>
            </div>
          )
        )}
        <p className="border-t border-line px-4 py-2 text-xs text-subtle">
          A change applies to new conversations; ones in progress keep their model.
        </p>
      </Section>

      <Section
        title="Auxiliary tasks"
        help="Smaller jobs Hermes does alongside a conversation. “Same as main” uses the main model."
        actions={
          pinned > 0 && (
            <Button
              size="sm"
              variant="ghost"
              icon={<RotateCcw className="size-3.5" />}
              onClick={() => setResetting(true)}
            >
              Reset all
            </Button>
          )
        }
      >
        {aux.isPending ? (
          <div className="space-y-2 p-4">
            <Skeleton className="h-8" />
            <Skeleton className="h-8" />
            <Skeleton className="h-8" />
          </div>
        ) : aux.isError ? (
          <LoadError error={aux.error} what="the auxiliary tasks" />
        ) : (
          <ul className="divide-y divide-line">
            {tasks.map((t) => {
              const [label, hint] = AUX[t.task] ?? [t.label || t.task, t.hint || '']
              const auto = !t.provider || t.provider === AUTO
              return (
                <li key={t.task} className="flex items-center gap-3 px-4 py-2">
                  <div className="min-w-0 flex-1">
                    <div className="text-[13px] font-medium">{label}</div>
                    {hint && <div className="truncate text-xs text-subtle">{hint}</div>}
                  </div>
                  <div className="min-w-0 max-w-[45%] text-right text-[13px]">
                    {auto ? (
                      <span className="text-muted">Same as main</span>
                    ) : (
                      <>
                        <div className="truncate font-mono text-xs">{t.model || 'default'}</div>
                        <div className="truncate text-[11px] text-subtle">{t.provider}</div>
                      </>
                    )}
                  </div>
                  <Button size="sm" variant="ghost" onClick={() => setPicking(t)}>
                    Change
                  </Button>
                </li>
              )
            })}
          </ul>
        )}
      </Section>

      <ModelPicker
        id={id}
        open={!!picking}
        onClose={() => setPicking(null)}
        title={
          picking === 'main'
            ? 'Main model'
            : picking
              ? `Model for ${(AUX[picking.task]?.[0] ?? picking.label ?? picking.task).toLowerCase()}`
              : ''
        }
        auxiliary={picking !== 'main'}
        current={
          picking === 'main'
            ? m && { provider: m.provider, model: m.model }
            : picking && { provider: picking.provider || AUTO, model: picking.model }
        }
        mainProvider={m?.provider ?? ''}
        onPick={pick}
      />
      <Confirm
        open={resetting}
        onClose={() => setResetting(false)}
        title="Use the main model for every auxiliary task?"
        confirmLabel="Reset all"
        onConfirm={() =>
          assign({ scope: 'auxiliary', task: '__reset__', provider: AUTO, model: '' }).catch(
            toastError,
          )
        }
      />
      <Confirm
        open={!!confirm}
        onClose={() => setConfirm(null)}
        danger={false}
        title="Use this model?"
        body={confirm?.message}
        confirmLabel="Use it"
        onConfirm={() => confirm?.run()}
      />
    </>
  )
}

interface Choice {
  provider: string
  model: string
  label: string
  hint?: string
  disabled?: string
}

/**
 * A searchable list of models, grouped by provider. Only the deploy's
 * provider can be picked while it pins it; a name typed in that isn't
 * listed can be used too (the gateway may serve more than it lists).
 */
function ModelPicker({
  id,
  open,
  onClose,
  title,
  auxiliary,
  current,
  mainProvider,
  onPick,
}: {
  id: number
  open: boolean
  onClose: () => void
  title: string
  auxiliary: boolean
  current: { provider: string; model: string } | null | undefined
  mainProvider: string
  onPick: (provider: string, model: string) => Promise<void>
}) {
  const qc = useQueryClient()
  const opts = useQuery({
    ...hermesQuery<ModelOptions>(id, 'model/options', undefined, 5 * 60_000),
    enabled: open,
  })
  const [text, setText] = useState('')
  const [active, setActive] = useState(0)
  const [busy, setBusy] = useState('')
  const [refreshing, setRefreshing] = useState(false)
  const locked = isPinned('model.provider') ? mainProvider : ''
  const groups = useMemo(() => {
    const t = text.trim().toLowerCase()
    const out: { name: string; slug: string; choices: Choice[] }[] = []
    for (const p of opts.data?.providers ?? []) {
      const models = p.models ?? []
      if (!models.length) continue
      const disabled = locked && p.slug !== locked ? 'The deploy sets the provider' : undefined
      const choices = models
        .filter((mo) => !t || mo.toLowerCase().includes(t) || p.name.toLowerCase().includes(t))
        .map((mo) => {
          const price = p.pricing?.[mo]
          return {
            provider: p.slug,
            model: mo,
            label: mo,
            hint: price ? (price.free ? 'free' : `${price.input} / ${price.output}`) : undefined,
            disabled,
          }
        })
      if (choices.length) out.push({ name: p.name, slug: p.slug, choices })
    }
    out.sort((a, b) => Number(b.slug === locked) - Number(a.slug === locked))
    return out
  }, [opts.data, text, locked])
  const typed = text.trim()
  const known = groups.some((g) => g.choices.some((c) => c.model === typed))
  const extra: Choice[] = []
  if (auxiliary && !typed)
    extra.push({ provider: AUTO, model: '', label: 'Same as the main model' })
  if (typed && !known) {
    const p = locked || current?.provider || mainProvider
    if (p && p !== AUTO) extra.push({ provider: p, model: typed, label: `Use “${typed}”`, hint: p })
  }
  const flat = [...extra, ...groups.flatMap((g) => g.choices)]
  const enabled = flat.filter((c) => !c.disabled)
  const isCurrent = (c: Choice) =>
    !!current &&
    (c.provider === AUTO
      ? current.provider === AUTO
      : c.provider === current.provider && c.model === current.model)
  const choose = async (c: Choice | undefined) => {
    if (!c || c.disabled || busy) return
    if (isCurrent(c)) return onClose()
    setBusy(c.provider + '/' + c.model)
    await onPick(c.provider, c.model)
    setBusy('')
  }
  const reload = async () => {
    setRefreshing(true)
    try {
      const fresh = await hermes<ModelOptions>(id, 'GET', 'model/options', undefined, {
        refresh: true,
      })
      qc.setQueryData(hermesQuery<ModelOptions>(id, 'model/options').queryKey, fresh)
    } catch (e) {
      toastError(e)
    } finally {
      setRefreshing(false)
    }
  }
  const listId = useId()
  const row = (c: Choice) => {
    const idx = enabled.indexOf(c)
    const key = c.provider + '/' + c.model
    return (
      <button
        key={key + c.label}
        type="button"
        data-idx={idx}
        id={idx >= 0 ? `${listId}-${idx}` : undefined}
        role="option"
        aria-selected={idx >= 0 && idx === active}
        aria-disabled={!!c.disabled || undefined}
        tabIndex={-1}
        disabled={!!c.disabled}
        title={c.disabled}
        onMouseEnter={() => idx >= 0 && setActive(idx)}
        onClick={() => choose(c)}
        className={clsx(
          'flex h-10 w-full items-center gap-2.5 rounded-lg px-2.5 text-left text-[13px] disabled:opacity-40 sm:h-9',
          idx === active && 'bg-hover',
        )}
      >
        <span
          className={clsx('min-w-0 flex-1 truncate', c.provider !== AUTO && 'font-mono text-xs')}
        >
          {c.label}
        </span>
        {c.hint && <span className="shrink-0 text-xs text-subtle">{c.hint}</span>}
        {busy === key ? (
          <Spinner className="size-3.5" />
        ) : (
          <Check
            className={clsx(
              'size-3.5 shrink-0 text-accent',
              isCurrent(c) ? 'opacity-100' : 'opacity-0',
            )}
          />
        )}
      </button>
    )
  }
  return (
    <Dialog
      open={open}
      onClose={() => {
        setText('')
        onClose()
      }}
      title={title}
    >
      <div className="flex items-center gap-2 border-b border-line px-4 py-2">
        <Search className="size-4 shrink-0 text-subtle" />
        <input
          autoFocus
          value={text}
          onChange={(e) => {
            setText(e.target.value)
            setActive(0)
          }}
          placeholder="Search or type a model name…"
          aria-label="Search models"
          role="combobox"
          aria-expanded
          aria-controls={listId}
          aria-autocomplete="list"
          aria-activedescendant={enabled[active] ? `${listId}-${active}` : undefined}
          className="h-9 min-w-0 flex-1 bg-transparent text-sm outline-none placeholder:text-subtle"
          onKeyDown={(e) => {
            const to =
              e.key === 'ArrowDown'
                ? Math.min(active + 1, enabled.length - 1)
                : e.key === 'ArrowUp'
                  ? Math.max(active - 1, 0)
                  : null
            if (to !== null) {
              e.preventDefault()
              setActive(to)
              document.querySelector(`[data-idx="${to}"]`)?.scrollIntoView({ block: 'nearest' })
            } else if (e.key === 'Enter') {
              e.preventDefault()
              choose(enabled[active])
            }
          }}
        />
        <IconButton
          label="Ask the providers again"
          onClick={reload}
          icon={<RefreshCw className={clsx('size-4', refreshing && 'animate-spin')} />}
        />
      </div>
      <div
        id={listId}
        role="listbox"
        aria-label="Models"
        className="max-h-[60vh] overflow-y-auto p-2"
      >
        {opts.isPending ? (
          <div className="space-y-2 p-2">
            <Skeleton className="h-8" />
            <Skeleton className="h-8" />
          </div>
        ) : opts.isError ? (
          <LoadError error={opts.error} what="the models" />
        ) : (
          <>
            {extra.map(row)}
            {groups.map((g) => (
              <div key={g.slug}>
                <div className="flex items-center gap-2 px-2.5 pb-1 pt-2 text-[11px] font-semibold uppercase tracking-wide text-subtle">
                  {g.name}
                  {g.slug === locked && <PinnedBadge className="normal-case tracking-normal" />}
                </div>
                {g.choices.map(row)}
              </div>
            ))}
            {flat.length === 0 && (
              <div className="px-3 py-6 text-center text-xs text-subtle">Nothing matches</div>
            )}
          </>
        )}
      </div>
    </Dialog>
  )
}
