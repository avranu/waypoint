package api

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	logspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb2 "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/protobuf/proto"

	"git.jmann.me/claude/waypoint/internal/core"
)

// A payload shaped like Claude Code's: resource attributes on the resource,
// event attributes (strings, as the docs list them) on each record.
func otlpFixture(now time.Time, session string) string {
	ns := now.UnixNano()
	return fmt.Sprintf(`{"resourceLogs":[{"resource":{"attributes":[
	  {"key":"service.name","value":{"stringValue":"claude-code"}},
	  {"key":"host.name","value":{"stringValue":"devhost"}}]},
	 "scopeLogs":[{"scope":{"name":"com.anthropic.claude_code.events"},"logRecords":[
	  {"timeUnixNano":"%d","body":{"stringValue":"claude_code.api_request"},"attributes":[
	    {"key":"event.name","value":{"stringValue":"api_request"}},
	    {"key":"event.sequence","value":{"intValue":"3"}},
	    {"key":"session.id","value":{"stringValue":%q}},
	    {"key":"user.email","value":{"stringValue":"someone-else@example.com"}},
	    {"key":"model","value":{"stringValue":"claude-sonnet-5-5"}},
	    {"key":"input_tokens","value":{"stringValue":"120"}},
	    {"key":"output_tokens","value":{"intValue":"30"}},
	    {"key":"cache_read_tokens","value":{"stringValue":"900"}},
	    {"key":"cache_creation_tokens","value":{"stringValue":"50"}},
	    {"key":"cost_usd","value":{"stringValue":"0.01"}},
	    {"key":"workspace.host_paths","value":{"arrayValue":{"values":[{"stringValue":"/home/robin/projects/waypoint"}]}}}]},
	  {"timeUnixNano":"%d","attributes":[
	    {"key":"event.name","value":{"stringValue":"user_prompt"}},
	    {"key":"session.id","value":{"stringValue":%q}},
	    {"key":"prompt","value":{"stringValue":"TOPSECRET prompt text"}}]}]}]}]}`, ns, session, ns, session)
}

func otlpPost(t *testing.T, base, path, token, ctype, enc string, body []byte) (int, string, http.Header) {
	t.Helper()
	req, _ := http.NewRequest("POST", base+path, bytes.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	if enc != "" {
		req.Header.Set("Content-Encoding", enc)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

func gz(t *testing.T, s []byte) []byte {
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	if _, err := w.Write(s); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	return b.Bytes()
}

func TestOTLPReceiver(t *testing.T) {
	ts, svc := setup(t)
	ctx := context.Background()
	u, _ := svc.CreateUser(ctx, core.SystemActor(), core.CreateUserInput{Username: "pat", Email: "p@example.com", Password: "correct horse battery"})
	actor := &core.Actor{User: u}
	tok, _, err := svc.CreateAPIToken(ctx, actor, "otel", false, 0)
	if err != nil {
		t.Fatal(err)
	}
	ro, _, _ := svc.CreateAPIToken(ctx, actor, "ro", true, 0)
	logs, metrics := "/api/v1/otlp/v1/logs", "/api/v1/otlp/v1/metrics"
	body := []byte(otlpFixture(time.Now(), "sess-1"))
	messages := func() int64 {
		o, err := svc.UsageOverview(ctx, actor, core.UsageQuery{Days: 7})
		if err != nil {
			t.Fatal(err)
		}
		return o.Totals.Messages
	}

	if code, _, _ := otlpPost(t, ts.URL, logs, "", "application/json", "", body); code != 401 {
		t.Errorf("no token: %d", code)
	}
	if code, _, _ := otlpPost(t, ts.URL, logs, "wrong", "application/json", "", body); code != 401 {
		t.Errorf("bad token: %d", code)
	}
	if code, _, _ := otlpPost(t, ts.URL, logs, ro, "application/json", "", body); code != 403 {
		t.Errorf("read-only token: %d", code)
	}
	if n := messages(); n != 0 {
		t.Fatalf("stored without auth: %d", n)
	}

	code, out, h := otlpPost(t, ts.URL, logs, tok, "application/json", "", body)
	if code != 200 || !strings.Contains(out, "partialSuccess") || !strings.HasPrefix(h.Get("Content-Type"), "application/json") {
		t.Errorf("json: %d %q %v", code, out, h)
	}
	if n := messages(); n != 1 {
		t.Errorf("messages after first post: %d", n)
	}
	// an exporter's retry, this time gzipped
	if code, _, _ := otlpPost(t, ts.URL, logs, tok, "application/json; charset=utf-8", "gzip", gz(t, body)); code != 200 {
		t.Errorf("gzip: %d", code)
	}
	if n := messages(); n != 1 {
		t.Errorf("a retry counted again: %d", n)
	}

	if code, out, _ := otlpPost(t, ts.URL, logs, tok, "application/json", "", []byte(`{"resourceLogs": [`)); code != 400 || !strings.Contains(out, "invalid OTLP JSON") {
		t.Errorf("bad json: %d %q", code, out)
	}
	if code, _, _ := otlpPost(t, ts.URL, logs, tok, "application/json", "gzip", []byte("not gzip")); code != 400 {
		t.Errorf("bad gzip: %d", code)
	}
	if code, out, _ := otlpPost(t, ts.URL, logs, tok, "application/grpc", "", body); code != 415 || !strings.Contains(out, "http/json") {
		t.Errorf("media type: %d %q", code, out)
	}
	if code, _, _ := otlpPost(t, ts.URL, logs, tok, "application/x-protobuf", "", []byte("\xff\xff\xff")); code != 400 {
		t.Errorf("bad protobuf: %d", code)
	}
	if code, _, _ := otlpPost(t, ts.URL, logs, tok, "application/json", "", bytes.Repeat([]byte(" "), otlpMaxBody+10)); code != 413 {
		t.Errorf("too large: %d", code)
	}

	// metrics are accepted and dropped
	if code, out, _ := otlpPost(t, ts.URL, metrics, tok, "application/json", "", []byte(`{"resourceMetrics":[]}`)); code != 200 || !strings.Contains(out, "partialSuccess") {
		t.Errorf("metrics: %d %q", code, out)
	}
	if code, _, _ := otlpPost(t, ts.URL, metrics, "", "application/json", "", []byte(`{}`)); code != 401 {
		t.Errorf("metrics without token: %d", code)
	}
	if code, _, _ := otlpPost(t, ts.URL, metrics, tok, "application/json", "", []byte(`{`)); code != 400 {
		t.Errorf("bad metrics: %d", code)
	}

	// protobuf, as OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf sends it
	str := func(k, v string) *commonpb.KeyValue {
		return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
	}
	req := &logspb.ExportLogsServiceRequest{ResourceLogs: []*logspb2.ResourceLogs{{ScopeLogs: []*logspb2.ScopeLogs{{LogRecords: []*logspb2.LogRecord{{
		TimeUnixNano: uint64(time.Now().UnixNano()),
		Attributes: []*commonpb.KeyValue{str("event.name", "api_request"), str("session.id", "sess-pb"), str("model", "claude-haiku-4-5"),
			str("input_tokens", "7"), str("output_tokens", "3")},
	}}}}}}}
	pb, _ := proto.Marshal(req)
	if code, _, h := otlpPost(t, ts.URL, logs, tok, "application/x-protobuf", "", pb); code != 200 || h.Get("Content-Type") != "application/x-protobuf" {
		t.Errorf("protobuf: %d %v", code, h)
	}
	if n := messages(); n != 2 {
		t.Errorf("messages after protobuf: %d", n)
	}
}
