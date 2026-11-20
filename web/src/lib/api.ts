// Thin fetch wrapper for the Waypoint API.

export class ApiError extends Error {
  status: number
  /** what kind of refusal, when the server says: `reauth` asks for the password again */
  code?: string
  constructor(status: number, message: string, code?: string) {
    super(message)
    this.status = status
    this.code = code
  }
}

type Method = 'GET' | 'POST' | 'PATCH' | 'PUT' | 'DELETE'

let onUnauthorized: (() => void) | null = null
export function setUnauthorizedHandler(f: () => void) {
  onUnauthorized = f
}

export async function request<T = unknown>(
  method: Method,
  path: string,
  body?: unknown,
): Promise<T> {
  const headers: Record<string, string> = { 'X-Waypoint-Client': 'web' }
  let payload: BodyInit | undefined
  if (body instanceof Blob || body instanceof FormData) {
    payload = body
  } else if (body !== undefined) {
    headers['Content-Type'] = 'application/json'
    payload = JSON.stringify(body)
  }
  const res = await fetch('/api/v1' + path, {
    method,
    headers,
    body: payload,
    credentials: 'same-origin',
  })
  if (res.status === 204) return undefined as T
  const text = await res.text()
  let data: unknown = undefined
  if (text) {
    try {
      data = JSON.parse(text)
    } catch {
      data = text
    }
  }
  if (!res.ok) {
    if (res.status === 401 && onUnauthorized && !path.startsWith('/auth/')) onUnauthorized()
    const msg =
      (data && typeof data === 'object' && 'error' in data
        ? String((data as { error: string }).error)
        : res.statusText) || 'Request failed'
    const code =
      data && typeof data === 'object' && 'code' in data
        ? String((data as { code: string }).code)
        : undefined
    throw new ApiError(res.status, msg, code)
  }
  return data as T
}

export const api = {
  get: <T>(p: string) => request<T>('GET', p),
  post: <T>(p: string, b?: unknown) => request<T>('POST', p, b),
  patch: <T>(p: string, b?: unknown) => request<T>('PATCH', p, b),
  put: <T>(p: string, b?: unknown) => request<T>('PUT', p, b),
  del: <T>(p: string) => request<T>('DELETE', p),
}

export function qs(params: Record<string, string | number | boolean | undefined | null>): string {
  const u = new URLSearchParams()
  for (const [k, v] of Object.entries(params)) {
    if (v !== undefined && v !== null && v !== '' && v !== false) u.set(k, String(v))
  }
  const s = u.toString()
  return s ? '?' + s : ''
}

export const enc = encodeURIComponent
