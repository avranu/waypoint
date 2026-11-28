package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"git.jmann.me/claude/waypoint/internal/core/udiff"
)

// SetupCheck is one of Waypoint's checks on a repo or machine.
type SetupCheck struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	State string `json:"state"` // ok, warn, fail
	Note  string `json:"note,omitempty"`
}

// Check states.
const (
	checkOK   = "ok"
	checkWarn = "warn"
	checkFail = "fail"
)

// SetupComponent is a component of a repo or machine, and whether no other
// repo (machine) has it.
type SetupComponent struct {
	AgentComponent
	OnlyHere bool `json:"only_here,omitempty"`
}

// CommonComponent is a component and how many repos (machines) have it.
type CommonComponent struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Count  int    `json:"count"`
	Common bool   `json:"common"` // its absence counts as missing
}

// AgentRepo is one attached repo's agent setup.
type AgentRepo struct {
	ForgeID   int64      `json:"forge_id"`
	Repo      string     `json:"repo"`
	URL       string     `json:"url"`
	Projects  []string   `json:"projects"`
	OptedOut  bool       `json:"opted_out"` // a project turned "Keep this project's repos in line" off
	CanEdit   bool       `json:"can_edit"`
	Branch    string     `json:"branch,omitempty"`
	Head      string     `json:"head,omitempty"`
	ScannedAt *time.Time `json:"scanned_at"`
	Error     string     `json:"error,omitempty"`
	// RootFile is the instruction file the shared block is (or goes) in.
	RootFile string `json:"root_file,omitempty"`
	// Shared is where its shared block stands: current, missing, behind,
	// edited, unread, none.
	Shared        string           `json:"shared"`
	SharedVersion int              `json:"shared_version,omitempty"`
	SharedPR      *AgentFilePR     `json:"shared_pr,omitempty"` // open
	SharedError   string           `json:"shared_error,omitempty"`
	Files         []AgentFile      `json:"files"`
	Components    []SetupComponent `json:"components"`
	Missing       []AgentComponent `json:"missing"`
	KitPlugins    []string         `json:"kit_plugins"`
	Checks        []SetupCheck     `json:"checks"`
	PRs           []AgentFilePR    `json:"prs"`
	// Status sums it up: ok, warn, fail, unread.
	Status string `json:"status"`
}

// AgentMachine is one machine's last report.
type AgentMachine struct {
	User       UserRef          `json:"user"`
	Machine    string           `json:"machine"`
	OS         string           `json:"os"`
	WPVersion  string           `json:"wp_version"`
	ReportedAt time.Time        `json:"reported_at"`
	Files      []AgentFile      `json:"files"`
	Components []SetupComponent `json:"components"`
	Missing    []AgentComponent `json:"missing"`
	Checks     []SetupCheck     `json:"checks"`
	Status     string           `json:"status"`
}

// SharedInfo is the shared instructions and where the repos stand.
type SharedInfo struct {
	Current  *SharedVersion  `json:"current"`
	Versions []SharedVersion `json:"versions"` // newest first, at most 20
	Counts   map[string]int  `json:"counts"`   // shared state → repos
}

// AgentSetup is the Setup tab.
type AgentSetup struct {
	Shared        SharedInfo        `json:"shared"`
	Repos         []AgentRepo       `json:"repos"`
	RepoCommon    []CommonComponent `json:"repo_common"`
	Machines      []AgentMachine    `json:"machines"`
	MachineCommon []CommonComponent `json:"machine_common"`
	// CanManage: the viewer may edit and publish the shared instructions.
	CanManage bool `json:"can_manage"`
	// CanEdit: the viewer is a person (agents read only).
	CanEdit bool `json:"can_edit"`
}

// GetAgentSetup is the Setup tab: the repos of the viewer's projects (all
// for admins), the viewer's machines (all for admins).
func (s *Service) GetAgentSetup(ctx context.Context, actor *Actor) (*AgentSetup, error) {
	if actor == nil || actor.User == nil {
		return nil, ErrUnauthorized
	}
	person := !actor.IsAgent() && actor.AgentEnv == "" && actor.Session == 0
	out := &AgentSetup{Repos: []AgentRepo{}, Machines: []AgentMachine{}, CanManage: actor.IsAdmin() && person, CanEdit: person}
	versions, err := s.sharedVersions(ctx, s.db.R, 20)
	if err != nil {
		return nil, err
	}
	out.Shared = SharedInfo{Versions: versions, Counts: map[string]int{}}
	cur := SharedVersion{}
	if len(versions) > 0 {
		out.Shared.Current, cur = &versions[0], versions[0]
	}
	repos, err := s.setupRepos(ctx)
	if err != nil {
		return nil, err
	}
	viewer, editor, err := s.projectKeysOf(ctx, actor)
	if err != nil {
		return nil, err
	}
	kit, err := s.theKit(ctx, s.db.R)
	if err != nil {
		return nil, err
	}
	hooks, err := s.repoHookStates(ctx)
	if err != nil {
		return nil, err
	}
	forges := map[int64]*Forge{}
	var comps [][]AgentComponent
	for _, r := range repos {
		if !actor.IsAdmin() && !slices.ContainsFunc(r.projects, func(k string) bool { return viewer[k] }) {
			continue
		}
		ar := AgentRepo{ForgeID: r.forgeID, Repo: r.repo, Projects: r.projects, OptedOut: r.optedOut, KitPlugins: []string{}, Missing: []AgentComponent{}}
		ar.CanEdit = person && (actor.IsAdmin() || slices.ContainsFunc(r.projects, func(k string) bool { return editor[k] }))
		f, ok := forges[r.forgeID]
		if !ok {
			var rerr error
			f, rerr = s.forgeByID(ctx, r.forgeID)
			if rerr != nil {
				if readFault(rerr) && !errors.Is(rerr, ErrNotFound) {
					s.log.WarnContext(ctx, "forge by id failed", "forgeid", r.forgeID, "err", rerr)
				}
			}
			forges[r.forgeID] = f
		}
		if f != nil {
			ar.URL = strings.TrimRight(f.BaseURL, "/") + "/" + r.repo
		}
		var scanned sql.NullInt64
		err := s.db.R.QueryRowContext(ctx, `SELECT branch, head, scanned_at, error FROM agent_repo_scans WHERE forge_id = ? AND repo = ? COLLATE NOCASE`,
			r.forgeID, r.repo).Scan(&ar.Branch, &ar.Head, &scanned, &ar.Error)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		ar.ScannedAt = ptrTime(scanned)
		files, err := s.repoAgentFiles(ctx, s.db.R, r.forgeID, r.repo, true)
		if err != nil {
			return nil, err
		}
		contents, kinds := map[string]string{}, map[string]string{}
		ar.Files = []AgentFile{}
		for _, fl := range files {
			ar.Files = append(ar.Files, AgentFile{Path: fl.path, Kind: fl.kind, Size: fl.size, Lines: lineCount(fl.content), Truncated: fl.truncated})
			contents[fl.path], kinds[fl.path] = fl.content, fl.kind
		}
		sortAgentFiles(ar.Files)
		rc := componentsOf(contents, kinds)
		comps = append(comps, rc)
		ar.Components = make([]SetupComponent, len(rc))
		for i, c := range rc {
			ar.Components[i] = SetupComponent{AgentComponent: c}
		}
		st, err := s.sharedState(ctx, r, cur)
		if err != nil {
			return nil, err
		}
		if out.Shared.Current == nil && st.state != sharedUnread {
			st.state = sharedNone
		}
		ar.RootFile, ar.Shared, ar.SharedVersion, ar.SharedError = st.file, st.state, st.version, ""
		if st.triedVersion == cur.Version && st.lastError != "" {
			ar.SharedError = st.lastError
		}
		prs, err := s.agentFilePRs(ctx, r.forgeID, r.repo)
		if err != nil {
			return nil, err
		}
		ar.PRs = prs
		if len(ar.PRs) > 10 {
			ar.PRs = ar.PRs[:10]
		}
		for i := range prs {
			if prs[i].Kind == agentPRShared && prs[i].State == "open" && prs[i].Version == st.openVersion {
				ar.SharedPR = &prs[i]
				break
			}
		}
		if kit != nil && kit.kit != nil {
			for _, p := range kit.kit.resolve(r.repo, r.projects).Plugins {
				ar.KitPlugins = append(ar.KitPlugins, p.Name)
			}
		}
		ar.Checks, err = s.repoChecks(ctx, r, &ar, kit, hooks[repoKey(r.forgeID, r.repo)], files)
		if err != nil {
			return nil, err
		}
		out.Shared.Counts[ar.Shared]++
		out.Repos = append(out.Repos, ar)
	}
	out.RepoCommon = compareComponents(comps, func(i int, missing []AgentComponent, only map[string]bool) {
		r := &out.Repos[i]
		if r.ScannedAt == nil || r.Head == "" {
			return
		}
		r.Missing = missing
		for j := range r.Components {
			r.Components[j].OnlyHere = only[r.Components[j].key()]
		}
	}, func(i int) bool { return out.Repos[i].Head != "" })
	for i := range out.Repos {
		out.Repos[i].Status = setupStatus(out.Repos[i].Checks, len(out.Repos[i].Missing), out.Repos[i].Head == "" && out.Repos[i].Error == "")
	}
	sort.SliceStable(out.Repos, func(i, j int) bool {
		a, b := statusRank(out.Repos[i].Status), statusRank(out.Repos[j].Status)
		if a != b {
			return a < b
		}
		return strings.ToLower(out.Repos[i].Repo) < strings.ToLower(out.Repos[j].Repo)
	})

	machines, err := s.machineReports(ctx, actor)
	if err != nil {
		return nil, err
	}
	out.Machines = machines
	mcomps := make([][]AgentComponent, len(machines))
	for i, m := range machines {
		for _, c := range m.Components {
			mcomps[i] = append(mcomps[i], c.AgentComponent)
		}
	}
	out.MachineCommon = compareComponents(mcomps, func(i int, missing []AgentComponent, only map[string]bool) {
		m := &out.Machines[i]
		m.Missing = missing
		for j := range m.Components {
			m.Components[j].OnlyHere = only[m.Components[j].key()]
		}
	}, func(int) bool { return true })
	newest := newestVersion(machines)
	for i := range out.Machines {
		m := &out.Machines[i]
		if newest != "" && m.WPVersion != "" && m.WPVersion != "dev" && versionLess(m.WPVersion, newest) {
			m.Checks = append(m.Checks, SetupCheck{Key: "wp_current", Label: "wp is current", State: checkWarn,
				Note: "wp " + m.WPVersion + " here, " + newest + " on another machine: run wp update"})
		}
		if age := s.now().Sub(m.ReportedAt); age > 7*24*time.Hour {
			m.Checks = append(m.Checks, SetupCheck{Key: "reported", Label: "Reported lately", State: checkWarn,
				Note: "last reported " + strconv.Itoa(int(age.Hours()/24)) + " days ago"})
		}
		m.Status = setupStatus(m.Checks, len(m.Missing), false)
	}
	return out, nil
}

func repoKey(forgeID int64, repo string) string {
	return strconv.FormatInt(forgeID, 10) + ":" + strings.ToLower(repo)
}

func sortAgentFiles(fs []AgentFile) {
	sort.SliceStable(fs, func(i, j int) bool {
		di, dj := agentPathDepth(fs[i].Path), agentPathDepth(fs[j].Path)
		ii, ij := fs[i].Kind == fileInstructions, fs[j].Kind == fileInstructions
		if ii != ij {
			return ii
		}
		if di != dj {
			return di < dj
		}
		return fs[i].Path < fs[j].Path
	})
}

// projectKeysOf are the keys of the projects the actor is a member of, and
// those it edits.
func (s *Service) projectKeysOf(ctx context.Context, actor *Actor) (viewer, editor map[string]bool, err error) {
	viewer, editor = map[string]bool{}, map[string]bool{}
	for _, m := range []struct {
		role int
		into map[string]bool
	}{{RoleViewer, viewer}, {RoleEditor, editor}} {
		set, args := actor.MemberProjects(m.role)
		// #nosec G202 -- set is placeholders; values are bound
		rows, err := s.db.R.QueryContext(ctx, `SELECT key FROM projects WHERE id IN `+set, args...)
		if err != nil {
			return nil, nil, err
		}
		for rows.Next() {
			var k string
			if err := rows.Scan(&k); err != nil {
				rows.Close()
				return nil, nil, err
			}
			m.into[k] = true
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, nil, err
		}
	}
	return viewer, editor, nil
}

type hookState struct {
	seen   bool
	failed bool
	note   string
}

// repoHookStates is each attached repo's forge webhook: whether a delivery
// came, whether installing it failed.
func (s *Service) repoHookStates(ctx context.Context) (map[string]hookState, error) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT forge_id, repo, hook_seen_at IS NOT NULL, hook_state, hook_note FROM project_repos`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]hookState{}
	for rows.Next() {
		var fid int64
		var repo, state, note string
		var seen bool
		if err := rows.Scan(&fid, &repo, &seen, &state, &note); err != nil {
			return nil, err
		}
		k := repoKey(fid, repo)
		h := out[k]
		h.seen = h.seen || seen
		if state == hookFailed {
			h.failed, h.note = true, note
		}
		out[k] = h
	}
	return out, rows.Err()
}

// repoChecks are Waypoint's checks on one repo.
func (s *Service) repoChecks(ctx context.Context, r setupRepo, ar *AgentRepo, kit *kitRow, hook hookState, files map[string]storedAgentFile) ([]SetupCheck, error) {
	out := []SetupCheck{}
	if ar.Error != "" {
		out = append(out, SetupCheck{Key: "read", Label: "Read from the forge", State: checkFail, Note: ar.Error})
	}
	switch {
	case hook.seen:
		out = append(out, SetupCheck{Key: "webhook", Label: "Forge webhook", State: checkOK})
	case hook.failed:
		out = append(out, SetupCheck{Key: "webhook", Label: "Forge webhook", State: checkFail, Note: cmp(hook.note, "Waypoint couldn't install its webhook")})
	default:
		out = append(out, SetupCheck{Key: "webhook", Label: "Forge webhook", State: checkWarn, Note: "no delivery from the forge yet: pushes and pull requests aren't seen as they happen"})
	}
	if kit != nil && kit.kit != nil {
		var n, current int
		err := s.db.R.QueryRowContext(ctx, `SELECT count(*), coalesce(sum(kit_commit = ?), 0) FROM kit_reports WHERE repo = ? COLLATE NOCASE`, kit.commit, r.repo).Scan(&n, &current)
		if err != nil {
			return nil, err
		}
		switch {
		case current > 0:
			out = append(out, SetupCheck{Key: "kit", Label: "Agent kit synced", State: checkOK})
		case n > 0:
			out = append(out, SetupCheck{Key: "kit", Label: "Agent kit synced", State: checkWarn, Note: "checkouts here run an older kit (wp kit sync updates one)"})
		default:
			out = append(out, SetupCheck{Key: "kit", Label: "Agent kit synced", State: checkWarn, Note: "no checkout of this repo has run wp kit sync"})
		}
	}
	if ar.Head != "" {
		_, hasC := files["CLAUDE.md"]
		_, hasA := files["AGENTS.md"]
		if hasC || hasA {
			out = append(out, SetupCheck{Key: "instructions", Label: "Root instruction file", State: checkOK})
		} else {
			out = append(out, SetupCheck{Key: "instructions", Label: "Root instruction file", State: checkFail, Note: "no CLAUDE.md or AGENTS.md at the root: agents start without the repo's rules"})
		}
	}
	note := ""
	if ar.SharedPR != nil {
		note = "pull request #" + strconv.Itoa(ar.SharedPR.Number) + " is open"
	}
	switch ar.Shared {
	case sharedCurrent:
		out = append(out, SetupCheck{Key: "shared", Label: "Shared instructions", State: checkOK})
	case sharedMissing:
		out = append(out, SetupCheck{Key: "shared", Label: "Shared instructions", State: checkWarn, Note: cmp(note, cmp(ar.SharedError, "not in "+ar.RootFile+" yet"))})
	case sharedBehind:
		out = append(out, SetupCheck{Key: "shared", Label: "Shared instructions", State: checkWarn, Note: cmp(note, cmp(ar.SharedError, "version "+strconv.Itoa(ar.SharedVersion)+" here"))})
	case sharedEdited:
		out = append(out, SetupCheck{Key: "shared", Label: "Shared instructions", State: checkWarn, Note: cmp(note, "edited by hand in "+ar.RootFile+": the next Publish replaces it")})
	}
	if r.optedOut {
		for i := range out {
			if out[i].Key == "shared" && out[i].State != checkOK {
				out[i].Note = "this repo's project turned automatic changes off"
			}
		}
	}
	return out, nil
}

// compareComponents counts each component over a set (repos or machines)
// and tells each member what it misses and what only it has. A component
// counts as common when at least half of the members (and at least 3) have
// it. counted says which members were read; the others are left out.
func compareComponents(sets [][]AgentComponent, apply func(i int, missing []AgentComponent, only map[string]bool), counted func(i int) bool) []CommonComponent {
	type agg struct {
		c     AgentComponent
		count int
	}
	tally := map[string]*agg{}
	n := 0
	for i, set := range sets {
		if !counted(i) {
			continue
		}
		n++
		seen := map[string]bool{}
		for _, c := range set {
			k := c.key()
			if seen[k] {
				continue
			}
			seen[k] = true
			if tally[k] == nil {
				tally[k] = &agg{c: AgentComponent{Kind: c.Kind, Name: c.Name}}
			}
			tally[k].count++
		}
	}
	common := func(count int) bool { return count >= 3 && count*2 >= n }
	for i, set := range sets {
		if !counted(i) {
			continue
		}
		have := map[string]bool{}
		for _, c := range set {
			have[c.key()] = true
		}
		missing := []AgentComponent{}
		only := map[string]bool{}
		for k, a := range tally {
			if common(a.count) && !have[k] {
				missing = append(missing, a.c)
			}
			if a.count == 1 && have[k] && n > 1 {
				only[k] = true
			}
		}
		sortComponents(missing)
		apply(i, missing, only)
	}
	out := []CommonComponent{}
	for _, a := range tally {
		out = append(out, CommonComponent{Kind: a.c.Kind, Name: a.c.Name, Count: a.count, Common: common(a.count)})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		a, b := slices.Index(componentOrder, out[i].Kind), slices.Index(componentOrder, out[j].Kind)
		if a != b {
			return a < b
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out
}

func setupStatus(checks []SetupCheck, missing int, unread bool) string {
	if unread {
		return "unread"
	}
	st := checkOK
	for _, c := range checks {
		switch c.State {
		case checkFail:
			return checkFail
		case checkWarn:
			st = checkWarn
		}
	}
	if missing > 0 {
		st = checkWarn
	}
	return st
}

func statusRank(s string) int {
	switch s {
	case checkFail:
		return 0
	case checkWarn:
		return 1
	case "unread":
		return 2
	}
	return 3
}

// AgentFileContent is one agent file with its text.
type AgentFileContent struct {
	AgentFile
	Repo    string `json:"repo,omitempty"`
	Machine string `json:"machine,omitempty"`
	// SHA is the git blob id of the text, for editing against it.
	SHA      string `json:"sha,omitempty"`
	URL      string `json:"url,omitempty"` // on the forge
	Editable bool   `json:"editable"`
	// Shared is the shared block's text in it, if it has one.
	Shared string `json:"shared,omitempty"`
}

// GetAgentFile is one of a repo's agent files as last read.
func (s *Service) GetAgentFile(ctx context.Context, actor *Actor, forgeID int64, repo, p string) (*AgentFileContent, error) {
	if actor == nil || actor.User == nil {
		return nil, ErrUnauthorized
	}
	r, err := s.setupRepoFor(ctx, actor, forgeID, repo, RoleViewer)
	if err != nil {
		return nil, err
	}
	files, err := s.repoAgentFiles(ctx, s.db.R, r.forgeID, r.repo, true)
	if err != nil {
		return nil, err
	}
	fl, ok := files[p]
	if !ok {
		return nil, ErrNotFound
	}
	out := &AgentFileContent{AgentFile: AgentFile{Path: fl.path, Kind: fl.kind, Size: fl.size, Lines: lineCount(fl.content), Truncated: fl.truncated, Content: fl.content}, Repo: r.repo}
	if !fl.truncated {
		out.SHA = udiff.GitBlobSHA(fl.content)
	}
	if b, has := sharedBlock(fl.content); has {
		out.Shared = b
	}
	editor, err := s.canSeeRepo(ctx, actor, *r, RoleEditor)
	if err != nil {
		return nil, err
	}
	out.Editable = editor && !fl.truncated && !actor.IsAgent() && actor.AgentEnv == "" && actor.Session == 0
	var branch string
	_ = s.db.R.QueryRowContext(ctx, `SELECT branch FROM agent_repo_scans WHERE forge_id = ? AND repo = ? COLLATE NOCASE`, r.forgeID, r.repo).Scan(&branch)
	if f, err := s.forgeByID(ctx, r.forgeID); err == nil && branch != "" {
		out.URL = forgeFileURL(f, r.repo, branch, p, 0)
	}
	return out, nil
}

// MachineReport is what `wp` reports about the machine it runs on.
type MachineReport struct {
	Machine   string        `json:"machine"`
	OS        string        `json:"os"`
	WPVersion string        `json:"wp_version"`
	Files     []MachineFile `json:"files"`
	Checks    []SetupCheck  `json:"checks"`
}

// MachineFile is one of a machine's agent files, its path relative to the
// home folder (.claude/CLAUDE.md).
type MachineFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// maxMachineBytes bounds what one machine's report keeps.
const maxMachineBytes = 2 << 20

// ReportMachine records a machine's setup (anyone signed in; reports are
// their own). Settings are cleaned again here, whatever wp sent.
func (s *Service) ReportMachine(ctx context.Context, actor *Actor, in MachineReport) error {
	if err := actor.CanWrite(); err != nil {
		return err
	}
	clean := func(v string, n int) string {
		return strings.TrimSpace(strings.Map(func(r rune) rune {
			if unicode.IsControl(r) {
				return -1
			}
			return r
		}, excerpt(v, n)))
	}
	in.Machine, in.OS, in.WPVersion = clean(in.Machine, 100), clean(in.OS, 40), clean(in.WPVersion, 64)
	switch {
	case in.Machine == "":
		return invalid("a report needs the machine's name")
	case len(in.Files) > maxAgentFiles || len(in.Checks) > 50:
		return invalid("at most %d files and 50 checks", maxAgentFiles)
	}
	files := []storedMachineFile{}
	total := 0
	for _, f := range in.Files {
		p := strings.TrimPrefix(strings.TrimSpace(f.Path), "./")
		kind := machineFileKind(p)
		if !relPath(p) || kind == "" {
			continue
		}
		body := f.Content
		switch {
		case p == ".claude.json":
			body = mcpOnly(body)
		case strings.HasSuffix(p, ".json"):
			r, ok := redactSettings(body)
			if !ok {
				r = ""
			}
			body = r
		}
		sf := newStoredFile(p, kind, body)
		if total+len(sf.content) > maxMachineBytes {
			sf.content, sf.truncated = "", true
		}
		total += len(sf.content)
		files = append(files, storedMachineFile{Path: p, Kind: kind, Content: sf.content, Size: sf.size, Truncated: sf.truncated})
	}
	checks := []SetupCheck{}
	for _, c := range in.Checks {
		c.Key, c.Label, c.Note = clean(c.Key, 40), clean(c.Label, 100), clean(c.Note, 500)
		if c.State != checkOK && c.State != checkWarn && c.State != checkFail {
			continue
		}
		if c.Key != "" && c.Label != "" {
			checks = append(checks, c)
		}
	}
	fj, _ := json.Marshal(files)
	cj, _ := json.Marshal(checks)
	return s.write(ctx, actor, func(t *tx) error {
		if _, err := t.ExecContext(ctx, `INSERT INTO machine_reports(user_id, machine, os, wp_version, files, checks, reported_at) VALUES (?,?,?,?,?,?,?)
			ON CONFLICT(user_id, machine) DO UPDATE SET os = excluded.os, wp_version = excluded.wp_version, files = excluded.files,
				checks = excluded.checks, reported_at = excluded.reported_at`,
			actor.ID(), in.Machine, in.OS, in.WPVersion, string(fj), string(cj), s.nowMS()); err != nil {
			return err
		}
		t.emit(&Event{Kind: EvAgentSetupUpdated, Data: map[string]any{"machine": in.Machine}})
		return nil
	})
}

type storedMachineFile struct {
	Path      string `json:"path"`
	Kind      string `json:"kind"`
	Content   string `json:"content"`
	Size      int    `json:"size"`
	Truncated bool   `json:"truncated,omitempty"`
}

// machineReports are the viewer's machines (admins: everyone's), newest
// report first, without file text.
func (s *Service) machineReports(ctx context.Context, actor *Actor) ([]AgentMachine, error) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT u.id, u.username, u.display_name, u.avatar_version, m.machine, m.os, m.wp_version, m.files, m.checks, m.reported_at
		FROM machine_reports m JOIN users u ON u.id = m.user_id WHERE ? OR m.user_id = ? ORDER BY m.reported_at DESC LIMIT 200`, actor.IsAdmin(), actor.ID())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AgentMachine{}
	for rows.Next() {
		var m AgentMachine
		var dname, files, checks string
		var av, at int64
		if err := rows.Scan(&m.User.ID, &m.User.Username, &dname, &av, &m.Machine, &m.OS, &m.WPVersion, &files, &checks, &at); err != nil {
			return nil, err
		}
		m.User.DisplayName, m.User.AvatarURL = cmp(dname, m.User.Username), avatarURL(m.User.ID, av)
		m.ReportedAt = msTime(at)
		var fs []storedMachineFile
		_ = json.Unmarshal([]byte(files), &fs)
		_ = json.Unmarshal([]byte(checks), &m.Checks)
		if m.Checks == nil {
			m.Checks = []SetupCheck{}
		}
		contents, kinds := map[string]string{}, map[string]string{}
		m.Files = []AgentFile{}
		for _, f := range fs {
			m.Files = append(m.Files, AgentFile{Path: f.Path, Kind: f.Kind, Size: f.Size, Lines: lineCount(f.Content), Truncated: f.Truncated})
			contents[f.Path], kinds[f.Path] = f.Content, f.Kind
		}
		sortAgentFiles(m.Files)
		for _, c := range componentsOf(contents, kinds) {
			m.Components = append(m.Components, SetupComponent{AgentComponent: c})
		}
		if m.Components == nil {
			m.Components = []SetupComponent{}
		}
		m.Missing = []AgentComponent{}
		out = append(out, m)
	}
	return out, rows.Err()
}

// GetMachineFile is one of a machine's agent files (its owner and admins).
func (s *Service) GetMachineFile(ctx context.Context, actor *Actor, userID int64, machine, p string) (*AgentFileContent, error) {
	if actor == nil || actor.User == nil {
		return nil, ErrUnauthorized
	}
	if userID != actor.ID() && !actor.IsAdmin() {
		return nil, ErrNotFound
	}
	var files string
	err := s.db.R.QueryRowContext(ctx, `SELECT files FROM machine_reports WHERE user_id = ? AND machine = ?`, userID, machine).Scan(&files)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var fs []storedMachineFile
	_ = json.Unmarshal([]byte(files), &fs)
	for _, f := range fs {
		if f.Path == p {
			return &AgentFileContent{AgentFile: AgentFile{Path: f.Path, Kind: f.Kind, Size: f.Size, Lines: lineCount(f.Content), Truncated: f.Truncated, Content: f.Content}, Machine: machine}, nil
		}
	}
	return nil, ErrNotFound
}

// newestVersion is the newest wp version the machines report.
func newestVersion(ms []AgentMachine) string {
	newest := ""
	for _, m := range ms {
		if m.WPVersion == "" || m.WPVersion == "dev" {
			continue
		}
		if newest == "" || versionLess(newest, m.WPVersion) {
			newest = m.WPVersion
		}
	}
	return newest
}

// versionLess compares wp versions (v0.25.1, 0.25.1-3-gabc1234) by their
// numbers; what follows them breaks ties as text.
func versionLess(a, b string) bool {
	pa, ra := versionParts(a)
	pb, rb := versionParts(b)
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			return pa[i] < pb[i]
		}
	}
	return ra < rb
}

func versionParts(v string) ([3]int, string) {
	var out [3]int
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	nums, rest, _ := strings.Cut(v, "-")
	for i, p := range strings.SplitN(nums, ".", 3) {
		n, _ := strconv.Atoi(p)
		out[i] = n
	}
	return out, rest
}
