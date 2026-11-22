package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// sent records the requests a wrapped server saw, and answers some itself.
type sent struct {
	mu    sync.Mutex
	posts []string // "POST /path body"
	stub  func(w http.ResponseWriter, r *http.Request) bool
}

func (s *sent) wrap(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body []byte
		if r.Body != nil {
			body, _ = io.ReadAll(r.Body)
			r.Body = io.NopCloser(strings.NewReader(string(body)))
		}
		if r.Method != http.MethodGet {
			s.mu.Lock()
			s.posts = append(s.posts, r.Method+" "+r.URL.Path+" "+string(body))
			s.mu.Unlock()
		}
		if s.stub != nil && s.stub(w, r) {
			return
		}
		h.ServeHTTP(w, r)
	})
}

func (s *sent) to(suffix string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, p := range s.posts {
		if strings.Contains(strings.SplitN(p, " ", 3)[1], suffix) {
			out = append(out, strings.SplitN(p, " ", 3)[2])
		}
	}
	return out
}

// wp objective diagnose sends the text, whether to ask a person, and who
// wrote it, from -m or from a file (WP-1449, audit 1214.38).
func TestObjectiveDiagnoseSendsWhatWasGiven(t *testing.T) {
	rec := &sent{stub: func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/objective/diagnose") {
			_, _ = io.WriteString(w, "diagnosis recorded\n")
			return true
		}
		return false
	}}
	e, _, key := objectiveEnvServing(t, rec.wrap)
	write(t, "d.md", "# Why\n\nThe check measures the wrong file.\n")
	for _, args := range [][]string{
		{"objective", "diagnose", key, "-m", "the check is wrong"},
		{"objective", "diagnose", key, "-m", "the target is out of reach", "--ask"},
		{"objective", "diagnose", key, "-f", "d.md"},
	} {
		if out := e.wp(args...); !strings.Contains(out, "diagnosis recorded") {
			t.Errorf("%v printed %q", args, out)
		}
	}
	type body struct {
		Text  string `json:"text"`
		Ask   bool   `json:"ask"`
		Agent string `json:"agent"`
	}
	var got []body
	for _, raw := range rec.to("/objective/diagnose") {
		var b body
		if err := json.Unmarshal([]byte(raw), &b); err != nil {
			t.Fatalf("%q: %v", raw, err)
		}
		got = append(got, b)
	}
	if len(got) != 3 {
		t.Fatalf("the server saw %d diagnoses, want 3: %+v", len(got), got)
	}
	if got[0].Text != "the check is wrong" || got[0].Ask {
		t.Errorf("-m: %+v", got[0])
	}
	if got[1].Text != "the target is out of reach" || !got[1].Ask {
		t.Errorf("-m --ask: %+v", got[1])
	}
	if got[2].Text != "# Why\n\nThe check measures the wrong file.\n" || got[2].Ask {
		t.Errorf("-f: %+v", got[2])
	}
	for _, raw := range rec.to("/objective/diagnose") {
		if !strings.Contains(raw, `"agent"`) {
			t.Errorf("the body names no agent field: %s", raw)
		}
	}
}

// wp ship finds out that the task's round is a diagnose round before it
// commits or pushes anything (WP-1449, audit 1214.5c).
func TestShipRefusesADiagnoseRoundBeforeItPushes(t *testing.T) {
	rec := &sent{}
	var key string
	rec.stub = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodGet && key != "" && r.URL.Path == "/api/v1/issues/"+key+"/objective" {
			_, _ = io.WriteString(w, `{"key":"`+key+`","check":"cat n.txt","state":"active","round":3,"round_kind":"diagnose"}`)
			return true
		}
		return false
	}
	e, _, k := objectiveEnvServing(t, rec.wrap)
	key = k
	head := gitDo(t, "rev-parse", "HEAD")
	write(t, "n.txt", "39\n")
	gitDo(t, "add", "-A")
	out, code := e.run("ship", key, "-m", "a fix")
	if code == 0 {
		t.Fatalf("shipped a diagnose round:\n%s", out)
	}
	for _, want := range []string{"diagnose round", "wp objective diagnose " + key} {
		if !strings.Contains(out, want) {
			t.Errorf("the refusal lacks %q:\n%s", want, out)
		}
	}
	if now := gitDo(t, "rev-parse", "HEAD"); now != head {
		t.Errorf("wp ship committed (HEAD %s, was %s)", now, head)
	}
	if staged := gitDo(t, "diff", "--cached", "--name-only"); staged != "n.txt" {
		t.Errorf("the staged change was touched: %q", staged)
	}
	if posts := rec.to("/pulls"); len(posts) != 0 {
		t.Errorf("a pull request was opened: %v", posts)
	}
}
