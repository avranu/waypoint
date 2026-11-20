package core

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"git.jmann.me/claude/waypoint/internal/tracing"
)

func TestWriteAndDispatchSpans(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	tracing.Use(tp)
	t.Cleanup(func() { tracing.Use(nil); _ = tp.Shutdown(context.Background()) })

	e := newEnv(t)
	exp.Reset()
	p := e.project(e.admin, "Alpha", "kanban")
	if _, err := e.s.CreateIssue(e.ctx, e.admin, IssueInput{Project: p.Key, Title: "Secret plans", Type: "task"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.CreateIssue(e.ctx, e.admin, IssueInput{Project: "NOPE", Title: "x", Type: "task"}); err == nil {
		t.Fatal("created in a missing project")
	}
	e.s.Wait()
	var writes []sdktrace.ReadOnlySpan
	var dispatch sdktrace.ReadOnlySpan
	for _, s := range exp.GetSpans().Snapshots() {
		switch s.Name() {
		case "db.write CreateIssue":
			writes = append(writes, s)
		case "dispatch":
			if slices.ContainsFunc(s.Attributes(), func(kv attribute.KeyValue) bool {
				return kv.Key == "waypoint.events" && slices.Contains(kv.Value.AsStringSlice(), EvIssueCreated)
			}) {
				dispatch = s
			}
		}
	}
	if len(writes) != 2 {
		t.Fatalf("want a write span per CreateIssue, got %d", len(writes))
	}
	ok, failed := writes[0], writes[1]
	if !slices.ContainsFunc(ok.Attributes(), func(kv attribute.KeyValue) bool { return kv.Key == "db.wait_ms" }) {
		t.Errorf("write span has no wait time: %v", ok.Attributes())
	}
	if failed.Status().Description != "not_found" {
		t.Errorf("failed write status: %+v", failed.Status())
	}
	if dispatch == nil || dispatch.Parent().SpanID() != ok.SpanContext().SpanID() {
		t.Errorf("dispatch isn't a child of its write: %v", dispatch)
	}
	for _, s := range exp.GetSpans().Snapshots() {
		for _, kv := range s.Attributes() {
			if v := kv.Value.String(); v == "Secret plans" {
				t.Errorf("%s carries issue text in %s", s.Name(), kv.Key)
			}
		}
	}
}

func TestEgressClientSpans(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	// Off when the client is made: no wrapper at all.
	if _, wrapped := (EgressPolicy{AllowLoopback: true}).client(time.Second).Transport.(*http.Transport); !wrapped {
		t.Error("the transport is wrapped while tracing is off")
	}
	tracing.Use(tp)
	t.Cleanup(func() { tracing.Use(nil); _ = tp.Shutdown(context.Background()) })
	c := (EgressPolicy{AllowLoopback: true}).client(time.Second)
	res, err := c.Post(srv.URL+"/message?token=s3cret", "text/plain", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if spans := exp.GetSpans(); len(spans) != 1 || spans[0].Name != "POST" {
		t.Errorf("outbound spans: %v", spans)
	}
}
