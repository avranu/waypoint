import type { QueryClient } from '@tanstack/react-query'
import type { Project, WEvent } from './types'
import { toast } from '@/components/ui/toast'
import { api } from './api'

export function startLive(qc: QueryClient): () => void {
  let es: EventSource | null = null
  let pending = new Set<string>()
  let timer: number | undefined
  let retry: number | undefined
  let backoff = 2000
  let connectedOnce = false
  let closed = false

  const projectKey = (id?: number) => {
    if (!id) return undefined
    const ps = qc.getQueryData<Project[]>(['projects'])
    return ps?.find((p) => p.id === id)?.key
  }

  const flush = () => {
    timer = undefined
    const keys = pending
    pending = new Set()
    for (const k of keys) qc.invalidateQueries({ queryKey: JSON.parse(k) })
  }
  const mark = (...keys: unknown[][]) => {
    for (const k of keys) pending.add(JSON.stringify(k))
    if (timer === undefined) timer = window.setTimeout(flush, 120)
  }

  // The Agents overview is built from nearly everything: sessions, requests,
  // pull requests, environments, issues and knowledge.
  const OVERVIEW = ['agents', 'overview']

  const handle = (kind: string, e: WEvent) => {
    const pk = projectKey(e.project_id)
    if (kind === 'bot.run') {
      mark(pk ? ['botRuns', pk] : ['botRuns'], ['pulls'], ['agents', 'activity'], OVERVIEW)
      return
    }
    if (
      kind === 'pr.updated' ||
      kind === 'pr.command' ||
      kind === 'pr.activity' ||
      kind === 'pr.ci' ||
      kind === 'pr.review_gate' ||
      kind === 'pr.review' ||
      kind === 'pr.comment' ||
      kind === 'pr.babysit'
    ) {
      mark(['pulls'], ['agents', 'activity'], OVERVIEW)
      if (kind === 'pr.review' || kind === 'pr.comment') mark(['workLog'])
      return
    }
    if (kind === 'issue.worklog') {
      if (e.issue_key) mark(['workLog', e.issue_key])
      else mark(['workLog'])
      mark(['autopilot'])
      return
    }
    if (kind === 'code_audit.updated') {
      const id = (e.data as { audit?: number } | undefined)?.audit
      mark(['audits'], ...(id ? [['audit', id]] : []))
      return
    }
    if (kind.startsWith('env.')) {
      mark(['environments'], ['pulls'], OVERVIEW)
      return
    }
    if (kind === 'runner.status') {
      mark(['runners'], OVERVIEW)
      return
    }
    if (kind === 'runner.job_updated' || kind === 'runner.job_finished') {
      const d = e.data as { job?: number; state?: string; size?: number } | undefined
      if (d?.job) mark(['runnerJob', d.job])
      if (kind === 'runner.job_finished' || d?.state !== 'running' || !d.size)
        mark(['runnerJobs'], ['runners'])
      if (kind === 'runner.job_finished') mark(OVERVIEW)
      return
    }
    if (kind === 'automation.run_finished') {
      mark(['automationRuns'], ['automationRun'], ['automations'], ['followUp'])
      return
    }
    if (kind === 'assistant.status') {
      const id = (e.data as { id?: number } | undefined)?.id
      mark(['assistants'], ...(id ? [['assistant', id, 'gate']] : []))
      return
    }
    if (kind.startsWith('kit.')) {
      mark(['kit'])
      return
    }
    if (kind === 'agent_setup.updated') {
      mark(['agentSetup'])
      return
    }
    if (kind === 'goals.drift') {
      mark(['goals'], ['trace'], ['issue'])
      return
    }
    if (kind === 'goals.traced') {
      // The issue page's goal line and risk also read traces, so they refresh too.
      mark(['goals'], ['trace'], ['issue'])
      return
    }
    if (kind === 'timer.changed') {
      mark(['timer'], ['time'], ['issueTime'], ['timeReport'])
      return
    }
    if (kind === 'knowledge.updated') {
      mark(
        ['glossary'],
        ['design'],
        ['links'],
        ['spec'],
        ['searchAll'],
        ['resolve'],
        ['attachments'],
        OVERVIEW,
        ['changes'],
        ['goals'],
        ['trace'],
      )
      return
    }
    if (kind === 'page.updated') {
      const id = (e.data as { page_id?: number } | undefined)?.page_id
      mark(['pages'], ['links'], id ? ['page', id] : ['page'], ['attachments'], OVERVIEW, [
        'changes',
      ])
      return
    }
    if (kind === 'notification') {
      mark(['notifications'], ['waiting'])
      const data = e.data as { kind?: string; read?: boolean; url?: string } | undefined
      if (data && !data.read && e.new)
        toast(e.new, { href: e.issue_key ? `/issue/${e.issue_key}` : data.url || undefined })
      return
    }
    if (kind.startsWith('request.')) {
      mark(
        ['requests'],
        ['request'],
        ['waiting'],
        ['sessions'],
        ['session'],
        ['attachments'],
        OVERVIEW,
      )
      return
    }
    if (kind.startsWith('session.')) {
      mark(['sessions'], ['session'], ['followUp'], ['requests'], ['attachments'], OVERVIEW)
      return
    }
    if (pk)
      mark(
        ['board', pk],
        ['backlog', pk],
        ['project', pk],
        ['activity', pk],
        ['report', pk],
        ['sprints', pk],
      )
    else mark(['board'], ['backlog'], ['project'])
    // The agent queue reads nearly every issue field: order, status, claims.
    mark(['search'], ['projects'], ['agents', 'queue'], ['agents', 'activity'], OVERVIEW, [
      'changes',
    ])
    // Objective changes arrive as issue.updated events with field 'objective'.
    if (e.field === 'objective' || kind === 'issue.status_changed') mark(['objectives'])
    if (e.issue_key) {
      mark(['issue', e.issue_key], ['history', e.issue_key], ['links', 'issue', e.issue_key])
      if (kind === 'issue.commented' || kind === 'project.updated')
        mark(['comments', e.issue_key], ['outside', e.issue_key])
    }
    // Parent issues show child progress, so child changes refresh the open issue page.
    if (
      kind === 'issue.status_changed' ||
      kind === 'issue.created' ||
      kind === 'issue.deleted' ||
      kind === 'issue.updated'
    )
      mark(['issue'])
    if (kind.startsWith('sprint.')) mark(['burndown'])
    mark(pk ? ['goals', pk] : ['goals'], ['trace'])
  }

  const kinds = [
    'issue.created',
    'issue.updated',
    'issue.status_changed',
    'issue.assigned',
    'issue.unassigned',
    'issue.commented',
    'issue.deleted',
    'issue.reopened',
    'issue.children_done',
    'issue.linked',
    'issue.shared',
    'issue.worklog',
    'external.pr_opened',
    'external.pr_merged',
    'external.pr_closed',
    'external.issue_closed',
    'sprint.started',
    'sprint.closed',
    'sprint.updated',
    'project.updated',
    'timer.changed',
    'notification',
    'knowledge.updated',
    'page.updated',
    'request.created',
    'request.updated',
    'session.started',
    'session.ended',
    'session.waiting',
    'session.updated',
    'session.finished',
    'session.message',
    'env.status',
    'env.deployed',
    'env.deploy_failed',
    'env.lagging',
    'env.created',
    'env.stopped',
    'env.updated',
    'runner.status',
    'runner.job_updated',
    'runner.job_finished',
    'bot.run',
    'pr.updated',
    'pr.command',
    'pr.activity',
    'pr.ci',
    'pr.review_gate',
    'pr.review',
    'pr.comment',
    'pr.babysit',
    'code_audit.updated',
    'automation.run_finished',
    'kit.updated',
    'kit.source_set',
    'kit.reported',
    'agent_setup.updated',
  ]

  // EventSource retries network drops itself, but gives up for good on an
  // HTTP error (a proxy 502 during a restart, or 401 once the session ends).
  // Check the session (a 401 sends us to sign in), then try again, backing off.
  const reconnect = () => {
    if (closed || retry !== undefined || document.visibilityState !== 'visible') return
    retry = window.setTimeout(() => {
      retry = undefined
      api.get('/me').then(connect, () => reconnect())
    }, backoff)
    backoff = Math.min(backoff * 2, 60_000)
  }

  const connect = () => {
    if (closed) return
    es?.close()
    es = new EventSource('/api/v1/stream')
    es.onerror = () => {
      if (es?.readyState === EventSource.CLOSED) reconnect()
    }
    es.addEventListener('hello', () => {
      backoff = 2000
      // Events sent while disconnected are not replayed, so a reconnect refreshes everything.
      if (connectedOnce) qc.invalidateQueries()
      connectedOnce = true
    })
    for (const k of kinds) {
      es.addEventListener(k, (m) => {
        try {
          handle(k, JSON.parse((m as MessageEvent).data))
        } catch {}
      })
    }
  }
  connect()

  const onVisible = () => {
    if (
      document.visibilityState === 'visible' &&
      es?.readyState === EventSource.CLOSED &&
      retry === undefined
    )
      connect()
  }
  document.addEventListener('visibilitychange', onVisible)

  return () => {
    closed = true
    window.clearTimeout(retry)
    es?.close()
    document.removeEventListener('visibilitychange', onVisible)
  }
}
