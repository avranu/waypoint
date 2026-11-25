package core

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// A forge that doesn't answer is ErrUnavailable with a message naming it;
// a forge that answers no is not (WP-265).
func TestForgeDown(t *testing.T) {
	f := &Forge{BaseURL: "https://git.example.com"}
	timeout := &url.Error{Op: "Get", URL: "https://git.example.com/api/v1/repos/a/b", Err: context.DeadlineExceeded}
	for _, c := range []struct {
		name string
		err  error
		want string // "" = returned as it is
	}{
		{"timeout", timeout, "git.example.com didn't answer within 15 s; try again"},
		{"wrapped timeout", errors.Join(errors.New("read it"), timeout), "git.example.com didn't answer within 15 s"},
		{"refused", &url.Error{Op: "Get", URL: "https://git.example.com/", Err: errors.New("dial tcp: connection refused")}, "couldn't reach git.example.com"},
		{"503", &forgeAPIError{method: "GET", path: "/repos/a/b", status: 503}, "git.example.com answered HTTP 503"},
		{"422", &forgeAPIError{method: "POST", path: "/repos/a/b/pulls", status: 422, body: "no commits"}, ""},
		{"blocked", &url.Error{Op: "Get", URL: "https://10.0.0.1/", Err: ErrEgressBlocked}, ""},
		{"canceled", &url.Error{Op: "Get", URL: "https://git.example.com/", Err: context.Canceled}, ""},
		{"other", errors.New("decode: bad json"), ""},
	} {
		got := forgeDown(f, 15*time.Second, c.err)
		if c.want == "" {
			if got != c.err {
				t.Errorf("%s: changed to %v", c.name, got)
			}
			continue
		}
		if !errors.Is(got, ErrUnavailable) || !strings.Contains(got.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.name, got, c.want)
		}
		if !errors.Is(got, c.err) && !errors.Is(got, context.DeadlineExceeded) {
			t.Errorf("%s: the cause is lost: %v", c.name, got)
		}
	}
}

// A call a person waits on gets the patient client; a real client timeout
// reads as the forge not answering.
func TestForgePatientClient(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
		_, _ = w.Write([]byte(`{"default_branch":"main"}`))
	}))
	defer srv.Close()
	defer close(release)
	c := &httpForgeClient{http: &http.Client{Timeout: 50 * time.Millisecond}, patient: &http.Client{Timeout: time.Minute}}
	f := &Forge{Kind: "forgejo", BaseURL: srv.URL, token: "t"}
	start := time.Now()
	_, err := c.DefaultBranch(context.Background(), f, "a/b")
	down := forgeDown(f, time.Since(start), err)
	if !errors.Is(down, ErrUnavailable) || !strings.Contains(down.Error(), "didn't answer within 1 s") {
		t.Fatalf("background call: %v", down)
	}
	if c.client(context.Background()) != c.http || c.client(forgePatient(context.Background())) != c.patient {
		t.Error("the wrong client")
	}
	if (&httpForgeClient{http: c.http}).client(forgePatient(context.Background())) != c.http {
		t.Error("no patient client: the plain one")
	}
}

// slowForge is a shipForge whose pull request calls don't get an answer.
type slowForge struct {
	*shipForge
	defaultDown, createDown bool
	createdAnyway           bool  // the forge opened it before going quiet
	createErr               error // CreatePR answers with it
}

var errForgeTimeout = &url.Error{Op: "Post", URL: "https://git.example.com/api/v1/repos/acme/app/pulls", Err: context.DeadlineExceeded}

func (g *slowForge) DefaultBranch(ctx context.Context, f *Forge, repo string) (string, error) {
	if g.defaultDown {
		return "", errForgeTimeout
	}
	return g.shipForge.DefaultBranch(ctx, f, repo)
}

func (g *slowForge) CreatePR(ctx context.Context, f *Forge, repo string, in NewPR) (*ForgePR, error) {
	if !isForgePatient(ctx) {
		return nil, errors.New("opening a pull request isn't a patient call")
	}
	if g.createErr != nil {
		return nil, g.createErr
	}
	if !g.createDown {
		return g.shipForge.CreatePR(ctx, f, repo, in)
	}
	if g.createdAnyway {
		fp, err := g.shipForge.CreatePR(ctx, f, repo, in)
		if err != nil {
			return nil, err
		}
		g.mu.Lock()
		g.listed = append(g.listed, fp)
		g.mu.Unlock()
		g.createdAnyway = false
		return nil, errForgeTimeout
	}
	return nil, errForgeTimeout
}

func TestOpenPullRequestForgeDown(t *testing.T) {
	e := newEnv(t)
	g := &slowForge{shipForge: newShipForge(), defaultDown: true}
	e.s.Forges = g
	p := e.project(e.admin, "App", "")
	f, err := e.s.CreateForge(e.ctx, e.admin, ForgeInput{Kind: str("forgejo"), BaseURL: str("https://git.example.com"), Token: str("t")})
	if err != nil {
		t.Fatal(err)
	}
	e.forgePeople(f, "wp-bot")
	if _, err := e.s.AddProjectRepo(e.ctx, e.admin, p.Key, f.ID, "acme/app", false); err != nil {
		t.Fatal(err)
	}
	in := OpenPRInput{Project: p.Key, Head: "feature", Title: "Do it"}

	// The default branch read times out: a 502 that says so, not a bug.
	_, err = e.s.OpenPullRequest(e.ctx, e.admin, in)
	if !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "git.example.com didn't answer") {
		t.Fatalf("default branch down: %v", err)
	}
	// The open itself times out.
	g.defaultDown, g.createDown = false, true
	_, err = e.s.OpenPullRequest(e.ctx, e.admin, in)
	if !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "open the pull request: git.example.com didn't answer") {
		t.Fatalf("open down: %v", err)
	}
	// "Already exists", and the forge doesn't list it: a 502 to try again,
	// not the forge's refusal.
	g.createDown, g.createErr = false, &forgeAPIError{method: "POST", path: "/repos/acme/app/pulls", status: 422, body: "A pull request already exists for acme:feature"}
	_, err = e.s.OpenPullRequest(e.ctx, e.admin, in)
	var ae *forgeAPIError
	if !errors.Is(err, ErrUnavailable) || errors.As(err, &ae) || shipRefused(err) || !strings.Contains(err.Error(), "already exist") || !strings.HasSuffix(err.Error(), "try again in a minute") {
		t.Fatalf("already exists, not listed: %v", err)
	}
	g.createDown, g.createErr = true, nil
	// It opened anyway, and no webhook said so: the forge's list has it, so
	// it is recorded and returned.
	g.createdAnyway = true
	res, err := e.s.OpenPullRequest(e.ctx, e.admin, in)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Existing || res.Pull == nil || res.Pull.HeadRef != "feature" || res.Pull.Title != "Do it" {
		t.Fatalf("opened but unanswered: %+v", res)
	}
	// A retry finds it in Waypoint, without asking the forge to open it.
	n := len(g.created)
	again, err := e.s.OpenPullRequest(e.ctx, e.admin, in)
	if err != nil || again.Pull.ID != res.Pull.ID || len(g.created) != n {
		t.Fatalf("retry: %v %+v", err, again)
	}
}

// An armed merge the forge didn't answer is tried again later, with a note
// that says why, rather than handed to a person (WP-265).
func TestArmedMergeForgeDownIsRetried(t *testing.T) {
	se := readyForgejo(t)
	id := se.pr(5).ID
	se.forgeSays(5, ptr(true))
	se.readForge(5)
	se.sg.mergeErr = errForgeTimeout
	if _, err := se.s.MergePR(se.ctx, se.admin, id, MergeInput{WhenReady: true}); err != nil {
		t.Fatal(err)
	}
	pr := se.pr(5)
	if pr.ArmedAt == nil || !strings.Contains(pr.armedNote, "didn't answer") || strings.HasPrefix(pr.armedNote, mergeFailedNote) || pr.mergeRetryN != 1 {
		t.Fatalf("armed = %v note %q retries %d", pr.ArmedAt, pr.armedNote, pr.mergeRetryN)
	}
	// A person merging now hears the forge is down: a 502, not a refusal.
	if _, err := se.s.MergePR(se.ctx, se.admin, id, MergeInput{}); !errors.Is(err, ErrUnavailable) || IsInvalid(err) {
		t.Fatalf("merge now: %v", err)
	}
	se.sg.mergeErr = nil
	se.setClock(se.clockNow().Add(time.Minute + time.Second))
	se.s.CheckArmedMerges(se.ctx)
	se.s.Wait()
	if m := se.sg.merged(); len(m) != 1 {
		t.Fatalf("merges = %+v", m)
	}
}

// A certificate that doesn't verify, a redirect where the API should be and
// a 200 login page are the forge's setup, not an outage: not ErrUnavailable
// (so not retried as one), an InvalidError saying the cause, with the host
// (WP-1370).
func TestForgeDownSetupFaultsAreRefusals(t *testing.T) {
	f := &Forge{BaseURL: "https://git.example.com"}
	jerr := json.Unmarshal([]byte("<html>"), new(map[string]any))
	for name, c := range map[string]struct {
		err  error
		want string
	}{
		"certificate": {&url.Error{Op: "Get", URL: "https://git.example.com/", Err: &tls.CertificateVerificationError{}}, "certificate"},
		"redirect":    {&forgeAPIError{method: "GET", path: "/repos/a/b", status: 302}, "redirect"},
		"login page":  {fmt.Errorf("decode: %w", jerr), "isn't its API"},
	} {
		got := forgeDown(f, time.Second, c.err)
		if errors.Is(got, ErrUnavailable) {
			t.Errorf("%s: treated as an outage", name)
		}
		if !IsInvalid(got) || !strings.Contains(got.Error(), c.want) || !strings.Contains(got.Error(), "git.example.com") {
			t.Errorf("%s: %v", name, got)
		}
		if forgeSetupFault(got) == "" || !shipRefused(got) {
			t.Errorf("%s: the ship's classifier disagrees", name)
		}
	}
}
