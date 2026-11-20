package tracing

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// recordSpans turns tracing on with an in-memory exporter for one test.
func recordSpans(t *testing.T) *tracetest.InMemoryExporter {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	Use(tp)
	t.Cleanup(func() { Use(nil); _ = tp.Shutdown(context.Background()) })
	return exp
}

func TestConfigured(t *testing.T) {
	for _, c := range []struct {
		env  map[string]string
		want bool
	}{
		{map[string]string{}, false},
		{map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://otel:4318"}, true},
		{map[string]string{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "http://otel:4318/v1/traces"}, true},
		{map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://otel:4318", "OTEL_SDK_DISABLED": "true"}, false},
		{map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://otel:4318", "OTEL_TRACES_EXPORTER": "none"}, false},
	} {
		for _, k := range []string{"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "OTEL_SDK_DISABLED", "OTEL_TRACES_EXPORTER"} {
			t.Setenv(k, c.env[k])
		}
		if got := Configured(); got != c.want {
			t.Errorf("%v: configured = %v", c.env, got)
		}
	}
}

func TestOffIsInert(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	stop, err := Setup(context.Background(), "test", slog.Default())
	if err != nil || Enabled() {
		t.Fatalf("setup without an endpoint: enabled %v, err %v", Enabled(), err)
	}
	_ = stop(context.Background())
	ctx, span := Start(context.Background(), "x")
	if span.IsRecording() || ctx != context.Background() {
		t.Error("a span was recorded while off")
	}
	// Ending what Start returned must not end the caller's span.
	exp := recordSpans(t)
	outer, parent := Start(context.Background(), "outer")
	Use(nil)
	_, child := Start(outer, "inner")
	child.End()
	if !parent.IsRecording() {
		t.Error("ending a no-op child ended its parent")
	}
	parent.End()
	_ = exp
}

func TestRequestSpanNames(t *testing.T) {
	exp := recordSpans(t)
	r := httptest.NewRequest("GET", "/api/v1/issues/WAYP-12?token=s3cret", nil)
	r.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	ctx, span := StartRequest(r, false)
	EndRequest(span, r.Method, "GET /api/v1/issues/{key}", 200)
	_, other := StartRequest(httptest.NewRequest("POST", "/nope", nil), true)
	EndRequest(other, "POST", "", 404)
	spans := exp.GetSpans()
	if len(spans) != 2 || spans[0].Name != "GET /api/v1/issues/{key}" || spans[1].Name != "POST other" {
		t.Fatalf("names: %v", spans)
	}
	if spans[0].Parent.IsValid() || spans[0].SpanContext.TraceID().String() == "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Error("an untrusted traceparent was honoured")
	}
	for _, kv := range spans[0].Attributes {
		if v := kv.Value.String(); strings.Contains(v, "WAYP-12") || strings.Contains(v, "s3cret") {
			t.Errorf("attribute %s leaks the path: %s", kv.Key, v)
		}
	}
	_ = ctx

	// From a trusted proxy, the request joins the caller's trace.
	r = httptest.NewRequest("GET", "/healthz", nil)
	r.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	_, joined := StartRequest(r, true)
	EndRequest(joined, "GET", "GET /healthz", 200)
	if got := exp.GetSpans()[2].SpanContext.TraceID().String(); got != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Errorf("trusted traceparent ignored: trace %s", got)
	}
}

func TestTransport(t *testing.T) {
	var sawParent bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawParent = r.Header.Get("traceparent") != ""
		w.WriteHeader(http.StatusTeapot)
	}))
	defer srv.Close()
	c := &http.Client{Transport: Transport(http.DefaultTransport)}
	// Off: passes straight through.
	if res, err := c.Get(srv.URL + "/x"); err != nil || res.StatusCode != http.StatusTeapot {
		t.Fatalf("off: %v %v", res, err)
	}
	exp := recordSpans(t)
	res, err := c.Get(srv.URL + "/hooks/abc?token=s3cret")
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	spans := exp.GetSpans()
	if len(spans) != 1 || spans[0].Name != "GET" {
		t.Fatalf("spans: %v", spans)
	}
	attrs := map[string]string{}
	for _, kv := range spans[0].Attributes {
		attrs[string(kv.Key)] = kv.Value.String()
		if strings.Contains(kv.Value.String(), "s3cret") || strings.Contains(kv.Value.String(), "/hooks") {
			t.Errorf("%s leaks the URL: %s", kv.Key, kv.Value.String())
		}
	}
	if attrs["server.address"] != "127.0.0.1" || attrs["http.response.status_code"] != "418" {
		t.Errorf("attributes: %v", attrs)
	}
	if sawParent {
		t.Error("trace headers were sent to a third party")
	}
}

func TestLogHandler(t *testing.T) {
	recordSpans(t)
	var buf bytes.Buffer
	log := slog.New(LogHandler(slog.NewJSONHandler(&buf, nil)))
	ctx, span := Start(context.Background(), "x")
	log.InfoContext(ctx, "inside")
	span.End()
	log.Info("outside")
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if want := span.SpanContext().TraceID().String(); !strings.Contains(lines[0], `"trace_id":"`+want+`"`) {
		t.Errorf("no trace id: %s", lines[0])
	}
	if strings.Contains(lines[1], "trace_id") {
		t.Errorf("trace id outside a span: %s", lines[1])
	}
}
