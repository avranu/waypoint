package core

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

// realTree is the top of this repo's tree at 35cba08 (git ls-tree), whose id
// is 667f113ffa1e9aa04293e73cf622053bc0c6f631.
var realTree = []treeEntry{
	{Mode: "040000", SHA: "5f28b363fe50f94ca1d8012732125c1ae92acbf4", Path: ".claude"},
	{Mode: "100644", SHA: "f6f29c9006b2219ffedded86d33cff1cd8445145", Path: ".editorconfig"},
	{Mode: "040000", SHA: "3abf426aa7f8773877fd2483d657d089214b0f0e", Path: ".forgejo"},
	{Mode: "100644", SHA: "2afe8004d4a635584681cc0ee6f531c5e6c1f97a", Path: ".gitignore"},
	{Mode: "100644", SHA: "3b52498b262836898bee26564b5b0c9f8b7aa330", Path: ".gitleaksignore"},
	{Mode: "100644", SHA: "8debaa7fcf2bcbb4300f36bba6704331727c0c97", Path: ".golangci.yml"},
	{Mode: "100644", SHA: "a45fd52cc5891570d6299fab38643103c3955474", Path: ".nvmrc"},
	{Mode: "100644", SHA: "fd2c106e917d596c26e98d684c182e3975356dca", Path: ".pre-commit-config.yaml"},
	{Mode: "100644", SHA: "bd06f1b1d3ae7d89a295cc66e82bdf7eddbbe8f5", Path: "AGENTS.md"},
	{Mode: "100644", SHA: "33740b3260dd6b760cb81f57f1ccd12dc8fdd499", Path: "Makefile"},
	{Mode: "100644", SHA: "6a06cd73de101750800ac9828963f13a2de72492", Path: "README.md"},
	{Mode: "100644", SHA: "ad0f4ebc1e6912b9bfca91951ae3ef273fca5cc1", Path: "REVIEW.md"},
	{Mode: "040000", SHA: "b65847e59d6bf59dac9052ec8855fbb00b8f9e2a", Path: "cmd"},
	{Mode: "040000", SHA: "a3c52b86871008c7c3f32b80acb03f5872698cf7", Path: "deploy"},
	{Mode: "040000", SHA: "796f78fbc45c41f37a9cc73d8786353a328de7d6", Path: "docs"},
	{Mode: "040000", SHA: "3a7b334035b1e046eb898b399d99138d6a603790", Path: "e2e"},
	{Mode: "100644", SHA: "8386d23e94a919214358b0fa18a717722e758879", Path: "go.mod"},
	{Mode: "100644", SHA: "843fda10e3f01aac2037eb8089e78fe86c532a60", Path: "go.sum"},
	{Mode: "040000", SHA: "dab1ad4a4a4f03b466d1d60d19bad59e8f8d7d24", Path: "internal"},
	{Mode: "100644", SHA: "1109a7ced2fbe5ae3671a2278d1d339bd5ec4000", Path: "renovate.json"},
	{Mode: "040000", SHA: "12e0edd182af977651523ecb58ccfc71deb2483f", Path: "web"},
}

const realTreeID = "667f113ffa1e9aa04293e73cf622053bc0c6f631"

// Forgejo says a commit's tree is the commit itself; Waypoint works the
// tree's id out from its entries as git does, in any order the forge gives.
func TestGitTreeID(t *testing.T) {
	if got, err := gitTreeID(realTree); err != nil || got != realTreeID {
		t.Fatalf("got %q %v, want %s", got, err, realTreeID)
	}
	rev := make([]treeEntry, len(realTree))
	for i, e := range realTree {
		rev[len(realTree)-1-i] = e
	}
	if got, _ := gitTreeID(rev); got != realTreeID {
		t.Errorf("reversed: %q", got)
	}
	// git sorts a subtree as if its name ended in "/": "a-b" before "a/".
	// (git mktree)
	if a, _ := gitTreeID([]treeEntry{{Mode: "040000", Path: "a", SHA: realTree[0].SHA}, {Mode: "100644", Path: "a-b", SHA: realTree[1].SHA}}); a != "7d26165c257e9664f1a89d83f34f7f8ee37d12d9" {
		t.Errorf("subtree order: %q", a)
	}
	for _, bad := range [][]treeEntry{
		{{Mode: "100644", Path: "x", SHA: "not hex"}},
		{{Mode: "100644", Path: "a/b", SHA: realTree[1].SHA}},
		{{Mode: "100644", Path: "x", SHA: realTree[1].SHA}, {Mode: "100644", Path: "y", SHA: realTree[1].SHA + realTree[1].SHA[:24]}},
	} {
		if id, err := gitTreeID(bad); err == nil {
			t.Errorf("%+v: %s", bad, id)
		}
	}
}

func TestCommitTreeForgejo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/repos/o/r/git/trees/35cba08b9953f1090d8839464dcf9482ecf7c6ae" {
			http.NotFound(w, r)
			return
		}
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		lo := min((page-1)*10, len(realTree))
		hi := min(lo+10, len(realTree))
		_ = json.NewEncoder(w).Encode(map[string]any{"sha": "35cba08b9953f1090d8839464dcf9482ecf7c6ae", "tree": realTree[lo:hi], "truncated": hi < len(realTree), "total_count": len(realTree)})
	}))
	defer srv.Close()
	cl := &httpForgeClient{http: srv.Client()}
	got, err := cl.CommitTree(context.Background(), &Forge{Kind: "forgejo", BaseURL: srv.URL, token: "t"}, "o/r", "35cba08b9953f1090d8839464dcf9482ecf7c6ae")
	if err != nil || got != realTreeID {
		t.Fatalf("got %q %v", got, err)
	}
	if _, err := cl.CommitTree(context.Background(), &Forge{Kind: "forgejo", BaseURL: srv.URL, token: "t"}, "o/r", "ffff"); err == nil {
		t.Error("a missing commit has a tree")
	}
}
