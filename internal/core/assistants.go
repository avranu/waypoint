package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"git.jmann.me/claude/waypoint/internal/hermes"
)

// EvAssistant is the live event for an assistant's state (owner only).
const EvAssistant = "assistant.status"

// NotifyAssistant is the notification kind for an assistant needing you.
const NotifyAssistant = "assistant"

// Assistant is one connected agent, without its secrets.
type Assistant struct {
	ID       int64   `json:"id"`
	Name     string  `json:"name"`
	Owner    UserRef `json:"owner"`
	URL      string  `json:"url"`
	HasCA    bool    `json:"has_ca"`
	Username string  `json:"username"`
	// KeyHeader and AgentURL are as set on this assistant; empty uses the
	// site's, in Site.
	KeyHeader string            `json:"key_header"`
	AgentURL  string            `json:"agent_url"`
	Site      AssistantDefaults `json:"site"`
	TokenID   *int64            `json:"token_id"`
	Token     string            `json:"token,omitempty"` // that token's name
	Agent     string            `json:"agent,omitempty"` // the agent identity its token acts as
	State     AssistantState    `json:"state"`
	Mine      bool              `json:"mine"`
	CreatedAt time.Time         `json:"created_at"`
	UpdatedAt time.Time         `json:"updated_at"`

	ownerID int64
}

// AssistantDefaults is a site's settings for every assistant's gate that
// doesn't set its own (WAYPOINT_ASSISTANT_KEY_HEADER and _AGENT_URL).
type AssistantDefaults struct {
	// KeyHeader is the header the gate's door reads its key from
	// (hermes.DefaultKeyHeader when the site sets none).
	KeyHeader string `json:"key_header"`
	// AgentURL is the gate as its agent reaches it, from inside the agent's
	// network: where it serves connections as MCP servers
	// ({AgentURL}/conn/NAME/mcp). Empty when unknown; then the browser can't
	// add a connection to the agent for its owner.
	AgentURL string `json:"agent_url"`
}

// siteAssistant is the site's defaults, the key header filled in.
func (s *Service) siteAssistant() AssistantDefaults {
	d := s.cfg.Assistant
	d.KeyHeader = strings.TrimSpace(d.KeyHeader)
	if d.KeyHeader == "" {
		d.KeyHeader = hermes.DefaultKeyHeader
	}
	d.AgentURL = strings.TrimRight(strings.TrimSpace(d.AgentURL), "/")
	return d
}

// AssistantState is what the watcher saw last.
type AssistantState struct {
	Reachable bool              `json:"reachable"`
	Frozen    bool              `json:"frozen"`
	Signal    bool              `json:"signal"`
	Pending   []hermes.Approval `json:"pending"`
	Error     string            `json:"error,omitempty"`
	CheckedAt time.Time         `json:"checked_at"`
	// DownSince is when the door stopped answering, for one notice per outage.
	DownSince *time.Time `json:"down_since,omitempty"`
}

// AssistantInput creates or changes an assistant; nil fields keep their
// value, and empty secrets on an update keep the stored ones.
type AssistantInput struct {
	Name     *string `json:"name"`
	OwnerID  *int64  `json:"owner_id"`
	URL      *string `json:"url"`
	CAPEM    *string `json:"ca_pem"`
	Key      *string `json:"key"`
	Username *string `json:"username"`
	Password *string `json:"password"`
	TokenID  *int64  `json:"token_id"` // 0 clears it
	// KeyHeader and AgentURL: empty uses the site's.
	KeyHeader *string `json:"key_header"`
	AgentURL  *string `json:"agent_url"`
}

type assistantRow struct {
	Assistant
	cfg hermes.Config
	upd int64
}

// assistantClients caches one client per assistant, rebuilt when its
// settings change (the cache key is updated_at), so the Hermes login and
// its cookies are shared by every request.
type assistantClients struct {
	mu sync.Mutex
	m  map[int64]*cachedClient
}

type cachedClient struct {
	upd int64
	c   *hermes.Client
}

func (s *Service) assistantClient(r *assistantRow) (*hermes.Client, error) {
	s.assistants.mu.Lock()
	defer s.assistants.mu.Unlock()
	if s.assistants.m == nil {
		s.assistants.m = map[int64]*cachedClient{}
	}
	if c, ok := s.assistants.m[r.ID]; ok && c.upd == r.upd {
		return c.c, nil
	}
	cfg := r.cfg
	if cfg.KeyHeader == "" {
		cfg.KeyHeader = s.siteAssistant().KeyHeader
	}
	c, err := hermes.New(cfg)
	if err != nil {
		return nil, invalid("%v", err)
	}
	if old, ok := s.assistants.m[r.ID]; ok {
		old.c.Close()
	}
	s.assistantHooks(r, c.Gateway())
	s.assistants.m[r.ID] = &cachedClient{upd: r.upd, c: c}
	return c, nil
}

const assistantCols = `a.id, a.owner_id, a.name, a.url, a.ca_pem, a.door_key, a.key_header, a.agent_url, a.username, a.password, a.token_id,
	COALESCE(t.name, ''), a.state, a.created_at, a.updated_at`

func (s *Service) scanAssistant(ctx context.Context, sc interface{ Scan(...any) error }) (*assistantRow, error) {
	var r assistantRow
	var tok sql.NullInt64
	var state string
	var created, updated int64
	if err := sc.Scan(&r.ID, &r.ownerID, &r.Name, &r.cfg.URL, &r.cfg.CAPEM, &r.cfg.Key, &r.cfg.KeyHeader, &r.AgentURL, &r.cfg.Username, &r.cfg.Password,
		&tok, &r.Token, &state, &created, &updated); err != nil {
		return nil, err
	}
	r.URL, r.Username, r.HasCA, r.upd = r.cfg.URL, r.cfg.Username, strings.TrimSpace(r.cfg.CAPEM) != "", updated
	r.KeyHeader, r.Site = r.cfg.KeyHeader, s.siteAssistant()
	if tok.Valid {
		r.TokenID = &tok.Int64
	}
	_ = json.Unmarshal([]byte(state), &r.State)
	if r.State.Pending == nil {
		r.State.Pending = []hermes.Approval{}
	}
	r.CreatedAt, r.UpdatedAt = time.UnixMilli(created).UTC(), time.UnixMilli(updated).UTC()
	if u, err := s.userByID(ctx, s.db.R, r.ownerID); err == nil {
		r.Owner = u.Ref()
	}
	return &r, nil
}

func (s *Service) assistantRows(ctx context.Context, where string, args ...any) ([]*assistantRow, error) {
	// #nosec G202 -- where is a fixed condition from this file; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT `+assistantCols+` FROM assistants a
		LEFT JOIN api_tokens t ON t.id = a.token_id `+where+` ORDER BY a.name COLLATE NOCASE, a.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*assistantRow
	for rows.Next() {
		r, err := s.scanAssistant(ctx, rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Service) assistantRow(ctx context.Context, id int64) (*assistantRow, error) {
	rs, err := s.assistantRows(ctx, `WHERE a.id = ?`, id)
	if err != nil {
		return nil, err
	}
	if len(rs) == 0 {
		return nil, ErrNotFound
	}
	return rs[0], nil
}

// person is a signed-in person in a browser: not a token, an automation or
// an MCP client.
func (a *Actor) person() bool {
	return a != nil && a.User != nil && a.User.ID != 0 && a.TokenID == 0 && a.Automation == nil && !a.MCP && a.Agent == ""
}

func (s *Service) finishAssistant(ctx context.Context, actor *Actor, r *assistantRow) *Assistant {
	a := r.Assistant
	a.Mine = actor.id() == r.ownerID
	if a.TokenID != nil {
		var agent sql.NullString
		_ = s.db.R.QueryRowContext(ctx, `SELECT agent FROM api_tokens WHERE id = ?`, *a.TokenID).Scan(&agent)
		a.Agent = agent.String
	}
	return &a
}

// ListAssistants is the assistants the actor owns; admins see every one,
// to set them up.
func (s *Service) ListAssistants(ctx context.Context, actor *Actor) ([]*Assistant, error) {
	if !actor.person() {
		return []*Assistant{}, nil
	}
	where, args := `WHERE a.owner_id = ?`, []any{actor.id()}
	if actor.User.IsAdmin {
		where, args = ``, nil
	}
	rows, err := s.assistantRows(ctx, where, args...)
	if err != nil {
		return nil, err
	}
	out := make([]*Assistant, 0, len(rows))
	for _, r := range rows {
		out = append(out, s.finishAssistant(ctx, actor, r))
	}
	return out, nil
}

// GetAssistant is one assistant for its owner or an admin.
func (s *Service) GetAssistant(ctx context.Context, actor *Actor, id int64) (*Assistant, error) {
	r, err := s.assistantRow(ctx, id)
	if err != nil {
		return nil, err
	}
	if !actor.person() || (actor.id() != r.ownerID && !actor.User.IsAdmin) {
		return nil, ErrNotFound
	}
	return s.finishAssistant(ctx, actor, r), nil
}

// UseAssistant is the client for an assistant its owner is using. Anyone
// else, and every token, gets not found.
func (s *Service) UseAssistant(ctx context.Context, actor *Actor, id int64) (*hermes.Client, *Assistant, error) {
	r, err := s.assistantRow(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	if !actor.person() || actor.id() != r.ownerID {
		return nil, nil, ErrNotFound
	}
	c, err := s.assistantClient(r)
	if err != nil {
		return nil, nil, err
	}
	return c, s.finishAssistant(ctx, actor, r), nil
}

func (in *AssistantInput) apply(r *assistantRow, creating bool) error {
	set := func(dst *string, v *string) {
		if v != nil {
			*dst = strings.TrimSpace(*v)
		}
	}
	set(&r.Name, in.Name)
	set(&r.cfg.URL, in.URL)
	set(&r.cfg.Username, in.Username)
	set(&r.cfg.KeyHeader, in.KeyHeader)
	set(&r.AgentURL, in.AgentURL)
	r.AgentURL = strings.TrimRight(r.AgentURL, "/")
	if in.CAPEM != nil {
		r.cfg.CAPEM = strings.TrimSpace(*in.CAPEM)
	}
	if in.Key != nil && (creating || strings.TrimSpace(*in.Key) != "") {
		r.cfg.Key = strings.TrimSpace(*in.Key)
	}
	if in.Password != nil && (creating || *in.Password != "") {
		r.cfg.Password = *in.Password
	}
	r.cfg.URL = strings.TrimRight(r.cfg.URL, "/")
	if r.Name == "" || len(r.Name) > 60 {
		return invalid("give the assistant a name of at most 60 characters")
	}
	if r.cfg.Key == "" {
		return invalid("the door's key is required")
	}
	if r.cfg.Username == "" {
		r.cfg.Username = "waypoint"
	}
	if r.cfg.Password == "" {
		return invalid("Hermes's password is required")
	}
	if _, err := hermes.New(r.cfg); err != nil {
		return invalid("%v", err)
	}
	if r.AgentURL != "" {
		if u, err := url.Parse(r.AgentURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
			u.RawQuery != "" || u.Fragment != "" || u.User != nil {
			return invalid("the gate's address for the agent must be http(s)://host[:port][/path]")
		}
	}
	r.KeyHeader = r.cfg.KeyHeader
	return nil
}

func (s *Service) checkAssistantToken(ctx context.Context, ownerID int64, tokenID *int64) (any, error) {
	if tokenID == nil || *tokenID == 0 {
		return nil, nil
	}
	var owner int64
	if err := s.db.R.QueryRowContext(ctx, `SELECT user_id FROM api_tokens WHERE id = ?`, *tokenID).Scan(&owner); err != nil {
		return nil, invalid("no such token")
	}
	if owner != ownerID {
		return nil, invalid("the assistant's token must be one of its owner's")
	}
	return *tokenID, nil
}

// CreateAssistant connects an assistant (admins).
func (s *Service) CreateAssistant(ctx context.Context, actor *Actor, in AssistantInput) (*Assistant, error) {
	if !actor.person() || !actor.User.IsAdmin {
		return nil, ErrForbidden
	}
	r := &assistantRow{}
	r.cfg.Username = "waypoint"
	if err := in.apply(r, true); err != nil {
		return nil, err
	}
	owner := actor.id()
	if in.OwnerID != nil && *in.OwnerID != 0 {
		if _, err := s.userByID(ctx, s.db.R, *in.OwnerID); err != nil {
			return nil, invalid("no such owner")
		}
		owner = *in.OwnerID
	}
	tok, err := s.checkAssistantToken(ctx, owner, in.TokenID)
	if err != nil {
		return nil, err
	}
	var id int64
	err = s.write(ctx, actor, func(t *tx) error {
		now := s.nowMS()
		res, err := t.ExecContext(ctx, `INSERT INTO assistants(owner_id, name, url, ca_pem, door_key, key_header, agent_url, username, password, token_id, created_at, updated_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`, owner, r.Name, r.cfg.URL, r.cfg.CAPEM, r.cfg.Key, r.cfg.KeyHeader, r.AgentURL, r.cfg.Username, r.cfg.Password, tok, now, now)
		if err != nil {
			return err
		}
		id, _ = res.LastInsertId()
		t.audit(AuditEntry{Action: "assistant.connect", Category: AuditSettings, TargetType: "assistant", TargetID: id,
			Target: r.Name, URL: fmt.Sprintf("/assistant/%d", id), Detail: "door " + r.cfg.URL})
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.goBG(func() { s.checkAssistant(context.Background(), id) })
	return s.GetAssistant(ctx, actor, id)
}

// UpdateAssistant changes an assistant's name, owner, door or login (admins).
func (s *Service) UpdateAssistant(ctx context.Context, actor *Actor, id int64, in AssistantInput) (*Assistant, error) {
	if !actor.person() || !actor.User.IsAdmin {
		return nil, ErrForbidden
	}
	r, err := s.assistantRow(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := in.apply(r, false); err != nil {
		return nil, err
	}
	owner := r.ownerID
	if in.OwnerID != nil && *in.OwnerID != 0 {
		if _, err := s.userByID(ctx, s.db.R, *in.OwnerID); err != nil {
			return nil, invalid("no such owner")
		}
		owner = *in.OwnerID
	}
	tok := any(nil)
	if r.TokenID != nil {
		tok = *r.TokenID
	}
	if in.TokenID != nil {
		if tok, err = s.checkAssistantToken(ctx, owner, in.TokenID); err != nil {
			return nil, err
		}
	}
	secrets := in.Key != nil && *in.Key != "" || in.Password != nil && *in.Password != "" || in.CAPEM != nil
	err = s.write(ctx, actor, func(t *tx) error {
		if _, err := t.ExecContext(ctx, `UPDATE assistants SET owner_id=?, name=?, url=?, ca_pem=?, door_key=?, key_header=?, agent_url=?, username=?, password=?, token_id=?, updated_at=? WHERE id=?`,
			owner, r.Name, r.cfg.URL, r.cfg.CAPEM, r.cfg.Key, r.cfg.KeyHeader, r.AgentURL, r.cfg.Username, r.cfg.Password, tok, s.nowMS(), id); err != nil {
			return err
		}
		detail := "settings"
		if secrets {
			detail = "settings and credentials"
		}
		t.audit(AuditEntry{Action: "assistant.update", Category: AuditSettings, TargetType: "assistant", TargetID: id,
			Target: r.Name, URL: fmt.Sprintf("/assistant/%d", id), Detail: detail})
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.goBG(func() { s.checkAssistant(context.Background(), id) })
	return s.GetAssistant(ctx, actor, id)
}

// DeleteAssistant disconnects an assistant (admins). Its agent is untouched.
func (s *Service) DeleteAssistant(ctx context.Context, actor *Actor, id int64) error {
	if !actor.person() || !actor.User.IsAdmin {
		return ErrForbidden
	}
	r, err := s.assistantRow(ctx, id)
	if err != nil {
		return err
	}
	err = s.write(ctx, actor, func(t *tx) error {
		if _, err := t.ExecContext(ctx, `DELETE FROM assistants WHERE id = ?`, id); err != nil {
			return err
		}
		t.audit(AuditEntry{Action: "assistant.disconnect", Category: AuditSettings, TargetType: "assistant", TargetID: id, Target: r.Name})
		return nil
	})
	s.assistants.mu.Lock()
	if old, ok := s.assistants.m[id]; ok {
		old.c.Close()
		delete(s.assistants.m, id)
	}
	s.assistants.mu.Unlock()
	return err
}

// FreezeAssistant stops the agent at its gate. Only its owner can undo
// that, on Signal; Waypoint never can.
func (s *Service) FreezeAssistant(ctx context.Context, actor *Actor, id int64) (*Assistant, error) {
	c, a, err := s.UseAssistant(ctx, actor, id)
	if err != nil {
		return nil, err
	}
	if err := c.Freeze(ctx); err != nil {
		return nil, err
	}
	s.audit(ctx, actor, AuditEntry{Action: "assistant.freeze", Category: AuditControl, TargetType: "assistant", TargetID: id,
		Target: a.Name, URL: fmt.Sprintf("/assistant/%d", id), Detail: "frozen from Waypoint"})
	st := a.State
	st.Frozen = true
	if b, err := json.Marshal(st); err == nil {
		_, _ = s.db.W.ExecContext(ctx, `UPDATE assistants SET state = ? WHERE id = ?`, string(b), id)
	}
	s.checkAssistant(ctx, id)
	return s.GetAssistant(ctx, actor, id)
}

// CheckAssistant refreshes an assistant's state now, for its owner.
func (s *Service) CheckAssistant(ctx context.Context, actor *Actor, id int64) (*Assistant, error) {
	if _, _, err := s.UseAssistant(ctx, actor, id); err != nil {
		return nil, err
	}
	s.checkAssistant(ctx, id)
	return s.GetAssistant(ctx, actor, id)
}

// assistantCheckEvery is how often the watcher asks each gate how it is.
const assistantCheckEvery = 15 * time.Second

// assistantDownNotice is how long a door must stay silent before its owner
// hears about it: a deploy restarts the gate for a few seconds.
const assistantDownNotice = 2 * time.Minute

// WatchAssistants asks every assistant's gate how it is until ctx ends,
// pushing changes live and telling owners what needs them.
func (s *Service) WatchAssistants(ctx context.Context) {
	t := time.NewTicker(assistantCheckEvery)
	defer t.Stop()
	for {
		s.untracedJob(ctx, jobAssistants, assistantCheckEvery, func(ctx context.Context) {
			rows, err := s.assistantRows(ctx, ``)
			if err != nil {
				if ctx.Err() == nil {
					s.log.WarnContext(ctx, "list assistants", "err", err)
				}
				return
			}
			var wg sync.WaitGroup
			for _, r := range rows {
				wg.Add(1)
				go func() { defer wg.Done(); s.checkAssistantRow(ctx, r) }()
			}
			wg.Wait()
		})
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Service) checkAssistant(ctx context.Context, id int64) {
	if r, err := s.assistantRow(ctx, id); err == nil {
		s.checkAssistantRow(ctx, r)
	}
}

// checkAssistantRow reads the gate's status, stores it, and on a change
// publishes it to the owner and notifies them of what is new: approvals
// waiting on Signal, a freeze, an outage.
func (s *Service) checkAssistantRow(ctx context.Context, r *assistantRow) {
	c, err := s.assistantClient(r)
	prev := r.State
	next := AssistantState{CheckedAt: s.now().UTC(), Pending: []hermes.Approval{}}
	if err == nil {
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		var st *hermes.GateStatus
		st, err = c.GateStatus(cctx)
		cancel()
		if err == nil {
			next.Reachable, next.Frozen, next.Signal = true, st.Frozen, st.Signal
			if st.Pending != nil {
				next.Pending = st.Pending
			}
		}
	}
	if err != nil {
		next.Pending, next.Frozen, next.Signal = prev.Pending, prev.Frozen, prev.Signal
		next.Error = excerpt(err.Error(), 300)
		next.DownSince = prev.DownSince
		if next.DownSince == nil {
			at := next.CheckedAt
			next.DownSince = &at
		}
	}
	b, _ := json.Marshal(next)
	if _, err := s.db.W.ExecContext(ctx, `UPDATE assistants SET state = ? WHERE id = ?`, string(b), r.ID); err != nil {
		return
	}
	if !stateChanged(prev, next) {
		return
	}
	s.Hub.Publish(&Event{Kind: EvAssistant, UserID: r.ownerID, Data: map[string]any{"id": r.ID}, At: next.CheckedAt})
	link := fmt.Sprintf("/assistant/%d", r.ID)
	seen := map[string]bool{}
	for _, p := range prev.Pending {
		seen[p.Code] = true
	}
	for _, p := range next.Pending {
		if !seen[p.Code] && !prev.CheckedAt.IsZero() {
			s.notifyURL(ctx, r.ownerID, nil, NotifyAssistant, fmt.Sprintf("%s is waiting for you on Signal (%s)", r.Name, p.Code),
				p.Summary, link+"?tab=activity&view=approvals")
		}
	}
	switch {
	case next.Frozen && !prev.Frozen && prev.Reachable:
		s.notifyURL(ctx, r.ownerID, nil, NotifyAssistant, r.Name+" is frozen", "Unfreeze it from Signal when you're ready.", link)
	case !next.Frozen && prev.Frozen && next.Reachable:
		s.notifyURL(ctx, r.ownerID, nil, NotifyAssistant, r.Name+" is running again", "", link)
	case !next.Reachable && next.DownSince != nil && s.now().Sub(*next.DownSince) >= assistantDownNotice &&
		(prev.DownSince == nil || prev.CheckedAt.Sub(*prev.DownSince) < assistantDownNotice):
		s.notifyURL(ctx, r.ownerID, nil, NotifyAssistant, r.Name+" isn't answering", next.Error, link)
	case next.Reachable && !prev.Reachable && prev.DownSince != nil && prev.CheckedAt.Sub(*prev.DownSince) >= assistantDownNotice:
		s.notifyURL(ctx, r.ownerID, nil, NotifyAssistant, r.Name+" is back", "", link)
	}
}

func stateChanged(a, b AssistantState) bool {
	if a.Reachable != b.Reachable || a.Frozen != b.Frozen || a.Signal != b.Signal || a.Error != b.Error ||
		len(a.Pending) != len(b.Pending) || a.CheckedAt.IsZero() {
		return true
	}
	if !b.Reachable && b.DownSince != nil && b.CheckedAt.Sub(*b.DownSince) >= assistantDownNotice &&
		a.DownSince != nil && a.CheckedAt.Sub(*a.DownSince) < assistantDownNotice {
		return true
	}
	return !slices.EqualFunc(a.Pending, b.Pending, func(x, y hermes.Approval) bool { return x.Code == y.Code })
}

// AssistantError explains a door failure in a person's words.
func AssistantError(err error) error {
	var he *hermes.Error
	switch {
	case errors.Is(err, hermes.ErrUnreachable):
		return fmt.Errorf("the assistant's door isn't answering (stopped or frozen?): %w", ErrUnavailable)
	case errors.As(err, &he) && he.Status == 401:
		return fmt.Errorf("the door refused Waypoint's key or login: %w", ErrUnavailable)
	}
	return err
}
