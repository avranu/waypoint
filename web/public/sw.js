// Waypoint service worker: hashed assets are cached forever; the app shell
// is network-first so deploys show up immediately, with a cached fallback
// for flaky mobile connections. API calls are never cached.
const SHELL = 'waypoint-shell-v2'
const ASSETS = 'waypoint-assets-v2'
const MAX_ASSETS = 150
const NAV_TIMEOUT = 4000

self.addEventListener('install', () => self.skipWaiting())
self.addEventListener('activate', (e) => {
  e.waitUntil(
    caches
      .keys()
      .then((keys) =>
        Promise.all(keys.filter((k) => k !== SHELL && k !== ASSETS).map((k) => caches.delete(k))),
      )
      .then(() => self.clients.claim()),
  )
})

async function trim(cache) {
  const keys = await cache.keys() // insertion order: oldest first
  await Promise.all(
    keys.slice(0, Math.max(0, keys.length - MAX_ASSETS)).map((k) => cache.delete(k)),
  )
}

self.addEventListener('fetch', (e) => {
  const url = new URL(e.request.url)
  if (e.request.method !== 'GET' || url.origin !== location.origin) return
  if (
    url.pathname.startsWith('/api/') ||
    url.pathname.startsWith('/cal/') ||
    url.pathname === '/mcp'
  )
    return
  if (url.pathname.startsWith('/assets/')) {
    e.respondWith(
      caches.open(ASSETS).then(async (c) => {
        const hit = await c.match(e.request)
        if (hit) return hit
        const res = await fetch(e.request)
        if (res.ok) e.waitUntil(c.put(e.request, res.clone()).then(() => trim(c)))
        return res
      }),
    )
    return
  }
  if (e.request.mode === 'navigate') {
    const network = fetch(e.request).then((res) => {
      if (res.ok) {
        const copy = res.clone()
        e.waitUntil(caches.open(SHELL).then((c) => c.put('/', copy)))
      }
      return res
    })
    const fallback = () => caches.match('/', { cacheName: SHELL })
    e.respondWith(
      new Promise((resolve) => {
        const t = setTimeout(() => fallback().then((hit) => hit && resolve(hit)), NAV_TIMEOUT)
        network
          .then((res) => (clearTimeout(t), resolve(res)))
          .catch(
            () => (clearTimeout(t), fallback().then((hit) => resolve(hit ?? Response.error()))),
          )
      }),
    )
  }
})

// Web push: show the notification, and open the issue (or Inbox) on click.
self.addEventListener('push', (e) => {
  let m = {}
  try {
    m = e.data ? e.data.json() : {}
  } catch {
    m = { body: e.data ? e.data.text() : '' }
  }
  e.waitUntil(
    self.registration.showNotification(m.title || 'Waypoint', {
      body: m.body || '',
      tag: m.tag || undefined,
      icon: '/icon-192.png',
      badge: '/icon-192.png',
      data: { url: m.url || '/inbox', body: m.body || '' },
      actions: Array.isArray(m.actions) ? m.actions : undefined,
    }),
  )
})

// The quota alert's Pause button (WP-146): pause every agent, running
// sessions included, with the alert as the note agents see; then open
// Sessions, where Resume is. Same-origin cookies authenticate it.
function pauseSessions(note) {
  return fetch('/api/v1/agents/pauses', {
    method: 'POST',
    credentials: 'same-origin',
    headers: { 'Content-Type': 'application/json', 'X-Waypoint-Client': 'web' },
    body: JSON.stringify({
      spec: 'any',
      sessions: true,
      note: ('Quota alert: ' + note).slice(0, 300),
    }),
  })
    .then((r) => {
      if (!r.ok) throw new Error('HTTP ' + r.status)
    })
    .catch((err) =>
      self.registration.showNotification('Pause failed', {
        body:
          'Sessions are still running (' +
          (err && err.message ? err.message : 'network error') +
          ').',
        tag: 'pause-failed',
        icon: '/icon-192.png',
      }),
    )
}

self.addEventListener('notificationclick', (e) => {
  e.notification.close()
  let url = new URL(e.notification.data?.url || '/inbox', location.origin).href
  const paused =
    e.action === 'pause-sessions'
      ? pauseSessions(e.notification.data?.body || '')
      : Promise.resolve()
  if (e.action === 'pause-sessions') url = new URL('/agents?tab=sessions', location.origin).href
  e.waitUntil(
    paused.then(() =>
      self.clients.matchAll({ type: 'window', includeUncontrolled: true }).then((list) => {
        for (const c of list) {
          if (new URL(c.url).origin === location.origin && 'focus' in c) {
            return c.focus().then((w) => w.navigate?.(url))
          }
        }
        return self.clients.openWindow(url)
      }),
    ),
  )
})
