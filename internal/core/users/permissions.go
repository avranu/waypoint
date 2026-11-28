package users

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/core/kernel"
)

// PermissionPreset is a starting selection the settings page offers.
type PermissionPreset struct {
	Key         string              `json:"key"`
	Label       string              `json:"label"`
	Description string              `json:"description"`
	Permissions []kernel.Permission `json:"permissions"`
}

// PermissionCatalog is everything the settings page needs to edit a token.
type PermissionCatalog struct {
	Groups  []kernel.PermissionGroup `json:"groups"`
	Presets []PermissionPreset       `json:"presets"`
}

var PermissionPresets = []PermissionPreset{
	{Key: "assistant", Label: "Assistant",
		Description: "Reads and writes issues, pages, the glossary and design notes, searches, comments and asks you things. Starts, runs, merges and deletes nothing.",
		Permissions: []kernel.Permission{kernel.PermIssuesRead, kernel.PermIssuesCreate, kernel.PermIssuesEdit, kernel.PermIssuesComment, kernel.PermTimeTrack,
			kernel.PermSearch, kernel.PermPagesRead, kernel.PermPagesWrite, kernel.PermGlossaryRead, kernel.PermGlossaryWrite, kernel.PermDesignRead, kernel.PermDesignWrite,
			kernel.PermLinksWrite, kernel.PermAttachmentsRead, kernel.PermAttachmentsWrite, kernel.PermRequestsRead, kernel.PermRequestsCreate, kernel.PermNotifications}},
	{Key: "read", Label: "Read only", Description: "Every safe read; changes nothing.", Permissions: readPermissions()},
}

// readPermissions are the safe permissions that only read.
func readPermissions() []kernel.Permission {
	return []kernel.Permission{kernel.PermIssuesRead, kernel.PermSearch, kernel.PermPagesRead, kernel.PermGlossaryRead, kernel.PermDesignRead, kernel.PermAttachmentsRead,
		kernel.PermRequestsRead, kernel.PermAgentsRead, kernel.PermSessionsRead, kernel.PermPullsRead, kernel.PermEnvironmentsRead, kernel.PermAutomationsRead, kernel.PermUsageRead}
}

// Permissions is the catalog of permissions and presets.
func (s *Store) Permissions() PermissionCatalog {
	return PermissionCatalog{Groups: kernel.PermissionGroups, Presets: PermissionPresets}
}

// normalizePermissions checks a token's permission list and returns it
// sorted and de-duplicated. nil stays nil (unrestricted); an empty list is
// a token that can only say who it is.
func normalizePermissions(in []kernel.Permission) ([]kernel.Permission, error) {
	if in == nil {
		return nil, nil
	}
	out := make([]kernel.Permission, 0, len(in))
	for _, p := range in {
		p = kernel.Permission(strings.TrimSpace(string(p)))
		if !kernel.KnownPermission(p) {
			return nil, kernel.Invalid("unknown permission %q", p)
		}
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	slices.Sort(out)
	return out, nil
}

func PermissionsJSON(ps []kernel.Permission) any {
	if ps == nil {
		return nil
	}
	b, _ := json.Marshal(ps)
	return string(b)
}

func ParsePermissions(v sql.NullString) []kernel.Permission {
	if !v.Valid {
		return nil
	}
	out := []kernel.Permission{}
	if err := json.Unmarshal([]byte(v.String), &out); err != nil {
		return []kernel.Permission{}
	}
	return out
}

func NewPermissionSet(ps []kernel.Permission) kernel.PermissionSet {
	if ps == nil {
		return nil
	}
	set := kernel.PermissionSet{}
	for _, p := range ps {
		set[p] = true
	}
	return set
}

// TokenRefusal is something a permission token was refused, counted per
// permission, so its owner can decide whether to grant it.
type TokenRefusal struct {
	Permission kernel.Permission `json:"permission"`
	Count      int               `json:"count"`
	LastAt     time.Time         `json:"last_at"`
	What       string            `json:"what"`
}

// RecordRefusal counts a refusal for the actor's token. It is best-effort:
// a failure to record never changes the answer.
func (s *Service) RecordRefusal(ctx context.Context, actor *kernel.Actor, p kernel.Permission, what string) {
	if actor == nil || actor.TokenID == 0 || !actor.Restricted() {
		return
	}
	if len(what) > 200 {
		what = what[:200]
	}
	_, err := s.st.k.DB.W.ExecContext(ctx, `INSERT INTO api_token_refusals(token_id, permission, count, last_at, what) VALUES (?,?,1,?,?)
		ON CONFLICT(token_id, permission) DO UPDATE SET count = count + 1, last_at = excluded.last_at, what = excluded.what`,
		actor.TokenID, string(p), s.st.k.NowMS(), what)
	if err != nil {
		s.st.k.Log.WarnContext(ctx, "best-effort write failed", "err", err)
	}
}

// Refused records err's refusal when it is a missing permission, and
// returns err unchanged, so callers can wrap any service call with it.
func (s *Service) Refused(ctx context.Context, actor *kernel.Actor, what string, err error) error {
	var pe *kernel.PermissionError
	if errors.As(err, &pe) {
		if pe.What != "" {
			what = pe.What
		}
		s.RecordRefusal(ctx, actor, pe.Permission, what)
	}
	return err
}

func (s *Store) TokenRefusals(ctx context.Context, ids []int64) (map[int64][]TokenRefusal, error) {
	out := map[int64][]TokenRefusal{}
	if len(ids) == 0 {
		return out, nil
	}
	// #nosec G202 -- only placeholders are concatenated; values are bound
	rows, err := s.k.DB.R.QueryContext(ctx, `SELECT token_id, permission, count, last_at, what FROM api_token_refusals
		WHERE token_id IN (`+kernel.Placeholders(len(ids))+`) ORDER BY last_at DESC`, kernel.Int64Args(ids)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, at int64
		var r TokenRefusal
		var p string
		if err := rows.Scan(&id, &p, &r.Count, &at, &r.What); err != nil {
			return nil, err
		}
		r.Permission, r.LastAt = kernel.Permission(p), kernel.MillisToTime(at)
		out[id] = append(out[id], r)
	}
	return out, rows.Err()
}

// ClearTokenRefusals forgets a token's refusals.
func (s *Service) ClearTokenRefusals(ctx context.Context, actor *kernel.Actor, id int64) error {
	if err := actor.CanManageTokens(); err != nil {
		return err
	}
	var n int
	if err := s.st.k.DB.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM api_tokens WHERE id = ? AND user_id = ?`, id, actor.ID()).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return kernel.NotFound("token")
	}
	_, err := s.st.k.DB.W.ExecContext(ctx, `DELETE FROM api_token_refusals WHERE token_id = ?`, id)
	return err
}

// TokenPatch changes a token: its name, and what it may do.
type TokenPatch struct {
	Name *string
	// SetPermissions applies Permissions (nil: unrestricted).
	SetPermissions bool
	Permissions    []kernel.Permission
	ReadOnly       *bool
}

// UpdateAPIToken changes one of the actor's tokens. Changing what a token
// may do is for people: an agent can't widen its own or another's access.
func (s *Service) UpdateAPIToken(ctx context.Context, actor *kernel.Actor, id int64, p TokenPatch) (*APIToken, error) {
	if err := actor.CanManageTokens(); err != nil {
		return nil, err
	}
	if (p.SetPermissions || p.ReadOnly != nil) && actor.IsAgent() {
		return nil, kernel.Forbidden("changing what a token may do is for people, not agents")
	}
	old, err := s.st.Token(ctx, actor, id)
	if err != nil {
		return nil, err
	}
	t := *old
	if p.Name != nil {
		name := strings.TrimSpace(*p.Name)
		if name == "" {
			return nil, kernel.Invalid("token name is required")
		}
		t.Name = name
	}
	if p.SetPermissions {
		perms, err := normalizePermissions(p.Permissions)
		if err != nil {
			return nil, err
		}
		t.Permissions = perms
	}
	if p.ReadOnly != nil {
		t.ReadOnly = *p.ReadOnly
	}
	if t.Permissions != nil {
		t.ReadOnly = false
	}
	if _, err := s.st.k.DB.W.ExecContext(ctx, `UPDATE api_tokens SET name = ?, read_only = ?, permissions = ? WHERE id = ? AND user_id = ?`,
		t.Name, kernel.BoolInt(t.ReadOnly), PermissionsJSON(t.Permissions), id, actor.ID()); err != nil {
		return nil, err
	}
	if detail := accessChange(old, &t); detail != "" {
		s.st.k.Audit(ctx, actor, kernel.AuditEntry{Action: "token.permissions", Category: kernel.AuditAccess, TargetType: "token", TargetID: t.ID,
			Target: t.Name, Detail: detail, About: t.Agent})
	}
	return s.st.Token(ctx, actor, id)
}

// accessChange describes a change in what a token may do, for the audit log.
func accessChange(old, t *APIToken) string {
	access := func(t *APIToken) string {
		switch {
		case t.Permissions != nil:
			return "permissions"
		case t.ReadOnly:
			return "read-only"
		}
		return "full access"
	}
	from, to := access(old), access(t)
	if from != "permissions" || to != "permissions" {
		if from == to {
			return ""
		}
		if to == "permissions" {
			return "limited to " + permissionNames(t.Permissions)
		}
		return "changed from " + from + " to " + to
	}
	var added, removed []kernel.Permission
	for _, p := range t.Permissions {
		if !slices.Contains(old.Permissions, p) {
			added = append(added, p)
		}
	}
	for _, p := range old.Permissions {
		if !slices.Contains(t.Permissions, p) {
			removed = append(removed, p)
		}
	}
	var parts []string
	if len(added) > 0 {
		parts = append(parts, "granted "+permissionNames(added))
	}
	if len(removed) > 0 {
		parts = append(parts, "removed "+permissionNames(removed))
	}
	return strings.Join(parts, "; ")
}

func permissionNames(ps []kernel.Permission) string {
	if len(ps) == 0 {
		return "nothing"
	}
	names := make([]string, len(ps))
	for i, p := range ps {
		names[i] = string(p)
	}
	return strings.Join(names, ", ")
}
