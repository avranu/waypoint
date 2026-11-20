package core

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/hermes"
	"git.jmann.me/claude/waypoint/internal/hermes/hermestest"
)

// asstHermes is the least of Hermes's dashboard the core needs: a login
// that sets a cookie and a few routes that want it.
type asstHermes struct {
	mu     sync.Mutex
	logins int
	paths  []string
	valid  map[string]bool
}

func (h *asstHermes) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.paths = append(h.paths, r.URL.Path)
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == "/auth/password-login" {
		b, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(b), `"password":"hunter2"`) {
			w.WriteHeader(401)
			io.WriteString(w, `{"detail":"bad credentials"}`)
			return
		}
		h.logins++
		if h.valid == nil {
			h.valid = map[string]bool{}
		}
		tok := "c" + string(rune('a'+h.logins))
		h.valid[tok] = true
		http.SetCookie(w, &http.Cookie{Name: "hs", Value: tok, Path: "/"})
		io.WriteString(w, `{"ok":true}`)
		return
	}
	if c, err := r.Cookie("hs"); err != nil || !h.valid[c.Value] {
		w.WriteHeader(401)
		io.WriteString(w, `{"error":"unauthenticated"}`)
		return
	}
	switch r.URL.Path {
	case "/api/sessions", "/api/cron/jobs", "/api/auth/me":
		io.WriteString(w, `{"ok":true}`)
	default:
		w.WriteHeader(404)
		io.WriteString(w, `{"detail":"Not Found"}`)
	}
}

func (h *asstHermes) saw(path string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, p := range h.paths {
		if p == path {
			return true
		}
	}
	return false
}

type asstEnv struct {
	*env
	door   *hermestest.Door
	hermes *asstHermes
	url    string
	down   atomic.Bool // the door stops answering
	owner  *Actor      // ann, not an admin
	other  *Actor      // bob
	a      *Assistant
}

const asstKey = "door-key-SECRET"
const asstPassword = "hunter2"

// newAsstEnv connects an assistant owned by ann, set up by robin (an admin).
func newAsstEnv(t *testing.T) *asstEnv {
	t.Helper()
	e := &asstEnv{env: newEnv(t), hermes: &asstHermes{}}
	e.door = hermestest.NewDoor(asstKey, e.hermes)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if e.down.Load() {
			panic(http.ErrAbortHandler) // the connection drops, like a stopped gate
		}
		e.door.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	e.url = ts.URL
	e.owner = e.user("ann", false)
	e.other = e.user("bob", false)
	a, err := e.s.CreateAssistant(e.ctx, e.admin, AssistantInput{Name: str("Sage"), OwnerID: &e.owner.User.ID,
		URL: str(ts.URL + "/"), Key: str(asstKey), Password: str(asstPassword)})
	if err != nil {
		t.Fatal(err)
	}
	e.s.Wait() // the first check
	e.a = a
	t.Cleanup(func() { _ = e.s.DeleteAssistant(e.ctx, e.admin, a.ID) })
	return e
}

func (e *asstEnv) notes() []Notification {
	e.t.Helper()
	ns, _, err := e.s.ListNotifications(e.ctx, e.owner, false, 200)
	if err != nil {
		e.t.Fatal(err)
	}
	var out []Notification
	for _, n := range ns {
		if n.Kind == NotifyAssistant {
			out = append(out, n)
		}
	}
	return out
}

func testCA(t *testing.T) string {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "gate CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func TestAssistantAccess(t *testing.T) {
	e := newAsstEnv(t)
	id := e.a.ID
	ownerTok := func(spec TokenSpec) *Actor {
		secret, _, err := e.s.CreateToken(e.ctx, e.owner, spec)
		if err != nil {
			t.Fatal(err)
		}
		a, err := e.s.TokenActor(e.ctx, secret)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	ann := e.owner.User
	actors := []struct {
		name string
		a    *Actor
		get  bool // may read its settings (owner, admins)
		use  bool // may use it (owner, in a browser)
	}{
		{"owner in a browser", e.owner, true, true},
		{"another person", e.other, false, false},
		{"admin, not the owner", e.admin, true, false},
		{"owner's full token", ownerTok(TokenSpec{Name: "full"}), false, false},
		{"owner's read-only token", ownerTok(TokenSpec{Name: "ro", ReadOnly: true}), false, false},
		{"owner's agent token", ownerTok(TokenSpec{Name: "agent", Agent: "nous/hermes"}), false, false},
		{"owner's permission token", ownerTok(TokenSpec{Name: "perm", Permissions: []Permission{PermIssuesRead}}), false, false},
		{"owner over MCP", &Actor{User: ann, MCP: true}, false, false},
		{"owner as a named agent", &Actor{User: ann, Agent: "anthropic/claude"}, false, false},
		{"owner's automation", &Actor{User: ann, Automation: &Automation{}}, false, false},
		{"no one", nil, false, false},
		{"system", SystemActor(), false, false},
	}
	for _, c := range actors {
		t.Run(c.name, func(t *testing.T) {
			_, err := e.s.GetAssistant(e.ctx, c.a, id)
			if c.get != (err == nil) {
				t.Errorf("GetAssistant: %v", err)
			}
			if !c.get && !errors.Is(err, ErrNotFound) {
				t.Errorf("GetAssistant refusal should be not found: %v", err)
			}
			list, err := e.s.ListAssistants(e.ctx, c.a)
			if err != nil || (len(list) == 1) != c.get {
				t.Errorf("ListAssistants = %d, %v", len(list), err)
			}
			if len(list) == 1 && list[0].Mine != c.use {
				t.Errorf("Mine = %v", list[0].Mine)
			}
			// Everything that drives the door.
			uses := map[string]error{}
			_, _, uses["UseAssistant"] = e.s.UseAssistant(e.ctx, c.a, id)
			_, _, uses["AssistantGateway"] = e.s.AssistantGateway(e.ctx, c.a, id)
			_, _, uses["AssistantGate"] = e.s.AssistantGate(e.ctx, c.a, id)
			_, uses["CheckAssistant"] = e.s.CheckAssistant(e.ctx, c.a, id)
			_, uses["AssistantLinks"] = e.s.AssistantLinks(e.ctx, c.a, id, "")
			resp, err := e.s.AssistantAPI(e.ctx, c.a, id, http.MethodGet, "sessions", nil, nil, "", nil)
			if resp != nil {
				resp.Body.Close()
			}
			uses["AssistantAPI"] = err
			if !c.use {
				// These would act; only check they're refused (with a
				// working door they'd succeed for the owner).
				_, uses["FreezeAssistant"] = e.s.FreezeAssistant(e.ctx, c.a, id)
				_, uses["AssistantCall"] = e.s.AssistantCall(e.ctx, c.a, id, "session.list", nil)
				uses["AnswerAssistant"] = e.s.AnswerAssistant(e.ctx, c.a, id, "a1", nil)
				_, _, uses["PutAssistantConnection"] = e.s.PutAssistantConnection(e.ctx, c.a, id, "x", map[string]any{"kind": "mcp", "url": "https://x"})
				uses["DeleteAssistantConnection"] = e.s.DeleteAssistantConnection(e.ctx, c.a, id, "x")
				_, uses["LinkAssistantSession"] = e.s.LinkAssistantSession(e.ctx, c.a, id, "s1", "X-1", "")
				uses["UnlinkAssistantSession"] = e.s.UnlinkAssistantSession(e.ctx, c.a, id, 1)
			}
			for what, err := range uses {
				if c.use && err != nil {
					t.Errorf("%s: %v", what, err)
				}
				if !c.use && !errors.Is(err, ErrNotFound) {
					t.Errorf("%s: %v, want not found", what, err)
				}
			}
		})
	}
	if e.door.Frozen() {
		t.Error("someone other than the owner froze the agent")
	}
	if _, _, err := e.s.UseAssistant(e.ctx, e.owner, id+100); !errors.Is(err, ErrNotFound) {
		t.Errorf("no such assistant: %v", err)
	}
}

func TestAssistantAdminCRUD(t *testing.T) {
	e := newAsstEnv(t)
	id := e.a.ID
	adminTok, _, _ := e.s.CreateToken(e.ctx, e.admin, TokenSpec{Name: "admin-full"})
	adminTokActor, _ := e.s.TokenActor(e.ctx, adminTok)
	in := AssistantInput{Name: str("Other"), URL: str(e.url), Key: str("k"), Password: str("p")}
	for _, c := range []struct {
		name string
		a    *Actor
	}{
		{"owner, not an admin", e.owner},
		{"another person", e.other},
		{"admin's token", adminTokActor},
		{"admin over MCP", &Actor{User: e.admin.User, MCP: true}},
		{"admin's automation", &Actor{User: e.admin.User, Automation: &Automation{}}},
	} {
		if _, err := e.s.CreateAssistant(e.ctx, c.a, in); !errors.Is(err, ErrForbidden) {
			t.Errorf("%s create: %v", c.name, err)
		}
		if _, err := e.s.UpdateAssistant(e.ctx, c.a, id, AssistantInput{Name: str("Renamed")}); !errors.Is(err, ErrForbidden) {
			t.Errorf("%s update: %v", c.name, err)
		}
		if err := e.s.DeleteAssistant(e.ctx, c.a, id); !errors.Is(err, ErrForbidden) {
			t.Errorf("%s delete: %v", c.name, err)
		}
	}

	// Validation.
	ann := e.owner.User.ID
	nobody := int64(9999)
	bobTok, bobInfo, _ := e.s.CreateToken(e.ctx, e.other, TokenSpec{Name: "bob"})
	_ = bobTok
	for _, c := range []struct {
		name string
		in   AssistantInput
		want string
	}{
		{"no name", AssistantInput{URL: str(e.url), Key: str("k"), Password: str("p")}, "name"},
		{"long name", AssistantInput{Name: str(strings.Repeat("n", 61)), URL: str(e.url), Key: str("k"), Password: str("p")}, "name"},
		{"no key", AssistantInput{Name: str("A"), URL: str(e.url), Password: str("p")}, "key"},
		{"no password", AssistantInput{Name: str("A"), URL: str(e.url), Key: str("k")}, "password"},
		{"bad url", AssistantInput{Name: str("A"), URL: str("door.local"), Key: str("k"), Password: str("p")}, "URL"},
		{"https without CA", AssistantInput{Name: str("A"), URL: str("https://10.0.0.1:9443"), Key: str("k"), Password: str("p")}, "CA"},
		{"CA not PEM", AssistantInput{Name: str("A"), URL: str("https://10.0.0.1:9443"), CAPEM: str("nope"), Key: str("k"), Password: str("p")}, "PEM"},
		{"no such owner", AssistantInput{Name: str("A"), URL: str(e.url), Key: str("k"), Password: str("p"), OwnerID: &nobody}, "owner"},
		{"someone else's token", AssistantInput{Name: str("A"), URL: str(e.url), Key: str("k"), Password: str("p"), OwnerID: &ann, TokenID: &bobInfo.ID}, "token"},
		{"key header not a name", AssistantInput{Name: str("A"), URL: str(e.url), Key: str("k"), Password: str("p"), KeyHeader: str("X Key:")}, "header name"},
		{"agent URL not http", AssistantInput{Name: str("A"), URL: str(e.url), Key: str("k"), Password: str("p"), AgentURL: str("gate:8080")}, "address for the agent"},
		{"agent URL with a login", AssistantInput{Name: str("A"), URL: str(e.url), Key: str("k"), Password: str("p"), AgentURL: str("http://u:p@gate:8080")}, "address for the agent"},
	} {
		if _, err := e.s.CreateAssistant(e.ctx, e.admin, c.in); !IsInvalid(err) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", c.name, err)
		}
	}

	// The owner's agent token ties the agent's own work to it.
	_, tok, err := e.s.CreateToken(e.ctx, e.owner, TokenSpec{Name: "sage", Agent: "nous/hermes"})
	if err != nil {
		t.Fatal(err)
	}
	e.setClock(e.clockNow().Add(time.Second))
	got, err := e.s.UpdateAssistant(e.ctx, e.admin, id, AssistantInput{Name: str("Sage 2"), TokenID: &tok.ID, Key: str(""), Password: str("")})
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "Sage 2" || got.TokenID == nil || *got.TokenID != tok.ID || got.Token != "sage" || got.Agent != "nous/hermes" {
		t.Errorf("updated = %+v", got)
	}
	// Empty secrets kept the stored ones: the rebuilt client still gets in.
	c, _, err := e.s.UseAssistant(e.ctx, e.owner, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Check(e.ctx); err != nil {
		t.Errorf("after an update with empty secrets: %v", err)
	}
	e.s.Wait()
	zero := int64(0)
	e.setClock(e.clockNow().Add(time.Second))
	if got, err := e.s.UpdateAssistant(e.ctx, e.admin, id, AssistantInput{TokenID: &zero}); err != nil || got.TokenID != nil {
		t.Errorf("clearing the token: %+v %v", got, err)
	}
	e.s.Wait()

	// Admins delete; the owner then has nothing to use.
	if err := e.s.DeleteAssistant(e.ctx, e.admin, id); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.s.UseAssistant(e.ctx, e.owner, id); !errors.Is(err, ErrNotFound) {
		t.Errorf("after delete: %v", err)
	}
	if err := e.s.DeleteAssistant(e.ctx, e.admin, id); !errors.Is(err, ErrNotFound) {
		t.Errorf("delete twice: %v", err)
	}
}

// A gate's key header and its address for the agent are the assistant's
// own when set, else the site's (WP-459).
func TestAssistantGateSettings(t *testing.T) {
	e := newAsstEnv(t)
	id := e.a.ID
	if e.a.KeyHeader != "" || e.a.AgentURL != "" || e.a.Site != (AssistantDefaults{KeyHeader: hermes.DefaultKeyHeader}) {
		t.Errorf("unset: header %q, agent URL %q, site %+v", e.a.KeyHeader, e.a.AgentURL, e.a.Site)
	}
	works := func(what string) {
		t.Helper()
		c, _, err := e.s.UseAssistant(e.ctx, e.owner, id)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Check(e.ctx); err != nil {
			t.Errorf("%s: %v", what, err)
		}
	}
	update := func(in AssistantInput) *Assistant {
		t.Helper()
		e.s.Wait()
		e.setClock(e.clockNow().Add(time.Second))
		a, err := e.s.UpdateAssistant(e.ctx, e.admin, id, in)
		if err != nil {
			t.Fatal(err)
		}
		e.s.Wait()
		return a
	}

	// The site's header and address, for every assistant without its own.
	e.s.Wait()
	e.s.cfg.Assistant = AssistantDefaults{KeyHeader: " X-Site-Key ", AgentURL: "http://10.9.0.1:8080/"}
	e.door.Header = "X-Site-Key"
	a := update(AssistantInput{Name: str("Sage")})
	if a.KeyHeader != "" || a.AgentURL != "" || a.Site != (AssistantDefaults{KeyHeader: "X-Site-Key", AgentURL: "http://10.9.0.1:8080"}) {
		t.Errorf("site's: header %q, agent URL %q, site %+v", a.KeyHeader, a.AgentURL, a.Site)
	}
	works("the site's header")

	// The assistant's own win.
	e.door.Header = "X-Own-Key"
	a = update(AssistantInput{KeyHeader: str("X-Own-Key"), AgentURL: str(" http://gate.internal:8080/ ")})
	if a.KeyHeader != "X-Own-Key" || a.AgentURL != "http://gate.internal:8080" {
		t.Errorf("own: header %q, agent URL %q", a.KeyHeader, a.AgentURL)
	}
	works("its own header")
	if got, _ := e.s.GetAssistant(e.ctx, e.owner, id); got == nil || got.AgentURL != "http://gate.internal:8080" {
		t.Errorf("the owner reads %+v", got)
	}

	// Emptied, the site's apply again.
	e.door.Header = "X-Site-Key"
	a = update(AssistantInput{KeyHeader: str(""), AgentURL: str("")})
	if a.KeyHeader != "" || a.AgentURL != "" {
		t.Errorf("cleared: header %q, agent URL %q", a.KeyHeader, a.AgentURL)
	}
	works("back to the site's header")
}

func TestAssistantSecretsNeverInJSON(t *testing.T) {
	e := newEnv(t)
	owner := e.user("ann", false)
	ca := testCA(t)
	a, err := e.s.CreateAssistant(e.ctx, e.admin, AssistantInput{Name: str("Sage"), OwnerID: &owner.User.ID,
		URL: str("https://127.0.0.1:1"), CAPEM: str(ca), Key: str("door-key-SECRET"), Password: str("pw-SECRET"), Username: str("wp")})
	if err != nil {
		t.Fatal(err)
	}
	e.s.Wait()
	if !a.HasCA || a.Username != "wp" || a.URL != "https://127.0.0.1:1" {
		t.Errorf("created = %+v", a)
	}
	got, _ := e.s.GetAssistant(e.ctx, owner, a.ID)
	list, _ := e.s.ListAssistants(e.ctx, e.admin)
	for name, v := range map[string]any{"create": a, "get": got, "list": list} {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{"SECRET", "BEGIN CERTIFICATE", strings.Split(ca, "\n")[1], "door_key", "password\""} {
			if strings.Contains(string(b), secret) {
				t.Errorf("%s JSON has %q: %s", name, secret, b)
			}
		}
	}
	// Nor the audit trail.
	entries, err := e.s.ListAudit(e.ctx, e.admin, AuditQuery{Limit: 50})
	if err == nil {
		b, _ := json.Marshal(entries)
		if strings.Contains(string(b), "SECRET") {
			t.Errorf("audit has a secret: %s", b)
		}
	}
}

func TestAssistantPathAllowlist(t *testing.T) {
	for _, c := range []struct {
		path string
		ok   bool
	}{
		{"sessions", true},
		{"sessions/abc/messages", true},
		{"cron/jobs", true},
		{"cron/jobs/7/trigger", true},
		{"files/download", true},
		{"env", true},
		{"config", true},
		{"status", true},
		{"env/reveal", false},
		{"env/reveal/OPENAI_API_KEY", false},
		{"hermes/update", false},
		{"auth/password-login", false},
		{"auth/ws-ticket", false},
		{"auth/me", false},
		{"cron/fire", false},
		{"providers/oauth/start", false},
		{"ssh/keys", false},
		{"tools/computer-use", false},
		{"sessions/../auth/me", false},
		{"../auth", false},
		{"sessions/..", false},
		{"sessions//x", false},
		{"fs/home/open-terminal", false},
		{"", false},
		{"ws", false},
	} {
		if got := assistantPathAllowed(c.path); got != c.ok {
			t.Errorf("assistantPathAllowed(%q) = %v, want %v", c.path, got, c.ok)
		}
	}
}

// TestAssistantPathAllowlistSegments: the allowlist names path segments
// ("env", "sessions"), and matches "env/…" and "env?…" explicitly, so a
// longer word that merely starts the same isn't on it.
func TestAssistantPathAllowlistSegments(t *testing.T) {
	for _, p := range []string{"envelope", "env-dump", "sessionsadmin", "statusz", "configure", "memory_dump", "logsink"} {
		if assistantPathAllowed(p) {
			t.Errorf("assistantPathAllowed(%q) = true: a prefix of a word, not a path segment", p)
		}
	}
}

func TestAssistantMethodAllowlist(t *testing.T) {
	for _, c := range []struct {
		m  string
		ok bool
	}{
		{"session.create", true},
		{"session.list", true},
		{"prompt.submit", true},
		{"approval.respond", true},
		{"ping", true},
		{"config.set", true},
		{"mcp.servers.list", true},
		{"image.attach", true},
		{"pingx", false},
		{"sessions", false},
		{"session", false},
		{"client.capabilities", false},
		{"shell.exec", false},
		{"secret.get", false},
		{"vault.unlock", false},
		{"mcp.servers.add", false},
		{"image.attachx", false},
		{"", false},
	} {
		if got := assistantMethodAllowed(c.m); got != c.ok {
			t.Errorf("assistantMethodAllowed(%q) = %v, want %v", c.m, got, c.ok)
		}
	}
}

func TestAssistantAPIAndCallRefusals(t *testing.T) {
	e := newAsstEnv(t)
	id := e.a.ID
	// Refused paths never reach Hermes, whoever asks.
	for _, p := range []string{"env/reveal", "hermes/update", "auth/password-login", "../auth/me"} {
		resp, err := e.s.AssistantAPI(e.ctx, e.owner, id, http.MethodGet, p, nil, nil, "", nil)
		if resp != nil {
			resp.Body.Close()
		}
		if !errors.Is(err, ErrForbidden) {
			t.Errorf("%s: %v", p, err)
		}
	}
	for _, p := range []string{"sessions", "cron/jobs"} {
		resp, err := e.s.AssistantAPI(e.ctx, e.owner, id, http.MethodGet, p, nil, nil, "", nil)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 || !e.hermes.saw("/api/"+p) {
			t.Errorf("%s: %d", p, resp.StatusCode)
		}
	}
	if e.hermes.saw("/api/env/reveal") || e.hermes.saw("/api/hermes/update") {
		t.Error("a refused path reached Hermes")
	}
	if _, err := e.s.AssistantCall(e.ctx, e.owner, id, "shell.exec", nil); !IsInvalid(err) {
		t.Errorf("refused method: %v", err)
	}
	// While frozen, nothing goes through to Hermes.
	if _, err := e.s.FreezeAssistant(e.ctx, e.owner, id); err != nil {
		t.Fatal(err)
	}
	if !e.door.Frozen() {
		t.Fatal("not frozen at the gate")
	}
	resp, err := e.s.AssistantAPI(e.ctx, e.owner, id, http.MethodGet, "sessions", nil, nil, "", nil)
	if resp != nil {
		resp.Body.Close()
	}
	if !errors.Is(err, ErrUnavailable) {
		t.Errorf("api while frozen: %v", err)
	}
	if _, err := e.s.AssistantCall(e.ctx, e.owner, id, "session.list", nil); !errors.Is(err, ErrUnavailable) {
		t.Errorf("call while frozen: %v", err)
	}
}

func TestAssistantConnections(t *testing.T) {
	e := newAsstEnv(t)
	code, raw, err := e.s.PutAssistantConnection(e.ctx, e.owner, e.a.ID, "github", map[string]any{"kind": "mcp", "url": "https://api.github.com/mcp"})
	if err != nil || code != http.StatusAccepted || !strings.Contains(string(raw), `"pending"`) {
		t.Fatalf("put: %d %s %v", code, raw, err)
	}
	if _, _, err := e.s.PutAssistantConnection(e.ctx, e.owner, e.a.ID, "Bad Name", map[string]any{"kind": "mcp", "url": "https://x"}); !IsInvalid(err) {
		t.Errorf("bad name: %v", err)
	}
	if err := e.s.DeleteAssistantConnection(e.ctx, e.owner, e.a.ID, "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("delete missing: %v", err)
	}
	e.s.Wait()
}

func TestAssistantWatcher(t *testing.T) {
	e := newAsstEnv(t)
	id := e.a.ID
	check := func() AssistantState {
		e.t.Helper()
		e.s.checkAssistant(e.ctx, id)
		a, err := e.s.GetAssistant(e.ctx, e.owner, id)
		if err != nil {
			e.t.Fatal(err)
		}
		return a.State
	}
	want := func(step string, n int, contains string) {
		t.Helper()
		ns := e.notes()
		if len(ns) != n {
			var titles []string
			for _, x := range ns {
				titles = append(titles, x.Title)
			}
			t.Fatalf("%s: %d notifications %q, want %d", step, len(ns), titles, n)
		}
		if contains != "" && !strings.Contains(ns[0].Title, contains) {
			t.Errorf("%s: newest = %q, want %q", step, ns[0].Title, contains)
		}
	}

	st := check()
	if !st.Reachable || st.Frozen || !st.Signal || len(st.Pending) != 0 {
		t.Fatalf("first state = %+v", st)
	}
	want("first look", 0, "")
	check()
	want("unchanged", 0, "")

	code := e.door.Ask("terminal", "rm -rf ~/old")
	st = check()
	if len(st.Pending) != 1 || st.Pending[0].Code != code {
		t.Fatalf("pending = %+v", st.Pending)
	}
	want("new approval", 1, code)
	if n := e.notes()[0]; n.Body != "rm -rf ~/old" || !strings.HasPrefix(n.URL, fmt.Sprintf("/assistant/%d", id)) {
		t.Errorf("approval notification = %+v", n)
	}
	e.setClock(e.clockNow().Add(15 * time.Second))
	check()
	want("same approval again", 1, "")
	code2 := e.door.Ask("http", "POST to bank")
	check()
	want("second approval", 2, code2)
	e.door.Decide(code, true)
	e.door.Decide(code2, false)
	check()
	want("approvals decided", 2, "")

	e.door.SetFrozen(true)
	st = check()
	if !st.Frozen {
		t.Fatal("not frozen")
	}
	want("frozen", 3, "is frozen")
	check()
	want("still frozen", 3, "")
	e.door.SetFrozen(false)
	check()
	want("thawed", 4, "running again")

	// An outage: nothing at first (a deploy restarts the gate), one notice
	// once it has lasted, then one when it's back.
	e.down.Store(true)
	st = check()
	if st.Reachable || st.DownSince == nil || st.Error == "" {
		t.Fatalf("down state = %+v", st)
	}
	want("just down", 4, "")
	e.setClock(e.clockNow().Add(time.Minute))
	check()
	want("down a minute", 4, "")
	e.setClock(e.clockNow().Add(90 * time.Second))
	check()
	want("down long enough", 5, "isn't answering")
	e.setClock(e.clockNow().Add(time.Minute))
	check()
	want("still down", 5, "")
	e.down.Store(false)
	e.setClock(e.clockNow().Add(15 * time.Second))
	st = check()
	if !st.Reachable || st.DownSince != nil || st.Error != "" {
		t.Fatalf("back state = %+v", st)
	}
	want("back", 6, "is back")
	check()
	want("steady", 6, "")

	// The owner hears live; nobody else does.
	sub := e.s.Hub.Subscribe(e.owner.User.ID)
	defer e.s.Hub.Unsubscribe(sub)
	bob := e.s.Hub.Subscribe(e.other.User.ID)
	defer e.s.Hub.Unsubscribe(bob)
	e.door.Ask("terminal", "ls")
	check()
	if !hubHas(sub, EvAssistant) {
		t.Error("owner got no live event")
	}
	if hubHas(bob, EvAssistant) {
		t.Error("another user got the owner's assistant event")
	}
}

// TestAssistantWatcherBlipKeepsApprovals: a single failed check (the gate
// restarting) shouldn't make approvals the owner already heard about look
// new.
func TestAssistantWatcherBlipKeepsApprovals(t *testing.T) {
	e := newAsstEnv(t)
	code := e.door.Ask("terminal", "rm -rf ~/old")
	e.s.checkAssistant(e.ctx, e.a.ID)
	if n := len(e.notes()); n != 1 {
		t.Fatalf("notifications = %d", n)
	}
	e.down.Store(true)
	e.setClock(e.clockNow().Add(15 * time.Second))
	e.s.checkAssistant(e.ctx, e.a.ID)
	e.down.Store(false)
	e.setClock(e.clockNow().Add(15 * time.Second))
	e.s.checkAssistant(e.ctx, e.a.ID)
	if ns := e.notes(); len(ns) != 1 {
		t.Errorf("after a 15-second blip the same approval (%s) was notified again: %d notifications, newest %q", code, len(ns), ns[0].Title)
	}
}

func hubHas(sub *Subscription, kind string) bool {
	deadline := time.After(500 * time.Millisecond)
	for {
		select {
		case ev := <-sub.C:
			if ev.Kind == kind {
				return true
			}
		case <-deadline:
			return false
		}
	}
}

func TestAssistantLinks(t *testing.T) {
	e := newAsstEnv(t)
	id := e.a.ID
	p := e.project(e.owner, "Errands", "")
	i := e.issue(e.owner, IssueInput{Project: p.Key, Title: "Renew passport"})
	secret := e.project(e.other, "Bob's", "")
	hidden := e.issue(e.other, IssueInput{Project: secret.Key, Title: "Surprise"})

	l, err := e.s.LinkAssistantSession(e.ctx, e.owner, id, " s-123 ", i.Key, "Passport chat")
	if err != nil {
		t.Fatal(err)
	}
	if l.SessionID != "s-123" || l.Issue == nil || l.Issue.Key != i.Key || l.Assistant != "Sage" || l.Title != "Passport chat" {
		t.Errorf("link = %+v", l)
	}
	// Linking again updates the title, not a second row.
	if l2, err := e.s.LinkAssistantSession(e.ctx, e.owner, id, "s-123", i.Key, "Renamed"); err != nil || l2.ID != l.ID || l2.Title != "Renamed" {
		t.Errorf("relink = %+v %v", l2, err)
	}
	if _, err := e.s.LinkAssistantSession(e.ctx, e.owner, id, "s-9", hidden.Key, ""); err == nil {
		t.Error("linked an issue the owner can't see")
	}
	if _, err := e.s.LinkAssistantSession(e.ctx, e.owner, id, "  ", i.Key, ""); !IsInvalid(err) {
		t.Errorf("empty session: %v", err)
	}
	if ls, err := e.s.AssistantLinks(e.ctx, e.owner, id, "s-123"); err != nil || len(ls) != 1 {
		t.Errorf("links for the session = %d %v", len(ls), err)
	}
	if ls, err := e.s.AssistantLinks(e.ctx, e.owner, id, "other"); err != nil || len(ls) != 0 {
		t.Errorf("links for another session = %d %v", len(ls), err)
	}
	if ls, err := e.s.IssueAssistantLinks(e.ctx, e.owner, i.Key); err != nil || len(ls) != 1 {
		t.Errorf("issue links for the owner = %d %v", len(ls), err)
	}
	// An admin who can see the issue still doesn't see the owner's chats.
	if ls, err := e.s.IssueAssistantLinks(e.ctx, e.admin, i.Key); err == nil && len(ls) != 0 {
		t.Errorf("issue links for an admin = %d", len(ls))
	}
	tok, _, _ := e.s.CreateToken(e.ctx, e.owner, TokenSpec{Name: "t"})
	ta, _ := e.s.TokenActor(e.ctx, tok)
	if ls, err := e.s.IssueAssistantLinks(e.ctx, ta, i.Key); err != nil || len(ls) != 0 {
		t.Errorf("issue links through a token = %d %v", len(ls), err)
	}
	if err := e.s.UnlinkAssistantSession(e.ctx, e.owner, id, l.ID); err != nil {
		t.Fatal(err)
	}
	if ls, _ := e.s.AssistantLinks(e.ctx, e.owner, id, ""); len(ls) != 0 {
		t.Errorf("after unlink: %d", len(ls))
	}
}

func TestAssistantError(t *testing.T) {
	for _, c := range []struct {
		in          error
		unavailable bool
	}{
		{hermes.ErrUnreachable, true},
		{&hermes.Error{Status: 401, Msg: "no"}, true},
		{&hermes.Error{Status: 500, Msg: "boom"}, false},
		{errors.New("other"), false},
	} {
		if got := errors.Is(AssistantError(c.in), ErrUnavailable); got != c.unavailable {
			t.Errorf("AssistantError(%v) unavailable = %v", c.in, got)
		}
	}
	if err := gateError(&hermes.Error{Status: 400, Msg: "bad name"}); !IsInvalid(err) || !strings.Contains(err.Error(), "bad name") {
		t.Errorf("gate 400: %v", err)
	}
	if err := gateError(&hermes.Error{Status: 404, Msg: "gone"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("gate 404: %v", err)
	}
}
