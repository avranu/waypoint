package core

import (
	"context"
	"database/sql"
	"fmt"
	"hash/fnv"
	"slices"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Project look (WP-65). A project's look is its icon (an emoji), colour and
// background. Nobody has to pick one: Waypoint does, without any model
// (WP-108), choosing what the other projects use least so each one stands
// apart. The owner can pick another or reset it in project settings.
// projects.look records who decided, so a person's choice is never
// overwritten on its own.
const (
	LookAuto      = "auto"      // nobody yet: Waypoint picks
	LookGenerated = "generated" // Waypoint picked; "Pick another" may replace it
	LookCustom    = "custom"    // a person chose it; only "Pick another" replaces it
	LookPlain     = "plain"     // a person reset it to the default; left alone
)

// lookColors and lookBackgrounds are what Waypoint chooses from: the
// swatches and background presets project settings offers (PROJECT_COLORS and
// BACKGROUNDS in web/src/lib/theme.ts; a test keeps them together). Every
// colour has readable text on it in light and dark (the app picks black or
// white text by the colour), and every background is a translucent wash the
// panels sit on.
var (
	lookColors = []string{
		"#6366f1", "#8b5cf6", "#ec4899", "#ef4444", "#f97316",
		"#eab308", "#22c55e", "#14b8a6", "#0ea5e9", "#64748b",
	}
	lookBackgrounds = []string{"dawn", "ocean", "forest", "dusk", "sand", "graphite", "grid"}
)

// defaultLook is what "Reset to default" shows: no icon or background, and a
// palette colour that follows from the key, so projects still differ.
func defaultLook(key string) lookChoice {
	h := fnv.New32a()
	_, _ = h.Write([]byte(strings.ToUpper(key)))
	n := uint64(h.Sum32()) % uint64(len(lookColors))
	return lookChoice{Color: lookColors[n]}
}

// lookChoice is an icon, colour and background.
type lookChoice struct {
	Icon       string `json:"icon"`
	Color      string `json:"color"`
	Background string `json:"background"`
}

// validate checks a look; nothing outside the palette gets stored.
func (l *lookChoice) validate() error {
	l.Icon = strings.TrimSpace(l.Icon)
	l.Color = strings.ToLower(strings.TrimSpace(l.Color))
	l.Background = strings.TrimSpace(l.Background)
	if !validEmoji(l.Icon) {
		return fmt.Errorf("icon %q is not a single emoji", excerpt(l.Icon, 20))
	}
	if !slices.Contains(lookColors, l.Color) {
		return fmt.Errorf("colour %q is not in the palette", excerpt(l.Color, 20))
	}
	if !slices.Contains(lookBackgrounds, l.Background) {
		return fmt.Errorf("background %q is not a preset", excerpt(l.Background, 20))
	}
	return nil
}

// validEmoji says whether s is one emoji: pictographs (or flag letters)
// joined or varied with selectors, skin tones and joiners, in at most 14 code
// points. Text, digits and punctuation fail.
func validEmoji(s string) bool {
	n := utf8.RuneCountInString(s)
	if n == 0 || n > 14 || !utf8.ValidString(s) {
		return false
	}
	for i, r := range []rune(s) {
		switch {
		case r == 0x200D || r == 0xFE0F || (r >= 0xE0020 && r <= 0xE007F): // joiner, emoji form, flag tags
			if i == 0 {
				return false
			}
		case isPictograph(r):
		default:
			return false
		}
	}
	return true
}

func isPictograph(r rune) bool {
	switch {
	case r >= 0x1F300 && r <= 0x1FAFF, // symbols, faces, people, skin tones
		r >= 0x1F1E6 && r <= 0x1F1FF, // regional indicators (flags)
		r >= 0x1F000 && r <= 0x1F2FF, // cards, enclosed symbols
		r >= 0x2600 && r <= 0x27BF,   // misc symbols, dingbats
		r >= 0x2300 && r <= 0x23FF,   // watch, hourglass, media keys
		r >= 0x2B00 && r <= 0x2BFF:   // stars, big shapes
		return true
	}
	return false
}

// iconRule maps words to an emoji. A project word matches a keyword when it is
// equal to it, or (for keywords of four letters or more) starts with it.
type iconRule struct {
	icon  string
	words []string
}

// iconRules is in priority order: on equal scores the earlier rule wins.
var iconRules = []iconRule{
	{"🤖", []string{"ai", "ml", "llm", "agent", "agents", "bot", "bots", "robot", "gpt", "neural", "model", "machine"}},
	{"🛰️", []string{"api", "server", "backend", "service", "gateway", "microservice"}},
	{"🗄️", []string{"data", "db", "database", "sql", "warehouse", "etl", "analytics", "storage"}},
	{"🌐", []string{"web", "website", "site", "frontend", "browser", "www", "landing", "portal"}},
	{"📱", []string{"mobile", "app", "ios", "android", "phone"}},
	{"📚", []string{"docs", "documentation", "wiki", "book", "library", "manual", "guide", "knowledge", "learn", "school", "course"}},
	{"🏗️", []string{"infra", "infrastructure", "ops", "deploy", "devops", "platform", "build", "ci", "cluster", "kubernetes", "k8s", "cloud"}},
	{"🎮", []string{"game", "games", "gaming", "play", "unity"}},
	{"🎵", []string{"music", "audio", "song", "sound", "podcast", "radio"}},
	{"💰", []string{"finance", "money", "billing", "payment", "payments", "invoice", "budget", "bank", "accounting", "tax", "pricing", "price"}},
	{"🩺", []string{"health", "medical", "clinic", "care", "fitness", "wellness"}},
	{"🏠", []string{"home", "house", "housing", "property", "real", "estate", "rent"}},
	{"💬", []string{"chat", "message", "messaging", "support", "forum", "community", "feedback"}},
	{"✉️", []string{"mail", "email", "newsletter", "inbox", "smtp"}},
	{"🔒", []string{"security", "secure", "auth", "authentication", "privacy", "crypto", "encryption", "vault", "secret", "secrets", "sso"}},
	{"🧪", []string{"test", "tests", "testing", "qa", "lab", "experiment", "experiments", "research"}},
	{"🎨", []string{"design", "ui", "ux", "style", "brand", "theme", "art", "creative"}},
	{"🗺️", []string{"map", "maps", "geo", "gis", "city", "urban", "location", "travel", "navigation", "atlas"}},
	{"🗓️", []string{"calendar", "time", "schedule", "planner", "planning", "booking", "events", "event"}},
	{"🎬", []string{"video", "movie", "film", "media", "stream", "streaming", "tv"}},
	{"📷", []string{"photo", "photos", "camera", "image", "images", "gallery", "picture"}},
	{"🧭", []string{"tracker", "issues", "issue", "tracking", "tickets", "waypoint", "roadmap"}},
	{"🛒", []string{"shop", "store", "ecommerce", "cart", "market", "marketplace", "retail", "order", "orders"}},
	{"🚗", []string{"car", "cars", "vehicle", "auto", "drive", "driving", "transport", "fleet"}},
	{"✈️", []string{"flight", "flights", "airline", "airport", "trip", "trips", "aviation"}},
	{"🍳", []string{"food", "recipe", "recipes", "cook", "cooking", "kitchen", "restaurant", "meal"}},
	{"🌱", []string{"garden", "plant", "plants", "farm", "agriculture", "grow", "green", "eco"}},
	{"⚡", []string{"energy", "power", "electric", "solar", "battery", "grid", "fast", "perf", "performance"}},
	{"🔧", []string{"tool", "tools", "tooling", "util", "utils", "utility", "cli", "sdk", "script", "scripts", "automation", "automate"}},
	{"📦", []string{"package", "packages", "lib", "release", "releases", "shipping", "inventory", "logistics", "module"}},
	{"📊", []string{"report", "reports", "reporting", "dashboard", "metrics", "stats", "statistics", "chart", "charts", "monitoring", "insights"}},
	{"👥", []string{"team", "people", "hr", "hiring", "crm", "customer", "customers", "social", "users"}},
	{"📝", []string{"notes", "note", "blog", "writing", "journal", "todo", "tasks", "cms", "content", "editor"}},
	{"🔍", []string{"search", "index", "crawler", "scraper", "discover", "seo"}},
	{"🧩", []string{"plugin", "plugins", "extension", "extensions", "addon", "integration", "integrations", "connector"}},
	{"🎓", []string{"education", "student", "students", "teaching", "tutor", "university", "training"}},
	{"⚖️", []string{"legal", "law", "compliance", "policy", "contract", "contracts", "audit"}},
	{"🌤️", []string{"weather", "climate", "forecast", "sky"}},
	{"🏋️", []string{"sport", "sports", "gym", "workout", "run", "running"}},
	{"🐛", []string{"bug", "bugs", "debug", "fix", "triage", "incident", "incidents"}},
	{"🚀", []string{"launch", "startup", "rocket", "space", "growth", "mvp"}},
}

// fallbackIcons are distinct, recognisable emoji for projects whose words
// match nothing.
var fallbackIcons = [...]string{
	"🦊", "🐙", "🦉", "🐝", "🦋", "🐳", "🦄", "🐢", "🦁", "🐧",
	"🌵", "🌊", "🔥", "❄️", "🌈", "🍀", "🍉", "🍋", "🍄", "🌻",
	"⭐", "🌙", "☀️", "⛰️", "🏝️", "🚲", "⛵", "🎈", "🎯", "🎲",
	"🧲", "🔭", "💎", "🪁", "🛠️", "🏆", "🔔", "🎁", "🧊", "🪴",
}

func hash32(parts ...string) uint32 {
	h := fnv.New32a()
	for _, p := range parts {
		_, _ = h.Write([]byte(strings.ToLower(p)))
		_, _ = h.Write([]byte{0})
	}
	return h.Sum32()
}

// lookWords splits text into lower-case words.
func lookWords(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// iconCandidates lists the emoji whose words appear in the project's name
// (weight 3), key (2) or description (1), best match first, ties in table
// order.
func iconCandidates(p *Project) []string {
	type weighted struct {
		words []string
		w     int
	}
	scores := make([]int, len(iconRules))
	for _, src := range []weighted{{lookWords(p.Name), 3}, {lookWords(p.Key), 2}, {lookWords(p.Description), 1}} {
		for _, word := range src.words {
			for i, r := range iconRules {
				for _, kw := range r.words {
					if word == kw || (len(kw) >= 4 && strings.HasPrefix(word, kw)) {
						scores[i] += src.w
						break
					}
				}
			}
		}
	}
	idx := make([]int, 0, len(iconRules))
	for i, sc := range scores {
		if sc > 0 {
			idx = append(idx, i)
		}
	}
	sort.SliceStable(idx, func(a, b int) bool { return scores[idx[a]] > scores[idx[b]] })
	out := make([]string, len(idx))
	for i, j := range idx {
		out[i] = iconRules[j].icon
	}
	return out
}

// leastUsed returns the option the others use least, skipping the avoided
// one. Ties go to the option whose hash with the salt is lowest, which is
// stable and differs from project to project.
func leastUsed(options []string, use map[string]int, avoid, salt string) string {
	best, bestN, bestH := "", -1, uint32(0)
	for _, o := range options {
		if o == avoid && len(options) > 1 {
			continue
		}
		n, h := use[o], hash32(salt, o)
		if bestN < 0 || n < bestN || (n == bestN && h < bestH) {
			best, bestN, bestH = o, n, h
		}
	}
	return best
}

// pickLook chooses a look for p without any model (WP-108): an icon from its
// words (or a stable fallback), and the colour and background the other
// projects (taken) use least, so projects stand apart. avoid is the look being
// replaced; none of its parts is chosen again. The result is deterministic.
func pickLook(p *Project, taken []lookChoice, avoid lookChoice) lookChoice {
	salt := p.Key + "|" + avoid.Icon + avoid.Color + avoid.Background
	icons := map[string]bool{}
	colorUse := map[string]int{}
	bgUse := map[string]int{}
	pairs := map[[2]string]bool{}
	for _, t := range taken {
		icons[t.Icon] = true
		colorUse[t.Color]++
		if t.Background != "" {
			bgUse[t.Background]++
		}
		pairs[[2]string{t.Color, t.Background}] = true
	}
	var l lookChoice
	// Icon: a keyword match nobody has, else a fallback nobody has, else the
	// best match, else a fallback by hash.
	matches := iconCandidates(p)
	free := func(icon string) bool { return !icons[icon] && icon != avoid.Icon }
	for _, ic := range matches {
		if free(ic) {
			l.Icon = ic
			break
		}
	}
	if l.Icon == "" {
		const n = uint32(len(fallbackIcons))
		start := hash32(salt) % n
		for i := uint32(0); i < n; i++ {
			if ic := fallbackIcons[(start+i)%n]; free(ic) {
				l.Icon = ic
				break
			}
		}
	}
	if l.Icon == "" {
		for _, ic := range matches {
			if ic != avoid.Icon {
				l.Icon = ic
				break
			}
		}
	}
	if l.Icon == "" {
		l.Icon = fallbackIcons[hash32(salt)%uint32(len(fallbackIcons))]
	}
	l.Color = leastUsed(lookColors, colorUse, avoid.Color, salt)
	// Background: the least used, and one that doesn't repeat the colour pair
	// of another project when there is a choice.
	opts := slices.Clone(lookBackgrounds)
	if fresh := slices.DeleteFunc(slices.Clone(opts), func(b string) bool { return pairs[[2]string{l.Color, b}] }); len(fresh) > 0 {
		opts = fresh
	}
	l.Background = leastUsed(opts, bgUse, avoid.Background, salt)
	return l
}

// takenLooks lists the looks of other non-archived projects that someone or
// something has decided (not the undecided defaults).
func takenLooks(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, except int64) ([]lookChoice, error) {
	rows, err := q.QueryContext(ctx, `SELECT icon, color, background FROM projects WHERE id != ? AND archived = 0 AND look != 'auto'`, except)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []lookChoice
	for rows.Next() {
		var l lookChoice
		if err := rows.Scan(&l.Icon, &l.Color, &l.Background); err != nil {
			return nil, err
		}
		l.Color = strings.ToLower(l.Color)
		out = append(out, l)
	}
	return out, rows.Err()
}

// proposeLook picks a project's look and checks it.
func (s *Service) proposeLook(ctx context.Context, p *Project, avoid lookChoice) (lookChoice, error) {
	taken, err := takenLooks(ctx, s.db.R, p.ID)
	if err != nil {
		return lookChoice{}, err
	}
	l := pickLook(p, taken, avoid)
	if err := l.validate(); err != nil {
		return lookChoice{}, fmt.Errorf("picked an invalid look: %w", err)
	}
	return l, nil
}

// saveLook stores a look and who decided it. With onlyAuto it changes nothing
// unless the project is still undecided, so a person's edit made meanwhile
// wins. It reports whether it changed the project.
func (s *Service) saveLook(ctx context.Context, actor *Actor, p *Project, l lookChoice, state, detail string, onlyAuto bool) (bool, error) {
	changed := false
	err := s.write(ctx, actor, func(t *tx) error {
		// A person's choice counts as a change to the project (it moves up in
		// "recently used"); the automatic pick doesn't.
		sets, args := "color = ?, icon = ?, background = ?, look = ?", []any{l.Color, l.Icon, l.Background, state}
		if !onlyAuto {
			sets, args = sets+", updated_at = ?", append(args, s.nowMS())
		}
		q := `UPDATE projects SET ` + sets + ` WHERE id = ?`
		args = append(args, p.ID)
		if onlyAuto {
			q += ` AND look = 'auto'`
		}
		res, err := t.ExecContext(ctx, q, args...)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		changed = true
		t.emit(&Event{Kind: EvProjectUpdated, ProjectID: p.ID, Field: "look", New: state})
		t.audit(AuditEntry{Action: "project.look", Category: AuditSettings, ProjectID: p.ID, TargetType: "project",
			TargetID: p.ID, Target: p.Name, Detail: detail})
		return nil
	})
	return changed, err
}

// RegenerateLook ("Pick another") picks a look for the project that differs
// from the current one in icon, colour and background, and replaces it, a
// person's own choices included (the owner asked).
func (s *Service) RegenerateLook(ctx context.Context, actor *Actor, ref string) (*Project, error) {
	if err := actor.canWrite(); err != nil {
		return nil, err
	}
	p, err := s.projectByRef(ctx, s.db.R, ref)
	if err != nil {
		return nil, err
	}
	if err := s.requireProjectRole(ctx, s.db.R, actor, p.ID, RoleOwner); err != nil {
		return nil, err
	}
	l, err := s.proposeLook(ctx, p, lookChoice{Icon: p.Icon, Color: strings.ToLower(p.Color), Background: p.Background})
	if err != nil {
		return nil, err
	}
	if _, err := s.saveLook(ctx, actor, p, l, LookGenerated, "picked another look", false); err != nil {
		return nil, err
	}
	return s.GetProject(ctx, actor, itoa64(p.ID))
}

// ResetLook puts the project's look back to the default and keeps Waypoint
// from styling it again until someone presses "Pick another".
func (s *Service) ResetLook(ctx context.Context, actor *Actor, ref string) (*Project, error) {
	if err := actor.canWrite(); err != nil {
		return nil, err
	}
	p, err := s.projectByRef(ctx, s.db.R, ref)
	if err != nil {
		return nil, err
	}
	if err := s.requireProjectRole(ctx, s.db.R, actor, p.ID, RoleOwner); err != nil {
		return nil, err
	}
	if _, err := s.saveLook(ctx, actor, p, defaultLook(p.Key), LookPlain, "reset to the default look", false); err != nil {
		return nil, err
	}
	return s.GetProject(ctx, actor, itoa64(p.ID))
}

// sweepLooks styles projects still undecided: ones from before looks were
// picked automatically, or created some other way. It runs with the hourly
// jobs, which also run at startup.
func (s *Service) sweepLooks(ctx context.Context) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT id FROM projects WHERE look = 'auto' AND archived = 0 ORDER BY id LIMIT 200`)
	if err != nil {
		s.log.WarnContext(ctx, "look sweep", "err", err)
		return
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			s.log.WarnContext(ctx, "look sweep", "err", err)
			return
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		s.log.WarnContext(ctx, "look sweep", "err", err)
		return
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		if err := s.styleProject(ctx, id); err != nil {
			s.log.Info("project look skipped", "project", id, "err", err)
		}
	}
}

// styleProject picks and saves a look for a project nobody has styled.
func (s *Service) styleProject(ctx context.Context, id int64) error {
	p, err := s.projectByID(ctx, s.db.R, id)
	if err != nil || p.Look != LookAuto {
		return err
	}
	l, err := s.proposeLook(ctx, p, lookChoice{})
	if err != nil {
		return err
	}
	_, err = s.saveLook(ctx, SystemActor(), p, l, LookGenerated, "picked automatically", true)
	return err
}
