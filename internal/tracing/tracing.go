// Package tracing is Waypoint's optional OpenTelemetry tracing. It is off
// unless an OTLP endpoint is configured with the standard environment
// variables, and while off every helper here is a no-op that allocates
// nothing and starts no goroutines.
//
// Spans carry routes, method names and status codes, never request paths,
// query strings, issue keys, URLs or error messages: those can hold tokens
// and private text, and traces usually leave the machine.
package tracing

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync/atomic"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

var (
	on      atomic.Bool
	current atomic.Pointer[trace.Tracer]
	parents = propagation.TraceContext{}
	// noop is what helpers return while off: ending it does nothing (unlike
	// the caller's own span, which a deferred End would close early).
	noop = trace.SpanFromContext(context.Background())
)

// Enabled reports whether spans are being recorded.
func Enabled() bool { return on.Load() }

// Configured reports whether the environment asks for traces: an OTLP
// endpoint is set and the SDK isn't disabled.
func Configured() bool {
	if strings.EqualFold(os.Getenv("OTEL_SDK_DISABLED"), "true") {
		return false
	}
	switch strings.ToLower(os.Getenv("OTEL_TRACES_EXPORTER")) {
	case "", "otlp":
	default:
		return false
	}
	return os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") != "" || os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != ""
}

// Setup starts exporting traces over OTLP/HTTP when Configured, reading the
// standard OTEL_* variables (endpoint, headers, OTEL_SERVICE_NAME, default
// "waypoint"; OTEL_RESOURCE_ATTRIBUTES; OTEL_TRACES_SAMPLER and its ARG).
// The returned function flushes and stops; it's safe to call when off.
func Setup(ctx context.Context, version string, log *slog.Logger) (func(context.Context) error, error) {
	if !Configured() {
		return func(context.Context) error { return nil }, nil
	}
	exp, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, err
	}
	res, err := resource.New(ctx,
		resource.WithAttributes(attribute.String("service.name", "waypoint"), attribute.String("service.version", version)),
		resource.WithFromEnv(),
		resource.WithTelemetrySDK(),
	)
	if err != nil && !errors.Is(err, resource.ErrPartialResource) {
		return nil, err
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp), sdktrace.WithResource(res))
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) { log.Warn("tracing", "err", err) }))
	Use(tp)
	return func(ctx context.Context) error {
		on.Store(false)
		return tp.Shutdown(ctx)
	}, nil
}

// Use makes tp the provider for Waypoint's spans (tests pass an in-memory
// one; nil turns tracing off again).
func Use(tp trace.TracerProvider) {
	if tp == nil {
		on.Store(false)
		current.Store(nil)
		return
	}
	t := tp.Tracer("waypoint")
	current.Store(&t)
	on.Store(true)
}

func tracer() trace.Tracer {
	if t := current.Load(); t != nil {
		return *t
	}
	return otel.Tracer("waypoint")
}

// Start begins a span named name under ctx's span, or returns ctx and a
// non-recording span when tracing is off.
func Start(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	if !on.Load() {
		return ctx, noop
	}
	return tracer().Start(ctx, name, trace.WithAttributes(attrs...))
}

// Fail marks a span failed with a category, not the message (messages can
// hold user text).
func Fail(span trace.Span, kind string) {
	if !span.IsRecording() {
		return
	}
	span.SetStatus(codes.Error, kind)
	span.SetAttributes(attribute.String("error.type", kind))
}

// StartRequest begins the server span for an incoming request. Its name is
// the method until EndRequest knows the route. A traceparent header is
// honoured only when trustParent is set (a request from a trusted proxy),
// so clients can't force sampling or join traces.
func StartRequest(r *http.Request, trustParent bool) (context.Context, trace.Span) {
	ctx := r.Context()
	if !on.Load() {
		return ctx, noop
	}
	if trustParent {
		ctx = parents.Extract(ctx, propagation.HeaderCarrier(r.Header))
	}
	return tracer().Start(ctx, r.Method,
		trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(attribute.String("http.request.method", r.Method)))
}

// EndRequest names the span after the matched route ("GET
// /api/v1/issues/{key}", never the raw path) and ends it.
func EndRequest(span trace.Span, method, route string, status int) {
	if !span.IsRecording() {
		return
	}
	path := route
	if m, p, ok := strings.Cut(route, " "); ok {
		method, path = m, p
	}
	if path == "" {
		path = "other"
	}
	span.SetName(method + " " + path)
	span.SetAttributes(attribute.String("http.route", path), attribute.Int("http.response.status_code", status))
	if status >= 500 {
		span.SetStatus(codes.Error, "")
	}
	span.End()
}

// Transport wraps an outbound transport with a client span per request:
// method, host and status, nothing from the path or query (webhook and
// Gotify URLs carry tokens). No trace headers are sent: these are other
// people's servers.
func Transport(next http.RoundTripper) http.RoundTripper {
	return roundTripper{next}
}

type roundTripper struct{ next http.RoundTripper }

func (t roundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	if !on.Load() {
		return t.next.RoundTrip(r)
	}
	ctx, span := tracer().Start(r.Context(), r.Method,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("http.request.method", r.Method),
			attribute.String("server.address", r.URL.Hostname()),
		))
	defer span.End()
	res, err := t.next.RoundTrip(r.WithContext(ctx))
	if err != nil {
		Fail(span, "transport")
		return res, err
	}
	span.SetAttributes(attribute.Int("http.response.status_code", res.StatusCode))
	if res.StatusCode >= 400 {
		span.SetStatus(codes.Error, "")
	}
	return res, nil
}

// LogHandler adds trace_id and span_id to log records made inside a
// recorded span, so logs and traces join up.
func LogHandler(h slog.Handler) slog.Handler { return logHandler{h} }

type logHandler struct{ slog.Handler }

func (h logHandler) Handle(ctx context.Context, r slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		r = r.Clone()
		r.AddAttrs(slog.String("trace_id", sc.TraceID().String()), slog.String("span_id", sc.SpanID().String()))
	}
	return h.Handler.Handle(ctx, r)
}

func (h logHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return logHandler{h.Handler.WithAttrs(as)}
}

func (h logHandler) WithGroup(name string) slog.Handler {
	return logHandler{h.Handler.WithGroup(name)}
}
