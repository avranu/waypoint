import { useQueryClient } from '@tanstack/react-query'
import { useUpdateIssue, type IssuePatch } from '@/lib/queries'
import { toastError } from '@/components/ui/toast'
import {
  isClosed,
  type Issue,
  type Label,
  type Project,
  type Status,
  type UserRef,
} from '@/lib/types'

/** Field setters for an issue that update every view optimistically. */
export function useIssueEdit(
  issue: Pick<Issue, 'key' | 'project_key' | 'status' | 'resolved_at' | 'labels'>,
  project?: Project,
) {
  const upd = useUpdateIssue()
  const qc = useQueryClient()
  const set = (patch: IssuePatch, optimistic?: Partial<Issue>) => {
    const p = upd
      .mutateAsync({ key: issue.key, patch, optimistic, projectKey: issue.project_key })
      .catch((e) => {
        toastError(e)
        throw e
      })
    p.catch(() => {})
    return p
  }
  const users = () => qc.getQueryData<UserRef[]>(['users']) ?? []
  return {
    pending: upd.isPending,
    patch: set,
    status: (s: Status) =>
      set(
        { status: String(s.id) },
        {
          status: s,
          resolved_at: isClosed(s.category)
            ? (issue.resolved_at ?? new Date().toISOString())
            : null,
        },
      ),
    priority: (p: number) => set({ priority: p }, { priority: p }),
    type: (t: string) => set({ type: t }, { type: t as Issue['type'] }),
    title: (t: string) => set({ title: t }, { title: t }),
    description: (d: string) => set({ description: d }, { description: d }),
    intent: (t: string) => set({ intent: t }),
    assignees: (names: string[]) =>
      set(
        { assignees: names },
        {
          assignees: names
            .map((n) => users().find((u) => u.username === n))
            .filter(Boolean) as UserRef[],
        },
      ),
    labels: (names: string[]) => {
      const known = [...(project?.labels ?? []), ...issue.labels]
      const labels: Label[] = names.map(
        (n, i) =>
          known.find((l) => l.name.toLowerCase() === n.toLowerCase()) ?? {
            id: -1 - i,
            name: n,
            color: '#64748b',
          },
      )
      return set({ labels: names }, { labels })
    },
    points: (p: number | null) => set({ points: p }, { points: p }),
    due: (d: string | null) => set({ due_date: d }, { due_date: d }),
    start: (d: string | null) => set({ start_date: d }, { start_date: d }),
    sprint: (name: string | null) => {
      const sp = project?.sprints?.find((s) => s.name === name)
      return set({ sprint: name }, { sprint_id: sp?.id ?? null, sprint_name: sp?.name })
    },
    parent: (key: string | null) => set({ parent: key }),
    agent: (spec: string) => set({ agent: spec }, { agent: spec }),
    category: (c: string) => set({ category: c }, { category: c }),
    workflow: (name: string | null) => set({ workflow: name }),
    step: (name: string) => set({ step: name }, { step: name }),
  }
}
