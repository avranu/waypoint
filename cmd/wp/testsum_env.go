package main

import (
	"fmt"
	"slices"
	"strings"
)

// testSecretSuffixes and testSecretParts are the name shapes of credentials.
var (
	testSecretSuffixes = []string{"_TOKEN", "_KEY", "_APIKEY", "_SECRET", "_CREDENTIALS", "_PAT", "_PASS", "_PWD"}
	testSecretParts    = []string{"SECRET", "PASSWORD", "PASSWD", "PASSPHRASE", "API_KEY", "ACCESS_KEY", "PRIVATE_KEY", "AUTH_TOKEN"}
	testSecretNames    = map[string]bool{"TOKEN": true, "DOCKER_AUTH_CONFIG": true}
)

// testSecretVar says whether the variable named k looks like a credential.
func testSecretVar(k string) bool {
	u := strings.ToUpper(k)
	if testSecretNames[u] {
		return true
	}
	for _, s := range testSecretSuffixes {
		if strings.HasSuffix(u, s) {
			return true
		}
	}
	for _, p := range testSecretParts {
		if strings.Contains(u, p) {
			return true
		}
	}
	return false
}

// testEnv is env without its credentials, except the names in keep; dropped
// names what it left out, sorted.
func testEnv(env []string, keep []string) (out, dropped []string) {
	out = make([]string, 0, len(env))
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if testSecretVar(k) && !slices.ContainsFunc(keep, func(n string) bool { return strings.EqualFold(n, k) }) {
			dropped = append(dropped, k)
			continue
		}
		out = append(out, kv)
	}
	slices.Sort(dropped)
	return out, slices.Compact(dropped)
}

// keepEnvFlag is --keep-env: repeatable, and NAME,NAME works too.
type keepEnvFlag []string

func (k *keepEnvFlag) String() string { return strings.Join(*k, ",") }
func (k *keepEnvFlag) Set(v string) error {
	for _, n := range strings.Split(v, ",") {
		n = strings.TrimSpace(n)
		if n == "" || strings.ContainsAny(n, "= \t") {
			return fmt.Errorf("give a variable's name, not %q", v)
		}
		*k = append(*k, n)
	}
	return nil
}

// withheldNote is the line a failing run adds about what it ran without,
// "" when nothing was left out.
func withheldNote(dropped []string) string {
	if len(dropped) == 0 {
		return ""
	}
	return fmt.Sprintf("It ran without %s (secret-looking variables are kept out of tests, WP-1065); if a test needs one, mock what it reaches, or pass it with --keep-env NAME.", strings.Join(dropped, ", "))
}
