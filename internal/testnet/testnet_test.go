package testnet

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// These tests try the network on purpose; what they tried is checked here,
// in place of Report.
func TestMain(m *testing.M) {
	Guard()
	code := m.Run()
	// Only a whole run makes every try; -run picks some of them.
	whole := flag.Lookup("test.run").Value.String() == ""
	if want := []string{"DNS wp1065-nowhere.nohost-wp.org", "api.github.com:443"}; whole && code == 0 && !slices.Equal(Tried(), want) {
		fmt.Fprintf(os.Stderr, "tried %q, want %q\n", Tried(), want)
		code = 1
	}
	os.Exit(code)
}

func TestGuardRefusesOnlyWhatLeavesTheMachine(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("loopback server: %v", err)
	}
	_ = resp.Body.Close()

	if _, err := http.Get("https://api.github.com/"); !errors.Is(err, ErrRefused) {
		t.Errorf("a real host: %v", err)
	}
	for _, name := range []string{"git.example.com", "wp1065-nowhere.nohost-wp.org"} {
		if _, err := net.DefaultResolver.LookupHost(context.Background(), name); err == nil {
			t.Errorf("%s resolved", name)
		}
	}
	if addrs, err := net.DefaultResolver.LookupHost(context.Background(), "localhost"); err != nil || len(addrs) == 0 {
		t.Errorf("localhost: %v %v", addrs, err)
	}
	if os.Getenv("GIT_SSH_COMMAND") == "" {
		t.Error("git's ssh is left on")
	}
}

func TestLocalAndReserved(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:80": true, "[::1]:443": true, "localhost:8080": true, "LOCALHOST": true, "app.localhost:1": true,
		"127.0.0.53:53": true, "0.0.0.0:80": true, "[::]:80": true, "10.0.0.1:80": false, "api.github.com:443": false, "example.com": false, "": false,
	} {
		if got := Local(addr); got != want {
			t.Errorf("Local(%q) = %v", addr, got)
		}
	}
	for host, want := range map[string]bool{
		"example.com": true, "git.example.com": true, "gotify.example.org.": true, "x.test": true, "a.invalid": true,
		"forge.example": true, "api.github.com": false, "example.com.au": false, "notexample.com": false, "contest": false,
	} {
		if got := Reserved(host); got != want {
			t.Errorf("Reserved(%q) = %v", host, got)
		}
	}
}

func TestSearchDomains(t *testing.T) {
	p := filepath.Join(t.TempDir(), "resolv.conf")
	if err := os.WriteFile(p, []byte("# c\nnameserver 127.0.0.53\nsearch a.lan b.lan\ndomain c.lan\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := searchDomains(p); !slices.Equal(got, []string{"a.lan", "b.lan", "c.lan"}) {
		t.Errorf("search = %q", got)
	}
	if searchDomains(filepath.Join(t.TempDir(), "none")) != nil {
		t.Error("a missing file has no search list")
	}
}

func TestReport(t *testing.T) {
	if Report(3) != 3 {
		t.Error("a failed run keeps its code")
	}
	// Make a try of our own (api.github.com is already in TestMain's list), so
	// this holds when the test runs alone.
	_, _ = http.Get("https://api.github.com/")
	if Report(0) != 1 {
		t.Error("a passing run that went out fails")
	}
}

// Every package with tests is guarded: a new one gets a main_test.go with
// func TestMain(m *testing.M) { os.Exit(testnet.Run(m)) }.
func TestEveryPackageIsGuarded(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	guarded, tested := map[string]bool{}, map[string]bool{}
	err = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if n := d.Name(); p != root && (strings.HasPrefix(n, ".") || n == "node_modules" || n == "testdata" || n == "web") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, "_test.go") {
			return nil
		}
		dir, _ := filepath.Rel(root, filepath.Dir(p))
		tested[dir] = true
		b, err := os.ReadFile(p) // #nosec G304 -- this repository's own files
		if err != nil {
			return err
		}
		src := string(b)
		if (strings.Contains(src, "func TestMain(") && (strings.Contains(src, "testnet.Run(m)") || strings.Contains(src, "testnet.Guard()"))) || dir == filepath.Join("internal", "testnet") {
			guarded[dir] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(tested) < 10 {
		t.Fatalf("found only %d test packages under %s", len(tested), root)
	}
	for dir := range tested {
		if !guarded[dir] {
			t.Errorf("%s: its TestMain doesn't call testnet.Run (add main_test.go: func TestMain(m *testing.M) { os.Exit(testnet.Run(m)) })", dir)
		}
	}
}

func TestQuestionName(t *testing.T) {
	msg := append(make([]byte, 12), 3, 'a', 'p', 'i', 6, 'g', 'i', 't', 'h', 'u', 'b', 3, 'c', 'o', 'm', 0, 0, 1, 0, 1)
	if got := questionName(msg); got != "api.github.com" {
		t.Errorf("name = %q", got)
	}
	for _, bad := range [][]byte{nil, make([]byte, 12), append(make([]byte, 12), 9, 'a'), append(make([]byte, 12), 0xc0, 1)} {
		if got := questionName(bad); got != "" {
			t.Errorf("questionName(%v) = %q", bad, got)
		}
	}
}
