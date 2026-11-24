package main

import (
	"fmt"
	"sort"
	"strings"
	"unicode"
)

// jobEnvVars are the variables a job keeps from the runner's environment.
var jobEnvVars = map[string]bool{}

// jobEnvPrefixes are families of variables a job keeps (LC_ALL, LC_CTYPE…).
var jobEnvPrefixes = []string{"LC_"}

func init() {
	for _, k := range strings.Fields(`
		PATH HOME USER LOGNAME SHELL LANG LANGUAGE TZ TERM COLORTERM NO_COLOR
		TMPDIR TMP TEMP
		XDG_CONFIG_HOME XDG_CACHE_HOME XDG_DATA_HOME XDG_STATE_HOME XDG_DATA_DIRS XDG_CONFIG_DIRS
		HTTP_PROXY HTTPS_PROXY NO_PROXY ALL_PROXY
		SSL_CERT_FILE SSL_CERT_DIR NODE_EXTRA_CA_CERTS REQUESTS_CA_BUNDLE CURL_CA_BUNDLE
		GOPATH GOROOT GOCACHE GOMODCACHE GOFLAGS GOTOOLCHAIN GOPROXY GOPRIVATE GONOPROXY GONOSUMDB GOSUMDB GOENV GOBIN CGO_ENABLED
		NVM_DIR NVM_BIN NODE_PATH NPM_CONFIG_PREFIX COREPACK_HOME COREPACK_ENABLE_AUTO_PIN BUN_INSTALL PNPM_HOME VOLTA_HOME
		CARGO_HOME RUSTUP_HOME PYENV_ROOT UV_CACHE_DIR PIP_CACHE_DIR JAVA_HOME
		DO_NOT_TRACK CLAUDE_CONFIG_DIR CODEX_HOME
		SYSTEMROOT COMSPEC PATHEXT WINDIR USERPROFILE APPDATA LOCALAPPDATA PROGRAMDATA HOMEDRIVE HOMEPATH OS
		PROCESSOR_ARCHITECTURE NUMBER_OF_PROCESSORS`) {
		jobEnvVars[k] = true
	}
}

// runnerEnvPass is what the runner's --env flags add to jobEnvVars.
var runnerEnvPass = map[string]bool{}

// apiKeyVars are what would bill an API account instead of the person's
// subscription (or let a job use keys it wasn't given). A job that keeps
// keys (opencode, a command job that asks) also keeps any *_API_KEY.
var apiKeyVars = []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "OPENAI_API_KEY", "CODEX_API_KEY", "CURSOR_API_KEY"}

// jobEnvKeeps says whether a job keeps the runner's variable k.
func jobEnvKeeps(k string, keepKeys bool) bool {
	u := strings.ToUpper(k)
	if strings.HasPrefix(u, "WAYPOINT_") {
		return false
	}
	if jobEnvVars[u] || runnerEnvPass[u] {
		return true
	}
	for _, p := range jobEnvPrefixes {
		if strings.HasPrefix(u, p) {
			return true
		}
	}
	if keepKeys {
		for _, a := range apiKeyVars {
			if u == a {
				return true
			}
		}
		return strings.HasSuffix(u, "_API_KEY")
	}
	return false
}

// jobEnv is the environment a job's process gets: what the allowlist keeps
// of the runner's, plus the job's variables, and WAYPOINT_UNATTENDED=1: no
// person drives what a runner runs, so the guard (WP-270) is on in it and in
// whatever it launches.
func jobEnv(base []string, extra map[string]string, keepKeys bool) []string {
	var out []string
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		if jobEnvKeeps(k, keepKeys) {
			out = append(out, kv)
		}
	}
	keys := make([]string, 0, len(extra))
	for k := range extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if strings.ToUpper(k) != "WAYPOINT_UNATTENDED" {
			out = append(out, k+"="+extra[k])
		}
	}
	return append(out, "WAYPOINT_UNATTENDED=1")
}

// runnerGitEnv is the environment of the git the runner runs itself
// (clones, fetches, pushes of a job's branch): the runner's, so its
// credentials work, without Waypoint's variables. No job's code runs in it.
func runnerGitEnv(base []string) []string {
	var out []string
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		if !strings.HasPrefix(strings.ToUpper(k), "WAYPOINT_") {
			out = append(out, kv)
		}
	}
	return out
}

// passJobEnv adds the runner's --env names to what its jobs keep. Waypoint's
// own variables can't be passed: a job never gets the runner's token.
func passJobEnv(names []string) error {
	if err := checkJobEnv(names); err != nil {
		return err
	}
	for _, n := range names {
		if u := strings.ToUpper(strings.TrimSpace(n)); !strings.EqualFold(u, "none") {
			runnerEnvPass[u] = true
		}
	}
	return nil
}

// checkJobEnv validates --env names without changing what jobs keep.
func checkJobEnv(names []string) error {
	for _, n := range names {
		u := strings.ToUpper(strings.TrimSpace(n))
		switch {
		case strings.EqualFold(u, "none"):
		case u == "" || strings.ContainsAny(u, "= ") || strings.IndexFunc(u, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) }) >= 0:
			return fmt.Errorf("--env %q: give a variable's name", n)
		case strings.HasPrefix(u, "WAYPOINT_"):
			return fmt.Errorf("--env %s: Waypoint's own variables never reach a job (it gets a token of its own)", n)
		}
	}
	return nil
}
