// Web push for this browser: subscribe/unsubscribe and pick what gets pushed.

import { useEffect, useState } from 'react'
import { BellRing, BellOff } from 'lucide-react'
import { api, request } from '@/lib/api'
import { useSavePrefs } from '@/lib/queries'
import type { User } from '@/lib/types'
import { Button } from '@/components/ui/button'
import { Section } from '@/components/ui/misc'
import { toast, toastError } from '@/components/ui/toast'
import { CheckRow, Help } from './common'

type State = 'unsupported' | 'loading' | 'off' | 'on' | 'blocked'

const supported = () =>
  typeof window !== 'undefined' &&
  'serviceWorker' in navigator &&
  'PushManager' in window &&
  'Notification' in window

function keyBytes(b64url: string) {
  const b64 = (b64url + '='.repeat((4 - (b64url.length % 4)) % 4))
    .replace(/-/g, '+')
    .replace(/_/g, '/')
  return Uint8Array.from(atob(b64), (c) => c.charCodeAt(0))
}

async function registration() {
  // The service worker only registers in production builds.
  const reg = await navigator.serviceWorker.getRegistration()
  if (!reg) throw new Error('Notifications need the installed app (or a production build).')
  return reg
}

export function PushDevice({ me, kinds }: { me: User; kinds: [string, string, string][] }) {
  const savePrefs = useSavePrefs()
  const [state, setState] = useState<State>(supported() ? 'loading' : 'unsupported')
  const [busy, setBusy] = useState(false)
  const muted = me.prefs.push_muted ?? []

  useEffect(() => {
    if (!supported()) return
    if (Notification.permission === 'denied') return setState('blocked')
    navigator.serviceWorker
      .getRegistration()
      .then((reg) => reg?.pushManager.getSubscription())
      .then((sub) => setState(sub ? 'on' : 'off'))
      .catch(() => setState('off'))
  }, [])

  const enable = async () => {
    setBusy(true)
    try {
      const reg = await registration()
      if ((await Notification.requestPermission()) !== 'granted') {
        setState('blocked')
        return
      }
      const { public_key } = await api.get<{ public_key: string }>('/me/push')
      const sub = await reg.pushManager.subscribe({
        userVisibleOnly: true,
        applicationServerKey: keyBytes(public_key),
      })
      await api.post('/me/push', sub.toJSON())
      setState('on')
      toast('This device will get notifications')
    } catch (e) {
      toastError(e)
    } finally {
      setBusy(false)
    }
  }

  const disable = async () => {
    setBusy(true)
    try {
      const sub = await (await registration()).pushManager.getSubscription()
      if (sub) {
        await request('DELETE', '/me/push', { endpoint: sub.endpoint })
        await sub.unsubscribe()
      }
      setState('off')
    } catch (e) {
      toastError(e)
    } finally {
      setBusy(false)
    }
  }

  const toggle = (kind: string, on: boolean) => {
    const next = on ? muted.filter((k) => k !== kind) : [...new Set([...muted, kind])]
    savePrefs.mutate({ push_muted: next }, { onError: toastError })
  }

  return (
    <Section title="Notifications on this device">
      <div className="flex flex-wrap items-center gap-3 p-4">
        {state === 'on' ? (
          <BellRing className="size-4 shrink-0 text-accent" />
        ) : (
          <BellOff className="size-4 shrink-0 text-subtle" />
        )}
        <div className="min-w-0 flex-1">
          <Help>
            {state === 'unsupported'
              ? 'This browser can’t receive push notifications. On iPhone, add Waypoint to your Home Screen first.'
              : state === 'blocked'
                ? 'Notifications are blocked for this site. Allow them in the browser’s site settings, then come back.'
                : state === 'on'
                  ? 'This device gets a notification for the kinds ticked below.'
                  : 'Get notifications on this phone or computer, even when Waypoint is closed. No other app needed.'}
          </Help>
        </div>
        {(state === 'off' || state === 'on') && (
          <Button
            size="sm"
            variant={state === 'on' ? 'secondary' : 'primary'}
            loading={busy}
            onClick={state === 'on' ? disable : enable}
          >
            {state === 'on' ? 'Turn off' : 'Notify this device'}
          </Button>
        )}
      </div>
      {state === 'on' && (
        <div className="divide-y divide-line border-t border-line">
          {kinds.map(([kind, label, hint]) => (
            <CheckRow
              key={kind}
              label={label}
              hint={hint}
              checked={!muted.includes(kind)}
              onChange={(on) => toggle(kind, on)}
            />
          ))}
        </div>
      )}
    </Section>
  )
}
