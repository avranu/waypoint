package core

import "testing"

func TestDiffHeaderEdgeCases(t *testing.T) {
	d := "diff --git a/old.go b/caf\\303\\251.go\nsimilarity index 100%\nrename from old.go\nrename to \"caf\\303\\251.go\"\n" +
		"diff --git a/user guide.md b/user guide.md\n--- a/user guide.md\t\n+++ b/user guide.md\t\n@@ -1 +1 @@\n-a\n+b\n"
	pf := diffFiles(d)
	want := map[string]bool{"old.go": true, `"caf\303\251.go"`: true, "user guide.md": true}
	if len(pf.Paths) != 3 {
		t.Fatalf("paths %q", pf.Paths)
	}
	for _, p := range pf.Paths {
		if !want[p] {
			t.Errorf("unexpected %q", p)
		}
	}
}

func TestMentionsRelativePath(t *testing.T) {
	if !mentionsPath("Fix ./pkg/a.go now", "pkg/a.go") {
		t.Error("./ prefix not recognised")
	}
	if mentionsPath("Fix x/pkg/a.go", "pkg/a.go") || mentionsPath("see ./pkg/a.go", "a.go") || mentionsPath("../pkg/a.go", "pkg/a.go") {
		t.Error("matched inside another path")
	}
}
