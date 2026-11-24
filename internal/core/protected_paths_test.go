package core

import (
	"slices"
	"testing"
)

// The fix-up agents' protected paths keep real env files and keys but let
// the committed env templates through (WP-505).
func TestProtectedPathsEnvTemplates(t *testing.T) {
	files := []string{
		"deploy/.env.example", ".env.sample", "app/.env.template", ".env.dist",
		".env", "deploy/.env", ".env.production", "deploy/.env.local", "deploy/.env.example.bak",
		".forgejo/workflows/ci.yml", "go.sum", "main.go",
	}
	got := protectedTouched(protectedPaths, files)
	want := []string{".env", "deploy/.env", ".env.production", "deploy/.env.local", "deploy/.env.example.bak", ".forgejo/workflows/ci.yml", "go.sum"}
	if !slices.Equal(got, want) {
		t.Errorf("protected = %v, want %v", got, want)
	}
	if got := protectedTouched(protectedPaths, []string{".github/.env.example", ".github/workflows/x.yml", "secrets/.env.example"}); len(got) != 3 {
		t.Errorf("directory protections lost to an exemption: %v", got)
	}
	// Source code in a package called secrets is not protected; other files there, and the same name under .github/, are.
	if got := protectedTouched(protectedPaths, []string{"gate/internal/secrets/secrets.go", "secrets/a/b.py", "main.go"}); got != nil {
		t.Errorf("source in a secrets package is protected: %v", got)
	}
	if got := protectedTouched(protectedPaths, []string{"secrets/prod.yaml", "deploy/secrets/tls.crt", ".github/secrets/x.go"}); len(got) != 3 {
		t.Errorf("real secrets let out: %v", got)
	}
	if got := protectedTouched([]string{"!**/.env.example"}, []string{"deploy/.env.example", "main.go"}); got != nil {
		t.Errorf("an exemption alone protects %v", got)
	}
}
