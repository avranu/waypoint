package core

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"testing"
)

// llmSchemas is every JSON schema the code sends to a model. A new one goes
// here; llm.go sends all of them with strict: true.
func llmSchemas(t *testing.T) map[string]map[string]any {
	t.Helper()
	var audit map[string]any
	if err := json.Unmarshal([]byte(auditSchema), &audit); err != nil {
		t.Fatal(err)
	}
	return map[string]map[string]any{
		"estimate": estimateSchema, "breakdown": breakdownSchema, "title": titleSchema,
		"prep": prepSchema, "prepPick": prepPickSchema, "prep_evidence": prepEvidenceSchema, "task_safety": screenSchema,
		"review": reviewSchema, "verify_new": verifyNewSchema, "verify": verifySchema,
		"reply": replySchema, "review_count": reviewCountSchema, "goal_suggest": goalSuggestSchema,
		"automation_text": textSchema, "audit": audit,
		"pr_body": prBodySchema, "task_goal": taskGoalSchema, "page_story": storySchema,
		"goal_drift": driftSchema, "goal_drift_week": driftWeekSchema,
		"goal_meaning": meaningSchema, "goal_meaning_construct": meaningConstructSchema,
		"held_edit": weakeningSchema,
	}
}

// strictProblems lists how a schema breaks OpenAI-style strict mode: every
// object lists all its properties in required and sets additionalProperties
// false.
func strictProblems(schema any, at string) []string {
	b, _ := json.Marshal(schema) // Go-typed values to plain JSON
	var v any
	_ = json.Unmarshal(b, &v)
	var out []string
	var walk func(n any, at string)
	walk = func(n any, at string) {
		m, ok := n.(map[string]any)
		if !ok {
			return
		}
		isObj := m["type"] == "object"
		if ts, ok := m["type"].([]any); ok {
			isObj = slices.Contains(ts, any("object"))
		}
		props, _ := m["properties"].(map[string]any)
		if isObj || props != nil {
			if m["additionalProperties"] != false {
				out = append(out, at+": additionalProperties is not false")
			}
			req, _ := m["required"].([]any)
			var names []string
			for k := range props {
				names = append(names, k)
				if !slices.Contains(req, any(k)) {
					out = append(out, fmt.Sprintf("%s: property %q is not in required", at, k))
				}
			}
			for _, r := range req {
				if _, ok := props[r.(string)]; !ok {
					out = append(out, fmt.Sprintf("%s: required %q is not a property", at, r))
				}
			}
			sort.Strings(names)
			for _, k := range names {
				walk(props[k], at+"."+k)
			}
		}
		if items, ok := m["items"]; ok {
			walk(items, at+"[]")
		}
		for _, k := range []string{"anyOf", "oneOf", "allOf"} {
			if alts, ok := m[k].([]any); ok {
				for i, a := range alts {
					walk(a, fmt.Sprintf("%s.%s[%d]", at, k, i))
				}
			}
		}
	}
	walk(v, at)
	return out
}

func TestEverySchemaSentToModelsIsStrictValid(t *testing.T) {
	for name, sch := range llmSchemas(t) {
		for _, p := range strictProblems(sch, name) {
			t.Error(p)
		}
	}
}

func TestStrictProblemsCatchesMistakes(t *testing.T) {
	bad := map[string]any{"type": "object", "properties": map[string]any{
		"a": map[string]any{"type": "string"},
		"l": map[string]any{"type": "array", "items": map[string]any{"type": "object",
			"properties": map[string]any{"x": map[string]any{"type": "string"}}, "required": []string{}}},
	}, "required": []string{"l"}}
	if got := strictProblems(bad, "bad"); len(got) != 4 {
		t.Errorf("want 4 problems, got %v", got)
	}
}

func TestOptionalNullableAnswerMayBeLeftOut(t *testing.T) {
	if err := checkAnswer(prepPickSchema, []byte(`{"path":"a.go","why":"w"}`)); err != nil {
		t.Error(err)
	}
	if err := checkAnswer(prepPickSchema, []byte(`{"path":"a.go","name":null,"why":"w"}`)); err != nil {
		t.Error(err)
	}
	if err := checkAnswer(prepPickSchema, []byte(`{"name":"f","why":"w"}`)); err == nil {
		t.Error("path is still required")
	}
}

// Every gateway request caps the answer, so a runaway stream ends at the cap.
func TestGatewayRequestsSendMaxTokens(t *testing.T) {
	var got []float64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		n, _ := body["max_tokens"].(float64)
		got = append(got, n)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"model":"m","choices":[{"message":{"content":"{\"points\": 3, \"confidence\": \"high\", \"reason\": \"ok\"}"}}]}`))
	}))
	defer srv.Close()
	c := gatewayClient()
	p := &AIProvider{Kind: "openai", BaseURL: srv.URL + "/v1", apiKey: "k"}
	if _, _, err := c.Complete(context.Background(), p, "free", "s", "p", estimateSchema); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Complete(withMaxTokens(context.Background(), verifyMaxTokens), p, "free", "s", "p", estimateSchema); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != defaultMaxTokens || got[1] != verifyMaxTokens {
		t.Errorf("max_tokens sent = %v", got)
	}
}
