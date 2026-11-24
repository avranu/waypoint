package core

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// envT is a project with an editor, a viewer and someone outside it, and a
// health-check client that may reach the loopback test servers.
type envT struct {
	*env
	p                      *Project
	editor, viewer, nobody *Actor
	sub                    *Subscription
}

func newEnvT(t *testing.T) *envT {
	t.Helper()
	e := newEnv(t)
	e.s.envs.http = EgressPolicy{AllowLoopback: true}.client(5 * time.Second)
	x := &envT{env: e, p: e.project(e.admin, "Alpha", "kanban")}
	x.editor, x.viewer, x.nobody = e.user("eddie", false), e.user("vera", false), e.user("nobody", false)
	for u, role := range map[*Actor]string{x.editor: "editor", x.viewer: "viewer"} {
		if err := e.s.SetMember(e.ctx, e.admin, x.p.Key, u.User.Username, role); err != nil {
			t.Fatal(err)
		}
	}
	x.sub = e.s.Hub.Subscribe(e.admin.User.ID)
	t.Cleanup(func() { e.s.Hub.Unsubscribe(x.sub) })
	return x
}

// events drains what the hub has published since the last call.
func (x *envT) events() []*Event {
	var out []*Event
	for {
		select {
		case ev := <-x.sub.C:
			out = append(out, ev)
		default:
			return out
		}
	}
}

func hasEvent(evs []*Event, kind string) *Event {
	for _, e := range evs {
		if e.Kind == kind {
			return e
		}
	}
	return nil
}

func (x *envT) create(name, url string, mod ...func(*EnvironmentInput)) *EnvironmentDetail {
	x.t.Helper()
	in := EnvironmentInput{Name: &name, URL: &url}
	for _, f := range mod {
		f(&in)
	}
	d, err := x.s.CreateEnvironment(x.ctx, x.editor, x.p.Key, in)
	if err != nil {
		x.t.Fatalf("create %s: %v", name, err)
	}
	return d
}

// check runs one health check now and returns the environment.
func (x *envT) check(id int64) *Environment {
	x.t.Helper()
	if err := x.s.checkEnv(x.ctx, id); err != nil {
		x.t.Fatal(err)
	}
	e, err := x.s.GetEnvironment(x.ctx, x.admin, id)
	if err != nil {
		x.t.Fatal(err)
	}
	return e.Environment
}

// flaky is a server whose answer the test sets.
type flaky struct {
	code  atomic.Int32
	body  atomic.Value
	delay atomic.Int64
	hits  atomic.Int32
}

func newFlaky(t *testing.T) (*flaky, *httptest.Server) {
	f := &flaky{}
	f.code.Store(200)
	f.body.Store("ok")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		time.Sleep(time.Duration(f.delay.Load()) * time.Millisecond)
		w.WriteHeader(int(f.code.Load()))
		_, _ = w.Write([]byte(f.body.Load().(string)))
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

func TestEnvironmentAccess(t *testing.T) {
	x := newEnvT(t)
	d := x.create("staging", "https://staging.example.com")
	if d.Kind != EnvStaging || d.IntervalS != 60 || d.Status != EnvUnknown || d.Project != x.p.Key {
		t.Errorf("created %+v", d.Environment)
	}
	if _, err := x.s.GetEnvironment(x.ctx, x.viewer, d.ID); err != nil {
		t.Errorf("viewer get: %v", err)
	}
	if l, err := x.s.ListEnvironments(x.ctx, x.viewer, "", false); err != nil || len(l) != 1 {
		t.Errorf("viewer list: %v %v", l, err)
	}
	n := "prod"
	if _, err := x.s.CreateEnvironment(x.ctx, x.viewer, x.p.Key, EnvironmentInput{Name: &n}); !errors.Is(err, ErrForbidden) {
		t.Errorf("viewer create: %v", err)
	}
	u := "https://x.example.com"
	if _, err := x.s.UpdateEnvironment(x.ctx, x.viewer, d.ID, EnvironmentInput{URL: &u}); !errors.Is(err, ErrForbidden) {
		t.Errorf("viewer update: %v", err)
	}
	if _, err := x.s.StopEnvironment(x.ctx, x.viewer, d.ID); !errors.Is(err, ErrForbidden) {
		t.Errorf("viewer stop: %v", err)
	}
	if _, err := x.s.GetEnvironment(x.ctx, x.nobody, d.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("outsider get: %v", err)
	}
	if _, err := x.s.ListEnvironments(x.ctx, x.nobody, x.p.Key, false); err == nil {
		t.Error("outsider listed a project's environments")
	}
	if l, _ := x.s.ListEnvironments(x.ctx, x.nobody, "", false); len(l) != 0 {
		t.Errorf("outsider sees %d", len(l))
	}
	ro := &Actor{User: x.editor.User, ReadOnly: true}
	if _, err := x.s.UpdateEnvironment(x.ctx, ro, d.ID, EnvironmentInput{URL: &u}); err == nil {
		t.Error("read-only token updated")
	}
	agent := &Actor{User: x.editor.User, Agent: "anthropic/claude-code"}
	if err := x.s.DeleteEnvironment(x.ctx, agent, d.ID); !errors.Is(err, ErrForbidden) {
		t.Errorf("agent delete: %v", err)
	}
	if _, err := x.s.StopEnvironment(x.ctx, agent, d.ID); err != nil {
		t.Errorf("agent stop: %v", err)
	}
	if err := x.s.DeleteEnvironment(x.ctx, x.editor, d.ID); err != nil {
		t.Errorf("editor delete: %v", err)
	}
	if _, err := x.s.GetEnvironment(x.ctx, x.admin, d.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("after delete: %v", err)
	}
}

func TestEnvironmentValidation(t *testing.T) {
	x := newEnvT(t)
	x.create("staging", "https://staging.example.com")
	dup := "Staging"
	if _, err := x.s.CreateEnvironment(x.ctx, x.editor, x.p.Key, EnvironmentInput{Name: &dup}); !IsInvalid(err) {
		t.Errorf("duplicate name: %v", err)
	}
	for name, mod := range map[string]func(*EnvironmentInput){
		"bad url":        func(in *EnvironmentInput) { in.URL = ptr("ftp://x") },
		"bad kind":       func(in *EnvironmentInput) { in.Kind = ptr("moon") },
		"fast interval":  func(in *EnvironmentInput) { in.IntervalS = ptr(2) },
		"bad status":     func(in *EnvironmentInput) { in.ExpectedStatus = ptr(42) },
		"bad field":      func(in *EnvironmentInput) { in.CommitField = ptr("a b") },
		"version no key": func(in *EnvironmentInput) { in.VersionURL = ptr("https://x.test/v") },
		"temporary prod": func(in *EnvironmentInput) { in.Kind, in.Ephemeral = ptr(EnvProduction), ptr(true) },
		"name":           func(in *EnvironmentInput) { in.Name = ptr("has space") },
	} {
		in := EnvironmentInput{Name: ptr("other")}
		mod(&in)
		if _, err := x.s.CreateEnvironment(x.ctx, x.editor, x.p.Key, in); !IsInvalid(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, want := range map[string]string{"prod": EnvProduction, "Staging": EnvStaging, "dev": EnvDev, "pr-12": EnvPreview} {
		if got := InferEnvKind(name); got != want {
			t.Errorf("InferEnvKind(%s) = %s", name, got)
		}
	}
	if _, err := x.s.UpdateProject(x.ctx, x.admin, x.p.Key, ProjectInput{Settings: &ProjectSettings{DevEnvURL: "https://dev.example.com"}}); !IsInvalid(err) {
		t.Errorf("pattern without {name}: %v", err)
	}
	if _, err := x.s.UpdateProject(x.ctx, x.admin, x.p.Key, ProjectInput{Settings: &ProjectSettings{DevEnvURL: "https://{name}.dev.example.com"}}); err != nil {
		t.Errorf("pattern: %v", err)
	}
}

func TestDeploymentLifecycle(t *testing.T) {
	x := newEnvT(t)
	x.events()
	d, err := x.s.RecordDeploymentNamed(x.ctx, x.editor, x.p.Key, "prod", DeployInput{Commit: "ABCDEF1234", Branch: "main", Status: "started", URL: "https://ci.example.com/run/9"})
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != DeployStarted || d.Commit != "abcdef1234" || d.ByKind != DeployByCI || d.URL == "" || d.FinishedAt != nil {
		t.Errorf("started: %+v", d)
	}
	evs := x.events()
	if hasEvent(evs, EvEnvCreated) == nil || hasEvent(evs, EvEnvDeployed) != nil {
		t.Errorf("events after start: %v", kinds(evs))
	}
	list, _ := x.s.ListEnvironments(x.ctx, x.editor, x.p.Key, false)
	if len(list) != 1 || list[0].Name != "prod" || list[0].Kind != EnvProduction || list[0].Commit != "" {
		t.Fatalf("created %+v", list)
	}
	if again, _ := x.s.RecordDeploymentNamed(x.ctx, x.editor, x.p.Key, "prod", DeployInput{Commit: "abcdef1234", Status: "started"}); again.ID != d.ID {
		t.Errorf("a second start made deployment %d, not %d", again.ID, d.ID)
	}
	x.setClock(x.clockNow().Add(90 * time.Second))
	done, err := x.s.RecordDeploymentNamed(x.ctx, x.editor, x.p.Key, "prod", DeployInput{Commit: "abcdef1", Status: "succeeded", Version: "1.4.0"})
	if err != nil {
		t.Fatal(err)
	}
	if done.ID != d.ID || done.Status != DeploySucceeded || done.FinishedAt == nil || done.Version != "1.4.0" || done.Branch != "main" || done.URL == "" {
		t.Errorf("finished: %+v", done)
	}
	ev := hasEvent(x.events(), EvEnvDeployed)
	if ev == nil || ev.Data["commit"] != "abcdef1234" || ev.Data["status"] != DeploySucceeded || ev.Data["name"] != "prod" || ev.ProjectID != x.p.ID {
		t.Fatalf("env.deployed: %+v", ev)
	}
	got, _ := x.s.GetEnvironmentNamed(x.ctx, x.viewer, x.p.Key, "prod")
	if got.Commit != "abcdef1234" || got.Branch != "main" || got.Version != "1.4.0" || got.DeployedAt == nil || len(got.Deployments) != 1 {
		t.Errorf("environment after deploy: %+v", got.Environment)
	}
	x.setClock(x.clockNow().Add(time.Hour))
	f, err := x.s.RecordDeploymentNamed(x.ctx, x.editor, x.p.Key, "prod", DeployInput{Commit: "1111111", Status: "failed", Notes: "migration failed"})
	if err != nil || f.Status != DeployFailed {
		t.Fatalf("failed: %+v %v", f, err)
	}
	if ev := hasEvent(x.events(), EvEnvDeployFailed); ev == nil || ev.Data["commit"] != "1111111" {
		t.Errorf("env.deploy_failed: %+v", ev)
	}
	got, _ = x.s.GetEnvironmentNamed(x.ctx, x.viewer, x.p.Key, "prod")
	if got.Commit != "abcdef1234" || got.LastDeploy == nil || got.LastDeploy.Status != DeployFailed || len(got.Deployments) != 2 {
		t.Errorf("after a failure: %+v / %+v", got.Environment, got.LastDeploy)
	}
	if _, err := x.s.RecordDeployment(x.ctx, x.editor, got.ID, DeployInput{Commit: "2222222", Branch: "release"}); err != nil {
		t.Fatal(err)
	}
	got, _ = x.s.GetEnvironment(x.ctx, x.viewer, got.ID)
	if got.Commit != "2222222" || got.Branch != "release" || got.Version != "" {
		t.Errorf("redeploy: %s %s %q", got.Commit, got.Branch, got.Version)
	}
	if d := got.LastDeploy; d.ByKind != DeployByPerson || d.By == nil || d.By.Username != "eddie" {
		t.Errorf("by: %+v", d)
	}
	agent := &Actor{User: x.editor.User, Agent: "anthropic/claude-code"}
	if d, _ := x.s.RecordDeployment(x.ctx, agent, got.ID, DeployInput{Commit: "3333333"}); d.ByKind != DeployByAgent || d.Agent != "anthropic/claude-code" {
		t.Errorf("agent deploy: %+v", d)
	}
	if d, _ := x.s.RecordDeployment(x.ctx, agent, got.ID, DeployInput{Commit: "3333334", Source: "ci"}); d == nil || d.ByKind != DeployByCI {
		t.Errorf("source ci from an agent: %+v", d)
	}
	if _, err := x.s.RecordDeploymentNamed(x.ctx, x.viewer, x.p.Key, "prod", DeployInput{Commit: "abcdef1"}); !errors.Is(err, ErrForbidden) {
		t.Errorf("viewer deploy: %v", err)
	}
	if _, err := x.s.RecordDeploymentNamed(x.ctx, x.nobody, x.p.Key, "brand-new", DeployInput{Commit: "abcdef1"}); err == nil {
		t.Error("an outsider created an environment")
	}
	if _, err := x.s.RecordDeploymentNamed(x.ctx, x.editor, x.p.Key, "prod", DeployInput{}); !IsInvalid(err) {
		t.Errorf("empty deploy: %v", err)
	}
	if _, err := x.s.RecordDeploymentNamed(x.ctx, x.editor, x.p.Key, "prod", DeployInput{Commit: "not a sha"}); !IsInvalid(err) {
		t.Errorf("bad commit: %v", err)
	}
	if _, err := x.s.RecordDeploymentNamed(x.ctx, x.editor, x.p.Key, "prod", DeployInput{Commit: "abcdef1", Status: "maybe"}); !IsInvalid(err) {
		t.Errorf("bad status: %v", err)
	}
}

func kinds(evs []*Event) []string {
	var out []string
	for _, e := range evs {
		out = append(out, e.Kind)
	}
	return out
}

func TestStatusTransitionsAndDebounce(t *testing.T) {
	x := newEnvT(t)
	f, srv := newFlaky(t)
	d := x.create("staging", srv.URL)
	x.events()

	e := x.check(d.ID)
	if e.Status != EnvUp || e.StatusSince == nil || e.HTTPCode == nil || *e.HTTPCode != 200 || e.LatencyMS == nil {
		t.Fatalf("first check: %+v", e)
	}
	if hasEvent(x.events(), EvEnvStatus) != nil {
		t.Error("unknown -> up emitted env.status")
	}
	since := *e.StatusSince

	x.setClock(x.clockNow().Add(time.Minute))
	f.code.Store(503)
	e = x.check(d.ID)
	if e.Status != EnvUp || e.Error != "HTTP 503" || e.failStreak != 1 || !e.StatusSince.Equal(since) {
		t.Errorf("after one failure: %s %q streak %d", e.Status, e.Error, e.failStreak)
	}
	if hasEvent(x.events(), EvEnvStatus) != nil {
		t.Error("a single failure emitted env.status")
	}
	x.setClock(x.clockNow().Add(time.Minute))
	e = x.check(d.ID)
	if e.Status != EnvDown || !e.StatusSince.After(since) || e.HTTPCode == nil || *e.HTTPCode != 503 {
		t.Errorf("after two failures: %+v", e)
	}
	ev := hasEvent(x.events(), EvEnvStatus)
	if ev == nil || ev.Data["status"] != EnvDown || ev.Data["previous"] != EnvUp || ev.Data["name"] != "staging" || ev.Data["error"] != "HTTP 503" {
		t.Fatalf("env.status: %+v", ev)
	}
	x.setClock(x.clockNow().Add(time.Minute))
	down := *e.StatusSince
	e = x.check(d.ID)
	if hasEvent(x.events(), EvEnvStatus) != nil || !e.StatusSince.Equal(down) {
		t.Error("still down, but the status changed or was announced again")
	}
	x.setClock(x.clockNow().Add(time.Minute))
	f.code.Store(200)
	e = x.check(d.ID)
	if e.Status != EnvUp || e.Error != "" || e.failStreak != 0 {
		t.Errorf("recovered: %+v", e)
	}
	if ev := hasEvent(x.events(), EvEnvStatus); ev == nil || ev.Data["status"] != EnvUp || ev.Data["previous"] != EnvDown {
		t.Errorf("recovery event: %+v", ev)
	}
	old := envSlow
	envSlow = 30 * time.Millisecond
	t.Cleanup(func() { envSlow = old })
	x.setClock(x.clockNow().Add(time.Minute))
	f.delay.Store(80)
	e = x.check(d.ID)
	if e.Status != EnvDegraded {
		t.Errorf("slow: %s", e.Status)
	}
	if ev := hasEvent(x.events(), EvEnvStatus); ev == nil || ev.Data["status"] != EnvDegraded {
		t.Errorf("degraded event: %+v", ev)
	}
	f.delay.Store(0)
	f.code.Store(302)
	x.s.UpdateEnvironment(x.ctx, x.editor, d.ID, EnvironmentInput{ExpectedStatus: ptr(302)})
	if e = x.check(d.ID); e.Status != EnvUp {
		t.Errorf("expected 302: %s %q", e.Status, e.Error)
	}
}

func TestEgressBlocksLoopbackByDefault(t *testing.T) {
	e := newEnv(t)
	_, srv := newFlaky(t)
	p := e.project(e.admin, "Alpha", "kanban")
	d, err := e.s.CreateEnvironment(e.ctx, e.admin, p.Key, EnvironmentInput{Name: ptr("staging"), URL: &srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.s.checkEnv(e.ctx, d.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := e.s.GetEnvironment(e.ctx, e.admin, d.ID)
	if got.Error != "that address is not allowed" || got.Status != EnvUnknown {
		t.Errorf("loopback check: %q %s", got.Error, got.Status)
	}
}

func TestTLSExpiryWarning(t *testing.T) {
	x := newEnvT(t)
	x.setClock(time.Now()) // certificates are checked against the real time
	cert, pool := selfSigned(t, x.clockNow().Add(5*24*time.Hour))
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) }))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	d := x.create("staging", srv.URL)

	if e := x.check(d.ID); e.Error != "certificate not trusted" || e.TLSExpiresAt != nil {
		t.Errorf("untrusted: %q %v", e.Error, e.TLSExpiresAt)
	}
	x.s.envs.http = &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
	e := x.check(d.ID)
	if e.Status != EnvUp || e.TLSExpiresAt == nil || !e.TLSWarning {
		t.Fatalf("expiring cert: %s %v warn=%v", e.Status, e.TLSExpiresAt, e.TLSWarning)
	}
	if days := e.TLSExpiresAt.Sub(x.clockNow()).Hours() / 24; days < 4.9 || days > 5.1 {
		t.Errorf("expires in %.1f days", days)
	}
	x.s.envs.http = EgressPolicy{}.client(time.Second)
	if e = x.check(d.ID); e.TLSExpiresAt == nil {
		t.Error("a failed check cleared the certificate expiry")
	}
	cert2, pool2 := selfSigned(t, x.clockNow().Add(90*24*time.Hour))
	srv2 := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv2.TLS = &tls.Config{Certificates: []tls.Certificate{cert2}, MinVersion: tls.VersionTLS12}
	srv2.StartTLS()
	t.Cleanup(srv2.Close)
	x.s.envs.http = &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool2, MinVersion: tls.VersionTLS12}}}
	d2 := x.create("prod", srv2.URL)
	if e := x.check(d2.ID); e.TLSWarning || e.TLSExpiresAt == nil {
		t.Errorf("long-lived cert: warn=%v exp=%v", e.TLSWarning, e.TLSExpiresAt)
	}
}

// selfSigned makes a certificate for 127.0.0.1 that expires at notAfter,
// and a pool that trusts it. (Its start is a day before the test clock.)
func selfSigned(t *testing.T, notAfter time.Time) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "127.0.0.1"},
		NotBefore: notAfter.Add(-100 * 24 * time.Hour), NotAfter: notAfter,
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, BasicConstraintsValid: true, IsCA: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, pool
}

func TestVersionDetection(t *testing.T) {
	x := newEnvT(t)
	f, srv := newFlaky(t)
	f.body.Store(`{"version":"2.1.0","git":{"commit":"AAAAAAA1111-dirty","branch":"main"}}`)
	d := x.create("staging", srv.URL, func(in *EnvironmentInput) {
		in.VersionURL, in.CommitField, in.BranchField, in.VersionField = &srv.URL, ptr("git.commit"), ptr("git.branch"), ptr("version")
	})
	x.events()

	e := x.check(d.ID)
	if e.Commit != "aaaaaaa1111" || e.Branch != "main" || e.Version != "2.1.0" || e.Status != EnvUp {
		t.Fatalf("detected: %+v", e)
	}
	det, _ := x.s.GetEnvironment(x.ctx, x.admin, d.ID)
	if len(det.Deployments) != 1 || det.Deployments[0].Status != DeployDetected || det.Deployments[0].ByKind != DeployByDetected {
		t.Fatalf("deployments: %+v", det.Deployments)
	}
	if ev := hasEvent(x.events(), EvEnvDeployed); ev == nil || ev.Data["status"] != DeployDetected || ev.Data["commit"] != "aaaaaaa1111" {
		t.Errorf("env.deployed: %+v", ev)
	}
	x.setClock(x.clockNow().Add(time.Minute))
	x.check(d.ID)
	if det, _ = x.s.GetEnvironment(x.ctx, x.admin, d.ID); len(det.Deployments) != 1 {
		t.Errorf("repeat check made %d deployments", len(det.Deployments))
	}
	x.setClock(x.clockNow().Add(time.Minute))
	if _, err := x.s.RecordDeploymentNamed(x.ctx, x.editor, x.p.Key, "staging", DeployInput{Commit: "bbbbbbb2222", Status: "succeeded"}); err != nil {
		t.Fatal(err)
	}
	x.setClock(x.clockNow().Add(time.Minute))
	e = x.check(d.ID)
	if e.Status != EnvDegraded || !e.Mismatch || e.Commit != "bbbbbbb2222" {
		t.Errorf("rollout: %s mismatch=%v commit=%s", e.Status, e.Mismatch, e.Commit)
	}
	f.code.Store(500)
	x.setClock(x.clockNow().Add(time.Minute))
	if e = x.check(d.ID); !e.Mismatch {
		t.Errorf("a failed check cleared the mismatch: %+v", e)
	}
	f.code.Store(200)
	f.code.Store(500)
	x.setClock(x.clockNow().Add(time.Minute))
	if e = x.check(d.ID); !e.Mismatch {
		t.Errorf("a failed check cleared the mismatch: %+v", e)
	}
	f.code.Store(200)
	f.body.Store(`{"version":"2.2.0","git":{"commit":"bbbbbbb2222","branch":"main"}}`)
	x.setClock(x.clockNow().Add(time.Minute))
	e = x.check(d.ID)
	if e.Status != EnvUp || e.Mismatch || e.Version != "2.2.0" {
		t.Errorf("rolled out: %s mismatch=%v version=%s", e.Status, e.Mismatch, e.Version)
	}
	f.body.Store(`{"version":"2.3.0","git":{"commit":"ccccccc3333","branch":"hotfix"}}`)
	x.setClock(x.clockNow().Add(time.Hour))
	e = x.check(d.ID)
	if e.Commit != "ccccccc3333" || e.Branch != "hotfix" || e.Status != EnvUp {
		t.Errorf("hotfix: %+v", e)
	}
	det, _ = x.s.GetEnvironment(x.ctx, x.admin, d.ID)
	if det.Deployments[0].Status != DeployDetected || det.Deployments[0].Commit != "ccccccc3333" {
		t.Errorf("latest deployment: %+v", det.Deployments[0])
	}
	_, hsrv := newFlaky(t)
	g, vs := newFlaky(t)
	g.body.Store(`{"build":{"id":"r-77"}}`)
	d2 := x.create("dev", hsrv.URL, func(in *EnvironmentInput) {
		in.VersionURL, in.VersionField = &vs.URL, ptr("build.id")
	})
	if e := x.check(d2.ID); e.Version != "r-77" || e.Commit != "" {
		t.Errorf("version only: %+v", e)
	}
	g.code.Store(500)
	if e := x.check(d2.ID); e.Status != EnvUp {
		t.Errorf("version endpoint down: %s", e.Status)
	}
}

func TestJSONPath(t *testing.T) {
	r := parseEnvReport([]byte(`{"a":{"b":[{"sha":"1234567890ab"}]},"n":42,"ok":true,"obj":{}}`), "a.b.0.sha", "", "n")
	if r.commit != "1234567890ab" || r.version != "42" {
		t.Errorf("report %+v", r)
	}
	if r := parseEnvReport([]byte(`{"v":"x"}`), "obj.nope", "", "v"); r.commit != "" || r.version != "x" {
		t.Errorf("missing path: %+v", r)
	}
	if r := parseEnvReport([]byte(`not json`), "a", "", "b"); !r.empty() {
		t.Errorf("garbage: %+v", r)
	}
	if r := parseEnvReport([]byte(`{"c":"dirty"}`), "c", "", ""); r.commit != "" {
		t.Errorf("non-hex commit kept: %+v", r)
	}
}

func TestUpDownAndLeases(t *testing.T) {
	x := newEnvT(t)
	if _, err := x.s.UpEnvironment(x.ctx, x.editor, x.p.Key, EnvUpInput{Name: "a23x42s"}); !IsInvalid(err) {
		t.Fatalf("no address and no pattern: %v", err)
	}
	if _, err := x.s.UpdateProject(x.ctx, x.admin, x.p.Key, ProjectInput{Settings: &ProjectSettings{DevEnvURL: "https://{name}.dev.example.com"}}); err != nil {
		t.Fatal(err)
	}
	x.events()
	d, err := x.s.UpEnvironment(x.ctx, x.editor, x.p.Key, EnvUpInput{Name: "A23X42S", Branch: "feature/login", Commit: "ABC1234", TTLS: 3 * 3600})
	if err != nil {
		t.Fatal(err)
	}
	if d.Name != "a23x42s" || d.URL != "https://a23x42s.dev.example.com" || d.Kind != EnvDev || !d.Ephemeral || d.Commit != "abc1234" || d.Branch != "feature/login" || d.ExpiresAt == nil {
		t.Errorf("up: %+v", d.Environment)
	}
	if len(d.Deployments) != 1 || d.Deployments[0].Status != DeploySucceeded {
		t.Errorf("up recorded %+v", d.Deployments)
	}
	if hasEvent(x.events(), EvEnvCreated) == nil {
		t.Error("no env.created")
	}
	x.setClock(x.clockNow().Add(2 * time.Hour))
	d2, err := x.s.UpEnvironment(x.ctx, x.editor, x.p.Key, EnvUpInput{Name: "a23x42s", Commit: "abc1234"})
	if err != nil || d2.ID != d.ID || len(d2.Deployments) != 1 || !d2.ExpiresAt.After(*d.ExpiresAt) {
		t.Errorf("up again: %+v %v", d2, err)
	}
	if _, err := x.s.UpEnvironment(x.ctx, x.viewer, x.p.Key, EnvUpInput{Name: "z"}); !errors.Is(err, ErrForbidden) {
		t.Errorf("viewer up: %v", err)
	}
	if z, _ := x.s.UpEnvironment(x.ctx, x.editor, x.p.Key, EnvUpInput{Name: "zed", URL: "https://zed.example.org"}); z == nil || z.URL != "https://zed.example.org" {
		t.Errorf("explicit url: %+v", z)
	}
	if _, err := x.s.UpEnvironment(x.ctx, x.editor, x.p.Key, EnvUpInput{Name: "prodlike", Kind: "production", URL: "https://x.example.com"}); !IsInvalid(err) {
		t.Errorf("up as production: %v", err)
	}
	x.events()
	down, err := x.s.DownEnvironment(x.ctx, x.editor, x.p.Key, "a23x42s")
	if err != nil || !down.Stopped || down.ExpiresAt != nil {
		t.Fatalf("down: %+v %v", down, err)
	}
	if hasEvent(x.events(), EvEnvStopped) == nil {
		t.Error("no env.stopped")
	}
	if l, _ := x.s.ListEnvironments(x.ctx, x.editor, x.p.Key, false); len(l) != 1 || l[0].Name != "zed" {
		t.Errorf("stopped ones are folded away: %d", len(l))
	}
	if l, _ := x.s.ListEnvironments(x.ctx, x.editor, x.p.Key, true); len(l) != 2 {
		t.Errorf("with stopped: %d", len(l))
	}
	x.events()
	back, err := x.s.UpEnvironment(x.ctx, x.editor, x.p.Key, EnvUpInput{Name: "a23x42s", TTLS: 3600, Commit: "fff1234"})
	if err != nil || back.Stopped || back.Status != EnvUnknown {
		t.Errorf("revived: %+v %v", back, err)
	}
	restarts := 0
	for _, ev := range x.events() {
		if ev.Kind == EvEnvCreated && ev.Data["restarted"] == true {
			restarts++
		}
	}
	if restarts != 1 {
		t.Errorf("%d restarted events", restarts)
	}
	x.events()
	x.setClock(x.clockNow().Add(2 * time.Hour))
	x.s.envUpkeep(x.ctx)
	got, _ := x.s.GetEnvironment(x.ctx, x.admin, back.ID)
	if !got.Stopped {
		t.Error("expired lease left the environment running")
	}
	if ev := hasEvent(x.events(), EvEnvStopped); ev == nil || ev.Data["reason"] != "ttl" {
		t.Errorf("env.stopped: %+v", ev)
	}
	if _, err := x.s.RecordDeploymentNamed(x.ctx, x.editor, x.p.Key, "a23x42s", DeployInput{Commit: "def5678"}); err != nil {
		t.Fatal(err)
	}
	got, _ = x.s.GetEnvironment(x.ctx, x.admin, back.ID)
	if got.Stopped || got.ExpiresAt == nil || got.Commit != "def5678" {
		t.Errorf("deploy revived: %+v", got.Environment)
	}
	x.s.StopEnvironment(x.ctx, x.editor, back.ID)
	x.setClock(x.clockNow().Add(6 * 24 * time.Hour))
	x.s.envUpkeep(x.ctx)
	if _, err := x.s.GetEnvironment(x.ctx, x.admin, back.ID); err != nil {
		t.Errorf("gone after 6 days: %v", err)
	}
	x.setClock(x.clockNow().Add(2 * 24 * time.Hour))
	x.s.envUpkeep(x.ctx)
	if _, err := x.s.GetEnvironment(x.ctx, x.admin, back.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("kept after 8 days: %v", err)
	}
}

func TestUnreachableDevEnvironmentsStop(t *testing.T) {
	x := newEnvT(t)
	f, srv := newFlaky(t)
	up := func(name string, ttl int) *EnvironmentDetail {
		d, err := x.s.UpEnvironment(x.ctx, x.editor, x.p.Key, EnvUpInput{Name: name, URL: srv.URL, TTLS: 0})
		if err != nil {
			t.Fatal(err)
		}
		if ttl > 0 {
			x.s.UpdateEnvironment(x.ctx, x.editor, d.ID, EnvironmentInput{TTLS: &ttl})
		}
		return d
	}
	short, long := up("short", 3600), up("long", 0)
	prod := x.create("prod", srv.URL)
	f.code.Store(500)
	for _, id := range []int64{short.ID, long.ID, prod.ID} {
		x.check(id)
		x.check(id)
	}
	x.setClock(x.clockNow().Add(61 * time.Minute))
	x.s.envUpkeep(x.ctx)
	state := func(id int64) bool { e, _ := x.s.GetEnvironment(x.ctx, x.admin, id); return e.Stopped }
	if !state(short.ID) || state(long.ID) || state(prod.ID) {
		t.Errorf("after an hour: short=%v long=%v prod=%v", state(short.ID), state(long.ID), state(prod.ID))
	}
	x.setClock(x.clockNow().Add(3 * 24 * time.Hour))
	x.s.envUpkeep(x.ctx)
	if !state(long.ID) {
		t.Error("an environment down for three days kept running")
	}
	if state(prod.ID) {
		t.Error("production was stopped for being down")
	}
	f.code.Store(200)
	back := up("back", 3600)
	x.check(back.ID)
	x.setClock(x.clockNow().Add(5 * time.Hour))
	x.s.envUpkeep(x.ctx)
	if e, _ := x.s.GetEnvironment(x.ctx, x.admin, back.ID); e.Stopped {
		t.Error("a reachable environment was stopped")
	}
}

func TestUptimeAggregatesAndRetention(t *testing.T) {
	x := newEnvT(t)
	f, srv := newFlaky(t)
	d := x.create("prod", srv.URL)
	start := x.clockNow()
	for h := 0; h < 72; h++ {
		x.setClock(start.Add(time.Duration(h) * time.Hour))
		if h >= 24 && h < 30 {
			f.code.Store(500)
		} else {
			f.code.Store(200)
		}
		x.check(d.ID)
	}
	det, _ := x.s.GetEnvironment(x.ctx, x.admin, d.ID)
	if len(det.Days) != 90 {
		t.Fatalf("days: %d", len(det.Days))
	}
	var checks, ok int
	for _, day := range det.Days {
		checks, ok = checks+day.Checks, ok+day.OK
	}
	if checks != 72 || ok != 66 {
		t.Errorf("checks %d ok %d", checks, ok)
	}
	last := det.Days[89]
	if last.Date != x.clockNow().Format(dateLayout) || last.Uptime == nil {
		t.Errorf("last day: %+v", last)
	}
	if det.Uptime30d == nil || *det.Uptime30d != pct(66, 72) {
		t.Errorf("uptime30d = %v", det.Uptime30d)
	}
	if det.Uptime24h == nil || *det.Uptime24h != 100 || len(det.Spark) != 24 {
		t.Errorf("uptime24h = %v, spark %d", det.Uptime24h, len(det.Spark))
	}
	if det.Days[0].Uptime != nil {
		t.Errorf("a day without checks has uptime %v", *det.Days[0].Uptime)
	}
	if len(det.History) == 0 {
		t.Error("no history")
	}
	x.setClock(x.clockNow().Add(time.Hour))
	f.code.Store(200)
	x.check(d.ID)
	f.code.Store(500)
	x.check(d.ID)
	f.code.Store(200)
	x.check(d.ID)
	var n, bad int
	x.s.db.R.QueryRow(`SELECT count(*), sum(1 - ok) FROM environment_checks WHERE at = ?`, x.clockNow().Truncate(time.Minute).UnixMilli()).Scan(&n, &bad)
	if n != 1 || bad != 1 {
		t.Errorf("samples in the minute: %d, failed %d", n, bad)
	}
	var minutes, hours int
	x.s.envUpkeep(x.ctx)
	x.s.db.R.QueryRow(`SELECT count(*) FROM environment_checks`).Scan(&minutes)
	if minutes > 49 {
		t.Errorf("kept %d minute samples", minutes)
	}
	x.s.db.R.QueryRow(`SELECT count(*) FROM environment_hourly`).Scan(&hours)
	if hours != 73 {
		t.Errorf("hourly rows before retention: %d", hours)
	}
	x.setClock(x.clockNow().Add(91 * 24 * time.Hour))
	x.s.envUpkeep(x.ctx)
	x.s.db.R.QueryRow(`SELECT count(*) FROM environment_hourly`).Scan(&hours)
	x.s.db.R.QueryRow(`SELECT count(*) FROM environment_checks`).Scan(&minutes)
	if hours != 0 || minutes != 0 {
		t.Errorf("after 91 days: %d hourly, %d minute", hours, minutes)
	}
}

func TestDueChecksAreBoundedAndSpread(t *testing.T) {
	x := newEnvT(t)
	var cur, peak atomic.Int32
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := cur.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		hits.Add(1)
		time.Sleep(40 * time.Millisecond)
		cur.Add(-1)
	}))
	t.Cleanup(srv.Close)
	for i := 0; i < 20; i++ {
		x.create(fmt.Sprintf("env%02d", i), srv.URL)
	}
	x.create("off", srv.URL, func(in *EnvironmentInput) { in.IntervalS = ptr(0) })
	stopped := x.create("stopped", srv.URL)
	x.s.StopEnvironment(x.ctx, x.editor, stopped.ID)
	x.create("noaddr", "")

	if n := x.s.runDueEnvChecks(x.ctx); n != 20 || hits.Load() != 20 {
		t.Fatalf("ran %d, %d requests", n, hits.Load())
	}
	if peak.Load() > envMaxParallel || peak.Load() < 2 {
		t.Errorf("peak concurrency %d (limit %d)", peak.Load(), envMaxParallel)
	}
	if n := x.s.runDueEnvChecks(x.ctx); n != 0 {
		t.Errorf("checked %d again at once", n)
	}
	x.setClock(x.clockNow().Add(50 * time.Second))
	if n := x.s.runDueEnvChecks(x.ctx); n != 0 {
		t.Errorf("checked %d after 50s of a 60s interval", n)
	}
	x.setClock(x.clockNow().Add(20 * time.Second))
	if n := x.s.runDueEnvChecks(x.ctx); n != 20 {
		t.Errorf("checked %d after 70s", n)
	}
	list, _ := x.s.ListEnvironments(x.ctx, x.admin, x.p.Key, false)
	x.s.UpdateEnvironment(x.ctx, x.editor, list[0].ID, EnvironmentInput{HealthURL: ptr(srv.URL + "/health")})
	if n := x.s.runDueEnvChecks(x.ctx); n != 1 {
		t.Errorf("after changing an address: %d", n)
	}
	ctx, cancel := context.WithCancel(x.ctx)
	done := make(chan struct{})
	go func() { x.s.RunEnvChecks(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunEnvChecks did not stop")
	}
	if _, err := x.s.CheckEnvironmentNow(x.ctx, x.editor, stopped.ID); !IsInvalid(err) {
		t.Errorf("check a stopped environment: %v", err)
	}
	if _, err := x.s.CheckEnvironmentNow(x.ctx, x.viewer, list[0].ID); !errors.Is(err, ErrForbidden) {
		t.Errorf("viewer check now: %v", err)
	}
}

func TestBehindBranchHeadAndCommitLinks(t *testing.T) {
	x := newEnvT(t)
	res, err := x.s.db.W.Exec(`INSERT INTO forges(kind, name, base_url, created_at) VALUES ('forgejo','Forge','https://git.example.com', 0)`)
	if err != nil {
		t.Fatal(err)
	}
	fid, _ := res.LastInsertId()
	if _, err := x.s.db.W.Exec(`INSERT INTO project_repos(project_id, forge_id, repo, created_at) VALUES (?,?, 'acme/app', 0)`, x.p.ID, fid); err != nil {
		t.Fatal(err)
	}
	if _, err := x.s.RecordDeploymentNamed(x.ctx, x.editor, x.p.Key, "staging", DeployInput{Commit: "aaaaaaa1111", Branch: "main"}); err != nil {
		t.Fatal(err)
	}
	got, _ := x.s.GetEnvironmentNamed(x.ctx, x.viewer, x.p.Key, "staging")
	if got.Repo != "acme/app" || got.CommitURL != "https://git.example.com/acme/app/commit/aaaaaaa1111" || got.Behind {
		t.Errorf("before any push: %+v", got.Environment)
	}
	if got.Deployments[0].CommitURL != got.CommitURL {
		t.Errorf("deployment link: %q", got.Deployments[0].CommitURL)
	}
	push := func(repo, branch, after string) {
		err := x.s.write(x.ctx, SystemActor(), func(t *tx) error {
			t.emit(&Event{Kind: EvRepoPushed, Data: map[string]any{"repo": repo, "branch": branch, "after": after}})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	x.events()
	push("acme/app", "main", "AAAAAAA1111222233334444")
	if got, _ = x.s.GetEnvironmentNamed(x.ctx, x.viewer, x.p.Key, "staging"); got.Behind {
		t.Error("behind at its own head")
	}
	push("acme/app", "other", "bbbbbbb2222")
	if got, _ = x.s.GetEnvironmentNamed(x.ctx, x.viewer, x.p.Key, "staging"); got.Behind {
		t.Error("behind because of another branch")
	}
	push("Acme/App", "refs/heads/main", "bbbbbbb2222333")
	got, _ = x.s.GetEnvironmentNamed(x.ctx, x.viewer, x.p.Key, "staging")
	if !got.Behind || got.Head != "bbbbbbb2222333" {
		t.Errorf("after a push: behind=%v head=%q", got.Behind, got.Head)
	}
	if ev := hasEvent(x.events(), EvEnvUpdated); ev == nil || ev.ProjectID != x.p.ID {
		t.Errorf("no live update for the push: %v", ev)
	}
	x.s.RecordDeploymentNamed(x.ctx, x.editor, x.p.Key, "staging", DeployInput{Commit: "bbbbbbb2222", Branch: "main"})
	if got, _ = x.s.GetEnvironmentNamed(x.ctx, x.viewer, x.p.Key, "staging"); got.Behind {
		t.Error("still behind after deploying the head")
	}
	push("acme/app", "main", "0000000000000000000000000000000000000000")
	if got, _ = x.s.GetEnvironmentNamed(x.ctx, x.viewer, x.p.Key, "staging"); got.Behind {
		t.Error("a deleted branch put it behind")
	}

	at, err := x.s.EnvironmentsForCommits(x.ctx, x.viewer, "acme/app", []string{"bbbbbbb2222333444555", "aaaaaaa1111222233334444", "ffffffffff"})
	if err != nil {
		t.Fatal(err)
	}
	if b := at["bbbbbbb2222333444555"]; len(b) != 1 || b[0].Name != "staging" || !b[0].Current {
		t.Errorf("bbbbbbb: %+v", b)
	}
	if a := at["aaaaaaa1111222233334444"]; len(a) != 1 || a[0].Current {
		t.Errorf("aaaaaaa should have run there and been replaced: %+v", a)
	}
	if len(at["ffffffffff"]) != 0 {
		t.Errorf("ffffffffff: %+v", at["ffffffffff"])
	}
	if other, _ := x.s.EnvironmentsForCommits(x.ctx, x.viewer, "acme/other", []string{"bbbbbbb2222333444555"}); len(other) != 0 {
		t.Errorf("another repo: %+v", other)
	}
	if hidden, _ := x.s.EnvironmentsForCommits(x.ctx, x.nobody, "acme/app", []string{"bbbbbbb2222333444555"}); len(hidden) != 0 {
		t.Errorf("an outsider sees %+v", hidden)
	}

	many := []string{"bbbbbbb2222333444555"}
	for i := 0; i < 2000; i++ {
		many = append(many, fmt.Sprintf("%x", sha256.Sum256([]byte{byte(i), byte(i >> 8)})))
	}
	if at, err := x.s.EnvironmentsForCommits(x.ctx, x.viewer, "acme/app", many); err != nil || len(at["bbbbbbb2222333444555"]) != 1 {
		t.Errorf("a long list: %v %+v", err, at)
	}
}

func TestEnvironmentSummary(t *testing.T) {
	x := newEnvT(t)
	f, srv := newFlaky(t)
	a, b := x.create("prod", srv.URL), x.create("staging", srv.URL)
	x.create("dev", "")
	x.check(a.ID)
	f.code.Store(500)
	x.check(b.ID)
	x.check(b.ID)
	other := x.project(x.admin, "Beta", "kanban")
	x.s.CreateEnvironment(x.ctx, x.admin, other.Key, EnvironmentInput{Name: ptr("prod")})
	sum, err := x.s.SummarizeEnvironments(x.ctx, x.viewer)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Total != 3 || sum.Up != 1 || sum.Down != 1 || sum.Unknown != 1 || len(sum.Projects) != 1 {
		t.Errorf("summary %+v", sum)
	}
	if c := sum.Projects[0]; c.Project != x.p.Key || len(c.DownNames) != 1 || c.DownNames[0] != "staging" {
		t.Errorf("project counts %+v", c)
	}
	if all, _ := x.s.SummarizeEnvironments(x.ctx, x.admin); all.Total != 4 || len(all.Projects) != 2 {
		t.Errorf("admin summary %+v", all)
	}
}

func TestEnvironmentWebhookDelivery(t *testing.T) {
	x := newEnvT(t)
	var mu sync.Mutex
	var bodies []string
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := new(strings.Builder)
		b := make([]byte, 4096)
		n, _ := r.Body.Read(b)
		buf.Write(b[:n])
		mu.Lock()
		bodies = append(bodies, buf.String())
		mu.Unlock()
	}))
	t.Cleanup(hook.Close)
	x.s.cfg.Egress.AllowLoopback = true
	x.s.http = x.s.cfg.Egress.client(5 * time.Second)
	if _, err := x.s.CreateWebhook(x.ctx, x.admin, x.p.Key, WebhookInput{URL: ptr(hook.URL), Events: ptr("env.*")}); err != nil {
		t.Fatal(err)
	}
	if _, err := x.s.RecordDeploymentNamed(x.ctx, x.editor, x.p.Key, "prod", DeployInput{Commit: "abcdef1"}); err != nil {
		t.Fatal(err)
	}
	x.s.Wait()
	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(bodies, "\n")
	if !strings.Contains(joined, `"env.deployed"`) || !strings.Contains(joined, `"commit":"abcdef1"`) || !strings.Contains(joined, `"env.created"`) {
		t.Errorf("webhook bodies: %s", joined)
	}
}
