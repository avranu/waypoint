package core

import (
	"strings"
)

// maxSessionRepos bounds Repos.
const maxSessionRepos = 200

// validateSessionPrefs checks the settings and tidies them: repo names lower case,
// repos with nothing set dropped. Empty is nil.
func validateSessionPrefs(p *SessionPrefs) (*SessionPrefs, error) {
	if p == nil {
		return nil, nil
	}
	if err := p.All.Validate("all sessions"); err != nil {
		return nil, err
	}
	if len(p.Repos) > maxSessionRepos {
		return nil, invalid("session settings for at most %d repos", maxSessionRepos)
	}
	repos := map[string]SessionSettings{}
	for name, s := range p.Repos {
		key := strings.ToLower(strings.TrimSpace(name))
		if !nestedRE.MatchString(key) || len(key) > 200 {
			return nil, invalid("%q isn't a repo like owner/name", name)
		}
		if _, dup := repos[key]; dup {
			return nil, invalid("%s has two sets of session settings", key)
		}
		if err := s.Validate(key); err != nil {
			return nil, err
		}
		if !s.Empty() {
			repos[key] = s
		}
	}
	out := &SessionPrefs{All: p.All}
	if len(repos) > 0 {
		out.Repos = repos
	}
	if out.All.Empty() && out.Repos == nil {
		return nil, nil
	}
	return out, nil
}
