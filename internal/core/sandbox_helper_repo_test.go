package core

import "testing"

func TestRepoURLIsMatchesForge(t *testing.T) {
	cases := []struct {
		url, base string
		want      bool
	}{
		{"https://git.example.com/o/n.git", "https://git.example.com", true},
		{"git@git.example.com:o/n.git", "https://git.example.com", true},
		{"ssh://git@git.example.com:2222/o/n", "https://git.example.com:3000", true},
		{"https://github.com/o/n", "https://git.example.com", false},
		{"git@github.com:o/n", "https://git.example.com", false},
		{"https://git.example.com/o/other", "https://git.example.com", false},
	}
	for _, c := range cases {
		if got := repoURLIs(c.url, "o/n", c.base); got != c.want {
			t.Errorf("repoURLIs(%q, %q) = %v, want %v", c.url, c.base, got, c.want)
		}
	}
}
