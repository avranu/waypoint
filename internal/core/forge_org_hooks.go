package core

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// OrgHook is an organisation webhook Waypoint added (or took over).
type OrgHook struct {
	ID        int64      `json:"id"`
	ForgeID   int64      `json:"forge_id"`
	Org       string     `json:"org"`
	State     string     `json:"state"` // pending, installed, failed
	Note      string     `json:"note,omitempty"`
	CheckedAt *time.Time `json:"checked_at"`
	SeenAt    *time.Time `json:"seen_at"` // the last signed delivery from a repo in it
}

const orgHookCols = `id, forge_id, org, state, note, checked_at, seen_at`

func scanOrgHook(sc interface{ Scan(...any) error }) (*OrgHook, error) {
	var h OrgHook
	var checked, seen sql.NullInt64
	if err := sc.Scan(&h.ID, &h.ForgeID, &h.Org, &h.State, &h.Note, &checked, &seen); err != nil {
		return nil, err
	}
	h.CheckedAt, h.SeenAt = ptrTime(checked), ptrTime(seen)
	return &h, nil
}

var (
	orgRE   = lazyre.New(`^[\w.-]+$`)
	groupRE = lazyre.New(`^[\w.-]+(/[\w.-]+)*$`)
)

func validOrgFor(kind, org string) bool {
	re := orgRE
	if kind == "gitlab" {
		re = groupRE
	}
	if !re.MatchString(org) || len(org) > 255 {
		return false
	}
	for _, seg := range strings.Split(org, "/") {
		if seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

func (s *Service) adminForge(ctx context.Context, actor *Actor, forgeID int64, write bool) (*Forge, error) {
	if write {
		if err := actor.canWrite(); err != nil {
			return nil, err
		}
	}
	if !actor.isAdmin() {
		return nil, ErrForbidden
	}
	return s.forgeByID(ctx, forgeID)
}

// ListOrgHooks lists a forge's organisation webhooks (admins only).
func (s *Service) ListOrgHooks(ctx context.Context, actor *Actor, forgeID int64) ([]*OrgHook, error) {
	if _, err := s.adminForge(ctx, actor, forgeID, false); err != nil {
		return nil, err
	}
	rows, err := s.db.R.QueryContext(ctx, `SELECT `+orgHookCols+` FROM forge_org_hooks WHERE forge_id = ? ORDER BY lower(org)`, forgeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*OrgHook{}
	for rows.Next() {
		h, err := scanOrgHook(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// ForgeOrgs lists the organisations (GitLab: groups the token owns) the
// forge's token can see, to offer for an organisation webhook.
func (s *Service) ForgeOrgs(ctx context.Context, actor *Actor, forgeID int64) ([]string, error) {
	f, err := s.adminForge(ctx, actor, forgeID, false)
	if err != nil {
		return nil, err
	}
	if f.token == "" {
		return nil, invalid("add an access token to %s first", f.Name)
	}
	h, ok := s.Forges.(ForgeHooker)
	if !ok {
		return nil, invalid("this forge client can't manage webhooks")
	}
	orgs, err := h.ListOrgs(ctx, f)
	if err != nil {
		return nil, invalid("couldn't list organisations: %s", PublicError(err))
	}
	if orgs == nil {
		orgs = []string{}
	}
	return orgs, nil
}

// AddOrgHook adds (or takes over, or tries again) the webhook on an
// organisation; the forge is asked after commit, in dispatch.
func (s *Service) AddOrgHook(ctx context.Context, actor *Actor, forgeID int64, org string) (*OrgHook, error) {
	f, err := s.adminForge(ctx, actor, forgeID, true)
	if err != nil {
		return nil, err
	}
	org = strings.Trim(strings.TrimSpace(org), "/")
	if !validOrgFor(f.Kind, org) {
		if f.Kind == "gitlab" {
			return nil, invalid("group must look like name or group/subgroup")
		}
		return nil, invalid("organisation must be a single name, like acme")
	}
	var id int64
	err = s.write(ctx, actor, func(t *tx) error {
		if _, err := t.ExecContext(ctx, `INSERT INTO forge_org_hooks(forge_id, org, state, created_at) VALUES (?,?,?,?)
			ON CONFLICT(forge_id, lower(org)) DO UPDATE SET state = excluded.state, note = ''`, f.ID, org, hookPending, s.nowMS()); err != nil {
			return err
		}
		if err := t.QueryRowContext(ctx, `SELECT id FROM forge_org_hooks WHERE forge_id = ? AND lower(org) = lower(?)`, f.ID, org).Scan(&id); err != nil {
			return err
		}
		t.emit(&Event{Kind: EvRepoHook, repoHook: &repoHookJob{installOrg: id}})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return scanOrgHook(s.db.W.QueryRowContext(ctx, `SELECT `+orgHookCols+` FROM forge_org_hooks WHERE id = ?`, id))
}

// RemoveOrgHook deletes an organisation webhook; its repos get their own
// webhooks back.
func (s *Service) RemoveOrgHook(ctx context.Context, actor *Actor, forgeID, id int64) error {
	f, err := s.adminForge(ctx, actor, forgeID, true)
	if err != nil {
		return err
	}
	return s.write(ctx, actor, func(t *tx) error {
		var org, hookID string
		if err := t.QueryRowContext(ctx, `SELECT org, hook_id FROM forge_org_hooks WHERE id = ? AND forge_id = ?`, id, f.ID).Scan(&org, &hookID); err != nil {
			return notFound("organisation webhook")
		}
		if _, err := t.ExecContext(ctx, `DELETE FROM forge_org_hooks WHERE id = ?`, id); err != nil {
			return err
		}
		t.emit(&Event{Kind: EvRepoHook, repoHook: &repoHookJob{remove: f, org: true, repo: org, hookID: hookID}})
		return nil
	})
}

// installOrgHook makes sure the organisation has Waypoint's webhook, then
// moves the repos it covers off their own webhooks (or back onto them, if
// it couldn't be added).
func (s *Service) installOrgHook(ctx context.Context, id int64) {
	var forgeID int64
	var org string
	if err := s.db.R.QueryRowContext(ctx, `SELECT forge_id, org FROM forge_org_hooks WHERE id = ?`, id).Scan(&forgeID, &org); err != nil {
		return
	}
	f, err := s.forgeByID(ctx, forgeID)
	if err != nil {
		return
	}
	on := hookOn{org: true, name: org}
	unlock := hookLock(f.ID, "org:"+org)
	hookID, created, hf, err := s.ensureForgeHook(ctx, f, on)
	state, note := hookInstalled, hookReachNote(f, s.BaseURL())
	if err != nil {
		state, note = hookFailed, hookErrorText(f, on, err)
		s.log.WarnContext(ctx, "forge organisation webhook", "forge", f.Name, "org", org, "err", err)
	}
	res, err := s.db.W.ExecContext(ctx, `UPDATE forge_org_hooks SET hook_id = CASE WHEN ? = '' THEN hook_id ELSE ? END,
		state = ?, note = ?, checked_at = ? WHERE id = ?`, hookID, hookID, state, note, s.nowMS(), id)
	if err != nil {
		unlock()
		s.log.ErrorContext(ctx, "record forge webhook", "err", err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if h, ok := s.Forges.(ForgeHooker); ok && created {
			_ = h.DeleteHook(ctx, hf, on, hookID)
		}
		unlock()
		return
	}
	if state == hookInstalled {
		s.testHook(ctx, hf, on, hookID, created)
	}
	unlock()
	s.rehookRepos(ctx, f, org)
}

// removeOrgHook deletes an organisation webhook Waypoint managed and gives
// the repos it covered their own webhooks again.
func (s *Service) removeOrgHook(ctx context.Context, f *Forge, org, hookID string) {
	if h, ok := s.Forges.(ForgeHooker); ok && hookID != "" && (f.token != "" || f.botToken != "") {
		unlock := hookLock(f.ID, "org:"+org)
		if err := deleteHook(ctx, h, f, hookOn{org: true, name: org}, hookID); err != nil && !isStatus(err, 404) {
			s.log.WarnContext(ctx, "remove forge organisation webhook", "forge", f.Name, "org", org, "err", err)
		}
		unlock()
	}
	s.rehookRepos(ctx, f, org)
}

// rehookRepos runs the repo webhook check again for every repo of an
// organisation that Waypoint manages hooks for: dropping them while the
// organisation hook covers it, adding them back when it doesn't.
func (s *Service) rehookRepos(ctx context.Context, f *Forge, org string) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT min(id), min(repo) FROM project_repos WHERE forge_id = ? AND hook_state != '' GROUP BY lower(repo)`, f.ID)
	if err != nil {
		return
	}
	var ids []int64
	for rows.Next() {
		var id int64
		var repo string
		if rows.Scan(&id, &repo) == nil && orgCovers(f.Kind, org, repo) {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		s.installRepoHook(ctx, id)
	}
}
