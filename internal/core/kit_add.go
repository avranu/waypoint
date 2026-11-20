package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// Sharing a skill, agent, command or hook through the kit (`wp kit add`,
// WP-ADR2 §4): Waypoint commits the files to a new branch of the kit repo
// and opens a pull request, reviewed and merged like any other. A plugin
// the kit doesn't have yet is added to the marketplace in the same PR.

// Kinds of things kit add shares.
var kitAddKinds = []string{"skill", "agent", "command", "hook"}

const (
	maxKitAddFiles = 50
	maxKitAddBytes = 1 << 20
)

var kitCompNameRE = lazyre.New(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// KitAddInput is one thing to share.
type KitAddInput struct {
	Kind   string `json:"kind"`   // skill, agent, command, hook
	Plugin string `json:"plugin"` // the kit plugin it goes in (made if new)
	Name   string `json:"name"`   // the skill's directory, the agent's or command's file name (no .md)
	// Files are relative to the component: a skill's SKILL.md and what is
	// beside it; an agent or command is one file; a hook's scripts (they go
	// in the plugin's hooks/).
	Files []KitAddFile `json:"files"`
	// Hooks (kind hook) is JSON to merge into the plugin's hooks/hooks.json:
	// {"hooks": {"PreToolUse": [{"matcher": "Bash", "hooks": [...]}]}}.
	Hooks   string `json:"hooks,omitempty"`
	Message string `json:"message,omitempty"` // why, for the PR
	// Description is a new plugin's, for the marketplace.
	Description string `json:"description,omitempty"`
}

// KitAddFile is a text file to add.
type KitAddFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// KitAddResult is the pull request kit add opened.
type KitAddResult struct {
	Branch   string   `json:"branch"`
	Commit   string   `json:"commit"`
	PRURL    string   `json:"pr_url"`
	PRNumber int      `json:"pr_number"`
	Files    []string `json:"files"` // paths in the kit repo
	// NewPlugin: the plugin wasn't in the kit; the PR adds it to the
	// marketplace (and it goes nowhere until kit.toml targets it).
	NewPlugin bool `json:"new_plugin,omitempty"`
}

// AddToKit opens a pull request on the kit repo with one shared component.
func (s *Service) AddToKit(ctx context.Context, actor *Actor, in KitAddInput) (*KitAddResult, error) {
	if err := actor.canWrite(); err != nil {
		return nil, err
	}
	in.Kind, in.Plugin, in.Name = strings.TrimSpace(in.Kind), strings.TrimSpace(in.Plugin), strings.TrimSuffix(strings.TrimSpace(in.Name), ".md")
	switch {
	case !slices.Contains(kitAddKinds, in.Kind):
		return nil, invalid("kind must be one of %s", strings.Join(kitAddKinds, ", "))
	case !kitNameRE.MatchString(in.Plugin):
		return nil, invalid("plugin %q must be lowercase letters, digits, dots, dashes or underscores", in.Plugin)
	case !kitCompNameRE.MatchString(in.Name):
		return nil, invalid("name %q must be letters, digits, dots, dashes or underscores", in.Name)
	case len(in.Files) > maxKitAddFiles:
		return nil, invalid("at most %d files", maxKitAddFiles)
	case len(in.Message) > 4096 || len(in.Description) > 500:
		return nil, invalid("the message can be at most 4 KB and the description 500 characters")
	}
	total := 0
	seen := map[string]bool{}
	for _, f := range in.Files {
		if !relPath(f.Path) || seen[f.Path] {
			return nil, invalid("file path %q isn't a unique relative path", f.Path)
		}
		seen[f.Path] = true
		if !utf8.ValidString(f.Content) || strings.ContainsRune(f.Content, 0) {
			return nil, invalid("%s isn't a text file: the kit takes text files only", f.Path)
		}
		total += len(f.Content)
	}
	if total > maxKitAddBytes {
		return nil, invalid("at most 1 MB of files")
	}
	switch in.Kind {
	case "skill":
		if !seen["SKILL.md"] {
			return nil, invalid("a skill needs its SKILL.md")
		}
	case "agent", "command":
		if len(in.Files) != 1 {
			return nil, invalid("an %s is one markdown file", in.Kind)
		}
	case "hook":
		if strings.TrimSpace(in.Hooks) == "" {
			return nil, invalid("a hook needs the hooks to add (JSON for hooks.json)")
		}
	}

	k, err := s.theKit(ctx, s.db.R)
	if err != nil {
		return nil, err
	}
	if k == nil || k.kit == nil {
		return nil, invalid("there is no agent kit yet: an admin picks its repo in Admin → Forges")
	}
	cl, ok := s.Forges.(ForgeCommitter)
	reader, ok2 := s.Forges.(ForgeKitReader)
	if !ok || !ok2 {
		return nil, errors.New("this forge client can't commit files")
	}
	f, err := s.forgeByID(ctx, k.forgeID)
	if err != nil {
		return nil, err
	}
	if f.token == "" {
		return nil, errors.New("the kit's forge has no API token; add one in Admin → Forges")
	}
	base := k.branch
	if base == "" {
		if base, err = cl.DefaultBranch(ctx, f, k.repo); err != nil {
			return nil, err
		}
	}
	read := func(p string) (string, error) {
		body, err := reader.FileAt(ctx, f, k.repo, p, base)
		if errors.Is(err, ErrNotFound) {
			return "", nil
		}
		return body, err
	}

	res := &KitAddResult{}
	root := "plugins/" + in.Plugin
	if p := k.plugin(in.Plugin); p != nil {
		if p.Source == "external" {
			return nil, invalid("plugin %s is hosted outside the kit repo; add to it there", in.Plugin)
		}
		root = strings.TrimPrefix(strings.TrimPrefix(p.Source, "./"), "/")
	} else {
		res.NewPlugin = true
	}
	join := func(rel string) string {
		if root == "" {
			return rel
		}
		return root + "/" + rel
	}
	var files []FileChange
	add := func(p, content string) {
		files = append(files, FileChange{Path: p, Content: content})
	}
	for _, fl := range in.Files {
		switch in.Kind {
		case "skill":
			add(join("skills/"+in.Name+"/"+fl.Path), fl.Content)
		case "agent":
			add(join("agents/"+in.Name+".md"), fl.Content)
		case "command":
			add(join("commands/"+in.Name+".md"), fl.Content)
		case "hook":
			add(join("hooks/"+fl.Path), fl.Content)
		}
	}
	if in.Kind == "hook" {
		cur, err := read(join("hooks/hooks.json"))
		if err != nil {
			return nil, err
		}
		merged, err := mergeHooks(cur, in.Hooks)
		if err != nil {
			return nil, err
		}
		add(join("hooks/hooks.json"), merged)
	}
	if res.NewPlugin {
		desc := cmp(strings.TrimSpace(in.Description), "Shared through wp kit add.")
		pj, _ := json.MarshalIndent(map[string]string{"name": in.Plugin, "description": desc}, "", "  ")
		add(join(".claude-plugin/plugin.json"), string(pj)+"\n")
		mkt, err := read(kitMarketplace)
		if err != nil {
			return nil, err
		}
		updated, err := addMarketplacePlugin(mkt, in.Plugin, "./"+root, desc)
		if err != nil {
			return nil, err
		}
		add(kitMarketplace, updated)
	}
	for i := range files {
		sha, err := cl.FileBlobSHA(ctx, f, k.repo, files[i].Path, base)
		if err != nil {
			return nil, err
		}
		files[i].BaseSHA = sha
		res.Files = append(res.Files, files[i].Path)
	}

	who := actor.User.DisplayName
	if ag := actor.agentName(); ag != "" {
		who = ag + " (for " + actor.User.DisplayName + ")"
	}
	title := fmt.Sprintf("Add %s %s to %s", in.Kind, in.Name, in.Plugin)
	sum := sha256.Sum256(fmt.Appendf(nil, "%d:%s:%d", actor.id(), title, s.nowMS()))
	res.Branch = fmt.Sprintf("kit/add-%s-%s-%s", in.Kind, slug(in.Name, 30), hex.EncodeToString(sum[:])[:5])
	msg := title
	if m := strings.TrimSpace(in.Message); m != "" {
		msg += "\n\n" + m
	}
	commit, err := cl.CommitFiles(ctx, f, k.repo, CommitInput{Base: base, Branch: res.Branch, Message: msg, Files: files})
	if err != nil {
		return nil, err
	}
	res.Commit = commit.SHA
	body := strings.TrimSpace(in.Message)
	if body != "" {
		body += "\n\n"
	}
	body += "Shared with `wp kit add` by " + who + " through Waypoint.\n\nFiles:\n"
	for _, p := range res.Files {
		body += "- `" + p + "`\n"
	}
	if res.NewPlugin {
		body += "\n`" + in.Plugin + "` is a new plugin: it goes nowhere until kit.toml targets it (`[plugins." + in.Plugin + "]`)."
	}
	pr, err := cl.CreatePR(ctx, f, k.repo, NewPR{Title: title, Body: body, Head: res.Branch, Base: base})
	if err != nil {
		return res, fmt.Errorf("pushed the branch %s but could not open the PR: %w", res.Branch, err)
	}
	res.PRURL, res.PRNumber = pr.URL, pr.Number
	return res, nil
}

// addMarketplacePlugin adds a plugin entry to marketplace.json, keeping
// the rest of the file as it is.
func addMarketplacePlugin(raw, name, source, desc string) (string, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return "", fmt.Errorf("the kit's marketplace.json: %w", err)
	}
	var plugins []json.RawMessage
	if p, ok := m["plugins"]; ok {
		if err := json.Unmarshal(p, &plugins); err != nil {
			return "", fmt.Errorf("the kit's marketplace.json plugins: %w", err)
		}
	}
	entry, _ := json.Marshal(map[string]string{"name": name, "source": source, "description": desc})
	plugins = append(plugins, entry)
	m["plugins"], _ = json.Marshal(plugins)
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b) + "\n", nil
}
