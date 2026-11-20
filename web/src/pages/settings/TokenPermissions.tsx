import { useQuery } from '@tanstack/react-query'
import clsx from 'clsx'
import { Eye, KeyRound, SlidersHorizontal } from 'lucide-react'
import { q } from '@/lib/queries'
import type { PermissionRisk, TokenPermission } from '@/lib/types'
import { Button } from '@/components/ui/button'
import { SegmentedControl } from '@/components/ui/segmented'
import { Badge, Skeleton } from '@/components/ui/misc'
import { Warning } from './common'

export type Access = 'full' | 'read' | 'custom'

/** What a token's saved settings amount to. */
export function accessOf(t: { permissions: string[] | null; read_only: boolean }): Access {
  return t.permissions ? 'custom' : t.read_only ? 'read' : 'full'
}

/** The fields the API takes for a chosen access. */
export function accessBody(access: Access, permissions: string[]) {
  return access === 'custom'
    ? { permissions, read_only: false }
    : { permissions: null, read_only: access === 'read' }
}

const RISK: Record<PermissionRisk, { text: string; color: string } | null> = {
  safe: null,
  careful: { text: 'careful', color: 'var(--wp-warn)' },
  dangerous: { text: 'dangerous', color: 'var(--wp-danger)' },
}

export function RiskBadge({ risk }: { risk: PermissionRisk }) {
  const r = RISK[risk]
  return r ? <Badge color={r.color}>{r.text}</Badge> : null
}

/** Full access / Read only / Custom. */
export function AccessChoice({
  value,
  onChange,
}: {
  value: Access
  onChange: (a: Access) => void
}) {
  return (
    <div className="space-y-1.5">
      <div className="text-[13px] font-medium">Access</div>
      <SegmentedControl
        label="Access"
        value={value}
        onChange={onChange}
        textFrom="always"
        options={[
          { value: 'full', label: 'Full access', icon: <KeyRound className="size-3.5" /> },
          { value: 'read', label: 'Read only', icon: <Eye className="size-3.5" /> },
          {
            value: 'custom',
            label: 'Custom',
            icon: <SlidersHorizontal className="size-3.5" />,
            hint: 'pick exactly what it may do',
          },
        ]}
      />
      <div className="text-xs text-subtle">
        {value === 'full'
          ? 'Can do everything you can.'
          : value === 'read'
            ? 'Can search and read, but not create or change anything.'
            : 'Can only do what you tick below.'}
      </div>
    </div>
  )
}

/**
 * Grouped checklists of what a token may do. `value` may hold keys the catalog
 * doesn't know (from a newer server); they're kept as they are.
 */
export function PermissionEditor({
  value,
  onChange,
}: {
  value: string[]
  onChange: (v: string[]) => void
}) {
  const cat = useQuery(q.tokenPermissions())
  if (cat.isPending)
    return (
      <div className="space-y-2">
        <Skeleton className="h-8" />
        <Skeleton className="h-8" />
        <Skeleton className="h-8" />
      </div>
    )
  if (!cat.data) return <Warning>Couldn't load the list of permissions.</Warning>
  const { groups, presets } = cat.data
  const on = new Set(value)
  const all = groups.flatMap((g) => g.permissions)
  const known = new Set(all.map((p) => p.key))
  const unknown = value.filter((k) => !known.has(k))
  const set = (keys: string[], add: boolean) => {
    const touched = new Set(keys)
    const rest = value.filter((k) => !touched.has(k))
    onChange(add ? [...rest, ...keys] : rest)
  }
  const risky = all.filter((p) => on.has(p.key) && p.risk === 'dangerous')
  const selected = all.filter((p) => on.has(p.key)).length

  return (
    <div className="space-y-3">
      <div className="flex flex-wrap items-center gap-1.5">
        <span className="mr-1 text-xs text-subtle">Start from</span>
        {presets.map((p) => (
          <Button
            key={p.key}
            size="xs"
            title={p.description}
            onClick={() => onChange([...p.permissions, ...unknown])}
          >
            {p.label}
          </Button>
        ))}
        <Button size="xs" variant="ghost" onClick={() => onChange([...unknown])}>
          None
        </Button>
        <span className="ml-auto text-xs text-subtle">
          {selected} of {all.length} selected
        </span>
      </div>
      <div className="space-y-3">
        {groups.map((g) => {
          const keys = g.permissions.map((p) => p.key)
          const count = keys.filter((k) => on.has(k)).length
          return (
            <fieldset key={g.key} className="rounded-lg border border-line">
              <legend className="sr-only">{g.label}</legend>
              <div className="flex items-center justify-between gap-2 border-b border-line bg-sunken/50 px-3 py-1.5">
                <span className="text-xs font-semibold uppercase tracking-wide text-subtle">
                  {g.label}{' '}
                  <span className="font-normal normal-case tracking-normal">
                    {count}/{keys.length}
                  </span>
                </span>
                <Button size="xs" variant="ghost" onClick={() => set(keys, count < keys.length)}>
                  {count < keys.length ? 'All' : 'None'}
                </Button>
              </div>
              <ul className="divide-y divide-line">
                {g.permissions.map((p) => (
                  <PermissionRow
                    key={p.key}
                    p={p}
                    checked={on.has(p.key)}
                    onChange={(v) => set([p.key], v)}
                  />
                ))}
              </ul>
            </fieldset>
          )
        })}
      </div>
      {unknown.length > 0 && (
        <p className="text-xs text-subtle">
          Also allows {unknown.length} permission{unknown.length === 1 ? '' : 's'} this version
          doesn't list ({unknown.join(', ')}). They're kept.
        </p>
      )}
      {risky.length > 0 && (
        <Warning>
          <b>Dangerous:</b> {risky.map((p) => p.label).join(', ')}. Only allow these for something
          you fully trust.
        </Warning>
      )}
    </div>
  )
}

function PermissionRow({
  p,
  checked,
  onChange,
}: {
  p: TokenPermission
  checked: boolean
  onChange: (v: boolean) => void
}) {
  return (
    <li>
      <label
        className={clsx(
          'flex cursor-pointer items-start gap-2.5 px-3 py-2 hover:bg-hover',
          checked && p.risk === 'dangerous' && 'bg-danger/5',
        )}
      >
        <input
          type="checkbox"
          checked={checked}
          onChange={(e) => onChange(e.target.checked)}
          className="mt-0.5 size-4 shrink-0 accent-[var(--wp-accent)]"
        />
        <span className="min-w-0 flex-1">
          <span className="flex flex-wrap items-center gap-1.5 text-[13px] font-medium">
            {p.label}
            <RiskBadge risk={p.risk} />
          </span>
          <span className="block text-xs text-subtle">{p.description}</span>
        </span>
      </label>
    </li>
  )
}
