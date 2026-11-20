package core

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// WP-1037: text Waypoint hands an agent never orders it to take a task
// with wp next. Whoever sends the text can run wp next itself and hand over
// the answer (the task, how to finish it, how to ask or give it back), as
// autopilot's kickoff does; an order to fetch it costs the agent a turn,
// and "and do it" leaves it no way to say the task shouldn't be done.
var takeOrderRE = regexp.MustCompile("(?i)(\\b(take|claim)\\s+(it|the task|%s|\\{key\\}|[A-Z]+-[0-9]+)?\\s*with\\s+`wp next|`wp next[^`]*`\\s+and do it)")

func TestNoPromptOrdersAgentsToTakeTasks(t *testing.T) {
	for _, root := range []string{"../../cmd", "../../internal"} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			ast.Inspect(f, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				s, err := strconv.Unquote(lit.Value)
				if err == nil && takeOrderRE.MatchString(s) {
					t.Errorf("%s: %q orders an agent to take a task with wp next; run it for the agent and hand over the answer (WP-1037)", fset.Position(lit.Pos()), s)
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

// The rule catches what WP-1037 removed, and not text that names wp next
// without ordering the agent to take a task with it.
func TestTakeOrderRE(t *testing.T) {
	for _, s := range []string{
		"Take %s with `wp next %s` and do it; its answer says how to finish.",
		"Take the task with `wp next APP-3`.",
		"take WP-12 with `wp next WP-12`",
	} {
		if !takeOrderRE.MatchString(s) {
			t.Errorf("missed: %q", s)
		}
	}
	for _, s := range []string{
		"`wp next %s` shows the task, or why it can't be taken",
		"inside a Claude Code session: `wp next KEY` takes it here",
	} {
		if takeOrderRE.MatchString(s) {
			t.Errorf("flagged: %q", s)
		}
	}
}
