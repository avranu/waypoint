package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Vouching for an environment's addresses (WP-1087, WP-ADR109 amending WP-ADR90).
// Unattended agent sessions may read from the hosts of the environments
// whose addresses a person set. Before this, the only way for a person to
// set them was the web form, which nobody was prompted to visit when an
// agent hit the guard (one task waited on WP:R-325 for days). VouchEnvironment
// is one step for whoever runs it: a person's call records the addresses as
// theirs; an agent's files one question for a person, and the person's
// "Allow reads" answer records them as that person's. What the person is
// shown in the question is exactly what gets recorded.

// EnvReadAllow is the choice of an environment read request that vouches.
const EnvReadAllow = "Allow reads"

// envReadDeny is the other choice.
const envReadDeny = "Don't allow"

// EnvReadAsk is what an environment read request asks a person to vouch
// for: the environment and the addresses it would have.
type EnvReadAsk struct {
	Environment string `json:"environment"`
	URL         string `json:"url"`
	HealthURL   string `json:"health_url,omitempty"`
	VersionURL  string `json:"version_url,omitempty"`
}

// hosts are the ask's distinct hosts, as the guard lists them.
func (a *EnvReadAsk) hosts() []string {
	var out []string
	for _, addr := range []string{a.URL, a.HealthURL, a.VersionURL} {
		if h := envAddressHost(addr); h != "" && !containsStr(out, h) {
			out = append(out, h)
		}
	}
	return out
}

// dedupe keys open asks for the same environment and addresses.
func (a *EnvReadAsk) dedupe() string {
	return "env-read:" + a.Environment + "\n" + a.URL + "\n" + a.HealthURL + "\n" + a.VersionURL
}

// question is what the person is asked.
func (a *EnvReadAsk) question() Question {
	var b strings.Builder
	fmt.Fprintf(&b, "Let unattended agents read from %s with GET requests only? Allowing records these addresses on the environment as yours:\n", a.Environment)
	fmt.Fprintf(&b, "- address: %s\n", a.URL)
	if a.HealthURL != "" {
		fmt.Fprintf(&b, "- health: %s\n", a.HealthURL)
	}
	if a.VersionURL != "" {
		fmt.Fprintf(&b, "- version: %s\n", a.VersionURL)
	}
	hosts := strings.Join(a.hosts(), ", ")
	return Question{
		Question: strings.TrimSpace(b.String()),
		Header:   "Agent reads",
		Choices: []Choice{
			{Label: EnvReadAllow, Description: "Agents may read (GET, HEAD) from " + hosts + " and send the repo's own credentials there; changes still need a person (WP-ADR90)"},
			{Label: envReadDeny, Description: "Agents keep asking a person to run each read"},
		},
	}
}

// allows: answers pick "Allow reads" for the ask's one question.
func envReadAllows(answers []Answer) bool {
	if len(answers) != 1 {
		return false
	}
	return containsStr(answers[0].Choices, EnvReadAllow) // picked, not typed
}

// EnvVouchInput: the addresses to vouch for; nil keeps the environment's
// (an empty string clears one). Reason and Issue go on an agent's request.
type EnvVouchInput struct {
	URL        *string `json:"url"`
	HealthURL  *string `json:"health_url"`
	VersionURL *string `json:"version_url"`
	Reason     string  `json:"reason"`
	Issue      string  `json:"issue"`
}

// EnvVouchResult says what happened: the environment, vouched (a person's
// call, or already vouched with these addresses), or the request a person
// is asked (an agent's call).
type EnvVouchResult struct {
	Vouched     bool               `json:"vouched"`
	Environment *EnvironmentDetail `json:"environment,omitempty"`
	Request     *Request           `json:"request,omitempty"`
	New         bool               `json:"new,omitempty"` // the request was filed now, not an open one found
	Hosts       []string           `json:"hosts"`
}

// VouchEnvironment vouches for an environment's addresses so unattended
// agent sessions of the project's repos may read from them, creating the
// environment when it doesn't exist. A person acting directly does it at
// once (editors); anyone else (an agent, an agent's shell, an automation)
// files a question to a person instead, or finds the open one.
func (s *Service) VouchEnvironment(ctx context.Context, actor *Actor, projectRef, name string, in EnvVouchInput) (*EnvVouchResult, error) {
	if err := actor.need(PermEnvironmentsRead); err != nil {
		return nil, err
	}
	p, err := s.projectByRef(ctx, s.db.R, projectRef)
	if err != nil {
		return nil, err
	}
	if err := s.requireProjectRole(ctx, s.db.R, actor, p.ID, RoleViewer); err != nil {
		return nil, err
	}
	if name, err = cleanEnvName(name); err != nil {
		return nil, err
	}
	ask := EnvReadAsk{Environment: name}
	cur, err := s.envRowByName(ctx, s.db.R, p.ID, name)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	if cur != nil {
		ask.URL, ask.HealthURL, ask.VersionURL = cur.URL, cur.HealthURL, cur.VersionURL
	}
	for _, f := range []struct {
		in  *string
		out *string
	}{{in.URL, &ask.URL}, {in.HealthURL, &ask.HealthURL}, {in.VersionURL, &ask.VersionURL}} {
		if f.in != nil {
			*f.out = strings.TrimSpace(*f.in)
		}
	}
	if ask.URL == "" {
		return nil, invalid("give %s's address (--url https://…)", name)
	}
	for _, addr := range []string{ask.URL, ask.HealthURL, ask.VersionURL} {
		if addr != "" && envAddressHost(addr) == "" {
			return nil, invalid("%q isn't an http:// or https:// address", addr)
		}
	}
	if cur != nil && cur.Stopped {
		return nil, invalid("%s is stopped, so agents don't read from it; start it again first (wp env up %s)", name, name)
	}
	if ask.VersionURL != "" && (cur == nil || cur.CommitField == "" && cur.VersionField == "") {
		// The save "Allow reads" makes would fail on it (validEnvSettings),
		// leaving the person a question that can't be allowed.
		return nil, invalid("a version address needs the field to read from it, which a person sets on the environment in the web; leave --version out")
	}
	res := &EnvVouchResult{Hosts: ask.hosts()}
	if cur != nil && cur.AddressAgent == "" && cur.URL == ask.URL && cur.HealthURL == ask.HealthURL && cur.VersionURL == ask.VersionURL {
		// A person already set exactly these: nothing to ask or change.
		res.Vouched = true
		res.Environment, err = s.envDetail(ctx, cur.ID)
		return res, err
	}
	if addressAgent(actor) != "" { // an agent, an agent's shell or an automation
		if err := actor.need(PermRequestsCreate); err != nil {
			return nil, err
		}
		req, created, err := s.CreateRequest(ctx, actor, RequestInput{
			Kind: RequestQuestion, Title: fmt.Sprintf("Let agents read from %s's %s (%s)", p.Key, name, strings.Join(ask.hosts(), ", ")),
			Body: strings.TrimSpace(in.Reason), Project: p.Key, Issue: in.Issue, envRead: &ask,
		})
		if err != nil {
			return nil, err
		}
		if !created && !s.mayDecideOrOwn(ctx, actor, req) {
			// Someone else's open ask: the agent learns it is asked, not
			// that person's reason.
			req = &Request{ID: req.ID, Code: req.Code, Ref: req.Ref, Number: req.Number, Project: req.Project,
				Kind: req.Kind, Title: req.Title, State: req.State, EnvRead: req.EnvRead}
		}
		res.Request, res.New = req, created
		return res, nil
	}
	url, health, version := ask.URL, ask.HealthURL, ask.VersionURL
	set := EnvironmentInput{URL: &url, HealthURL: &health, VersionURL: &version}
	if cur == nil {
		set.Name = &name
		res.Environment, err = s.CreateEnvironment(ctx, actor, p.Key, set)
	} else {
		res.Environment, err = s.UpdateEnvironment(ctx, actor, cur.ID, set)
	}
	if err != nil {
		return nil, err
	}
	res.Vouched = res.Environment.AddressAgent == ""
	return res, nil
}

// mayDecideOrOwn: the request is the actor's own, or in a project they edit
// (requestVisible's rule), whether or not the actor is an agent.
func (s *Service) mayDecideOrOwn(ctx context.Context, actor *Actor, r *Request) bool {
	if r.userID == actor.id() || actor.isAdmin() {
		return true
	}
	if !r.projectID.Valid {
		return false
	}
	role, _ := s.projectRole(ctx, s.db.R, actor, r.projectID.Int64)
	return role >= RoleEditor
}

// vouchAnswered vouches for what an environment read request asked, as the
// person answering it; nothing when the request isn't one, or the answer
// isn't "Allow reads".
func (s *Service) vouchAnswered(ctx context.Context, actor *Actor, r *Request, answers []Answer) error {
	if r.EnvRead == nil || !envReadAllows(answers) {
		return nil
	}
	if r.Project == nil {
		return invalid("%s isn't in a project, so there is no environment to vouch for", r.Code)
	}
	a := r.EnvRead
	url, health, version := a.URL, a.HealthURL, a.VersionURL
	_, err := s.VouchEnvironment(ctx, actor, r.Project.Key, a.Environment, EnvVouchInput{URL: &url, HealthURL: &health, VersionURL: &version})
	return err
}
