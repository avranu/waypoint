package api

import (
	"strings"
	"testing"
)

func TestEnvironmentMCPTools(t *testing.T) {
	x := newEnvAPI(t)
	out := callMCPTool(t, x.ci.base, x.ci.token, "record_deployment", map[string]any{"project": x.key, "environment": "staging", "commit": "3fa9c1e5b7d2", "branch": "main", "status": "succeeded"})
	if !strings.Contains(out, "succeeded deployment of 3fa9c1e to staging") {
		t.Errorf("record: %s", out)
	}
	out = callMCPTool(t, x.viewer.base, x.viewer.token, "list_environments", map[string]any{"project": x.key})
	if !strings.Contains(out, x.key+"/staging (staging) unknown") || !strings.Contains(out, "runs main@3fa9c1e") {
		t.Errorf("list: %s", out)
	}
}
