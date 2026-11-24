package hermes

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// frameMsg is the parts of a frame the tests look at.
type frameMsg struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params struct {
		Type        string          `json:"type"`
		SessionID   string          `json:"session_id"`
		ID          string          `json:"id"`
		Reason      string          `json:"reason"`
		Method      string          `json:"method"`
		State       string          `json:"state"`
		Reconnected bool            `json:"reconnected"`
		Payload     json.RawMessage `json:"payload"`
	} `json:"params"`
}

func parse(t *testing.T, f Frame) frameMsg {
	t.Helper()
	var m frameMsg
	if err := json.Unmarshal(f.Msg, &m); err != nil {
		t.Fatalf("frame %d: %v", f.Seq, err)
	}
	return m
}

// next reads frames until one matches, failing after a deadline.
func next(t *testing.T, s *Sub, match func(frameMsg) bool) (Frame, frameMsg) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case f := <-s.C:
			if m := parse(t, f); match(m) {
				return f, m
			}
		case <-s.Gone():
			t.Fatal("subscriber dropped")
		case <-deadline:
			t.Fatal("no matching frame")
		}
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

type heard struct {
	method  string
	watched bool
}

func gatewaySetup(t *testing.T) (*fakeHermes, *Gateway, chan heard, chan string) {
	t.Helper()
	fh := newFakeHermes(t)
	_, ts, ca := tlsDoor(t, "k", fh)
	c := newClient(t, Config{URL: ts.URL, CAPEM: ca, Key: "k", Username: "waypoint", Password: "hunter2"})
	gw := c.Gateway()
	if c.Gateway() != gw {
		t.Fatal("Gateway() isn't one per client")
	}
	reqs := make(chan heard, 16)
	events := make(chan string, 64)
	gw.OnRequest = func(method string, _ json.RawMessage, watched bool) { reqs <- heard{method, watched} }
	gw.OnEvent = func(typ, sid string, _ json.RawMessage) { events <- typ + "@" + sid }
	return fh, gw, reqs, events
}

func waitOpen(t *testing.T, gw *Gateway) {
	t.Helper()
	waitFor(t, "the gateway to open", func() bool { s, _ := gw.State(); return s == "open" })
}

func TestGatewayCallsAndEvents(t *testing.T) {
	fh, gw, _, events := gatewaySetup(t)
	ctx := tctx(t)

	sub, hello, replay := gw.Subscribe(0)
	defer gw.Unsubscribe(sub)
	if len(replay) != 0 || hello.Reset || len(hello.Open) != 0 {
		t.Errorf("first hello = %+v, replay %d", hello, len(replay))
	}
	if !gw.Watched() {
		t.Error("not watched with a subscriber")
	}
	waitOpen(t, gw)
	next(t, sub, func(m frameMsg) bool { return m.Method == "event" && m.Params.Type == "gateway.ready" })
	_, m := next(t, sub, func(m frameMsg) bool { return m.Method == "bridge" && m.Params.State == "open" })
	if m.Params.Reconnected {
		t.Error("first connection reported as a reconnect")
	}

	res, err := gw.Call(ctx, "echo", map[string]any{"text": "hi"})
	if err != nil || string(res) != `{"text":"hi"}` {
		t.Fatalf("echo: %s %v", res, err)
	}
	_, err = gw.Call(ctx, "session.nope", nil)
	var re *RPCError
	if !errors.As(err, &re) || re.Code != -32601 || !strings.Contains(re.Message, "session.nope") {
		t.Errorf("error reply: %v", err)
	}

	fh.push(t, map[string]any{"jsonrpc": "2.0", "method": "event", "params": map[string]any{"type": "message.delta", "session_id": "s1", "payload": map[string]any{"text": "Hel"}}})
	fh.push(t, map[string]any{"jsonrpc": "2.0", "method": "event", "params": map[string]any{"type": "message.complete", "session_id": "s1", "payload": map[string]any{"text": "Hello"}}})
	f1, _ := next(t, sub, func(m frameMsg) bool { return m.Params.Type == "message.delta" })
	f2, _ := next(t, sub, func(m frameMsg) bool { return m.Params.Type == "message.complete" })
	if f2.Seq != f1.Seq+1 {
		t.Errorf("seqs %d then %d", f1.Seq, f2.Seq)
	}
	got := map[string]bool{}
	waitFor(t, "OnEvent", func() bool {
		select {
		case e := <-events:
			got[e] = true
		default:
		}
		return got["message.delta@s1"] && got["message.complete@s1"]
	})

	sub2, h2, replay2 := gw.Subscribe(f1.Seq - 1)
	defer gw.Unsubscribe(sub2)
	if h2.Reset || len(replay2) < 2 || replay2[0].Seq != f1.Seq || replay2[1].Seq != f2.Seq {
		t.Errorf("replay after %d: reset %v, %d frames", f1.Seq-1, h2.Reset, len(replay2))
	}
	if h2.State != "open" {
		t.Errorf("hello state = %q", h2.State)
	}
}

func TestGatewayServerRequests(t *testing.T) {
	fh, gw, reqs, _ := gatewaySetup(t)
	sub, _, _ := gw.Subscribe(0)
	waitOpen(t, gw)

	fh.push(t, map[string]any{"jsonrpc": "2.0", "id": "a1", "method": "approval", "params": map[string]any{"command": "rm -rf /tmp/x", "description": "delete"}})
	fa, _ := next(t, sub, func(m frameMsg) bool { return m.Method == "approval" })
	select {
	case h := <-reqs:
		if h.method != "approval" || !h.watched {
			t.Errorf("OnRequest = %+v", h)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnRequest not called")
	}
	other, hello, _ := gw.Subscribe(0)
	if len(hello.Open) != 1 || hello.Open[0].Seq != fa.Seq {
		t.Errorf("open questions in hello = %+v", hello.Open)
	}
	gw.Unsubscribe(other)

	if err := gw.Answer("a1", json.RawMessage(`{"choice":"once"}`)); err != nil {
		t.Fatal(err)
	}
	r := fh.reply(t)
	if r["id"] != "a1" || r["error"] != nil {
		t.Errorf("answer = %v", r)
	}
	if res, _ := r["result"].(map[string]any); res["choice"] != "once" {
		t.Errorf("answer result = %v", r["result"])
	}
	_, m := next(t, sub, func(m frameMsg) bool { return m.Method == "bridge.answered" })
	if m.Params.ID != "a1" {
		t.Errorf("answered frame = %+v", m.Params)
	}
	if err := gw.Answer("a1", nil); err == nil {
		t.Error("answered twice")
	}
	if err := gw.Answer("never-asked", nil); err == nil {
		t.Error("answered a question nobody asked")
	}
	if s, h, _ := gw.Subscribe(0); len(h.Open) != 0 {
		t.Errorf("answered question still open: %+v", h.Open)
	} else {
		gw.Unsubscribe(s)
	}

	for i, method := range []string{"secret", "sudo", "vault.code", "vault.save_login"} {
		id := "x" + string(rune('0'+i))
		fh.push(t, map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": map[string]any{"prompt": "API key?"}})
		r := fh.reply(t)
		e, _ := r["error"].(map[string]any)
		if r["id"] != id || e == nil || e["code"] != float64(4404) || r["result"] != nil {
			t.Errorf("%s: reply = %v", method, r)
		}
		_, m := next(t, sub, func(m frameMsg) bool { return m.Method == "bridge.declined" })
		if m.Params.Method != method {
			t.Errorf("declined frame for %s = %+v", method, m.Params)
		}
		if err := gw.Answer(id, json.RawMessage(`{"value":"hunter2"}`)); err == nil {
			t.Errorf("%s: a declined request could still be answered", method)
		}
	}
	fh.push(t, map[string]any{"jsonrpc": "2.0", "id": "u1", "method": "window.open", "params": map[string]any{}})
	if r := fh.reply(t); r["id"] != "u1" || r["error"] == nil {
		t.Errorf("unknown request reply = %v", r)
	}
	select {
	case h := <-reqs:
		t.Errorf("OnRequest called for a declined request: %+v", h)
	default:
	}

	fh.push(t, map[string]any{"jsonrpc": "2.0", "id": "a2", "method": "clarify", "params": map[string]any{"questions": []any{map[string]any{"question": "Which day?"}}}})
	next(t, sub, func(m frameMsg) bool { return m.Method == "clarify" })
	<-reqs
	fh.push(t, map[string]any{"jsonrpc": "2.0", "method": "event", "params": map[string]any{"type": "request.cancel", "payload": map[string]any{"id": "a2"}}})
	next(t, sub, func(m frameMsg) bool { return m.Params.Type == "request.cancel" })
	if err := gw.Answer("a2", nil); err == nil {
		t.Error("answered a withdrawn question")
	}

	gw.Unsubscribe(sub)
	if gw.Watched() {
		t.Fatal("still watched")
	}
	fh.push(t, map[string]any{"jsonrpc": "2.0", "id": "a3", "method": "approval", "params": map[string]any{}})
	select {
	case h := <-reqs:
		if h.watched {
			t.Error("unwatched approval reported as watched")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnRequest not called")
	}
}

// TestGatewayNumericRequestID: JSON-RPC 2.0 says a response carries the
// request's id unchanged, number or string. Hermes's server requests may
// use numbers.
func TestGatewayNumericRequestID(t *testing.T) {
	fh, gw, reqs, _ := gatewaySetup(t)
	sub, _, _ := gw.Subscribe(0)
	defer gw.Unsubscribe(sub)
	waitOpen(t, gw)
	fh.push(t, map[string]any{"jsonrpc": "2.0", "id": 7, "method": "approval", "params": map[string]any{}})
	<-reqs
	if err := gw.Answer("7", json.RawMessage(`{"choice":"deny"}`)); err != nil {
		t.Fatal(err)
	}
	if r := fh.reply(t); r["id"] != float64(7) {
		t.Errorf("reply id = %#v, want the number 7", r["id"])
	}
	fh.push(t, map[string]any{"jsonrpc": "2.0", "id": 8, "method": "secret", "params": map[string]any{}})
	if r := fh.reply(t); r["id"] != float64(8) {
		t.Errorf("decline id = %#v, want the number 8", r["id"])
	}
}

func TestGatewayDisconnect(t *testing.T) {
	fh, gw, reqs, _ := gatewaySetup(t)
	sub, _, _ := gw.Subscribe(0)
	defer gw.Unsubscribe(sub)
	waitOpen(t, gw)
	fh.push(t, map[string]any{"jsonrpc": "2.0", "id": "a1", "method": "approval", "params": map[string]any{}})
	<-reqs

	fh.mu.Lock()
	fh.conn.CloseNow()
	fh.mu.Unlock()
	_, m := next(t, sub, func(m frameMsg) bool { return m.Method == "bridge.answered" })
	if m.Params.ID != "a1" || m.Params.Reason != "disconnected" {
		t.Errorf("answered frame = %+v", m.Params)
	}
	if err := gw.Answer("a1", nil); err == nil {
		t.Error("answered a question from a dropped connection")
	}
	_, m = next(t, sub, func(m frameMsg) bool { return m.Method == "bridge" && m.Params.State == "open" })
	if !m.Params.Reconnected {
		t.Error("the second connection wasn't marked as a reconnect")
	}
	fh.mu.Lock()
	n := fh.conns
	fh.mu.Unlock()
	if n != 2 {
		t.Errorf("connections = %d", n)
	}
	res, err := gw.Call(tctx(t), "echo", []int{1})
	if err != nil || string(res) != "[1]" {
		t.Errorf("call after reconnect: %s %v", res, err)
	}
}

// bare is a gateway with no connection, for the ring's bookkeeping.
func bare() *Gateway {
	return &Gateway{calls: map[string]chan rpcReply{}, subs: map[*Sub]struct{}{}, open: map[string]Frame{}, state: "down", running: true}
}

func TestGatewayReplayAndReset(t *testing.T) {
	g := bare()
	for range 10 {
		g.publish(json.RawMessage(`{}`))
	}
	seqs := func(fs []Frame) []int64 {
		out := []int64{}
		for _, f := range fs {
			out = append(out, f.Seq)
		}
		return out
	}
	for _, c := range []struct {
		after int64
		reset bool
		want  []int64
	}{
		{0, false, []int64{}},
		{5, false, []int64{6, 7, 8, 9, 10}},
		{10, false, []int64{}},
		{11, true, []int64{}},
		{500, true, []int64{}},
	} {
		s, h, replay := g.Subscribe(c.after)
		if h.Reset != c.reset || h.Seq != 10 || len(seqs(replay)) != len(c.want) {
			t.Errorf("after %d: reset %v seq %d replay %v", c.after, h.Reset, h.Seq, seqs(replay))
		}
		for i := range c.want {
			if replay[i].Seq != c.want[i] {
				t.Errorf("after %d: replay %v, want %v", c.after, seqs(replay), c.want)
				break
			}
		}
		g.Unsubscribe(s)
	}
	for range ringSize {
		g.publish(json.RawMessage(`{}`))
	}
	if len(g.ring) != ringSize || g.ring[0].Seq != 11 {
		t.Fatalf("ring = %d frames from %d", len(g.ring), g.ring[0].Seq)
	}
	if _, h, replay := g.Subscribe(5); !h.Reset || len(replay) != 0 {
		t.Errorf("after 5 (gone): reset %v, %d frames", h.Reset, len(replay))
	}
	if _, h, replay := g.Subscribe(10); h.Reset || len(replay) != ringSize {
		t.Errorf("after 10 (the ring's start): reset %v, %d frames", h.Reset, len(replay))
	}
}

func TestGatewaySlowSubscriber(t *testing.T) {
	g := bare()
	slow, _, _ := g.Subscribe(0)
	fast, _, _ := g.Subscribe(0)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-fast.C:
			case <-fast.Gone():
				return
			}
		}
	}()
	for range cap(slow.C) + 1 {
		g.publish(json.RawMessage(`{}`))
	}
	select {
	case <-slow.Gone():
	default:
		t.Fatal("a subscriber that fell behind wasn't dropped")
	}
	select {
	case <-fast.Gone():
		t.Fatal("a subscriber that kept up was dropped")
	default:
	}
	g.Unsubscribe(slow)
	g.Unsubscribe(fast)
	<-done
	if g.Watched() {
		t.Error("still watched")
	}
	s, _, _ := g.Subscribe(0)
	g.close()
	select {
	case <-s.Gone():
	default:
		t.Error("close left a subscriber open")
	}
}

func TestGatewayCallWhileDown(t *testing.T) {
	g := bare()
	g.state = "open"
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := g.Call(ctx, "ping", nil); !errors.Is(err, ErrUnreachable) {
		t.Errorf("call without a connection: %v", err)
	}
	g.state = "down"
	ctx2, cancel2 := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel2()
	if _, err := g.Call(ctx2, "ping", nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("call while connecting: %v", err)
	}
}
