import { useEffect, useMemo, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import {
  Check,
  ChevronDown,
  CircleHelp,
  MoreHorizontal,
  PlugZap,
  Pencil,
  Plus,
  Play,
  RotateCcw,
  ShieldCheck,
  Sparkles,
  Trash2,
  X,
} from 'lucide-react'
import { api } from '@/lib/api'
import {
  aiq,
  useRunSafetyDrill,
  useSetAITask,
  type AIProvider,
  type AITask,
  type DrillCase,
  type ProviderKind,
} from '@/lib/ai'
import { relativeTime } from '@/lib/format'
import { renderMarkdown } from '@/lib/markdown'
import { renderNote, reviewNoteQuery, setReviewNote } from '@/lib/pulls'
import { CLI_LABELS, CLI_SIGN_IN, REVIEW_CLIS, cliReadiness, runnersQuery } from '@/lib/runners'
import type { ChainModel } from '@/lib/types'
import { ModelChain, ModelChooser } from '@/components/ModelChain'
import { Button, IconButton } from '@/components/ui/button'
import { Confirm, Dialog } from '@/components/ui/dialog'
import { Field, Input, Select } from '@/components/ui/input'
import { Badge, Empty, Section, Skeleton } from '@/components/ui/misc'
import { MenuList, Popover, usePopover } from '@/components/ui/popover'
import { toast, toastError } from '@/components/ui/toast'
import { ConnectPanel } from '@/pages/settings/Runners'
import { PricesSection } from './Prices'
import { ReviewArmsSection } from './ReviewArms'
import { ReviewStatsSection } from './ReviewStats'
import { ShipReplySection } from './ShipReply'
import { AutopilotTextSection } from '@/components/AutopilotWording'
import { StalledJobsSection } from '@/components/StalledJobsSettings'

export function AITab() {
  const qc = useQueryClient()
  const providers = useQuery(aiq.providers())
  const tasks = useQuery(aiq.tasks())
  const [editing, setEditing] = useState<AIProvider | 'new' | null>(null)
  const [deleting, setDeleting] = useState<AIProvider | null>(null)
  const del = useMutation({
    mutationFn: (id: number) => api.del(`/ai/providers/${id}`),
    onSettled: () => qc.invalidateQueries({ queryKey: ['ai'] }),
    onError: toastError,
  })
  const list = providers.data ?? []
  return (
    <>
      <Section
        title="What AI does"
        help={
          <>
            Pick a model for each job, and models to fall back on when it fails: they are tried in
            order until one answers. A pattern like{' '}
            <code className="font-mono text-xs">opencode/muse-spark-*</code> picks the newest model
            of that name. Suggestions change nothing until someone applies them, except automatic
            estimates (a project setting), which only fill in empty points, and pull request
            reviews, which automations post.
          </>
        }
      >
        {tasks.isPending || providers.isPending ? (
          <div className="p-4">
            <Skeleton className="h-20" />
          </div>
        ) : (
          <ul className="divide-y divide-line">
            {(tasks.data ?? []).map((t) => (
              <TaskRow key={t.task} task={t} providers={list} />
            ))}
          </ul>
        )}
      </Section>
      <ReviewArmsSection />
      <ReviewStatsSection />
      <SafetyDrillSection />
      <Section
        title="Providers"
        help={
          <>
            An <b>opencode</b> server (<code className="font-mono text-xs">opencode serve</code>)
            brings free models; <b>Ollama</b> runs models on your own hardware, local or remote; any{' '}
            <b>OpenAI-compatible</b> API works too (OpenRouter, LM Studio, xAI).
          </>
        }
        className="mt-4"
        actions={
          <Button
            size="sm"
            variant="primary"
            icon={<Plus className="size-3.5" />}
            onClick={() => setEditing('new')}
          >
            Add provider
          </Button>
        }
      >
        {providers.isPending ? (
          <div className="p-4">
            <Skeleton className="h-14" />
          </div>
        ) : list.length === 0 ? (
          <Empty icon={<Sparkles />} title="No AI providers yet" />
        ) : (
          <ul className="divide-y divide-line">
            {list.map((p) => (
              <ProviderRow
                key={p.id}
                provider={p}
                onEdit={() => setEditing(p)}
                onDelete={() => setDeleting(p)}
              />
            ))}
          </ul>
        )}
      </Section>
      <CodingAgentsSection />
      <ShipReplySection />
      <AutopilotTextSection />
      <StalledJobsSection />
      <PricesSection />
      <ProviderDialog provider={editing} onClose={() => setEditing(null)} />
      <Confirm
        open={!!deleting}
        onClose={() => setDeleting(null)}
        title={`Remove ${deleting?.name}?`}
        body="Tasks using its models are switched off until you pick another model."
        confirmLabel="Remove"
        onConfirm={() => deleting && del.mutate(deleting.id)}
      />
    </>
  )
}

/**
 * The weekly safety drill (WP-278): the screen's models read task texts with
 * known answers, so one that drifted or was swapped for a weaker one shows.
 */
function SafetyDrillSection() {
  const drill = useQuery(aiq.drill())
  const run = useRunSafetyDrill()
  const d = drill.data
  const last = d?.last
  return (
    <Section
      title="Safety drill"
      help={
        <>
          Once a week the models for <b>Screen agent-written tasks</b> read {d?.cases ?? 8} task
          texts an agent might write: bad ones they must hold (software on a NAS,{' '}
          <code className="font-mono text-xs">curl | sh</code>, a secret, text that talks to the
          screen, a subtle one in prose) and plain ones they must clear. Real tasks are untouched.
          When a bad one clears or a plain one is held, admins are notified.
        </>
      }
      className="mt-4"
      actions={
        <Button
          size="sm"
          icon={<Play className="size-3.5" />}
          disabled={!d?.configured}
          loading={run.isPending || !!d?.running}
          onClick={() => run.mutate(undefined, { onError: toastError })}
        >
          {d?.running ? 'Running' : 'Run now'}
        </Button>
      }
    >
      {drill.isPending ? (
        <div className="p-4">
          <Skeleton className="h-20" />
        </div>
      ) : !d?.configured ? (
        <p className="px-4 py-3 text-xs text-subtle">
          No model screens agent-written tasks, so the rules alone decide and there is nothing to
          drill.
        </p>
      ) : !last ? (
        <p className="px-4 py-3 text-xs text-subtle">
          {d.running ? 'The first drill is running.' : 'The first drill runs within the hour.'}
        </p>
      ) : (
        <div data-testid="safety-drill">
          <div className="flex flex-wrap items-center gap-x-3 gap-y-1 px-4 py-2.5 text-xs text-muted">
            <span className="inline-flex items-center gap-1.5 text-[13px] font-medium text-fg">
              <ShieldCheck
                className={last.failed ? 'size-4 text-danger' : 'size-4 text-ok'}
                aria-hidden
              />
              {last.passed} of {last.cases.length} right
            </span>
            {last.failed > 0 && <Badge color="var(--wp-danger)">{last.failed} wrong</Badge>}
            {last.errors > 0 && <Badge>{last.errors} unanswered</Badge>}
            <span>
              {relativeTime(last.finished_at)}
              {last.started_by ? `, run by ${last.started_by.display_name}` : ''}
              {d.next_at && !d.running ? ` · next ${relativeTime(d.next_at)}` : ''}
            </span>
          </div>
          <ul className="divide-y divide-line border-t border-line">
            {last.cases.map((c) => (
              <DrillCaseRow key={c.name} c={c} />
            ))}
          </ul>
          {d.models.length > 0 && (
            <div className="border-t border-line px-4 py-2.5">
              <div className="mb-1 text-[11px] font-semibold uppercase tracking-wide text-subtle">
                Right answers by model, every drill
              </div>
              <ul className="space-y-1">
                {d.models.map((m) => (
                  <li key={m.model} className="flex items-center gap-3 text-xs">
                    <span className="min-w-0 flex-1 truncate font-mono">{m.model}</span>
                    <span className={m.passed < m.total ? 'text-danger' : 'text-muted'}>
                      {m.passed}/{m.total} ({Math.round((100 * m.passed) / m.total)}%)
                    </span>
                    <span className="hidden w-24 text-right text-subtle sm:inline">
                      {relativeTime(m.last_at)}
                    </span>
                  </li>
                ))}
              </ul>
            </div>
          )}
        </div>
      )}
    </Section>
  )
}

function DrillCaseRow({ c }: { c: DrillCase }) {
  const want = c.want === 'hold' ? 'must be held' : 'must clear'
  const got = !c.verdict ? 'no answer' : c.verdict === 'hold' ? 'held' : 'cleared'
  return (
    <li className="flex gap-3 px-4 py-2" data-pass={c.pass}>
      {!c.verdict ? (
        <CircleHelp className="mt-0.5 size-4 shrink-0 text-subtle" aria-label="No answer" />
      ) : c.pass ? (
        <Check className="mt-0.5 size-4 shrink-0 text-ok" aria-label="Right" />
      ) : (
        <X className="mt-0.5 size-4 shrink-0 text-danger" aria-label="Wrong" />
      )}
      <div className="min-w-0 flex-1">
        <div className="flex flex-wrap items-center gap-x-2 gap-y-0.5">
          <span className="text-[13px] font-medium">{c.label}</span>
          <span className="text-xs text-subtle">{want}</span>
          <Badge color={!c.verdict ? undefined : c.pass ? 'var(--wp-ok)' : 'var(--wp-danger)'}>
            {got}
          </Badge>
        </div>
        {(c.reason || c.error || c.flags?.length) && (
          <div className="mt-0.5 text-xs text-muted">
            {c.error || c.reason || c.flags?.join(', ')}
          </div>
        )}
        {c.model && <div className="truncate font-mono text-[11px] text-subtle">{c.model}</div>}
      </div>
    </li>
  )
}

/**
 * Claude Code, Codex, Cursor and opencode on the viewer's own runners: the
 * subscriptions a review rule can use, which stay on those computers (WP-D14).
 * A rule uses the runners of whoever saved it, so this lists yours only.
 */
function CodingAgentsSection() {
  const runners = useQuery(runnersQuery())
  const [connecting, setConnecting] = useState(false)
  const list = runners.data ?? []
  const ready = cliReadiness(list)
  return (
    <>
      <Section
        title="Coding agents on your runners"
        help={
          <>
            Reviews can run on your Claude Code or opencode subscription instead of a provider: add{' '}
            <b>Coding agent on my runner</b> to a review rule&apos;s models. They run read-only and
            confined to a checkout, on your own computer, and Waypoint never sees the sign-in. Only
            pull requests by project members are read this way, and only Claude Code and opencode
            can be confined so far. A rule uses the runners of whoever saved it, so this is your
            list.
          </>
        }
        className="mt-4"
        actions={
          <Button
            size="sm"
            icon={<Plus className="size-3.5" />}
            onClick={() => setConnecting((c) => !c)}
          >
            Connect a computer
          </Button>
        }
      >
        {runners.isPending ? (
          <div className="p-4">
            <Skeleton className="h-14" />
          </div>
        ) : (
          <ul className="divide-y divide-line" data-testid="coding-agents">
            {Object.entries(CLI_LABELS).map(([cli, label]) => {
              const r = ready[cli]
              return (
                <li key={cli} className="flex items-center gap-3 px-4 py-2.5" data-state={r.state}>
                  <span
                    role="img"
                    aria-label={r.state === 'ready' ? 'Ready' : 'Not ready'}
                    className={
                      r.state === 'ready'
                        ? 'size-2.5 shrink-0 rounded-full bg-ok shadow-[0_0_0_3px_color-mix(in_oklab,var(--wp-ok)_25%,transparent)]'
                        : 'size-2.5 shrink-0 rounded-full bg-subtle/50'
                    }
                  />
                  <span className="text-sm font-medium">{label}</span>
                  {!REVIEW_CLIS.includes(cli) && (
                    <span title="It can't be confined to a checkout yet, so reviews don't use it">
                      <Badge>not for reviews</Badge>
                    </span>
                  )}
                  <span className="min-w-0 flex-1 text-right text-xs text-muted">
                    {r.state === 'ready' ? (
                      <span className="inline-flex items-center gap-1">
                        <Check className="size-3.5 text-ok" />
                        signed in on {r.runner}
                      </span>
                    ) : r.state === 'signed_out' ? (
                      <>
                        signed out on {r.runner}: run{' '}
                        <code className="font-mono">{CLI_SIGN_IN[cli]}</code> there
                      </>
                    ) : (
                      'not on any of your online runners'
                    )}
                  </span>
                </li>
              )
            })}
          </ul>
        )}
      </Section>
      {connecting && <ConnectPanel />}
    </>
  )
}

function TaskRow({ task, providers }: { task: AITask; providers: AIProvider[] }) {
  const set = useSetAITask()
  const save = (primary: ChainModel | null, fallbacks: ChainModel[], note?: string) =>
    set.mutate(
      primary
        ? { task: task.task, provider_id: primary.provider_id, model: primary.model, fallbacks }
        : { task: task.task, provider_id: 0, model: '' },
      { onSuccess: () => note && toast(note), onError: toastError },
    )
  const primary = task.provider_id ? { provider_id: task.provider_id, model: task.model } : null
  return (
    <li className="px-4 py-3">
      <div className="flex flex-col gap-2 sm:flex-row sm:items-center">
        <div className="min-w-0 flex-1">
          <div className="text-[13px] font-medium">{task.label}</div>
          <div className="text-xs text-subtle">{task.description}</div>
        </div>
        {providers.length === 0 ? (
          <span className="text-xs text-subtle">Add a provider first</span>
        ) : (
          <ModelChooser
            providers={providers}
            label={`Model for ${task.label}`}
            selected={task.model}
            allowOff
            initialProvider={task.provider_id}
            onPick={(m) =>
              save(
                m,
                m ? task.fallbacks : [],
                m ? `${task.label}: ${m.model}` : `${task.label} is off`,
              )
            }
            trigger={(props) => (
              <Button {...props} size="sm" className="max-w-[16rem] justify-between">
                <span className="truncate font-mono text-xs">{task.model || 'Off'}</span>
                <ChevronDown className="size-3.5 shrink-0 text-subtle" />
              </Button>
            )}
          />
        )}
      </div>
      {primary && (
        <div className="mt-2 sm:pl-6">
          {task.fallbacks.length > 0 && (
            <div className="mb-1 text-xs text-subtle">
              If it fails (rate limit, error, timeout, a bad answer), then try:
            </div>
          )}
          <ModelChain
            value={task.fallbacks}
            start={2}
            addLabel={task.fallbacks.length ? 'Add another fallback' : 'Add a fallback model'}
            onChange={(fb) => save(primary, fb)}
          />
        </div>
      )}
      {task.task === 'review' && <ReviewNoteEditor model={task.model} />}
    </li>
  )
}

/**
 * The line every bot review starts with, which also signs its line
 * comments: how far to trust it, in the team's own words.
 */
function ReviewNoteEditor({ model }: { model: string }) {
  const qc = useQueryClient()
  const note = useQuery(reviewNoteQuery())
  const [text, setText] = useState<string | null>(null)
  const value = text ?? note.data?.note ?? ''
  const save = useMutation({
    mutationFn: (v: string) => setReviewNote(v),
    onSuccess: (n) => {
      qc.setQueryData(reviewNoteQuery().queryKey, n)
      setText(null)
      toast(n.custom ? 'Review note saved' : 'Review note back to the default')
    },
    onError: toastError,
  })
  const preview = useMemo(
    () =>
      renderMarkdown(
        renderNote(
          value.trim() ? value : (note.data?.default ?? ''),
          '`' + (model || 'opencode/muse-spark-1.3-contributor-free') + '`',
        ),
      ),
    [value, model, note.data?.default],
  )
  if (note.isPending) return <Skeleton className="mt-3 h-16 sm:ml-6" />
  if (!note.data) return null
  const changed = value.trim() !== note.data.note
  return (
    <form
      className="mt-3 space-y-2 sm:pl-6"
      onSubmit={(e) => {
        e.preventDefault()
        if (changed) save.mutate(value)
      }}
    >
      <Field
        label="Note on every review"
        note={
          <>
            Starts each review and signs its line comments, so readers know who wrote it and how far
            to trust it. <code className="font-mono">{'{models}'}</code> names the models that
            answered; Markdown works.
          </>
        }
      >
        <Input
          value={value}
          maxLength={500}
          onChange={(e) => setText(e.target.value)}
          placeholder={note.data.default}
        />
      </Field>
      <div className="flex gap-2">
        <Button
          type="submit"
          size="sm"
          variant="primary"
          disabled={!changed}
          loading={save.isPending}
        >
          Save
        </Button>
        {(note.data.custom || changed) && (
          <Button
            type="button"
            size="sm"
            variant="ghost"
            icon={<RotateCcw className="size-3.5" />}
            onClick={() => (changed ? setText(null) : save.mutate(''))}
          >
            {changed ? 'Undo' : 'Use the default'}
          </Button>
        )}
      </div>
      <div className="rounded-lg border border-line bg-sunken px-3 py-2">
        <div className="mb-1 text-[11px] font-semibold uppercase tracking-wide text-subtle">
          Preview
        </div>
        <div className="prose-wp text-[13px]" dangerouslySetInnerHTML={{ __html: preview }} />
        {!value.includes('{models}') && value.trim() && (
          <div className="mt-1 text-xs text-warn">
            Without {'{models}'}, reviews won’t say which model wrote them.
          </div>
        )}
      </div>
    </form>
  )
}

function ProviderRow({
  provider: p,
  onEdit,
  onDelete,
}: {
  provider: AIProvider
  onEdit: () => void
  onDelete: () => void
}) {
  const m = usePopover()
  return (
    <li className="flex items-center gap-3 px-4 py-3">
      <Sparkles className="size-4 shrink-0 text-subtle" />
      <div className="min-w-0 flex-1">
        <div className="flex flex-wrap items-center gap-1.5">
          <span className="truncate text-[13px] font-medium">{p.name}</span>
          <Badge>{KIND[p.kind]?.label ?? p.kind}</Badge>
          {p.has_key && <Badge color="var(--wp-ok)">{KIND[p.kind]?.secret ?? 'key'} set</Badge>}
        </div>
        <div className="mt-0.5 truncate font-mono text-xs text-subtle">{p.base_url}</div>
      </div>
      <IconButton
        ref={m.anchor}
        label={`Actions for ${p.name}`}
        onClick={m.toggle}
        icon={<MoreHorizontal className="size-4" />}
      />
      <Popover open={m.open} onClose={m.close} anchor={m.anchor} align="end">
        <MenuList
          onClose={m.close}
          items={[
            { label: 'Edit', icon: <Pencil />, onSelect: onEdit },
            { label: 'Test connection', icon: <PlugZap />, onSelect: () => testProvider(p) },
            { separator: true, label: '' },
            { label: 'Remove…', icon: <Trash2 />, danger: true, onSelect: onDelete },
          ]}
        />
      </Popover>
    </li>
  )
}

function ProviderDialog({
  provider,
  onClose,
}: {
  provider: AIProvider | 'new' | null
  onClose: () => void
}) {
  const qc = useQueryClient()
  const existing = provider && provider !== 'new' ? provider : null
  const [kind, setKind] = useState<ProviderKind>('opencode')
  const [name, setName] = useState('')
  const [baseURL, setBaseURL] = useState('')
  const [key, setKey] = useState('')
  useEffect(() => {
    if (!provider) return
    setKind(existing?.kind ?? 'opencode')
    setName(existing?.name ?? '')
    setBaseURL(existing?.base_url ?? '')
    setKey('')
  }, [provider]) // eslint-disable-line react-hooks/exhaustive-deps
  const save = useMutation({
    mutationFn: () => {
      const body: Record<string, string> = { kind, name: name.trim(), base_url: baseURL.trim() }
      if (key.trim() || !existing) body.api_key = key.trim()
      return existing
        ? api.patch<AIProvider>(`/ai/providers/${existing.id}`, body)
        : api.post<AIProvider>('/ai/providers', body)
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['ai'] })
      onClose()
    },
    onError: toastError,
  })
  const k = KIND[kind]
  return (
    <Dialog
      open={!!provider}
      onClose={onClose}
      title={existing ? `Edit ${existing.name}` : 'Add an AI provider'}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button
            variant="primary"
            disabled={!baseURL.trim()}
            loading={save.isPending}
            onClick={() => save.mutate()}
          >
            {existing ? 'Save' : 'Add'}
          </Button>
        </>
      }
    >
      <form
        className="space-y-4 p-4"
        onSubmit={(e) => {
          e.preventDefault()
          if (baseURL.trim()) save.mutate()
        }}
      >
        <div className="grid gap-4 sm:grid-cols-2">
          <Field label="Kind">
            <Select value={kind} onChange={(e) => setKind(e.target.value as ProviderKind)}>
              {(Object.keys(KIND) as ProviderKind[]).map((id) => (
                <option key={id} value={id}>
                  {KIND[id].option}
                </option>
              ))}
            </Select>
          </Field>
          <Field label="Name" hint="Defaults to the host name.">
            <Input value={name} onChange={(e) => setName(e.target.value)} />
          </Field>
        </div>
        <Field label="URL" hint={k.urlHint}>
          <Input
            type="url"
            value={baseURL}
            onChange={(e) => setBaseURL(e.target.value)}
            placeholder={k.placeholder}
          />
        </Field>
        <Field
          label={k.secretLabel}
          hint={existing?.has_key ? 'Set. Leave blank to keep it.' : k.secretHint}
        >
          <Input
            type="password"
            value={key}
            onChange={(e) => setKey(e.target.value)}
            autoComplete="off"
            className="font-mono"
          />
        </Field>
      </form>
    </Dialog>
  )
}

/** What each provider kind is called and asks for. */
const KIND: Record<
  ProviderKind,
  {
    label: string
    option: string
    secret: string
    secretLabel: string
    secretHint: string
    urlHint: string
    placeholder: string
  }
> = {
  opencode: {
    label: 'opencode',
    option: 'opencode server',
    secret: 'password',
    secretLabel: 'Server password',
    secretHint: 'The server’s OPENCODE_SERVER_PASSWORD, if it has one.',
    urlHint: 'Where `opencode serve` listens, e.g. http://opencode:4096',
    placeholder: 'http://opencode:4096',
  },
  ollama: {
    label: 'Ollama',
    option: 'Ollama (local or remote)',
    secret: 'token',
    secretLabel: 'Bearer token',
    secretHint: 'Only for a proxy in front of a remote Ollama; a local one needs none.',
    urlHint: 'The Ollama server, e.g. http://localhost:11434 (not its /v1).',
    placeholder: 'http://localhost:11434',
  },
  openai: {
    label: 'OpenAI-compatible',
    option: 'OpenAI-compatible API',
    secret: 'key',
    secretLabel: 'API key',
    secretHint: 'Optional for local servers like LM Studio.',
    urlHint: 'The API base, e.g. https://openrouter.ai/api/v1',
    placeholder: 'https://openrouter.ai/api/v1',
  },
}

/** Lists a provider's models: it answers, and says how many it offers. */
function testProvider(p: AIProvider) {
  api
    .get<{ id: string }[]>(`/ai/providers/${p.id}/models`)
    .then((ms) =>
      toast(
        ms.length
          ? `${p.name} answered: ${ms.length} model${ms.length === 1 ? '' : 's'}`
          : `${p.name} answered, but offers no models yet`,
      ),
    )
    .catch(toastError)
}
