package core

import (
	"context"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	forgeRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "waypoint_forge_requests_total",
		Help: "Requests Waypoint sent to forges, by forge kind, method, route template, status class (404 and 429 apart; error when no response came back) and source (the job, webhook or API route that led to it).",
	}, []string{"kind", "method", "route", "code", "source"})
	forgeRequestSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "waypoint_forge_request_duration_seconds",
		Help:    "Time to the forge's response headers (or the failure), by forge kind and route template.",
		Buckets: []float64{.025, .05, .1, .25, .5, 1, 2.5, 5, 10},
	}, []string{"kind", "route"})
)

type forgeSourceKey struct{}

// withForgeSource names the unit of work whose forge requests ctx carries:
// a scheduled job, the webhook handler, a background step, an API route.
// The innermost name wins, so a job that runs a named step is counted under
// the step. Names are code-defined, never an id, repo or user input, so the
// source label stays bounded.
func withForgeSource(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, forgeSourceKey{}, name)
}

// forgeSource is the name withForgeSource set, "other" when none was.
func forgeSource(ctx context.Context) string {
	if n, _ := ctx.Value(forgeSourceKey{}).(string); n != "" {
		return n
	}
	return "other"
}

// WithAPIForgeSource names an API request by its route pattern
// ("GET /api/v1/pulls/{id}/wait"); a request with none is just "api".
func WithAPIForgeSource(ctx context.Context, pattern string) context.Context {
	if pattern == "" {
		return withForgeSource(ctx, "api")
	}
	return withForgeSource(ctx, "api "+pattern)
}

// send is the one place a forge request leaves Waypoint: it sends req on the
// client for its context and counts it, whatever the call (JSON, raw, log
// tail, GraphQL).
func (c *httpForgeClient) send(f *Forge, req *http.Request) (*http.Response, error) {
	start := time.Now()
	resp, err := c.client(req.Context()).Do(req)
	route := forgeRoute(req.URL.EscapedPath())
	forgeRequestSeconds.WithLabelValues(f.Kind, route).Observe(time.Since(start).Seconds())
	code := "error"
	if err == nil {
		code = forgeCodeLabel(resp.StatusCode)
	}
	forgeRequests.WithLabelValues(f.Kind, req.Method, route, code, forgeSource(req.Context())).Inc()
	return resp, err
}

// forgeCodeLabel is the status class, with 404 and 429 kept as themselves.
func forgeCodeLabel(status int) string {
	switch {
	case status == 404 || status == 429:
		return strconv.Itoa(status)
	case status >= 100 && status < 600:
		return strconv.Itoa(status/100) + "xx"
	}
	return "error"
}

var (
	hexRe    = regexp.MustCompile(`^(?:[0-9a-fA-F]{40}|[0-9a-fA-F]{64})$`)
	numRe    = regexp.MustCompile(`^[0-9]+(\.[A-Za-z]+)?$`)
	digitsRe = regexp.MustCompile(`[0-9]`)
)

// forgeRestPlaceholder names what the rest of a path after a segment is.
var forgeRestPlaceholder = map[string]string{
	"branches": "{branch}", "compare": "{range}", "contents": "{path}",
	"raw": "{path}", "files": "{path}", "refs": "{ref}",
}

// forgeOneSegPlaceholder names the single segment after a segment.
var forgeOneSegPlaceholder = map[string]string{
	"trees": "{ref}", "commits": "{sha}", "statuses": "{sha}", "labels": "{label}",
	"users": "{user}", "orgs": "{org}", "groups": "{group}",
}

// forgeRoute is a low-cardinality template of an API path (escaped, query
// already dropped): no repository, branch, file path, sha or number survives.
func forgeRoute(path string) string {
	if i := strings.Index(path, "?"); i >= 0 {
		path = path[:i]
	}
	if i := strings.Index(path, "/api/v"); i >= 0 {
		path = path[i+len("/api/v"):]
		if j := strings.Index(path, "/"); j >= 0 {
			path = path[j:]
		} else {
			path = ""
		}
	}
	segs := strings.Split(strings.Trim(path, "/"), "/")
	if segs[len(segs)-1] == "graphql" {
		return "graphql"
	}
	var out []string
	for i := 0; i < len(segs); i++ {
		s := segs[i]
		prev := ""
		if i > 0 {
			prev = segs[i-1]
		}
		switch {
		case s == "":
		case prev == "repos":
			out = append(out, "{repo}")
			i++ // owner/name is two segments
		case prev == "projects":
			out = append(out, "{id}")
		case forgeRestPlaceholder[prev] != "":
			out = append(out, forgeRestPlaceholder[prev])
			i = len(segs)
		case forgeOneSegPlaceholder[prev] != "":
			out = append(out, forgeOneSegPlaceholder[prev])
		case hexRe.MatchString(s):
			out = append(out, "{sha}")
		case numRe.MatchString(s):
			ext := ""
			if j := strings.Index(s, "."); j >= 0 {
				ext = s[j:]
			}
			out = append(out, "{n}"+ext)
		case digitsRe.MatchString(s):
			out = append(out, "{x}")
		default:
			out = append(out, s)
		}
	}
	return "/" + strings.Join(out, "/")
}
