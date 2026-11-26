import type { Assistant, GateApproval, GateConnection } from '@/lib/assistant'

/**
 * The gate as the agent sees it: its side of the agent's network. Hermes
 * reaches a connection as an MCP server at {gate}/conn/<name>/mcp, and the
 * gate's own and Waypoint's MCP servers sit at /mcp and /waypoint/mcp.
 * Nothing else on that network answers the agent. The address is the
 * assistant's own (Admin → Assistants), else the site's; '' when neither
 * is set, and then nothing here can point Hermes at a connection.
 */
export const gateAgentUrl = (a: Pick<Assistant, 'agent_url' | 'site'>) =>
  (a.agent_url || a.site?.agent_url || '').replace(/\/+$/, '')

export const connMcpUrl = (gate: string, name: string) => `${gate}/conn/${name}/mcp`

const CONN_PATH = /^\/conn\/([a-z0-9][a-z0-9-]{0,39})\/mcp\/?$/

/** The connection a Hermes MCP server URL points at, if it is one of ours. */
export function connFromUrl(gate: string, url: string | null | undefined): string | null {
  if (!gate || !url?.startsWith(gate + '/')) return null
  const m = CONN_PATH.exec(url.slice(gate.length))
  return m ? m[1] : null
}

/** Hermes MCP servers the assistant's deploy pins: removing one is undone. */
export const PINNED_SERVERS = new Set(['gate', 'waypoint'])

export const METHODS = ['GET', 'HEAD', 'POST', 'PUT', 'PATCH', 'DELETE', 'OPTIONS'] as const
export type Method = (typeof METHODS)[number]
export type MethodRule = 'free' | 'ask' | 'refuse'

export const DEFAULT_HTTP = {
  allow_methods: ['GET', 'HEAD'],
  approve_methods: ['POST', 'PUT', 'PATCH', 'DELETE'],
  paths: ['/'],
}

const list = (p: Record<string, string[]> | undefined, k: string, d: string[] = []) =>
  Array.isArray(p?.[k]) ? p![k] : d

export function httpPolicy(p: Record<string, string[]> | undefined) {
  return {
    allow_methods: list(p, 'allow_methods', DEFAULT_HTTP.allow_methods),
    approve_methods: list(p, 'approve_methods', DEFAULT_HTTP.approve_methods),
    paths: list(p, 'paths', DEFAULT_HTTP.paths),
  }
}

export function mcpPolicy(p: Record<string, string[]> | undefined) {
  return { allow: list(p, 'allow'), deny: list(p, 'deny') }
}

export function methodRules(p: Record<string, string[]> | undefined): Record<Method, MethodRule> {
  const h = httpPolicy(p)
  return Object.fromEntries(
    METHODS.map((m) => [
      m,
      h.allow_methods.includes(m) ? 'free' : h.approve_methods.includes(m) ? 'ask' : 'refuse',
    ]),
  ) as Record<Method, MethodRule>
}

/** The policy as one line: "GET, HEAD freely · POST asks you · under /api/documents". */
export function policyWords(c: Pick<GateConnection, 'kind' | 'policy'>): string {
  if (c.kind === 'mcp') {
    const p = mcpPolicy(c.policy)
    const parts: string[] = []
    if (p.allow.length) parts.push(`${p.allow.join(', ')} freely`)
    if (p.deny.length) parts.push(`${p.deny.join(', ')} refused`)
    parts.push(
      p.allow.length || p.deny.length ? 'every other tool asks you' : 'every tool asks you',
    )
    return parts.join(' · ')
  }
  const h = httpPolicy(c.policy)
  if (!h.allow_methods.length && !h.approve_methods.length) return 'every call refused'
  const parts: string[] = []
  if (h.allow_methods.length) parts.push(`${h.allow_methods.join(', ')} freely`)
  if (h.approve_methods.length)
    parts.push(
      `${h.approve_methods.join(', ')} ${h.approve_methods.length === 1 ? 'asks' : 'ask'} you`,
    )
  const refused = METHODS.filter(
    (m) => m !== 'OPTIONS' && !h.allow_methods.includes(m) && !h.approve_methods.includes(m),
  )
  if (refused.length) parts.push(`${refused.join(', ')} refused`)
  const paths = h.paths.filter((x) => x !== '/')
  parts.push(
    !h.paths.length
      ? 'no paths'
      : paths.length < h.paths.length
        ? 'any path'
        : `under ${paths.join(', ')}`,
  )
  return parts.join(' · ')
}

export const NAME = /^[a-z0-9][a-z0-9-]{0,39}$/
const HEADER_NAME = /^[A-Za-z0-9!#$%&'*+.^_`|~-]{1,64}$/
const FORBIDDEN_HEADERS = new Set([
  'host',
  'content-length',
  'transfer-encoding',
  'connection',
  'upgrade',
  'te',
  'trailer',
  'keep-alive',
  'proxy-authorization',
  'proxy-connection',
])

export function nameError(name: string, taken: (n: string) => boolean): string | undefined {
  if (!name) return 'Required'
  if (!NAME.test(name)) {
    if (/[A-Z]/.test(name)) return 'Lowercase only'
    if (name.length > 40) return 'At most 40 characters'
    if (name.startsWith('-')) return 'Start with a letter or digit'
    return 'Letters, digits and dashes only'
  }
  if (PINNED_SERVERS.has(name)) return `This assistant already has a "${name}" server`
  if (taken(name)) return 'There is a connection with this name; edit that one'
  return undefined
}

function blockedHost(host: string): boolean {
  const h = host.replace(/^\[|\]$/g, '').toLowerCase()
  if (h === 'localhost' || h === 'localhost.localdomain' || h.endsWith('.localhost')) return true
  if (h === '::1' || h === '::' || h.startsWith('fe80:') || h.startsWith('::ffff:')) return true
  const v4 = /^(\d+)\.(\d+)\.(\d+)\.(\d+)$/.exec(h)
  if (!v4) return false
  const [a, b, c] = v4.slice(1).map(Number)
  return (
    a === 127 ||
    a === 0 ||
    (a === 169 && b === 254) ||
    a >= 224 ||
    (a === 192 && b === 168 && c === 254)
  )
}

export function urlError(raw: string): string | undefined {
  const url = raw.trim()
  if (!url) return 'Required'
  if (url.length > 300) return 'At most 300 characters'
  let u: URL
  try {
    u = new URL(url)
  } catch {
    return 'Not a URL'
  }
  if ((u.protocol !== 'http:' && u.protocol !== 'https:') || !u.hostname)
    return 'Use http:// or https:// with a host'
  if (u.username || u.password) return 'Put credentials in headers, not the URL'
  if (url.includes('?') || url.includes('#')) return 'No query or fragment'
  if (blockedHost(u.hostname)) return "Can't point at the gate, the agent's network or this machine"
  return undefined
}

export function headerNameError(name: string): string | undefined {
  if (!HEADER_NAME.test(name)) return 'Letters, digits and - only'
  if (FORBIDDEN_HEADERS.has(name.toLowerCase())) return 'The gate sets this one itself'
  return undefined
}

export function headerValueError(v: string): string | undefined {
  if (!v) return 'Enter the value'
  if (v.length > 8192) return 'At most 8192 characters'
  if (/[\r\n\0]/.test(v)) return 'One line only'
  return undefined
}

export function pathError(p: string): string | undefined {
  if (!p.startsWith('/')) return 'Start with /'
  if (p.includes('//') || /[\\?#;\s]/.test(p)) return 'Not a plain path'
  if (/%2e|%2f|%5c|%00/i.test(p)) return 'No encoded dots or slashes'
  if (p.split('/').some((s) => s === '.' || s === '..')) return 'No . or .. segments'
  if (p.length > 200) return 'At most 200 characters'
  return undefined
}

export function globError(g: string): string | undefined {
  if (!g.trim() || g.length > 200) return 'At most 200 characters'
  return undefined
}

const same = (a: unknown, b: unknown) => JSON.stringify(a) === JSON.stringify(b)

/** Policies as the gate compares them: list by list, in order. */
function samePolicy(a: Record<string, string[]> | undefined, b: Record<string, string[]>) {
  const keys = new Set([...Object.keys(a ?? {}), ...Object.keys(b)])
  return [...keys].every((k) => same(a?.[k] ?? null, b[k] ?? null))
}

/**
 * Lists in the order the connection already has when only their order
 * would differ: the gate compares policies exactly, so a reordering alone
 * would otherwise go to Signal as a change.
 */
export function keepOrder(next: string[], prev: string[] | undefined): string[] {
  if (!prev || prev.length !== next.length) return next
  const a = new Set(next)
  return prev.every((x) => a.has(x)) ? prev : next
}

/**
 * Whether the gate will hold a save for the owner's approval: a new connection,
 * or a new kind, URL, policy or set of header names. (Same header names with
 * new values, and a new description, save at once.)
 */
export function widens(
  old: GateConnection | undefined,
  next: {
    kind: string
    url: string
    policy: Record<string, string[]>
    headers: Record<string, string>
  },
): boolean {
  if (!old) return true
  if (old.kind !== next.kind || old.url !== next.url) return true
  if (!samePolicy(old.policy, next.policy)) return true
  return !same([...(old.header_names ?? [])].sort(), Object.keys(next.headers).sort())
}

export const approvalConn = (a: GateApproval) =>
  a.tool.startsWith('connection:') ? a.tool.slice('connection:'.length) : null

/** The newest gate approval about each connection. */
export function latestApprovals(
  data: { pending: GateApproval[] | null; recent: GateApproval[] | null } | undefined,
): Map<string, GateApproval> {
  const out = new Map<string, GateApproval>()
  for (const a of [...(data?.pending ?? []), ...(data?.recent ?? [])]) {
    const n = approvalConn(a)
    if (!n) continue
    const cur = out.get(n)
    if (!cur || a.created > cur.created) out.set(n, a)
  }
  return out
}
