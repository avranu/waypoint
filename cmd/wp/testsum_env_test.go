package main

import (
	"bytes"
	"slices"
	"strings"
	"testing"
)

func TestTestSecretVar(t *testing.T) {
	for _, k := range []string{
		"FORGEJO_TOKEN", "GITHUB_TOKEN", "WAYPOINT_TOKEN", "OPEN_LLMS_API_KEY", "ANTHROPIC_API_KEY",
		"ANTHROPIC_AUTH_TOKEN", "AWS_SECRET_ACCESS_KEY", "AWS_ACCESS_KEY_ID", "AWS_SESSION_TOKEN",
		"DJANGO_SECRET_KEY", "PGPASSWORD", "APP_PASSWORD", "GOOGLE_APPLICATION_CREDENTIALS",
		"GH_PAT", "TOKEN", "MYSQL_PWD", "APP_CONTROL_KEY", "client_secret", "SMTP_PASS", "DB_PASS",
		"GPG_PASSPHRASE", "REDIS_PWD",
	} {
		if !testSecretVar(k) {
			t.Errorf("%s: kept, want dropped", k)
		}
	}
	for _, k := range []string{
		"PATH", "HOME", "GOFLAGS", "TOKENIZERS_PARALLELISM", "WAYPOINT_SLOTS_HELD", "WAYPOINT_URL",
		"SSH_AUTH_SOCK", "SITE_NAMES", "KEYTIMEOUT", "DATABASE_URL", "APP_URL", "PYTHONPATH",
		"PWD", "OLDPWD", "BYPASS_CACHE",
	} {
		if testSecretVar(k) {
			t.Errorf("%s: dropped, want kept", k)
		}
	}
}

func TestTestEnvKeepsWhatItIsTold(t *testing.T) {
	env := []string{"PATH=/bin", "FORGEJO_TOKEN=t", "OPENAI_API_KEY=k", "HOME=/h", "OPENAI_API_KEY=k2"}
	out, dropped := testEnv(env, []string{"openai_api_key"})
	if want := []string{"PATH=/bin", "OPENAI_API_KEY=k", "HOME=/h", "OPENAI_API_KEY=k2"}; !slices.Equal(out, want) {
		t.Errorf("env = %q, want %q", out, want)
	}
	if !slices.Equal(dropped, []string{"FORGEJO_TOKEN"}) {
		t.Errorf("dropped = %q", dropped)
	}
	_, dropped = testEnv(env, nil)
	if !slices.Equal(dropped, []string{"FORGEJO_TOKEN", "OPENAI_API_KEY"}) {
		t.Errorf("dropped once each, sorted: %q", dropped)
	}
	if n := withheldNote([]string{"A_TOKEN", "B_TOKEN"}); !strings.HasPrefix(n, "It ran without A_TOKEN, B_TOKEN (") || !strings.Contains(n, "--keep-env NAME") {
		t.Errorf("note = %q", n)
	}
	if withheldNote(nil) != "" {
		t.Error("nothing dropped: no note")
	}
}

// The command runs without the caller's credentials; a failing run says so,
// a passing one says nothing, and --keep-env lets one through.
func TestTestsumKeepsSecretsOutOfTheCommand(t *testing.T) {
	slotEnv(t)
	t.Chdir(t.TempDir())
	t.Setenv("WP1065_TEST_TOKEN", "s3cret")
	t.Setenv("WP1065_PLAIN", "plain")
	pass := `printf '{"Action":"pass","Package":"p","Test":"T","Elapsed":0}\n{"Action":"pass","Package":"p","Elapsed":1}\n'`
	check := `[ -z "${WP1065_TEST_TOKEN:-}" ] && [ "$WP1065_PLAIN" = plain ] || exit 9; ` + pass
	var out, errb bytes.Buffer
	if code := run([]string{"testsum", "--", "sh", "-c", check}, &out, &errb); code != 0 || strings.Contains(out.String(), "ran without") {
		t.Errorf("secret reached the command, or a pass printed the note: %d %q %q", code, out.String(), errb.String())
	}
	out.Reset()
	if code := run([]string{"testsum", "--", "sh", "-c", "exit 4"}, &out, &errb); code != 1 || !strings.Contains(out.String(), "It ran without ") || !strings.Contains(out.String(), "WP1065_TEST_TOKEN") {
		t.Errorf("failing run should name what it ran without: %d %q", code, out.String())
	}
	out.Reset()
	keep := `[ "${WP1065_TEST_TOKEN:-}" = s3cret ] || exit 9; ` + pass
	if code := run([]string{"testsum", "--keep-env", "WP1065_TEST_TOKEN", "--", "sh", "-c", keep}, &out, &errb); code != 0 {
		t.Errorf("--keep-env: %d %q %q", code, out.String(), errb.String())
	}
	out.Reset()
	if code := run([]string{"testsum", "--repeat", "2", "--", "sh", "-c", check}, &out, &errb); code != 0 {
		t.Errorf("--repeat leaked the secret: %d %q %q", code, out.String(), errb.String())
	}

	out.Reset()
	errb.Reset()
	if code := run([]string{"slot", "run", "tests", "--", "sh", "-c", check}, &out, &errb); code != 0 {
		t.Errorf("slot run tests leaked the secret: %d %q %q", code, out.String(), errb.String())
	}
	if code := run([]string{"slot", "run", "tests", "--", "sh", "-c", "exit 4"}, &out, &errb); code != 4 || !strings.Contains(errb.String(), "WP1065_TEST_TOKEN") {
		t.Errorf("failing slot run should name what it ran without: %d %q", code, errb.String())
	}
	if code := run([]string{"slot", "run", "tests", "--keep-env", "WP1065_TEST_TOKEN", "--", "sh", "-c", keep}, &out, &errb); code != 0 {
		t.Errorf("slot run --keep-env: %d %q %q", code, out.String(), errb.String())
	}
	if code := run([]string{"slot", "run", "build", "--", "sh", "-c", `[ "${WP1065_TEST_TOKEN:-}" = s3cret ]`}, &out, &errb); code != 0 {
		t.Errorf("another slot lost the environment: %d %q", code, errb.String())
	}
}
