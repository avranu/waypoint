package core

import "testing"

func TestMentionsPath(t *testing.T) {
	for _, c := range []struct {
		text, name string
		want       bool
	}{
		{"on `pkg/b/config.go` line 3", "config.go", false},
		{"see config.go.", "config.go", true},
		{"in `config.go`", "config.go", true},
		{"on pkg/a/config.go", "pkg/a/config.go", true},
		{"myconfig.go", "config.go", false},
		{"config.go.bak", "config.go", false},
	} {
		if got := mentionsPath(c.text, c.name); got != c.want {
			t.Errorf("mentionsPath(%q, %q) = %v", c.text, c.name, got)
		}
	}
}

func TestDiffFilesRename(t *testing.T) {
	pf := diffFiles("diff --git a/old.go b/new.go\nsimilarity index 100%\nrename from old.go\nrename to new.go\n")
	if len(pf.Paths) != 2 || pf.Paths[0] != "old.go" || pf.Paths[1] != "new.go" {
		t.Errorf("paths = %v", pf.Paths)
	}
}
