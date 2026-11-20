// Shared pieces of the sandbox settings (WP-418): the password check that
// loosening needs, the reason dialog, and the change log with Revert.

import { useCallback, useRef, useState, type ReactNode } from 'react'
import { useQuery } from '@tanstack/react-query'
import { ArrowRight, RotateCcw, ShieldAlert, ShieldCheck } from 'lucide-react'
import clsx from 'clsx'
import { ApiError } from '@/lib/api'
import { dateTime, relativeTime } from '@/lib/format'
import { q } from '@/lib/queries'
import {
  changeLoosens,
  levelLabel,
  reauth,
  useRevertSandboxChange,
  type SandboxChange,
} from '@/lib/sandbox'
import { Button } from '@/components/ui/button'
import { Dialog } from '@/components/ui/dialog'
import { Field, Input, Textarea } from '@/components/ui/input'
import { Badge, LoadError, Skeleton } from '@/components/ui/misc'
import { toast, toastError } from '@/components/ui/toast'

/** Thrown when the person closes the password dialog instead. */
class ReauthCancelled extends Error {}

/** toastError, quiet when the person closed the password dialog. */
export const sandboxError = (e: unknown) => {
  if (!(e instanceof ReauthCancelled)) toastError(e)
}

/**
 * Runs a sandbox write; when the server asks for the password again
 * (`reauth`), asks for it and runs the write once more. Render `dialog`.
 */
export function useReauth() {
  const waiting = useRef<{ resolve: () => void; reject: (e: Error) => void } | null>(null)
  const [open, setOpen] = useState(false)
  const guard = useCallback(async <T,>(write: () => Promise<T>): Promise<T> => {
    try {
      return await write()
    } catch (e) {
      if (!(e instanceof ApiError && e.code === 'reauth')) throw e
      await new Promise<void>((resolve, reject) => {
        waiting.current = { resolve, reject }
        setOpen(true)
      })
      return write()
    }
  }, [])
  const finish = (ok: boolean) => {
    setOpen(false)
    if (ok) waiting.current?.resolve()
    else waiting.current?.reject(new ReauthCancelled('cancelled'))
    waiting.current = null
  }
  const dialog = (
    <ReauthDialog open={open} onDone={() => finish(true)} onCancel={() => finish(false)} />
  )
  return { guard, dialog }
}

function ReauthDialog({
  open,
  onDone,
  onCancel,
}: {
  open: boolean
  onDone: () => void
  onCancel: () => void
}) {
  const [password, setPassword] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const close = () => {
    setPassword('')
    setError('')
    onCancel()
  }
  const submit = async () => {
    setBusy(true)
    setError('')
    try {
      await reauth(password)
      setPassword('')
      onDone()
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(false)
    }
  }
  return (
    <Dialog
      open={open}
      onClose={close}
      title="Enter your password"
      footer={
        <>
          <Button variant="ghost" onClick={close}>
            Cancel
          </Button>
          <Button
            variant="primary"
            type="submit"
            form="sandbox-reauth"
            loading={busy}
            disabled={!password}
          >
            Confirm
          </Button>
        </>
      }
    >
      <form
        id="sandbox-reauth"
        className="space-y-3 px-4 py-4"
        onSubmit={(e) => {
          e.preventDefault()
          if (password) void submit()
        }}
      >
        <p className="text-sm text-muted">
          Loosening a sandbox needs your password from the last ten minutes, so a session left open,
          or an agent driving this browser, can’t do it on its own.
        </p>
        <Field label="Password" error={error}>
          <Input
            type="password"
            autoComplete="current-password"
            autoFocus
            value={password}
            onChange={(e) => setPassword(e.target.value)}
          />
        </Field>
      </form>
    </Dialog>
  )
}

/** Asks why before a change that loosens a sandbox. */
export function ReasonDialog({
  open,
  title,
  children,
  confirmLabel,
  needReason,
  busy,
  onConfirm,
  onClose,
}: {
  open: boolean
  title: string
  children?: ReactNode
  confirmLabel: string
  needReason: boolean
  busy?: boolean
  onConfirm: (reason: string) => void
  onClose: () => void
}) {
  const [reason, setReason] = useState('')
  const ok = !needReason || reason.trim() !== ''
  const close = () => {
    setReason('')
    onClose()
  }
  return (
    <Dialog
      open={open}
      onClose={close}
      title={title}
      footer={
        <>
          <Button variant="ghost" onClick={close}>
            Cancel
          </Button>
          <Button
            variant={needReason ? 'danger' : 'primary'}
            type="submit"
            form="sandbox-reason"
            loading={busy}
            disabled={!ok}
          >
            {confirmLabel}
          </Button>
        </>
      }
    >
      <form
        id="sandbox-reason"
        className="space-y-3 px-4 py-4"
        onSubmit={(e) => {
          e.preventDefault()
          if (ok) {
            onConfirm(reason.trim())
            setReason('')
          }
        }}
      >
        {children && <div className="text-sm text-muted">{children}</div>}
        <Field
          label={needReason ? 'Why?' : 'Why? (optional)'}
          note="Kept in the sandbox log; admins are told."
        >
          <Textarea
            autoFocus
            rows={2}
            maxLength={500}
            value={reason}
            onChange={(e) => setReason(e.target.value)}
          />
        </Field>
      </form>
    </Dialog>
  )
}

const FIELD_LABEL: Record<SandboxChange['field'], string> = {
  level: 'runner jobs',
  interactive_level: 'interactive sessions',
  default_level: 'default level',
  paused: 'sandboxing paused',
}

function valueLabel(c: SandboxChange, v: string) {
  if (c.field === 'paused') return v === 'yes' ? 'yes' : 'no'
  return levelLabel(v, c.field === 'level' || c.field === 'default_level' ? 'unset' : 'default')
}

/**
 * Sandbox changes, newest first. `canEdit` says whether this person may
 * revert a row; a revert goes through the same checks as the change.
 */
export function SandboxLog({
  filter,
  canEdit,
  showTarget,
  empty = 'No changes yet.',
}: {
  filter: { project?: string; runner?: number }
  canEdit: (c: SandboxChange) => boolean
  showTarget?: boolean
  empty?: string
}) {
  const log = useQuery(q.sandboxLog({ ...filter, limit: 50 }))
  const revert = useRevertSandboxChange()
  const { guard, dialog } = useReauth()
  const [reverting, setReverting] = useState<SandboxChange | null>(null)
  const loosens = reverting ? changeLoosens(reverting.field, reverting.new, reverting.old) : false
  if (log.isPending)
    return (
      <div className="p-4">
        <Skeleton className="h-16" />
      </div>
    )
  if (log.isError) return <LoadError what="The change log" error={log.error} />
  const rows = log.data ?? []
  return (
    <>
      {rows.length === 0 ? (
        <p className="px-4 py-3 text-[13px] text-subtle">{empty}</p>
      ) : (
        <ul className="divide-y divide-line">
          {rows.map((c) => (
            <li key={c.id} className="flex items-start gap-3 px-4 py-2.5">
              {c.loosens ? (
                <ShieldAlert className="mt-0.5 size-4 shrink-0 text-danger" aria-label="Loosened" />
              ) : (
                <ShieldCheck
                  className="mt-0.5 size-4 shrink-0 text-subtle"
                  aria-label="Tightened"
                />
              )}
              <div className="min-w-0 flex-1 text-[13px]">
                <div className="flex flex-wrap items-center gap-x-1.5 gap-y-0.5">
                  {showTarget && <span className="font-medium">{c.target_name}</span>}
                  <span className="text-muted">{FIELD_LABEL[c.field]}</span>
                  <span className={clsx(!c.loosens && 'text-muted')}>{valueLabel(c, c.old)}</span>
                  <ArrowRight className="size-3 text-subtle" aria-label="to" />
                  <span className={clsx('font-medium', c.loosens && 'text-danger')}>
                    {valueLabel(c, c.new)}
                  </span>
                  {c.reverts && <Badge>revert of #{c.reverts}</Badge>}
                </div>
                {c.reason && <div className="mt-0.5 break-words text-muted">“{c.reason}”</div>}
                <div className="mt-0.5 text-xs text-subtle" title={dateTime(c.at)}>
                  #{c.id} · {c.user ? '@' + c.user.username : 'someone'} · {relativeTime(c.at)}
                </div>
              </div>
              {c.current && canEdit(c) && (
                <Button
                  size="xs"
                  variant="ghost"
                  icon={<RotateCcw className="size-3" />}
                  onClick={() => setReverting(c)}
                >
                  Revert
                </Button>
              )}
            </li>
          ))}
        </ul>
      )}
      <ReasonDialog
        open={!!reverting}
        title={`Revert change #${reverting?.id ?? ''}`}
        confirmLabel="Revert"
        needReason={loosens}
        busy={revert.isPending}
        onClose={() => setReverting(null)}
        onConfirm={(reason) => {
          const c = reverting!
          setReverting(null)
          guard(() => revert.mutateAsync({ id: c.id, reason })).then(
            () => toast('Change reverted'),
            sandboxError,
          )
        }}
      >
        Puts {FIELD_LABEL[reverting?.field ?? 'level']}
        {reverting?.target_name ? ` of ${reverting.target_name}` : ''} back to{' '}
        <b>{reverting ? valueLabel(reverting, reverting.old) : ''}</b>.
        {loosens && ' That loosens the sandbox.'}
      </ReasonDialog>
      {dialog}
    </>
  )
}
