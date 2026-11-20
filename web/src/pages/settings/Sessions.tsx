import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { LogOut, Monitor, Smartphone } from 'lucide-react'
import { api, enc } from '@/lib/api'
import { q } from '@/lib/queries'
import { dateTime, relativeTime } from '@/lib/format'
import type { Session } from '@/lib/types'
import { IconButton } from '@/components/ui/button'
import { Confirm } from '@/components/ui/dialog'
import { Badge, Section, Skeleton } from '@/components/ui/misc'
import { toast, toastError } from '@/components/ui/toast'

/** "Firefox on macOS" from a user agent; good enough to recognise your own devices. */
function describe(ua: string): { label: string; mobile: boolean } {
  const browser = /Edg\//.test(ua)
    ? 'Edge'
    : /Firefox\//.test(ua)
      ? 'Firefox'
      : /Chrome\//.test(ua)
        ? 'Chrome'
        : /Safari\//.test(ua)
          ? 'Safari'
          : /curl|Go-http|python|node/i.test(ua)
            ? 'Script'
            : 'Browser'
  const os = /iPhone|iPad/.test(ua)
    ? 'iOS'
    : /Android/.test(ua)
      ? 'Android'
      : /Mac OS X/.test(ua)
        ? 'macOS'
        : /Windows/.test(ua)
          ? 'Windows'
          : /Linux/.test(ua)
            ? 'Linux'
            : ''
  return { label: os ? `${browser} on ${os}` : browser, mobile: /Mobile|Android|iPhone/.test(ua) }
}

export function SessionsTab() {
  const qc = useQueryClient()
  const sessions = useQuery(q.sessions())
  const [confirmOthers, setConfirmOthers] = useState(false)
  const revoke = useMutation({
    mutationFn: (id: string) => api.del(`/me/sessions/${enc(id)}`),
    onSuccess: (_d, id) =>
      toast(id === 'others' ? 'Signed out everywhere else' : 'Signed out that session'),
    onError: toastError,
    onSettled: () => qc.invalidateQueries({ queryKey: ['sessions'] }),
  })
  const list = sessions.data ?? []
  const others = list.filter((s) => !s.current).length
  const [open, setOpen] = useState(false)
  return (
    <Section
      title="Where you're signed in"
      help={<>Changing your password also signs out everywhere else.</>}
    >
      {sessions.isPending ? (
        <div className="space-y-2 p-4">
          <Skeleton className="h-12" />
        </div>
      ) : sessions.isError ? (
        <p className="p-4 text-[13px] text-danger">
          Couldn't load your sessions: {sessions.error.message}
        </p>
      ) : (
        <>
          <div className="flex flex-wrap items-center gap-x-2 gap-y-1 px-4 py-3 text-[13px] text-muted">
            <span className="font-medium text-fg">This device</span>
            {others > 0 && (
              <>
                <span>·</span>
                <button
                  type="button"
                  aria-expanded={open}
                  onClick={() => setOpen(!open)}
                  className="hover:text-fg"
                >
                  {others} other{others === 1 ? '' : 's'}
                </button>
                <span>·</span>
                <button
                  type="button"
                  onClick={() => setConfirmOthers(true)}
                  className="hover:text-fg"
                >
                  Sign out others
                </button>
              </>
            )}
          </div>
          {open && (
            <ul className="divide-y divide-line border-t border-line">
              {list.map((s) => (
                <Row key={s.id} s={s} onRevoke={() => revoke.mutate(s.id)} />
              ))}
            </ul>
          )}
        </>
      )}
      <Confirm
        open={confirmOthers}
        onClose={() => setConfirmOthers(false)}
        onConfirm={() => revoke.mutate('others')}
        title="Sign out other sessions?"
        body={`${others} other ${others === 1 ? 'session' : 'sessions'} will need to sign in again. This one stays signed in.`}
        confirmLabel="Sign out"
      />
    </Section>
  )
}

function Row({ s, onRevoke }: { s: Session; onRevoke: () => void }) {
  const d = describe(s.user_agent)
  return (
    <li className="flex items-center gap-3 px-4 py-3">
      {d.mobile ? (
        <Smartphone className="size-4 shrink-0 text-subtle" />
      ) : (
        <Monitor className="size-4 shrink-0 text-subtle" />
      )}
      <div className="min-w-0 flex-1">
        <div className="flex items-center gap-1.5 text-[13px] font-medium">
          {d.label}
          {s.current && <Badge color="var(--wp-ok)">This device</Badge>}
        </div>
        <div className="truncate text-xs text-subtle" title={s.user_agent}>
          {s.ip && `${s.ip} · `}
          <span title={dateTime(s.last_seen_at)}>active {relativeTime(s.last_seen_at)}</span> ·
          signed in {relativeTime(s.created_at)}
        </div>
      </div>
      {!s.current && (
        <IconButton
          label={`Sign out ${d.label}`}
          onClick={onRevoke}
          icon={<LogOut className="size-3.5" />}
        />
      )}
    </li>
  )
}
