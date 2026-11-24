package core

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strconv"
	"strings"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// Frontends link only the codes ResolveRefs returns (POST /refs); a code it does not resolve stays text.

// Reference kinds beyond the knowledge kinds.
const (
	KRequest = "request"
	KPull    = "pull"
)

// Ref is one reference found in a text and what it points at.
type Ref struct {
	Match      string `json:"match"` // the text as written: API:R-28, page #60, lease
	Kind       string `json:"kind"`  // issue, request, design, page, glossary or pull
	ID         int64  `json:"id,omitempty"`
	Key        string `json:"key,omitempty"` // the code to show: APP-12, API:R-28, ADR7
	Title      string `json:"title"`
	State      string `json:"state,omitempty"`
	ProjectKey string `json:"project_key,omitempty"`
	URL        string `json:"url"` // an app path, or the forge's page for a pull request
	External   bool   `json:"external,omitempty"`

	sub string
}

// refMaxTokens bounds the codes one text may ask about.
const refMaxTokens = 400

var (
	refProj = `[A-Z][A-Z0-9]{1,9}`
	refRE   = lazyre.New(`(?:^|[^A-Za-z0-9_/:#@.\-])(` + strings.Join([]string{
		`(?:` + refProj + `|me):R-?\d{1,9}`,
		`R-\d{1,9}`,
		`[A-Za-z0-9_.\-]+/[A-Za-z0-9_.\-]+#\d{1,9}`,
		`(?i:page|design note|design|decision|request|pull request|pr|pull) ?#\d{1,9}`,
		`#\d{1,9}`,
		`(?:` + refProj + `-)?ADR-?\d{1,9}`,
		refProj + `-\d{1,9}`,
		`(?:` + refProj + `-)?[A-Z]{1,2}\d{1,9}`,
	}, "|") + `)\b`)
	refNumberedRE   = lazyre.New(`(?i)^(page|design note|design|decision|request|pull request|pr|pull) ?#(\d+)$`)
	refPullRE       = lazyre.New(`^([A-Za-z0-9_.\-]+/[A-Za-z0-9_.\-]+)#(\d+)$`)
	refRequestRE    = lazyre.New(`(?i)^(?:(` + refProj + `|me):)?R-?(\d+)$`)
	refBareReqRE    = lazyre.New(`^R-\d+$`)
	refBareNoDashRE = lazyre.New(`^R\d+$`)
	refIssueRE      = lazyre.New(`^` + refProj + `-\d+$`)
)

// ScanRefs lists the code-shaped words in a text, once each in order of
// appearance, ignoring code and URLs. Which of them are real is for
// ResolveRefs to say.
func ScanRefs(text string) []string {
	text = proseOnly(text)
	var out []string
	seen := map[string]bool{}
	for _, m := range refRE.FindAllStringSubmatch(text, 1000) {
		if t := m[1]; !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out
}

// refScope is where a text lives: the project bare codes are looked up in
// first.
type refScope struct {
	key string
	id  int64
}

// ResolveRefs finds what a text names that the actor can see. A bare code
// is looked up in project (a key; may be empty) first, then wherever it is
// unambiguous. Codes that name nothing, or nothing visible, are left out.
func (s *Service) ResolveRefs(ctx context.Context, actor *Actor, text, project string) ([]Ref, error) {
	if actor == nil || actor.User == nil {
		return nil, ErrUnauthorized
	}
	var sc refScope
	if project = strings.TrimSpace(project); project != "" && !strings.EqualFold(project, "me") && !strings.EqualFold(project, "global") {
		if p, err := s.projectByRef(ctx, s.db.R, project); err == nil && s.requireProjectRole(ctx, s.db.R, actor, p.ID, RoleViewer) == nil {
			sc = refScope{p.Key, p.ID}
		}
	}
	out := []Ref{}
	for i, tok := range ScanRefs(text) {
		if i >= refMaxTokens {
			break
		}
		if r := s.resolveRef(ctx, actor, tok, sc); r != nil {
			r.Match = tok
			out = append(out, *r)
		}
	}
	return append(out, s.termRefs(ctx, actor, text, sc)...), nil
}

// resolveRef is what one code-shaped word means, if anything.
func (s *Service) resolveRef(ctx context.Context, actor *Actor, tok string, sc refScope) *Ref {
	if m := refNumberedRE.FindStringSubmatch(tok); m != nil {
		n, _ := strconv.ParseInt(m[2], 10, 32)
		switch strings.ToLower(m[1]) {
		case "page":
			return s.itemRef(ctx, actor, KPage, n)
		case "design note", "design", "decision":
			return s.itemRef(ctx, actor, KDesign, n)
		case "request":
			// A bare integer in resolveRequestID is the database id, not the request number (R-N), once other projects have filed requests.
			return s.requestByCode(ctx, actor, "R-"+m[2], sc)
		default:
			return s.pullRef(ctx, actor, "", int(min(max(n, 0), math.MaxInt32)), sc)
		}
	}
	if m := refPullRE.FindStringSubmatch(tok); m != nil {
		n, _ := strconv.Atoi(m[2])
		return s.pullRef(ctx, actor, m[1], n, sc)
	}
	if strings.HasPrefix(tok, "#") {
		n, _ := strconv.Atoi(tok[1:])
		return s.pullRef(ctx, actor, "", n, sc)
	}
	if strings.Contains(tok, ":") || refBareReqRE.MatchString(tok) {
		return s.requestByCode(ctx, actor, tok, sc)
	}
	if c, ok := ParseDesignCode(tok); ok {
		if id, err := s.designByCode(ctx, actor, c, sc.key); err == nil {
			return s.itemRef(ctx, actor, KDesign, id)
		}
	}
	if refIssueRE.MatchString(tok) {
		if id, err := s.resolveIssueID(ctx, s.db.R, tok); err == nil {
			return s.itemRef(ctx, actor, KIssue, id)
		}
		return nil
	}
	if id, err := s.ResolvePage(ctx, actor, tok, sc.key); err == nil {
		return s.itemRef(ctx, actor, KPage, id)
	}
	if refBareNoDashRE.MatchString(tok) {
		return s.requestByCode(ctx, actor, "R-"+tok[1:], sc)
	}
	return nil
}

// itemRef describes an issue, page, design note or glossary entry the
// actor can read.
func (s *Service) itemRef(ctx context.Context, actor *Actor, typ string, id int64) *Ref {
	it, err := s.kitem(ctx, actor, typ, id, "")
	if err != nil {
		return nil
	}
	r := &Ref{Kind: typ, ID: it.ID, Key: it.Key, Title: it.Title, State: it.State, ProjectKey: it.ProjectKey, sub: it.Kind}
	switch typ {
	case KIssue:
		r.URL = "/issue/" + it.Key
	case KPage:
		r.URL = "/pages/" + itoa64(it.ID)
	case KDesign:
		r.URL = "/design/" + itoa64(it.ID)
	case KGlossary:
		r.URL = "/glossary/" + itoa64(it.ID)
	}
	return r
}

// requestByCode finds the request R-28 / API:R-28 / me:R-3 names for the
// actor: in the text's own project first.
func (s *Service) requestByCode(ctx context.Context, actor *Actor, code string, sc refScope) *Ref {
	m := refRequestRE.FindStringSubmatch(code)
	if m == nil {
		return nil
	}
	if m[1] == "" && sc.id != 0 {
		n, _ := strconv.Atoi(m[2])
		vis, vargs := requestVisible(actor, "r")
		var id int64
		err := s.db.R.QueryRowContext(ctx, `SELECT r.id FROM requests r WHERE r.project_id = ? AND r.number = ? AND `+vis,
			append([]any{sc.id, n}, vargs...)...).Scan(&id)
		if err == nil {
			return s.requestRef(ctx, id)
		}
	}
	id, err := s.resolveRequestID(ctx, s.db.R, actor, code)
	if err != nil {
		return nil
	}
	return s.requestRef(ctx, id)
}

// requestRef describes a request the caller already checked is visible.
func (s *Service) requestRef(ctx context.Context, id int64) *Ref {
	var number int
	var key, title, state, kind sql.NullString
	err := s.db.R.QueryRowContext(ctx, `SELECT r.number, p.key, r.title, r.state, r.kind FROM requests r LEFT JOIN projects p ON p.id = r.project_id WHERE r.id = ?`, id).
		Scan(&number, &key, &title, &state, &kind)
	if err != nil {
		return nil
	}
	ref := requestRef(key.String, number)
	return &Ref{Kind: KRequest, ID: id, Key: ref, Title: title.String, State: state.String, ProjectKey: key.String, URL: "/requests/" + ref, sub: kind.String}
}

// pullRef links a pull request of a repo attached to a project the actor
// can see; repo is empty for a bare #91, which means the text's project's
// only repo. The forge's page is not asked for: the repo makes the URL.
func (s *Service) pullRef(ctx context.Context, actor *Actor, repo string, n int, sc refScope) *Ref {
	if n <= 0 {
		return nil
	}
	q := `SELECT pr.project_id, p.key, pr.repo, f.kind, f.base_url FROM project_repos pr JOIN forges f ON f.id = pr.forge_id JOIN projects p ON p.id = pr.project_id WHERE `
	var args []any
	if repo != "" {
		q, args = q+`pr.repo = ? COLLATE NOCASE ORDER BY pr.project_id`, []any{repo}
	} else if sc.id != 0 {
		q, args = q+`pr.project_id = ? ORDER BY pr.id`, []any{sc.id}
	} else {
		return nil
	}
	rows, err := s.db.R.QueryContext(ctx, q, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	type hit struct {
		pid                    int64
		pkey, repo, kind, base string
	}
	var hits []hit
	for rows.Next() {
		var h hit
		if rows.Scan(&h.pid, &h.pkey, &h.repo, &h.kind, &h.base) == nil && s.requireProjectRole(ctx, s.db.R, actor, h.pid, RoleViewer) == nil {
			hits = append(hits, h)
		}
	}
	if len(hits) == 0 || (repo == "" && len(hits) > 1) {
		return nil
	}
	h := hits[0]
	path := "/pulls/"
	switch h.kind {
	case "github":
		path = "/pull/"
	case "gitlab":
		path = "/-/merge_requests/"
	}
	return &Ref{Kind: KPull, Key: fmt.Sprintf("%s#%d", h.repo, n), Title: fmt.Sprintf("Pull request #%d in %s", n, h.repo), ProjectKey: h.pkey,
		URL: strings.TrimRight(h.base, "/") + "/" + h.repo + path + strconv.Itoa(n), External: true}
}

// termRefs are the glossary terms a text uses, as mentions do: terms that
// opted in to being linked, at least termMinLen letters, the nearest
// project's definition winning.
func (s *Service) termRefs(ctx context.Context, actor *Actor, text string, sc refScope) []Ref {
	idx, err := s.glossaryWords(ctx, s.db.R)
	if err != nil {
		return nil
	}
	var chain []int64
	if sc.id != 0 {
		var rerr error
		chain, rerr = s.projectChainIDs(ctx, s.db.R, sc.id)
		if rerr != nil {
			if readFault(rerr) {
				s.log.WarnContext(ctx, "project chain ids failed", "sc", sc.id, "err", rerr)
			}
		}
	}
	var out []Ref
	seen := map[int64]bool{}
	for _, h := range idx.findLongest(wordTokens(unscannedRE.ReplaceAllString(proseOnly(text), " ")), chain, s.senseGate(idx)) {
		if len(out) >= termLinkCap {
			break
		}
		if len(strings.Join(wordTokens(h.word), "")) < termMinLen || seen[h.entry] {
			continue
		}
		seen[h.entry] = true
		if r := s.itemRef(ctx, actor, KGlossary, h.entry); r != nil {
			r.Match = h.word
			out = append(out, *r)
		}
	}
	return out
}
