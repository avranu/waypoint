package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"git.jmann.me/claude/waypoint/internal/core"
)

// The transcript refusals carry a machine-readable code beside their words,
// so the client does not depend on the text (audit 1236.7).
func TestTranscriptRefusalsCarryCodes(t *testing.T) {
	s := &Server{done: make(chan struct{})}
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{core.ErrTranscriptQuota, 507, "transcript_full"},
		{core.ErrTranscriptDiskLow, 507, "transcript_full"},
		{core.ErrTranscriptJob, http.StatusForbidden, "transcript_job"},
		{core.ErrForbidden, http.StatusForbidden, ""},
	} {
		w := httptest.NewRecorder()
		s.writeError(w, httptest.NewRequest("POST", "/api/v1/usage/transcript", nil), tc.err)
		var body map[string]string
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		if w.Code != tc.status || body["code"] != tc.code || body["error"] == "" {
			t.Errorf("%v: %d %v, want %d code %q", tc.err, w.Code, body, tc.status, tc.code)
		}
	}
}
