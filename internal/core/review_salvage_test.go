package core

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A reviewer's answer cut off at the token cap keeps the findings it
// finished (WP-1421, 1248.8).
func TestSalvageFindings(t *testing.T) {
	f1 := `{"file":"a.go","claim":"A \"quoted\" } brace {"}`
	f2 := `{"file":"b.go","claim":"two","lines":[1,2],"extra":{"k":["x","}"]}}`
	cases := []struct {
		name string
		in   string
		want int
	}{
		{"cut in the third element", `{"findings":[` + f1 + `,` + f2 + `,{"file":"c.go","cl`, 2},
		{"cut between elements", `{"findings":[` + f1 + `,` + f2 + `,`, 2},
		{"cut right after an element", `{"findings":[` + f1 + f2[:0], 1},
		{"cut inside a string with escapes", `{"findings":[` + f1 + `,{"file":"b.go","claim":"x \"y {`, 1},
		{"cut after a backslash", `{"findings":[` + f1 + `,{"claim":"x\`, 1},
		{"cut inside nested array", `{"findings":[` + f1 + `,{"file":"b.go","lines":[1,2`, 1},
		{"cut inside nested object", `{"findings":[` + f1 + `,{"file":"b.go","extra":{"k":[`, 1},
		{"prose before", "Here you go:\n" + `{"findings":[` + f1 + `,{"fi`, 1},
		{"code fence before", "```json\n" + `{"findings":[` + f1 + "," + f2 + `,{"fi`, 2},
		{"prose with a brace first", "a {note} then " + `{"findings":[` + f1 + `,{"fi`, 1},
		{"cut in the first element", `{"findings":[{"file":"a.go","cla`, 0},
		{"cut before the array", `{"findings":`, 0},
		{"empty array", `{"findings":[`, 0},
		{"another key", `{"items":[` + f1 + `,{"fi`, 0},
		{"findings object nested in another (found by the later start)", `{"x":{"findings":[` + f1 + `,{"fi`, 1},
		{"no object", `no json here`, 0},
		{"mismatched brackets", `{"findings":[` + f1 + `]],{"fi`, 0},
		{"closed array, cut in a later field", `{"findings":[` + f1 + `,` + f2 + `],"summary":"x`, 2},
		{"closed array, cut right after it", `{"findings":[` + f1 + `,` + f2 + `]`, 2},
		{"closed array, nested values after it", `{"findings":[` + f1 + `],"meta":{"a":{"b":[1]}},"s":"x`, 1},
		{"closed empty array, cut after it", `{"findings":[],"summary":"x`, 0},
		{"array elements", `{"findings":[["a"],["b"`, 0},
		{"an array among the objects", `{"findings":[` + f1 + `,["a"],{"fi`, 0},
		{"a scalar in the cut tail", `{"findings":[` + f1 + `,"x",{"fi`, 1},
		{"a string between objects", `{"findings":[` + f1 + `,"x",` + f2 + `,{"fi`, 0},
		{"a number among the objects", `{"findings":[` + f1 + `,3,` + f2 + `,{"fi`, 0},
	}
	for _, c := range cases {
		raw, n, ok := salvageFindings(c.in)
		if (c.want > 0) != ok || n != c.want {
			t.Errorf("%s: ok=%v n=%d, want %d (%s)", c.name, ok, n, c.want, raw)
			continue
		}
		if !ok {
			continue
		}
		var out struct {
			Findings []json.RawMessage `json:"findings"`
		}
		if err := json.Unmarshal(raw, &out); err != nil || len(out.Findings) != c.want {
			t.Errorf("%s: salvaged %s: %v", c.name, raw, err)
		}
	}
	whole := `{"findings":[` + f1 + `]}`
	if _, _, ok := salvageFindings(whole); ok {
		t.Error("a complete answer was salvaged")
	}
}

func cutAnswerServer(t *testing.T, content string) *AIProvider {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": content}}}})
		w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return &AIProvider{Kind: "openai", BaseURL: srv.URL + "/v1", apiKey: "k"}
}

// Only a call that asked for salvage gets it, and it is told how much.
func TestCompleteSalvagesOnlyWhenAsked(t *testing.T) {
	cut := `{"findings":[{"file":"a.go","claim":"one"},{"file":"b.go","claim":"tw`
	p := cutAnswerServer(t, cut)
	c := &httpLLM{http: EgressPolicy{AllowLoopback: true}.client(10 * time.Second)}
	if _, _, err := c.Complete(context.Background(), p, "m", "sys", "prompt", reviewSchema); err == nil {
		t.Fatal("a cut answer passed without salvage")
	}
	ctx, note := withSalvage(context.Background())
	raw, _, err := c.Complete(ctx, p, "m", "sys", "prompt", reviewSchema)
	if err != nil || !strings.Contains(string(raw), `"one"`) || strings.Contains(string(raw), `"tw`) {
		t.Fatalf("raw %s err %v", raw, err)
	}
	if note.answers != 1 || note.findings != 1 {
		t.Errorf("note %+v", note)
	}
	p = cutAnswerServer(t, `{"findings":[{"file":"a.go","cla`)
	ctx, note = withSalvage(context.Background())
	if _, _, err := c.Complete(ctx, p, "m", "sys", "prompt", reviewSchema); err == nil || note.answers != 0 {
		t.Errorf("err %v note %+v", err, note)
	}
}

func TestReviewBodyNotesSalvagedAnswers(t *testing.T) {
	one := reviewBody("abc1234", reviewOutcome{salvagedAnswers: 1, salvagedFindings: 3})
	if !strings.Contains(one, "a reviewer's answer was cut off at its length limit; kept the 3 findings it had finished") {
		t.Errorf("one:\n%s", one)
	}
	two := reviewBody("abc1234", reviewOutcome{salvagedAnswers: 2, salvagedFindings: 1})
	if !strings.Contains(two, "2 reviewers' answers were cut off at their length limit; kept the 1 finding they had finished") {
		t.Errorf("two:\n%s", two)
	}
	if none := reviewBody("abc1234", reviewOutcome{}); strings.Contains(none, "cut off") {
		t.Errorf("none:\n%s", none)
	}
}

// The note is the last answer's: a whole answer after a salvaged one clears it.
func TestSalvageNoteIsTheLastAnswers(t *testing.T) {
	ctx, note := withSalvage(context.Background())
	if _, err := extractAnswerJSON(ctx, `{"findings":[{"claim":"one"},{"cl`); err != nil || note.findings != 1 {
		t.Fatalf("err %v note %+v", err, note)
	}
	if _, err := extractAnswerJSON(ctx, `{"findings":[]}`); err != nil || note.answers != 0 || note.findings != 0 {
		t.Errorf("err %v, a whole answer left the note %+v", err, note)
	}
}

// Each kind of provider salvages a cut answer when asked (1255.16).
func TestEachProviderKindSalvages(t *testing.T) {
	cut := `{"findings":[{"file":"a.go","claim":"one"},{"file":"b.go","claim":"tw`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b []byte
		switch {
		case r.URL.Path == "/api/chat":
			b, _ = json.Marshal(map[string]any{"message": map[string]any{"content": cut}})
		case r.URL.Path == "/session":
			b, _ = json.Marshal(map[string]any{"id": "s1", "permission": []any{map[string]string{"permission": "*", "pattern": "*", "action": "ask"}}})
		case strings.HasSuffix(r.URL.Path, "/message"):
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			if in["format"] != nil {
				b, _ = json.Marshal(map[string]any{"info": map[string]any{"error": map[string]any{"name": "StructuredOutputError"}}})
				break
			}
			b, _ = json.Marshal(map[string]any{"parts": []any{map[string]any{"type": "text", "text": cut}}})
		default:
			b = []byte(`{}`)
		}
		w.Write(b)
	}))
	t.Cleanup(srv.Close)
	c := &httpLLM{http: EgressPolicy{AllowLoopback: true}.client(10 * time.Second)}
	for _, kind := range []string{"ollama", "opencode"} {
		p := &AIProvider{Kind: kind, BaseURL: srv.URL, apiKey: "k"}
		ctx, note := withSalvage(context.Background())
		raw, _, err := c.Complete(ctx, p, "prov/m", "sys", "prompt", reviewSchema)
		if err != nil || !strings.Contains(string(raw), `"one"`) || strings.Contains(string(raw), `"tw`) || note.findings != 1 {
			t.Errorf("%s: raw %s err %v note %+v", kind, raw, err, note)
		}
	}
}
