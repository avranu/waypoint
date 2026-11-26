package kernel

import (
	"context"
	"slices"
	"strings"
	"testing"
)

func recorder(log *[]string, tag string) Handler {
	return func(_ context.Context, _ *Actor, e *Event) { *log = append(*log, tag+":"+e.Kind) }
}

func TestBusStagesRunInCreationOrderEventsOuter(t *testing.T) {
	k := New(nil, nil)
	var log []string
	a := k.Stage("a", SeesEvery)
	b := k.Stage("b", SeesEvery)
	a.On(recorder(&log, "a1"))
	a.On(recorder(&log, "a2"))
	b.On(recorder(&log, "b1"))
	k.Publish(context.Background(), nil, []*Event{{Kind: "x"}, {Kind: "y"}})
	want := "a1:x a2:x a1:y a2:y b1:x b1:y"
	if got := strings.Join(log, " "); got != want {
		t.Fatalf("got %s want %s", got, want)
	}
}

func TestBusDuplicateStagePanics(t *testing.T) {
	k := New(nil, nil)
	k.Stage("a", SeesEvery)
	defer func() {
		if recover() == nil {
			t.Fatal("no panic")
		}
	}()
	k.Stage("a", SeesVisible)
}

func TestBusSeesFilters(t *testing.T) {
	k := New(nil, nil)
	var priv, every, vis []string
	k.Stage("p", SeesPrivate).On(recorder(&priv, "p"))
	k.Stage("e", SeesEvery).On(recorder(&every, "e"))
	k.Stage("v", SeesVisible).On(recorder(&vis, "v"))
	k.Publish(context.Background(), nil, []*Event{
		{Kind: "plain"}, {Kind: "private", Private: true}, {Kind: "merged", Merged: true},
		{Kind: "quiet", Quiet: true}, {Kind: "draft", Draft: true},
	})
	if want := []string{"p:private"}; !slices.Equal(priv, want) {
		t.Errorf("private: %v", priv)
	}
	if want := []string{"e:plain", "e:merged", "e:quiet", "e:draft"}; !slices.Equal(every, want) {
		t.Errorf("every: %v", every)
	}
	if want := []string{"v:plain"}; !slices.Equal(vis, want) {
		t.Errorf("visible: %v", vis)
	}
}

func TestBusOnKindWithNoKindsPanics(t *testing.T) {
	st := New(nil, nil).Stage("s", SeesEvery)
	defer func() {
		if recover() == nil {
			t.Fatal("no panic")
		}
	}()
	st.OnKind(func(context.Context, *Actor, *Event) {})
}

func TestBusOnKind(t *testing.T) {
	k := New(nil, nil)
	var log []string
	st := k.Stage("s", SeesEvery)
	st.OnKind(recorder(&log, "ab"), "a", "b")
	st.On(recorder(&log, "all"))
	k.Publish(context.Background(), nil, []*Event{{Kind: "a"}, {Kind: "c"}})
	if want := "ab:a all:a all:c"; strings.Join(log, " ") != want {
		t.Fatalf("got %v", log)
	}
}

func TestBusBatchGetsVisibleAndIsSkippedWhenNone(t *testing.T) {
	k := New(nil, nil)
	var calls [][]string
	var order []string
	k.AfterStages(func(_ context.Context, _ *Actor, evs []*Event) {
		order = append(order, "one")
		var kinds []string
		for _, e := range evs {
			kinds = append(kinds, e.Kind)
		}
		calls = append(calls, kinds)
	})
	k.AfterStages(func(context.Context, *Actor, []*Event) { order = append(order, "two") })
	ctx := context.Background()
	k.Publish(ctx, nil, []*Event{{Kind: "m", Merged: true}, {Kind: "ok"}, {Kind: "p", Private: true}, {Kind: "d", Draft: true}})
	if len(calls) != 1 || !slices.Equal(calls[0], []string{"ok"}) || !slices.Equal(order, []string{"one", "two"}) {
		t.Fatalf("calls %v order %v", calls, order)
	}
	k.Publish(ctx, nil, []*Event{{Kind: "m", Merged: true}, {Kind: "q", Quiet: true}, {Kind: "p", Private: true}})
	if len(calls) != 1 {
		t.Fatalf("batch ran with no visible events: %v", calls)
	}
}

func TestBusVisibleChosenAfterEarlierStages(t *testing.T) {
	k := New(nil, nil)
	var log []string
	k.Stage("e", SeesEvery).On(func(_ context.Context, _ *Actor, e *Event) { e.Quiet = true })
	k.Stage("v", SeesVisible).On(recorder(&log, "v"))
	k.Publish(context.Background(), nil, []*Event{{Kind: "x"}})
	if len(log) != 0 {
		t.Fatalf("got %v", log)
	}
}
