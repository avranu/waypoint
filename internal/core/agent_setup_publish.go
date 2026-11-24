package core

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// maxSharedBytes bounds the shared instructions: every session in every
// repo reads them.
const maxSharedBytes = 16 << 10

// Kinds of pull request Waypoint opens on agent files.
const (
	agentPRShared = "shared"
	agentPREdit   = "edit"
)

// SharedVersion is one saved version of the shared instructions.
type SharedVersion struct {
	Version int       `json:"version"`
	Text    string    `json:"text"`
	SavedBy *UserRef  `json:"saved_by,omitempty"`
	SavedAt time.Time `json:"saved_at"`
}

// sharedVersions are the saved versions, newest first (at most n; 0 = all).
func (s *Service) sharedVersions(ctx context.Context, q queryer, n int) ([]SharedVersion, error) {
	lim := -1
	if n > 0 {
		lim = n
	}
	rows, err := q.QueryContext(ctx, `SELECT version, text, saved_by, saved_at FROM agent_shared_versions ORDER BY version DESC LIMIT ?`, lim)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SharedVersion{}
	var by []sql.NullInt64
	for rows.Next() {
		var v SharedVersion
		var who sql.NullInt64
		var at int64
		if err := rows.Scan(&v.Version, &v.Text, &who, &at); err != nil {
			return nil, err
		}
		v.SavedAt = msTime(at)
		out = append(out, v)
		by = append(by, who)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	for i := range out {
		out[i].SavedBy = s.userRefByID(ctx, q, by[i])
	}
	return out, nil
}

// SaveSharedInstructions saves a new version of the shared instructions
// (admins, people only). Saving doesn't publish: repos missing the block
// get it within the hour, the others on Publish.
func (s *Service) SaveSharedInstructions(ctx context.Context, actor *Actor, text string) (*SharedVersion, error) {
	if err := actor.canWrite(); err != nil {
		return nil, err
	}
	if err := actor.requirePersonDirect("Editing the shared instructions"); err != nil {
		return nil, err
	}
	if !actor.isAdmin() {
		return nil, forbidden("only admins edit the shared instructions")
	}
	text = strings.TrimSpace(strings.ReplaceAll(text, "\r\n", "\n"))
	switch {
	case len(text) > maxSharedBytes:
		return nil, invalid("the shared instructions can be at most %d KB", maxSharedBytes>>10)
	case !utf8.ValidString(text) || strings.ContainsRune(text, 0):
		return nil, invalid("the shared instructions must be text")
	case strings.Contains(text, sharedOpen) || strings.Contains(text, sharedClose):
		return nil, invalid("the shared instructions can't hold Waypoint's own markers")
	}
	cur, err := s.sharedVersions(ctx, s.db.R, 1)
	if err != nil {
		return nil, err
	}
	if len(cur) == 1 && cur[0].Text == text {
		return &cur[0], nil
	}
	if len(cur) == 0 && text == "" {
		return nil, invalid("write the instructions to share first")
	}
	v := &SharedVersion{Text: text, SavedAt: msTime(s.nowMS())}
	if len(cur) == 1 {
		v.Version = cur[0].Version + 1
	} else {
		v.Version = 1
	}
	ref := actor.User.Ref()
	v.SavedBy = &ref
	err = s.write(ctx, actor, func(t *tx) error {
		if _, err := t.ExecContext(ctx, `INSERT INTO agent_shared_versions(version, text, saved_by, saved_at) VALUES (?,?,?,?)`,
			v.Version, text, actor.id(), v.SavedAt.UnixMilli()); err != nil {
			return err
		}
		t.emit(&Event{Kind: EvAgentSetupUpdated, New: fmt.Sprint(v.Version)})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return v, nil
}

// PublishResult is the repos a publish goes to, and those it skips.
type PublishResult struct {
	Version int      `json:"version"`
	Repos   []string `json:"repos"`             // a pull request is being opened on each
	Skipped []string `json:"skipped,omitempty"` // opted out: "repo (project turned it off)"
}

// PublishShared opens a pull request with the current shared instructions
// on every repo whose block differs from them (admins, people only), in
// the background; each repo's row shows how it went.
func (s *Service) PublishShared(ctx context.Context, actor *Actor) (*PublishResult, error) {
	if err := actor.canWrite(); err != nil {
		return nil, err
	}
	if err := actor.requirePersonDirect("Publishing the shared instructions"); err != nil {
		return nil, err
	}
	if !actor.isAdmin() {
		return nil, forbidden("only admins publish the shared instructions")
	}
	cur, err := s.sharedVersions(ctx, s.db.R, 1)
	if err != nil {
		return nil, err
	}
	if len(cur) == 0 {
		return nil, invalid("save the shared instructions first")
	}
	v := cur[0]
	repos, err := s.setupRepos(ctx)
	if err != nil {
		return nil, err
	}
	out := &PublishResult{Version: v.Version, Repos: []string{}}
	var todo []setupRepo
	for _, r := range repos {
		st, err := s.sharedState(ctx, r, v)
		if err != nil {
			return nil, err
		}
		switch {
		case st.state == sharedCurrent || st.state == sharedUnread || st.state == sharedNone || (st.openVersion == v.Version && st.openPR > 0):
			continue
		case r.optedOut:
			out.Skipped = append(out.Skipped, r.repo)
			continue
		}
		todo = append(todo, r)
		out.Repos = append(out.Repos, r.repo)
	}
	bg := context.WithoutCancel(ctx)
	s.goBG(func() {
		for _, r := range todo {
			if err := s.publishSharedTo(bg, actor, r, v); err != nil {
				s.log.WarnContext(ctx, "publish shared instructions", "repo", r.repo, "err", err)
			}
		}
	})
	return out, nil
}

// autoPublishShared opens the shared instructions' pull request on repos
// missing the block or carrying an older saved version, once per version
// per repo (hourly, after the repos are read).
func (s *Service) autoPublishShared(ctx context.Context) error {
	cur, err := s.sharedVersions(ctx, s.db.R, 1)
	if err != nil || len(cur) == 0 || cur[0].Text == "" {
		return err
	}
	v := cur[0]
	repos, err := s.setupRepos(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, r := range repos {
		if r.optedOut {
			continue
		}
		st, err := s.sharedState(ctx, r, v)
		if err != nil {
			return err
		}
		if (st.state != sharedMissing && st.state != sharedBehind) || st.triedVersion >= v.Version {
			continue
		}
		if err := s.publishSharedTo(ctx, SystemActor(), r, v); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// States of a repo's shared block.
const (
	sharedCurrent = "current"
	sharedMissing = "missing"
	sharedBehind  = "behind"
	sharedEdited  = "edited"
	sharedUnread  = "unread"
	sharedNone    = "none"
)

type sharedStatus struct {
	state        string
	file         string
	version      int
	openPR       int
	openURL      string
	openVersion  int
	triedVersion int
	lastError    string
}

// sharedState is where a repo's shared block stands against version v,
// from its last read and the pull requests Waypoint opened.
func (s *Service) sharedState(ctx context.Context, r setupRepo, v SharedVersion) (*sharedStatus, error) {
	st := &sharedStatus{}
	var head string
	err := s.db.R.QueryRowContext(ctx, `SELECT head FROM agent_repo_scans WHERE forge_id = ? AND repo = ? COLLATE NOCASE`, r.forgeID, r.repo).Scan(&head)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	prs, err := s.agentFilePRsOfKind(ctx, r.forgeID, r.repo, agentPRShared)
	if err != nil {
		return nil, err
	}
	for _, p := range prs {
		if p.Kind != agentPRShared {
			continue
		}
		if p.Version > st.triedVersion {
			st.triedVersion = p.Version
			st.lastError = p.Error
		}
		if p.State == "open" && p.Version > st.openVersion {
			st.openPR, st.openURL, st.openVersion = p.Number, p.URL, p.Version
		}
	}
	if head == "" {
		st.state = sharedUnread
		return st, nil
	}
	files, err := s.repoAgentFiles(ctx, s.db.R, r.forgeID, r.repo, true)
	if err != nil {
		return nil, err
	}
	claude, hasC := files["CLAUDE.md"]
	agents, hasA := files["AGENTS.md"]
	st.file = rootInstructionFile(claude.content, hasC, agents.content, hasA)
	block, has := sharedBlock(files[st.file].content)
	if !has && st.file == "AGENTS.md" && hasC {
		block, has = sharedBlock(claude.content)
	}
	switch {
	case v.Text == "" && !has:
		st.state = sharedNone
	case !has:
		st.state = sharedMissing
	case block == v.Text:
		st.state, st.version = sharedCurrent, v.Version
	default:
		st.state = sharedEdited
		old, err := s.sharedVersions(ctx, s.db.R, 0)
		if err != nil {
			return nil, err
		}
		for _, o := range old {
			if o.Text == block {
				st.state, st.version = sharedBehind, o.Version
				break
			}
		}
	}
	return st, nil
}

// AgentFilePR is a pull request Waypoint opened on a repo's agent files.
type AgentFilePR struct {
	ID       int64     `json:"id"`
	Repo     string    `json:"repo"`
	Kind     string    `json:"kind"` // shared, edit
	Version  int       `json:"version,omitempty"`
	Paths    []string  `json:"paths"`
	Branch   string    `json:"branch,omitempty"`
	Number   int       `json:"number,omitempty"`
	URL      string    `json:"url,omitempty"`
	State    string    `json:"state"` // open, merged, closed, failed
	Error    string    `json:"error,omitempty"`
	OpenedBy *UserRef  `json:"opened_by,omitempty"`
	OpenedAt time.Time `json:"opened_at"`
}

// agentFilePRs are a repo's, newest first, their state as Waypoint last
// heard it from the forge.
func (s *Service) agentFilePRs(ctx context.Context, forgeID int64, repo string) ([]AgentFilePR, error) {
	return s.agentFilePRsOfKind(ctx, forgeID, repo, "")
}

// agentFilePRsOfKind is agentFilePRs limited to one kind ("" for all). The
// newest 50 of a kind are kept, so a busy kind can't push another's records out.
func (s *Service) agentFilePRsOfKind(ctx context.Context, forgeID int64, repo, kind string) ([]AgentFilePR, error) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT a.id, a.repo, a.kind, a.version, a.paths, a.branch, a.number, a.url,
			CASE WHEN a.state = 'open' AND pr.state IN ('merged','closed') THEN pr.state ELSE a.state END, a.error, a.opened_by, a.opened_at
		FROM agent_file_prs a LEFT JOIN pull_requests pr ON pr.forge_id = a.forge_id AND pr.repo = a.repo COLLATE NOCASE AND pr.number = a.number AND a.number > 0
		WHERE a.forge_id = ? AND a.repo = ? COLLATE NOCASE AND (? = '' OR a.kind = ?) ORDER BY a.id DESC LIMIT 50`, forgeID, repo, kind, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AgentFilePR{}
	var by []sql.NullInt64
	for rows.Next() {
		var p AgentFilePR
		var paths string
		var who sql.NullInt64
		var at int64
		if err := rows.Scan(&p.ID, &p.Repo, &p.Kind, &p.Version, &paths, &p.Branch, &p.Number, &p.URL, &p.State, &p.Error, &who, &at); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(paths), &p.Paths)
		if p.Paths == nil {
			p.Paths = []string{}
		}
		p.OpenedAt = msTime(at)
		out = append(out, p)
		by = append(by, who)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	for i := range out {
		out[i].OpenedBy = s.userRefByID(ctx, s.db.R, by[i])
	}
	return out, nil
}

// agentForge is what writing a repo's agent files needs from the forge.
type agentForge struct {
	f      *Forge
	reader ForgeKitReader
	commit ForgeCommitter
}

func (s *Service) agentForge(ctx context.Context, forgeID int64) (*agentForge, error) {
	reader, ok := s.Forges.(ForgeKitReader)
	commit, ok2 := s.Forges.(ForgeCommitter)
	if !ok || !ok2 {
		return nil, errors.New("this forge client can't commit files")
	}
	f, err := s.forgeByID(ctx, forgeID)
	if err != nil {
		return nil, err
	}
	if f.token == "" {
		return nil, errors.New("the forge has no API token; add one in Admin → Forges")
	}
	return &agentForge{f: f, reader: reader, commit: commit}, nil
}

// readAt is a file at a ref, "" and false when it isn't there.
func (a *agentForge) readAt(ctx context.Context, repo, p, ref string) (string, bool, error) {
	body, err := a.reader.FileAt(ctx, a.f, repo, p, ref)
	if errors.Is(err, ErrNotFound) {
		return "", false, nil
	}
	return body, err == nil, err
}

var sharedPublishLocks sync.Map

// publishSharedTo opens the shared instructions' pull request on one repo,
// read fresh at its default branch, and closes the older one it replaces.
// A failure is recorded on the repo (and returned).
func (s *Service) publishSharedTo(ctx context.Context, actor *Actor, r setupRepo, v SharedVersion) error {
	lk, _ := sharedPublishLocks.LoadOrStore(fmt.Sprintf("%d/%s", r.forgeID, strings.ToLower(r.repo)), &sync.Mutex{})
	lk.(*sync.Mutex).Lock()
	defer lk.(*sync.Mutex).Unlock()
	prs, err := s.agentFilePRsOfKind(ctx, r.forgeID, r.repo, agentPRShared)
	if err != nil {
		return err
	}
	for _, p := range prs {
		if p.Version == v.Version && (p.State == "open" || actor.id() == 0) {
			return nil
		}
	}
	rec := &agentPRRecord{forgeID: r.forgeID, repo: r.repo, kind: agentPRShared, version: v.Version, by: actor.id()}
	err = func() error {
		af, err := s.agentForge(ctx, r.forgeID)
		if err != nil {
			return err
		}
		base, err := af.commit.DefaultBranch(ctx, af.f, r.repo)
		if err != nil {
			return err
		}
		head, err := af.reader.BranchHead(ctx, af.f, r.repo, base)
		if err != nil {
			return err
		}
		claude, hasC, err := af.readAt(ctx, r.repo, "CLAUDE.md", head)
		if err != nil {
			return err
		}
		agents, hasA, err := af.readAt(ctx, r.repo, "AGENTS.md", head)
		if err != nil {
			return err
		}
		target := rootInstructionFile(claude, hasC, agents, hasA)
		cur := agents
		if target == "CLAUDE.md" {
			cur = claude
		}
		changes := []FileChange{}
		if target == "AGENTS.md" && hasC {
			if _, has := sharedBlock(claude); has {
				changes = append(changes, FileChange{Path: "CLAUDE.md", Content: withSharedBlock(claude, "")})
			}
		}
		if next := withSharedBlock(cur, v.Text); next != cur {
			changes = append(changes, FileChange{Path: target, Content: next})
		}
		if len(changes) == 0 {
			rec.skip = true
			return nil
		}
		for i := range changes {
			sha, err := af.commit.FileBlobSHA(ctx, af.f, r.repo, changes[i].Path, base)
			if err != nil {
				return err
			}
			changes[i].BaseSHA = sha
			rec.paths = append(rec.paths, changes[i].Path)
		}
		sum := sha256.Sum256(fmt.Appendf(nil, "%s:%d:%d", r.repo, v.Version, s.nowMS()))
		rec.branch = fmt.Sprintf("waypoint/shared-v%d-%s", v.Version, hex.EncodeToString(sum[:])[:5])
		title := fmt.Sprintf("Shared agent instructions, version %d", v.Version)
		if v.Text == "" {
			title = "Take out the shared agent instructions"
		}
		msg := title + "\n\nWaypoint keeps the text between the waypoint:shared markers in line with Agents → Setup (WP-ADR44); the rest of the file is the repo's own."
		if _, err := af.commit.CommitFiles(ctx, af.f, r.repo, CommitInput{Base: base, Branch: rec.branch, Message: msg, Files: changes}); err != nil {
			return err
		}
		who := "Waypoint"
		if actor.id() != 0 {
			who = actor.User.DisplayName
		}
		body := "The shared agent instructions, version " + fmt.Sprint(v.Version) + ", published by " + who + " from Waypoint's Agents → Setup.\n\n" +
			"Only the text between `" + sharedOpen + " … -->` and `" + sharedClose + "` in `" + target + "` changes; the rest of the file is this repo's own. " +
			"Edit the shared text in Waypoint, not here: a hand edit inside the markers is replaced at the next publish."
		pr, err := af.commit.CreatePR(ctx, af.f, r.repo, NewPR{Title: title, Body: body, Head: rec.branch, Base: base, Labels: []string{automergeLabel}})
		if err != nil {
			return fmt.Errorf("pushed the branch %s but could not open the pull request: %w", rec.branch, err)
		}
		rec.number, rec.url = pr.Number, pr.URL
		return nil
	}()
	if rec.skip {
		return nil
	}
	if err != nil {
		rec.err = excerpt(err.Error(), 500)
	}
	if werr := s.recordAgentPR(ctx, rec); werr != nil {
		return errors.Join(err, werr)
	}
	if err == nil {
		s.closeOlderSharedPRs(ctx, r, rec.id)
	}
	return err
}

// closeOlderSharedPRs closes the shared pull requests on a repo that a
// newer one replaces, and deletes their branches.
func (s *Service) closeOlderSharedPRs(ctx context.Context, r setupRepo, newest int64) {
	prs, err := s.agentFilePRsOfKind(ctx, r.forgeID, r.repo, agentPRShared)
	if err != nil {
		return
	}
	cl, _ := s.Forges.(ForgeBranches)
	f, err := s.forgeByID(ctx, r.forgeID)
	if err != nil {
		return
	}
	for _, p := range prs {
		if p.Kind != agentPRShared || p.State != "open" || p.ID == newest || p.Number == 0 {
			continue
		}
		if cl == nil {
			continue
		}
		if err := cl.ClosePR(ctx, f, r.repo, p.Number); err != nil {
			s.log.WarnContext(ctx, "close a replaced shared-instructions PR", "repo", r.repo, "pr", p.Number, "err", err)
			continue
		}
		if p.Branch != "" {
			_ = cl.DeleteBranch(ctx, f, r.repo, p.Branch)
		}
		if _, err := s.db.W.ExecContext(ctx, `UPDATE agent_file_prs SET state = 'closed', closed_at = ? WHERE id = ?`, s.nowMS(), p.ID); err != nil {
			s.log.WarnContext(ctx, "record a replaced shared-instructions PR", "err", err)
		}
	}
}

type agentPRRecord struct {
	id          int64
	forgeID     int64
	repo, kind  string
	version     int
	paths       []string
	branch, url string
	number      int
	err         string
	by          int64
	skip        bool
}

func (s *Service) recordAgentPR(ctx context.Context, r *agentPRRecord) error {
	state := "open"
	if r.err != "" {
		state = "failed"
	}
	if r.paths == nil {
		r.paths = []string{}
	}
	pj, _ := json.Marshal(r.paths)
	var by any
	if r.by != 0 {
		by = r.by
	}
	return s.write(ctx, SystemActor(), func(t *tx) error {
		res, err := t.ExecContext(ctx, `INSERT INTO agent_file_prs(forge_id, repo, kind, version, paths, branch, number, url, state, error, opened_by, opened_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
			r.forgeID, r.repo, r.kind, r.version, string(pj), r.branch, r.number, r.url, state, r.err, by, s.nowMS())
		if err != nil {
			return err
		}
		r.id, _ = res.LastInsertId()
		t.emit(&Event{Kind: EvAgentSetupUpdated, Data: map[string]any{"repo": r.repo}})
		return nil
	})
}

// AgentFileEdit is a person's change to one agent file of a repo.
type AgentFileEdit struct {
	ForgeID int64  `json:"forge_id"`
	Repo    string `json:"repo"`
	Path    string `json:"path"`
	Content string `json:"content"`
	// BaseSHA is the git blob id of the file the person edited ("" for a
	// new file): the change is refused if it moved since.
	BaseSHA string `json:"base_sha"`
	Message string `json:"message,omitempty"`
}

// AgentFileEditResult is the pull request an edit opened.
type AgentFileEditResult struct {
	Branch string `json:"branch"`
	Number int    `json:"number"`
	URL    string `json:"url"`
}

// EditAgentFile opens a pull request, labelled automerge, with a person's
// change to a repo's agent file (editors of one of the repo's projects).
func (s *Service) EditAgentFile(ctx context.Context, actor *Actor, in AgentFileEdit) (*AgentFileEditResult, error) {
	if err := actor.canWrite(); err != nil {
		return nil, err
	}
	if err := actor.requirePersonDirect("Editing agent files"); err != nil {
		return nil, err
	}
	in.Path = strings.TrimPrefix(strings.TrimSpace(in.Path), "./")
	in.Content = strings.ReplaceAll(in.Content, "\r\n", "\n")
	switch {
	case !relPath(in.Path) || agentFileKind(in.Path) == "":
		return nil, invalid("%s isn't an agent file (CLAUDE.md, AGENTS.md, or one under .claude/)", in.Path)
	case len(in.Content) > maxAgentFileBytes:
		return nil, invalid("an agent file edited here can be at most %d KB", maxAgentFileBytes>>10)
	case !utf8.ValidString(in.Content) || strings.ContainsRune(in.Content, 0):
		return nil, invalid("an agent file must be text")
	case len(in.Message) > 2000:
		return nil, invalid("the message can be at most 2000 characters")
	case strings.Contains(in.Content, redacted):
		return nil, invalid("the file has a redacted value (%s) in it; put the real value back or remove it", redacted)
	}
	r, err := s.setupRepoFor(ctx, actor, in.ForgeID, in.Repo, RoleEditor)
	if err != nil {
		return nil, err
	}
	af, err := s.agentForge(ctx, r.forgeID)
	if err != nil {
		return nil, err
	}
	base, err := af.commit.DefaultBranch(ctx, af.f, r.repo)
	if err != nil {
		return nil, err
	}
	cur, err := af.commit.FileBlobSHA(ctx, af.f, r.repo, in.Path, base)
	if err != nil {
		return nil, err
	}
	if cur != in.BaseSHA {
		return nil, &ChangeConflict{Paths: []string{in.Path}}
	}
	if cur != "" {
		if old, _, err := af.readAt(ctx, r.repo, in.Path, base); err == nil && old == in.Content {
			return nil, invalid("nothing changed")
		}
	}
	verb := "Update"
	if cur == "" {
		verb = "Add"
	}
	what := verb + " " + in.Path
	title, rest := what, ""
	if m := strings.TrimSpace(in.Message); m != "" {
		first, more, _ := strings.Cut(m, "\n")
		title, rest = excerpt(strings.TrimSpace(first), 120), strings.TrimSpace(more)
	}
	msg := title
	if title != what {
		msg += "\n\n" + what + "."
	}
	if rest != "" {
		msg += "\n\n" + rest
	}
	body := rest
	if body != "" {
		body += "\n\n"
	}
	body += what + ", edited by " + actor.User.DisplayName + " in Waypoint's Agents → Setup."
	var labels []string
	switch agentFileKind(in.Path) {
	case fileHookScript, fileSettings, fileMCP, fileAgent, fileSkill:
	default:
		if !strings.Contains("/"+strings.TrimPrefix(in.Path, "./"), "/.claude/") {
			labels = []string{automergeLabel}
		}
	}
	return s.openAgentFilePR(ctx, actor, r, af, agentFilePR{base: base, path: in.Path, baseSHA: cur, content: in.Content,
		branch: "edit-" + slug(strings.TrimSuffix(in.Path, ".md"), 30), message: msg, title: title, body: body, labels: labels})
}

// agentFilePR is one file's change for openAgentFilePR.
type agentFilePR struct {
	base, path, baseSHA, content string
	branch                       string
	message, title, body         string
	labels                       []string
}

// openAgentFilePR commits one agent file's change on a new branch and opens
// its pull request, recorded as an agent PR (EditAgentFile, and a
// permission request approved with a rule, WP-233).
func (s *Service) openAgentFilePR(ctx context.Context, actor *Actor, r *setupRepo, af *agentForge, in agentFilePR) (*AgentFileEditResult, error) {
	rec := &agentPRRecord{forgeID: r.forgeID, repo: r.repo, kind: agentPREdit, paths: []string{in.path}, by: actor.id()}
	sum := sha256.Sum256(fmt.Appendf(nil, "%d:%s:%s:%d:%s:%s", actor.id(), r.repo, in.path, s.nowMS(), in.message, in.content))
	rec.branch = fmt.Sprintf("waypoint/%s-%s", in.branch, hex.EncodeToString(sum[:])[:5])
	if _, err := af.commit.CommitFiles(ctx, af.f, r.repo, CommitInput{Base: in.base, Branch: rec.branch, Message: in.message,
		Files: []FileChange{{Path: in.path, BaseSHA: in.baseSHA, Content: in.content}}}); err != nil {
		return nil, err
	}
	pr, err := af.commit.CreatePR(ctx, af.f, r.repo, NewPR{Title: in.title, Body: in.body, Head: rec.branch, Base: in.base, Labels: in.labels})
	if err != nil {
		rec.err = excerpt(err.Error(), 500)
		if err := s.recordAgentPR(ctx, rec); err != nil {
			if readFault(err) {
				s.log.WarnContext(ctx, "record agent pr failed", "repo", r.repo, "err", err)
			}
		}
		return nil, fmt.Errorf("pushed the branch %s but could not open the pull request: %w", rec.branch, err)
	}
	rec.number, rec.url = pr.Number, pr.URL
	if err := s.recordAgentPR(ctx, rec); err != nil {
		return nil, err
	}
	return &AgentFileEditResult{Branch: rec.branch, Number: pr.Number, URL: pr.URL}, nil
}

// setupRepoFor is an attached repo the actor has at least role min in one
// of the projects of (admins: any).
func (s *Service) setupRepoFor(ctx context.Context, actor *Actor, forgeID int64, repo string, min int) (*setupRepo, error) {
	repos, err := s.setupRepos(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range repos {
		if !strings.EqualFold(r.repo, strings.Trim(repo, "/ ")) || (forgeID != 0 && r.forgeID != forgeID) {
			continue
		}
		ok, err := s.canSeeRepo(ctx, actor, r, min)
		if err != nil {
			return nil, err
		}
		if !ok {
			if min > RoleViewer {
				return nil, forbidden("editing %s's agent files needs the editor role in one of its projects", r.repo)
			}
			return nil, ErrNotFound
		}
		return &r, nil
	}
	return nil, ErrNotFound
}

// canSeeRepo: the actor has at least role min (viewer or editor) in one of
// the repo's projects.
func (s *Service) canSeeRepo(ctx context.Context, actor *Actor, r setupRepo, min int) (bool, error) {
	if actor.isAdmin() {
		return true, nil
	}
	viewer, editor, err := s.projectKeysOf(ctx, actor)
	if err != nil {
		return false, err
	}
	keys := viewer
	if min > RoleViewer {
		keys = editor
	}
	return slices.ContainsFunc(r.projects, func(k string) bool { return keys[k] }), nil
}
