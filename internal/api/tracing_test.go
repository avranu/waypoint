package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"git.jmann.me/claude/waypoint/internal/core"
	"git.jmann.me/claude/waypoint/internal/db"
	"git.jmann.me/claude/waypoint/internal/tracing"
)

func TestRequestSpans(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	tracing.Use(tp)
	t.Cleanup(func() { tracing.Use(nil); _ = tp.Shutdown(context.Background()) })

	ctx := context.Background()
	d, err := db.OpenFresh(ctx, filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	svc := core.New(d, core.Config{BaseURL: "http://wp.test"}, nil)
	svc.StartHub()
	t.Cleanup(svc.Wait)
	link, err := svc.BootstrapInvite(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tok := link.URL[strings.LastIndex(link.URL, "/")+1:]
	for _, trusted := range []bool{false, true} {
		cfg := Config{BaseURL: "http://wp.test", Version: "test"}
		if trusted {
			cfg.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}
		}
		ts := httptest.NewServer(New(svc, cfg, svc.Log()).Handler())
		exp.Reset()
		req, _ := http.NewRequest("GET", ts.URL+"/api/v1/auth/link/"+tok+"?x=s3cret", nil)
		req.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		ts.Close()
		var span sdktrace.ReadOnlySpan
		for _, s := range exp.GetSpans().Snapshots() {
			if s.Name() == "GET /api/v1/auth/link/{token}" {
				span = s
			}
			for _, kv := range s.Attributes() {
				if v := kv.Value.String(); strings.Contains(v, tok) || strings.Contains(v, "s3cret") {
					t.Errorf("%s: %s leaks the request: %s", s.Name(), kv.Key, v)
				}
			}
		}
		if span == nil {
			t.Fatalf("no span named by route among %d", len(exp.GetSpans()))
		}
		joined := span.SpanContext().TraceID().String() == "4bf92f3577b34da6a3ce929d0e0e4736"
		if joined != trusted {
			t.Errorf("trusted proxy %v: joined the caller's trace = %v", trusted, joined)
		}
	}
}
