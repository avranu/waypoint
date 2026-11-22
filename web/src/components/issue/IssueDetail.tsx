import { useEffect, useRef, useState, type ReactNode } from 'react'
import { Link, useNavigate } from '@tanstack/react-router'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import clsx from 'clsx'
import {
  ArrowRightLeft,
  Bot,
  Bell,
  BellOff,
  CalendarClock,
  Check,
  CheckCheck,
  ChevronRight,
  Copy,
  Eye,
  Hourglass,
  Link2,
  Plus,
  Paperclip,
  BookOpen,
  ListTree,
  GitMerge,
  Maximize2,
  MoreHorizontal,
  Play,
  Share2,
  ShieldAlert,
  Square,
  Target,
  Trash2,
  X,
} from 'lucide-react'
import { api, enc } from '@/lib/api'
import { invalidateProject, q, useDeleteIssue, usePublishIssue, useTimer } from '@/lib/queries'
import { TITLE_MAX, titleTooLong, utf8Len } from '@/lib/quickadd'
import { clearDraft, readDraft, useUnsaved, writeDraft } from '@/lib/drafts'
import { LongTitleHint, SaveError, useQuickAdd } from './quickAdd'
import {
  QuickAddDetailsButton,
  isDetailsKey,
  openDetails,
} from '@/components/issue/QuickAddDetails'
import { clock, dateTime, duration, relativeTime } from '@/lib/format'
import { copyText } from '@/lib/clipboard'
import { useTick } from '@/lib/hooks'
import { Button, IconButton, ToggleButton } from '@/components/ui/button'
import { Avatar, Badge, Skeleton } from '@/components/ui/misc'
import { Confirm, Dialog } from '@/components/ui/dialog'
import { MenuList, Picker, Popover, usePopover } from '@/components/ui/popover'
import { toast, toastError } from '@/components/ui/toast'
import { ProjectIcon, StatusIcon, TypeIcon } from './icons'
import {
  AssigneePicker,
  DatePicker,
  LabelPicker,
  ParentPicker,
  PointsPicker,
  PriorityPicker,
  SprintPicker,
  StatusPicker,
  TypePicker,
} from './pickers'
import { useIssueEdit } from './edit'
import { MarkdownEditor } from './Description'
import { IssueActivity } from './Activity'
import { LinksBlock, RelationsBlock, SharePanel } from './Connections'
import { DeliveryBanner } from '@/components/delivery'
import { IssueRow } from './IssueRow'
import { SuggestSubtasks } from './AI'
import { AgentRuns, NewPageFromIssue } from './AgentWork'
import { AssistantChats } from './AssistantChats'
import { WorkLog, workLogHasNews } from './WorkLog'
import { Links as KnowledgeLinks } from '@/components/Links'
import { Mentions } from './Mentions'
import { linksQuery } from '@/lib/knowledge'
import {
  CategoryPicker,
  ClaimedBy,
  HumansOnlyLock,
  WorkflowPicker,
  activeClaim,
  claimWorked,
} from './agentPickers'
import { LineageNote, RiskNote, WrittenBy } from '@/components/Provenance'
import { GoalTrace } from '@/components/goals'
import { DraftBanner } from '@/components/Drafts'
import { HoldBanner, HoldDialog } from '@/components/Hold'
import { ObjectiveBanner, ObjectiveDialog } from '@/components/Objective'
import { OutsideTexts } from './Outside'
import { agentLabel, isDelegated, whoOf } from '@/lib/agents'
import { Attachments } from '@/components/attachments/Attachments'
import { IssueSummary, IssueTabs, useIssueTab } from './Summary'
import { PartsBar } from './Lifecycle'
import type { FiledFrom, IssueDetail as Detail, Project } from '@/lib/types'

const ADDER_ICON: Record<string, typeof Plus> = {
  'sub-issue': ListTree,
  file: Paperclip,
  link: Link2,
  relation: GitMerge,
  knowledge: BookOpen,
}

export function IssueDetailView({
  issueKey,
  mode,
  onClose,
}: {
  issueKey: string
  mode: 'sheet' | 'page'
  onClose?: () => void
}) {
  const issueQ = useQuery(q.issue(issueKey))
  const issue = issueQ.data
  const projectQ = useQuery({ ...q.project(issue?.project_key ?? ''), enabled: !!issue })
  if (issueQ.isError) {
    return (
      <div className="p-8 text-center">
        <div className="text-[15px] font-medium">Can't open {issueKey}</div>
        <div className="mt-1 text-[13px] text-muted">{(issueQ.error as Error).message}</div>
        {onClose && (
          <Button className="mt-4" size="sm" onClick={onClose}>
            Close
          </Button>
        )}
      </div>
    )
  }
  if (!issue || !projectQ.data) return <DetailSkeleton />
  return <Loaded issue={issue} project={projectQ.data} mode={mode} onClose={onClose} />
}

function DetailSkeleton() {
  return (
    <div className="space-y-4 p-6">
      <Skeleton className="h-4 w-40" />
      <Skeleton className="h-7 w-3/4" />
      <Skeleton className="h-24 w-full" />
      <Skeleton className="h-4 w-1/2" />
    </div>
  )
}

function Loaded({
  issue,
  project,
  mode,
  onClose,
}: {
  issue: Detail
  project: Project
  mode: 'sheet' | 'page'
  onClose?: () => void
}) {
  const edit = useIssueEdit(issue, project)
  const editable = issue.role === 'owner' || issue.role === 'editor'
  const closed = issue.status.category === 'done' || issue.status.category === 'canceled'
  const publish = usePublishIssue()
  // Empty sections stay out of sight; "+ File" and friends bring them back.
  const [opened, setOpened] = useState<string[]>([])
  // The section a "+" just opened starts adding, once: coming back to
  // Details later doesn't reopen its box or the file chooser.
  const [starting, setStarting] = useState('')
  const [objectiveOpen, setObjectiveOpen] = useState(false)
  const knowledge = useQuery(linksQuery({ type: 'issue', key: issue.key }))
  const filled: Record<string, boolean> = {
    'sub-issue': issue.children.length > 0,
    file: (issue.attachments?.length ?? 0) > 0,
    link: issue.links.length > 0,
    relation: issue.relations.length > 0,
    // Mentions have their own section below, as in KnowledgeLinks' skipMentions.
    knowledge: (knowledge.data ?? []).some((l) => !(l.auto && l.rel === 'mentions')),
  }
  const visible = (k: string) => filled[k] || opened.includes(k)
  const hidden = ['sub-issue', 'file', 'link', 'relation', 'knowledge'].filter((k) => !visible(k))
  // Summary: what it's for and where it stands; Details: everything (WP-780).
  const [tab, setTab] = useIssueTab()
  const details = tab === 'details'
  useEffect(() => {
    if (!details) setStarting('')
  }, [details])
  return (
    <div className="@container flex min-h-full flex-col">
      <Header
        issue={issue}
        project={project}
        mode={mode}
        onClose={onClose}
        editable={editable}
        onObjective={() => setObjectiveOpen(true)}
      />
      <ObjectiveDialog
        open={objectiveOpen}
        onClose={() => setObjectiveOpen(false)}
        issueKey={issue.key}
        objective={issue.objective}
      />
      <div
        className={clsx(
          'flex flex-1 flex-col gap-6 px-4 pb-10 pt-2 sm:px-6 @4xl:flex-row',
          mode === 'page' && 'mx-auto w-full max-w-6xl',
        )}
      >
        <div className="min-w-0 flex-1 space-y-6">
          <div>
            <TitleEditor
              key={issue.key}
              issueKey={issue.key}
              value={issue.title}
              onSave={edit.title}
              editable={editable}
              closed={closed}
              after={issue.relations
                .filter((r) => r.kind === 'follows' && r.subkind)
                .map((r) => (
                  <Link
                    key={r.id}
                    to="/issue/$key"
                    params={{ key: r.issue.key }}
                    title={`${r.subkind === 'fix' ? 'Mends something the earlier work got wrong or missed' : 'Does more of what the earlier work did'}: ${r.issue.key}`}
                    className="shrink-0 rounded-md hover:opacity-80"
                  >
                    <Badge color={r.subkind === 'fix' ? 'var(--wp-warn)' : 'var(--wp-info)'}>
                      {r.subkind} of {r.issue.key}
                    </Badge>
                  </Link>
                ))}
            />
            <WrittenBy
              agent={issue.text_agent}
              unreviewed={issue.unreviewed}
              reviewedBy={issue.reviewed_by}
              review={editable ? { type: 'issue', ref: issue.key } : undefined}
              className="mt-1"
            />
            {closed && issue.resolved_agent && (
              <WrittenBy
                agent={issue.resolved_agent}
                label={`Moved to ${issue.status.name} by`}
                unreviewed={issue.close_unreviewed}
                reviewedBy={issue.close_reviewed_by}
                review={editable ? { type: 'close', ref: issue.key } : undefined}
                className="mt-1 flex"
              />
            )}
            {issue.safety?.verdict === 'hold' && (
              <p className="mt-1 flex items-start gap-1 text-xs text-warn">
                <ShieldAlert className="mt-0.5 size-3 shrink-0" />
                <span>Held from agents: {issue.safety.reason}. Reviewing it releases it.</span>
              </p>
            )}
            <LineageNote lineage={issue.lineage} className="mt-1" />
            <RiskNote risk={issue.risk} className="mt-1" />
            <GoalTrace
              issueKey={issue.key}
              project={issue.project_key}
              scoped={!!issue.goal}
              editable={editable}
              className="mt-1"
            />
            {issue.draft && (
              <DraftBanner
                what="issue"
                className="mt-3"
                publishing={publish.isPending}
                outside={
                  issue.outside ? { by: issue.outside_by, forge: issue.outside_forge } : undefined
                }
                onPublish={
                  editable
                    ? () =>
                        publish.mutate(issue.key, {
                          onSuccess: () =>
                            toast(
                              `${issue.outside ? 'Approved' : 'Published'} ${issue.key}: agents can see it now`,
                            ),
                          onError: toastError,
                        })
                    : undefined
                }
              />
            )}
            {issue.hold && !closed && (
              <HoldBanner
                issueKey={issue.key}
                hold={issue.hold}
                editable={editable}
                className="mt-3"
              />
            )}
            {issue.objective && (
              <ObjectiveBanner
                objective={issue.objective}
                editable={editable && project.role !== 'guest'}
                closed={closed}
                onChange={() => setObjectiveOpen(true)}
                className="mt-3"
              />
            )}
            <DeliveryBanner issue={issue} className="mt-3" />
          </div>
          <IssueTabs tab={tab} setTab={setTab} />
          {/* On narrow layouts properties sit under the title. */}
          <div className="@4xl:hidden">
            <Properties issue={issue} project={project} editable={editable} compact />
          </div>
          {details ? (
            <MarkdownEditor
              key={issue.key}
              draftKey={editable ? `description:${issue.key}` : undefined}
              autosave
              rich
              ariaLabel="Description"
              placeholder="Add a description… Type @ to mention someone; paste or drop files to attach them."
              value={issue.description ?? ''}
              onSave={edit.description}
              editable={editable}
              issueKey={issue.key}
              project={issue.project_key}
            />
          ) : (
            <IssueSummary
              issue={issue}
              project={project}
              editable={editable}
              onIntent={edit.intent}
              onDetails={() => setTab('details')}
            />
          )}
          <OutsideTexts issue={issue} editable={editable} />
          <Children
            issue={issue}
            project={project}
            editable={editable}
            adding={opened.includes('sub-issue')}
          />
          {details && (
            <>
              <div hidden={!visible('file')}>
                <Attachments
                  target={`issue:${issue.key}`}
                  startAdding={starting === 'file'}
                  files={issue.attachments ?? []}
                  editable={editable}
                  wrap={(title, actions, body) => (
                    <Block title={title} actions={actions}>
                      {body}
                    </Block>
                  )}
                />
              </div>
              {visible('link') && (
                <LinksBlock
                  issue={issue}
                  editable={editable}
                  startAdding={starting === 'link'}
                  wrap={(actions, body) => (
                    <Block title="Links" actions={actions}>
                      {body}
                    </Block>
                  )}
                />
              )}
              {visible('relation') && (
                <RelationsBlock
                  issue={issue}
                  editable={editable}
                  startAdding={starting === 'relation'}
                  wrap={(actions, body) => (
                    <Block title="Relations" actions={actions}>
                      {body}
                    </Block>
                  )}
                />
              )}
              {visible('knowledge') && (
                <KnowledgeLinks
                  at={{ type: 'issue', key: issue.key }}
                  editable={editable}
                  title="Knowledge"
                  hideEmpty
                  skipMentions
                  actions={editable && <NewPageFromIssue issue={issue} />}
                />
              )}
              {editable && hidden.length > 0 && (
                <div className="flex flex-wrap items-center gap-1 text-subtle">
                  {hidden.map((k) => {
                    const name = k === 'sub-issue' ? 'sub-issue' : k
                    const Icon = ADDER_ICON[k] ?? Plus
                    return (
                      <IconButton
                        key={k}
                        label={`Add ${name}`}
                        onClick={() => {
                          setOpened([...opened, k])
                          setStarting(k)
                        }}
                        icon={
                          <span className="relative">
                            <Icon className="size-4" />
                            <Plus className="absolute -right-1.5 -top-1.5 size-2.5 rounded-full bg-surface" />
                          </span>
                        }
                      />
                    )
                  })}
                </div>
              )}
              <Mentions
                issueKey={issue.key}
                forge={issue.forge_mentions}
                related={issue.relations.map((r) => r.issue.key)}
              />
              <AssistantChats issue={issue} />
              {(issue.agent_runs?.length ?? 0) > 0 && (
                <Block title="Agent runs">
                  <AgentRuns runs={issue.agent_runs!} />
                </Block>
              )}
              <WorkLogBlock issueKey={issue.key} editable={editable} />
            </>
          )}
          <IssueActivity issue={issue} />
        </div>
        <aside className="hidden w-72 shrink-0 @4xl:block">
          <div className="sticky top-2">
            <Properties issue={issue} project={project} editable={editable} />
          </div>
        </aside>
      </div>
    </div>
  )
}

/** The work log, once it holds more than the runs and pull requests shown elsewhere (or anything, for someone who may add a note). */
function WorkLogBlock({ issueKey, editable }: { issueKey: string; editable: boolean }) {
  const log = useQuery(q.workLog(issueKey))
  if (!workLogHasNews(log.data) && !(editable && (log.data?.entries.length ?? 0) > 0)) return null
  return (
    <Block title="Work log">
      <WorkLog issueKey={issueKey} editable={editable} />
    </Block>
  )
}

function Block({
  title,
  children,
  actions,
}: {
  title: string
  children: ReactNode
  actions?: ReactNode
}) {
  return (
    <section>
      <div className="mb-1.5 flex items-center justify-between">
        <h3 className="text-xs font-semibold uppercase tracking-wide text-subtle">{title}</h3>
        {actions}
      </div>
      {children}
    </section>
  )
}

function Header({
  issue,
  project,
  mode,
  onClose,
  editable,
  onObjective,
}: {
  issue: Detail
  project: Project
  mode: 'sheet' | 'page'
  onClose?: () => void
  onObjective: () => void
  editable: boolean
}) {
  const qc = useQueryClient()
  const navigate = useNavigate()
  const menu = usePopover()
  const share = usePopover()
  const [confirmDelete, setConfirmDelete] = useState(false)
  const [moving, setMoving] = useState(false)
  const [holding, setHolding] = useState(false)
  const closed = issue.status.category === 'done' || issue.status.category === 'canceled'
  const del = useDeleteIssue()
  const timer = useQuery(q.timer()).data
  const { start, stop } = useTimer()
  const running = timer?.issue?.key === issue.key
  useTick(1000, running)
  const watch = useMutation({
    mutationFn: (on: boolean) =>
      on ? api.put(`/issues/${enc(issue.key)}/watch`) : api.del(`/issues/${enc(issue.key)}/watch`),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['issue', issue.key] }),
    onError: toastError,
  })
  const url = `${location.origin}/issue/${issue.key}`
  const copy = (text: string, what: string) =>
    copyText(text).then(() => toast(`Copied ${what}`), toastError)
  return (
    <div className="sticky top-0 z-10 flex h-12 shrink-0 items-center gap-1 border-b border-line bg-panel/90 px-3 backdrop-blur sm:px-4">
      <nav className="flex min-w-0 flex-1 items-center gap-1 text-[13px] text-muted">
        <Link
          to="/p/$key/board"
          params={{ key: project.key }}
          className="flex shrink-0 items-center gap-1.5 rounded px-1 hover:bg-hover hover:text-fg"
        >
          <ProjectIcon icon={project.icon} color={project.color} name={project.name} size={18} />
          <span className="hidden sm:inline">{project.name}</span>
        </Link>
        {issue.ancestors.map((a) => (
          <span key={a.key} className="hidden min-w-0 items-center gap-1 md:flex">
            <ChevronRight className="size-3.5 shrink-0 text-subtle" />
            <Link
              to="/issue/$key"
              params={{ key: a.key }}
              className="flex min-w-0 items-center gap-1 rounded px-1 hover:bg-hover hover:text-fg"
              title={a.title}
            >
              <TypeIcon type={a.type} />
              <span className="max-w-40 truncate">{a.title}</span>
            </Link>
          </span>
        ))}
        <ChevronRight className="size-3.5 shrink-0 text-subtle" />
        <button
          onClick={() => copy(issue.key, issue.key)}
          className="flex shrink-0 items-center gap-1 rounded px-1 font-medium text-fg hover:bg-hover"
          title="Copy key"
        >
          <TypeIcon type={issue.type} />
          {issue.key}
        </button>
      </nav>
      {running ? (
        <Button
          size="sm"
          variant="ghost"
          onClick={() => stop.mutate()}
          icon={<Square className="size-3 fill-danger text-danger" />}
          className="tabular-nums"
        >
          {clock((Date.now() - new Date(timer!.started_at).getTime()) / 1000)}
        </Button>
      ) : (
        <IconButton
          label="Start timer"
          onClick={() => start.mutate({ issue: issue.key })}
          icon={<Play className="size-3.5" />}
        />
      )}
      {editable && project.role !== 'guest' && (
        <>
          <IconButton
            ref={share.anchor}
            label="Share"
            onClick={share.toggle}
            icon={<Share2 className="size-3.5" />}
          />
          <Popover open={share.open} onClose={share.close} anchor={share.anchor} align="end">
            <SharePanel issue={issue} onClose={share.close} />
          </Popover>
        </>
      )}
      <IconButton
        label={issue.watching ? 'Stop watching' : 'Watch'}
        onClick={() => !watch.isPending && watch.mutate(!issue.watching)}
        icon={
          issue.watching ? (
            <Bell className="size-3.5 fill-current" />
          ) : (
            <BellOff className="size-3.5" />
          )
        }
      />
      <IconButton
        ref={menu.anchor}
        label="More"
        onClick={menu.toggle}
        icon={<MoreHorizontal className="size-4" />}
      />
      <Popover open={menu.open} onClose={menu.close} anchor={menu.anchor} align="end">
        <MenuList
          onClose={menu.close}
          items={[
            { label: 'Copy link', icon: <Link2 />, onSelect: () => copy(url, 'link') },
            {
              label: 'Copy key and title',
              icon: <Copy />,
              onSelect: () => copy(`${issue.key} ${issue.title}`, 'key and title'),
            },
            ...(mode === 'sheet'
              ? [
                  {
                    label: 'Open full page',
                    icon: <Maximize2 />,
                    onSelect: () => navigate({ to: '/issue/$key', params: { key: issue.key } }),
                  },
                ]
              : []),
            ...(editable && project.role !== 'guest'
              ? [
                  { separator: true, label: '' },
                  ...(closed
                    ? []
                    : [
                        {
                          label: issue.hold ? 'Change hold…' : 'Put on hold…',
                          icon: <Hourglass />,
                          onSelect: () => setHolding(true),
                        },
                        {
                          label: issue.objective ? 'Change objective…' : 'Set an objective…',
                          icon: <Target />,
                          onSelect: onObjective,
                        },
                      ]),
                  {
                    label: 'Move to project…',
                    icon: <ArrowRightLeft />,
                    onSelect: () => setMoving(true),
                  },
                  {
                    label: 'Delete',
                    icon: <Trash2 />,
                    danger: true,
                    onSelect: () => setConfirmDelete(true),
                  },
                ]
              : []),
          ]}
        />
      </Popover>
      {onClose && <IconButton label="Close" onClick={onClose} icon={<X className="size-4" />} />}
      <Confirm
        open={confirmDelete}
        onClose={() => setConfirmDelete(false)}
        title={`Delete ${issue.key}?`}
        body={
          issue.children.length > 0
            ? `Its ${issue.children.length} child issue(s) will move up to ${issue.parent ? issue.parent.key : 'the top level'}. This can't be undone.`
            : "This can't be undone."
        }
        onConfirm={() =>
          del.mutate(
            { key: issue.key, projectKey: issue.project_key },
            {
              onSuccess: () => {
                toast(`Deleted ${issue.key}`)
                if (onClose) onClose()
                else navigate({ to: '/p/$key/board', params: { key: issue.project_key } })
              },
              onError: toastError,
            },
          )
        }
      />
      <MoveDialog open={moving} onClose={() => setMoving(false)} issue={issue} />
      <HoldDialog
        open={holding}
        onClose={() => setHolding(false)}
        issueKey={issue.key}
        hold={issue.hold}
      />
    </div>
  )
}

function MoveDialog({
  open,
  onClose,
  issue,
}: {
  open: boolean
  onClose: () => void
  issue: Detail
}) {
  const qc = useQueryClient()
  const navigate = useNavigate()
  const projects = useQuery(q.projects()).data ?? []
  const move = useMutation({
    mutationFn: (key: string) =>
      api.post<Detail>(`/issues/${enc(issue.key)}/move`, { project: key }),
    onSuccess: (d) => {
      invalidateProject(qc, issue.project_key)
      invalidateProject(qc, d.project_key)
      toast(`Moved to ${d.key}`)
      onClose()
      navigate({ to: '/issue/$key', params: { key: d.key } })
    },
    onError: toastError,
  })
  return (
    <Dialog open={open} onClose={onClose} title={`Move ${issue.key}`}>
      <div className="p-2">
        <p className="px-2 pb-2 text-[13px] text-muted">
          Moves this issue{issue.children.length ? ' and everything beneath it' : ''} to another
          project. It gets a new key; the old one keeps working.
        </p>
        <Picker
          items={projects
            .filter((p) => p.key !== issue.project_key && p.role !== 'guest' && p.role !== 'viewer')
            .map((p) => ({
              value: p.key,
              label: p.name,
              hint: p.key,
              icon: <ProjectIcon icon={p.icon} color={p.color} name={p.name} size={18} />,
            }))}
          selected={[]}
          onPick={(k) => move.mutate(k)}
          placeholder="Choose a project…"
        />
      </div>
    </Dialog>
  )
}

/**
 * The issue title, edited in place (WP-68). Enter or leaving the field
 * saves, Esc goes back to the saved title. An edit is kept in a draft until
 * the server has it, so a refresh brings it back; a save that fails, or a
 * title too long to send, leaves the edit where it is and says why.
 */
function TitleEditor({
  issueKey,
  value,
  onSave,
  editable,
  closed,
  after,
}: {
  issueKey: string
  value: string
  onSave: (v: string) => Promise<unknown>
  editable: boolean
  closed: boolean
  /** Sits right after the title text, on its line where it fits. */
  after?: ReactNode
}) {
  const key = `title:${issueKey}`
  const [stored] = useState(() => (editable ? readDraft<string>(key) : null))
  const [draft, setDraft] = useState(stored ?? value)
  // Dirty: the field holds an edit the server hasn't got. Only a clean field
  // follows the saved title, so a refetch can't wipe what's being typed.
  const [dirty, setDirty] = useState(stored !== null && stored !== value)
  const [restored, setRestored] = useState(dirty)
  const [kept, setKept] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const saving = useRef(false)
  // Set by Esc: the blur it causes fires before React re-renders, with the
  // edit still in this render's closure, and must not save it.
  const cancelled = useRef(false)
  const ref = useRef<HTMLTextAreaElement>(null)
  useUnsaved(dirty && !kept)
  useEffect(() => {
    if (!dirty) setDraft(value)
    // eslint-disable-next-line react-hooks/exhaustive-deps -- only a new saved title re-syncs
  }, [value])
  useEffect(() => {
    const el = ref.current
    if (el) {
      el.style.height = 'auto'
      el.style.height = el.scrollHeight + 'px'
    }
  }, [draft])
  const change = (v: string) => {
    const d = v !== value
    setDraft(v)
    setDirty(d)
    setKept(writeDraft(key, d ? v : null))
    setError(null)
  }
  const reset = () => {
    setDraft(value)
    setDirty(false)
    setRestored(false)
    setError(null)
    clearDraft(key)
  }
  // True when there's nothing left to save.
  const commit = async (): Promise<boolean> => {
    if (saving.current) return false
    if (cancelled.current) {
      cancelled.current = false
      return true
    }
    const t = draft.trim()
    if (!dirty || t === value) {
      reset()
      return true
    }
    if (!t) {
      setError('A title is required. Esc puts the old one back.')
      return false
    }
    if (titleTooLong(t)) {
      setError(
        `Titles can be at most ${TITLE_MAX} characters; this one is ${utf8Len(t)}. Shorten it, or move the rest to the description.`,
      )
      return false
    }
    saving.current = true
    try {
      await onSave(t)
      setDirty(false)
      setRestored(false)
      clearDraft(key)
      return true
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
      return false
    } finally {
      saving.current = false
    }
  }
  const over = utf8Len(draft.trim()) - TITLE_MAX
  return (
    <div>
      <div className="flex flex-wrap items-baseline gap-x-2 gap-y-1">
        <textarea
          ref={ref}
          rows={1}
          value={draft}
          readOnly={!editable}
          aria-label="Title"
          aria-invalid={!!error || undefined}
          onChange={(e) => change(e.target.value.replace(/\n/g, ' '))}
          onBlur={() => void commit()}
          onKeyDown={(e) => {
            const el = e.currentTarget
            if (e.key === 'Enter') {
              e.preventDefault()
              // Stay in the field unless it saved.
              void commit().then((done) => done && el.blur())
            } else if (e.key === 'Escape') {
              e.preventDefault()
              e.stopPropagation()
              cancelled.current = true
              reset()
              el.blur()
              cancelled.current = false
            }
          }}
          className={clsx(
            '-mx-1 block min-w-[8ch] max-w-full resize-none overflow-hidden rounded-lg bg-transparent px-1 py-0.5 [field-sizing:content] text-xl font-semibold leading-snug tracking-tight outline-none focus:bg-hover sm:text-[22px]',
            closed && 'text-muted',
            error && 'ring-1 ring-danger/50',
          )}
        />
        {after}
      </div>
      {over > -50 && dirty && (
        <p className={clsx('mt-1 text-xs tabular-nums', over > 0 ? 'text-danger' : 'text-subtle')}>
          {utf8Len(draft.trim())}/{TITLE_MAX}
        </p>
      )}
      {error ? (
        <p role="alert" className="mt-1 text-xs text-danger">
          Not saved: {error}
        </p>
      ) : (
        restored &&
        dirty && (
          <p className="mt-1 text-xs text-muted">
            Unsaved edit restored. Enter saves it, Esc goes back to the saved title.
          </p>
        )
      )}
    </div>
  )
}

function Prop({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="flex min-h-8 items-center gap-2">
      <div className="w-24 shrink-0 text-[13px] text-subtle">{label}</div>
      <div className="min-w-0 flex-1">{children}</div>
    </div>
  )
}

/**
 * "X is on it · 12:03 · until 15:40": an agent's live claim, with the time
 * it has worked on it; "X paused" while it's idle (its clock stopped).
 */
function ClaimNote({ issue }: { issue: Detail }) {
  const until = issue.claimed_until ? new Date(issue.claimed_until) : null
  const live = !!issue.claimed_by && !!until && until.getTime() >= Date.now()
  const working = !!issue.claim_working_since
  useTick(1000, live && working)
  if (!live) return null
  const worked = claimWorked(issue)
  return (
    <div
      className={clsx(
        'flex h-7 items-center gap-1.5 rounded-md px-2 text-xs',
        working ? 'bg-accent-soft text-accent' : 'bg-sunken text-muted',
      )}
      title={`Claimed until ${dateTime(issue.claimed_until!)}${working ? '' : '; idle, so its clock is stopped'}`}
    >
      <Bot className={clsx('size-3.5', working && 'animate-pulse motion-reduce:animate-none')} />
      <span className="truncate">
        {agentLabel(issue.claimed_by!)} {working ? 'is on it' : 'paused'}
        {worked !== null && <span className="font-medium tabular-nums"> · {clock(worked)}</span>} ·
        until {until!.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })}
      </span>
    </div>
  )
}

/**
 * Where a task an agent filed from another repo came from, and the reason
 * if this repo's agents declined it (WP-378). pill: the phone's compact row.
 */
function FiledFromNote({ from, pill }: { from: FiledFrom; pill?: boolean }) {
  const where =
    (from.project ?? from.repo ?? 'another repo') +
    (from.project && from.repo ? ` (${from.repo})` : '')
  return (
    <span
      data-testid="filed-from"
      className={clsx(
        'min-w-0',
        pill
          ? 'flex h-7 max-w-full items-center gap-1.5 rounded-md bg-sunken px-2 text-xs text-muted'
          : 'block px-2 text-[13px]',
      )}
      title={
        from.declined_at
          ? `Declined by ${agentLabel(from.declined_by ?? '')}: ${from.declined ?? ''}`
          : 'An agent working in another repo filed this; agents here check it belongs before working on it'
      }
    >
      <span className={clsx('block truncate', pill && 'shrink-0')}>
        {pill && 'From '}
        {where}
      </span>
      {from.declined_at && (
        <span className={clsx('block truncate', !pill && 'text-muted')}>
          {pill ? '· ' : ''}Declined: {from.declined}
        </span>
      )}
    </span>
  )
}

/** The optional fields, in the order they're listed. */
const OPTIONAL = ['Points', 'Parent', 'Tags', 'Category', 'Workflow', 'Start', 'Due']

function Properties({
  issue,
  project,
  editable,
  compact,
}: {
  issue: Detail
  project: Project
  editable: boolean
  compact?: boolean
}) {
  const edit = useIssueEdit(issue, project)
  const closed = issue.status.category === 'done' || issue.status.category === 'canceled'
  const openChildren = issue.children.filter(
    (c) => c.status.category !== 'done' && c.status.category !== 'canceled',
  ).length
  const showSprint =
    project.settings.mode === 'sprints' || (project.sprints?.length ?? 0) > 0 || issue.sprint_id
  const v = compact ? 'pill' : 'row'
  const isSet: Record<string, boolean> = {
    Points: issue.points != null,
    Parent: !!issue.parent,
    Tags: issue.labels.length > 0,
    Category: !!issue.category,
    Workflow: !!issue.workflow_id,
    Start: !!issue.start_date,
    Due: !!issue.due_date,
  }
  // Everyone who can set a field sees all of them, each its own picker, so
  // setting one is one click (the strip wraps; no "More" menu in between).
  const always = !editable ? [] : OPTIONAL
  const show = (k: string) => always.includes(k) || isSet[k]
  // A row's picker takes the full width, so the lock's "Humans only" text
  // goes under it instead of clipping "None" (WP-1196 audit).
  const lockedRow = v === 'row' && whoOf(issue.agent ?? '') === 'humans'
  const assignee = (
    <span className={clsx('flex min-w-0 items-center gap-0.5', lockedRow && 'flex-wrap')}>
      <AssigneePicker
        value={issue.assignees}
        onChange={edit.assignees}
        disabled={!editable}
        variant={v}
        members={project.members?.map((m) => m.user.id)}
        agent={issue.agent ?? ''}
        onAgent={edit.agent}
      />
      <HumansOnlyLock
        value={issue.agent ?? ''}
        onChange={edit.agent}
        people={issue.assignees.length}
        agentOn={isDelegated(issue.agent) || !!activeClaim(issue)}
        disabled={!editable}
      />
    </span>
  )
  const fields: Record<string, ReactNode> = {
    Points: (
      <PointsPicker
        value={issue.points}
        onChange={edit.points}
        disabled={!editable}
        variant={v}
        estimateKey={issue.key}
      />
    ),
    Parent: (
      <ParentPicker
        projectKey={project.key}
        value={issue.parent}
        onChange={edit.parent}
        exclude={[issue.key, ...issue.children.map((c) => c.key)]}
        disabled={!editable}
        variant={v}
      />
    ),
    Tags: (
      <LabelPicker
        labels={project.labels ?? []}
        value={issue.labels}
        onChange={edit.labels}
        disabled={!editable}
        variant={v}
      />
    ),
    Category: (
      <CategoryPicker
        value={issue.category ?? ''}
        onChange={edit.category}
        disabled={!editable}
        variant={v}
      />
    ),
    Workflow: (
      <WorkflowPicker
        projectKey={project.key}
        workflowId={issue.workflow_id}
        step={issue.step}
        onWorkflow={edit.workflow}
        onStep={edit.step}
        disabled={!editable}
        variant={v}
      />
    ),
    Start: (
      <DatePicker
        value={issue.start_date}
        onChange={edit.start}
        disabled={!editable}
        variant={v}
        placeholder="Start"
        icon={<CalendarClock className="size-3.5 text-subtle" />}
      />
    ),
    Due: (
      <DatePicker
        value={issue.due_date}
        onChange={edit.due}
        disabled={!editable}
        variant={v}
        placeholder="Due"
        closed={closed}
      />
    ),
  }
  const sprint = showSprint && (
    <SprintPicker
      sprints={project.sprints ?? []}
      value={issue.sprint_id}
      valueName={issue.sprint_name}
      onChange={edit.sprint}
      disabled={!editable}
      variant={v}
    />
  )
  if (compact) {
    return (
      <div className="-mx-1 flex flex-wrap items-center gap-1 rounded-xl border border-line bg-panel p-1.5">
        <StatusPicker
          statuses={project.statuses ?? []}
          value={issue.status}
          onChange={edit.status}
          disabled={!editable}
          openChildren={openChildren}
        />
        <PriorityPicker value={issue.priority} onChange={edit.priority} disabled={!editable} />
        {assignee}
        <TypePicker value={issue.type} onChange={edit.type} disabled={!editable} />
        {sprint}
        {OPTIONAL.filter(show).map((k) => (
          <span key={k} className="contents">
            {fields[k]}
          </span>
        ))}
        <ClaimNote issue={issue} />
        {issue.filed_from && <FiledFromNote from={issue.filed_from} pill />}
      </div>
    )
  }
  // Watchers who are only the reporter say nothing new.
  const watchersNews =
    issue.shares.length > 0 ||
    issue.watchers.some((w) => issue.reporter_agent || w.id !== issue.reporter?.id)
  return (
    <div className="space-y-0.5 rounded-xl border border-line bg-panel p-3">
      <Prop label="Status">
        <StatusPicker
          statuses={project.statuses ?? []}
          value={issue.status}
          onChange={edit.status}
          disabled={!editable}
          openChildren={openChildren}
          variant={v}
        />
      </Prop>
      <Prop label="Assignees">
        {assignee}
        {/* The agent working on it counts as an assignee while its claim lasts. */}
        <div className="px-2 empty:hidden">
          <ClaimedBy issue={issue} />
        </div>
      </Prop>
      <Prop label="Priority">
        <PriorityPicker
          value={issue.priority}
          onChange={edit.priority}
          disabled={!editable}
          variant={v}
        />
      </Prop>
      <Prop label="Type">
        <TypePicker value={issue.type} onChange={edit.type} disabled={!editable} variant={v} />
      </Prop>
      {sprint && <Prop label="Sprint">{sprint}</Prop>}
      {OPTIONAL.filter(show).map((k) => (
        <Prop key={k} label={k}>
          {fields[k]}
        </Prop>
      ))}
      <div className="my-2 h-px bg-line" />
      <Prop label="Reporter">
        {issue.reporter_agent ? (
          <span
            className="flex min-w-0 items-center gap-1.5 px-2 text-[13px]"
            title={`Created by ${issue.reporter_agent}`}
          >
            <Bot className="size-4 shrink-0 text-muted" />
            <span className="truncate">{agentLabel(issue.reporter_agent)}</span>
          </span>
        ) : (
          <span className="flex min-w-0 items-center gap-1.5 px-2 text-[13px]">
            <Avatar user={issue.reporter} size={18} />
            <span className="truncate">{issue.reporter?.display_name ?? 'Waypoint'}</span>
          </span>
        )}
      </Prop>
      {issue.filed_from && (
        <Prop label="Filed from">
          <FiledFromNote from={issue.filed_from} />
        </Prop>
      )}
      {issue.time_spent > 0 && (
        <Prop label="Time spent">
          <span className="px-2 text-[13px]">{duration(issue.time_spent)}</span>
        </Prop>
      )}
      {watchersNews && (
        <Prop label="Watchers">
          <span className="flex items-center gap-1 px-2">
            {issue.watchers.map((w) => (
              <Avatar key={w.id} user={w} size={18} />
            ))}
            {issue.shares.length > 0 && (
              <span
                className="ml-1 flex items-center gap-1 text-xs text-subtle"
                title={`Shared with ${issue.shares.map((s) => s.user.display_name).join(', ')}`}
              >
                <Eye className="size-3" /> {issue.shares.length}
              </span>
            )}
          </span>
        </Prop>
      )}
      <div className="px-2 pt-2 text-xs text-subtle" title={dateTime(issue.updated_at)}>
        Updated {relativeTime(issue.updated_at)}
      </div>
    </div>
  )
}

function Children({
  issue,
  project,
  editable,
  adding,
}: {
  issue: Detail
  project: Project
  editable: boolean
  /** Asked for with "+ Sub-issue": shows the add box while there are none. */
  adding: boolean
}) {
  const navigate = useNavigate()
  const add = useQuickAdd(`children:${issue.key}`)
  const [showDone, setShowDone] = useState(true)
  const done = issue.children.filter(
    (c) => c.status.category === 'done' || c.status.category === 'canceled',
  ).length
  const childType = issue.type === 'epic' ? 'story' : 'subtask'
  const shown = showDone
    ? issue.children
    : issue.children.filter((c) => c.status.category !== 'done' && c.status.category !== 'canceled')
  const submit = () =>
    add.submit((p) => ({ type: childType, ...p.input, title: p.title, parent: issue.key }))
  const details = () =>
    openDetails(add, { type: childType, parent: issue.key, project: issue.project_key })
  if (!editable && issue.children.length === 0) return null
  // With none, it waits behind "+ Sub-issue" with the other empty sections.
  if (issue.children.length === 0 && !adding && !add.text) return null
  return (
    <Block
      title={issue.children.length ? `Sub-issues · ${done}/${issue.children.length}` : 'Sub-issues'}
      actions={
        <div className="flex items-center gap-3">
          {editable && <SuggestSubtasks issueKey={issue.key} childType={childType} />}
          {done > 0 && (
            <ToggleButton
              label="Done sub-issues"
              hint="Finished and canceled sub-issues"
              pressed={showDone}
              onPressedChange={setShowDone}
              icon={<CheckCheck className="size-3.5" />}
              text="Done"
            />
          )}
        </div>
      }
    >
      {issue.children.length > 0 && <PartsBar parts={issue.children} className="mb-2" />}
      <div className="overflow-hidden rounded-lg border border-line">
        {shown.map((c) => (
          <IssueRow
            key={c.id}
            issue={c}
            project={project}
            hideParent
            onOpen={(k) => navigate({ to: '/issue/$key', params: { key: k } })}
          />
        ))}
        {editable && (
          <form
            className="flex h-10 items-center gap-2 px-3 sm:px-4"
            onSubmit={(e) => {
              e.preventDefault()
              submit()
            }}
          >
            {add.pending ? (
              <StatusIcon category="todo" className="size-3.5 animate-pulse text-subtle" />
            ) : (
              <Check className="size-3.5 text-subtle" />
            )}
            <input
              value={add.text}
              readOnly={add.pending}
              onChange={(e) => add.setText(e.target.value)}
              autoFocus={adding}
              placeholder={`Add ${childType === 'story' ? 'a story' : 'a subtask'}…  (!high @me #tag ~3 due:fri)`}
              aria-label={`Add ${childType === 'story' ? 'a story' : 'a subtask'}`}
              onKeyDown={(e) => {
                if (isDetailsKey(e)) {
                  e.preventDefault()
                  details()
                }
              }}
              className="h-full flex-1 bg-transparent text-[13px] outline-none placeholder:text-subtle"
            />
            {!add.pending && <QuickAddDetailsButton onOpen={details} />}
          </form>
        )}
        {editable && (
          <>
            <LongTitleHint show={add.long} className="px-3 pb-2 text-[11px] text-muted sm:px-4" />
            <SaveError error={add.error} className="px-3 pb-2 text-[11px] text-danger sm:px-4" />
          </>
        )}
      </div>
    </Block>
  )
}
