package broker

import (
	"crypto/x509"
	"encoding/json"
	"errors"
	"net/netip"
	"testing"
)

func TestPathMatch(t *testing.T) {
	for _, tc := range []struct {
		pattern, path string
		want          bool
	}{
		{"/**", "/", true},
		{"/**", "/a/b", true},
		{"/api/v1/issues", "/api/v1/issues", true},
		{"/api/v1/issues", "/api/v1/issues/WP-1", false},
		{"/api/v1/issues/**", "/api/v1/issues", true},
		{"/api/v1/issues/**", "/api/v1/issues/WP-1/comments", true},
		{"/api/v1/issues/**", "/api/v1/issuesX", false},
		{"/*/*/git-upload-pack", "/o/r.git/git-upload-pack", true},
		{"/*/*/git-upload-pack", "/o/git-upload-pack", false},
		{"/*/*/git-upload-pack", "/o//git-upload-pack", false},
		{"/a", "a", false},
	} {
		if got := pathMatch(tc.pattern, tc.path); got != tc.want {
			t.Errorf("pathMatch(%q, %q) = %v", tc.pattern, tc.path, got)
		}
	}
}

func TestCleanPath(t *testing.T) {
	for p, want := range map[string]bool{
		"/":                      true,
		"/a/b/":                  true,
		"/a/../b":                false,
		"/a/./b":                 false,
		"/a//b":                  false,
		"/a/%2e%2e/b":            false,
		"/a%2Fb":                 false,
		"/a%5cb":                 false,
		"/a\\b":                  false,
		"/@scope%2fpkg":          false,
		"/api/v1/p/WP/%73andbox": false,
		"/a/%6Dembers":           false,
		"/a/b%":                  false,
		"/a/b%zz":                false,
		"/a%20b":                 true,
		"/golang.org/x/@v/list":  true,
	} {
		if got := cleanPath(p); got != want {
			t.Errorf("cleanPath(%q) = %v", p, got)
		}
	}
}

func TestGoTemplate(t *testing.T) {
	rules := Go([]string{"github.com/BurntSushi/toml", "golang.org/x/net"})
	proxy, sum := &rules[0], &rules[1]
	for _, tc := range []struct {
		rule         *Rule
		method, path string
		want         string // "" allowed, else the reason
	}{
		{proxy, "GET", "/golang.org/x/net/@v/list", ""},
		{proxy, "GET", "/golang.org/x/net/@v/v0.30.0.zip", ""},
		{proxy, "GET", "/golang.org/x/net/html/@v/v0.30.0.mod", ""}, // a package path under the module
		{proxy, "GET", "/golang.org/x/net/@latest", ""},
		{proxy, "GET", "/github.com/!burnt!sushi/toml/@v/v1.4.0.info", ""},
		{proxy, "GET", "/github.com/BurntSushi/toml/@v/v1.4.0.info", "path"}, // never sent unescaped
		{proxy, "GET", "/golang.org/toolchain/@v/v0.0.1-go1.27.3.linux-amd64.zip", ""},
		{proxy, "GET", "/golang.org/x/netx/@v/list", "module"},
		{proxy, "GET", "/evil.example.com/leak-the-secret/@v/list", "module"},
		{proxy, "GET", "/golang.org/x/net/@v/../../evil/@v/list", "path"},
		{proxy, "GET", "/golang.org/x/net/@v/v1.zip/extra", "path"},
		{proxy, "POST", "/golang.org/x/net/@v/list", "method"},
		{proxy, "GET", "/sumdb/sum.golang.org/supported", ""},
		{proxy, "GET", "/sumdb/sum.golang.org/lookup/golang.org/x/net@v0.30.0", ""},
		{proxy, "GET", "/sumdb/sum.golang.org/lookup/evil.example.com/x@v1.0.0", "module"},
		{proxy, "GET", "/", "path"},
		{sum, "GET", "/latest", ""},
		{sum, "GET", "/tile/8/0/x123/456", ""},
		{sum, "GET", "/lookup/golang.org/x/net@v0.30.0", ""},
		{sum, "GET", "/lookup/evil.example.com/x@v1.0.0", "module"},
		{sum, "GET", "/lookup/@v1", "path"},
	} {
		d := tc.rule.check(tc.method, tc.path, "")
		if (tc.want == "" && !d.ok) || (tc.want != "" && (d.ok || d.reason != tc.want)) {
			t.Errorf("%s %s %s: ok=%v reason=%q, want %q", tc.rule.Host, tc.method, tc.path, d.ok, d.reason, tc.want)
		}
	}
	gcs := &rules[3]
	if !gcs.check("GET", "/golang/go1.27.3.linux-amd64.tar.gz", "").ok || gcs.check("GET", "/other-bucket/x", "").ok {
		t.Error("storage.googleapis.com is limited to /golang/")
	}
}

func TestForgeAndGitHubTemplates(t *testing.T) {
	forge := Forge(ForgeConfig{Host: "git.example.org", Repos: []string{"Robin/waypoint"}, Cred: "forge", Pin: []string{"10.9.0.31"}})[0]
	for _, tc := range []struct {
		method, path, query string
		ok                  bool
	}{
		{"GET", "/Robin/waypoint.git/info/refs", "service=git-upload-pack", true},
		{"POST", "/Robin/waypoint.git/git-upload-pack", "", true},
		{"GET", "/Robin/waypoint/info/refs", "service=git-upload-pack", true},
		{"GET", "/Robin/waypoint.git/info/refs", "service=git-receive-pack", false},
		{"POST", "/Robin/waypoint.git/git-receive-pack", "", false},
		{"GET", "/Robin/other.git/info/refs", "service=git-upload-pack", false},
		{"GET", "/api/v1/repos/Robin/waypoint", "", false},
		{"POST", "/api/v1/repos/Robin/waypoint/statuses/abc", "", false},
	} {
		if d := forge.check(tc.method, tc.path, tc.query); d.ok != tc.ok {
			t.Errorf("forge %s %s?%s: %v, want %v", tc.method, tc.path, tc.query, d.ok, tc.ok)
		}
	}
	if Forge(ForgeConfig{Host: "git.example.org"}) != nil {
		t.Error("a forge rule with no repositories")
	}

	gh := GitHub()
	if !gh[0].check("POST", "/golang/go/git-upload-pack", "").ok {
		t.Error("git fetch from GitHub refused")
	}
	if gh[0].check("POST", "/o/r/git-receive-pack", "").ok {
		t.Error("git push to GitHub allowed")
	}
	if gh[1].check("POST", "/repos/o/r/issues", "").ok {
		t.Error("POST to api.github.com allowed")
	}
}

func TestBuiltInTemplatesValidate(t *testing.T) {
	var rules []Rule
	for _, rs := range [][]Rule{
		Anthropic(), Go(nil), NPM(), PyPI(), DockerHub(), Ubuntu(), Playwright(), GitHub(),
		Forge(ForgeConfig{Host: "git.example.org", Repos: []string{"o/r"}, Cred: "forge"}),
		Waypoint(WaypointConfig{Host: "waypoint.example.org", Cred: "waypoint"}),
	} {
		rules = append(rules, rs...)
	}
	p := Policy{Rules: rules}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	// A policy survives a round trip through JSON, as it will in a spec.
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var back Policy
	if err := json.Unmarshal(b, &back); err != nil || back.Validate() != nil || len(back.Rules) != len(rules) {
		t.Fatalf("round trip: %v", err)
	}
	ca, _ := NewCA("t")
	if _, err := New(Config{Policy: p, CA: ca}); !errors.Is(err, ErrPolicy) {
		t.Errorf("credentials named but not configured: %v", err)
	}
	creds := map[string]Credential{"forge": {"Authorization", "Basic x"}, "waypoint": {"Authorization", "Bearer y"}}
	if _, err := New(Config{Policy: p, Creds: creds}); !errors.Is(err, ErrPolicy) {
		t.Errorf("inspected hosts without a CA: %v", err)
	}
	if _, err := New(Config{Policy: p, CA: ca, Creds: creds}); err != nil {
		t.Error(err)
	}
}

func TestPolicyValidate(t *testing.T) {
	for name, r := range map[string]Rule{
		"upper case":         {Host: "Example.org"},
		"IP literal":         {Host: "10.0.0.1"},
		"wildcard":           {Host: "*.example.org"},
		"single label":       {Host: "localhost"},
		"tunnel with routes": {Host: "a.example.org", Mode: Tunnel, Routes: []Route{{Methods: []string{"GET"}, Path: "/**"}}},
		"tunnel with cred":   {Host: "a.example.org", Mode: Tunnel, Cred: "x"},
		"cred over http":     {Host: "a.example.org", HTTP: true, Cred: "x"},
		"bad mode":           {Host: "a.example.org", Mode: "open"},
		"go without modules": {Host: "proxy.golang.org", Kind: KindGoProxy},
		"bad module":         {Host: "proxy.golang.org", Kind: KindGoProxy, Modules: []string{"../x"}},
		"route without /":    {Host: "a.example.org", Routes: []Route{{Methods: []string{"GET"}, Path: "x"}}},
		"** in the middle":   {Host: "a.example.org", Routes: []Route{{Methods: []string{"GET"}, Path: "/**/x"}}},
		"CONNECT route":      {Host: "a.example.org", Routes: []Route{{Methods: []string{"CONNECT"}, Path: "/**"}}},
		"bad pin":            {Host: "a.example.org", Pin: []string{"git.example.org"}},
	} {
		p := Policy{Rules: []Rule{r}}
		if err := p.Validate(); !errors.Is(err, ErrPolicy) {
			t.Errorf("%s: %v", name, err)
		}
	}
	dup := Policy{Rules: []Rule{{Host: "a.example.org"}, {Host: "a.example.org", Port: 443}}}
	if dup.Validate() == nil {
		t.Error("two rules for one host:port")
	}
	ok := Policy{Rules: []Rule{{Host: "a.example.org"}, {Host: "a.example.org", HTTP: true}}}
	if err := ok.Validate(); err != nil {
		t.Errorf("https and http rules for one host: %v", err)
	}
}

func TestPublicAddr(t *testing.T) {
	for a, want := range map[string]bool{
		"1.1.1.1":                              true,
		"2606:4700::1111":                      true,
		"127.0.0.1":                            false,
		"10.9.0.31":                            false,
		"172.16.5.4":                           false,
		"192.168.1.1":                          false,
		"100.100.1.1":                          false, // tailnet
		"169.254.169.254":                      false, // metadata
		"0.0.0.0":                              false,
		"255.255.255.255":                      false,
		"224.0.0.1":                            false,
		"198.18.0.1":                           false,
		"::1":                                  false,
		"fe80::1":                              false,
		"fd7a:115c:a1e0::1":                    false, // tailnet ULA
		"::ffff:127.0.0.1":                     false,
		"64:ff9b::a02:1f":                      false, // NAT64 of 10.9.0.31
		"64:ff9b::101:101":                     true,  // NAT64 of 1.1.1.1
		"2002:a02:1f::1":                       false, // 6to4 of 10.9.0.31
		"2001:0:4136:e378:8000:63bf:f5fd:ffe0": false, // Teredo of 10.9.0.31
	} {
		if got := publicAddr(netip.MustParseAddr(a)); got != want {
			t.Errorf("publicAddr(%s) = %v", a, got)
		}
	}
	b := &Broker{local: map[netip.Addr]bool{netip.MustParseAddr("203.0.114.9"): true}}
	r := &Rule{Host: "git.example.org", Pin: []string{"10.9.0.31"}}
	if !b.addrAllowed(r, netip.MustParseAddr("10.9.0.31")) {
		t.Error("a pinned address was refused")
	}
	if b.addrAllowed(r, netip.MustParseAddr("10.9.0.32")) {
		t.Error("an unpinned private address was allowed")
	}
	if b.addrAllowed(&Rule{Host: "x.example.org"}, netip.MustParseAddr("203.0.114.9")) {
		t.Error("the machine's own public address was allowed")
	}
}

func TestCALoadAndLeaf(t *testing.T) {
	ca, err := NewCA("t")
	if err != nil {
		t.Fatal(err)
	}
	cp, kp, err := ca.MarshalPEM()
	if err != nil {
		t.Fatal(err)
	}
	back, err := LoadCA(cp, kp)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := back.Leaf("pkg.example.org")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := leaf.Leaf.Verify(x509VerifyOpts(ca, "pkg.example.org")); err != nil {
		t.Errorf("a leaf from the reloaded CA doesn't verify: %v", err)
	}
	if _, err := leaf.Leaf.Verify(x509VerifyOpts(ca, "evil.example.org")); err == nil {
		t.Error("a leaf verified for another host")
	}
	again, _ := back.Leaf("pkg.example.org")
	if again != leaf {
		t.Error("leaves aren't reused")
	}
	if _, err := LoadCA([]byte("junk"), kp); err == nil {
		t.Error("junk loaded as a CA")
	}
}

func x509VerifyOpts(ca *CA, host string) x509.VerifyOptions {
	return x509.VerifyOptions{Roots: ca.Pool(), DNSName: host}
}
