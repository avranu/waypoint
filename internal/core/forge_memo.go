package core

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// aheadCacheMax bounds the compare memory: the oldest answer goes first.
const aheadCacheMax = 4096

// aheadCache remembers how far one commit is ahead of another. A comparison of
// two commit ids never changes, so the answer needs no expiry; only a bound on
// memory. Branch names move and are never put here, nor are errors (WP-1980).
type aheadCache struct {
	mu    sync.Mutex
	m     map[string]int
	order []string
}

func (a *aheadCache) get(key string) (int, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	n, ok := a.m[key]
	return n, ok
}

func (a *aheadCache) put(key string, n int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.m == nil {
		a.m = map[string]int{}
	}
	if _, ok := a.m[key]; !ok {
		if len(a.order) >= aheadCacheMax {
			delete(a.m, a.order[0])
			a.order = a.order[1:]
		}
		a.order = append(a.order, key)
	}
	a.m[key] = n
}

// aheadKey names a comparison of two full commit ids (40 hex digits, or 64 for
// SHA-256 repositories) per forge and repository; ok is false when either side
// is a branch, tag or abbreviated id, which can name different commits later.
func aheadKey(f *Forge, repo, base, head string) (key string, ok bool) {
	if !fullCommitID(base) || !fullCommitID(head) {
		return "", false
	}
	return fmt.Sprintf("%d/%s/%s/%s", f.ID, strings.ToLower(repo), strings.ToLower(base), strings.ToLower(head)), true
}

func fullCommitID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
			return false
		}
	}
	return true
}

// tipMemoMaxAge caps how long a base tip read in one sweep is reused, so a
// slow sweep doesn't decide on a tip minutes old.
const tipMemoMaxAge = 20 * time.Second

// tipMemo is the base-branch tips read during one sweep of armed pull
// requests, so a base shared by many is read once. It lives in the sweep's
// context, never across sweeps. A merge Waypoint makes into any base bumps gen
// and so drops every tip read before it: the next pull request must see the
// base it just changed.
type tipMemo struct {
	gen *atomic.Uint64
	mu  sync.Mutex
	m   map[string]tipRead
}

type tipRead struct {
	tip string
	gen uint64
	at  time.Time
}

type tipMemoKey struct{}

func withTipMemo(ctx context.Context, gen *atomic.Uint64) context.Context {
	return context.WithValue(ctx, tipMemoKey{}, &tipMemo{gen: gen, m: map[string]tipRead{}})
}

func tipMemoOf(ctx context.Context) *tipMemo {
	m, _ := ctx.Value(tipMemoKey{}).(*tipMemo)
	return m
}

func (m *tipMemo) get(key string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.m[key]
	if !ok || r.gen != m.gen.Load() || time.Since(r.at) > tipMemoMaxAge {
		return "", false
	}
	return r.tip, true
}

// put keeps tip, tagged with the generation read before the forge was asked:
// a merge made during the read leaves it already out of date.
func (m *tipMemo) put(key, tip string, gen uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.m[key] = tipRead{tip: tip, gen: gen, at: time.Now()}
}
