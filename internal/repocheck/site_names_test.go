package repocheck

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"unicode"
)

// siteBaseline is how many site names a file may still hold, and why. A
// file not listed may hold none. Lower a count when a file loses some; the
// test only fails when one gains.
var siteBaseline = map[string]int{
	// This instance's working rules: an example of a decision's approval.
	"AGENTS.md": 1,
}

var siteWordRE = regexp.MustCompile(`[A-Za-z0-9]+(?:\.[A-Za-z0-9]+)*`)

type siteHit struct {
	line int
	word string
}

// siteNames are the names to keep out of the tree, from SITE_NAMES: words
// separated by commas or spaces, such as a maintainer's name, a domain, host
// names, other projects' names and the first three parts of a home network's
// IPv4 range (10.1.2). A word ending in "-" is another project's issue key
// (APP-): it matches only in a code (APP-12, APP#12, APP-PL13, APP:R-3), so
// a key that is also a word (MAP-) doesn't flag every "map" (WP-1184). A
// word with a hyphen inside (tumble-weed) matches as that phrase. They are a
// site's settings, so like its other settings they come from the
// environment and are never committed: in CI the SITE_NAMES secret, locally
// the environment or deploy/site.mk (gitignored), which the Makefile exports.
func siteNames() map[string]bool {
	names := map[string]bool{}
	sep := func(r rune) bool { return r == ',' || unicode.IsSpace(r) }
	for _, w := range strings.FieldsFunc(os.Getenv("SITE_NAMES"), sep) {
		names[strings.ToLower(w)] = true
	}
	return names
}

// siteChecker finds names, given in lower case, in text.
type siteChecker struct {
	names map[string]bool
	// phrases matches the names that aren't one word: issue keys as task
	// codes, and hyphenated names. Nil when there are none.
	phrases *regexp.Regexp
}

func isAlnum(b byte) bool { return isLower(b) || isUpper(b) || '0' <= b && b <= '9' }

// phraseAlone reports whether line[i:j] stands apart from the text around
// it: no letter or digit right next to it, and no dotted number before or
// after it (1.2.ab-4 and graceful-fs-4.2.11 are versions). A hyphen or
// underscore around it doesn't join it to a word, so a code in a slug
// (ab-12-fix, worktree-ab-12, _AB-12_) and a host name with a suffix
// (box-one-01) still count.
func phraseAlone(line string, i, j int) bool {
	isDigit := func(k int) bool { return 0 <= k && k < len(line) && '0' <= line[k] && line[k] <= '9' }
	if i > 0 && (isAlnum(line[i-1]) || line[i-1] == '.' && isDigit(i-2)) {
		return false
	}
	return j == len(line) || !isAlnum(line[j]) && (line[j] != '.' || !isDigit(j+1))
}

func newSiteChecker(names map[string]bool) *siteChecker {
	c := &siteChecker{names: map[string]bool{}}
	var keys, phrases []string
	for n := range names {
		// A leading hyphen means nothing: -ab is ab.
		n = strings.TrimLeft(n, "-")
		switch stem := strings.TrimRight(n, "-"); {
		case stem == "":
		case stem != n: // a key, even one with a hyphen in it (my-app-)
			keys = append(keys, regexp.QuoteMeta(stem))
		case strings.Contains(n, "-"):
			phrases = append(phrases, regexp.QuoteMeta(n))
		default:
			c.names[n] = true
		}
	}
	sort.Strings(keys)
	sort.Strings(phrases)
	var alts []string
	if len(keys) > 0 {
		// A code: KEY-12, KEY#12 (a pull request), KEY-PL13 or KEY-ADR7 /
		// KEY-ADR-0007 (a page or a decision), KEY:R-3 (a request; not
		// group-open:rotate-90, a style).
		alts = append(alts, `(?:`+strings.Join(keys, "|")+`)(?:[-#]?[0-9]+|-?(?:adr-?|[a-z]{1,2})[0-9]+|:[a-z]{1,3}-?[0-9]+)`)
	}
	if len(phrases) > 0 {
		alts = append(alts, `(?:`+strings.Join(phrases, "|")+`)`)
	}
	if len(alts) > 0 {
		c.phrases = regexp.MustCompile(`(?i)` + strings.Join(alts, "|"))
	}
	return c
}

// hits finds the names in text: at most one a word. A camelCase word is
// also read as its humps, so a name inside an identifier (nameFile,
// @nameAI) counts.
func (c *siteChecker) hits(text []byte) []siteHit {
	var hits []siteHit
	sc := bufio.NewScanner(bytes.NewReader(text))
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for n := 1; sc.Scan(); n++ {
		line := sc.Text()
		for _, w := range siteWordRE.FindAllString(line, -1) {
			if c.word(strings.ToLower(w)) || c.humps(w) {
				hits = append(hits, siteHit{n, w})
			}
		}
		if c.phrases == nil {
			continue
		}
		for _, m := range c.phrases.FindAllStringIndex(line, -1) {
			if phraseAlone(line, m[0], m[1]) {
				hits = append(hits, siteHit{n, line[m[0]:m[1]]})
			}
		}
	}
	return hits
}

// humps reports whether a camelCase word has a name as one of its humps:
// a lower-case letter followed by an upper-case one starts a new hump.
func (c *siteChecker) humps(w string) bool {
	start, split := 0, false
	for i := 1; i <= len(w); i++ {
		if i < len(w) && (!isLower(w[i-1]) || !isUpper(w[i])) {
			continue
		}
		if i == len(w) && !split {
			return false
		}
		if c.word(strings.ToLower(w[start:i])) {
			return true
		}
		start, split = i, true
	}
	return false
}

func isLower(b byte) bool { return 'a' <= b && b <= 'z' }
func isUpper(b byte) bool { return 'A' <= b && b <= 'Z' }

// word reports whether a lower-case word is a name: as a whole, as any run
// of its dotted parts with a letter in it, or, for an IPv4 address, as its
// first three parts.
func (c *siteChecker) word(w string) bool {
	is := func(s string) bool { return c.names[s] }
	parts := strings.Split(w, ".")
	if len(parts) == 4 && allDigits(parts) {
		return is(strings.Join(parts[:3], "."))
	}
	if len(parts) > 6 {
		return is(w)
	}
	for i := range parts {
		for j := i + 1; j <= len(parts); j++ {
			if !allDigits(parts[i:j]) && is(strings.Join(parts[i:j], ".")) {
				return true
			}
		}
	}
	return false
}

func allDigits(parts []string) bool {
	for _, p := range parts {
		if strings.Trim(p, "0123456789") != "" {
			return false
		}
	}
	return true
}

// TestNoSiteNames keeps the tree free of the names of the site Waypoint runs
// on (WP-459), as SITE_NAMES lists them; without SITE_NAMES it has nothing to
// check. A file over its count in siteBaseline fails with each line that
// names one. Migrations are read too: the runner keys them by file name, so
// a comment in one can change after it ran.
func TestNoSiteNames(t *testing.T) {
	names := siteNames()
	if len(names) == 0 {
		t.Skip("SITE_NAMES is not set: no site names to look for")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("git", "-C", root, "ls-files", "-z").Output()
	if err != nil {
		t.Skipf("not a git checkout (%v): nothing to check", err)
	}
	mod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	// The module path is every import's prefix, not a stray mention: it
	// changes by renaming the module (go mod edit -module), so it isn't flagged.
	module := strings.TrimSpace(strings.TrimPrefix(strings.SplitN(string(mod), "\n", 2)[0], "module"))
	check := newSiteChecker(names)
	counts := map[string]int{}
	var problems []string
	for _, path := range strings.Split(strings.TrimRight(string(out), "\x00"), "\x00") {
		text, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil || bytes.IndexByte(text, 0) >= 0 {
			continue // deleted in the working tree, a submodule, or binary
		}
		if module != "" {
			text = bytes.ReplaceAll(text, []byte(module), nil)
		}
		hits := check.hits(text)
		if len(hits) == 0 {
			continue
		}
		counts[path] = len(hits)
		if allowed := siteBaseline[path]; len(hits) > allowed {
			p := fmt.Sprintf("%s has %d, %d allowed:", path, len(hits), allowed)
			for _, h := range hits {
				p += fmt.Sprintf("\n\t%s:%d: %s", path, h.line, h.word)
			}
			problems = append(problems, p)
		}
	}
	if os.Getenv("REPOCHECK_COUNTS") != "" {
		paths := make([]string, 0, len(counts))
		for p := range counts {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		for _, p := range paths {
			t.Logf("%q: %d,", p, counts[p])
		}
	}
	if len(problems) > 0 {
		t.Errorf("these files name the site Waypoint runs on: the maintainer, their domain, hosts, "+
			"other projects or home network, as SITE_NAMES lists them (WP-459). Use a stand-in (Robin, git.example.net, prodhost, "+
			"runner1, 10.9.0.0/24) or a setting instead. A file that must keep one gets a count and a "+
			"reason in siteBaseline (internal/repocheck/site_names_test.go).\n%s", strings.Join(problems, "\n"))
	}
}

func TestSiteWord(t *testing.T) {
	t.Setenv("SITE_NAMES", "Tumbleweed, example.org\n10.77.0")
	c := newSiteChecker(siteNames())
	for w, want := range map[string]bool{
		"tumbleweed":               true,
		"git.example.net":          false,
		"waypoint.example.org":     true,
		"10.77.0.216":              true,
		"10.77.0":                  false, // a version, not an address
		"v1.10.77.0":               false,
		"10.77.1.1":                false,
		"robin":                    false,
		"a.b.c.d.e.f.g.tumbleweed": false, // too many parts to try every run
	} {
		if got := c.word(w); got != want {
			t.Errorf("word(%q) = %v, want %v", w, got, want)
		}
	}
	hits := c.hits([]byte("one\nRobin and Tumbleweed\nnone\n"))
	if len(hits) != 1 || hits[0].line != 2 || hits[0].word != "Tumbleweed" {
		t.Errorf("hits %+v", hits)
	}
	for w, want := range map[string]bool{
		"tumbleweedFile": true,
		"newTumbleweed":  true,
		"atumbleweedAI":  false,
		"TUMBLEWEEDAI":   false,
		"Tumbleweeds":    false,
	} {
		if got := len(c.hits([]byte(w))) == 1; got != want {
			t.Errorf("hits(%q) = %v, want %v", w, got, want)
		}
	}
}

// A project's key flags its task codes, not the word; a hyphenated name
// flags as a phrase (WP-1184).
func TestSiteKeysAndPhrases(t *testing.T) {
	t.Setenv("SITE_NAMES", "MAP-, ab- tumble-weed my-app- -zz")
	c := newSiteChecker(siteNames())
	for text, want := range map[string]string{
		"see MAP-153.":             "MAP-153",
		"(map-12)":                 "map-12",
		"XY#11 and AB#4":           "AB#4",
		"AB:R-28 waits":            "AB:R-28",
		"AB-PL13 and ab-adr7":      "AB-PL13,ab-adr7",
		"see AB12 and ABPL13":      "AB12,ABPL13",
		"AB-ADR-0007":              "AB-ADR-0007",
		"ab-ext4":                  "",
		"`wp show ab-3`":           "ab-3",
		"MAP-1 MAP-2":              "MAP-1,MAP-2",
		"a map of the town":        "",
		"MAP-reading":              "",
		"remap-12":                 "",
		"the MAP- prefix":          "",
		"x-map-12":                 "map-12",
		"map-12-b":                 "map-12",
		"AB-12-14":                 "AB-12",
		"ab-12-AB-14":              "ab-12,AB-14",
		"branch wp-9-ab-12-fix":    "ab-12",
		"_AB-12_ in italics":       "AB-12",
		"AB-12abc":                 "",
		"graceful-ab-4.2.11":       "",
		"see AB-4.":                "AB-4",
		"group-ab:rotate-90":       "",
		"see AB-ADR-0007":          "AB-ADR-0007",
		"my-app-3 is a code":       "my-app-3",
		"a my-app name":            "",
		"zz top":                   "zz",
		"tumble-weed-01.lan":       "tumble-weed",
		"tumble-weed_backup":       "tumble-weed",
		"tumble-weed grows":        "tumble-weed",
		"tumble-weeds":             "",
		"a tumble weed":            "",
		"version 1.2.ab-4":         "",
		"TAB-12 is another key":    "",
		"WP-12 stays: it is ours.": "",
	} {
		var got []string
		for _, h := range c.hits([]byte(text)) {
			got = append(got, h.word)
		}
		if strings.Join(got, ",") != want {
			t.Errorf("hits(%q) = %v, want %q", text, got, want)
		}
	}
}
