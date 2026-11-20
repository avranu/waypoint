package core

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"sync"
	"testing"
)

// jevSeq is a test's decision model that answers its nth ask with
// answer(n), counting from 0.
type jevSeq struct {
	mu     sync.Mutex
	n      int
	answer func(n int) (map[string]jevAnswer, error)
}

func (j *jevSeq) Decide(ctx context.Context, p *AIProvider, model, state string, qs map[string]jevQuestion) (map[string]jevAnswer, string, error) {
	j.mu.Lock()
	n := j.n
	j.n++
	j.mu.Unlock()
	a, err := j.answer(n)
	return a, "jev-1.14.0", err
}

type seqScoringLLM struct {
	*scriptLLM
	*jevSeq
}

func probs(kv ...any) jevAnswer {
	a := jevAnswer{Type: "choice", Probabilities: map[string]float64{}}
	for i := 0; i < len(kv); i += 2 {
		a.Probabilities[kv[i].(string)] = kv[i+1].(float64)
	}
	return a
}

// meanAnswers averages each question's probabilities over the answers
// that gave it; a bare choice counts at its confidence.
func TestMeanAnswers(t *testing.T) {
	one := map[string]jevAnswer{"f1": probs("serious", 0.9)}
	if got := meanAnswers([]map[string]jevAnswer{one}); got["f1"].Probabilities["serious"] != 0.9 {
		t.Errorf("one answer = %+v", got)
	}
	got := meanAnswers([]map[string]jevAnswer{
		{"f1": probs("serious", 0.6, "moderate", 0.4), "r1": {Type: "choice", Choice: "e1", Confidence: 0.8}},
		{"f1": probs("serious", 0.2, "moderate", 0.8), "r1": probs("new", 0.7, "e1", 0.3), "fixes": probs("complete", 1.0)},
	})
	near := func(a, b float64) bool { return math.Abs(a-b) < 1e-9 }
	if f := got["f1"]; !near(f.Probabilities["serious"], 0.4) || !near(f.Probabilities["moderate"], 0.6) || f.Choice != "moderate" || !near(f.Confidence, 0.6) {
		t.Errorf("f1 = %+v", f)
	}
	if r := got["r1"]; !near(r.Probabilities["e1"], 0.55) || !near(r.Probabilities["new"], 0.35) || r.Choice != "e1" {
		t.Errorf("r1 = %+v", r)
	}
	// Only one answer gave fixes: its mean is that answer's.
	if f := got["fixes"]; !near(f.Probabilities["complete"], 1) {
		t.Errorf("fixes = %+v", f)
	}
}

// samplesSplit names the questions the answers call differently, a
// repeat question by againRepeatOf (so a sub-0.6 lean to an earlier
// finding is "new").
func TestSamplesSplit(t *testing.T) {
	same := map[string]jevAnswer{"f1": probs("serious", 0.7, "moderate", 0.3), "r1": probs("new", 0.5, "e1", 0.5)}
	lean := map[string]jevAnswer{"f1": probs("serious", 0.6, "moderate", 0.4), "r1": probs("new", 0.45, "e1", 0.55)}
	if got := samplesSplit([]map[string]jevAnswer{same, lean}, 1); len(got) != 0 {
		t.Errorf("agreeing answers split: %v", got)
	}
	sure := map[string]jevAnswer{"f1": probs("serious", 0.4, "moderate", 0.6), "r1": probs("new", 0.2, "e1", 0.8)}
	got := samplesSplit([]map[string]jevAnswer{same, lean, sure}, 1)
	if len(got) != 2 || len(got["f1"]) != 3 || got["f1"][2] != "moderate" || got["r1"][0] != "new" || got["r1"][2] != "e1" {
		t.Errorf("split = %v", got)
	}
}

// decideAgain asks againSamplesMin times; while the decision isn't
// settled (an ask alone deciding differently at first, then the mean's
// decision changing with the asks added) it asks againSamplesStep more, to
// againSamplesMax; a call that splits without changing the decision asks
// no more but is recorded; failed asks are left out and asked again while
// others answer, and only when none answers is it an error.
func TestDecideAgain(t *testing.T) {
	be := newBabysitEnv(t)
	withScoring(t, be.s, be.llm, "cosmetic")
	qs := map[string]jevQuestion{"f1": {Type: "choice"}, "f2": {Type: "choice"}}
	// The decision here turns on f1 alone; f2 is a call that decides nothing.
	outcome := func(a map[string]jevAnswer) string { return sampleCall("f1", a["f1"], 0) }
	ask := func(answer func(n int) (map[string]jevAnswer, error)) (*againDecision, map[string]jevAnswer, int, error) {
		j := &jevSeq{answer: answer}
		be.s.LLM = &seqScoringLLM{scriptLLM: be.llm, jevSeq: j}
		d := &againDecision{}
		ans, _, err := be.s.decideAgain(context.Background(), d, "state", qs, 0, outcome)
		return d, ans, j.n, err
	}
	serious := map[string]jevAnswer{"f1": probs("serious", 0.9)}
	moderate := map[string]jevAnswer{"f1": probs("moderate", 0.9)}

	// Agreeing: the minimum, nothing split.
	d, ans, n, err := ask(func(int) (map[string]jevAnswer, error) { return serious, nil })
	if err != nil || n != againSamplesMin || d.samples != againSamplesMin || d.spread != "" || ans["f1"].Choice != "serious" {
		t.Errorf("agreeing: %d asks, %+v, %v", n, d, err)
	}
	// One ask in three disagrees: two more asks, and the mean decides as
	// it did, so it stops there, the split recorded, settled.
	d, ans, n, err = ask(func(n int) (map[string]jevAnswer, error) {
		if n%3 == 2 {
			return moderate, nil
		}
		return serious, nil
	})
	var spread map[string][]string
	_ = json.Unmarshal([]byte(d.spread), &spread)
	want := againSamplesMin + againSamplesStep
	if err != nil || n != want || d.samples != want || len(spread["f1"]) != want || len(spread[spreadOutcome]) != want || spread[spreadUnsettled] != nil || ans["f1"].Choice != "serious" {
		t.Errorf("split: %d asks, %+v, %v", n, d, err)
	}
	// The mean's decision flips with every step: asked to the most, and
	// recorded unsettled, each step's decision in order.
	d, _, n, err = ask(func(n int) (map[string]jevAnswer, error) {
		if slices.Contains([]int{2, 3, 4, 7, 8}, n) {
			return moderate, nil
		}
		return serious, nil
	})
	spread = nil
	_ = json.Unmarshal([]byte(d.spread), &spread)
	if err != nil || n != againSamplesMax || d.samples != againSamplesMax || !slices.Equal(spread[spreadUnsettled], []string{"serious", "moderate", "serious", "moderate"}) {
		t.Errorf("unsettled: %d asks, %+v, %v", n, d, err)
	}
	// A call that splits but decides nothing: the minimum, the split
	// recorded, no outcome in it.
	d, _, n, err = ask(func(n int) (map[string]jevAnswer, error) {
		a := map[string]jevAnswer{"f1": probs("serious", 0.9), "f2": probs("minor", 0.9)}
		if n == 1 {
			a["f2"] = probs("moderate", 0.9)
		}
		return a, nil
	})
	spread = nil
	_ = json.Unmarshal([]byte(d.spread), &spread)
	if err != nil || n != againSamplesMin || len(spread["f2"]) != againSamplesMin || spread[spreadOutcome] != nil {
		t.Errorf("a split that decides nothing: %d asks, %+v, %v", n, d, err)
	}
	// An ask that fails is left out and asked again: the minimum is of
	// answers.
	d, _, n, err = ask(func(n int) (map[string]jevAnswer, error) {
		if n == 0 {
			return nil, errors.New("timeout")
		}
		return serious, nil
	})
	if err != nil || n != againSamplesMin+1 || d.samples != againSamplesMin {
		t.Errorf("one failed: %d asks, %+v, %v", n, d, err)
	}
	// A failed first ask doesn't change which rule judges the first step:
	// the same three answers as "split" (serious, serious, moderate) are
	// judged together, unsettled, and two more are asked.
	d, _, n, err = ask(func(n int) (map[string]jevAnswer, error) {
		switch n {
		case 0:
			return nil, errors.New("timeout")
		case 3:
			return moderate, nil
		}
		return serious, nil
	})
	if err != nil || n != againSamplesMin+1+againSamplesStep || d.samples != againSamplesMin+againSamplesStep {
		t.Errorf("a failure before a split: %d asks, %+v, %v", n, d, err)
	}
	// A step whose asks all fail adds nothing: an unsettled first step
	// stays unsettled, not settled by judging the same answers twice.
	d, _, n, err = ask(func(n int) (map[string]jevAnswer, error) {
		switch {
		case n == 2:
			return moderate, nil
		case n > 2:
			return nil, errors.New("timeout")
		}
		return serious, nil
	})
	spread = nil
	_ = json.Unmarshal([]byte(d.spread), &spread)
	if err != nil || n != againSamplesMax || d.samples != againSamplesMin || !slices.Equal(spread[spreadUnsettled], []string{"serious"}) {
		t.Errorf("later asks all failed: %d asks, %+v, %v", n, d, err)
	}
	// Asks that keep failing while one answered: asked again to the most,
	// no further, and the one answer decides.
	d, ans, n, err = ask(func(n int) (map[string]jevAnswer, error) {
		if n != 0 {
			return nil, errors.New("timeout")
		}
		return serious, nil
	})
	if err != nil || n != againSamplesMax || d.samples != 1 || ans["f1"].Probabilities["serious"] != 0.9 {
		t.Errorf("most failed: %d asks, %+v, %v", n, d, err)
	}
	// None answers: the first error, nothing recorded.
	d, _, n, err = ask(func(int) (map[string]jevAnswer, error) { return nil, errNoDecider })
	if !errors.Is(err, errNoDecider) || n != againSamplesMin || d.samples != 0 {
		t.Errorf("none: %d asks, %+v, %v", n, d, err)
	}
}
