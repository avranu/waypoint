// Package depage keeps new dependency versions at least a week behind their
// release (WP-844): a hijacked release is usually caught by others within
// days, so a version that has been public for a week is much less likely to
// be one. cmd/dep-age runs it in CI's lint job and the pre-push hook.
//
// It compares two trees (a pull request's head and main): every Go module
// version in go.sum, every npm package version in a package-lock.json, and
// every Go or npm pin a renovate.json regex manager finds (the Makefile's
// tools, pre-commit's prettier, deploy/opencode's opencode-ai) that the head
// has and the base doesn't must be old enough, or be waived in
// .dep-age-waivers with a reason. Lockfiles count, so a transitive version
// is checked like a direct one.
//
// How old a version is:
//   - npm: the registry's publish time (the packument's "time"), which the
//     publisher can't set.
//   - Go: the module proxy's .info Time is the commit's time, which whoever
//     tags the release can backdate. So a Go version must also be in Go's
//     checksum database (sum.golang.org) before a record that was new a
//     week ago: the database's record numbers only grow, and the module
//     index (index.golang.org) says which version the proxy first saw just
//     after the cutoff.
//
// Not covered: CI actions (pinned by commit SHA, which carries no trusted
// date), Docker base images, and the Go toolchain.
package depage

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Ecosystems.
const (
	Go  = "go"
	NPM = "npm"
)

// WaiverFile is where a stated reason lets a version through early.
const WaiverFile = ".dep-age-waivers"

// Dep is one version of one package.
type Dep struct {
	Eco     string // Go or NPM
	Name    string // module path or npm package name
	Version string
	Where   string // the file it was found in (the first, if several)
}

// Key identifies a dep regardless of where it was found.
func (d Dep) Key() string { return d.Eco + " " + d.Name + " " + d.Version }

func (d Dep) String() string { return d.Eco + " " + d.Name + "@" + d.Version }

// Tree is one version of the repository: its file paths and a way to read
// them. Read returns nil, nil for a file the tree doesn't have.
type Tree struct {
	Files []string
	Read  func(path string) ([]byte, error)
}

// Collect returns every dep the tree pins, deduplicated, in a stable order.
// rules are the renovate.json regex managers to apply (see ParseRenovate).
func Collect(t Tree, rules []Rule) ([]Dep, error) {
	seen := map[string]bool{}
	var out []Dep
	add := func(d Dep) {
		if d.Name == "" || d.Version == "" || seen[d.Key()] {
			return
		}
		seen[d.Key()] = true
		out = append(out, d)
	}
	for _, f := range t.Files {
		base := f[strings.LastIndex(f, "/")+1:]
		var rs []Rule
		for _, r := range rules {
			if r.Files.MatchString(f) {
				rs = append(rs, r)
			}
		}
		if base != "go.sum" && base != "package-lock.json" && len(rs) == 0 {
			continue
		}
		b, err := t.Read(f)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", f, err)
		}
		if b == nil {
			continue
		}
		switch base {
		case "go.sum":
			for _, d := range ParseGoSum(b) {
				d.Where = f
				add(d)
			}
		case "package-lock.json":
			ds, err := ParseLock(b)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", f, err)
			}
			for _, d := range ds {
				d.Where = f
				add(d)
			}
		}
		for _, r := range rs {
			for _, d := range r.Find(b) {
				d.Where = f
				add(d)
			}
		}
	}
	return out, nil
}

// New returns the deps head has that base doesn't.
func New(head, base []Dep) []Dep {
	had := map[string]bool{}
	for _, d := range base {
		had[d.Key()] = true
	}
	var out []Dep
	for _, d := range head {
		if !had[d.Key()] {
			out = append(out, d)
		}
	}
	return out
}

// ParseGoSum returns the module versions in a go.sum. A "/go.mod" line (the
// version's go.mod only, read while resolving the build list) counts too:
// it is the same version, and a hijacked go.mod could pull in others.
func ParseGoSum(b []byte) []Dep {
	var out []Dep
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 3 {
			continue
		}
		out = append(out, Dep{Eco: Go, Name: f[0], Version: strings.TrimSuffix(f[1], "/go.mod")})
	}
	return out
}

// ParseLock returns the registry packages in an npm package-lock.json
// (lockfileVersion 2 or 3). Workspace links and the root are skipped, and so
// are packages bundled inside another's tarball (inBundle): they arrive with
// the package that bundles them, whose own age is checked. A package from a
// git repository or a URL other than a registry tarball has no publish time
// to check, so it comes back with Version prefixed "unverifiable:".
func ParseLock(b []byte) ([]Dep, error) {
	var lock struct {
		LockfileVersion int `json:"lockfileVersion"`
		Packages        map[string]struct {
			Name     string `json:"name"`
			Version  string `json:"version"`
			Resolved string `json:"resolved"`
			Link     bool   `json:"link"`
			InBundle bool   `json:"inBundle"`
		} `json:"packages"`
	}
	if err := json.Unmarshal(b, &lock); err != nil {
		return nil, err
	}
	if lock.LockfileVersion < 2 {
		return nil, fmt.Errorf("lockfileVersion %d: only 2 and 3 list every package (run npm install with npm 7 or later)", lock.LockfileVersion)
	}
	keys := make([]string, 0, len(lock.Packages))
	for k := range lock.Packages {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var out []Dep
	for _, k := range keys {
		p := lock.Packages[k]
		i := strings.LastIndex(k, "node_modules/")
		if i < 0 || p.Link || p.InBundle || p.Version == "" {
			continue
		}
		name := k[i+len("node_modules/"):]
		if p.Name != "" {
			name = p.Name
		}
		v := p.Version
		if p.Resolved != "" && p.Resolved != registryTarball(name, p.Version) {
			v = "unverifiable:" + p.Resolved
		}
		out = append(out, Dep{Eco: NPM, Name: name, Version: v})
	}
	return out, nil
}

// registryTarball is the npm registry's tarball URL for name@version
// (https://registry.npmjs.org/@scope/pkg/-/pkg-1.2.3.tgz). A lockfile entry
// resolved anywhere else (another host, a repository, a local file, or
// another package's or version's tarball) installs something other than the
// name@version whose publish time the registry gives, so it is unverifiable.
func registryTarball(name, version string) string {
	return "https://registry.npmjs.org/" + name + "/-/" + name[strings.LastIndex(name, "/")+1:] + "-" + version + ".tgz"
}

// Rule is one renovate.json regex manager whose datasource is Go or npm.
type Rule struct {
	Files   *regexp.Regexp
	Matches []*regexp.Regexp
	Eco     string
	Name    string // depNameTemplate, when the match has no depName
}

// Find returns the deps a rule's match strings find in b.
func (r Rule) Find(b []byte) []Dep {
	var out []Dep
	for _, re := range r.Matches {
		ni, vi := re.SubexpIndex("depName"), re.SubexpIndex("currentValue")
		for _, m := range re.FindAllSubmatch(b, -1) {
			d := Dep{Eco: r.Eco, Name: r.Name}
			if ni > 0 && len(m[ni]) > 0 {
				d.Name = string(m[ni])
			}
			if vi > 0 {
				d.Version = string(m[vi])
			}
			out = append(out, d)
		}
	}
	return out
}

// RenovateRules returns the go and npm regex managers of every tree's
// renovate.json. Collect head and base with base's rules too, so a change
// that drops or narrows a rule can't hide the pin it bumps alongside.
func RenovateRules(trees ...Tree) ([]Rule, error) {
	var out []Rule
	for _, t := range trees {
		b, err := t.Read("renovate.json")
		if err != nil {
			return nil, err
		}
		if b == nil {
			continue
		}
		rs, err := ParseRenovate(b)
		if err != nil {
			return nil, fmt.Errorf("renovate.json: %w", err)
		}
		out = append(out, rs...)
	}
	return out, nil
}

// ParseRenovate returns renovate.json's regex managers for the go and npm
// datasources. Others (docker, github-tags, …) have no check here.
func ParseRenovate(b []byte) ([]Rule, error) {
	var cfg struct {
		CustomManagers []struct {
			CustomType          string   `json:"customType"`
			ManagerFilePatterns []string `json:"managerFilePatterns"`
			MatchStrings        []string `json:"matchStrings"`
			DepNameTemplate     string   `json:"depNameTemplate"`
			DatasourceTemplate  string   `json:"datasourceTemplate"`
		} `json:"customManagers"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, err
	}
	var out []Rule
	for i, m := range cfg.CustomManagers {
		eco := map[string]string{"go": Go, "npm": NPM}[m.DatasourceTemplate]
		if m.CustomType != "regex" || eco == "" {
			continue
		}
		if len(m.ManagerFilePatterns) == 0 {
			continue // Renovate reads no files for it; an empty regexp would match every one
		}
		var files []string
		for _, p := range m.ManagerFilePatterns {
			if len(p) < 2 || p[0] != '/' || p[len(p)-1] != '/' {
				return nil, fmt.Errorf("customManagers[%d]: file pattern %q isn't a /regex/; only those are read here", i, p)
			}
			files = append(files, "(?:"+p[1:len(p)-1]+")")
		}
		fre, err := regexp.Compile(strings.Join(files, "|"))
		if err != nil {
			return nil, fmt.Errorf("customManagers[%d]: %w", i, err)
		}
		r := Rule{Files: fre, Eco: eco, Name: m.DepNameTemplate}
		for _, s := range m.MatchStrings {
			re, err := regexp.Compile(s)
			if err != nil {
				return nil, fmt.Errorf("customManagers[%d]: %w", i, err)
			}
			if re.SubexpIndex("currentValue") < 0 {
				return nil, fmt.Errorf("customManagers[%d]: match string %q has no currentValue group", i, s)
			}
			r.Matches = append(r.Matches, re)
		}
		out = append(out, r)
	}
	return out, nil
}

// Waiver lets one version through before it is old enough.
type Waiver struct {
	Line   int
	Reason string
}

// ParseWaivers reads WaiverFile: one waiver per line, "ECO NAME VERSION
// REASON…" (eco is go or npm), with # comments and blank lines ignored. A
// waiver without a reason is an error: the reason is the point.
func ParseWaivers(b []byte) (map[string]Waiver, error) {
	out := map[string]Waiver{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 4 || (f[0] != Go && f[0] != NPM) {
			return nil, fmt.Errorf("%s:%d: want \"go|npm NAME VERSION REASON\", got %q", WaiverFile, n, line)
		}
		d := Dep{Eco: f[0], Name: f[1], Version: f[2]}
		out[d.Key()] = Waiver{Line: n, Reason: strings.Join(f[3:], " ")}
	}
	return out, sc.Err()
}
