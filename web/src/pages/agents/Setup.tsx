// Agents page, Setup tab (WP-208, WP-ADR44): is every repo and machine set up
// the same way? The shared instructions Waypoint publishes into each repo's
// root CLAUDE.md or AGENTS.md; every attached repo's agent files, the
// components it has that others lack and the ones it lacks, and Waypoint's
// checks on it; the same for the machines people's wp reports from.

import { useMemo, useState, type ReactNode } from 'react'
import { useQuery } from '@tanstack/react-query'
import clsx from 'clsx'
import {
  ChevronDown,
  ChevronLeft,
  ChevronRight,
  CircleAlert,
  ExternalLink,
  FileText,
  FolderGit2,
  GitPullRequest,
  History,
  Laptop,
  ListFilter,
  List,
  Pencil,
  Plus,
  RefreshCw,
  Rocket,
  Send,
} from 'lucide-react'
import { q } from '@/lib/queries'
import { useRolloutKit } from '@/lib/kit'
import {
  KIND_LABEL,
  SHARED,
  STATUS,
  tally,
  useEditAgentFile,
  usePublishShared,
  useRefreshSetup,
  useSaveShared,
  type AgentComponent,
  type AgentFile,
  type AgentFilePR,
  type AgentMachine,
  type AgentRepo,
  type AgentSetup,
  type CommonComponent,
  type SetupCheck,
  type SetupComponent,
  type SetupStatus,
} from '@/lib/agent-setup'
import { dateTime, plural, relativeTime, safeUrl } from '@/lib/format'
import { Button, IconButton } from '@/components/ui/button'
import { Confirm, Dialog } from '@/components/ui/dialog'
import { Field, Input, Textarea } from '@/components/ui/input'
import { Badge, Empty, Section, Skeleton } from '@/components/ui/misc'
import { SegmentedControl } from '@/components/ui/segmented'
import { toast } from '@/components/ui/toast'
import type { HubTabProps } from './search'

/** Which file the viewer has open: a repo's or a machine's, by index. */
type Opened =
  | { kind: 'repo'; repo: AgentRepo; index: number }
  | { kind: 'machine'; machine: AgentMachine; index: number }
  | { kind: 'new'; repo: AgentRepo }

export function SetupTab(_: HubTabProps) {
  const setup = useQuery(q.agentSetup())
  const [open, setOpen] = useState<Opened | null>(null)
  if (setup.isPending)
    return (
      <div className="space-y-4">
        <div className="grid gap-3 sm:grid-cols-3">
          <Skeleton className="h-20" />
          <Skeleton className="h-20" />
          <Skeleton className="h-20" />
        </div>
        <Skeleton className="h-40" />
        <Skeleton className="h-64" />
      </div>
    )
  if (setup.error || !setup.data)
    return <p className="text-[13px] text-danger">Couldn’t load the agent setup.</p>
  const s = setup.data
  return (
    <div className="space-y-4">
      <Glance s={s} />
      <SharedCard s={s} />
      <ReposSection s={s} onOpen={setOpen} />
      <MachinesSection s={s} onOpen={setOpen} />
      {open && open.kind === 'new' ? (
        <NewFileDialog repo={open.repo} onClose={() => setOpen(null)} />
      ) : (
        open && <FileDialog opened={open} onMove={setOpen} onClose={() => setOpen(null)} />
      )}
    </div>
  )
}

// ---- at a glance ----

function Glance({ s }: { s: AgentSetup }) {
  const repos = tally(s.repos)
  const machines = tally(s.machines)
  const cur = s.shared.current
  const current = s.shared.counts.current ?? 0
  const counted = s.repos.filter((r) => r.shared !== 'unread').length
  return (
    <div className="grid gap-3 sm:grid-cols-3" aria-label="At a glance">
      <Tile
        icon={<FolderGit2 />}
        title="Repos"
        value={`${repos.ok} of ${s.repos.length} in line`}
        parts={repos}
      />
      <Tile
        icon={<FileText />}
        title="Shared instructions"
        value={cur ? `v${cur.version} in ${current} of ${counted}` : 'Not written yet'}
        sub={
          cur
            ? `saved ${relativeTime(cur.saved_at)}${cur.saved_by ? ` by ${cur.saved_by.display_name}` : ''}`
            : s.can_manage
              ? 'Write them below.'
              : 'An admin writes them.'
        }
      />
      <Tile
        icon={<Laptop />}
        title="Machines"
        value={
          s.machines.length ? `${machines.ok} of ${s.machines.length} in line` : 'None reported'
        }
        parts={s.machines.length ? machines : undefined}
        sub={s.machines.length ? undefined : 'wp kit sync and session starts report them.'}
      />
    </div>
  )
}

function Tile({
  icon,
  title,
  value,
  sub,
  parts,
}: {
  icon: ReactNode
  title: string
  value: string
  sub?: string
  parts?: Record<SetupStatus, number>
}) {
  return (
    <section className="min-w-0 rounded-xl border border-line bg-panel px-4 py-3">
      <div className="flex items-center gap-1.5 text-xs text-subtle [&_svg]:size-3.5">
        {icon}
        {title}
      </div>
      <div className="mt-1 truncate text-[15px] font-semibold">{value}</div>
      {parts ? (
        <div className="mt-1 flex flex-wrap gap-x-3 gap-y-0.5 text-xs">
          {(['fail', 'warn', 'unread'] as const)
            .filter((k) => parts[k] > 0)
            .map((k) => (
              <span key={k} className="inline-flex items-center gap-1 text-muted">
                <Dot status={k} />
                {parts[k]} {STATUS[k].label.toLowerCase()}
              </span>
            ))}
          {parts.fail + parts.warn + parts.unread === 0 && (
            <span className="inline-flex items-center gap-1 text-muted">
              <Dot status="ok" />
              all in line
            </span>
          )}
        </div>
      ) : (
        sub && <div className="mt-1 truncate text-xs text-subtle">{sub}</div>
      )}
    </section>
  )
}

function Dot({ status }: { status: SetupStatus }) {
  return (
    <span
      aria-hidden
      className="inline-block size-2 shrink-0 rounded-full"
      style={{ background: STATUS[status].color }}
    />
  )
}

// ---- shared instructions ----

function SharedCard({ s }: { s: AgentSetup }) {
  const cur = s.shared.current
  const [draft, setDraft] = useState<string | null>(null)
  const [publishing, setPublishing] = useState(false)
  const [history, setHistory] = useState(false)
  const save = useSaveShared()
  const publish = usePublishShared()
  const refresh = useRefreshSetup()
  const behind = s.repos.filter((r) => ['missing', 'behind', 'edited'].includes(r.shared))
  const optedOut = behind.filter((r) => r.opted_out).length
  const editing = draft !== null
  return (
    <Section
      title="Shared instructions"
      helpId="agents:setup-shared"
      help={
        <p>
          Lines every repo’s agents read. Waypoint keeps them between markers in each repo’s root
          instruction file (<code>CLAUDE.md</code>, or <code>AGENTS.md</code> where{' '}
          <code>CLAUDE.md</code> points to it) through a pull request labelled to merge itself. A
          saved version goes out within the hour; <b>Publish</b> sends it now. A project can turn
          this off in its settings.
        </p>
      }
      actions={
        <div className="flex items-center gap-1">
          {s.can_edit && (
            <IconButton
              label="Read every repo again now"
              onClick={() => refresh.mutate()}
              disabled={refresh.isPending}
            >
              <RefreshCw className={refresh.isPending ? 'size-3.5 animate-spin' : 'size-3.5'} />
            </IconButton>
          )}
          {s.can_manage && !editing && (
            <>
              <Button
                size="sm"
                icon={<Pencil className="size-3.5" />}
                onClick={() => setDraft(cur?.text ?? '')}
              >
                Edit
              </Button>
              <Button
                size="sm"
                variant="primary"
                icon={<Send className="size-3.5" />}
                loading={publish.isPending}
                disabled={!cur || behind.length === optedOut}
                onClick={() => setPublishing(true)}
              >
                Publish
              </Button>
            </>
          )}
        </div>
      }
    >
      {editing ? (
        <div className="space-y-2 px-4 py-3">
          <Textarea
            aria-label="Shared instructions"
            autoFocus
            autoGrow
            value={draft}
            onChange={(e) => setDraft(e.target.value)}
            className="min-h-40 font-mono text-[12.5px]"
            placeholder={'- Run make check before you ship.\n- Ship with wp ship.'}
          />
          <div className="flex flex-wrap items-center justify-end gap-2">
            <span className="mr-auto text-xs text-subtle">
              Markdown. Saving makes version {(cur?.version ?? 0) + 1}.
            </span>
            <Button size="sm" variant="ghost" onClick={() => setDraft(null)}>
              Cancel
            </Button>
            <Button
              size="sm"
              variant="primary"
              loading={save.isPending}
              disabled={!draft.trim() || draft === cur?.text}
              onClick={() => save.mutate(draft, { onSuccess: () => setDraft(null) })}
            >
              Save
            </Button>
          </div>
        </div>
      ) : cur ? (
        <div className="px-4 py-3">
          <pre className="max-h-72 overflow-auto whitespace-pre-wrap break-words rounded-lg bg-sunken px-3 py-2 font-mono text-[12.5px] leading-relaxed">
            {cur.text}
          </pre>
          <div className="mt-2 flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-subtle">
            <span title={dateTime(cur.saved_at)}>
              Version {cur.version}
              {cur.saved_by && `, ${cur.saved_by.display_name}`}, {relativeTime(cur.saved_at)}
            </span>
            {(Object.keys(SHARED) as (keyof typeof SHARED)[])
              .filter((k) => (s.shared.counts[k] ?? 0) > 0)
              .map((k) => (
                <span key={k} title={SHARED[k].hint} className="inline-flex items-center gap-1">
                  <span
                    aria-hidden
                    className="inline-block size-1.5 rounded-full"
                    style={{ background: SHARED[k].color ?? 'var(--wp-subtle)' }}
                  />
                  {s.shared.counts[k]} {SHARED[k].label}
                </span>
              ))}
            {s.shared.versions.length > 1 && (
              <button
                type="button"
                onClick={() => setHistory((h) => !h)}
                className="ml-auto inline-flex items-center gap-1 text-muted hover:text-fg"
                aria-expanded={history}
              >
                <History className="size-3.5" />
                {history ? 'Hide' : 'Earlier versions'}
              </button>
            )}
          </div>
          {history && (
            <ol className="mt-2 space-y-2" aria-label="Earlier versions">
              {s.shared.versions.slice(1).map((v) => (
                <li key={v.version} className="rounded-lg border border-line px-3 py-2">
                  <div className="flex items-center gap-2 text-xs text-subtle">
                    <b className="font-medium text-muted">v{v.version}</b>
                    <span title={dateTime(v.saved_at)}>
                      {v.saved_by?.display_name} {relativeTime(v.saved_at)}
                    </span>
                    {s.can_manage && (
                      <button
                        type="button"
                        className="ml-auto text-accent hover:underline"
                        onClick={() => setDraft(v.text)}
                      >
                        Edit from this
                      </button>
                    )}
                  </div>
                  <pre className="mt-1 max-h-32 overflow-auto whitespace-pre-wrap break-words font-mono text-[12px] text-muted">
                    {v.text}
                  </pre>
                </li>
              ))}
            </ol>
          )}
        </div>
      ) : (
        <Empty icon={<FileText />} title="No shared instructions yet.">
          {s.can_manage
            ? 'Write the lines every repo’s agents should read; each repo gets them through a pull request.'
            : 'An admin writes the lines every repo’s agents read.'}
        </Empty>
      )}
      <Confirm
        open={publishing}
        onClose={() => setPublishing(false)}
        title={`Publish version ${cur?.version ?? ''} now?`}
        body={`Opens a pull request on ${plural(behind.length - optedOut, 'repo')} without it, labelled to merge itself once its checks pass, and closes older ones.${
          optedOut ? ` ${optedOut} opted out ${optedOut === 1 ? 'is' : 'are'} left alone.` : ''
        }`}
        confirmLabel="Publish"
        danger={false}
        onConfirm={() => publish.mutate()}
      />
    </Section>
  )
}

// ---- repos ----

type Show = 'attention' | 'all'

/** The repo's agent kit isn't current on any checkout: a roll-out is what fixes it. */
const kitBehind = (r: AgentRepo) => r.checks.some((c) => c.key === 'kit' && c.state !== 'ok')

/**
 * Rolls the kit out: each of your online runners runs wp kit sync over its
 * checkouts, which brings every repo up to date at once (an admin's action).
 */
function RollOutButton({ label, short, title }: { label: string; short?: string; title: string }) {
  const rollout = useRolloutKit()
  return (
    <Button
      size="xs"
      icon={<Rocket className="size-3" />}
      loading={rollout.isPending}
      title={title}
      aria-label={label}
      onClick={() =>
        rollout.mutate(undefined, {
          onSuccess: (rows) => {
            const n = rows.filter((x) => x.job_id).length
            toast(
              n
                ? `Kit syncing on ${plural(n, 'runner')}`
                : `Nothing started: ${rows[0]?.skipped ?? 'no runner is online'}`,
            )
          },
        })
      }
    >
      {short ? (
        <>
          <span className="sm:hidden">{short}</span>
          <span className="hidden sm:inline">{label}</span>
        </>
      ) : (
        label
      )}
    </Button>
  )
}

function ReposSection({ s, onOpen }: { s: AgentSetup; onOpen: (o: Opened) => void }) {
  const attention = s.repos.filter((r) => r.status === 'fail' || r.status === 'warn').length
  const [show, setShow] = useState<Show>('all')
  const rows =
    show === 'all' ? s.repos : s.repos.filter((r) => r.status === 'fail' || r.status === 'warn')
  const behind = s.can_manage ? s.repos.filter(kitBehind).length : 0
  return (
    <Section
      title={`Repos (${s.repos.length})`}
      helpId="agents:setup-repos"
      help={
        <p>
          Each attached repo’s agent files at the head of its default branch: every{' '}
          <code>CLAUDE.md</code> and <code>AGENTS.md</code> (subfolders too) and{' '}
          <code>.claude/</code>’s agents, skills, commands and settings. <b>Missing</b> is what most
          other repos have and this one lacks; <b>only here</b> is what no other repo has. Read
          after a push that touches them and hourly. Edits go through a pull request.
        </p>
      }
      actions={
        <>
          {behind > 1 && (
            <RollOutButton
              label={`Roll out kit to ${behind} repos`}
              short={`Roll out (${behind})`}
              title="Each of your online runners runs wp kit sync over its checkouts, which brings every repo up to date"
            />
          )}
          {attention > 0 && (
            <SegmentedControl<Show>
              label="Which repos"
              value={show}
              onChange={setShow}
              textFrom="always"
              options={[
                { value: 'all', label: 'All', icon: <List className="size-3.5" /> },
                {
                  value: 'attention',
                  label: `Differs (${attention})`,
                  icon: <ListFilter className="size-3.5" />,
                },
              ]}
            />
          )}
        </>
      }
    >
      {s.repos.length === 0 ? (
        <Empty icon={<FolderGit2 />} title="No repos attached to your projects." />
      ) : (
        <>
          <CommonLine common={s.repo_common} of="repos" />
          <ul>
            {rows.map((r) => (
              <RepoRow
                key={`${r.forge_id}:${r.repo}`}
                r={r}
                onOpen={onOpen}
                canRollOut={s.can_manage}
              />
            ))}
          </ul>
        </>
      )}
    </Section>
  )
}

/** What most repos (machines) have: absence of these counts as missing. */
function CommonLine({ common, of }: { common: CommonComponent[]; of: string }) {
  const shown = common.filter((c) => c.common)
  if (shown.length === 0) return null
  return (
    <p className="border-b border-line px-4 py-2 text-xs text-subtle">
      Most {of} have{' '}
      {shown.map((c, i) => (
        <span key={c.kind + c.name}>
          {i > 0 && ', '}
          <span className="text-muted">{c.name}</span>{' '}
          <span className="text-subtle">({KIND_LABEL[c.kind][0]})</span>
        </span>
      ))}
      .
    </p>
  )
}

function RepoRow({
  r,
  onOpen,
  canRollOut,
}: {
  r: AgentRepo
  onOpen: (o: Opened) => void
  canRollOut: boolean
}) {
  const [expanded, setExpanded] = useState(false)
  // The shared block's badge and pull request say where it stands; its
  // check line only when publishing failed.
  const problems = r.checks.filter(
    (c) => c.state !== 'ok' && (c.key !== 'shared' || !!r.shared_error),
  )
  const only = r.components.filter((c) => c.only_here)
  const url = safeUrl(r.url)
  const sharedInfo = SHARED[r.shared]
  // The row says what is wrong; opening it adds only what it doesn't.
  const passing = r.checks.filter((c) => !problems.includes(c))
  const shownError = problems.some((c) => c.key === 'shared' && c.note === r.shared_error)
  const detail =
    !!url ||
    !!r.head ||
    r.files.length > 0 ||
    r.components.length > 0 ||
    r.prs.length > 0 ||
    passing.length > 0 ||
    (!!r.shared_error && !shownError)
  return (
    <li className="border-b border-line last:border-b-0">
      <div className="flex items-start gap-3 px-4 py-2.5 text-[13px]">
        <button
          type="button"
          className="mt-0.5 flex min-w-0 flex-1 items-start gap-2 text-left"
          onClick={() => detail && setExpanded((e) => !e)}
          aria-expanded={detail ? expanded : undefined}
        >
          <span
            className="mt-1.5"
            role="img"
            aria-label={STATUS[r.status].label}
            title={STATUS[r.status].label}
          >
            <Dot status={r.status} />
          </span>
          <span className="min-w-0 flex-1">
            <span className="flex flex-wrap items-baseline gap-x-2">
              <span className="font-medium">{r.repo}</span>
              <span className="text-xs text-subtle">{r.projects.join(', ')}</span>
            </span>
            {r.error ? (
              <span className="block text-xs text-danger">{r.error}</span>
            ) : (
              <span className="block text-xs text-subtle">
                {r.head
                  ? `${plural(r.files.length, 'agent file')}${summary(r.components)}`
                  : 'Not read yet'}
              </span>
            )}
            {problems.map((c) => (
              <CheckLine key={c.key} c={c} />
            ))}
            {r.missing.length > 0 && (
              <span className="mt-0.5 block text-xs text-warn">
                Missing {r.missing.map((m) => `${m.name} (${KIND_LABEL[m.kind][0]})`).join(', ')}
              </span>
            )}
            {only.length > 0 && (
              <span className="mt-0.5 block text-xs text-muted">
                Only here: {only.map((m) => `${m.name} (${KIND_LABEL[m.kind][0]})`).join(', ')}
              </span>
            )}
          </span>
        </button>
        <div className="flex shrink-0 flex-col items-end gap-1 text-xs">
          <div className="flex items-center gap-1.5">
            {r.opted_out && (
              <Badge>
                <span title="One of its projects turned “Keep this project’s repos in line” off">
                  opted out
                </span>
              </Badge>
            )}
            {r.shared !== 'none' && r.shared !== 'unread' && (
              <span title={sharedInfo.hint}>
                <Badge color={sharedInfo.color}>{sharedInfo.label}</Badge>
              </span>
            )}
          </div>
          {r.shared_pr && <PRLink pr={r.shared_pr} />}
          {canRollOut && kitBehind(r) && (
            <RollOutButton
              label="Roll out"
              title="Each of your online runners runs wp kit sync over its checkouts"
            />
          )}
          {detail && (
            <ChevronDown
              aria-hidden
              className={clsx(
                'size-3.5 text-subtle transition-transform',
                expanded && 'rotate-180',
              )}
            />
          )}
        </div>
      </div>
      {expanded && (
        <div className="space-y-3 border-t border-dashed border-line bg-sunken/40 px-4 py-3 text-[13px]">
          {r.shared_error && !shownError && (
            <p className="flex items-start gap-1.5 text-xs text-danger" role="alert">
              <CircleAlert className="mt-0.5 size-3.5 shrink-0" />
              Couldn’t publish here: {r.shared_error}
            </p>
          )}
          <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-subtle">
            {url && (
              <a
                href={url}
                target="_blank"
                rel="noreferrer"
                className="inline-flex items-center gap-1 hover:text-fg"
              >
                <ExternalLink className="size-3" /> On the forge
              </a>
            )}
            {r.branch && r.head && (
              <span className="font-mono" title={r.head}>
                {r.branch}@{r.head.slice(0, 8)}
              </span>
            )}
            {r.scanned_at && (
              <span title={dateTime(r.scanned_at)}>read {relativeTime(r.scanned_at)}</span>
            )}
            {r.root_file && <span>shared block in {r.root_file}</span>}
            {r.kit_plugins.length > 0 && <span>kit: {r.kit_plugins.join(', ')}</span>}
          </div>
          {passing.length > 0 && (
            <div>
              <h4 className="mb-1 text-xs font-medium text-muted">Passing checks</h4>
              <ul className="space-y-0.5">
                {passing.map((c) => (
                  <CheckLine key={c.key} c={c} full />
                ))}
              </ul>
            </div>
          )}
          <Files
            label="Agent instruction files"
            hint="The CLAUDE.md and AGENTS.md files, and .claude/ settings, that agents read in this repo, from its default branch"
            files={r.files}
            onOpen={(i) => onOpen({ kind: 'repo', repo: r, index: i })}
            action={
              r.can_edit &&
              r.head && (
                <IconButton
                  size="xs"
                  label="New file"
                  onClick={() => onOpen({ kind: 'new', repo: r })}
                >
                  <Plus className="size-3" />
                </IconButton>
              )
            }
          />
          <Components components={r.components} />
          {r.prs.length > 0 && (
            <div>
              <h4 className="mb-1 text-xs font-medium text-muted">Pull requests from here</h4>
              <ul className="space-y-0.5 text-xs">
                {r.prs.map((p) => (
                  <li key={p.id} className="flex flex-wrap items-center gap-x-2">
                    <PRLink pr={p} />
                    <span className="text-subtle">
                      {p.kind === 'shared' ? `shared v${p.version}` : p.paths.join(', ')}
                      {p.opened_by && ` · ${p.opened_by.display_name}`} ·{' '}
                      {relativeTime(p.opened_at)}
                    </span>
                    {p.error && <span className="text-danger">{p.error}</span>}
                  </li>
                ))}
              </ul>
            </div>
          )}
        </div>
      )}
    </li>
  )
}

function summary(cs: AgentComponent[]) {
  const by = new Map<AgentComponent['kind'], number>()
  for (const c of cs) by.set(c.kind, (by.get(c.kind) ?? 0) + 1)
  const parts = (Object.keys(KIND_LABEL) as AgentComponent['kind'][])
    .filter((k) => by.get(k))
    .map((k) => plural(by.get(k)!, ...KIND_LABEL[k]))
  return parts.length ? ' · ' + parts.join(', ') : ''
}

const CHECK_COLOR = { ok: 'var(--wp-ok)', warn: 'var(--wp-warn)', fail: 'var(--wp-danger)' }

function CheckLine({ c, full }: { c: SetupCheck; full?: boolean }) {
  const Tag = full ? 'li' : 'span'
  return (
    <Tag className="mt-0.5 flex items-start gap-1.5 text-xs">
      <span
        aria-label={c.state}
        className="mt-1 inline-block size-1.5 shrink-0 rounded-full"
        style={{ background: CHECK_COLOR[c.state] }}
      />
      <span className={c.state === 'fail' ? 'text-danger' : c.state === 'warn' ? 'text-warn' : ''}>
        {c.label}
        {c.note && <span className="text-subtle">: {c.note}</span>}
      </span>
    </Tag>
  )
}

function PRLink({ pr }: { pr: AgentFilePR }) {
  const url = safeUrl(pr.url)
  const color =
    pr.state === 'merged'
      ? 'var(--wp-ok)'
      : pr.state === 'failed'
        ? 'var(--wp-danger)'
        : pr.state === 'open'
          ? 'var(--wp-accent)'
          : undefined
  const label = (
    <Badge color={color}>
      <GitPullRequest className="size-3" />
      {pr.number ? `#${pr.number}` : ''} {pr.state}
    </Badge>
  )
  return url ? (
    <a href={url} target="_blank" rel="noreferrer" className="hover:underline">
      {label}
    </a>
  ) : (
    label
  )
}

function Files({
  files,
  onOpen,
  action,
  label,
  hint,
}: {
  label: string
  hint: string
  files: AgentFile[]
  onOpen: (index: number) => void
  action?: ReactNode
}) {
  return (
    <div>
      <div className="mb-1 flex items-center justify-between">
        <h4 className="text-xs font-medium text-muted" title={hint}>
          {label}
        </h4>
        {action}
      </div>
      {files.length === 0 ? (
        <p className="text-xs text-subtle">None yet.</p>
      ) : (
        <ul className="grid gap-x-4 sm:grid-cols-2">
          {files.map((f, i) => (
            <li key={f.path} className="min-w-0">
              <button
                type="button"
                onClick={() => onOpen(i)}
                className="flex w-full min-w-0 items-center gap-1.5 rounded px-1 py-0.5 text-left text-xs hover:bg-hover"
              >
                <FileText className="size-3 shrink-0 text-subtle" />
                <span className="min-w-0 flex-1 truncate font-mono" title={f.path}>
                  {f.path}
                </span>
                <span className="shrink-0 text-subtle">
                  {f.truncated && f.lines === 0 ? 'binary' : `${f.lines} ln`}
                </span>
              </button>
            </li>
          ))}
        </ul>
      )}
    </div>
  )
}

function Components({ components }: { components: SetupComponent[] }) {
  const groups = useMemo(() => {
    const m = new Map<SetupComponent['kind'], SetupComponent[]>()
    for (const c of components) m.set(c.kind, [...(m.get(c.kind) ?? []), c])
    return [...m.entries()]
  }, [components])
  if (groups.length === 0) return null
  return (
    <div>
      <h4 className="mb-1 text-xs font-medium text-muted">Components</h4>
      <dl className="space-y-1 text-xs">
        {groups.map(([kind, cs]) => (
          <div key={kind} className="flex min-w-0 gap-2">
            <dt className="w-24 shrink-0 text-subtle">{plural(cs.length, ...KIND_LABEL[kind])}</dt>
            <dd className="flex min-w-0 flex-wrap gap-1">
              {cs.map((c) => (
                <span
                  key={c.path + c.name}
                  title={[c.detail, c.path].filter(Boolean).join('\n')}
                  className={clsx(
                    'rounded px-1.5 py-px',
                    c.only_here ? 'bg-accent/10 text-accent' : 'bg-sunken text-muted',
                  )}
                >
                  {c.name}
                  {c.only_here && <span className="sr-only"> (only here)</span>}
                </span>
              ))}
            </dd>
          </div>
        ))}
      </dl>
    </div>
  )
}

// ---- machines ----

function MachinesSection({ s, onOpen }: { s: AgentSetup; onOpen: (o: Opened) => void }) {
  return (
    <Section
      title={`Machines (${s.machines.length})`}
      helpId="agents:setup-machines"
      help={
        <p>
          Each machine’s own agent files from its home folder: <code>~/.claude/CLAUDE.md</code>, its
          agents, commands, skills and hooks, <code>settings.json</code> (env values and keys hidden
          before they leave the machine), the names of its MCP servers, and{' '}
          <code>~/.codex/AGENTS.md</code>; with checks that wp’s hooks and the agent kit are set up.{' '}
          <code>wp kit sync</code> and session starts (every 6 hours) send them;{' '}
          <code>wp kit report</code> sends them now. You see your own; admins see everyone’s.
        </p>
      }
    >
      {s.machines.length === 0 ? (
        <Empty icon={<Laptop />} title="No machine has reported yet.">
          Run <code>wp kit report</code> on one.
        </Empty>
      ) : (
        <>
          <CommonLine common={s.machine_common} of="machines" />
          <ul>
            {s.machines.map((m) => (
              <MachineRow
                key={`${m.user.id}:${m.machine}`}
                m={m}
                onOpen={onOpen}
                many={s.can_manage}
              />
            ))}
          </ul>
        </>
      )}
    </Section>
  )
}

function MachineRow({
  m,
  onOpen,
  many,
}: {
  m: AgentMachine
  onOpen: (o: Opened) => void
  many: boolean
}) {
  const [expanded, setExpanded] = useState(false)
  const problems = m.checks.filter((c) => c.state !== 'ok')
  const only = m.components.filter((c) => c.only_here)
  const passing = m.checks.filter((c) => c.state === 'ok')
  const detail = passing.length > 0 || m.files.length > 0 || m.components.length > 0
  return (
    <li className="border-b border-line last:border-b-0">
      <button
        type="button"
        className="flex w-full items-start gap-2 px-4 py-2.5 text-left text-[13px]"
        onClick={() => detail && setExpanded((e) => !e)}
        aria-expanded={detail ? expanded : undefined}
      >
        <span
          className="mt-1.5"
          role="img"
          aria-label={STATUS[m.status].label}
          title={STATUS[m.status].label}
        >
          <Dot status={m.status} />
        </span>
        <span className="min-w-0 flex-1">
          <span className="flex flex-wrap items-baseline gap-x-2">
            <span className="font-medium">{m.machine}</span>
            {many && <span className="text-xs text-subtle">{m.user.display_name}</span>}
          </span>
          <span className="block text-xs text-subtle">
            {m.os} · wp {m.wp_version || '?'} · {plural(m.files.length, 'file')}
            {summary(m.components)}
          </span>
          {problems.map((c) => (
            <CheckLine key={c.key} c={c} />
          ))}
          {m.missing.length > 0 && (
            <span className="mt-0.5 block text-xs text-warn">
              Missing {m.missing.map((x) => `${x.name} (${KIND_LABEL[x.kind][0]})`).join(', ')}
            </span>
          )}
          {only.length > 0 && (
            <span className="mt-0.5 block text-xs text-muted">
              Only here: {only.map((x) => `${x.name} (${KIND_LABEL[x.kind][0]})`).join(', ')}
            </span>
          )}
        </span>
        <span className="flex shrink-0 flex-col items-end gap-1 text-xs text-subtle">
          <time title={dateTime(m.reported_at)}>{relativeTime(m.reported_at)}</time>
          {detail && (
            <ChevronDown
              aria-hidden
              className={clsx('size-3.5 transition-transform', expanded && 'rotate-180')}
            />
          )}
        </span>
      </button>
      {expanded && (
        <div className="space-y-3 border-t border-dashed border-line bg-sunken/40 px-4 py-3 text-[13px]">
          {passing.length > 0 && (
            <div>
              <h4 className="mb-1 text-xs font-medium text-muted">Passing checks</h4>
              <ul className="space-y-0.5">
                {passing.map((c) => (
                  <CheckLine key={c.key} c={c} full />
                ))}
              </ul>
            </div>
          )}
          <Files
            label="Agent files"
            hint="This machine’s home-folder CLAUDE.md, agents, commands, skills, hooks and settings"
            files={m.files}
            onOpen={(i) => onOpen({ kind: 'machine', machine: m, index: i })}
          />
          <Components components={m.components} />
        </div>
      )}
    </li>
  )
}

// ---- one file ----

function FileDialog({
  opened,
  onMove,
  onClose,
}: {
  opened: Exclude<Opened, { kind: 'new' }>
  onMove: (o: Opened) => void
  onClose: () => void
}) {
  const files = opened.kind === 'repo' ? opened.repo.files : opened.machine.files
  const file = files[opened.index]
  const query =
    opened.kind === 'repo'
      ? q.agentFile(opened.repo.forge_id, opened.repo.repo, file.path)
      : q.machineFile(opened.machine.user.id, opened.machine.machine, file.path)
  const f = useQuery(query)
  const [draft, setDraft] = useState<string | null>(null)
  const [message, setMessage] = useState('')
  // The file's blob id when the edit began: a refetch while editing mustn't
  // move it, or the server's check against a changed file would pass.
  const [baseSha, setBaseSha] = useState('')
  const edit = useEditAgentFile()
  const move = (step: number) => {
    setDraft(null)
    onMove({ ...opened, index: (opened.index + step + files.length) % files.length })
  }
  const where = opened.kind === 'repo' ? opened.repo.repo : opened.machine.machine
  const url = safeUrl(f.data?.url)
  const editing = draft !== null
  return (
    <Dialog
      open
      wide
      onClose={onClose}
      title={
        <span className="flex min-w-0 items-baseline gap-2">
          <span className="truncate font-mono text-[13px]">{file.path}</span>
          <span className="shrink-0 text-xs font-normal text-subtle">{where}</span>
        </span>
      }
      footer={
        editing ? (
          <>
            <Input
              aria-label="What changed"
              placeholder="What changed (the pull request’s title)"
              value={message}
              onChange={(e) => setMessage(e.target.value)}
              className="mr-auto h-7 min-w-0 flex-1 text-[13px]"
            />
            <Button size="sm" variant="ghost" onClick={() => setDraft(null)}>
              Cancel
            </Button>
            <Button
              size="sm"
              variant="primary"
              icon={<GitPullRequest className="size-3.5" />}
              loading={edit.isPending}
              disabled={draft === f.data?.content || opened.kind !== 'repo'}
              onClick={() =>
                opened.kind === 'repo' &&
                edit.mutate(
                  {
                    forge_id: opened.repo.forge_id,
                    repo: opened.repo.repo,
                    path: file.path,
                    content: draft,
                    base_sha: baseSha,
                    message: message.trim() || undefined,
                  },
                  { onSuccess: () => setDraft(null) },
                )
              }
            >
              Open pull request
            </Button>
          </>
        ) : (
          <>
            <span className="mr-auto flex items-center gap-1">
              <IconButton
                label="Previous file"
                onClick={() => move(-1)}
                disabled={files.length < 2}
              >
                <ChevronLeft className="size-4" />
              </IconButton>
              <span className="text-xs text-subtle">
                {opened.index + 1} / {files.length}
              </span>
              <IconButton label="Next file" onClick={() => move(1)} disabled={files.length < 2}>
                <ChevronRight className="size-4" />
              </IconButton>
            </span>
            {url && (
              <a
                href={url}
                target="_blank"
                rel="noreferrer"
                className="inline-flex items-center gap-1 text-xs text-muted hover:text-fg"
              >
                <ExternalLink className="size-3" /> On the forge
              </a>
            )}
            {f.data?.editable && (
              <Button
                size="sm"
                icon={<Pencil className="size-3.5" />}
                onClick={() => {
                  setMessage('')
                  setBaseSha(f.data?.sha ?? '')
                  setDraft(f.data?.content ?? '')
                }}
              >
                Edit
              </Button>
            )}
          </>
        )
      }
    >
      <div className="px-4 py-3">
        {f.isPending ? (
          <Skeleton className="h-48" />
        ) : f.error || !f.data ? (
          <p className="text-[13px] text-danger">Couldn’t load this file.</p>
        ) : editing ? (
          <Textarea
            aria-label={file.path}
            autoFocus
            value={draft}
            onChange={(e) => setDraft(e.target.value)}
            spellCheck={false}
            className="h-[55vh] font-mono text-[12.5px]"
          />
        ) : (
          <>
            <div className="mb-2 flex flex-wrap gap-x-3 text-xs text-subtle">
              <span>{f.data.kind}</span>
              <span>
                {plural(f.data.lines, 'line')}, {formatBytes(f.data.size)}
              </span>
              {f.data.truncated && <span className="text-warn">only the first 64 KB is kept</span>}
              {f.data.shared !== undefined && <span>has the shared block</span>}
              {opened.kind === 'machine' && <span>change it on that machine</span>}
            </div>
            <pre className="max-h-[60vh] overflow-auto whitespace-pre-wrap break-words rounded-lg bg-sunken px-3 py-2 font-mono text-[12.5px] leading-relaxed">
              {f.data.content || <span className="text-subtle">(empty)</span>}
            </pre>
          </>
        )}
      </div>
    </Dialog>
  )
}

function formatBytes(n: number) {
  return n < 1024 ? `${n} B` : `${(n / 1024).toFixed(n < 10240 ? 1 : 0)} KB`
}

const NEW_FILE_HINTS = [
  'CLAUDE.md',
  'AGENTS.md',
  '.claude/agents/',
  '.claude/commands/',
  '.claude/skills/',
]

function NewFileDialog({ repo, onClose }: { repo: AgentRepo; onClose: () => void }) {
  const [path, setPath] = useState(
    repo.files.some((f) => f.path === 'CLAUDE.md') ? '' : 'CLAUDE.md',
  )
  const [content, setContent] = useState('')
  const [message, setMessage] = useState('')
  const edit = useEditAgentFile()
  const taken = repo.files.some((f) => f.path === path.trim())
  return (
    <Dialog
      open
      wide
      onClose={onClose}
      title={`New agent file in ${repo.repo}`}
      footer={
        <>
          <Button size="sm" variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button
            size="sm"
            variant="primary"
            icon={<GitPullRequest className="size-3.5" />}
            loading={edit.isPending}
            disabled={!path.trim() || taken}
            onClick={() =>
              edit.mutate(
                {
                  forge_id: repo.forge_id,
                  repo: repo.repo,
                  path: path.trim(),
                  content,
                  base_sha: '',
                  message: message.trim() || undefined,
                },
                { onSuccess: onClose },
              )
            }
          >
            Open pull request
          </Button>
        </>
      }
    >
      <div className="space-y-3 px-4 py-3">
        <Field
          label="Path"
          error={taken ? 'That file is there already: open it to edit it.' : undefined}
          note={
            <span className="flex flex-wrap gap-1">
              {NEW_FILE_HINTS.map((h) => (
                <button
                  key={h}
                  type="button"
                  className="rounded bg-sunken px-1.5 font-mono hover:bg-hover"
                  onClick={() => setPath(h)}
                >
                  {h}
                </button>
              ))}
            </span>
          }
        >
          <Input
            value={path}
            onChange={(e) => setPath(e.target.value)}
            placeholder="docs/CLAUDE.md"
            className="font-mono text-[13px]"
            autoFocus
          />
        </Field>
        <Textarea
          aria-label="Content"
          value={content}
          onChange={(e) => setContent(e.target.value)}
          spellCheck={false}
          className="h-[40vh] font-mono text-[12.5px]"
        />
        <Input
          aria-label="What changed"
          placeholder="What it is for (the pull request’s title)"
          value={message}
          onChange={(e) => setMessage(e.target.value)}
          className="text-[13px]"
        />
      </div>
    </Dialog>
  )
}
