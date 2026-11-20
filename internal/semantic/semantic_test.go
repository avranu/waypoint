package semantic

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// twoTopics is a corpus where "king" and "queen" never share a document
// but share every context, as do "gcc" and "clang".
func twoTopics() []map[string]int {
	royal := []string{"crown", "palace", "throne", "castle", "court"}
	code := []string{"compiler", "parser", "syntax", "linker", "build"}
	var docs []map[string]int
	for _, topic := range []struct {
		ctx   []string
		names [2]string
	}{{royal, [2]string{"king", "queen"}}, {code, [2]string{"gcc", "clang"}}} {
		for c := 0; c < 10; c++ {
			for _, name := range topic.names {
				d := map[string]int{name: 1}
				for k := 0; k < 3; k++ {
					d[topic.ctx[(c+k)%len(topic.ctx)]]++
				}
				docs = append(docs, d)
			}
		}
	}
	return docs
}

func TestTrainRelatesWordsByContext(t *testing.T) {
	m, err := Train(context.Background(), twoTopics(), Options{K: 2})
	if err != nil || m == nil {
		t.Fatalf("train = %v, %v", m, err)
	}
	if m.Docs() != 40 || m.Terms() != 14 || m.Dims() != 2 {
		t.Errorf("docs %d terms %d dims %d", m.Docs(), m.Terms(), m.Dims())
	}
	if c := m.TermCos("king", "queen"); c < 0.8 {
		t.Errorf("king~queen = %.3f, want near", c)
	}
	if c := m.TermCos("king", "compiler"); c > 0.3 {
		t.Errorf("king~compiler = %.3f, want far", c)
	}
	king, queen, gcc := m.Project(map[string]int{"king": 1}), m.Project(map[string]int{"queen": 2}), m.Project(map[string]int{"gcc": 1})
	if c := Cos(king, queen); c < 0.8 {
		t.Errorf("text king~queen = %.3f", c)
	}
	if c := Cos(king, gcc); c > 0.3 {
		t.Errorf("text king~gcc = %.3f", c)
	}
	if v := m.Project(map[string]int{"unheard": 3}); v != nil {
		t.Errorf("unknown words project to %v, want nil", v)
	}
	if m.TermCos("king", "unheard") != 0 || Cos(nil, king) != 0 {
		t.Error("unknown terms and nil vectors should compare as 0")
	}
}

func TestTrainIsRepeatable(t *testing.T) {
	a, _ := Train(context.Background(), twoTopics(), Options{K: 2})
	b, _ := Train(context.Background(), twoTopics(), Options{K: 2})
	q := map[string]int{"king": 1, "palace": 2, "syntax": 1}
	if !reflect.DeepEqual(a.Project(q), b.Project(q)) {
		t.Error("the same corpus trained two models that place text differently")
	}
}

func TestTrainEdges(t *testing.T) {
	if m, err := Train(context.Background(), []map[string]int{{"a": 1}, {"b": 1}}, Options{}); m != nil || err != nil {
		t.Errorf("nothing shared = %v, %v; want no model", m, err)
	}
	var none *Model
	if none.Project(map[string]int{"king": 1}) != nil || none.TermCos("a", "b") != 0 {
		t.Error("a nil model should place nothing")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Train(ctx, twoTopics(), Options{K: 2}); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled train = %v", err)
	}
}
