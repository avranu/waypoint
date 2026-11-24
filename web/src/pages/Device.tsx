// Approving lets the waiting CLI sign in as you, or as the agent it names.

import { useEffect, useState } from 'react'
import { Link, useNavigate, useSearch } from '@tanstack/react-router'
import { useMutation, useQuery } from '@tanstack/react-query'
import { Bot, CheckCircle2, ShieldAlert, Terminal } from 'lucide-react'
import { api, ApiError, enc } from '@/lib/api'
import { agentLabel } from '@/lib/agents'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Skeleton } from '@/components/ui/misc'
import { toastError } from '@/components/ui/toast'
import { relativeTime } from '@/lib/format'
import { Warning } from './settings/common'

interface DeviceRequest {
  user_code: string
  name: string
  agent?: string
  ip?: string
  user_agent?: string
  created_at?: string
  expires_at: string
  /** Where this page is being viewed from, to compare with ip. */
  your_ip?: string
  same_address?: boolean
}

const tidy = (c: string) => {
  const s = c.toUpperCase().replace(/[^A-Z0-9]/g, '')
  return s.length > 4 ? `${s.slice(0, 4)}-${s.slice(4, 8)}` : s
}

export function DevicePage() {
  const { code } = useSearch({ from: '/app/device' })
  const navigate = useNavigate()
  const [entry, setEntry] = useState('')
  const [state, setState] = useState<'ask' | 'approved' | 'cancelled'>('ask')
  useEffect(() => {
    document.title = 'Approve sign-in · Waypoint'
  }, [])
  const req = useQuery({
    queryKey: ['device', code],
    queryFn: () => api.get<DeviceRequest>(`/cli/login/${enc(code!)}`),
    enabled: !!code,
    retry: false,
  })
  const approve = useMutation({
    mutationFn: () => api.post(`/cli/login/${enc(code!)}/approve`),
    onSuccess: () => setState('approved'),
    onError: toastError,
  })
  const r = req.data
  return (
    <div className="flex flex-1 items-start justify-center overflow-y-auto p-4 sm:items-center">
      <div className="wp-surface w-full max-w-md space-y-4 rounded-2xl border border-line p-5 sm:p-6">
        <div className="flex items-center gap-2.5">
          <span className="flex size-9 items-center justify-center rounded-xl bg-accent-soft text-accent">
            <Terminal className="size-4.5" />
          </span>
          <h1 className="text-[17px] font-semibold tracking-tight">Sign in on the command line</h1>
        </div>
        {!code ? (
          <form
            className="space-y-3"
            onSubmit={(e) => {
              e.preventDefault()
              if (entry.replace(/[^A-Za-z0-9]/g, '').length === 8)
                navigate({ to: '/device', search: { code: tidy(entry) } })
            }}
          >
            <p className="text-[13px] text-muted">Enter the code `wp auth login` showed you.</p>
            <Input
              value={entry}
              onChange={(e) => setEntry(tidy(e.target.value))}
              placeholder="XXXX-XXXX"
              aria-label="Login code"
              autoFocus
              autoComplete="off"
              spellCheck={false}
              className="text-center font-mono text-lg tracking-[0.2em]"
            />
            <Button
              type="submit"
              variant="primary"
              className="w-full"
              disabled={entry.replace(/-/g, '').length !== 8}
            >
              Continue
            </Button>
          </form>
        ) : state === 'approved' ? (
          <div className="space-y-2 text-[13px]">
            <p className="flex items-center gap-2 font-medium text-ok">
              <CheckCircle2 className="size-4" /> Approved
            </p>
            <p className="text-muted">
              You can close this tab; <code>wp</code> is signed in. Its token is listed under{' '}
              <Link
                to="/settings"
                search={{ tab: 'tokens' }}
                className="text-accent hover:underline"
              >
                Settings → API tokens
              </Link>{' '}
              if you want to revoke it.
            </p>
          </div>
        ) : state === 'cancelled' ? (
          <p className="text-[13px] text-muted">
            Cancelled. Nothing was signed in; the code expires on its own.{' '}
            <Link to="/" className="text-accent hover:underline">
              Back to Waypoint
            </Link>
          </p>
        ) : req.isPending ? (
          <Skeleton className="h-28" />
        ) : req.isError && !(req.error instanceof ApiError && req.error.status === 404) ? (
          <p className="text-[13px] text-danger">
            Couldn't load this sign-in: {req.error.message}. Reload to try again.
          </p>
        ) : !r ? (
          <div className="space-y-3 text-[13px]">
            <p className="text-muted">
              No pending sign-in for <b className="font-mono text-fg">{tidy(code)}</b>. It may have
              expired (codes last a few minutes) or been used already. Run{' '}
              <code>wp auth login</code> again for a new one.
            </p>
            <Button size="sm" onClick={() => navigate({ to: '/device', search: {} })}>
              Enter another code
            </Button>
          </div>
        ) : (
          <div className="space-y-4">
            {r.same_address === false && (
              <div
                role="alert"
                className="flex gap-2.5 rounded-lg border border-danger/40 bg-danger/10 px-3 py-2.5 text-[13px]"
              >
                <ShieldAlert className="mt-0.5 size-4 shrink-0 text-danger" />
                <div>
                  <b>This sign-in was started from a different address</b> ({r.ip || 'unknown'})
                  than the one you're on now ({r.your_ip || 'unknown'}). That's fine if you're
                  signing in a server or another machine. If someone sent you this link or code,
                  don't approve: they would get full access to your account.
                </div>
              </div>
            )}
            <p className="text-[13px] text-muted">
              Check this code matches the one in your terminal before approving.
            </p>
            <div className="rounded-xl bg-sunken py-3 text-center font-mono text-2xl font-semibold tracking-[0.2em]">
              {r.user_code}
            </div>
            <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1.5 text-[13px]">
              <dt className="text-subtle" title="As the device describes itself">
                Device
              </dt>
              <dd className="truncate">{r.name || 'wp'}</dd>
              <dt className="text-subtle">Requested</dt>
              <dd className="min-w-0">
                <span className="break-words">
                  {[
                    r.ip && `from ${r.ip}`,
                    r.user_agent && r.user_agent.slice(0, 60),
                    r.created_at && relativeTime(r.created_at),
                  ]
                    .filter(Boolean)
                    .join(' · ') || 'unknown'}
                </span>
              </dd>
              <dt className="text-subtle" title="As the device asked">
                Acts as
              </dt>
              <dd className="flex items-center gap-1.5">
                {r.agent ? (
                  <>
                    <Bot className="size-3.5 text-accent" /> an agent ({agentLabel(r.agent)}) on
                    your behalf
                  </>
                ) : (
                  'you'
                )}
              </dd>
            </dl>
            <p className="text-xs text-subtle">
              The device name and agent are what the device reported, not checked by Waypoint.
            </p>
            <Warning>
              Only approve if you just ran <code className="font-mono">wp auth login</code>{' '}
              yourself: this gives that machine full access to your account.
            </Warning>
            <div className="flex justify-end gap-2">
              <Button onClick={() => setState('cancelled')}>Cancel</Button>
              <Button
                variant="primary"
                loading={approve.isPending}
                onClick={() => approve.mutate()}
              >
                Approve
              </Button>
            </div>
          </div>
        )}
      </div>
    </div>
  )
}
