// Package sandbox is what the Waypoint server and the root sandbox helper,
// waypoint-sandboxd, share (WP-418, WP-ADR28, PL7 §5.3): signed job specs,
// machine user names, and the helper's wire protocol. The helper takes no
// free-form arguments: every call that changes the machine carries a spec
// the server signed, so a runner (or anything running as its user) can ask
// only for what Waypoint decided.
package sandbox

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// SpecVersion is the spec format this code reads and writes. Version 2
// names every repository a job may work on (WP-448).
const SpecVersion = 2

// Operations a spec can ask for (PL7 §14).
const (
	// OpEnsureUsers makes the machine users of a job's repositories.
	OpEnsureUsers = "ensure_users"
	// OpRemoveUser takes one machine user away: the account goes, its uid
	// is retired and its store directories are archived (WP-448).
	OpRemoveUser = "remove_user"
	// OpReconcile finishes what a crash or a failure left half done.
	OpReconcile = "reconcile"
	// OpStart runs a session Contained (WP-470, PL7 §6.3): its own
	// workspace in the store, a broker for its egress, and the session's
	// command in a transient unit as the job's repo user. The call lasts
	// as long as the session.
	OpStart = "start"
)

// LevelContained is the only level OpStart runs: an allowlisted systemd
// unit as the repo user (the VM level is Phase 3).
const LevelContained = "contained"

// MaxSpecRepos bounds the repositories one spec names.
const MaxSpecRepos = 50

// MaxSpecLife bounds how long a spec stays valid: long enough for a claim
// to reach the helper, short enough that a copy is soon worthless.
const MaxSpecLife = time.Hour

// Spec is what the server signs. Paths never appear in it: the helper
// takes those from its own ledger.
type Spec struct {
	V      int    `json:"v"`
	Op     string `json:"op"`
	Nonce  string `json:"nonce"`
	Issued int64  `json:"iat"` // unix seconds
	Expiry int64  `json:"exp"` // unix seconds
	Runner int64  `json:"runner"`
	Job    int64  `json:"job,omitempty"`
	// Repos are the repositories a job may work on (ensure_users), the
	// job's own first.
	Repos []Repo `json:"repos,omitempty"`
	// User is the machine user remove_user takes away.
	User  *Target `json:"user,omitempty"`
	Level string  `json:"level,omitempty"`
	// Session is what start runs, as the user of Repos[0].
	Session *Session `json:"session,omitempty"`
}

// Target names a machine user by its forge identity and its name, which
// must both match the helper's ledger.
type Target struct {
	Name string `json:"name"`
	Kind string `json:"kind"` // repo, org
	Key  string `json:"key"`
}

// Repo is the forge repository a spec is for. ID and Owner are what the
// machine users are keyed by, so a renamed repo keeps its users (WP-166);
// Slug is only for names and display.
type Repo struct {
	// ID is "<waypoint forge id>/<the forge's repo id>".
	ID string `json:"id"`
	// Slug is owner/name when the spec was signed.
	Slug string `json:"slug"`
	// Owner is "<waypoint forge id>/<owner login, lower case>".
	Owner string `json:"owner"`
}

// Signed is a spec with the server's signature: Payload is the spec's
// JSON, base64url, exactly as signed.
type Signed struct {
	Payload string `json:"payload"`
	Sig     string `json:"sig"`
	// Key is the signing key's fingerprint, to say which key a refusal
	// expected; it isn't trusted.
	Key string `json:"key"`
}

// NewNonce is a random spec nonce.
func NewNonce() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// Sign signs a spec.
func Sign(priv ed25519.PrivateKey, s *Spec) (*Signed, error) {
	if s.V == 0 {
		s.V = SpecVersion
	}
	b, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	pub, _ := priv.Public().(ed25519.PublicKey)
	return &Signed{
		Payload: base64.RawURLEncoding.EncodeToString(b),
		Sig:     base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, b)),
		Key:     Fingerprint(pub),
	}, nil
}

// Errors Verify returns.
var (
	ErrBadSignature = errors.New("the spec isn't signed by this machine's Waypoint key")
	ErrExpired      = errors.New("the spec has expired")
)

// Verify checks a signed spec against the pinned key and the clock, and
// returns the spec. It doesn't check the nonce: the helper's ledger does.
func Verify(pub ed25519.PublicKey, sd *Signed, now time.Time) (*Spec, error) {
	if sd == nil {
		return nil, errors.New("no spec")
	}
	b, err := base64.RawURLEncoding.DecodeString(sd.Payload)
	if err != nil {
		return nil, ErrBadSignature
	}
	sig, err := base64.RawURLEncoding.DecodeString(sd.Sig)
	if err != nil || len(pub) != ed25519.PublicKeySize || !ed25519.Verify(pub, b, sig) {
		return nil, ErrBadSignature
	}
	var s Spec
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("the spec doesn't parse: %w", err)
	}
	if s.V != SpecVersion {
		return nil, fmt.Errorf("spec version %d; this helper reads %d (update it)", s.V, SpecVersion)
	}
	n := now.Unix()
	switch {
	case s.Nonce == "":
		return nil, errors.New("the spec has no nonce")
	case s.Expiry <= s.Issued || time.Duration(s.Expiry-s.Issued)*time.Second > MaxSpecLife:
		return nil, errors.New("the spec's lifetime is wrong")
	case s.Issued > n+60:
		return nil, errors.New("the spec is from the future: check this machine's clock")
	case n >= s.Expiry:
		return nil, ErrExpired
	}
	return &s, nil
}

// Key text is "ed25519:<base64 of the public key>", one line.
const keyPrefix = "ed25519:"

// FormatKey writes a public key as text.
func FormatKey(pub ed25519.PublicKey) string {
	return keyPrefix + base64.StdEncoding.EncodeToString(pub)
}

// ParseKey reads a public key written by FormatKey.
func ParseKey(text string) (ed25519.PublicKey, error) {
	t := strings.TrimSpace(text)
	if !strings.HasPrefix(t, keyPrefix) {
		return nil, errors.New("not a Waypoint sandbox key (want ed25519:…)")
	}
	b, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(t, keyPrefix))
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, errors.New("the sandbox key is damaged")
	}
	return ed25519.PublicKey(b), nil
}

// Fingerprint is a short, comparable name for a key: SHA256:<base64>, as
// ssh shows them.
func Fingerprint(pub ed25519.PublicKey) string {
	h := sha256.Sum256(pub)
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(h[:])
}
