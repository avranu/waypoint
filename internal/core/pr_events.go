package core

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"time"
)

// addedLabels are cur's labels that old didn't have (all of them for a pull
// request seen for the first time).
func addedLabels(old, cur *PullRequest) []string {
	var out []string
	for _, l := range cur.Labels {
		if old != nil && slices.ContainsFunc(old.Labels, func(x string) bool { return strings.EqualFold(x, l) }) {
			continue
		}
		out = append(out, l)
	}
	return out
}

// ForgeWhoAmI says whose account a token is; httpForgeClient has it.
type ForgeWhoAmI interface {
	TokenLogin(ctx context.Context, f *Forge) (string, error)
}

// TokenLogin is the login of the account f's token belongs to.
func (c *httpForgeClient) TokenLogin(ctx context.Context, f *Forge) (string, error) {
	var u struct {
		Login    string `json:"login"`
		Username string `json:"username"`
	}
	if err := c.do(ctx, f, http.MethodGet, "/user", nil, &u); err != nil {
		return "", err
	}
	return cmp(u.Login, u.Username), nil
}

type botLogin struct {
	token, login string
	at           time.Time
}

// tokenLoginKey: one cached login per forge and token kind.
type tokenLoginKey struct {
	forge int64
	bot   bool
}

// forgeBotLogin is the account the forge's bots post as: its bot token's,
// else its token's; "" when the forge can't say. Kept a day per token.
func (s *Service) forgeBotLogin(ctx context.Context, f *Forge) string {
	return s.tokenLogin(ctx, f, true)
}

// forgeOwnLogin is the account of the forge's token, the one Waypoint posts
// its commit statuses as (WP-416); "" when the forge can't say.
func (s *Service) forgeOwnLogin(ctx context.Context, f *Forge) string {
	return s.tokenLogin(ctx, f, false)
}

func (s *Service) tokenLogin(ctx context.Context, f *Forge, bot bool) string {
	tf := f
	if bot {
		tf = botForge(f)
	}
	if tf.token == "" {
		return ""
	}
	key := tokenLoginKey{f.ID, bot}
	if v, ok := s.botLogins.Load(key); ok {
		if b := v.(botLogin); b.token == tf.token && (s.now().Sub(b.at) < time.Hour || (b.login != "" && s.now().Sub(b.at) < 24*time.Hour)) {
			return b.login
		}
	}
	who, ok := s.Forges.(ForgeWhoAmI)
	if !ok {
		return ""
	}
	login, err := who.TokenLogin(ctx, tf)
	if err != nil {
		s.log.WarnContext(ctx, "read the account of a forge token", "forge", f.Name, "bot", bot, "err", err)
		if ctx.Err() == nil {
			s.botLogins.Store(key, botLogin{token: tf.token, at: s.now()})
		}
		return ""
	}
	s.botLogins.Store(key, botLogin{token: tf.token, login: login, at: s.now()})
	return login
}

// posters are the accounts Waypoint posts as on a forge: its token's (it
// posts as that person without a bot token, and sets statuses as them) and
// its bot token's. A marker Waypoint or a bot leaves in a comment says a
// bot wrote it only when one of them posted it: anyone can type a marker,
// and one in a person's comment made it a bot's finding, with a bot's
// severity, and took their review out of the people's (WP-934).
type posters struct {
	logins []string
}

// waypointPosters are the accounts Waypoint posts as on f; none on a forge
// with no token, where it posts nothing. A token whose account can't be read
// adds no login, so its posts count as a person's: a marker is never trusted
// from an account not known to be Waypoint's.
func (s *Service) waypointPosters(ctx context.Context, f *Forge) posters {
	var p posters
	for _, bot := range []bool{false, true} {
		if (bot && f.botToken == "") || (!bot && f.token == "") {
			continue
		}
		if l := s.tokenLogin(ctx, f, bot); l != "" {
			p.logins = append(p.logins, l)
		}
	}
	return p
}

// ours says whether author is an account Waypoint posts as.
func (p posters) ours(author string) bool {
	return author != "" && slices.ContainsFunc(p.logins, func(l string) bool { return strings.EqualFold(l, author) })
}

// botText says whether a bot wrote body, by its marker (isBotText), and an
// account Waypoint posts as posted it.
func (p posters) botText(author, body string) bool {
	return isBotText(body) && p.ours(author)
}
