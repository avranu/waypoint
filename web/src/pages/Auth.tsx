import { useState, type ReactNode } from 'react'
import { useNavigate, useParams, useSearch } from '@tanstack/react-router'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api, enc } from '@/lib/api'
import { Button } from '@/components/ui/button'
import { Field, Input } from '@/components/ui/input'
import type { User, UserLink } from '@/lib/types'

function Logo() {
  return (
    <div className="mb-8 flex items-center justify-center gap-2.5">
      <img src="/icon.svg" alt="" className="size-9" />
      <span className="text-xl font-semibold tracking-tight">Waypoint</span>
    </div>
  )
}

function Card({
  title,
  subtitle,
  children,
}: {
  title: string
  subtitle?: ReactNode
  children: ReactNode
}) {
  return (
    <div className="flex min-h-full items-center justify-center bg-bg px-4 py-12">
      <div className="w-full max-w-sm">
        <Logo />
        <div className="rounded-2xl border border-line bg-panel p-6 shadow-card">
          <h1 className="text-lg font-semibold">{title}</h1>
          {subtitle && <p className="mt-1 text-[13px] text-muted">{subtitle}</p>}
          <div className="mt-5">{children}</div>
        </div>
      </div>
    </div>
  )
}

export function LoginPage() {
  const qc = useQueryClient()
  const navigate = useNavigate()
  const { next } = useSearch({ from: '/login' })
  const [login, setLogin] = useState('')
  const [password, setPassword] = useState('')
  const m = useMutation({
    mutationFn: () => api.post<User>('/auth/login', { login, password }),
    onSuccess: (u) => {
      qc.setQueryData(['me'], u)
      // Only same-site paths: '//host' or '/\\host' would leave the app.
      navigate({ to: next && /^\/(?![/\\])/.test(next) ? next : '/' })
    },
  })
  return (
    <Card title="Sign in">
      <form
        className="space-y-4"
        onSubmit={(e) => {
          e.preventDefault()
          m.mutate()
        }}
      >
        <Field label="Username or email">
          <Input
            autoFocus
            autoComplete="username"
            value={login}
            onChange={(e) => setLogin(e.target.value)}
          />
        </Field>
        <Field label="Password" error={m.error?.message}>
          <Input
            type="password"
            autoComplete="current-password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
          />
        </Field>
        <Button
          type="submit"
          variant="primary"
          className="w-full"
          loading={m.isPending}
          disabled={!login || !password}
        >
          Sign in
        </Button>
        <p className="text-center text-xs text-subtle">
          Forgot your password? Ask your Waypoint admin for a reset link.
        </p>
      </form>
    </Card>
  )
}

export function InvitePage() {
  const { token } = useParams({ from: '/invite/$token' })
  const qc = useQueryClient()
  const navigate = useNavigate()
  const link = useQuery({
    queryKey: ['link', token],
    queryFn: () => api.get<UserLink>(`/auth/link/${enc(token)}`),
    retry: false,
  })
  const [form, setForm] = useState({ username: '', email: '', display_name: '', password: '' })
  const m = useMutation({
    mutationFn: () =>
      api.post<User>(`/auth/invite/${enc(token)}`, {
        ...form,
        email: link.data?.email || form.email,
      }),
    onSuccess: (u) => {
      qc.setQueryData(['me'], u)
      navigate({ to: '/' })
    },
  })
  if (link.isError)
    return (
      <Card title="This invite doesn't work">{link.error.message}. Ask for a new invite link.</Card>
    )
  if (!link.data) return <Card title="Loading…">{null}</Card>
  const set = (k: keyof typeof form) => (e: React.ChangeEvent<HTMLInputElement>) =>
    setForm({ ...form, [k]: e.target.value })
  return (
    <Card
      title="Join Waypoint"
      subtitle={
        link.data.is_admin
          ? 'You will be an administrator.'
          : "You've been invited. Pick a username and password."
      }
    >
      <form
        className="space-y-4"
        onSubmit={(e) => {
          e.preventDefault()
          m.mutate()
        }}
      >
        <Field label="Your name">
          <Input
            autoFocus
            value={form.display_name}
            onChange={set('display_name')}
            placeholder="Ada Lovelace"
            autoComplete="name"
          />
        </Field>
        <Field label="Username" note="Letters, digits, . _ -">
          <Input
            value={form.username}
            onChange={set('username')}
            autoComplete="username"
            autoCapitalize="none"
          />
        </Field>
        <Field label="Email">
          {link.data.email ? (
            <Input value={link.data.email} disabled />
          ) : (
            <Input type="email" value={form.email} onChange={set('email')} autoComplete="email" />
          )}
        </Field>
        <Field label="Password" note="At least 10 characters" error={m.error?.message}>
          <Input
            type="password"
            value={form.password}
            onChange={set('password')}
            autoComplete="new-password"
          />
        </Field>
        <Button type="submit" variant="primary" className="w-full" loading={m.isPending}>
          Create account
        </Button>
      </form>
    </Card>
  )
}

export function ResetPage() {
  const { token } = useParams({ from: '/reset/$token' })
  const navigate = useNavigate()
  const link = useQuery({
    queryKey: ['link', token],
    queryFn: () => api.get<UserLink>(`/auth/link/${enc(token)}`),
    retry: false,
  })
  const [password, setPassword] = useState('')
  const [confirm, setConfirm] = useState('')
  const m = useMutation({
    mutationFn: () => api.post(`/auth/reset/${enc(token)}`, { password }),
    onSuccess: () => navigate({ to: '/login' }),
  })
  if (link.isError)
    return (
      <Card title="This link doesn't work">
        {link.error.message}. Ask your admin for a new reset link.
      </Card>
    )
  const mismatch = confirm.length > 0 && confirm !== password
  return (
    <Card
      title="Choose a new password"
      subtitle={link.data?.username ? `For @${link.data.username}` : undefined}
    >
      <form
        className="space-y-4"
        onSubmit={(e) => {
          e.preventDefault()
          if (!mismatch) m.mutate()
        }}
      >
        <Field label="New password" note="At least 10 characters">
          <Input
            autoFocus
            type="password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            autoComplete="new-password"
          />
        </Field>
        <Field label="Confirm" error={mismatch ? "Passwords don't match" : m.error?.message}>
          <Input
            type="password"
            value={confirm}
            onChange={(e) => setConfirm(e.target.value)}
            autoComplete="new-password"
          />
        </Field>
        <Button
          type="submit"
          variant="primary"
          className="w-full"
          loading={m.isPending}
          disabled={!password || mismatch}
        >
          Set password
        </Button>
      </form>
    </Card>
  )
}
