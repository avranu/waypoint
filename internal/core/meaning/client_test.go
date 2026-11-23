package meaning

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeService answers /v1/embeddings and /v1/nli, deriving each answer from
// its input so a test can check the order, and records the request sizes.
type fakeService struct {
	mu    sync.Mutex
	paths []string
	sizes []int
}

func (f *fakeService) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Input []string  `json:"input"`
			Pairs []NLIPair `json:"pairs"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode: %v", err)
		}
		f.mu.Lock()
		f.paths = append(f.paths, r.URL.Path)
		f.mu.Unlock()
		switch r.URL.Path {
		case "/v1/embeddings":
			f.record(len(body.Input))
			data := make([]map[string]any, len(body.Input))
			for i, s := range body.Input {
				n, _ := strconv.Atoi(s)
				data[i] = map[string]any{"index": i, "embedding": []float32{float32(n)}}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"model": "e5@1", "data": data})
		case "/v1/nli":
			f.record(len(body.Pairs))
			scores := make([]map[string]any, len(body.Pairs))
			for i, p := range body.Pairs {
				n, _ := strconv.Atoi(p.Premise)
				scores[i] = map[string]any{"entailment": float64(n) / 1000}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"model": "nli@1", "scores": scores})
		default:
			http.NotFound(w, r)
		}
	})
}

func (f *fakeService) record(n int) {
	f.mu.Lock()
	f.sizes = append(f.sizes, n)
	f.mu.Unlock()
}

func (f *fakeService) check(t *testing.T, path string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sizes) != 2 || f.sizes[0] != Batch || f.sizes[1] != 3 {
		t.Fatalf("request sizes %v, want [%d 3]", f.sizes, Batch)
	}
	for _, p := range f.paths {
		if p != path || strings.Contains(p, "//") {
			t.Fatalf("request path %q, want %q", p, path)
		}
	}
}

func TestEmbedBatchesAndKeepsOrder(t *testing.T) {
	f := &fakeService{}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	m := NewHTTP(srv.URL+"/", "", srv.Client(), nil, nil)
	texts := make([]string, Batch+3)
	for i := range texts {
		texts[i] = strconv.Itoa(i)
	}
	model, vecs, err := m.Embed(context.Background(), texts)
	if err != nil {
		t.Fatal(err)
	}
	if model != "e5@1" || len(vecs) != len(texts) {
		t.Fatalf("%q, %d vectors", model, len(vecs))
	}
	for i, v := range vecs {
		if len(v) != 1 || int(v[0]) != i {
			t.Fatalf("vector %d is %v", i, v)
		}
	}
	f.check(t, "/v1/embeddings")
}

func TestEntailBatchesAndKeepsOrder(t *testing.T) {
	f := &fakeService{}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	m := NewHTTP(srv.URL+"/", "", srv.Client(), nil, nil)
	pairs := make([]NLIPair, Batch+3)
	for i := range pairs {
		pairs[i] = NLIPair{Premise: strconv.Itoa(i), Hypothesis: "h"}
	}
	model, scores, err := m.Entail(context.Background(), pairs)
	if err != nil {
		t.Fatal(err)
	}
	if model != "nli@1" || len(scores) != len(pairs) {
		t.Fatalf("%q, %d scores", model, len(scores))
	}
	for i, s := range scores {
		if s != float64(i)/1000 {
			t.Fatalf("score %d is %v", i, s)
		}
	}
	f.check(t, "/v1/nli")
}

type bareModels struct{ Models }

func TestHealthOf(t *testing.T) {
	h, ok := HealthOf(NewHTTP("http://x", "", nil, nil, nil))
	if !ok || h.State != "unasked" {
		t.Fatalf("%+v %v", h, ok)
	}
	if _, ok := HealthOf(bareModels{}); ok {
		t.Fatal("a Models without Health should give ok=false")
	}
	if _, ok := HealthOf(nil); ok {
		t.Fatal("nil should give ok=false")
	}
}
