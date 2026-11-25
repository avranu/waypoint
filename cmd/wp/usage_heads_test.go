package main

import (
	"slices"
	"strings"
	"testing"
)

func TestCommandHeads(t *testing.T) {
	for _, tc := range []struct {
		cmd  string
		want []string
	}{
		{"git commit -m 'fix: x && y'", []string{"git commit"}},
		{"cd /tmp && git status | head -5", []string{"cd", "git status", "head"}},
		{"FOO=1 BAR='a b' go test ./... -run X", []string{"go test"}},
		{"sudo -u root systemctl restart nginx", []string{"systemctl restart"}},
		{"time nohup make check > out.log 2>&1 &", []string{"make check"}},
		{"git -C /repo log --oneline", []string{"git log"}},
		{"kubectl -n prod get pods -o wide", []string{"kubectl get"}},
		{"docker compose up -d", []string{"docker compose up"}},
		{"npm run build && npm test", []string{"npm run", "npm test"}},
		{"cat file.txt | grep foo | wc -l", []string{"cat", "grep", "wc"}},
		{"ls; ls -la; ls", []string{"ls"}},
		{"/usr/bin/curl -s https://example.com", []string{"curl"}},
		{"./bin/host_pytest.sh --reuse-db x", []string{"host_pytest.sh"}},
		{"bash -c 'cd x && cargo build --release'", []string{"cd", "cargo build"}},
		{"timeout 30 wp ls -A", []string{"wp ls"}},
		{"env FOO=bar python3 -m pytest -x", []string{"python -m pytest"}},
		{"python3 script.py", []string{"python"}},
		{"echo \"$(git rev-parse HEAD)\" > sha.txt", []string{"echo"}},
		{"for f in *.go; do gofmt -l $f; done", []string{"gofmt"}},
		{"if [ -f x ]; then echo yes; fi", []string{"echo"}},
		{"cat <<'EOF' > x.sh\nrm -rf /\nEOF\nchmod +x x.sh", []string{"cat", "chmod"}},
		{"git log 2>&1 | head", []string{"git log", "head"}},
		{"make -C sub -j4 test", []string{"make test"}},
		{"gh pr create --title x", []string{"gh pr"}},
		{"ssh host 'rm -rf /tmp/x'", []string{"ssh"}},
		{"(cd a && ls)", []string{"cd", "ls"}},
		{"$HOME/bin/tool run", []string{"tool"}},
		{"$CMD arg", []string{"(variable)"}},
		{"# just a comment", nil},
		{"", nil},
		{"FOO=1", nil},
		{"git --no-pager diff", []string{"git diff"}},
		{"git ./x", []string{"git"}},
	} {
		if got := CommandHeads(tc.cmd); !slices.Equal(got, tc.want) {
			t.Errorf("%q → %q, want %q", tc.cmd, got, tc.want)
		}
	}
}

func TestRedact(t *testing.T) {
	for in, want := range map[string]string{
		"curl -H 'Authorization: Bearer abc123def456' https://x":  "curl -H 'Authorization: ***' https://x",
		"export API_KEY=supersecretvalue":                         "export API_KEY=***",
		"docker run -p 8080:80 nginx":                             "docker run -p 8080:80 nginx",
		"git push https://x:ghp_abcdefghijklmnopqrstuvwxyz0123@h": "git push https://x:***@h",
		"echo password=hunter22 ok":                               "echo password=*** ok",
		// WP-860: whole risky commands carry these shapes too.
		"psql postgres://admin:S3cretPw@db.prod/app -c 'drop table x'": "psql postgres://admin:***@db.prod/app -c 'drop table x'",
		"mysql -u root --password hunter2hunter2 -e 'drop'":            "mysql -u root --password *** -e 'drop'",
		"AWS_SECRET_ACCESS_KEY=wJalrXUtnFEMIK7MDENG terraform destroy": "AWS_SECRET_ACCESS_KEY=*** terraform destroy",
		"kubectl delete ns prod --token abcdef123456":                  "kubectl delete ns prod --token ***",
		"git push --force-with-lease origin main":                      "git push --force-with-lease origin main",
	} {
		if got := redact(in); got != want {
			t.Errorf("redact(%q) = %q, want %q", in, got, want)
		}
	}
	if strings.Contains(redact("TOKEN: abcdefgh"), "abcdefgh") {
		t.Error("token left")
	}
}

func TestRedactMoreTokenShapes(t *testing.T) {
	a36 := strings.Repeat("aB3", 12)
	for in, want := range map[string]string{
		"key sk_live_" + strings.Repeat("a1", 12) + " end": "key *** end",
		"sk_test_" + strings.Repeat("a1", 8):               "***",
		"rk_live_" + strings.Repeat("a1", 8):               "***",
		"rk_test_" + strings.Repeat("a1", 8):               "***",
		"k AIza" + strings.Repeat("a_-", 12) + " e":        "k *** e",
		"npm_" + a36:                                     "***",
		"pypi-" + strings.Repeat("aB_-", 6):              "***",
		"xapp-1-A0123-4567-abcdef":                       "***",
		"https://" + strings.Repeat("u", 24) + "@host/x": "https://***@host/x",
		"https://tok123abc@git.example/x":                "https://***@git.example/x",
		"https://jenkinsci@git.example/x":                "https://jenkinsci@git.example/x",
		"ssh://git@github.com/x":                         "ssh://git@github.com/x",
		"https://tok123abc:x-oauth-basic@github.com/x":   "https://***:***@github.com/x",
		"https://tok123abc:@host/x":                      "https://***:@host/x",
		"postgres://admin:pw@db/app":                     "postgres://admin:***@db/app",
		"https://jenkinsci:pw@h/x":                       "https://jenkinsci:***@h/x",
		"https://" + strings.Repeat("u", 20) + ":pw@h/x": "https://***:***@h/x",
		"https://1234-5678@h/x":                          "https://***@h/x",
		"https://123456789012@h/x":                       "https://123456789012@h/x",
		"https://deploy@git.example/x":                   "https://deploy@git.example/x",
		"https://" + strings.Repeat("u", 19) + "@h/x":    "https://" + strings.Repeat("u", 19) + "@h/x",
		"https://" + strings.Repeat("u", 20) + "@h/x":    "https://***@h/x",
		"https://build200@host/x":                        "https://***@host/x",
		"https://buil200@host/x":                         "https://buil200@host/x",
		"https://example.com?email=jess@example.com":     "https://example.com?email=jess@example.com",
		"npm_config_cache":                               "npm_config_cache",
		"sk_live_short":                                  "sk_live_short",
		"pypi-short":                                     "pypi-short",
	} {
		if got := redact(in); got != want {
			t.Errorf("redact(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRedactPGPBlock(t *testing.T) {
	body := strings.Repeat("aB3/", 16)
	in := "-----BEGIN PGP PRIVATE KEY BLOCK-----\nVersion: GnuPG v2\n\n" + body + "\n=AbCd\n-----END PGP PRIVATE KEY BLOCK-----\nafter"
	got := redact(in)
	if strings.Contains(got, body) || strings.Contains(got, "=AbCd") || !strings.HasSuffix(got, "-----END PGP PRIVATE KEY BLOCK-----\nafter") {
		t.Errorf("redact: %q", got)
	}
}
