package hermes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Gateway is Waypoint's one connection to Hermes's chat gateway (/api/ws,
// JSON-RPC 2.0), held by the server rather than the browser: a turn keeps
// streaming when the tab closes, every tab sees the same events, a question
// the agent asks can reach its owner as a notification, and the ticket and
// cookies never leave the server. Each frame Hermes sends gets a sequence
// number and goes to every subscriber; a ring of recent frames lets one
// that reconnects catch up.
type Gateway struct {
	c *Client

	// OnRequest hears every question the agent asks (approval, clarify)
	// and whether anyone was watching when it arrived.
	OnRequest func(method string, params json.RawMessage, watched bool)
	// OnEvent hears every event, for what the server records itself.
	OnEvent func(typ, sessionID string, payload json.RawMessage)

	mu      sync.Mutex
	conn    *websocket.Conn
	running bool
	state   string // connecting, open, down
	lastErr string
	ids     int64
	calls   map[string]chan rpcReply
	subs    map[*Sub]struct{}
	ring    []Frame
	seq     int64
	open    map[string]Frame           // the agent's questions not yet answered, by request id
	rawIDs  map[string]json.RawMessage // their ids as Hermes sent them (string or number)
	used    time.Time
	quiet   map[string]bool // conversations whose replies notify nobody
	stop    context.CancelFunc
	conns   int // successful connections, to tell a reconnect apart
}

// Frame is one message from Hermes (or the bridge itself), numbered.
type Frame struct {
	Seq int64           `json:"seq"`
	Msg json.RawMessage `json:"msg"`
}

// Sub is one subscriber: a browser tab's live stream, or Waypoint itself
// following a conversation it started (quiet: nobody is watching).
type Sub struct {
	C     chan Frame
	gone  chan struct{}
	quiet bool
}

// Gone is closed when the subscriber fell too far behind and must
// reconnect (and catch up from the ring).
func (s *Sub) Gone() <-chan struct{} { return s.gone }

type rpcReply struct {
	Result json.RawMessage `json:"result"`
	Error  *RPCError       `json:"error"`
}

// RPCError is a JSON-RPC error from Hermes.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string { return e.Message }

const (
	ringSize      = 4000
	idleAfter     = 15 * time.Minute // close a connection nobody uses
	heartbeat     = 15 * time.Second
	silentTooLong = 45 * time.Second
)

// Gateway is the client's chat gateway, created on first use.
func (c *Client) Gateway() *Gateway {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gw == nil {
		c.gw = &Gateway{c: c, calls: map[string]chan rpcReply{}, subs: map[*Sub]struct{}{}, open: map[string]Frame{}, state: "down"}
	}
	return c.gw
}

// Close ends the gateway's connection for good (the assistant's settings
// changed, or it was removed).
func (c *Client) Close() {
	c.mu.Lock()
	gw := c.gw
	c.mu.Unlock()
	if gw != nil {
		gw.close()
	}
}

func (g *Gateway) close() {
	g.mu.Lock()
	if g.stop != nil {
		g.stop()
	}
	for s := range g.subs {
		delete(g.subs, s)
		close(s.gone)
	}
	g.mu.Unlock()
}

// Watched reports whether any tab is subscribed.
func (g *Gateway) Watched() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.watchedLocked()
}

func (g *Gateway) watchedLocked() bool {
	for s := range g.subs {
		if !s.quiet {
			return true
		}
	}
	return false
}

// Quiet marks a conversation Waypoint drives itself (an automation's), so
// its replies don't notify anyone.
func (g *Gateway) Quiet(sessionID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.quiet == nil {
		g.quiet = map[string]bool{}
	}
	g.quiet[sessionID] = true
}

// IsQuiet reports whether Quiet marked the conversation.
func (g *Gateway) IsQuiet(sessionID string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.quiet[sessionID]
}

// State is the connection's state and its last error.
func (g *Gateway) State() (string, string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.state, g.lastErr
}

// ensure starts the connection loop if it isn't running.
func (g *Gateway) ensure() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.used = time.Now()
	if g.running {
		return
	}
	g.running = true
	ctx, cancel := context.WithCancel(context.Background())
	g.stop = cancel
	go g.loop(ctx)
}

// wanted: someone is watching, or the connection was used lately (a turn
// may still be streaming for a tab that closed).
func (g *Gateway) wanted() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.subs) > 0 || time.Since(g.used) < idleAfter
}

func (g *Gateway) loop(ctx context.Context) {
	defer func() {
		g.mu.Lock()
		g.running = false
		g.mu.Unlock()
		g.setState("down", "")
	}()
	backoff := time.Second
	for ctx.Err() == nil && g.wanted() {
		g.setState("connecting", "")
		start := time.Now()
		err := g.session(ctx)
		if ctx.Err() != nil {
			return
		}
		msg := "the connection closed"
		if err != nil {
			msg = err.Error()
		}
		g.setState("down", msg)
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

// session is one connection, from dialling to its end.
func (g *Gateway) session(ctx context.Context) error {
	dctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	ticket, err := g.c.ticket(dctx)
	if err != nil {
		return err
	}
	u := *g.c.base
	u.Scheme = map[string]string{"https": "wss", "http": "ws"}[u.Scheme]
	u.Path = "/api/ws"
	h := http.Header{}
	h.Set(g.c.cfg.keyHeader(), g.c.cfg.Key)
	conn, _, err := websocket.Dial(dctx, u.String(), &websocket.DialOptions{
		HTTPClient:   g.c.long,
		HTTPHeader:   h,
		Subprotocols: []string{"hermes-gateway-v1", "hermes-gateway-ticket." + ticket},
	})
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	conn.SetReadLimit(256 << 20)
	defer func() { _ = conn.CloseNow() }()

	g.mu.Lock()
	g.conn = conn
	g.conns++
	reconnected := g.conns > 1
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		g.conn = nil
		for id, ch := range g.calls {
			ch <- rpcReply{Error: &RPCError{Code: -32000, Message: "the connection to the assistant dropped"}}
			delete(g.calls, id)
		}
		// Questions asked on this connection can't be answered on the next.
		for id := range g.open {
			delete(g.open, id)
			delete(g.rawIDs, id)
			g.publishLocked(bridgeMsg("bridge.answered", map[string]any{"id": id, "reason": "disconnected"}))
		}
		g.mu.Unlock()
	}()

	sctx, scancel := context.WithCancel(ctx)
	defer scancel()
	last := make(chan struct{}, 1)
	go g.pinger(sctx, conn, last)
	go func() {
		// Without this Hermes withdraws approvals and clarify questions.
		if _, err := g.Call(sctx, "client.capabilities", map[string]any{"server_requests": true}); err != nil {
			_ = conn.Close(websocket.StatusGoingAway, "capabilities refused")
			return
		}
		g.setStateReconnect("open", "", reconnected)
	}()
	for {
		_, data, err := conn.Read(sctx)
		if err != nil {
			if sctx.Err() != nil {
				return sctx.Err()
			}
			var ce websocket.CloseError
			if errors.As(err, &ce) {
				switch ce.Code {
				case 4401:
					return errors.New("hermes refused the chat login")
				case 4403:
					return errors.New("hermes refused the chat connection (chat disabled, or Host/Origin)")
				}
			}
			return err
		}
		select {
		case last <- struct{}{}:
		default:
		}
		g.handle(data)
	}
}

func (g *Gateway) pinger(ctx context.Context, conn *websocket.Conn, last <-chan struct{}) {
	t := time.NewTicker(heartbeat)
	defer t.Stop()
	heard := time.Now()
	n := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-last:
			heard = time.Now()
		case <-t.C:
			if time.Since(heard) > silentTooLong {
				_ = conn.Close(websocket.StatusGoingAway, "no heartbeat")
				return
			}
			if !g.wanted() {
				_ = conn.Close(websocket.StatusNormalClosure, "idle")
				return
			}
			n++
			b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": "heartbeat-" + strconv.Itoa(n), "method": "gateway.ping", "params": map[string]any{}})
			_ = conn.Write(ctx, websocket.MessageText, b)
		}
	}
}

// handle routes one frame: a reply to our call, a question for the owner,
// or an event for subscribers.
func (g *Gateway) handle(data []byte) {
	var m struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
		Result json.RawMessage `json:"result"`
		Error  *RPCError       `json:"error"`
	}
	if json.Unmarshal(data, &m) != nil {
		return
	}
	id := strings.Trim(string(m.ID), `"`)
	switch {
	case m.Method == "" && len(m.ID) > 0: // a reply
		if strings.HasPrefix(id, "heartbeat-") {
			return
		}
		g.mu.Lock()
		ch, ok := g.calls[id]
		delete(g.calls, id)
		g.mu.Unlock()
		if ok {
			ch <- rpcReply{Result: m.Result, Error: m.Error}
		}
	case m.Method == "event":
		var ev struct {
			Type      string          `json:"type"`
			SessionID string          `json:"session_id"`
			Payload   json.RawMessage `json:"payload"`
		}
		_ = json.Unmarshal(m.Params, &ev)
		if ev.Type == "request.cancel" {
			var p struct {
				ID string `json:"id"`
			}
			_ = json.Unmarshal(ev.Payload, &p)
			g.mu.Lock()
			delete(g.open, p.ID)
			delete(g.rawIDs, p.ID)
			g.mu.Unlock()
		}
		g.publish(data)
		if g.OnEvent != nil {
			g.OnEvent(ev.Type, ev.SessionID, ev.Payload)
		}
	case len(m.ID) > 0: // a question from the agent
		g.request(id, m.ID, m.Method, m.Params, data)
	}
}

// request handles a server→client request. The owner answers approvals
// and clarify questions; Waypoint declines the rest itself: passwords,
// secrets and logins never pass through it (the gate holds credentials),
// and the desktop app's window bridges don't exist here.
func (g *Gateway) request(id string, raw json.RawMessage, method string, params, data json.RawMessage) {
	switch method {
	case "approval", "clarify":
		g.mu.Lock()
		if g.rawIDs == nil {
			g.rawIDs = map[string]json.RawMessage{}
		}
		g.rawIDs[id] = raw
		watched := g.watchedLocked()
		f := g.publishLocked(data)
		g.open[id] = f
		g.mu.Unlock()
		if g.OnRequest != nil {
			g.OnRequest(method, params, watched)
		}
	case "sudo", "secret", "vault.code", "vault.save_login", "vault.unlock_prompt", "display.install.sudo":
		_ = g.reply(raw, nil, &RPCError{Code: 4404, Message: "not shown: Waypoint doesn't pass passwords or secrets to the agent"})
		g.publish(bridgeMsg("bridge.declined", map[string]any{"method": method, "params": params}))
	default:
		_ = g.reply(raw, nil, &RPCError{Code: 4404, Message: "not shown"})
	}
}

func bridgeMsg(method string, params any) json.RawMessage {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
	return b
}

// setState records the connection's state and tells subscribers.
func (g *Gateway) setState(state, errMsg string) { g.setStateReconnect(state, errMsg, false) }

func (g *Gateway) setStateReconnect(state, errMsg string, reconnected bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.state == state && errMsg == "" && !reconnected {
		return
	}
	g.state = state
	switch {
	case state == "open":
		g.lastErr = ""
	case errMsg != "":
		g.lastErr = errMsg
	}
	g.publishLocked(bridgeMsg("bridge", map[string]any{"state": state, "error": g.lastErr, "reconnected": reconnected}))
}

func (g *Gateway) publish(msg json.RawMessage) {
	g.mu.Lock()
	g.publishLocked(msg)
	g.mu.Unlock()
}

func (g *Gateway) publishLocked(msg json.RawMessage) Frame {
	g.seq++
	f := Frame{Seq: g.seq, Msg: msg}
	g.ring = append(g.ring, f)
	if len(g.ring) > ringSize {
		g.ring = append(g.ring[:0:0], g.ring[len(g.ring)-ringSize:]...)
	}
	for s := range g.subs {
		select {
		case s.C <- f:
		default:
			// Too far behind: drop it; it reconnects and catches up.
			delete(g.subs, s)
			close(s.gone)
		}
	}
	return f
}

// Hello is what a new subscriber learns first.
type Hello struct {
	State string  `json:"state"`
	Error string  `json:"error,omitempty"`
	Seq   int64   `json:"seq"`
	Open  []Frame `json:"open"`  // the agent's unanswered questions
	Reset bool    `json:"reset"` // frames after the asked-for seq are gone: reload
}

// Subscribe starts a live stream of frames after seq (0: only new ones),
// connecting to Hermes if need be. Call Unsubscribe when done.
func (g *Gateway) Subscribe(after int64) (*Sub, Hello, []Frame) {
	return g.subscribe(after, false)
}

// Listen is Subscribe for Waypoint itself: it doesn't count as someone
// watching, so the owner is still told what happens.
func (g *Gateway) Listen() *Sub {
	s, _, _ := g.subscribe(0, true)
	return s
}

func (g *Gateway) subscribe(after int64, quiet bool) (*Sub, Hello, []Frame) {
	g.ensure()
	g.mu.Lock()
	defer g.mu.Unlock()
	s := &Sub{C: make(chan Frame, 4096), gone: make(chan struct{}), quiet: quiet}
	g.subs[s] = struct{}{}
	h := Hello{State: g.state, Error: g.lastErr, Seq: g.seq, Open: make([]Frame, 0, len(g.open))}
	for _, f := range g.open {
		h.Open = append(h.Open, f)
	}
	var replay []Frame
	if after > 0 && after < g.seq {
		if len(g.ring) == 0 || g.ring[0].Seq > after+1 {
			h.Reset = true
		} else {
			for _, f := range g.ring {
				if f.Seq > after {
					replay = append(replay, f)
				}
			}
		}
	} else if after > g.seq {
		h.Reset = true // the server restarted since
	}
	return s, h, replay
}

// Unsubscribe ends a stream.
func (g *Gateway) Unsubscribe(s *Sub) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.subs[s]; ok {
		delete(g.subs, s)
		close(s.gone)
	}
	g.used = time.Now()
}

// Call sends one request and waits for its reply, connecting first if
// need be.
func (g *Gateway) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	g.ensure()
	if method != "client.capabilities" {
		if err := g.waitOpen(ctx); err != nil {
			return nil, err
		}
	}
	g.mu.Lock()
	conn := g.conn
	if conn == nil {
		g.mu.Unlock()
		return nil, fmt.Errorf("%w: the chat connection is down", ErrUnreachable)
	}
	g.ids++
	id := "w" + strconv.FormatInt(g.ids, 10)
	ch := make(chan rpcReply, 1)
	g.calls[id] = ch
	g.mu.Unlock()
	if params == nil {
		params = map[string]any{}
	}
	b, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		return nil, err
	}
	if err := conn.Write(ctx, websocket.MessageText, b); err != nil {
		g.mu.Lock()
		delete(g.calls, id)
		g.mu.Unlock()
		return nil, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	select {
	case r := <-ch:
		if r.Error != nil {
			return nil, r.Error
		}
		return r.Result, nil
	case <-ctx.Done():
		g.mu.Lock()
		delete(g.calls, id)
		g.mu.Unlock()
		return nil, ctx.Err()
	}
}

// waitOpen waits (up to the context, at most 20 seconds) for the
// connection to be ready.
func (g *Gateway) waitOpen(ctx context.Context) error {
	deadline := time.Now().Add(20 * time.Second)
	for {
		g.mu.Lock()
		state, errMsg := g.state, g.lastErr
		g.mu.Unlock()
		if state == "open" {
			return nil
		}
		if time.Now().After(deadline) {
			if errMsg == "" {
				errMsg = "still connecting"
			}
			return fmt.Errorf("%w: %s", ErrUnreachable, errMsg)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// Answer replies to one of the agent's questions, once.
func (g *Gateway) Answer(id string, result json.RawMessage) error {
	g.mu.Lock()
	_, ok := g.open[id]
	raw := g.rawIDs[id]
	delete(g.open, id)
	delete(g.rawIDs, id)
	g.mu.Unlock()
	if !ok {
		return fmt.Errorf("that question was already answered or withdrawn")
	}
	if err := g.reply(raw, result, nil); err != nil {
		return err
	}
	g.publish(bridgeMsg("bridge.answered", map[string]any{"id": id}))
	return nil
}

func (g *Gateway) reply(id json.RawMessage, result json.RawMessage, rpcErr *RPCError) error {
	g.mu.Lock()
	conn := g.conn
	g.mu.Unlock()
	if conn == nil {
		return fmt.Errorf("%w: the chat connection is down", ErrUnreachable)
	}
	msg := map[string]any{"jsonrpc": "2.0", "id": id}
	if rpcErr != nil {
		msg["error"] = rpcErr
	} else {
		if len(result) == 0 {
			result = json.RawMessage("{}")
		}
		msg["result"] = result
	}
	b, _ := json.Marshal(msg)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return conn.Write(ctx, websocket.MessageText, b)
}
