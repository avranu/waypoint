package api

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"math"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	logspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	"google.golang.org/protobuf/proto"

	"git.jmann.me/claude/waypoint/internal/core"
)

// OTLP/HTTP receiver for Claude Code's OpenTelemetry export (WP-D20, WP-26).
// OTEL_EXPORTER_OTLP_ENDPOINT=<server>/api/v1/otlp makes the exporter post
// to /v1/logs and /v1/metrics under it, with the person's Waypoint token in
// OTEL_EXPORTER_OTLP_HEADERS. JSON and protobuf bodies are accepted, gzip or
// not. Logs become usage rows (core.IngestOTLP); metrics are validated and
// dropped, since the same numbers arrive as events.
func (s *Server) otlpRoutes(mux *http.ServeMux) {
	a := s.authed
	mux.HandleFunc("POST /api/v1/otlp/v1/logs", a(s.otlpLogs))
	mux.HandleFunc("POST /api/v1/otlp/v1/metrics", a(s.otlpMetrics))
}

const (
	otlpMaxBody    = 8 << 20  // as sent
	otlpMaxDecoded = 64 << 20 // after gunzip
)

// handled marks a response the handler wrote itself.
type handled struct{}

type otlpBadRequest string

func (e otlpBadRequest) Error() string { return string(e) }

// otlpBody reads and gunzips a request body and reports whether it is protobuf.
func otlpBody(w http.ResponseWriter, r *http.Request) ([]byte, bool, error) {
	mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	var proto bool
	switch mt {
	case "application/json":
	case "application/x-protobuf":
		proto = true
	default:
		return nil, false, errOTLPMediaType
	}
	var rd io.Reader = http.MaxBytesReader(w, r.Body, otlpMaxBody)
	switch enc := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Encoding"))); enc {
	case "", "identity":
	case "gzip":
		zr, err := gzip.NewReader(rd)
		if err != nil {
			return nil, false, otlpBadRequest("body is not valid gzip: " + err.Error())
		}
		defer func() { _ = zr.Close() }()
		rd = zr
	default:
		return nil, false, otlpBadRequest("unsupported Content-Encoding " + strconv.Quote(enc) + "; use gzip or none")
	}
	b, err := io.ReadAll(io.LimitReader(rd, otlpMaxDecoded+1))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return nil, false, errOTLPTooLarge
		}
		return nil, false, otlpBadRequest("reading body: " + err.Error())
	}
	if len(b) > otlpMaxDecoded {
		return nil, false, errOTLPTooLarge
	}
	return b, proto, nil
}

var (
	errOTLPMediaType = errors.New("unsupported Content-Type; send application/json or application/x-protobuf (OTEL_EXPORTER_OTLP_PROTOCOL=http/json)")
	errOTLPTooLarge  = errors.New("export too large")
)

func (s *Server) otlpFail(w http.ResponseWriter, r *http.Request, err error) {
	var bad otlpBadRequest
	switch {
	case errors.Is(err, errOTLPMediaType):
		writeJSON(w, http.StatusUnsupportedMediaType, map[string]any{"code": 3, "message": err.Error()})
	case errors.Is(err, errOTLPTooLarge):
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"code": 3, "message": err.Error()})
	case errors.As(err, &bad):
		writeJSON(w, http.StatusBadRequest, map[string]any{"code": 3, "message": bad.Error()})
	default:
		var ie *core.InvalidError
		if errors.As(err, &ie) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"code": 3, "message": ie.Msg})
			return
		}
		s.writeError(w, r, err)
	}
}

func (s *Server) otlpLogs(w http.ResponseWriter, r *http.Request) (any, error) {
	body, isProto, err := otlpBody(w, r)
	if err != nil {
		s.otlpFail(w, r, err)
		return handled{}, nil
	}
	var events []core.OTLPEvent
	if isProto {
		var req logspb.ExportLogsServiceRequest
		if err := proto.Unmarshal(body, &req); err != nil {
			s.otlpFail(w, r, otlpBadRequest("invalid protobuf: "+err.Error()))
			return handled{}, nil
		}
		events = eventsFromProto(&req)
	} else {
		if events, err = eventsFromJSON(body); err != nil {
			s.otlpFail(w, r, otlpBadRequest("invalid OTLP JSON: "+err.Error()))
			return handled{}, nil
		}
	}
	res, err := s.svc.IngestOTLP(r.Context(), actorFrom(r), events)
	if err != nil {
		s.otlpFail(w, r, err)
		return handled{}, nil
	}
	otlpReply(w, isProto, "rejectedLogRecords", res.Rejected)
	return handled{}, nil
}

func (s *Server) otlpMetrics(w http.ResponseWriter, r *http.Request) (any, error) {
	body, isProto, err := otlpBody(w, r)
	if err != nil {
		s.otlpFail(w, r, err)
		return handled{}, nil
	}
	if !isProto && !json.Valid(body) {
		s.otlpFail(w, r, otlpBadRequest("invalid OTLP JSON"))
		return handled{}, nil
	}
	// Nothing in the usage tables is a metric; the same tokens, costs and
	// tool calls arrive as log events. Accepted and dropped.
	otlpReply(w, isProto, "", 0)
	return handled{}, nil
}

// otlpReply answers an export: an empty success, or a partial success naming
// the rejected count.
func otlpReply(w http.ResponseWriter, isProto bool, field string, rejected int64) {
	if isProto {
		var resp logspb.ExportLogsServiceResponse
		if rejected > 0 {
			resp.PartialSuccess = &logspb.ExportLogsPartialSuccess{RejectedLogRecords: rejected, ErrorMessage: "events without a session id, model or tool name, or with an implausible time"}
		}
		b, _ := proto.Marshal(&resp)
		w.Header().Set("Content-Type", "application/x-protobuf")
		_, _ = w.Write(b)
		return
	}
	ps := map[string]any{}
	if rejected > 0 {
		ps[field] = strconv.FormatInt(rejected, 10)
		ps["errorMessage"] = "events without a session id, model or tool name, or with an implausible time"
	}
	writeJSON(w, http.StatusOK, map[string]any{"partialSuccess": ps})
}

// ---- JSON ----

type flexNum string

func (f *flexNum) UnmarshalJSON(b []byte) error {
	*f = flexNum(strings.Trim(string(b), `"`))
	return nil
}

type otlpAnyValue struct {
	StringValue *string  `json:"stringValue"`
	IntValue    *flexNum `json:"intValue"`
	DoubleValue *float64 `json:"doubleValue"`
	BoolValue   *bool    `json:"boolValue"`
	ArrayValue  *struct {
		Values []otlpAnyValue `json:"values"`
	} `json:"arrayValue"`
}

type otlpKV struct {
	Key   string       `json:"key"`
	Value otlpAnyValue `json:"value"`
}

func (v otlpAnyValue) text() string {
	switch {
	case v.StringValue != nil:
		return *v.StringValue
	case v.IntValue != nil:
		return string(*v.IntValue)
	case v.DoubleValue != nil:
		return strconv.FormatFloat(*v.DoubleValue, 'f', -1, 64)
	case v.BoolValue != nil:
		return strconv.FormatBool(*v.BoolValue)
	case v.ArrayValue != nil:
		parts := make([]string, 0, len(v.ArrayValue.Values))
		for _, e := range v.ArrayValue.Values {
			parts = append(parts, e.text())
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

type otlpLogsJSON struct {
	ResourceLogs []struct {
		Resource struct {
			Attributes []otlpKV `json:"attributes"`
		} `json:"resource"`
		ScopeLogs []struct {
			LogRecords []struct {
				TimeUnixNano         flexNum      `json:"timeUnixNano"`
				ObservedTimeUnixNano flexNum      `json:"observedTimeUnixNano"`
				EventName            string       `json:"eventName"`
				Body                 otlpAnyValue `json:"body"`
				Attributes           []otlpKV     `json:"attributes"`
			} `json:"logRecords"`
		} `json:"scopeLogs"`
	} `json:"resourceLogs"`
}

func eventsFromJSON(b []byte) ([]core.OTLPEvent, error) {
	var req otlpLogsJSON
	if err := json.NewDecoder(bytes.NewReader(b)).Decode(&req); err != nil {
		return nil, err
	}
	var out []core.OTLPEvent
	for _, rl := range req.ResourceLogs {
		res := map[string]string{}
		for _, kv := range rl.Resource.Attributes {
			res[kv.Key] = kv.Value.text()
		}
		for _, sl := range rl.ScopeLogs {
			for _, lr := range sl.LogRecords {
				attrs := make(map[string]string, len(res)+len(lr.Attributes))
				for k, v := range res {
					attrs[k] = v
				}
				for _, kv := range lr.Attributes {
					attrs[kv.Key] = kv.Value.text()
				}
				name := lr.EventName
				if name == "" {
					name = lr.Body.text()
				}
				ns, _ := strconv.ParseInt(string(lr.TimeUnixNano), 10, 64)
				if ns == 0 {
					ns, _ = strconv.ParseInt(string(lr.ObservedTimeUnixNano), 10, 64)
				}
				out = append(out, core.OTLPEvent{At: nanosToMS(ns, attrs), Name: name, Attrs: attrs})
			}
		}
	}
	return out, nil
}

// nanosToMS is the record's time; when it has none, event.timestamp.
func nanosToMS(ns int64, attrs map[string]string) int64 {
	if ns > 0 {
		return ns / 1_000_000
	}
	if t, err := time.Parse(time.RFC3339Nano, attrs["event.timestamp"]); err == nil {
		return t.UnixMilli()
	}
	return 0
}

// ---- protobuf ----

func pbText(v *commonpb.AnyValue) string {
	switch x := v.GetValue().(type) {
	case *commonpb.AnyValue_StringValue:
		return x.StringValue
	case *commonpb.AnyValue_IntValue:
		return strconv.FormatInt(x.IntValue, 10)
	case *commonpb.AnyValue_DoubleValue:
		return strconv.FormatFloat(x.DoubleValue, 'f', -1, 64)
	case *commonpb.AnyValue_BoolValue:
		return strconv.FormatBool(x.BoolValue)
	case *commonpb.AnyValue_ArrayValue:
		parts := make([]string, 0, len(x.ArrayValue.GetValues()))
		for _, e := range x.ArrayValue.GetValues() {
			parts = append(parts, pbText(e))
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func eventsFromProto(req *logspb.ExportLogsServiceRequest) []core.OTLPEvent {
	var out []core.OTLPEvent
	for _, rl := range req.GetResourceLogs() {
		res := map[string]string{}
		for _, kv := range rl.GetResource().GetAttributes() {
			res[kv.GetKey()] = pbText(kv.GetValue())
		}
		for _, sl := range rl.GetScopeLogs() {
			for _, lr := range sl.GetLogRecords() {
				attrs := make(map[string]string, len(res)+len(lr.GetAttributes()))
				for k, v := range res {
					attrs[k] = v
				}
				for _, kv := range lr.GetAttributes() {
					attrs[kv.GetKey()] = pbText(kv.GetValue())
				}
				name := lr.GetEventName()
				if name == "" {
					name = pbText(lr.GetBody())
				}
				ns := u64ns(lr.GetTimeUnixNano())
				if ns == 0 {
					ns = u64ns(lr.GetObservedTimeUnixNano())
				}
				out = append(out, core.OTLPEvent{At: nanosToMS(ns, attrs), Name: name, Attrs: attrs})
			}
		}
	}
	return out
}

// u64ns is a protobuf timestamp as int64; one beyond year 2262 is no time.
func u64ns(n uint64) int64 {
	if n > math.MaxInt64 {
		return 0
	}
	return int64(n)
}
