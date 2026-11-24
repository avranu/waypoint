import { useEffect, useMemo, useRef, useState } from 'react'
import { Search } from 'lucide-react'
import type { DiscoveredRepo, Forge } from '@/lib/types'
import { Dialog } from '@/components/ui/dialog'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Badge } from '@/components/ui/misc'

/** More new projects than this and discovery asks first. */
export const ASK_ABOVE = 5

/** How many projects attaching these repos makes, organisations' included. */
export function projectsMade(repos: DiscoveredRepo[]): number {
  const orgs = new Set(
    repos.filter((r) => r.new_namespace).map((r) => r.namespace?.toLowerCase() ?? ''),
  )
  return repos.filter((r) => !r.joins).length + orgs.size
}

const owner = (repo: string) => repo.slice(0, repo.lastIndexOf('/'))
const short = (repo: string) => repo.slice(repo.lastIndexOf('/') + 1)

/**
 * Lets an admin uncheck repos before discovery makes projects for them.
 * Unchecked repos are skipped for good (automatic projects leave them be).
 */
export function DiscoverPicker({
  forge,
  repos,
  busy,
  onCancel,
  onConfirm,
  onUnignore,
}: {
  forge: Forge | null
  repos: DiscoveredRepo[]
  busy: boolean
  onCancel: () => void
  onConfirm: (attach: string[], skip: string[]) => void
  /** offers an ignored repo again */
  onUnignore: (repo: string) => void
}) {
  const [picked, setPicked] = useState<Set<string>>(new Set())
  const [filter, setFilter] = useState('')
  const known = useRef<Set<string>>(new Set())
  const ignored = useMemo(() => repos.filter((r) => r.ignored), [repos])
  const offered = useMemo(() => repos.filter((r) => !r.ignored), [repos])
  useEffect(() => {
    if (!forge) {
      known.current = new Set()
      return
    }
    const fresh = offered.filter((r) => !known.current.has(r.repo)).map((r) => r.repo)
    known.current = new Set(offered.map((r) => r.repo))
    setPicked((prev) => new Set([...[...prev].filter((r) => known.current.has(r)), ...fresh]))
  }, [forge, offered])
  useEffect(() => setFilter(''), [forge])
  const groups = useMemo(() => {
    const m = new Map<string, DiscoveredRepo[]>()
    for (const r of offered) m.set(owner(r.repo), [...(m.get(owner(r.repo)) ?? []), r])
    return [...m.entries()].sort(([a], [b]) => a.localeCompare(b))
  }, [offered])
  const needle = filter.trim().toLowerCase()
  const shown = groups
    .map(([g, list]) => [g, list.filter((r) => r.repo.toLowerCase().includes(needle))] as const)
    .filter(([, list]) => list.length > 0)
  const shownIgnored = ignored.filter((r) => r.repo.toLowerCase().includes(needle))
  const set = (list: DiscoveredRepo[], on: boolean) => {
    const next = new Set(picked)
    for (const r of list)
      if (on) next.add(r.repo)
      else next.delete(r.repo)
    setPicked(next)
  }
  const visible = shown.flatMap(([, list]) => list)
  const chosen = offered.filter((r) => picked.has(r.repo))
  const made = projectsMade(chosen)
  const box = 'size-4 shrink-0 accent-[var(--wp-accent)]'
  return (
    <Dialog
      open={!!forge}
      onClose={busy ? () => {} : onCancel}
      title={`New repositories on ${forge?.name ?? ''}`}
      footer={
        <>
          <span className="mr-auto text-xs text-subtle">
            {chosen.length} of {offered.length} selected
          </span>
          <Button variant="ghost" disabled={busy} onClick={onCancel}>
            Cancel
          </Button>
          <Button
            variant="primary"
            loading={busy}
            onClick={() =>
              onConfirm(
                chosen.map((r) => r.repo),
                offered.filter((r) => !picked.has(r.repo)).map((r) => r.repo),
              )
            }
          >
            {offered.length === 0
              ? 'Done'
              : chosen.length === 0
                ? 'Skip all'
                : made === 0
                  ? 'Attach'
                  : `Create ${made} ${made === 1 ? 'project' : 'projects'}`}
          </Button>
        </>
      }
    >
      <div className="sticky top-0 z-10 space-y-2 border-b border-line bg-raised p-3">
        <p className="text-[13px] text-muted">
          These would make {projectsMade(offered)} projects. Uncheck the repositories you don't
          want: they won't get projects now or later. You can still attach one from a project's
          settings.
        </p>
        {repos.length > 10 && (
          <div className="relative">
            <Search className="pointer-events-none absolute left-2.5 top-1/2 size-3.5 -translate-y-1/2 text-subtle" />
            <Input
              value={filter}
              onChange={(e) => setFilter(e.target.value)}
              placeholder="Filter repositories"
              aria-label="Filter repositories"
              className="h-8 pl-8 text-[13px]"
            />
          </div>
        )}
        <label className="flex cursor-pointer items-center gap-2.5 px-1 text-xs text-muted">
          <input
            type="checkbox"
            className={box}
            checked={visible.length > 0 && visible.every((r) => picked.has(r.repo))}
            onChange={(e) => set(visible, e.target.checked)}
          />
          {needle ? `Select all ${visible.length} matching` : `Select all ${visible.length}`}
        </label>
      </div>
      <div className="space-y-2 p-1.5">
        {shown.length === 0 && (
          <p className="p-2.5 text-[13px] text-muted">
            {offered.length === 0
              ? 'Nothing new to offer.'
              : shownIgnored.length > 0
                ? 'No offered repositories match.'
                : 'No repositories match.'}
          </p>
        )}
        {shown.map(([g, list]) => (
          <div key={g}>
            <label className="flex min-h-9 cursor-pointer items-center gap-2.5 rounded-lg px-2 text-[13px] font-medium hover:bg-hover">
              <input
                type="checkbox"
                className={box}
                checked={list.every((r) => picked.has(r.repo))}
                ref={(el) => {
                  if (el)
                    el.indeterminate =
                      list.some((r) => picked.has(r.repo)) && !list.every((r) => picked.has(r.repo))
                }}
                onChange={(e) => set(list, e.target.checked)}
              />
              <span className="min-w-0 flex-1 truncate font-mono">{g}</span>
              {list.some((r) => r.new_namespace) && <Badge>new parent project</Badge>}
            </label>
            <ul>
              {list.map((r) => (
                <li key={r.repo}>
                  <label className="flex min-h-9 cursor-pointer items-center gap-2.5 rounded-lg py-1 pl-8 pr-2 text-[13px] hover:bg-hover">
                    <input
                      type="checkbox"
                      className={box}
                      checked={picked.has(r.repo)}
                      onChange={(e) => set([r], e.target.checked)}
                    />
                    <span className="min-w-0 flex-1 truncate font-mono">{short(r.repo)}</span>
                    {r.joins && (
                      <span className="max-w-[45%] truncate text-xs text-subtle">
                        joins {r.joins}
                      </span>
                    )}
                  </label>
                </li>
              ))}
            </ul>
          </div>
        ))}
        {shownIgnored.length > 0 && (
          <div>
            <p className="px-2 pt-2 text-[13px] font-medium">Ignored</p>
            <p className="px-2 pb-1 text-xs text-subtle">
              Left out earlier, or from a deleted project, so automatic projects skip them.
            </p>
            <ul>
              {shownIgnored.map((r) => (
                <li
                  key={r.repo}
                  className="flex min-h-9 items-center gap-2.5 rounded-lg py-1 pl-2 pr-2 text-[13px]"
                >
                  <span className="min-w-0 flex-1 truncate font-mono">{r.repo}</span>
                  <Badge>ignored</Badge>
                  <Button
                    size="sm"
                    variant="ghost"
                    disabled={busy}
                    onClick={() => onUnignore(r.repo)}
                  >
                    Offer again
                  </Button>
                </li>
              ))}
            </ul>
          </div>
        )}
      </div>
    </Dialog>
  )
}
