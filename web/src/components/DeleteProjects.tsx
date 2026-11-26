import { useMemo, useState } from 'react'
import { useNavigate, useRouterState } from '@tanstack/react-router'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { Search } from 'lucide-react'
import { api, enc } from '@/lib/api'
import { q } from '@/lib/queries'
import { projectTree } from '@/lib/agents'
import type { Project } from '@/lib/types'
import { Dialog } from '@/components/ui/dialog'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { toast, toastError } from '@/components/ui/toast'
import { ProjectIcon } from '@/components/issue/icons'

/**
 * Delete one project (only), or pick several to delete at once. Lists the
 * projects the viewer owns; checking one checks its sub-projects too.
 * Mount it to open it: it starts fresh each time.
 */
export function DeleteProjectsDialog({
  onClose,
  only,
}: {
  onClose: () => void
  only?: Project | null
}) {
  const qc = useQueryClient()
  const navigate = useNavigate()
  const pathname = useRouterState({ select: (s) => s.location.pathname })
  const all = useQuery(q.projects()).data
  const owned = useMemo(() => (all ?? []).filter((p) => p.role === 'owner'), [all])
  const tree = useMemo(() => projectTree(owned, 'recent'), [owned])
  const [picked, setPicked] = useState<Set<number>>(() => new Set(only ? [only.id] : []))
  const [filter, setFilter] = useState('')
  const [confirming, setConfirming] = useState(!!only)
  const [done, setDone] = useState<number | null>(null)

  const doomed = owned.filter((p) => picked.has(p.id))
  const openIssues = doomed.reduce((n, p) => n + p.open_issues, 0)
  const needle = filter.trim().toLowerCase()
  const shown = needle
    ? tree.filter(
        ({ project: p }) =>
          p.name.toLowerCase().includes(needle) || p.key.toLowerCase().includes(needle),
      )
    : tree
  const toggle = (id: number, on: boolean) => {
    const next = new Set(picked)
    for (const n of tree)
      if (n.project.id === id || n.ancestors.includes(id))
        if (on) next.add(n.project.id)
        else next.delete(n.project.id)
    setPicked(next)
  }
  const allShown = shown.length > 0 && shown.every((n) => picked.has(n.project.id))
  const setShown = (on: boolean) => {
    const next = new Set(picked)
    for (const n of shown)
      if (on) next.add(n.project.id)
      else next.delete(n.project.id)
    setPicked(next)
  }

  const busy = done !== null
  const run = async () => {
    setDone(0)
    const queue = [...doomed]
    const failed = new Set<number>()
    let err: unknown
    let count = 0
    await Promise.all(
      Array.from({ length: Math.min(4, queue.length) }, async () => {
        for (let p = queue.shift(); p; p = queue.shift()) {
          try {
            await api.del(`/projects/${enc(p.key)}`)
            qc.removeQueries({ queryKey: ['project', p.key] })
          } catch (e) {
            failed.add(p.id)
            err = e
          }
          setDone(++count)
        }
      }),
    )
    qc.invalidateQueries()
    const gone = doomed.filter((p) => !failed.has(p.id))
    if (gone.some((p) => pathname === `/p/${p.key}` || pathname.startsWith(`/p/${p.key}/`)))
      navigate({ to: '/' })
    if (failed.size) {
      toastError(err)
      setDone(null)
      setPicked(new Set(doomed.filter((p) => failed.has(p.id)).map((p) => p.id)))
      setConfirming(false)
      return
    }
    toast(gone.length === 1 ? `Deleted ${gone[0].name}` : `Deleted ${gone.length} projects`)
    onClose()
  }

  const single = doomed.length === 1 ? doomed[0] : null
  const close = busy ? () => {} : onClose
  if (confirming)
    return (
      <Dialog
        open
        onClose={close}
        title={single ? `Delete ${single.name}?` : `Delete ${doomed.length} projects?`}
        footer={
          <>
            <Button
              variant="ghost"
              disabled={busy}
              onClick={only ? onClose : () => setConfirming(false)}
            >
              {only ? 'Cancel' : 'Back'}
            </Button>
            <Button variant="danger" loading={busy} disabled={doomed.length === 0} onClick={run}>
              {busy
                ? `Deleting ${done} of ${doomed.length}…`
                : single
                  ? 'Delete forever'
                  : `Delete ${doomed.length} projects`}
            </Button>
          </>
        }
      >
        <div className="space-y-3 p-4 text-[13px] text-muted">
          <p>
            This permanently deletes{' '}
            {single ? 'the project and all its' : 'these projects and all their'} issues
            {openIssues > 0 && ` (${openIssues} open)`}, comments and history. It can't be undone.
          </p>
          {!single && (
            <ul className="max-h-48 space-y-1 overflow-auto rounded-lg border border-line p-2">
              {doomed.map((p) => (
                <li key={p.id} className="flex items-center gap-2 text-fg">
                  <ProjectIcon icon={p.icon} color={p.color} name={p.name} size={16} />
                  <span className="min-w-0 flex-1 truncate">{p.name}</span>
                  <span className="font-mono text-xs text-subtle">{p.key}</span>
                </li>
              ))}
            </ul>
          )}
          <p className="text-xs text-subtle">
            Repositories attached to {single ? 'it' : 'them'} won't be offered again by automatic
            projects or Discover repositories; attach one again from a project's settings.
          </p>
        </div>
      </Dialog>
    )

  return (
    <Dialog
      open
      onClose={close}
      title="Delete projects"
      footer={
        <>
          <span className="mr-auto text-xs text-subtle">
            {picked.size ? `${picked.size} selected` : 'Pick projects to delete'}
          </span>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button
            variant="danger"
            disabled={doomed.length === 0}
            onClick={() => setConfirming(true)}
          >
            {doomed.length > 1 ? `Delete ${doomed.length}…` : 'Delete…'}
          </Button>
        </>
      }
    >
      <div className="sticky top-0 z-10 space-y-2 border-b border-line bg-raised p-3">
        {owned.length > 8 && (
          <div className="relative">
            <Search className="pointer-events-none absolute left-2.5 top-1/2 size-3.5 -translate-y-1/2 text-subtle" />
            <Input
              value={filter}
              onChange={(e) => setFilter(e.target.value)}
              placeholder="Filter projects"
              aria-label="Filter projects"
              className="h-8 pl-8 text-[13px]"
            />
          </div>
        )}
        <label className="flex cursor-pointer items-center gap-2.5 px-1 text-xs text-muted">
          <input
            type="checkbox"
            checked={allShown}
            onChange={(e) => setShown(e.target.checked)}
            className="size-4 shrink-0 accent-[var(--wp-accent)]"
          />
          {needle ? `Select all ${shown.length} matching` : `Select all ${shown.length}`}
        </label>
      </div>
      {owned.length === 0 ? (
        <p className="p-4 text-[13px] text-muted">You don't own any projects.</p>
      ) : shown.length === 0 ? (
        <p className="p-4 text-[13px] text-muted">No projects match.</p>
      ) : (
        <ul className="p-1.5">
          {shown.map(({ project: p, depth }) => (
            <li key={p.id}>
              <label
                className="flex min-h-9 cursor-pointer items-center gap-2.5 rounded-lg px-2 text-[13px] hover:bg-hover"
                style={needle ? undefined : { paddingLeft: 8 + depth * 18 }}
              >
                <input
                  type="checkbox"
                  checked={picked.has(p.id)}
                  onChange={(e) => toggle(p.id, e.target.checked)}
                  className="size-4 shrink-0 accent-[var(--wp-accent)]"
                />
                <ProjectIcon icon={p.icon} color={p.color} name={p.name} size={18} />
                <span className="min-w-0 flex-1 truncate">{p.name}</span>
                <span className="hidden font-mono text-xs text-subtle sm:inline">{p.key}</span>
                <span className="w-14 text-right text-xs tabular-nums text-subtle">
                  {p.open_issues} open
                </span>
              </label>
            </li>
          ))}
        </ul>
      )}
    </Dialog>
  )
}
