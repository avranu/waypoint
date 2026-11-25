import { useEffect, useMemo, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import { Link, useNavigate, useParams } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import clsx from 'clsx'
import {
  AlarmClock,
  ArrowLeft,
  Bot,
  Check,
  CheckCircle2,
  ChevronRight,
  GitBranch,
  GitPullRequest,
  HardDrive,
  MessageSquareText,
  Play,
  X,
} from 'lucide-react'
import { ApiError } from '@/lib/api'
import { dateTime, duration, relativeTime } from '@/lib/format'
import { renderMarkdown } from '@/lib/markdown'
import { refsQuery } from '@/lib/knowledge'
import {
  kindLabel,
  requestQuery,
  stateLabel,
  useDecide,
  useRunCell,
  useSnooze,
  type Answer,
  type CellRun,
  type Question,
  type RequestCell,
  type WRequest,
} from '@/lib/requests'
import { Button, Spinner } from '@/components/ui/button'
import { Textarea } from '@/components/ui/input'
import { Avatar, Badge, Empty, Skeleton } from '@/components/ui/misc'
import { MenuList, Popover, usePopover } from '@/components/ui/popover'
import { useEscapeLayer } from '@/components/ui/layers'
import { DiffFile } from '@/components/Diff'
import { requestIcon } from '@/components/request-icons'
import { Attachments } from '@/components/attachments/Attachments'
import { RefusedBefore } from '@/pages/agents/Refused'
import { pickedRule, RuleChoice, ruleNone, ruleOwn } from '@/components/RuleChoice'

const toneClass = {
  open: 'bg-accent-soft text-fg',
  ok: 'bg-ok/15 text-ok',
  bad: 'bg-danger/15 text-danger',
  muted: 'bg-sunken text-muted',
  busy: 'bg-warn/15 text-warn',
}

export function StateBadge({ r }: { r: Pick<WRequest, 'state' | 'kind' | 'via'> }) {
  const s = stateLabel(r)
  return (
    <span
      className={clsx(
        'inline-flex h-5 shrink-0 items-center gap-1 rounded-md px-1.5 text-[11px] font-medium',
        toneClass[s.tone],
      )}
    >
      {s.tone === 'busy' && <Spinner className="size-3" />}
      {s.text}
    </span>
  )
}

/** /requests/$code */
export function RequestPage() {
  const { code } = useParams({ strict: false }) as { code: string }
  useEffect(() => {
    document.title = `${code} · Waypoint`
  }, [code])
  return (
    <div className="flex-1 overflow-y-auto">
      <RequestView key={code} code={code} mode="page" />
    </div>
  )
}

/** The request as a side sheet over the current page. */
export function RequestSheet({ code, onClose }: { code: string; onClose: () => void }) {
  useEscapeLayer(true, () => {
    const t = document.activeElement as HTMLElement | null
    if (t && (t.isContentEditable || ['INPUT', 'TEXTAREA', 'SELECT'].includes(t.tagName))) {
      t.blur()
      return false
    }
    return onClose()
  })
  return createPortal(
    <div className="fixed inset-0 z-40 flex justify-end">
      <div className="absolute inset-0 bg-black/20 md:bg-black/10" onClick={onClose} />
      <div
        role="dialog"
        aria-label={`Request ${code}`}
        className="animate-sheet relative h-full w-full overflow-y-auto border-l border-line bg-panel shadow-pop md:w-[min(720px,calc(100vw-80px))]"
      >
        <RequestView key={code} code={code} mode="sheet" onClose={onClose} />
      </div>
    </div>,
    document.body,
  )
}

const active = (r?: WRequest) =>
  !!r?.cells?.some((c) => c.runs.some((x) => x.state === 'queued' || x.state === 'running')) ||
  r?.state === 'applying'

export function RequestView({
  code,
  mode,
  onClose,
}: {
  code: string
  mode: 'page' | 'sheet'
  onClose?: () => void
}) {
  const navigate = useNavigate()
  const { data: r, error } = useQuery({
    ...requestQuery(code),
    refetchInterval: (query) => (active(query.state.data) ? 2500 : false),
  })
  const back = () => (onClose ? onClose() : navigate({ to: '/inbox' }))
  if (error instanceof ApiError && error.status === 404)
    return (
      <div className="mx-auto max-w-3xl p-4">
        <Back mode={mode} onClick={back} />
        <Empty icon={<MessageSquareText />} title="Request not found">
          It may belong to a project you can't edit, or the reference is wrong.
        </Empty>
      </div>
    )
  if (!r)
    return (
      <div className="mx-auto max-w-3xl space-y-3 p-4 sm:p-6">
        <Skeleton className="h-6 w-2/3" />
        <Skeleton className="h-4 w-1/2" />
        <Skeleton className="h-32 w-full" />
      </div>
    )
  return <RequestBody r={r} mode={mode} back={back} />
}

function Back({ mode, onClick }: { mode: 'page' | 'sheet'; onClick: () => void }) {
  return (
    <button
      onClick={onClick}
      className="mb-2 inline-flex items-center gap-1 text-[13px] text-muted hover:text-fg"
    >
      {mode === 'page' ? <ArrowLeft className="size-4" /> : <X className="size-4" />}
      {mode === 'page' ? 'Inbox' : 'Close'}
    </button>
  )
}

function RequestBody({ r, mode, back }: { r: WRequest; mode: 'page' | 'sheet'; back: () => void }) {
  const refs = useQuery({
    ...refsQuery(r.body ?? '', r.project?.key),
    enabled: !!r.body,
    placeholderData: (p) => p,
  }).data
  const html = useMemo(
    () => (r.body ? renderMarkdown(r.body, { project: r.project?.key, refs }) : ''),
    [r.body, r.project?.key, refs],
  )
  const open = r.state === 'open'
  const canAct = open && r.can_decide
  return (
    <div className="mx-auto max-w-3xl px-4 pb-32 pt-4 sm:px-6 sm:pt-5">
      <Back mode={mode} onClick={back} />
      <header className="space-y-2">
        <div className="flex flex-wrap items-center gap-2 text-xs text-muted">
          <span className="flex items-center gap-1 [&>svg]:size-3.5">
            {requestIcon[r.kind]}
            {kindLabel[r.kind]}
          </span>
          <span className="font-mono">{r.code}</span>
          {r.project && <Badge>{r.project.key}</Badge>}
          <StateBadge r={r} />
          {r.auto && (
            <span title="Filed automatically from the end of the agent's turn">
              <Badge>auto</Badge>
            </span>
          )}
          {r.snoozed_until && new Date(r.snoozed_until) > new Date() && (
            <Badge className="gap-1">
              <AlarmClock className="size-3" />
              Snoozed until {dateTime(r.snoozed_until)}
            </Badge>
          )}
        </div>
        <h1 className="text-xl font-semibold leading-snug tracking-tight">{r.title}</h1>
        <Context r={r} />
      </header>

      {html && (
        <div className="prose-wp mt-4 text-[14px]" dangerouslySetInnerHTML={{ __html: html }} />
      )}

      <div className="mt-5 space-y-4">
        {r.kind === 'question' && <Questions r={r} />}
        {r.kind === 'change' && <Change r={r} />}
        {r.kind === 'permission' && <Permission r={r} />}
        {(r.kind === 'command' || r.kind === 'action') && !!r.cells?.length && <Cells r={r} />}
      </div>

      {!!r.watches?.length && (
        <section className="mt-5 text-[13px] text-muted" aria-label="Closes when">
          {r.watches.map((w) => (
            <p key={w.summary}>
              {w.summary}
              {w.state === 'satisfied'
                ? ' (satisfied)'
                : w.note
                  ? ` (last check: ${w.note})`
                  : ' (not checked yet)'}
            </p>
          ))}
        </section>
      )}

      <RefusedBefore list={r.refused} />
      <Outcome r={r} />
      <div className="mt-5">
        <Attachments target={`request:${r.id}`} editable={r.can_decide} />
      </div>
      {!open && r.state !== 'applying' && <Decided r={r} />}
      {open && !r.can_decide && (
        <p className="mt-6 rounded-lg border border-line bg-sunken px-3 py-2 text-[13px] text-muted">
          {r.user.display_name} or an editor of the project decides this. Agents and read-only
          tokens can't.
        </p>
      )}
      {(canAct || (r.state === 'failed' && r.kind === 'change' && r.can_decide)) && (
        <Actions r={r} />
      )}
    </div>
  )
}

/** Who asked, from where, about what. */
function Context({ r }: { r: WRequest }) {
  const s = r.session
  return (
    <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-[13px] text-muted">
      {r.agent && (
        <span className="flex items-center gap-1">
          <Bot className="size-3.5" />
          {r.agent}
        </span>
      )}
      {s && (
        <Link
          to="/agents/sessions/$id"
          params={{ id: String(s.id) }}
          className="flex min-w-0 items-center gap-1 hover:text-fg hover:underline"
          title="The agent session this came from"
        >
          <HardDrive className="size-3.5 shrink-0" />
          <span className="truncate">
            {s.machine}
            {s.repo && ` · ${s.repo}`}
          </span>
          {s.branch && (
            <span className="flex shrink-0 items-center gap-0.5">
              <GitBranch className="size-3" />
              {s.branch}
            </span>
          )}
        </Link>
      )}
      {r.issue && (
        <Link
          to="/issue/$key"
          params={{ key: r.issue.key }}
          className="hover:text-fg hover:underline"
        >
          {r.issue.key} {r.issue.title}
        </Link>
      )}
      <span className="flex items-center gap-1" title={dateTime(r.created_at)}>
        <Avatar user={r.user} size={16} />
        for {r.user.display_name} · {relativeTime(r.created_at)}
      </span>
    </div>
  )
}

function Questions({ r }: { r: WRequest }) {
  const qs = r.questions ?? []
  const decide = useDecide(r.ref)
  const [answers, setAnswers] = useState<Answer[]>(() =>
    qs.map((q) => ({ choices: q.default ? [q.default] : [], text: '' })),
  )
  const [note, setNote] = useState('')
  const canAnswer = r.state === 'open' && r.can_decide
  const ready = answers.every((a) => (a.choices?.length ?? 0) > 0 || !!a.text?.trim())
  const set = (i: number, a: Answer) => setAnswers(answers.map((x, n) => (n === i ? a : x)))
  const shown = canAnswer ? answers : (r.answers ?? [])
  const send = () =>
    decide.mutate({
      decision: 'answer',
      answers: answers.map((a) => ({ choices: a.choices, text: a.text?.trim() })),
      note: note.trim() || undefined,
    })
  return (
    <div className="space-y-5">
      {qs.map((q, i) => (
        <QuestionBlock
          key={i}
          q={q}
          index={qs.length > 1 ? i + 1 : 0}
          title={r.title}
          answer={shown[i] ?? {}}
          editable={canAnswer}
          onChange={(a) => set(i, a)}
          onSubmit={qs.length === 1 && ready ? send : undefined}
        />
      ))}
      {canAnswer && (
        <>
          <NoteField value={note} onChange={setNote} />
          <div className="flex flex-wrap items-center gap-2">
            <Button
              variant="primary"
              disabled={!ready}
              loading={decide.isPending}
              onClick={send}
              icon={<Check className="size-4" />}
            >
              Send answer
            </Button>
            {r.auto && <NotAQuestionButton r={r} note={note} />}
            <DismissButton r={r} note={note} />
            <SnoozeButton r={r} />
          </div>
        </>
      )}
    </div>
  )
}

function QuestionBlock({
  q,
  index,
  title,
  answer,
  editable,
  onChange,
  onSubmit,
}: {
  q: Question
  index: number
  title: string
  answer: Answer
  editable: boolean
  onChange: (a: Answer) => void
  onSubmit?: () => void
}) {
  const picked = answer.choices ?? []
  const toggle = (label: string) => {
    if (!editable) return
    if (q.multi)
      onChange({
        ...answer,
        choices: picked.includes(label) ? picked.filter((x) => x !== label) : [...picked, label],
      })
    else onChange({ ...answer, choices: picked[0] === label ? [] : [label] })
  }
  const showQuestion = index > 0 || q.header || q.question.trim() !== title.trim()
  return (
    <fieldset className="space-y-2">
      {showQuestion && (
        <legend className="mb-1 text-[14px] font-medium">
          {q.header && <span className="mr-2 text-xs font-normal text-subtle">{q.header}</span>}
          {index > 0 && <span className="text-subtle">{index}. </span>}
          {q.question}
        </legend>
      )}
      {!!q.choices?.length && (
        <div
          role={q.multi ? 'group' : 'radiogroup'}
          aria-label={q.multi ? 'Pick any' : 'Pick one'}
          className="grid gap-1.5"
        >
          {q.choices.map((c) => {
            const on = picked.includes(c.label)
            return (
              <button
                key={c.label}
                type="button"
                role={q.multi ? 'checkbox' : 'radio'}
                aria-checked={on}
                disabled={!editable}
                onClick={() => toggle(c.label)}
                className={clsx(
                  'flex items-start gap-3 rounded-lg border px-3 py-2.5 text-left transition-colors',
                  on ? 'border-accent/60 bg-accent-soft' : 'border-line bg-panel',
                  editable && !on && 'hover:bg-hover',
                  !editable && !on && 'opacity-60',
                )}
              >
                <span
                  className={clsx(
                    'mt-0.5 flex size-4 shrink-0 items-center justify-center border',
                    q.multi ? 'rounded' : 'rounded-full',
                    on ? 'border-accent bg-accent text-accent-fg' : 'border-line-strong',
                  )}
                >
                  {on && <Check className="size-3" />}
                </span>
                <span className="min-w-0">
                  <span className="block text-[14px]">{c.label}</span>
                  {c.description && (
                    <span className="block text-[13px] text-muted">{c.description}</span>
                  )}
                </span>
              </button>
            )
          })}
        </div>
      )}
      {editable ? (
        <Textarea
          autoGrow
          rows={q.choices?.length ? 2 : 3}
          value={answer.text ?? ''}
          onChange={(e) => onChange({ ...answer, text: e.target.value })}
          onKeyDown={(e) => {
            if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) {
              e.preventDefault()
              onSubmit?.()
            }
          }}
          aria-label={q.choices?.length ? 'Or write your own answer' : 'Your answer'}
          placeholder={q.choices?.length ? 'Or write your own answer…' : 'Your answer…'}
        />
      ) : (
        answer.text && (
          <p className="whitespace-pre-wrap rounded-lg bg-sunken px-3 py-2 text-[14px]">
            {answer.text}
          </p>
        )
      )}
    </fieldset>
  )
}

function Change({ r }: { r: WRequest }) {
  const files = r.files ?? []
  const adds = files.reduce((n, f) => n + f.additions, 0)
  const dels = files.reduce((n, f) => n + f.deletions, 0)
  return (
    <section className="space-y-2">
      <div className="flex items-center justify-between text-[13px] text-muted">
        <span>
          {files.length} file{files.length === 1 ? '' : 's'} ·{' '}
          <span className="text-ok">+{adds}</span> <span className="text-danger">−{dels}</span>
        </span>
        {r.message && r.message !== r.title && (
          <span className="truncate" title="Commit message">
            “{r.message.split('\n')[0]}”
          </span>
        )}
      </div>
      {files.map((f) => (
        <DiffFile
          key={f.path}
          path={f.path}
          additions={f.additions}
          deletions={f.deletions}
          diff={f.diff ?? ''}
          status={f.new ? 'added' : f.delete ? 'deleted' : 'modified'}
          defaultOpen={files.length <= 3}
        />
      ))}
    </section>
  )
}

/** How an approved change gets applied; shown with the buttons. */
function ApplyPath({
  r,
  value,
  onChange,
}: {
  r: WRequest
  value: 'runner' | 'pr'
  onChange: (v: 'runner' | 'pr') => void
}) {
  const machine = r.runner?.machine ?? r.session?.machine ?? 'the agent’s machine'
  const online = !!r.runner?.online
  const outside = !!r.files?.[0]?.outside
  const hasWorktree = outside ? !!r.session : !!r.session?.cwd
  const canPR = !!r.project && !outside
  const opts: { v: 'runner' | 'pr'; title: string; hint: string; off?: string }[] = [
    {
      v: 'runner',
      title: `Apply on ${machine}`,
      hint: outside
        ? 'Your runner writes just these files, if it is allowed to write in their directory. Nothing is committed.'
        : 'Your runner writes the files in the session’s worktree and commits them there.',
      off: !hasWorktree
        ? outside
          ? 'This request doesn’t say which machine asked.'
          : 'This request has no worktree.'
        : !online
          ? `No runner online on ${machine}; connect one or open a PR instead.`
          : undefined,
    },
    {
      v: 'pr',
      title: 'Open a pull request',
      hint: 'Commits to a new branch through the forge. Works with protected branches.',
      off: outside
        ? 'These files are outside any git repository, so there is no pull request.'
        : !canPR
          ? 'Needs a project with the repository attached.'
          : undefined,
    },
  ]
  return (
    <div role="radiogroup" aria-label="How to apply" className="grid gap-1.5 sm:grid-cols-2">
      {opts.map((o) => {
        const on = value === o.v
        return (
          <button
            key={o.v}
            type="button"
            role="radio"
            aria-checked={on}
            disabled={!!o.off}
            onClick={() => onChange(o.v)}
            className={clsx(
              'flex items-start gap-2.5 rounded-lg border px-3 py-2 text-left disabled:cursor-not-allowed',
              on ? 'border-accent/60 bg-accent-soft' : 'border-line bg-panel',
              !on && !o.off && 'hover:bg-hover',
              o.off && 'opacity-60',
            )}
          >
            <span
              className={clsx(
                'mt-0.5 flex size-4 shrink-0 items-center justify-center rounded-full border',
                on ? 'border-accent bg-accent text-accent-fg' : 'border-line-strong',
              )}
            >
              {on && <Check className="size-3" />}
            </span>
            <span className="min-w-0 text-[13px]">
              <span className="block font-medium">{o.title}</span>
              <span className="block text-muted">{o.off ?? o.hint}</span>
            </span>
          </button>
        )
      })}
    </div>
  )
}

function Permission({ r }: { r: WRequest }) {
  const t = r.tool
  if (!t) return null
  const input = t.input === undefined ? '' : JSON.stringify(t.input, null, 2)
  return (
    <section className="space-y-2">
      <div className="flex items-center gap-2 text-[13px]">
        <Badge className="font-mono">{t.name}</Badge>
        <span className="text-muted">was refused</span>
      </div>
      {input && input !== '{}' && (
        <pre className="max-h-64 overflow-auto rounded-lg border border-line bg-sunken p-3 font-mono text-[12px] leading-5">
          {input}
        </pre>
      )}
      {t.reason && <p className="text-[13px] text-muted">{t.reason}</p>}
      {r.state === 'approved' && !r.result.rule && (
        <p className="text-[13px] text-muted">
          No rule was added, so auto mode still decides this call.
          {r.result.reason && <> {r.result.reason}</>}
        </p>
      )}
      {r.result.rule && <RuleOutcome r={r} />}
    </section>
  )
}

function RuleOutcome({ r }: { r: WRequest }) {
  const res = r.result
  return (
    <div className="flex flex-wrap items-center gap-x-2 gap-y-1 rounded-lg border border-line bg-sunken px-3 py-2 text-[13px]">
      <span>Rule</span>
      <code className="font-mono text-[12px]">{res.rule}</code>
      {res.pr_url ? (
        <a
          href={res.pr_url}
          target="_blank"
          rel="noreferrer noopener"
          className="inline-flex items-center gap-1 font-medium underline"
        >
          <GitPullRequest className="size-3.5" />
          {res.pr_title || 'the pull request'}
        </a>
      ) : (
        <span className="text-muted">{res.reason}</span>
      )}
    </div>
  )
}

function Cells({ r }: { r: WRequest }) {
  return (
    <section className="space-y-3" aria-label="Commands">
      {r.cells!.map((c) => (
        <Cell key={c.n} r={r} cell={c} />
      ))}
      {r.state === 'open' && r.can_decide && r.runner && !r.runner.online && (
        <p className="text-[13px] text-muted">
          No runner online on {r.runner.machine}; connect one, or run these yourself and mark the
          request done.
        </p>
      )}
    </section>
  )
}

function Cell({ r, cell }: { r: WRequest; cell: RequestCell }) {
  const run = useRunCell(r.ref)
  const latest = cell.runs[0]
  // A reload shows the command last run, which is what the person edited it to.
  const [cmd, setCmd] = useState(latest?.command ?? cell.command)
  // A run that lands while the page is open (another tab, another person, this one) is what the
  // editor shows, unless the person has typed over what it showed: their unsent text stays.
  const latestId = latest?.id
  const seenRun = useRef(latestId)
  const shown = useRef(latest?.command ?? cell.command)
  useEffect(() => {
    if (latestId === seenRun.current) return
    seenRun.current = latestId
    if (!latest) return
    const was = shown.current
    shown.current = latest.command
    setCmd((c) => (c === was ? latest.command : c))
  }, [latestId, latest])
  const busy = latest?.state === 'queued' || latest?.state === 'running' || run.isPending
  const canRun =
    (r.state === 'open' || (r.state === 'failed' && r.kind === 'command')) && r.can_decide
  const edited = cmd !== cell.command
  return (
    <div className="overflow-hidden rounded-xl border border-line bg-panel">
      <div className="flex items-start gap-2 p-2.5">
        <span aria-hidden className="mt-2 select-none font-mono text-[13px] text-subtle">
          $
        </span>
        <Textarea
          autoGrow
          rows={1}
          spellCheck={false}
          readOnly={!canRun}
          value={cmd}
          aria-label={`Command ${cell.n}`}
          onChange={(e) => setCmd(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === 'Enter' && (e.metaKey || e.ctrlKey) && canRun && !busy && cmd.trim()) {
              e.preventDefault()
              run.mutate({ n: cell.n, command: cmd })
            }
          }}
          className="min-w-0 flex-1 !border-transparent !bg-sunken font-mono !text-[13px] !shadow-none"
        />
        {canRun && (
          <Button
            size="sm"
            variant={latest ? 'secondary' : 'primary'}
            loading={busy}
            disabled={!cmd.trim()}
            icon={<Play className="size-3.5" />}
            onClick={() => run.mutate({ n: cell.n, command: cmd })}
            className="mt-0.5 shrink-0"
          >
            {latest ? 'Run again' : 'Run'}
          </Button>
        )}
      </div>
      <div className="flex flex-wrap items-center gap-x-3 gap-y-0.5 px-3 pb-2 text-xs text-subtle">
        {cell.dir ? (
          <span className="font-mono">in {cell.dir}</span>
        ) : (
          r.session?.cwd && <span className="font-mono">in {r.session.cwd}</span>
        )}
        {cell.machine && <span>on {cell.machine}</span>}
        {edited && canRun && (
          <button className="underline" onClick={() => setCmd(cell.command)}>
            Reset to the agent’s command
          </button>
        )}
        {/* Read-only, the editor shows the command that ran: the agent's is still said. */}
        {edited && !canRun && (
          <span className="whitespace-pre-wrap break-all font-mono" title="The agent’s command">
            agent’s: {cell.command}
          </span>
        )}
      </div>
      {latest && <RunOutput run={latest} />}
      {cell.runs.length > 1 && <History runs={cell.runs.slice(1)} />}
    </div>
  )
}

function RunOutput({ run }: { run: CellRun }) {
  const ok = run.state === 'done' && (run.exit_code ?? 0) === 0
  const running = run.state === 'queued' || run.state === 'running'
  const ref = useRef<HTMLPreElement>(null)
  useEffect(() => {
    const el = ref.current
    if (el) el.scrollTop = el.scrollHeight
  }, [run.output])
  return (
    <div className="border-t border-line">
      <div className="flex flex-wrap items-center gap-2 px-3 py-1.5 text-xs">
        {running ? (
          <span className="flex items-center gap-1.5 text-warn">
            <Spinner className="size-3" />
            {run.state === 'queued' ? 'Waiting for the runner…' : 'Running…'}
          </span>
        ) : (
          <span
            className={clsx(
              'inline-flex h-5 items-center rounded px-1.5 font-medium',
              ok ? 'bg-ok/15 text-ok' : 'bg-danger/15 text-danger',
            )}
          >
            {run.exit_code !== undefined ? `exit ${run.exit_code}` : run.state}
          </span>
        )}
        {run.duration_ms ? (
          <span className="text-subtle">{duration(run.duration_ms / 1000)}</span>
        ) : null}
        {run.finished_at && (
          <span className="text-subtle" title={dateTime(run.finished_at)}>
            {relativeTime(run.finished_at)}
          </span>
        )}
        {run.run_by && <span className="text-subtle">by {run.run_by.display_name}</span>}
        {run.truncated && <span className="text-subtle">Output cut to its last 256 KB</span>}
      </div>
      {run.state === 'queued' && run.waiting && (
        <p className="px-3 pb-2 text-xs text-muted">{run.waiting}</p>
      )}
      {run.error && <p className="px-3 pb-2 text-[13px] text-danger">{run.error}</p>}
      {run.output ? (
        <pre
          ref={ref}
          tabIndex={0}
          aria-label="Output"
          className="max-h-72 overflow-auto whitespace-pre-wrap break-words bg-sunken px-3 py-2 font-mono text-[12px] leading-5"
        >
          {run.output}
        </pre>
      ) : (
        !running &&
        !run.error && <p className="px-3 pb-2 text-xs text-subtle">It printed nothing.</p>
      )}
    </div>
  )
}

function History({ runs }: { runs: CellRun[] }) {
  const [open, setOpen] = useState(false)
  const [which, setWhich] = useState<number | null>(null)
  return (
    <div className="border-t border-line">
      <button
        aria-expanded={open}
        onClick={() => setOpen(!open)}
        className="flex w-full items-center gap-1 px-3 py-1.5 text-left text-xs text-muted hover:bg-hover"
      >
        <ChevronRight className={clsx('size-3.5 transition-transform', open && 'rotate-90')} />
        {runs.length} earlier run{runs.length === 1 ? '' : 's'}
      </button>
      {open && (
        <ul>
          {runs.map((x) => (
            <li key={x.id} className="border-t border-line/60">
              <button
                aria-expanded={which === x.id}
                onClick={() => setWhich(which === x.id ? null : x.id)}
                className="flex w-full items-center gap-2 px-3 py-1.5 text-left text-xs hover:bg-hover"
              >
                <span
                  className={clsx(
                    'font-medium',
                    x.state === 'done' && (x.exit_code ?? 0) === 0 ? 'text-ok' : 'text-danger',
                  )}
                >
                  {x.exit_code !== undefined ? `exit ${x.exit_code}` : x.state}
                </span>
                <span className="min-w-0 flex-1 truncate font-mono text-muted">{x.command}</span>
                <span className="shrink-0 text-subtle">{relativeTime(x.created_at)}</span>
              </button>
              {which === x.id && <RunOutput run={x} />}
            </li>
          ))}
        </ul>
      )}
    </div>
  )
}

/** What happened to an approved change. */
function Outcome({ r }: { r: WRequest }) {
  if (r.state === 'applying')
    return (
      <p className="mt-5 flex items-center gap-2 rounded-lg border border-line bg-sunken px-3 py-2 text-[13px]">
        <Spinner className="size-4" />
        Applying
        {r.apply_path === 'runner' && r.session ? ` on ${r.session.machine}` : ''}…
      </p>
    )
  if (r.state === 'applied' && r.result.no_change)
    return (
      <div className="mt-5 rounded-lg border border-warn/40 bg-warn/10 px-3 py-2 text-[13px]">
        <div className="font-medium">Approved, but it changed nothing</div>
        <p className="text-muted">
          Every file already had the requested content, so no commit or pull request was made.
        </p>
      </div>
    )
  if (r.state === 'applied')
    return (
      <div className="mt-5 flex flex-wrap items-center gap-2 rounded-lg border border-ok/40 bg-ok/10 px-3 py-2 text-[13px]">
        <CheckCircle2 className="size-4 text-ok" />
        {r.result.pr_url ? (
          <>
            Opened{' '}
            <a
              href={r.result.pr_url}
              target="_blank"
              rel="noreferrer noopener"
              className="inline-flex items-center gap-1 font-medium underline"
            >
              <GitPullRequest className="size-3.5" />
              {r.result.pr_title || 'the pull request'}
            </a>
            <span className="text-muted">
              on <code className="font-mono">{r.result.branch}</code>
            </span>
          </>
        ) : (
          <>
            Applied
            {r.session ? ` on ${r.session.machine}` : ''}
            {r.result.commit && (
              <>
                {' '}
                as commit <code className="font-mono">{r.result.commit.slice(0, 8)}</code>
              </>
            )}
          </>
        )}
      </div>
    )
  if (r.state === 'conflict')
    return (
      <div className="mt-5 rounded-lg border border-danger/40 bg-danger/10 px-3 py-2 text-[13px]">
        <div className="font-medium">The files changed since the agent made this change</div>
        <p className="text-muted">
          Nothing was written.
          {r.result.conflicts?.length ? ` Changed: ${r.result.conflicts.join(', ')}.` : ''} The
          agent has to make the change again on the current files.
        </p>
      </div>
    )
  if (r.state === 'failed' && r.kind === 'change')
    return (
      <div className="mt-5 rounded-lg border border-danger/40 bg-danger/10 px-3 py-2 text-[13px]">
        <div className="font-medium">Applying failed</div>
        <p className="text-muted">{r.error || 'No reason was given.'}</p>
      </div>
    )
  return null
}

/** Who decided and what they said. */
function Decided({ r }: { r: WRequest }) {
  if (!r.decided_at && r.state !== 'withdrawn') return null
  return (
    <div className="mt-5 flex items-start gap-2 text-[13px] text-muted">
      {r.decided_by && <Avatar user={r.decided_by} size={20} />}
      <div>
        {r.state === 'withdrawn' ? (
          <span>The agent withdrew this.</span>
        ) : r.state === 'expired' ? (
          <span title={r.decided_at ? dateTime(r.decided_at) : ''}>
            Expired {relativeTime(r.decided_at)}, so it no longer waits on you. {r.result.reason}
          </span>
        ) : r.via === 'terminal' ? (
          <span>Answered in the agent’s terminal {relativeTime(r.decided_at)}.</span>
        ) : (
          <span title={r.decided_at ? dateTime(r.decided_at) : ''}>
            {r.decided_by?.display_name ?? 'Someone'}{' '}
            {r.decision === 'answer'
              ? 'answered'
              : r.decision === 'approve'
                ? 'approved'
                : r.decision === 'deny'
                  ? 'denied'
                  : r.decision === 'done'
                    ? 'marked it done'
                    : r.decision === 'ran'
                      ? `ran it and it failed (${r.error || 'no exit code'}), so it went back to the agent`
                      : 'dismissed it'}{' '}
            {relativeTime(r.decided_at)}
            {r.quiet
              ? '; the agent isn’t told, since there was no note for it.'
              : r.delivered_at
                ? '; the agent has been told.'
                : '; the agent will be told when it next stops.'}
          </span>
        )}
        {r.note && <p className="mt-1 whitespace-pre-wrap text-fg">{r.note}</p>}
      </div>
    </div>
  )
}

function NoteField({ value, onChange }: { value: string; onChange: (v: string) => void }) {
  const [open, setOpen] = useState(false)
  if (!open && !value)
    return (
      <button className="text-[13px] text-muted underline" onClick={() => setOpen(true)}>
        Add a note for the agent
      </button>
    )
  return (
    <Textarea
      autoGrow
      rows={2}
      autoFocus={open}
      value={value}
      onChange={(e) => onChange(e.target.value)}
      aria-label="Note for the agent"
      placeholder="A note for the agent…"
    />
  )
}

function DismissButton({ r, note }: { r: WRequest; note?: string }) {
  const decide = useDecide(r.ref)
  return (
    <Button
      variant="ghost"
      loading={decide.isPending && decide.variables?.decision === 'dismiss'}
      title={
        note?.trim()
          ? 'No answer needed; the agent gets your note'
          : 'No answer needed; the agent isn’t told unless you add a note, or it is waiting on this request'
      }
      onClick={() => decide.mutate({ decision: 'dismiss', note: note?.trim() || undefined })}
    >
      Dismiss
    </Button>
  )
}

/** Dismisses an auto-filed question that wasn't one; the dismissal is counted. */
function NotAQuestionButton({ r, note }: { r: WRequest; note?: string }) {
  const decide = useDecide(r.ref)
  return (
    <Button
      variant="ghost"
      loading={decide.isPending && decide.variables?.not_a_question}
      onClick={() =>
        decide.mutate({
          decision: 'dismiss',
          note: note?.trim() || undefined,
          not_a_question: true,
        })
      }
    >
      Not a question
    </Button>
  )
}

const SNOOZES = [
  { label: 'For an hour', minutes: 60 },
  { label: 'Until this evening', minutes: 0, evening: true },
  { label: 'Until tomorrow morning', minutes: 0, tomorrow: true },
  { label: 'For a week', minutes: 7 * 24 * 60 },
]

function minutesUntil(s: (typeof SNOOZES)[number]): number {
  if (s.minutes) return s.minutes
  const d = new Date()
  if (s.evening) d.setHours(18, 0, 0, 0)
  else {
    d.setDate(d.getDate() + 1)
    d.setHours(9, 0, 0, 0)
  }
  return Math.max(15, Math.round((d.getTime() - Date.now()) / 60_000))
}

function SnoozeButton({ r }: { r: WRequest }) {
  const snooze = useSnooze(r.ref)
  const menu = usePopover()
  return (
    <>
      <Button
        ref={menu.anchor}
        variant="ghost"
        icon={<AlarmClock className="size-4" />}
        onClick={menu.toggle}
        aria-haspopup="menu"
        aria-expanded={menu.open}
      >
        Snooze
      </Button>
      <Popover open={menu.open} onClose={menu.close} anchor={menu.anchor}>
        <MenuList
          onClose={menu.close}
          items={SNOOZES.map((s) => ({
            label: s.label,
            onSelect: () => snooze.mutate(minutesUntil(s)),
          }))}
        />
      </Popover>
    </>
  )
}

/** The decision buttons: sticky at the bottom on phones. */
function Actions({ r }: { r: WRequest }) {
  const decide = useDecide(r.ref)
  const [note, setNote] = useState('')
  const [path, setPath] = useState<'runner' | 'pr'>(
    r.runner?.online || r.files?.[0]?.outside ? 'runner' : r.project ? 'pr' : 'runner',
  )
  const [rule, setRule] = useState(ruleNone)
  const [ownRule, setOwnRule] = useState('')
  const picked = pickedRule(rule, ownRule)
  const n = note.trim() || undefined
  const pending = (d: string) => decide.isPending && decide.variables?.decision === d
  if (r.kind === 'question') return null
  return (
    <div className="mt-6 space-y-3">
      {r.kind === 'change' && <ApplyPath r={r} value={path} onChange={setPath} />}
      {r.kind === 'permission' && r.state === 'open' && r.tool && (
        <RuleChoice
          rules={r.rules ?? []}
          repo={r.rule_repo || r.session?.repo}
          tool={r.tool.name}
          value={rule}
          own={ownRule}
          onChange={setRule}
          onOwn={setOwnRule}
          none="Only tell the agent, at its next prompt. Auto mode still decides this call."
        />
      )}
      <NoteField value={note} onChange={setNote} />
      <div className="sticky bottom-0 -mx-4 flex flex-wrap items-center gap-2 border-t border-line bg-panel/95 px-4 py-3 pb-[max(0.75rem,env(safe-area-inset-bottom))] backdrop-blur sm:static sm:mx-0 sm:border-0 sm:bg-transparent sm:p-0 sm:backdrop-blur-none">
        {r.kind === 'change' && (
          <>
            <Button
              variant="primary"
              loading={pending('approve')}
              icon={<Check className="size-4" />}
              onClick={() => decide.mutate({ decision: 'approve', apply_path: path, note: n })}
            >
              {r.state === 'failed' ? 'Try again' : 'Approve and apply'}
            </Button>
            {r.state === 'open' && (
              <Button
                variant="secondary"
                loading={pending('deny')}
                onClick={() => decide.mutate({ decision: 'deny', note: n })}
              >
                Deny
              </Button>
            )}
          </>
        )}
        {r.kind === 'permission' && (
          <>
            <Button
              variant="primary"
              loading={pending('approve')}
              icon={<Check className="size-4" />}
              disabled={rule === ruleOwn && !picked}
              onClick={() => decide.mutate({ decision: 'approve', rule: picked, note: n })}
            >
              {rule === ruleNone ? 'Approve' : 'Approve and add rule'}
            </Button>
            <Button
              variant="secondary"
              loading={pending('deny')}
              onClick={() => decide.mutate({ decision: 'deny', note: n })}
            >
              Deny
            </Button>
          </>
        )}
        {(r.kind === 'command' || r.kind === 'action') && (
          <Button
            variant="primary"
            loading={pending('done')}
            icon={<Check className="size-4" />}
            onClick={() => decide.mutate({ decision: 'done', note: n })}
          >
            Done
          </Button>
        )}
        {r.state === 'open' && <DismissButton r={r} note={note} />}
        {r.state === 'open' && <SnoozeButton r={r} />}
      </div>
    </div>
  )
}
