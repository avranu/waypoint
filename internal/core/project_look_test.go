package core

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func TestValidEmoji(t *testing.T) {
	for _, s := range []string{"🚀", "🏠", "⚙️", "👩‍💻", "🇸🇪", "👍🏽", "⭐", "🧪"} {
		if !validEmoji(s) {
			t.Errorf("%q should be an emoji", s)
		}
	}
	for _, s := range []string{"", "a", "rocket", "1", "#", "🚀 x", "<b>", "\u200d", "\ufe0f", "🚀\n", strings.Repeat("🚀", 15), "é", "→"} {
		if validEmoji(s) {
			t.Errorf("%q should not be an emoji", s)
		}
	}
}

// The palette Waypoint picks from is the one project settings offers.
func TestLookPaletteMatchesWeb(t *testing.T) {
	b, err := os.ReadFile("../../web/src/lib/theme.ts")
	if err != nil {
		t.Skip("web sources not here:", err)
	}
	src := string(b)
	colors := src[strings.Index(src, "export const PROJECT_COLORS"):]
	var web []string
	for _, m := range regexp.MustCompile(`'(#[0-9a-f]{6})'`).FindAllStringSubmatch(colors, -1) {
		web = append(web, m[1])
	}
	if !slices.Equal(web, lookColors) {
		t.Errorf("lookColors %v differ from PROJECT_COLORS %v", lookColors, web)
	}
	bgs := src[strings.Index(src, "export const BACKGROUNDS"):strings.Index(src, "// Unsaved look")]
	var webBg []string
	for _, m := range regexp.MustCompile(`id: '([a-z]+)'`).FindAllStringSubmatch(bgs, -1) {
		webBg = append(webBg, m[1])
	}
	if !slices.Equal(webBg, lookBackgrounds) {
		t.Errorf("lookBackgrounds %v differ from BACKGROUNDS %v", lookBackgrounds, webBg)
	}
}

func TestDefaultLook(t *testing.T) {
	a, b := defaultLook("WP"), defaultLook("wp")
	if a != b || a.Icon != "" || a.Background != "" || !slices.Contains(lookColors, a.Color) {
		t.Errorf("default look = %+v / %+v", a, b)
	}
}

func lookOf(e *env, key string) *Project {
	e.t.Helper()
	p, err := e.s.GetProject(e.ctx, e.admin, key)
	if err != nil {
		e.t.Fatal(err)
	}
	return p
}

func TestPickLookKeywords(t *testing.T) {
	for _, c := range []struct {
		name, key, desc, icon string
	}{
		{"Marketing website", "MKT", "", "🌐"},
		{"Payments API", "PAY", "", "🛰️"},
		{"Garden", "GRD", "", "🌱"},
		{"Docs", "DOC", "", "📚"},
		{"Atlas", "AT", "city maps", "🗺️"},
		{"Zebra", "ZZ", "a game about llamas", "🎮"},
		{"Infrastructure", "INF", "", "🏗️"},
	} {
		got := pickLook(&Project{Name: c.name, Key: c.key, Description: c.desc}, nil, lookChoice{})
		if got.Icon != c.icon {
			t.Errorf("%q: icon %s, want %s", c.name, got.Icon, c.icon)
		}
		if err := (&got).validate(); err != nil {
			t.Errorf("%q: %v", c.name, err)
		}
	}
	a := pickLook(&Project{Name: "Qwxz", Key: "QW"}, nil, lookChoice{})
	if a.Icon == "" || a != pickLook(&Project{Name: "Qwxz", Key: "QW"}, nil, lookChoice{}) {
		t.Errorf("fallback not stable: %+v", a)
	}
	b := pickLook(&Project{Name: "Garden", Key: "GRD"}, []lookChoice{{Icon: "🌱"}}, lookChoice{})
	if b.Icon == "🌱" || b.Icon == "" {
		t.Errorf("a taken icon was chosen: %+v", b)
	}
	for _, ic := range fallbackIcons {
		if !validEmoji(ic) {
			t.Errorf("fallback %q is not an emoji", ic)
		}
	}
	for _, r := range iconRules {
		if !validEmoji(r.icon) {
			t.Errorf("rule icon %q is not an emoji", r.icon)
		}
	}
}

func TestProjectLookPickedOnCreate(t *testing.T) {
	e := newEnv(t)
	colors, bgs, icons := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, name := range []string{"Alpha", "Beta", "Gamma", "Delta", "Epsilon", "Zeta", "Eta"} {
		p := e.project(e.admin, name, "")
		if p.Look != LookGenerated || p.Icon == "" || p.Background == "" || !slices.Contains(lookColors, p.Color) {
			t.Fatalf("%s: look=%q icon=%q color=%q bg=%q", name, p.Look, p.Icon, p.Color, p.Background)
		}
		colors[p.Color], bgs[p.Background], icons[p.Icon] = true, true, true
	}
	if len(colors) != 7 || len(bgs) != 7 || len(icons) != 7 {
		t.Errorf("not all different: %d colours, %d backgrounds, %d icons", len(colors), len(bgs), len(icons))
	}
	n, c := "Mine", "#ef4444"
	mine, err := e.s.CreateProject(e.ctx, e.admin, ProjectInput{Name: &n, Color: &c})
	if err != nil || mine.Look != LookCustom || mine.Icon != "" || mine.Background != "" || mine.Color != c {
		t.Fatalf("explicit colour: %v %+v", err, mine)
	}
}

func TestSweepLooksStylesAutoOnly(t *testing.T) {
	e := newEnv(t)
	old := e.project(e.admin, "Old", "")
	if _, err := e.s.db.W.ExecContext(e.ctx, `UPDATE projects SET look='auto', icon='', background='' WHERE id=?`, old.ID); err != nil {
		t.Fatal(err)
	}
	icon := "🎸"
	mine := e.project(e.admin, "Band", "")
	if _, err := e.s.UpdateProject(e.ctx, e.admin, mine.Key, ProjectInput{Icon: &icon}); err != nil {
		t.Fatal(err)
	}
	plain := e.project(e.admin, "Plain", "")
	if _, err := e.s.ResetLook(e.ctx, e.admin, plain.Key); err != nil {
		t.Fatal(err)
	}
	e.s.sweepLooks(e.ctx)
	if p := lookOf(e, old.Key); p.Look != LookGenerated || p.Icon == "" || p.Background == "" {
		t.Errorf("auto project not styled: %+v", p)
	}
	if p := lookOf(e, mine.Key); p.Look != LookCustom || p.Icon != "🎸" {
		t.Errorf("custom look changed: %+v", p)
	}
	if p := lookOf(e, plain.Key); p.Look != LookPlain || p.Icon != "" || p.Background != "" {
		t.Errorf("plain look changed: %+v", p)
	}
	before := lookOf(e, old.Key)
	e.s.sweepLooks(e.ctx)
	if got := lookOf(e, old.Key); got.Icon != before.Icon || got.Color != before.Color {
		t.Errorf("sweep restyled a styled project")
	}
}

func TestProjectLookCustomRegenerateReset(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Garden", "")
	cur := lookOf(e, p.Key)
	if cur.Look != LookGenerated {
		t.Fatal("not generated")
	}
	// the form sends the look on every save, so an unchanged save must keep it generated
	if got, err := e.s.UpdateProject(e.ctx, e.admin, p.Key, ProjectInput{Icon: &cur.Icon, Color: &cur.Color, Background: &cur.Background}); err != nil || got.Look != LookGenerated {
		t.Fatalf("resaving the same look: %v %q", err, got.Look)
	}
	icon := "🌻"
	got, err := e.s.UpdateProject(e.ctx, e.admin, p.Key, ProjectInput{Icon: &icon})
	if err != nil || got.Look != LookCustom || got.Icon != "🌻" {
		t.Fatalf("person's change: %v %+v", err, got)
	}
	e.s.sweepLooks(e.ctx)
	if got := lookOf(e, p.Key); got.Icon != "🌻" || got.Look != LookCustom {
		t.Fatalf("a person's look was replaced: %+v", got)
	}

	sam := e.user("sam", false)
	if _, err := e.s.RegenerateLook(e.ctx, sam, p.Key); err == nil {
		t.Error("a non-member regenerated a look")
	}
	if _, err := e.s.ResetLook(e.ctx, sam, p.Key); err == nil {
		t.Error("a non-member reset a look")
	}
	prev := lookOf(e, p.Key)
	for i := 0; i < 4; i++ {
		next, err := e.s.RegenerateLook(e.ctx, e.admin, p.Key)
		if err != nil || next.Look != LookGenerated {
			t.Fatalf("pick another: %v %+v", err, next)
		}
		if next.Icon == prev.Icon || next.Color == prev.Color || next.Background == prev.Background {
			t.Errorf("pick another kept a part: %s %s %s -> %s %s %s", prev.Icon, prev.Color, prev.Background, next.Icon, next.Color, next.Background)
		}
		prev = next
	}

	got, err = e.s.ResetLook(e.ctx, e.admin, p.Key)
	if err != nil || got.Look != LookPlain || got.Icon != "" || got.Background != "" || got.Color != defaultLook(p.Key).Color {
		t.Fatalf("reset: %v %+v", err, got)
	}
	e.s.sweepLooks(e.ctx)
	if lookOf(e, p.Key).Look != LookPlain {
		t.Error("a reset look was styled again without Pick another")
	}
	got, err = e.s.RegenerateLook(e.ctx, e.admin, p.Key)
	if err != nil || got.Look != LookGenerated {
		t.Fatalf("pick another after reset: %v %+v", err, got)
	}
}

// A person's edit made while the sweep was working beats the automatic pick.
func TestProjectLookEditDuringSweep(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Race", "")
	icon := "🎸"
	if _, err := e.s.UpdateProject(e.ctx, e.admin, p.Key, ProjectInput{Icon: &icon}); err != nil {
		t.Fatal(err)
	}
	changed, err := e.s.saveLook(e.ctx, SystemActor(), p, lookChoice{Icon: "🚀", Color: "#14b8a6", Background: "ocean"}, LookGenerated, "picked", true)
	if err != nil || changed {
		t.Fatalf("saveLook onlyAuto over a custom look: changed=%v err=%v", changed, err)
	}
	if got := lookOf(e, p.Key); got.Icon != "🎸" || got.Look != LookCustom {
		t.Errorf("look = %+v", got)
	}
}
