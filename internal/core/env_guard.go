package core

import (
	"context"
	"net"
	"net/url"
	"sort"
	"strings"
)

// Addresses an agent, an agent's shell, an automation or a token set don't count, so an agent can't open the guard by recording one.

// maxGuardHosts bounds the list one SessionStart carries.
const maxGuardHosts = 50

// EnvGuardHosts lists those hosts for repo (owner/name) as host:port,
// sorted, lower case; empty (not nil) when there are none, so the CLI's
// copy is cleared.
func (s *Service) EnvGuardHosts(ctx context.Context, actor *Actor, repo string) []string {
	hosts := []string{}
	repo = strings.TrimSpace(repo)
	if repo == "" || actor == nil || actor.User == nil {
		return hosts
	}
	ids, err := queryIDs(ctx, s.db.R, `SELECT DISTINCT project_id FROM project_repos WHERE repo = ? COLLATE NOCASE ORDER BY project_id`, repo)
	if err != nil {
		s.log.WarnContext(ctx, "guard hosts", "repo", repo, "err", err)
		return hosts
	}
	seen := map[string]bool{}
	for _, id := range ids {
		r, rerr := s.projectRole(ctx, s.db.R, actor, id)
		if rerr != nil {
			if readFault(rerr) {
				s.log.WarnContext(ctx, "project role failed", "id", id, "err", rerr)
			}
		}
		if r <= RoleNone {
			continue
		}
		rows, err := s.db.R.QueryContext(ctx, `SELECT url, health_url, version_url FROM environments
			WHERE project_id = ? AND stopped_at IS NULL AND address_agent = '' ORDER BY position, id`, id)
		if err != nil {
			s.log.WarnContext(ctx, "guard hosts", "project", id, "err", err)
			continue
		}
		for rows.Next() {
			var addrs [3]string
			if rows.Scan(&addrs[0], &addrs[1], &addrs[2]) != nil {
				continue
			}
			for _, a := range addrs {
				if h := envAddressHost(a); h != "" && !seen[h] && len(seen) < maxGuardHosts {
					seen[h] = true
					hosts = append(hosts, h)
				}
			}
		}
		_ = rows.Close()
	}
	sort.Strings(hosts)
	return hosts
}

// envAddressHost is the host and port of an environment address, lower
// case, the port the scheme's when it names none (app.example.com:443); ""
// when it has none. Only that port: other services on the machine aren't
// what the person named.
func envAddressHost(addr string) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return ""
	}
	u, err := url.Parse(addr)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return ""
	}
	port := u.Port()
	if port == "" {
		port = map[string]string{"http": "80", "https": "443"}[u.Scheme]
	}
	return net.JoinHostPort(strings.ToLower(u.Hostname()), port)
}
