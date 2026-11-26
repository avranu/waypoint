import { Link } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import { q } from '@/lib/queries'
import { useSetKitSource } from '@/lib/kit'
import { toast } from '@/components/ui/toast'
import { Select } from '@/components/ui/input'
import { LoadError, Section, Skeleton } from '@/components/ui/misc'

export function KitSourceSection() {
  const src = useQuery(q.kitSource())
  const set = useSetKitSource()
  const candidates = src.data?.candidates ?? []
  return (
    <Section
      title="Agent kit"
      helpId="admin:kit"
      help={
        <p>
          A repo laid out as a Claude Code plugin marketplace, with <code>kit.toml</code> saying
          which plugins and notes go to which repos. Waypoint reads it on a push and hourly;{' '}
          <code>wp kit sync</code> brings checkouts in line.
        </p>
      }
    >
      <div className="flex flex-wrap items-center gap-3 px-4 py-3 text-[13px]">
        {src.isPending ? (
          <Skeleton className="h-9 w-64" />
        ) : src.isError ? (
          <LoadError what="The kit repository" error={src.error} />
        ) : (
          <div className="w-full max-w-sm">
            <Select
              aria-label="Kit repository"
              value={String(src.data?.repo_id ?? 0)}
              disabled={set.isPending}
              onChange={(e) =>
                set.mutate(Number(e.target.value), {
                  onSuccess: (r) => toast(r.repo_id ? 'The kit is read now' : 'No kit repo'),
                })
              }
            >
              <option value="0">No kit</option>
              {candidates.map((c) => (
                <option key={c.id} value={String(c.id)}>
                  {c.repo} ({c.project}, {c.forge})
                </option>
              ))}
            </Select>
          </div>
        )}
        {!!src.data?.repo_id && (
          <Link to="/agents" search={{ tab: 'kit' }} className="text-accent hover:underline">
            Agents → Kit
          </Link>
        )}
      </div>
    </Section>
  )
}
