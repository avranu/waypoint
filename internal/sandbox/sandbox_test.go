package sandbox

import (
	"crypto/ed25519"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestNames(t *testing.T) {
	cases := []struct{ got, prefix string }{
		{RepoUser("Robin/waypoint", "1/42", 0), "wpr-robin-waypoint-"},
		{RepoUser("Ops/infrastructure", "1/7", 0), "wpr-ops-infrastructure-"},
		{RepoUser("some-very-long-organisation/an-even-longer-repository-name", "1/9", 0), "wpr-some-v-an-even-longer-"},
		{OrgUser("Robin", "1/robin", 0), "wpo-robin-"},
		{OrgUser("A_Really_Long_Organisation_Name_Here", "1/x", 0), "wpo-a-really-long-organis-"},
	}
	for _, c := range cases {
		if !strings.HasPrefix(c.got, c.prefix) {
			t.Errorf("%s: want prefix %s", c.got, c.prefix)
		}
		if len(c.got) > MaxUserName {
			t.Errorf("%s is %d long", c.got, len(c.got))
		}
		if h := c.got[len(c.got)-6:]; strings.Trim(h, "0123456789abcdef") != "" {
			t.Errorf("%s doesn't end in its hash", c.got)
		}
	}
	if RepoUser("foo-bar/baz", "1/1", 0) == RepoUser("foo/bar-baz", "1/2", 0) {
		t.Error("two repos share a name")
	}
	a, b := RepoUser("o/r", "1/1", 0), RepoUser("o/r", "1/1", 1)
	if a == b || a[:len(a)-6] != b[:len(b)-6] {
		t.Errorf("salted %s vs %s", a, b)
	}
	if got := RepoUser("__/..", "1/3", 0); !strings.HasPrefix(got, "wpr-") || strings.Contains(got, "--") {
		t.Errorf("symbol slug: %s", got)
	}
}

func TestSpec(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	now := time.Unix(1_800_000_000, 0)
	spec := func() *Spec {
		return &Spec{Op: OpEnsureUsers, Nonce: NewNonce(), Issued: now.Unix(), Expiry: now.Add(10 * time.Minute).Unix(), Runner: 3,
			Repos: []Repo{{ID: "1/42", Slug: "Robin/waypoint", Owner: "1/robin"}}}
	}
	sd, err := Sign(priv, spec())
	if err != nil {
		t.Fatal(err)
	}
	if sd.Key != Fingerprint(pub) {
		t.Errorf("key %s", sd.Key)
	}
	got, err := Verify(pub, sd, now.Add(time.Minute))
	if err != nil || len(got.Repos) != 1 || got.Repos[0].Slug != "Robin/waypoint" || got.V != SpecVersion {
		t.Fatalf("verify: %+v %v", got, err)
	}
	other, _, _ := ed25519.GenerateKey(nil)
	if _, err := Verify(other, sd, now); !errors.Is(err, ErrBadSignature) {
		t.Errorf("other key: %v", err)
	}
	bad := *sd
	bad.Payload = bad.Payload[:len(bad.Payload)-2] + "AA"
	if _, err := Verify(pub, &bad, now); !errors.Is(err, ErrBadSignature) {
		t.Errorf("tampered: %v", err)
	}
	if _, err := Verify(pub, sd, now.Add(11*time.Minute)); !errors.Is(err, ErrExpired) {
		t.Errorf("expired: %v", err)
	}
	if _, err := Verify(pub, sd, now.Add(-5*time.Minute)); err == nil {
		t.Error("a spec from the future passed")
	}
	long := spec()
	long.Expiry = now.Add(2 * time.Hour).Unix()
	lsd, _ := Sign(priv, long)
	if _, err := Verify(pub, lsd, now); err == nil {
		t.Error("an overlong spec passed")
	}
	if _, err := Verify(pub, nil, now); err == nil {
		t.Error("no spec passed")
	}
	k, err := ParseKey(FormatKey(pub) + "\n")
	if err != nil || !k.Equal(pub) {
		t.Errorf("key text: %v", err)
	}
	if _, err := ParseKey("ssh-ed25519 AAAA"); err == nil {
		t.Error("parsed an ssh key")
	}
}
