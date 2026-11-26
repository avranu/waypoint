package users

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/core/kernel"
	"git.jmann.me/claude/waypoint/internal/core/kernel/kerneltest"
)

const goodPassword = "correct horse battery"

type rig struct {
	*kerneltest.Env
	st    *Store
	svc   *Service
	admin *kernel.Actor
}

// newRig is a users domain over a fresh kernel, with hooks for the test to set
// before it acts (the Store holds the Hooks value it was given, so they are
// passed in).
func newRig(t *testing.T, hooks Hooks) *rig {
	t.Helper()
	e := kerneltest.New(t)
	st := NewStore(e.K, Options{BaseURL: func() string { return "https://wp.test/" }, ArchivesTranscripts: func() bool { return true }}, hooks)
	return &rig{Env: e, st: st, svc: New(st), admin: e.User("admin", true)}
}

// person makes an account with a password and signs it in.
func (r *rig) person(t *testing.T, name string) (*kernel.User, string) {
	t.Helper()
	u, err := r.svc.CreateUser(r.Ctx, r.admin, CreateUserInput{Username: name, Email: name + "@example.com", Password: goodPassword})
	if err != nil {
		t.Fatal(err)
	}
	tok, _, err := r.st.Login(r.Ctx, name, goodPassword, "test", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	return u, tok
}

func count(t *testing.T, r *rig, q string, args ...any) int {
	t.Helper()
	var n int
	if err := r.K.DB.R.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestSignInAndSessionActor(t *testing.T) {
	r := newRig(t, Hooks{})
	u, tok := r.person(t, "robin")
	if _, _, err := r.st.Login(r.Ctx, "robin", "wrong password!", "", ""); !errors.Is(err, kernel.ErrUnauthorized) {
		t.Errorf("wrong password: %v", err)
	}
	if _, _, err := r.st.Login(r.Ctx, "nobody", goodPassword, "", ""); !errors.Is(err, kernel.ErrUnauthorized) {
		t.Errorf("unknown user: %v", err)
	}
	a, err := r.st.SessionActor(r.Ctx, tok)
	if err != nil || a.User.ID != u.ID || a.SessionHash != HashToken(tok) {
		t.Fatalf("session actor: %+v %v", a, err)
	}
	if sessions, err := r.svc.ListSessions(r.Ctx, a); err != nil || len(sessions) != 1 || !sessions[0].Current {
		t.Fatalf("sessions: %+v %v", sessions, err)
	}
	r.Advance(sessionTTL + time.Hour)
	if _, err := r.st.SessionUser(r.Ctx, tok); !errors.Is(err, kernel.ErrUnauthorized) {
		t.Errorf("an expired session: %v", err)
	}
	r.Advance(-sessionTTL)
	if err := r.st.Logout(r.Ctx, tok); err != nil {
		t.Fatal(err)
	}
	if _, err := r.st.SessionUser(r.Ctx, tok); !errors.Is(err, kernel.ErrUnauthorized) {
		t.Errorf("after logout: %v", err)
	}
	off := true
	if _, err := r.svc.UpdateUser(r.Ctx, r.admin, u.ID, UserPatch{Disabled: &off}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.st.Login(r.Ctx, "robin", goodPassword, "", ""); !errors.Is(err, kernel.ErrUnauthorized) {
		t.Errorf("a disabled account signed in: %v", err)
	}
}

func TestTokenCreateActorRevoke(t *testing.T) {
	r := newRig(t, Hooks{})
	u, tok := r.person(t, "robin")
	person, err := r.st.SessionActor(r.Ctx, tok)
	if err != nil {
		t.Fatal(err)
	}
	secret, made, err := r.svc.CreateToken(r.Ctx, person, TokenSpec{Name: "ci", Agent: "anthropic/claude-code", TTL: time.Hour})
	if err != nil || !strings.HasPrefix(secret, "wpt_") || made.Agent != "anthropic/claude-code" || made.ExpiresAt == nil {
		t.Fatalf("create: %q %+v %v", secret, made, err)
	}
	a, err := r.st.TokenActor(r.Ctx, secret)
	if err != nil || a.User.ID != u.ID || a.TokenID != made.ID || a.Agent != "anthropic/claude-code" {
		t.Fatalf("token actor: %+v %v", a, err)
	}
	if _, _, err := r.svc.CreateToken(r.Ctx, &kernel.Actor{User: person.User, Permissions: kernel.PermissionSet{}}, TokenSpec{Name: "wider"}); err == nil {
		t.Error("a restricted token made a token")
	}
	if _, _, err := r.svc.CreateToken(r.Ctx, person, TokenSpec{Name: "both", ReadOnly: true, Permissions: []kernel.Permission{kernel.PermIssuesRead}}); !kernel.IsInvalid(err) {
		t.Errorf("read-only with permissions: %v", err)
	}
	if list, err := r.svc.ListAPITokens(r.Ctx, person); err != nil || len(list) != 1 || list[0].Name != "ci" {
		t.Fatalf("list: %+v %v", list, err)
	}
	r.Advance(2 * time.Hour)
	if _, err := r.st.TokenActor(r.Ctx, secret); !errors.Is(err, kernel.ErrUnauthorized) {
		t.Errorf("an expired token: %v", err)
	}
	r.Advance(-2 * time.Hour)
	if err := r.svc.RevokeAPIToken(r.Ctx, person, made.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.st.TokenActor(r.Ctx, secret); !errors.Is(err, kernel.ErrUnauthorized) {
		t.Errorf("a revoked token: %v", err)
	}
	if err := r.svc.RevokeAPIToken(r.Ctx, person, made.ID); !errors.Is(err, kernel.ErrNotFound) {
		t.Errorf("revoking twice: %v", err)
	}
	if n := count(t, r, `SELECT COUNT(*) FROM audit_log WHERE action IN ('token.create', 'token.revoke') AND user_id = ?`, u.ID); n != 2 {
		t.Errorf("token audit rows = %d, want 2", n)
	}
}

func TestJobTokenScopeComesFromTheHook(t *testing.T) {
	var asked int64
	r := newRig(t, Hooks{JobScope: func(_ context.Context, jobID int64, u *kernel.User) (map[int64]int, error) {
		asked = jobID
		return map[int64]int{u.ID: kernel.RoleEditor}, nil
	}})
	u, _ := r.person(t, "robin")
	now := r.K.NowMS()
	if _, err := r.K.DB.W.Exec(`INSERT INTO runner_jobs(id, owner_id, kind, timeout, created_at) VALUES (9, ?, 'command', 60, ?)`, u.ID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := r.K.DB.W.Exec(`INSERT INTO api_tokens(user_id, name, token_hash, prefix, read_only, agent, created_at, runner_job_id) VALUES (?, 'job', 'h', 'wpt_', 0, '', ?, 9)`, u.ID, now); err != nil {
		t.Fatal(err)
	}
	a, err := r.st.ActorFor(r.Ctx, "h", 0, false)
	if err != nil || asked != 9 || a.Scope[u.ID] != kernel.RoleEditor || a.RunnerJob != 9 {
		t.Fatalf("actor: %+v asked %d %v", a, asked, err)
	}
}

func TestInviteAccept(t *testing.T) {
	r := newRig(t, Hooks{})
	if _, err := r.svc.CreateInvite(r.Ctx, r.User("sam", false), "x@example.com", false, 0); !errors.Is(err, kernel.ErrForbidden) {
		t.Errorf("a non-admin invited: %v", err)
	}
	link, err := r.svc.CreateInvite(r.Ctx, r.admin, "", false, 0)
	if err != nil || !strings.HasPrefix(link.URL, "https://wp.test/invite/") {
		t.Fatalf("invite: %+v %v", link, err)
	}
	if pending, err := r.svc.ListInvites(r.Ctx, r.admin); err != nil || len(pending) != 1 {
		t.Fatalf("pending: %+v %v", pending, err)
	}
	tok := strings.TrimPrefix(link.URL, "https://wp.test/invite/")
	if l, err := r.st.GetLink(r.Ctx, tok); err != nil || l.Kind != "invite" {
		t.Fatalf("link: %+v %v", l, err)
	}
	in := AcceptInviteInput{Username: "robin", Email: "robin@example.com", Password: goodPassword}
	if _, _, err := r.st.AcceptInvite(r.Ctx, tok, AcceptInviteInput{Username: "me", Email: in.Email, Password: goodPassword}, "", ""); !kernel.IsInvalid(err) {
		t.Errorf("a reserved username: %v", err)
	}
	sess, u, err := r.st.AcceptInvite(r.Ctx, tok, in, "test", "127.0.0.1")
	if err != nil || u.Username != "robin" || sess == "" {
		t.Fatalf("accept: %q %+v %v", sess, u, err)
	}
	if a, err := r.st.SessionActor(r.Ctx, sess); err != nil || a.User.ID != u.ID {
		t.Errorf("the new session: %+v %v", a, err)
	}
	if _, _, err := r.st.AcceptInvite(r.Ctx, tok, AcceptInviteInput{Username: "other", Email: "o@example.com", Password: goodPassword}, "", ""); !kernel.IsInvalid(err) {
		t.Errorf("a used invite: %v", err)
	}
	if n, err := r.st.UserCount(r.Ctx); err != nil || n != 3 {
		t.Errorf("user count = %d (admin, sam, robin), %v", n, err)
	}
}

func TestResetPasswordRunsCredentialsChangedInTheTransaction(t *testing.T) {
	var r *rig
	var inHook, committedDuringHook string
	var hookFor int64
	fail := errors.New("subscriptions would not go")
	var hookErr error
	r = newRig(t, Hooks{CredentialsChanged: func(ctx context.Context, tx *kernel.Tx, userID int64) error {
		hookFor = userID
		if err := tx.QueryRowContext(ctx, `SELECT password_hash FROM users WHERE id = ?`, userID).Scan(&inHook); err != nil {
			return err
		}
		if err := r.K.DB.R.QueryRowContext(ctx, `SELECT password_hash FROM users WHERE id = ?`, userID).Scan(&committedDuringHook); err != nil {
			return err
		}
		return hookErr
	}})
	u, _ := r.person(t, "robin")
	was := u.PasswordHash
	newPW := "a different password"
	link, err := r.svc.CreateResetLink(r.Ctx, r.admin, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	tok := strings.TrimPrefix(link.URL, "https://wp.test/reset/")

	hookErr = fail
	if err := r.st.ResetPassword(r.Ctx, tok, newPW); !errors.Is(err, fail) {
		t.Fatalf("a failing hook: %v", err)
	}
	if got, _ := r.st.UserByID(r.Ctx, u.ID); got.PasswordHash != was {
		t.Error("the password changed though the hook failed")
	}
	if inHook == was || committedDuringHook != was {
		t.Errorf("the hook did not run inside the transaction: it saw a new hash %t, the database saw the old one %t", inHook != was, committedDuringHook == was)
	}
	if _, err := r.st.GetLink(r.Ctx, tok); err != nil {
		t.Errorf("the failed reset used the link: %v", err)
	}

	hookErr = nil
	if err := r.st.ResetPassword(r.Ctx, tok, newPW); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.st.UserByID(r.Ctx, u.ID); !CheckPassword(got.PasswordHash, newPW) || hookFor != u.ID {
		t.Errorf("reset: hook for %d, password ok %t", hookFor, CheckPassword(got.PasswordHash, newPW))
	}
	if _, _, err := r.st.Login(r.Ctx, "robin", goodPassword, "", ""); !errors.Is(err, kernel.ErrUnauthorized) {
		t.Errorf("the old password still signs in: %v", err)
	}
	if n := count(t, r, `SELECT COUNT(*) FROM sessions WHERE user_id = ?`, u.ID); n != 0 {
		t.Errorf("%d sessions survived a reset", n)
	}

	// ChangePassword runs the same hook and keeps the current session.
	_, tok2 := r.person(t, "sam")
	sam, _ := r.st.SessionActor(r.Ctx, tok2)
	hookFor = 0
	if err := r.svc.ChangePassword(r.Ctx, sam, "not my password", newPW); !kernel.IsInvalid(err) {
		t.Errorf("a wrong current password: %v", err)
	}
	if err := r.svc.ChangePassword(r.Ctx, sam, goodPassword, newPW); err != nil || hookFor != sam.User.ID {
		t.Errorf("change: hook for %d, %v", hookFor, err)
	}
	if _, err := r.st.SessionUser(r.Ctx, tok2); err != nil {
		t.Errorf("the current session ended: %v", err)
	}
}

func TestDeleteUserRunsBeforeDeleteThenEmits(t *testing.T) {
	var r *rig
	var sawRow bool
	var gotUser, gotBy int64
	var hookErr error
	r = newRig(t, Hooks{BeforeDelete: func(ctx context.Context, tx *kernel.Tx, userID, by int64) error {
		gotUser, gotBy = userID, by
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE id = ?`, userID).Scan(&n); err != nil {
			return err
		}
		sawRow = n == 1
		return hookErr
	}})
	u, _ := r.person(t, "robin")
	if err := r.svc.DeleteUser(r.Ctx, r.admin, r.admin.User.ID); !kernel.IsInvalid(err) {
		t.Errorf("deleting yourself: %v", err)
	}
	if err := r.svc.DeleteUser(r.Ctx, r.User("sam", false), u.ID); !errors.Is(err, kernel.ErrForbidden) {
		t.Errorf("a non-admin deleted: %v", err)
	}
	r.Events()

	hookErr = errors.New("no")
	if err := r.svc.DeleteUser(r.Ctx, r.admin, u.ID); !errors.Is(err, hookErr) {
		t.Fatalf("a failing hook: %v", err)
	}
	if _, err := r.st.UserByID(r.Ctx, u.ID); err != nil || len(r.Events()) != 0 {
		t.Errorf("the failed delete left a mark: %v", err)
	}

	hookErr = nil
	if err := r.svc.DeleteUser(r.Ctx, r.admin, u.ID); err != nil {
		t.Fatal(err)
	}
	if !sawRow || gotUser != u.ID || gotBy != r.admin.User.ID {
		t.Errorf("hook: row present %t, user %d, by %d", sawRow, gotUser, gotBy)
	}
	if _, err := r.st.UserByID(r.Ctx, u.ID); !errors.Is(err, kernel.ErrNotFound) {
		t.Errorf("the user is still there: %v", err)
	}
	evs := r.Events()
	if len(evs) != 1 || evs[0].Kind != EvUserDeleted || evs[0].UserID != u.ID || !evs[0].AdminOnly {
		t.Fatalf("events: %+v", evs)
	}
	var detail string
	if err := r.K.DB.R.QueryRow(`SELECT detail FROM audit_log WHERE action = 'user.delete'`).Scan(&detail); err != nil || !strings.Contains(detail, "archived transcripts") {
		t.Errorf("audit detail %q, %v", detail, err)
	}
	if err := r.svc.DeleteUser(r.Ctx, r.admin, u.ID); !errors.Is(err, kernel.ErrNotFound) {
		t.Errorf("deleting twice: %v", err)
	}
}

func TestRefusalRecordedAndCleared(t *testing.T) {
	r := newRig(t, Hooks{})
	_, tok := r.person(t, "robin")
	person, _ := r.st.SessionActor(r.Ctx, tok)
	secret, made, err := r.svc.CreateToken(r.Ctx, person, TokenSpec{Name: "reader", Permissions: []kernel.Permission{kernel.PermIssuesRead}})
	if err != nil {
		t.Fatal(err)
	}
	agent, err := r.st.TokenActor(r.Ctx, secret)
	if err != nil || !agent.Restricted() {
		t.Fatalf("token actor: %+v %v", agent, err)
	}
	r.svc.RecordRefusal(r.Ctx, person, kernel.PermIssuesEdit, "not a token")
	if n := count(t, r, `SELECT COUNT(*) FROM api_token_refusals`); n != 0 {
		t.Errorf("a refusal for a person was recorded: %d", n)
	}
	err = r.svc.Refused(r.Ctx, agent, "edit an issue", agent.Need(kernel.PermIssuesEdit))
	var pe *kernel.PermissionError
	if !errors.As(err, &pe) {
		t.Fatalf("Refused changed the error: %v", err)
	}
	r.svc.RecordRefusal(r.Ctx, agent, kernel.PermIssuesEdit, "edit again")
	list, err := r.svc.ListAPITokens(r.Ctx, person)
	if err != nil || len(list) != 1 || len(list[0].Refusals) != 1 || list[0].Refusals[0].Count != 2 || list[0].Refusals[0].What != "edit again" {
		t.Fatalf("refusals: %+v %v", list, err)
	}
	if err := r.svc.ClearTokenRefusals(r.Ctx, r.User("sam", false), made.ID); !errors.Is(err, kernel.ErrNotFound) {
		t.Errorf("clearing another person's: %v", err)
	}
	if err := r.svc.ClearTokenRefusals(r.Ctx, person, made.ID); err != nil {
		t.Fatal(err)
	}
	if list, _ := r.svc.ListAPITokens(r.Ctx, person); len(list[0].Refusals) != 0 {
		t.Errorf("refusals after clearing: %+v", list[0].Refusals)
	}
}

// jobToken inserts a runner job and a token for it, and returns the token's hash.
func jobToken(t *testing.T, r *rig, u *kernel.User, hash string, withJob bool) {
	t.Helper()
	now := r.K.NowMS()
	var job any
	if withJob {
		if _, err := r.K.DB.W.Exec(`INSERT INTO runner_jobs(id, owner_id, kind, timeout, created_at) VALUES (9, ?, 'command', 60, ?)`, u.ID, now); err != nil {
			t.Fatal(err)
		}
		job = 9
	}
	if _, err := r.K.DB.W.Exec(`INSERT INTO api_tokens(user_id, name, token_hash, prefix, read_only, agent, created_at, runner_job_id) VALUES (?, 'job', ?, 'wpt_', 0, '', ?, ?)`, u.ID, hash, now, job); err != nil {
		t.Fatal(err)
	}
}

func TestJobTokenWithoutJobScopeHookReachesNoProject(t *testing.T) {
	r := newRig(t, Hooks{})
	u, _ := r.person(t, "robin")
	jobToken(t, r, u, "h", true)
	a, err := r.st.ActorFor(r.Ctx, "h", 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if a.Scope == nil || len(a.Scope) != 0 {
		t.Errorf("scope = %#v, want non-nil and empty", a.Scope)
	}
}

func TestJobTokenScopeErrorFailsTheActor(t *testing.T) {
	fail := errors.New("no scope")
	r := newRig(t, Hooks{JobScope: func(context.Context, int64, *kernel.User) (map[int64]int, error) { return nil, fail }})
	u, _ := r.person(t, "robin")
	jobToken(t, r, u, "h", true)
	if a, err := r.st.ActorFor(r.Ctx, "h", 0, false); !errors.Is(err, fail) || a != nil {
		t.Errorf("actor %+v, err %v; want the hook's error", a, err)
	}
}

func TestTokenWithoutJobSkipsJobScope(t *testing.T) {
	called := false
	r := newRig(t, Hooks{JobScope: func(context.Context, int64, *kernel.User) (map[int64]int, error) {
		called = true
		return map[int64]int{}, nil
	}})
	u, _ := r.person(t, "robin")
	jobToken(t, r, u, "h", false)
	a, err := r.st.ActorFor(r.Ctx, "h", 0, false)
	if err != nil || called || a.Scope != nil {
		t.Errorf("hook called %t, scope %#v, err %v", called, a.Scope, err)
	}
}

func TestNilHooksAreSkipped(t *testing.T) {
	r := newRig(t, Hooks{})
	u, _ := r.person(t, "robin")
	link, err := r.svc.CreateResetLink(r.Ctx, r.admin, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.st.ResetPassword(r.Ctx, strings.TrimPrefix(link.URL, "https://wp.test/reset/"), "a different password"); err != nil {
		t.Errorf("reset with no hook: %v", err)
	}
	_, tok := r.person(t, "sam")
	sam, _ := r.st.SessionActor(r.Ctx, tok)
	if err := r.svc.ChangePassword(r.Ctx, sam, goodPassword, "a different password"); err != nil {
		t.Errorf("change with no hook: %v", err)
	}
	if err := r.svc.DeleteUser(r.Ctx, r.admin, u.ID); err != nil {
		t.Errorf("delete with no hook: %v", err)
	}
}

func TestFailedCredentialsChangedRollsBackChangePassword(t *testing.T) {
	fail := errors.New("subscriptions would not go")
	r := newRig(t, Hooks{CredentialsChanged: func(context.Context, *kernel.Tx, int64) error { return fail }})
	u, tok := r.person(t, "robin")
	if _, _, err := r.st.Login(r.Ctx, "robin", goodPassword, "other", "127.0.0.2"); err != nil {
		t.Fatal(err)
	}
	a, err := r.st.SessionActor(r.Ctx, tok)
	if err != nil {
		t.Fatal(err)
	}
	was := u.PasswordHash
	if err := r.svc.ChangePassword(r.Ctx, a, goodPassword, "a different password"); !errors.Is(err, fail) {
		t.Fatalf("change: %v", err)
	}
	if got, _ := r.st.UserByID(r.Ctx, u.ID); got.PasswordHash != was {
		t.Error("the password changed though the hook failed")
	}
	if n := count(t, r, `SELECT COUNT(*) FROM sessions WHERE user_id = ?`, u.ID); n != 2 {
		t.Errorf("%d sessions, want both kept", n)
	}
}
