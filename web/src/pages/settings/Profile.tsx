import { useEffect, useMemo, useRef, useState } from 'react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import clsx from 'clsx'
import { Monitor, Moon, Sun, Upload } from 'lucide-react'
import { api } from '@/lib/api'
import { setTheme, storedTheme, type Theme } from '@/lib/theme'
import { useSavePrefs } from '@/lib/queries'
import type { User } from '@/lib/types'
import { Button } from '@/components/ui/button'
import { Field, Input, Select } from '@/components/ui/input'
import { Avatar, Section } from '@/components/ui/misc'
import { toast, toastError } from '@/components/ui/toast'
import { avatarProblem, Help } from './common'

function timeZones(current: string): string[] {
  let list: string[] = []
  try {
    list = Intl.supportedValuesOf('timeZone')
  } catch {
    list = []
  }
  if (!list.includes('UTC')) list = ['UTC', ...list]
  if (current && !list.includes(current)) list = [current, ...list]
  return list
}

export function ProfileTab({ me }: { me: User }) {
  const qc = useQueryClient()
  const [name, setName] = useState(me.display_name)
  const [email, setEmail] = useState(me.email)
  const [tz, setTz] = useState(me.timezone || 'UTC')
  const synced = useRef({ name: me.display_name, email: me.email, tz: me.timezone || 'UTC' })
  useEffect(() => {
    const prev = synced.current
    synced.current = { name: me.display_name, email: me.email, tz: me.timezone || 'UTC' }
    setName((cur) => (cur === prev.name ? me.display_name : cur))
    setEmail((cur) => (cur === prev.email ? me.email : cur))
    setTz((cur) => (cur === prev.tz ? me.timezone || 'UTC' : cur))
  }, [me.display_name, me.email, me.timezone])
  const zones = useMemo(() => timeZones(me.timezone), [me.timezone])
  const browserTz = Intl.DateTimeFormat().resolvedOptions().timeZone
  const dirty = name !== me.display_name || email !== me.email || tz !== (me.timezone || 'UTC')

  const save = useMutation({
    mutationFn: () =>
      api.patch<User>('/me', { display_name: name.trim(), email: email.trim(), timezone: tz }),
    onSuccess: (u) => {
      qc.setQueryData(['me'], u)
      qc.invalidateQueries({ queryKey: ['users'] })
      toast('Profile saved')
    },
    onError: toastError,
  })

  return (
    <>
      <AvatarSection me={me} />
      <Section title="Profile">
        <form
          className="space-y-4 p-4"
          onSubmit={(e) => {
            e.preventDefault()
            if (dirty && name.trim()) save.mutate()
          }}
        >
          <div className="grid gap-4 sm:grid-cols-2">
            <Field label="Display name">
              <Input
                value={name}
                onChange={(e) => setName(e.target.value)}
                autoComplete="name"
                required
              />
            </Field>
            <Field label="Username" hint="Only an admin can change your username.">
              <Input value={'@' + me.username} disabled />
            </Field>
          </div>
          <Field label="Email">
            <Input
              type="email"
              value={email}
              onChange={(e) => setEmail(e.target.value)}
              autoComplete="email"
              required
            />
          </Field>
          <Field
            label="Time zone"
            hint="Used for due dates, reminders and how time is split into days."
          >
            <div className="flex flex-col gap-2 sm:flex-row">
              <Select value={tz} onChange={(e) => setTz(e.target.value)} className="sm:flex-1">
                {zones.map((z) => (
                  <option key={z} value={z}>
                    {z.replace(/_/g, ' ')}
                  </option>
                ))}
              </Select>
              <Button
                size="md"
                variant="subtle"
                disabled={!browserTz || tz === browserTz}
                onClick={() => setTz(browserTz)}
              >
                Use my browser's
              </Button>
            </div>
          </Field>
          <div className="flex justify-end">
            <Button
              type="submit"
              variant="primary"
              disabled={!dirty || !name.trim()}
              loading={save.isPending}
            >
              Save changes
            </Button>
          </div>
        </form>
      </Section>
    </>
  )
}

function AvatarSection({ me }: { me: User }) {
  const qc = useQueryClient()
  const input = useRef<HTMLInputElement>(null)
  const done = () => qc.invalidateQueries({ queryKey: ['users'] })
  const upload = useMutation({
    mutationFn: (f: File) => api.put<User>('/me/avatar', f),
    onSuccess: (u) => {
      qc.setQueryData(['me'], u)
      done()
      toast('Avatar updated')
    },
    onError: toastError,
  })
  const remove = useMutation({
    mutationFn: () => api.del('/me/avatar'),
    onSuccess: () => {
      qc.setQueryData<User>(['me'], (u) => (u ? { ...u, avatar_url: undefined } : u))
      qc.invalidateQueries({ queryKey: ['me'] })
      done()
    },
    onError: toastError,
  })
  return (
    <Section title="Avatar">
      <div className="flex items-center gap-4 p-4">
        <Avatar user={me} size={64} />
        <div className="min-w-0 flex-1 space-y-2">
          <div className="flex flex-wrap gap-2">
            <Button
              size="sm"
              icon={<Upload className="size-3.5" />}
              loading={upload.isPending}
              onClick={() => input.current?.click()}
            >
              Upload image
            </Button>
            {me.avatar_url && (
              <Button
                size="sm"
                variant="ghost"
                loading={remove.isPending}
                onClick={() => remove.mutate()}
              >
                Remove
              </Button>
            )}
          </div>
          <div className="text-xs text-subtle">
            PNG, JPEG, GIF or WebP, up to 2 MB. Square images look best.
          </div>
        </div>
        <input
          ref={input}
          type="file"
          accept="image/png,image/jpeg,image/gif,image/webp"
          className="hidden"
          onChange={(e) => {
            const f = e.target.files?.[0]
            e.target.value = ''
            if (!f) return
            const bad = avatarProblem(f)
            if (bad) toast(bad, { kind: 'error' })
            else upload.mutate(f)
          }}
        />
      </div>
    </Section>
  )
}

const themes: { id: Theme; label: string; icon: React.ReactNode; hint: string }[] = [
  { id: '', label: 'System', icon: <Monitor />, hint: 'Follow your device' },
  { id: 'light', label: 'Light', icon: <Sun />, hint: 'Always light' },
  { id: 'dark', label: 'Dark', icon: <Moon />, hint: 'Always dark' },
]

export function AppearanceTab({ me }: { me: User }) {
  const [theme, setT] = useState<Theme>(storedTheme)
  const savePrefs = useSavePrefs()
  const pick = (t: Theme) => {
    setTheme(t)
    setT(t)
    savePrefs.mutate({ theme: t }, { onError: toastError })
  }
  return (
    <>
      <Section title="Theme">
        <div className="grid grid-cols-3 gap-2 p-4">
          {themes.map((t) => (
            <button
              key={t.id || 'system'}
              type="button"
              aria-pressed={theme === t.id}
              onClick={() => pick(t.id)}
              className={clsx(
                'flex flex-col items-center gap-1.5 rounded-xl border px-2 py-4 text-[13px] transition-colors [&>svg]:size-5',
                theme === t.id
                  ? 'border-accent bg-accent-soft text-fg [&>svg]:text-accent'
                  : 'border-line text-muted hover:bg-hover hover:text-fg',
              )}
            >
              {t.icon}
              <span className="font-medium">{t.label}</span>
              <span className="hidden text-xs text-subtle sm:block">{t.hint}</span>
            </button>
          ))}
        </div>
      </Section>
      <Section title="Calendar">
        <div className="space-y-2 p-4">
          <Field label="Week starts on" hint="Used by the timesheet and date pickers.">
            <Select
              value={String(me.prefs.week_start ?? 1)}
              onChange={(e) =>
                savePrefs.mutate({ week_start: Number(e.target.value) }, { onError: toastError })
              }
              className="sm:w-56"
            >
              <option value="1">Monday</option>
              <option value="0">Sunday</option>
            </Select>
          </Field>
        </div>
      </Section>
      <Section title="Quota alert">
        <div className="space-y-2 p-4">
          <Field
            label="Push when the week's quota rises by (points in an hour)"
            hint="Checked as the status line reports. 0 turns it off."
          >
            <Input
              type="number"
              min={0}
              max={100}
              defaultValue={me.prefs.quota_jump ?? 4}
              onBlur={(e) => {
                const v = Math.min(100, Math.max(0, Math.round(Number(e.target.value) || 0)))
                if (v !== (me.prefs.quota_jump ?? 4))
                  savePrefs.mutate({ quota_jump: v }, { onError: toastError })
              }}
              className="sm:w-32"
            />
          </Field>
        </div>
      </Section>
      <Section title="Prompt caches">
        <div className="space-y-2 p-4">
          <Field
            label="Keep a waiting session's cache warm by"
            hint="Compacting reads the context once and leaves a short summary. A session gets one ping at most, and only while its context is small; after that, or when it is big, it is compacted."
          >
            <Select
              value={me.prefs.cache_warm ?? ''}
              onChange={(e) =>
                savePrefs.mutate(
                  { cache_warm: e.target.value === 'compact' ? 'compact' : '' },
                  { onError: toastError },
                )
              }
              className="sm:w-72"
            >
              <option value="">One short ping, then compacting</option>
              <option value="compact">Compacting it right away</option>
            </Select>
          </Field>
        </div>
      </Section>
    </>
  )
}

export function PasswordTab() {
  const [cur, setCur] = useState('')
  const [next, setNext] = useState('')
  const [confirm, setConfirm] = useState('')
  const mismatch = confirm.length > 0 && next !== confirm
  const tooShort = next.length > 0 && next.length < 10
  const change = useMutation({
    mutationFn: () => api.post('/me/password', { current: cur, new: next }),
    onSuccess: () => {
      setCur('')
      setNext('')
      setConfirm('')
      toast('Password changed')
    },
    onError: toastError,
  })
  return (
    <Section title="Change password">
      <form
        className="space-y-4 p-4"
        onSubmit={(e) => {
          e.preventDefault()
          if (cur && next && !mismatch && !tooShort) change.mutate()
        }}
      >
        <Field label="Current password">
          <Input
            type="password"
            value={cur}
            onChange={(e) => setCur(e.target.value)}
            autoComplete="current-password"
            required
          />
        </Field>
        <Field
          label="New password"
          error={tooShort ? 'At least 10 characters.' : undefined}
          note="At least 10 characters. A passphrase is easiest."
        >
          <Input
            type="password"
            value={next}
            onChange={(e) => setNext(e.target.value)}
            autoComplete="new-password"
            required
          />
        </Field>
        <Field label="Confirm new password" error={mismatch ? "Passwords don't match." : undefined}>
          <Input
            type="password"
            value={confirm}
            onChange={(e) => setConfirm(e.target.value)}
            autoComplete="new-password"
            required
          />
        </Field>
        <Help>Forgot it? An admin can send you a reset link.</Help>
        <div className="flex justify-end">
          <Button
            type="submit"
            variant="primary"
            disabled={!cur || !next || next !== confirm || tooShort}
            loading={change.isPending}
          >
            Change password
          </Button>
        </div>
      </form>
    </Section>
  )
}
