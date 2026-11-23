package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

// Test slot holds (WP-1646): wp sends the holds it recorded and reads them
// back for `wp slot replay`.
func (s *Server) slotHoldRoutes(mux *http.ServeMux) {
	a := s.authed
	mux.HandleFunc("POST /api/v1/slot-holds", a(s.slotHoldsPost))
	mux.HandleFunc("GET /api/v1/slot-holds", a(s.slotHoldsList))
}

func (s *Server) slotHoldsPost(w http.ResponseWriter, r *http.Request) (any, error) {
	q := r.URL.Query()
	in := core.SlotHoldsIn{Machine: q.Get("machine")}
	for name, dst := range map[string]*int64{"pending": &in.Pending, "oldest": &in.Oldest, "dropped": &in.Dropped} {
		if q.Get(name) == "" {
			continue
		}
		n, err := strconv.ParseInt(q.Get(name), 10, 64)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("%w: %s is a count", errBadRequest, name)
		}
		*dst = n
	}
	// Fields this server doesn't know are let through (a wp newer than the
	// server must not have every batch refused), and a body that can't be
	// read still records what the machine says is waiting, so it shows in
	// the collector health instead of failing unseen. A hold that does not
	// decode (a field of the wrong type) becomes an empty one, which is
	// counted as rejected, so one bad hold does not refuse the batch.
	var body struct {
		Holds []json.RawMessage `json:"holds"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&body); err != nil {
		if _, rerr := s.svc.RecordSlotHolds(r.Context(), actorFrom(r), in); rerr != nil {
			return nil, rerr
		}
		return nil, fmt.Errorf("%w: the body is not {\"holds\":[...]}: %v", errBadRequest, err)
	}
	in.Holds = make([]core.SlotHold, len(body.Holds))
	for i, raw := range body.Holds {
		if err := json.Unmarshal(raw, &in.Holds[i]); err != nil {
			in.Holds[i] = core.SlotHold{}
		}
	}
	return s.svc.RecordSlotHolds(r.Context(), actorFrom(r), in)
}

func (s *Server) slotHoldsList(w http.ResponseWriter, r *http.Request) (any, error) {
	v := r.URL.Query()
	f := core.SlotHoldsFilter{Machine: v.Get("machine"), User: v.Get("user"), Limit: queryInt(r, "limit", 0)}
	for name, dst := range map[string]*time.Time{"since": &f.Since, "until": &f.Until} {
		if v.Get(name) == "" {
			continue
		}
		ms, err := strconv.ParseInt(v.Get(name), 10, 64)
		if err != nil || ms < 0 {
			return nil, fmt.Errorf("%w: %s is unix milliseconds", errBadRequest, name)
		}
		*dst = time.UnixMilli(ms)
	}
	return s.svc.SlotHolds(r.Context(), actorFrom(r), f)
}
