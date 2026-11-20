package cilint

import (
	"strings"
	"testing"
)

func fakeModes(m map[string]string) ModeFunc { return func(p string) string { return m[p] } }

const wfExec = `name: ci
on: [push]
jobs:
  lint:
    runs-on: ubuntu-latest
    steps:
      - name: image
        run: |
          set -e
          want=$(.forgejo/ci-image/build.sh --tag)
          bash scripts/ok.sh
          ./tools/a.sh && echo done
      - run: sh scripts/other.sh
      - run: scripts/glob*.sh
`

func TestScriptModesWorkflow(t *testing.T) {
	modes := fakeModes(map[string]string{
		".forgejo/ci-image/build.sh": "100644",
		"scripts/ok.sh":              "100644",
		"tools/a.sh":                 "100755",
		"scripts/other.sh":           "100644",
	})
	got := ScriptModes(".forgejo/workflows/ci.yml", []byte(wfExec), modes)
	if len(got) != 1 {
		t.Fatalf("want 1 finding, got %v", got)
	}
	f := got[0]
	if f.Rule != RuleScriptNotExec || f.Line != 10 || !strings.Contains(f.Msg, ".forgejo/ci-image/build.sh") {
		t.Errorf("bad finding: %v", f)
	}
	if !strings.Contains(f.Fix, "git update-index --chmod=+x .forgejo/ci-image/build.sh") || !strings.Contains(f.Fix, "wp request run") {
		t.Errorf("fix: %s", f.Fix)
	}
}

func TestScriptModesMakefile(t *testing.T) {
	mk := "all:\n\t@./scripts/x.sh\n\t-bash scripts/y.sh\n\t$(MAKE) other\n./notrecipe.sh\n"
	modes := fakeModes(map[string]string{"scripts/x.sh": "100644", "scripts/y.sh": "100644", "scripts/z.sh": "100644", "notrecipe.sh": "100644"})
	got := ScriptModes("Makefile", []byte(mk), modes)
	if len(got) != 1 || got[0].Line != 2 {
		t.Fatalf("want only scripts/x.sh at line 2, got %v", got)
	}
	if strings.Contains(got[0].Fix, "wp request") {
		t.Errorf("non-.forgejo fix should not mention requests: %s", got[0].Fix)
	}
	got = ScriptModes("deploy.mk", []byte("t:\n\tx=`scripts/z.sh`\n\ty=$$(scripts/z.sh | cat)\n"), modes)
	if len(got) != 1 {
		t.Errorf("want scripts/z.sh once: %v", got)
	}
}

func TestScriptModesSubstitution(t *testing.T) {
	modes := fakeModes(map[string]string{"a/b.sh": "100644"})
	wf := "on: push\njobs:\n  j:\n    steps:\n      - run: echo \"$(a/b.sh)\"\n"
	if got := ScriptModes(".github/workflows/x.yml", []byte(wf), modes); len(got) != 1 {
		t.Errorf("want 1, got %v", got)
	}
	if got := ScriptModes(".github/workflows/x.yml", []byte(wf), fakeModes(nil)); len(got) != 0 {
		t.Errorf("untracked path: %v", got)
	}
}

func TestParseLsFiles(t *testing.T) {
	m := ParseLsFiles("100755 abc 0\ta/b.sh\n100644 def 0\tc d.txt\n")
	if m("a/b.sh") != "100755" || m("c d.txt") != "100644" || m("x") != "" {
		t.Error("bad parse")
	}
}

func TestScriptModesCdAndQuoted(t *testing.T) {
	mode := ParseLsFiles("100644 a 0\tsub/x.sh\n100755 b 0\tx.sh\n100644 c 0\t\"scripts/caf\\303\\251.sh\"\n")
	wf := []byte("jobs:\n  a:\n    steps:\n      - run: |\n          ./scripts/café.sh\n          cd sub && ./x.sh\n")
	got := ScriptModes(".forgejo/workflows/ci.yml", wf, mode)
	if len(got) != 2 || !strings.Contains(got[1].Msg, "sub/x.sh") || !strings.Contains(got[0].Msg, "café.sh") {
		t.Fatalf("got %+v", got)
	}
}

func TestScriptModesWorkingDirectory(t *testing.T) {
	mode := fakeModes(map[string]string{"sub/x.sh": "100644", "x.sh": "100755", "web/y.sh": "100644", "y.sh": "100755", "z.sh": "100644", "deep/z.sh": "100644"})
	wf := []byte(`defaults:
  run:
    working-directory: web
jobs:
  a:
    steps:
      - run: ./y.sh
      - working-directory: ./sub/
        run: ./x.sh
  b:
    defaults:
      run:
        working-directory: .
    steps:
      - run: ./x.sh
      - run: ./y.sh
      - working-directory: ${{ inputs.dir }}
        run: ./z.sh
      - run: cd "$HOME" && ./z.sh
      - run: cd .. && ./z.sh
      - run: cd deep && cd .. && ./x.sh
`)
	got := ScriptModes(".forgejo/workflows/ci.yml", wf, mode)
	var msgs []string
	for _, f := range got {
		msgs = append(msgs, f.Msg)
	}
	if len(got) != 2 || !strings.Contains(got[0].Msg, "web/y.sh") || got[0].Line != 7 || !strings.Contains(got[1].Msg, "sub/x.sh") || got[1].Line != 9 {
		t.Fatalf("want web/y.sh:7 and sub/x.sh:9, got %q", msgs)
	}
}
