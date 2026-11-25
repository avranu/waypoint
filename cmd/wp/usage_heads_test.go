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
		"curl -H 'Authorization: Bearer abc123def456' https://x":       "curl -H 'Authorization: ***' https://x",
		"export API_KEY=supersecretvalue":                              "export API_KEY=***",
		"docker run -p 8080:80 nginx":                                  "docker run -p 8080:80 nginx",
		"git push https://x:ghp_abcdefghijklmnopqrstuvwxyz0123@h":      "git push https://x:***@h",
		"echo password=hunter22 ok":                                    "echo password=*** ok",
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
